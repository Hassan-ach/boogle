package policy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmitVisitedIsTheCheapestPossibleAnswer(t *testing.T) {
	m, st := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{}, body: "", status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	const target = "https://example.com/page"
	if v := mustAdmit(t, m, target); v.Kind != Allow {
		t.Fatalf("first Admit = %s, want Allow", FormatVerdict(v))
	}
	if err := st.MarkVisited(context.Background(), target); err != nil {
		t.Fatalf("mark visited: %v", err)
	}

	callsBefore := fetch.totalCalls()
	assertVerdict(t, mustAdmit(t, m, target), Skip, ReasonVisited)

	if got := fetch.totalCalls(); got != callsBefore {
		t.Errorf("a visited URL triggered %d robots fetches, want 0", got-callsBefore)
	}
	if n := st.StatsTotal("example.com"); n == 0 {
		t.Error("a refusal was not counted")
	}
	if n := st.Stats("example.com")[ReasonVisited]; n != 1 {
		t.Errorf("visited refusals counted = %d, want 1", n)
	}
}

func TestAdmitKeysVisitedByTheCanonicalForm(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
	ctx := context.Background()

	const page = "https://example.com/a"
	spellings := []string{
		page,
		"https://Example.com/a/",
		"https://EXAMPLE.com/a#section",
	}

	if v := mustAdmit(t, m, spellings[1]); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if err := st.MarkVisited(ctx, page); err != nil {
		t.Fatal(err)
	}

	for _, spelling := range spellings {
		if v := mustAdmit(t, m, spelling); v.Kind != Skip || v.Reason != ReasonVisited {
			t.Errorf("Admit(%q) = %s, want skip/visited; the page has been crawled",
				spelling, FormatVerdict(v))
		}
	}

	if got := canonicalOf("https://Example.com/a/"); got != page {
		t.Errorf("canonicalOf = %q, want %q", got, page)
	}
}

func TestAdmitChecksDeadHostBeforeTheNetwork(t *testing.T) {
	m, st := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{}}
	m = m.WithRobotsFetcher(fetch.fetcher())
	ctx := context.Background()

	if err := st.SetMarker(ctx, "dead.example", MarkerDead, time.Minute); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	fetch.forbidCalls(t)

	for i := range 50 {
		url := "https://dead.example/page-" + itoa(i)
		assertVerdict(t, mustAdmit(t, m, url), Skip, ReasonHostDead)
	}
	if got := fetch.totalCalls(); got != 0 {
		t.Errorf("50 URLs on a dead host caused %d robots fetches, want 0", got)
	}

	v := mustAdmit(t, m, "https://dead.example/again")
	if v.Until.IsZero() {
		t.Error("a dead-host refusal carried no Until; the host could never be probed again")
	}
}

func TestAdmitChecksColdAndCooldownBeforeTheNetwork(t *testing.T) {
	cases := []struct {
		name       string
		marker     MarkerKind
		wantKind   VerdictKind
		wantReason Reason
	}{
		{"cold", MarkerCold, Skip, ReasonHostCold},
		{"cooldown", MarkerCooldown, Defer, ReasonHostCoolingDown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestManager(t)
			fetch := &fakeRobots{reply: map[string]robotsResponse{}}
			m = m.WithRobotsFetcher(fetch.fetcher())
			ctx := context.Background()

			if err := st.SetMarker(ctx, tc.name+".example", tc.marker, time.Minute); err != nil {
				t.Fatalf("set marker: %v", err)
			}
			fetch.forbidCalls(t)

			v := mustAdmit(t, m, "https://"+tc.name+".example/x")
			assertVerdict(t, v, tc.wantKind, tc.wantReason)
			if v.Until.IsZero() {
				t.Error("verdict carried no Until; the URL or host could never come back")
			}
			if got := fetch.totalCalls(); got != 0 {
				t.Errorf("caused %d robots fetches, want 0", got)
			}
		})
	}
}

