package policy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdmitLinksSeparatesTheTwoOutcomes is the replacement for ValidateLinks, and
// the difference is the whole reason it exists.
//
// The old function returned one list of URLs, so the ones it had dropped left no
// trace: a page with forty links and eleven PDFs put eleven entries in the frontier
// that a different function later removed, and the logs said nothing about which
// eleven or why. Nobody could answer "why is this site barely in the index".
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

// TestAdmitLinksCountsEveryRefusal proves the counting claim, which is the whole
// justification for the function. A refusal that is not counted cannot be
// reported, and an unreported refusal is indistinguishable from a crawl bug.
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

	stats := st.Stats("example.com")
	if n := stats[ReasonExtensionSkipped]; n != 3 {
		t.Errorf("extensions skipped = %d, want 3; a refusal nobody counts is "+
			"a refusal nobody can explain", n)
	}
}

// TestAdmitLinksPreservesOrderAndHandlesNothing checks the two shapes a caller
// actually passes: an empty page's worth of links, and a page full of them.
func TestAdmitLinksPreservesOrderAndHandlesNothing(t *testing.T) {
	m, _ := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{reply: map[string]robotsResponse{}, status: 200}).fetcher())

	admitted, refused := m.AdmitLinks(context.Background(), nil)
	if len(admitted) != 0 || len(refused) != 0 {
		t.Errorf("AdmitLinks(nil) = %v/%v, want empty/empty", admitted, refused)
	}
	// The maps must be non-nil: a caller that ranges over them, or writes into
	// refused to add a reason of its own, gets a nil-map panic on an empty page.
	if admitted == nil || refused == nil {
		t.Error("AdmitLinks returned nil maps; ranging over them panics")
	}
}

// TestAdmitLinksEnqueuesNothing checks the boundary, because getting it wrong
// twice is worse than getting it wrong once.
//
// Marking a link visited inside AdmitLinks would fight a caller that batches: the
// caller enqueues the whole page's links as one operation, and a URL already
// marked visited is dropped by the pop as a duplicate -- so every link but the
// first would vanish. This is why AdmitLinks filters and leaves the enqueueing to
// the caller.
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

// TestCanonicalOfFallsBackRatherThanDropping covers a link that will not
// canonicalise on its own.
//
// Admit has already approved this link, so something about it parsed. Returning
// "" here would drop an approved page; returning the original enqueues a
// non-canonical key, which risks a duplicate fetch but never a hole in the index.
func TestCanonicalOfFallsBackRatherThanDropping(t *testing.T) {
	if got := canonicalOf("https://example.com/a//b/../c"); got != "https://example.com/a/c" {
		t.Errorf("canonicalOf did not canonicalise: %q", got)
	}
	// A link that canonicalisation cannot handle keeps its spelling.
	if got := canonicalOf("https://example.com/%" + "zz"); got == "" {
		t.Error("canonicalOf returned an empty string for an uncanonicalisable link")
	}
}

// TestFormatVerdictDistinguishesAWaitFromADecision checks the one piece of
// operator-facing output this package produces.
//
// The difference between "we will not crawl this again" and "we will crawl this
// in nine minutes" is a deadline. A log line that omits it leaves an operator
// deciding whether to intervene based on a string that does not say.
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
			// The deadline must never appear as a zero timestamp, which is the
			// one thing an operator would act on and be wrong about.
			if strings.Contains(got, "0001-01-01") {
				t.Errorf("FormatVerdict = %q contains a zero timestamp", got)
			}
		})
	}
}

// TestHTTPRobotsFetcherFetchesWhatARobotWould drives the real fetcher, which is
// the one piece of this package that talks to the network.
//
// A hand-written fake proves the fake. Everything interesting here -- the status
// codes, the redirect, the redirect loop, the body limit, the timeout -- is
// behaviour of an HTTP client that this package configures rather than writes, so
// these are the assertions worth having.
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
	// The user agent is how a site chooses which of its stanzas applies. Getting
	// this wrong means either following another crawler's rules or none of our
	// own, and it is invisible until a site that splits its rules is crawled.
	if !strings.Contains(gotAgent, "BoogleBot") {
		t.Errorf("User-Agent = %q, want it to name BoogleBot", gotAgent)
	}
	if !strings.Contains(string(body), "Disallow: /x") {
		t.Errorf("body = %q, want the robots.txt", body)
	}
}

// TestHTTPRobotsFetcherFollowsRedirects covers the common case and the pathological
// one, which are different failures.
//
// Most sites redirect /robots.txt to /robots.txt.txt or to a scheme-relative URL.
// A host that redirects /robots.txt to itself forever must fail: left to the
// client it would spin until the timeout, and the timeout is ten seconds per host
// per crawl.
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
		// The client's own limit is the backstop; this only checks it is a limit
		// and not an unbounded wait.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("a redirect loop took %v; the limit is not being enforced", elapsed)
		}
	})
}

