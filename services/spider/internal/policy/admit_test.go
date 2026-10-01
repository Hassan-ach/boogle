package policy

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestAdmitVisitedIsTheCheapestPossibleAnswer pins the first thing Admit does with
// a URL it has seen before.
//
// Visited is checked before the host markers, the host hash and robots.txt, and
// that ordering is the difference between a rediscovered link costing one Redis
// read and costing eight checks plus a possible network call. A link that appears
// on a thousand pages is admitted a thousand times.
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

// TestAdmitChecksDeadHostBeforeTheNetwork is the fix for the reported crawl loop,
// asserted as an ordering property rather than as a total.
//
// Before this existed, a dead domain's robots.txt was re-fetched for every URL the
// frontier handed out, because nothing recorded that the host had failed. With
// MAX_CRAWLERS=20 all twenty workers eventually sat on one dead domain, each
// holding a fetchpool slot for up to fifteen seconds, retrying forever. The
// property that stops it is not "the fetch is cheap" but "the fetch does not
// happen": a host with a dead marker is answered from Redis alone.
// TestAdmitKeysVisitedByTheCanonicalForm is the contract between Admit and the
// frontier, and it is the reason Admit canonicalises at all.
//
// The frontier stores canonical URLs, so the visited set is keyed by canonical
// URLs too. If Admit checked a raw spelling instead, a page reached through two
// spellings -- "https://Example.com/A/" and its canonical form -- would be
// crawled, indexed and counted twice, and the duplicate would compete with the
// original for the same words. Nothing else in the crawl would notice: each
// spelling looks like a different URL right up until the page is indexed twice.
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
	// The caller enqueues the canonical form, so that is what the visited set holds.
	if err := st.MarkVisited(ctx, page); err != nil {
		t.Fatal(err)
	}

	for _, spelling := range spellings {
		if v := mustAdmit(t, m, spelling); v.Kind != Skip || v.Reason != ReasonVisited {
			t.Errorf("Admit(%q) = %s, want skip/visited; the page has been crawled",
				spelling, FormatVerdict(v))
		}
	}

	// The same canonicalisation the frontier applies, so the two agree by
	// construction rather than by coincidence.
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
	// Forbid the fetch outright: a call here is a test failure, not a value to
	// inspect afterwards.
	fetch.forbidCalls(t)

	for i := range 50 {
		url := "https://dead.example/page-" + itoa(i)
		assertVerdict(t, mustAdmit(t, m, url), Skip, ReasonHostDead)
	}
	if got := fetch.totalCalls(); got != 0 {
		t.Errorf("50 URLs on a dead host caused %d robots fetches, want 0", got)
	}

	// And the refusal must have a due time, or the caller has no way to know when
	// the host comes back. Without it, a Skip on a dead host is indistinguishable
	// from a permanent decision, and the host is never probed again.
	v := mustAdmit(t, m, "https://dead.example/again")
	if v.Until.IsZero() {
		t.Error("a dead-host refusal carried no Until; the host could never be probed again")
	}
}

// TestAdmitChecksColdAndCooldownBeforeTheNetwork is the same ordering property for
// the other two markers.
//
// A cold host has spent its page budget and a cooling host is answering slowly.
// Neither is a reason to open a connection, and in both cases the answer comes
// from a key whose TTL the caller needs.
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

// TestAdmitCooldownDefersRatherThanSkips is a decision, not a convenience.
//
// A cooling-down host is answering, just not happily, and it recovers in seconds.
// Skip would mark its URLs visited and strand every one of them for good, on a
// host that was about to be fine. This is the one marker whose expiry is expected
// to bring the host back.
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
	// The URL must not have been retired. Retire is what turns a Defer into a
	// permanent loss, and it is the mistake this assertion exists to prevent.
	if visited, err := st.IsVisited(ctx, target); err != nil {
		t.Fatalf("is visited: %v", err)
	} else if visited {
		t.Error("a deferred URL was marked visited; it can never be crawled")
	}
}