func TestAdmitCooldownDefersRatherThanSkips(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}}).fetcher())
	ctx := context.Background()

	const target = "https://slow.example/page"
	if err := st.SetMarker(ctx, "slow.example", MarkerCooldown, time.Minute); err != nil {
		t.Fatalf("set marker: %v", err)
	}

	kind, _ := admitReason(t, m, target)
	if kind != Defer {
		t.Fatalf("kind = %v, want Defer", kind)
	}
	if visited, err := st.IsVisited(ctx, target); err != nil {
		t.Fatalf("is visited: %v", err)
	} else if visited {
		t.Error("a deferred URL was marked visited; it can never be crawled")
	}
}

func TestAdmitRefusesURLsThatAreNotURLs(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantKind   VerdictKind
		wantReason Reason
	}{
		{"mailto", "mailto:someone@example.com", Skip, ReasonNonHTTPScheme},
		{"javascript", "javascript:void(0)", Skip, ReasonNonHTTPScheme},
		{"data", "data:text/html;base64,PGgxPmhpPC9oMT4=", Skip, ReasonNonHTTPScheme},
		{"tel", "tel:+441234567890", Skip, ReasonNonHTTPScheme},
		{"ftp", "ftp://files.example.com/x", Skip, ReasonNonHTTPScheme},
		{"fragment only", "#section-3", Skip, ReasonMalformedURL},
		{"empty", "", Skip, ReasonMalformedURL},
		{"whitespace", "   ", Skip, ReasonMalformedURL},
		{"no host", "/relative/path", Skip, ReasonMalformedURL},
		{"unparseable", "http://[::1", Skip, ReasonMalformedURL},
		{"invalid utf8", "https://example.com/\xff\xfe", Skip, ReasonMalformedURL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			fetch := &fakeRobots{reply: map[string]robotsResponse{}}
			m = m.WithRobotsFetcher(fetch.fetcher())

			assertVerdict(t, mustAdmit(t, m, tc.url), tc.wantKind, tc.wantReason)
			if got := fetch.totalCalls(); got != 0 {
				t.Errorf("a malformed URL caused %d robots fetches, want 0", got)
			}
		})
	}
}

func TestAdmitAppliesRuleTables(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantKind   VerdictKind
		wantReason Reason
	}{
		{"path prefix exact", "https://example.com/login", Skip, ReasonPathDisallowed},
		{"path prefix nested", "https://example.com/account/settings", Skip, ReasonPathDisallowed},
		{"path prefix trailing slash form", "https://example.com/admin/users", Skip, ReasonPathDisallowed},
		{"cart is not cartoon", "https://example.com/cartoon", Allow, ReasonOK},
		{"search is not searching", "https://example.com/searching", Allow, ReasonOK},
		{"login is not logins-archive", "https://example.com/logins-archive", Allow, ReasonOK},
		{"admin is not administration", "https://example.com/administration", Allow, ReasonOK},
		{"search is not a prefix of search-archive", "https://example.com/search-archive", Allow, ReasonOK},

		{"pdf", "https://example.com/report.pdf", Skip, ReasonExtensionSkipped},
		{"uppercase pdf", "https://example.com/Report.PDF", Skip, ReasonExtensionSkipped},
		{"zip", "https://example.com/bundle.zip", Skip, ReasonExtensionSkipped},
		{"javascript", "https://example.com/app.js", Skip, ReasonExtensionSkipped},
		{"png", "https://example.com/logo.png", Skip, ReasonExtensionSkipped},
		{"no extension", "https://example.com/article", Allow, ReasonOK},
		{"dot in directory", "https://example.com/v1.2/article", Allow, ReasonOK},
		{"extension then slash", "https://example.com/report.pdf/", Allow, ReasonOK},

		{"template in french", "https://en.wikipedia.org/wiki/Template:Infobox/fr/", Skip, ReasonLanguageNotEnglish},
		{"help in german", "https://en.wikipedia.org/wiki/Help:Contents/de", Skip, ReasonLanguageNotEnglish},
		{"article language subpage", "https://en.wikipedia.org/wiki/Paris/fr", Allow, ReasonOK},
		{"template in english", "https://en.wikipedia.org/wiki/Template:Infobox", Allow, ReasonOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())

			assertVerdict(t, mustAdmit(t, m, tc.url), tc.wantKind, tc.wantReason)
		})
	}
}