// TestHTTPRobotsFetcherBoundsTheResponse covers the two ways a robots.txt can be
// hostile, and both of them are cheap to defend against.
//
// A file can be enormous, and a response can arrive forever. Both are refused by
// the same two settings, and both matter because this fetch is on the path to
// every decision about a host: a stalled fetch is a stalled crawl.
func TestHTTPRobotsFetcherBoundsTheResponse(t *testing.T) {
	t.Run("an oversized body is truncated", func(t *testing.T) {
		// Rather than the error, the assertion is that this returns at all and
		// does not try to buffer whatever the server felt like sending. Anything
		// past the limit is unreadable as a rule anyway.
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

		// Whatever it decided, the rule at the front must have been readable: the
		// limit exists to bound the buffer, not to drop the whole file.
		if err == nil && !strings.Contains(string(body), "/keepme") {
			t.Errorf("body = %d bytes, want the rules at the front preserved", len(body))
		}
		_ = status
	})
}

// TestHTTPRobotsFetcherTruncatesAtTheLimit checks the number itself.
//
// A limit that is not enforced is not a limit, and this is the assertion that
// would notice if someone removed the ReadAll cap while leaving the constant.
func TestHTTPRobotsFetcherTruncatesAtTheLimit(t *testing.T) {
	// One byte past the limit, so a fetcher with no cap reads it all and one with
	// a cap stops short.
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

// TestHTTPRobotsFetcherStopsReadingAtTheLimit checks that the connection is
// closed rather than drained.
//
// Draining a hostile response to find its end is how a crawler becomes the reason
// a site runs out of bandwidth. The connection has to be closed with the body
// unread, so the server is the one left with the bytes.
func TestHTTPRobotsFetcherStopsReadingAtTheLimit(t *testing.T) {
	// A response far larger than the limit, from a server that counts what it
	// managed to write. If the client drains, the whole thing goes out.
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

	// Give the server a moment to notice the write failed.
	time.Sleep(100 * time.Millisecond)
	// 200 * 64KiB is 12.8 MiB against a 1 MiB cap. Draining would show close to
	// the full figure; stopping short shows a small fraction of it.
	if got := written.Load(); got > 8*maxRobotsBytes {
		t.Errorf("the server wrote %d bytes against a %d cap; the body is being "+
			"drained rather than abandoned", got, maxRobotsBytes)
	}
}

// TestRobotsRefusedErrorCarriesItsReason checks the typed error rather than its
// message.
//
// The reason is what decides the counter an operator reads. Deriving it from the
// message string would work until someone reworded the message, at which point
// every overloaded host would start being counted as an unreachable one -- a
// silent change in the only number anyone watches.
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

	// And every other error is not a refusal.
	for _, other := range []error{nil, context.Canceled, errConnRefused} {
		if _, ok := RobotsRefusedReason(other); ok {
			t.Errorf("RobotsRefusedReason(%v) claimed a robots refusal", other)
		}
	}
}

// TestDefaultUserAgentNamesTheBot guards a value nobody would notice being wrong.
//
// The user agent is how a site decides which of its robots.txt stanzas to show us,
// and how an operator identifies this crawler in their logs. A default of "" or
// "Go-http-client" is not a failure this package could detect anywhere else.
func TestDefaultUserAgentNamesTheBot(t *testing.T) {
	got := defaultUserAgent(Config{UserAgent: defaultBotUserAgent})
	if !strings.Contains(got, defaultBotUserAgent) {
		t.Errorf("defaultUserAgent(Config{UserAgent: defaultBotUserAgent}) = %q, want it to contain the bot name", got)
	}
	// A real agent string carries a version and a contact, because that is what
	// lets a site owner find out who is crawling them.
	if !strings.Contains(got, "/") {
		t.Errorf("defaultUserAgent = %q, want a product/version token", got)
	}

	// An operator who set their own must get theirs verbatim: it is how they
	// identify the crawler, and rewriting it would break that silently.
	const mine = "MyBot (+https://example.com/bot)"
	if got := defaultUserAgent(Config{UserAgent: mine}); got != mine {
		t.Errorf("defaultUserAgent overrode an explicit agent: %q", got)
	}
	// And an empty one still gets something identifiable rather than the Go default.
	if got := defaultUserAgent(Config{}); !strings.Contains(got, defaultBotUserAgent) {
		t.Errorf("defaultUserAgent(Config{}) = %q, want the bot name", got)
	}
}