// TestAdmitRefusesURLsThatAreNotURLs covers the two canonicalisation failures that
// used to reach the network.
//
// A link is not required to be a URL. A page can contain "mailto:...", a
// "javascript:" href, a "tel:" number, or a percent-encoded fragment from a
// template engine. Each parsed without error and each became a fetch.
func TestAdmitRefusesURLsThatAreNotURLs(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantKind   VerdictKind
		wantReason Reason
	}{
		// Not fetchable over HTTP. Note these have to be caught before the host
		// is even derived: "mailto:someone@example.com" has an "@" in its path and
		// no host, and reading a host out of it would look up a domain built from
		// someone's mailbox.
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

// TestAdmitAppliesRuleTables is the table of skip rules, asserted one row at a
// time.
//
// The order of these checks relative to robots.txt is asserted separately; this
// covers only that each rule refuses, so a failure here means the rule itself is
// wrong rather than that it ran in the wrong place.
func TestAdmitAppliesRuleTables(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantKind   VerdictKind
		wantReason Reason
	}{
		// Path prefixes. "/cart" must not take "/cartoon" with it, which is the
		// whole reason the comparison is on path segments.
		{"path prefix exact", "https://example.com/login", Skip, ReasonPathDisallowed},
		{"path prefix nested", "https://example.com/account/settings", Skip, ReasonPathDisallowed},
		{"path prefix trailing slash form", "https://example.com/admin/users", Skip, ReasonPathDisallowed},
		{"cart is not cartoon", "https://example.com/cartoon", Allow, ReasonOK},
		{"search is not searching", "https://example.com/searching", Allow, ReasonOK},
		{"login is not logins-archive", "https://example.com/logins-archive", Allow, ReasonOK},
		{"admin is not administration", "https://example.com/administration", Allow, ReasonOK},
		{"search is not a prefix of search-archive", "https://example.com/search-archive", Allow, ReasonOK},

		// File extensions, refused before a transfer rather than after parsing.
		{"pdf", "https://example.com/report.pdf", Skip, ReasonExtensionSkipped},
		{"uppercase pdf", "https://example.com/Report.PDF", Skip, ReasonExtensionSkipped},
		{"zip", "https://example.com/bundle.zip", Skip, ReasonExtensionSkipped},
		{"javascript", "https://example.com/app.js", Skip, ReasonExtensionSkipped},
		{"png", "https://example.com/logo.png", Skip, ReasonExtensionSkipped},
		{"no extension", "https://example.com/article", Allow, ReasonOK},
		// A dot in a directory name is not an extension.
		{"dot in directory", "https://example.com/v1.2/article", Allow, ReasonOK},
		// A trailing slash after an extension-looking segment is a directory.
		{"extension then slash", "https://example.com/report.pdf/", Allow, ReasonOK},

		// Translated copies of pages that were not worth indexing anyway.
		{"template in french", "https://en.wikipedia.org/wiki/Template:Infobox/fr/", Skip, ReasonLanguageNotEnglish},
		{"help in german", "https://en.wikipedia.org/wiki/Help:Contents/de", Skip, ReasonLanguageNotEnglish},
		// A language subpage on an ordinary article is real content -- the
		// English mirror of a translated article -- and must survive.
		{"article language subpage", "https://en.wikipedia.org/wiki/Paris/fr", Allow, ReasonOK},
		// A noisy namespace with no language subpage is not a translated copy of
		// anything; the table declines it for being a template, not for its
		// language, and that is a different reason.
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

// TestAdmitObeysRobotsAllowOverDisallow is the rule that was parsed and thrown
// away for the life of the codebase.
//
// "Allow: /wiki/" together with "Disallow: /" is the ordinary way a site says
// "come to the good part". The single rule that would have unblocked it was the
// one being ignored, so the crawler obeyed the blunter half of the instruction and
// lost the site entirely -- the one outcome worse than not following robots.txt at
// all.
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
		// Outside the carve-out, "/" still governs.
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

// TestAdmitRobotsLongestMatchWins pins the specificity rule, in both directions.
//
// Longest match is what makes a nested carve-out work. Without it, "Disallow: /"
// would beat "Allow: /wiki/" by being checked first, or "Allow: /wiki/" would
// beat "Disallow: /wiki/private" by being the only rule considered.
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

// TestAdmitFailsClosedOnStateErrors is the safety property for Admit specifically.
//
// hostGate covers this for the marker and hash reads. Admit adds two more reads
// and a network call, and every one of them can fail. A Redis outage must not
// produce Allow: it would fetch URLs whose cooldowns, dead markers and page
// budgets were all unreadable, which is the same as having no policy at all.
func TestAdmitFailsClosedOnStateErrors(t *testing.T) {
	// The reads Admit makes before it could possibly allow anything. Each is made
	// to fail on its own, because "everything is broken" and "the one thing I care
	// about is broken" are different bugs and only the second one is silent.
	//
	// Only reads are here, and that is the shape of the rule: an unreadable fact
	// must stop the crawl, while an unwritable one may not.
	// TestAdmitStillCrawlsWhenAWriteFails covers the other half. ResetWindow is
	// absent because a window has to have expired for it to be reached at all,
	// which this setup does not arrange; TestAdmitResetsFailClosed covers that.
	for _, op := range []string{"IsVisited", "Markers", "HostState"} {
		t.Run(op, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())
			st.FailOn = map[string]error{op: errRedisDown}

			v, err := m.Admit(context.Background(), "https://example.com/page")
			if err != nil {
				// An error is acceptable; an Allow is not. The contract is that the
				// caller never gets permission to crawl.
				return
			}
			if v.Kind == Allow {
				t.Fatalf("%s failing produced Allow", op)
			}
		})
	}
}