func TestAdmitObeysRobotsAllowOverDisallow(t *testing.T) {
	const robots = `User-agent: *
Allow: /wiki/
Disallow: /
Sitemap: https://example.com/sitemap.xml
`

	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"example.com": {body: robots}},
	}).fetcher())

	cases := []struct {
		path string
		want VerdictKind
	}{
		{"/wiki/Article", Allow},
		{"/wiki/", Allow},
		{"/blog/post", Skip},
		{"/", Skip},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			v := mustAdmit(t, m, "https://example.com"+tc.path)
			if v.Kind != tc.want {
				t.Errorf("%s = %s, want %v", tc.path, FormatVerdict(v), tc.want)
			}
			if tc.want == Skip && v.Reason != ReasonRobotsDisallow {
				t.Errorf("%s refused for %q, want %q", tc.path, v.Reason, ReasonRobotsDisallow)
			}
		})
	}
}

func TestAdmitRobotsLongestMatchWins(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"example.com": {body: `User-agent: *
Allow: /wiki/
Disallow: /wiki/private/
Disallow: /
`}},
	}).fetcher())

	cases := []struct {
		path string
		want VerdictKind
	}{
		{"/wiki/Public", Allow},
		{"/wiki/private/Notes", Skip},
		{"/other", Skip},
	}
	for _, tc := range cases {
		if got := mustAdmit(t, m, "https://example.com"+tc.path).Kind; got != tc.want {
			t.Errorf("%s = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestAdmitFailsClosedOnStateErrors(t *testing.T) {
	for _, op := range []string{"IsVisited", "Markers", "HostState"} {
		t.Run(op, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
			st.FailOn = map[string]error{op: errRedisDown}

			v, err := m.Admit(context.Background(), "https://example.com/page")
			if err != nil {
				return
			}
			if v.Kind == Allow {
				t.Fatalf("%s failing produced Allow", op)
			}
		})
	}
}

func TestEveryDeferComesWithADueTime(t *testing.T) {
	now := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		breakIt func(m *PolicyManager, st *MemoryState)
		host    string
		robots  *fakeRobots
	}{
		{
			name:    "the policy state is unreadable",
			breakIt: func(_ *PolicyManager, st *MemoryState) { st.FailOn = map[string]error{"IsVisited": errRedisDown} },
		},
		{
			name: "the host is cooling down",
			breakIt: func(_ *PolicyManager, st *MemoryState) {
				_ = st.SetMarker(context.Background(), "example.com", MarkerCooldown, time.Hour)
			},
		},
		{
			name:    "the host cannot be resolved",
			breakIt: func(_ *PolicyManager, _ *MemoryState) {},
			host:    "slow.example",
			robots: &fakeRobots{reply: map[string]robotsResponse{
				"slow.example": {err: &net.DNSError{Err: "no such host", Name: "slow.example"}},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithClock(func() time.Time { return now })
			host := tc.host
			if host == "" {
				host = "example.com"
			}
			robots := tc.robots
			if robots == nil {
				robots = &fakeRobots{
					reply: map[string]robotsResponse{host: {body: ""}}, status: 200,
				}
			}
			m = m.WithRobotsFetcher(robots.fetcher())
			tc.breakIt(m, st)

			v := mustAdmit(t, m, "https://"+host+"/page")
			if v.Kind != Defer {
				t.Fatalf("Kind = %v (reason %q), want Defer: this case was supposed "+
					"to produce a URL whose crawl time is unknown", v.Kind, v.Reason)
			}
			if v.Until.IsZero() {
				t.Fatalf("Reason %q deferred the URL with no due time, so the crawl "+
					"loop cannot park it and the URL is lost rather than deferred",
					v.Reason)
			}
			if !v.Until.After(now) {
				t.Errorf("Until = %v, want a time after now (%v): a due time in the "+
					"past comes straight back off the delayed set and is fetched in "+
					"the same round", v.Until, now)
			}
		})
	}
}

func TestAdmitStillCrawlsWhenAWriteFails(t *testing.T) {
	for _, op := range []string{"SaveHostState", "CountReason", "SetMarker"} {
		t.Run(op, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
			st.FailOn = map[string]error{op: errRedisDown}

			if v := mustAdmit(t, m, "https://example.com/page"); v.Kind != Allow {
				t.Errorf("%s failing produced %s, want Allow", op, FormatVerdict(v))
			}
		})
	}
}

func TestARefusalSurvivesACounterFailure(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
	st.FailOn = map[string]error{"CountReason": errRedisDown}

	const pdf = "https://example.com/report.pdf"
	v, err := m.Admit(context.Background(), pdf)
	if err != nil {
		t.Fatalf("a counter write failure turned a refusal into an error: %v", err)
	}
	assertVerdict(t, v, Skip, ReasonExtensionSkipped)

	if kind, _ := admitReason(t, m, pdf); kind != Skip {
		t.Errorf("a refused URL was %s after its counter failed to write", kind)
	}
}

func TestAdmitCountsEveryRefusal(t *testing.T) {
	const robots = `User-agent: *
Disallow: /private/
`

	cases := []struct {
		name       string
		url        string
		marker     MarkerKind
		withMarker bool
		wantReason Reason
	}{
		{"dead host", "https://dead.example/x", MarkerDead, true, ReasonHostDead},
		{"cold host", "https://cold.example/x", MarkerCold, true, ReasonHostCold},
		{"cooling host", "https://cool.example/x", MarkerCooldown, true, ReasonHostCoolingDown},
		{"path rule", "https://rules.example/login", 0, false, ReasonPathDisallowed},
		{"extension rule", "https://rules.example/a.pdf", 0, false, ReasonExtensionSkipped},
		{"robots rule", "https://rules.example/private/x", 0, false, ReasonRobotsDisallow},
		{"non-http scheme", "mailto:a@example.com", 0, false, ReasonNonHTTPScheme},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{
				reply:  map[string]robotsResponse{"rules.example": {body: robots}},
				status: 200,
			}).fetcher())
			ctx := context.Background()
			host := hostOfURL(tc.url)

			if tc.withMarker {
				if err := st.SetMarker(ctx, host, tc.marker, time.Minute); err != nil {
					t.Fatalf("set marker: %v", err)
				}
			}

			kind, reason := admitReason(t, m, tc.url)
			if kind == Allow {
				t.Fatalf("%s was allowed", tc.url)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if host == "" {
				if n := st.StatsTotal(""); n != 0 {
					t.Errorf("a host-less refusal was counted against %q", "")
				}
				return
			}
			if n := st.Stats(host)[tc.wantReason]; n != 1 {
				t.Errorf("stats[%s][%s] = %d, want 1", host, tc.wantReason, n)
			}
		})
	}
}