// TestEveryDeferComesWithADueTime is the invariant the crawl loop's correctness
// rests on, and it is the one thing about a Defer that is easy to leave out.
//
// A URL is taken off the frontier before this function is asked about it. So by
// the time a verdict arrives, the queue has already given the URL up, and a Defer
// is the only thing that can put it back. A Defer with no due time cannot be
// parked -- there is nothing to score it by -- and an unparked URL is not deferred,
// it is deleted.
//
// Deleted from where is worth being concrete about. Not from the visited set, so
// nothing stops it being offered again; but the pages that would offer it are
// themselves gated on the store that is down, so during the outage there is no
// rediscovery, and when the store comes back the URL is not in the frontier, not in
// the delayed set, and not in the visited set. It is simply gone, along with every
// other URL that came up while the cache was unreachable. The crawl restarts
// apparently healthy with an empty queue and no error anywhere.
//
// Each case below is a different path to a Defer, because "every" is the claim and a
// single example would not establish it.
func TestEveryDeferComesWithADueTime(t *testing.T) {
	now := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// breakIt arranges the state that produces the Defer.
		breakIt func(m *PolicyManager, st *MemoryState)
		// host is the host to admit a URL on. Empty means example.com.
		host string
		// robots is the fetcher, when the case needs a particular one.
		robots *fakeRobots
	}{
		{
			// "we could not ask the store anything"
			name:    "the policy state is unreadable",
			breakIt: func(_ *PolicyManager, st *MemoryState) { st.FailOn = map[string]error{"IsVisited": errRedisDown} },
		},
		{
			// "the host answered and said wait"
			name: "the host is cooling down",
			breakIt: func(_ *PolicyManager, st *MemoryState) {
				_ = st.SetMarker(context.Background(), "example.com", MarkerCooldown, time.Hour)
			},
		},
		{
			// "we could not resolve a host nobody has seen" -- the second of the two
			// ways to end up not knowing when, and the more expensive one, because
			// resolving a host is the only step in Admit that touches the network.
			name:    "the host cannot be resolved",
			breakIt: func(_ *PolicyManager, _ *MemoryState) {},
			// A host that does not resolve is a Defer, not a Skip, for the ordinary
			// reason: the domain may be back in ten seconds. The state read behind
			// that decision is the marker EnsureHost raised, and when it expires the
			// next Admit is the implicit probe.
			host: "slow.example",
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

// TestAdmitStillCrawlsWhenAWriteFails is the other half of the fail-closed rule,
// and the half that is easy to get wrong.
//
// Fails closed means not crawling when the facts are *unreadable*. It does not
// mean refusing when a *result* could not be recorded -- and here the rules were
// demonstrably just read, in the hand, off the wire. Refusing would drop a page
// whose rules we know, and would do so because of a bookkeeping failure having
// nothing to do with the page. The cost is that the next URL re-fetches
// robots.txt, which is the behaviour we had before caching existed.
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

// TestARefusalSurvivesACounterFailure is the other half of the same asymmetry, and
// the half a first reading gets wrong.
//
// Reads fail closed and writes fail open, but "fail open" must not mean "the
// decision evaporates". A refusal is reached by refusing a URL, and refusing a URL
// happens by counting the reason -- so making the counter fail-close would turn
// every refused URL into an error. The caller would then retry it, and retrying a
// refusal is the loop this package exists to stop: the URL is refused, the caller
// sees an error, the caller retries, the caller is refused again.
//
// The verdict therefore has to survive a counter that cannot be written, and the
// error must stay on the log where the count went missing.
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

	// And it is still a refusal rather than an allow: the crawl does not proceed on
	// the strength of a write it could not make.
	if kind, _ := admitReason(t, m, pdf); kind != Skip {
		t.Errorf("a refused URL was %s after its counter failed to write", kind)
	}
}

// TestAdmitCountsEveryRefusal is the phase-5 acceptance criterion, asserted early
// because it is the reason the manager exists rather than a set of helper
// functions.
//
// A refusal that is not counted is invisible. Before this, a page yielding forty
// links produced eleven frontier entries that were silently removed later, so the
// logs said nothing about the pages being declined and no operator could answer
// "why is this site barely in the index".
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
				// A refusal with no host has nothing to count against. "mailto:..."
				// has no host, and inventing one -- a domain built out of someone's
				// mailbox -- would be worse than dropping the count. The refusal
				// still reaches the log, which is the only place it can be read.
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

// TestAdmitResolvesUnknownHostExactlyOnce covers the caching that replaced a
// per-URL robots fetch.
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

// TestAdmitRereadsRobotsAfterTheTTL proves the cache has an end, which matters as
// much as its start: a rules file that changed and was never re-read is a site
// asking us to stop and us declining to hear it.
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

	// Just inside the TTL.
	clock.Advance(m.cfg.RobotsTTL - time.Second)
	if v := mustAdmit(t, m, "https://example.com/b"); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if n := fetch.callCount("example.com"); n != 1 {
		t.Errorf("inside the TTL the host was re-fetched: %d fetches, want 1", n)
	}

	// Past it.
	clock.Advance(2 * time.Second)
	if v := mustAdmit(t, m, "https://example.com/c"); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	if n := fetch.callCount("example.com"); n != 2 {
		t.Errorf("after the TTL fetches = %d, want 2", n)
	}
	_ = st
}