func TestAdmitResolvesUnknownHostExactlyOnce(t *testing.T) {
	m, _ := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{"example.com": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	for range 20 {
		if v := mustAdmit(t, m, uniqueURL("example.com", "/page")); v.Kind != Allow {
			t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
		}
	}
	if n := fetch.callCount("example.com"); n != 1 {
		t.Errorf("20 URLs on one host caused %d robots fetches, want 1", n)
	}
}

func TestAdmitRereadsRobotsAfterTheTTL(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)

	fetch := &fakeRobots{reply: map[string]robotsResponse{"example.com": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	if v := mustAdmit(t, m, "https://example.com/a"); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if n := fetch.callCount("example.com"); n != 1 {
		t.Fatalf("first Admit caused %d fetches, want 1", n)
	}

	clock.Advance(m.cfg.RobotsTTL - time.Second)
	if v := mustAdmit(t, m, "https://example.com/b"); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if n := fetch.callCount("example.com"); n != 1 {
		t.Errorf("inside the TTL the host was re-fetched: %d fetches, want 1", n)
	}

	clock.Advance(2 * time.Second)
	if v := mustAdmit(t, m, "https://example.com/c"); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if n := fetch.callCount("example.com"); n != 2 {
		t.Errorf("after the TTL fetches = %d, want 2", n)
	}
	_ = st
}

func TestAdmitLinksSeparatesTheTwoOutcomes(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{
			"example.com":   {body: "User-agent: *\nDisallow: /private/\n"},
			"pdfs.example":  {body: ""},
			"dead.example":  {err: errConnRefused},
			"quiet.example": {body: ""},
		},
		status: 200,
	}).fetcher())
	ctx := context.Background()

	if err := m.State().SetMarker(ctx, "dead.example", MarkerDead, time.Minute); err != nil {
		t.Fatal(err)
	}

	links := []string{
		"https://example.com/article",
		"https://example.com/private/secret",
		"https://example.com/report.pdf",
		"https://example.com/login",
		"https://pdfs.example/a.docx",
		"https://quiet.example/b?x=1#frag",
		"https://dead.example/c",
		"mailto:someone@example.com",
		"#top",
	}

	admitted, refused := m.AdmitLinks(ctx, links)

	wantAdmitted := []string{
		"https://example.com/article",
		"https://quiet.example/b?x=1",
	}
	if strings.Join(admitted, " ") != strings.Join(wantAdmitted, " ") {
		t.Errorf("admitted = %v\nwant      %v", admitted, wantAdmitted)
	}

	wantRefused := map[string]Reason{
		"https://example.com/private/secret": ReasonRobotsDisallow,
		"https://example.com/report.pdf":     ReasonExtensionSkipped,
		"https://example.com/login":          ReasonPathDisallowed,
		"https://pdfs.example/a.docx":        ReasonExtensionSkipped,
		"https://dead.example/c":             ReasonHostDead,
		"mailto:someone@example.com":         ReasonNonHTTPScheme,
		"#top":                               ReasonMalformedURL,
	}
	for link, want := range wantRefused {
		got, ok := refused[link]
		if !ok {
			t.Errorf("%s was neither admitted nor recorded as refused", link)
			continue
		}
		if got != want {
			t.Errorf("%s refused for %q, want %q", link, got, want)
		}
	}
	if len(refused) != len(wantRefused) {
		t.Errorf("refused has %d entries, want %d: %v", len(refused), len(wantRefused), refused)
	}
}

func TestAdmitLinksCountsEveryRefusal(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"example.com": {body: ""}},
		status: 200,
	}).fetcher())

	_, refused := m.AdmitLinks(context.Background(), []string{
		"https://example.com/a.pdf",
		"https://example.com/b.pdf",
		"https://example.com/c.png",
	})
	if len(refused) != 3 {
		t.Fatalf("refused = %v, want three entries", refused)
	}

	if n := st.Stats("example.com")[ReasonExtensionSkipped]; n != 3 {
		t.Errorf("extensions skipped = %d, want 3; a refusal nobody counts is "+
			"a refusal nobody can explain", n)
	}
}

func TestAdmitLinksPreservesOrderAndHandlesNothing(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())

	admitted, refused := m.AdmitLinks(context.Background(), nil)
	if len(admitted) != 0 || len(refused) != 0 {
		t.Errorf("AdmitLinks(nil) = %v/%v, want empty/empty", admitted, refused)
	}
	if admitted == nil || refused == nil {
		t.Error("AdmitLinks returned nil maps; ranging over them panics")
	}
}

func TestAdmitLinksEnqueuesNothing(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
	ctx := context.Background()

	links := []string{
		"https://example.com/a",
		"https://example.com/b",
		"https://example.com/c",
	}
	if admitted, _ := m.AdmitLinks(ctx, links); len(admitted) != len(links) {
		t.Fatalf("admitted %d of %d", len(admitted), len(links))
	}

	if n, err := st.FrontierLen(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("frontier holds %d URLs; AdmitLinks must not enqueue", n)
	}
	for _, link := range links {
		visited, err := st.IsVisited(ctx, link)
		if err != nil {
			t.Fatal(err)
		}
		if visited {
			t.Errorf("%s was marked visited by AdmitLinks; the caller's own "+
				"enqueue would then be dropped as a duplicate", link)
		}
	}
}

func TestCanonicalOfFallsBackRatherThanDropping(t *testing.T) {
	if got := canonicalOf("https://example.com/a//b/../c"); got != "https://example.com/a/c" {
		t.Errorf("canonicalOf did not canonicalise: %q", got)
	}
	if got := canonicalOf("https://example.com/%" + "zz"); got == "" {
		t.Error("canonicalOf returned an empty string for an uncanonicalisable link")
	}
}

func TestFormatVerdict(t *testing.T) {
	cases := []struct {
		name string
		v    *Verdict
		want []string
	}{
		{"nil", nil, []string{"none"}},
		{"a decision", &Verdict{Kind: Skip, Reason: ReasonHostDead},
			[]string{"skip", "host_dead"}},
		{"a wait", &Verdict{
			Kind:   Defer,
			Reason: ReasonHostCoolingDown,
			Until:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		}, []string{"defer", "host_cooling_down", "2026-03-01T12:00:00Z"}},
		{"a wait in another zone", &Verdict{
			Kind:   Defer,
			Reason: ReasonServerError,
			Until: time.Date(2026, 3, 1, 12, 0, 0, 0,
				time.FixedZone("CET", 3600)),
		}, []string{"defer", "server_error", "2026-03-01T11:00:00Z"}},
		{"a zero deadline is not a wait", &Verdict{Kind: Allow, Reason: ReasonOK},
			[]string{"allow", "ok"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatVerdict(tc.v)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("FormatVerdict = %q, want it to contain %q", got, want)
				}
			}
			if strings.Contains(got, "0001-01-01") {
				t.Errorf("FormatVerdict = %q contains a zero timestamp", got)
			}
		})
	}
}

func TestHTTPRobotsFetcherFetchesWhatARobotWould(t *testing.T) {
	var gotPath, gotAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAgent = r.Header.Get("User-Agent")
		io.WriteString(w, "User-agent: *\nDisallow: /x\n")
	}))
	defer srv.Close()

	body, status, err := newHTTPRobotsFetcher(nil, "BoogleBot")(t.Context(), srv.URL+"/robots.txt")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if gotPath != "/robots.txt" {
		t.Errorf("path = %q, want /robots.txt", gotPath)
	}
	if !strings.Contains(gotAgent, "BoogleBot") {
		t.Errorf("User-Agent = %q, want it to name BoogleBot", gotAgent)
	}
	if !strings.Contains(string(body), "Disallow: /x") {
		t.Errorf("body = %q, want the robots.txt", body)
	}
}

func TestHTTPRobotsFetcherFollowsRedirects(t *testing.T) {
	t.Run("a redirect to another path is followed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				http.Redirect(w, r, "/static/robots.txt", http.StatusMovedPermanently)
				return
			}
			io.WriteString(w, "User-agent: *\nDisallow: /y\n")
		}))
		defer srv.Close()

		body, status, err := newHTTPRobotsFetcher(nil, "Bot")(t.Context(), srv.URL+"/robots.txt")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if status != 200 || !strings.Contains(string(body), "/y") {
			t.Errorf("status = %d body = %q, want the redirected rules", status, body)
		}
	})

	t.Run("a redirect loop fails instead of spinning", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/robots.txt", http.StatusFound)
		}))
		defer srv.Close()

		start := time.Now()
		_, _, err := newHTTPRobotsFetcher(nil, "Bot")(t.Context(), srv.URL+"/robots.txt")
		if err == nil {
			t.Fatal("a redirect loop returned no error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("a redirect loop took %v; the limit is not being enforced", elapsed)
		}
	})
}

func TestHTTPRobotsFetcherBoundsTheResponse(t *testing.T) {
	t.Run("an oversized body is truncated", func(t *testing.T) {
		big := strings.Repeat("# padding padding padding\n", maxRobotsBytes/20)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "User-agent: *\nDisallow: /keepme\n")
			io.WriteString(w, big)
		}))
		defer srv.Close()

		done := make(chan struct{})
		var body []byte
		var status int
		var err error
		go func() {
			defer close(done)
			body, status, err = newHTTPRobotsFetcher(nil, "Bot")(t.Context(), srv.URL+"/robots.txt")
		}()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("an oversized robots.txt never completed")
		}

		if err == nil && !strings.Contains(string(body), "/keepme") {
			t.Errorf("body = %d bytes, want the rules at the front preserved", len(body))
		}
		_ = status
	})
}

func TestHTTPRobotsFetcherTruncatesAtTheLimit(t *testing.T) {
	oversized := "User-agent: *\nDisallow: /a\n" + strings.Repeat("x", maxRobotsBytes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, oversized)
	}))
	defer srv.Close()

	body, _, err := newHTTPRobotsFetcher(nil, "Bot")(t.Context(), srv.URL+"/robots.txt")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(body) > maxRobotsBytes {
		t.Errorf("read %d bytes, want at most %d", len(body), maxRobotsBytes)
	}
}

func TestHTTPRobotsFetcherStopsReadingAtTheLimit(t *testing.T) {
	written := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("y", 64<<10)
		for i := range 200 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			written.Add(int64(len(chunk)))
			if i%32 == 0 {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}))
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = newHTTPRobotsFetcher(nil, "Bot")(t.Context(), srv.URL+"/robots.txt")
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Fetch never returned")
	}

	time.Sleep(100 * time.Millisecond)
	if got := written.Load(); got > 8*maxRobotsBytes {
		t.Errorf("the server wrote %d bytes against a %d cap; the body is being "+
			"drained rather than abandoned", got, maxRobotsBytes)
	}
}

func TestRobotsRefusedErrorCarriesItsReason(t *testing.T) {
	err := error(&robotsRefusedError{status: 503, reason: ReasonServerError})

	reason, ok := RobotsRefusedReason(err)
	if !ok {
		t.Fatal("RobotsRefusedReason did not recognise a robots refusal")
	}
	if reason != ReasonServerError {
		t.Errorf("reason = %q, want %q", reason, ReasonServerError)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("Error() = %q, want it to name the status an operator would check", err.Error())
	}

	for _, other := range []error{nil, context.Canceled, errConnRefused} {
		if _, ok := RobotsRefusedReason(other); ok {
			t.Errorf("RobotsRefusedReason(%v) claimed a robots refusal", other)
		}
	}
}

func TestDefaultUserAgentNamesTheBot(t *testing.T) {
	got := defaultUserAgent(Config{UserAgent: defaultBotUserAgent})
	if !strings.Contains(got, defaultBotUserAgent) {
		t.Errorf("defaultUserAgent(Config{UserAgent: defaultBotUserAgent}) = %q, want it to contain the bot name", got)
	}
	if !strings.Contains(got, "/") {
		t.Errorf("defaultUserAgent = %q, want a product/version token", got)
	}

	const mine = "MyBot (+https://example.com/bot)"
	if got := defaultUserAgent(Config{UserAgent: mine}); got != mine {
		t.Errorf("defaultUserAgent overrode an explicit agent: %q", got)
	}
	if got := defaultUserAgent(Config{}); !strings.Contains(got, defaultBotUserAgent) {
		t.Errorf("defaultUserAgent(Config{}) = %q, want the bot name", got)
	}
}

func TestHTTPRobotsFetcherGivesUpOnASilentHost(t *testing.T) {
	original := defaultRobotsTimeout
	defaultRobotsTimeout = 100 * time.Millisecond
	t.Cleanup(func() { defaultRobotsTimeout = original })

	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, _, err := newHTTPRobotsFetcher(&http.Client{}, "Bot")(t.Context(), srv.URL+"/robots.txt")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a robots.txt fetch against a silent server reported no error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("the fetch took %v to give up on a silent host; the deadline is "+
				"the only thing bounding it", elapsed)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a silent host held the fetch for the whole test: there is no " +
			"deadline on the robots.txt request")
	}
}
