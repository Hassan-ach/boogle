package policy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Fetch is the boundary between "a decision" and "an observation". Everything the
// policy layer knows about a URL arrives through an Outcome, so the mapping from a
// real HTTP exchange onto an Outcome is load-bearing: a status dropped here is a
// 404 the crawler will happily fetch again.

func TestFetchReportsWhatTheServerSaid(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>page</html>"))
	}))
	defer srv.Close()

	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithFetchClient(srv.Client())

	body, out := m.Fetch(context.Background(), srv.URL)
	if out.Err != nil {
		t.Fatalf("Fetch reported %v for a 200", out.Err)
	}
	if out.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", out.StatusCode)
	}
	if out.ContentType != "text/html; charset=utf-8" {
		t.Errorf("ContentType = %q, want the raw header", out.ContentType)
	}
	if string(body) != "<html>page</html>" {
		t.Errorf("Body = %q, want the page", body)
	}
	if out.FinalURL != srv.URL {
		t.Errorf("FinalURL = %q, want %q", out.FinalURL, srv.URL)
	}

	// The user agent is the configuration's, not a constant, so the name this
	// crawler is matched by in a robots.txt is the name it answers to on the wire.
	// Two different strings means being held to rules addressed to somebody else.
	if ua != defaultUserAgent(m.cfg) {
		t.Errorf("User-Agent on the wire = %q, want %q", ua, defaultUserAgent(m.cfg))
	}
}

// TestFetchReportsAFailedStatusWithoutInventingAnError is the split Classify
// depends on. A 503 is an answer, and an answer has a status; folding it into the
// error channel would lose the one piece of information that says "this host is
// there and unhappy" as opposed to "this host is gone".
func TestFetchReportsAFailedStatusWithoutInventingAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithFetchClient(srv.Client())

	_, out := m.Fetch(context.Background(), srv.URL)
	if out.Err != nil {
		t.Errorf("Err = %v, want nil: a status is an answer, not a failure to reach one", out.Err)
	}
	if out.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", out.StatusCode)
	}
}

func TestFetchReportsATransportFailureWithNoStatus(t *testing.T) {
	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))

	// Nothing is listening on port 0, so this fails before any status exists.
	body, out := m.Fetch(context.Background(), "http://127.0.0.1:0/")
	if out.Err == nil {
		t.Fatal("Err = nil for an unreachable host")
	}
	if body != nil {
		t.Errorf("Body = %q, want nil", body)
	}
	if out.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0: there was no response to carry one", out.StatusCode)
	}
}

// TestFetchKeepsTheStatusWhenTheBodyFailsHalfwayThrough is the case the comment on
// NewHTTPFetcher is about. A server that answers 200 and then drops the connection
// has told us something a bare transport error would not: it exists, it is
// reachable, and it is worth trying again. Losing the status turns that into "no
// idea", and a host that is genuinely flaky gets treated like one that is gone.
func TestFetchKeepsTheStatusWhenTheBodyFailsHalfwayThrough(t *testing.T) {
	// Announce more content than is sent, then close: the client reads a 200 and
	// then an unexpected EOF.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Closing without the rest panics the handler's connection in a way httptest
		// turns into a short read on the client, which is what this needs.
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithFetchClient(srv.Client())

	_, out := m.Fetch(context.Background(), srv.URL)
	if out.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200: the server did answer, and throwing that "+
			"away classifies a flaky host as a vanished one", out.StatusCode)
	}
	if out.Err == nil {
		t.Error("Err = nil, want the truncated read to be reported")
	}
}

// TestFetchHonoursTheConfiguredBodyCap is why NewHTTPFetcher takes its caps from
// the configuration. A crawler pointed at the wrong URL will otherwise read until
// the process dies, and the cap is the last thing between that and a crash.
func TestFetchHonoursTheConfiguredBodyCap(t *testing.T) {
	const cap = 8192
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, cap*2))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MaxBodyBytes = cap
	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithConfig(cfg).WithFetchClient(srv.Client())

	_, out := m.Fetch(context.Background(), srv.URL)
	// Exactly one byte past the cap, and not merely "more than the cap". The extra
	// byte is what makes truncation detectable at all -- a body read to exactly the
	// limit is indistinguishable from one that ended there -- so an assertion of
	// "greater than" would pass just as happily against a fetcher that read the
	// whole response and ignored the cap entirely.
	if out.BytesRead != cap+1 {
		t.Errorf("BytesRead = %d, want exactly %d -- one byte past the %d cap, so "+
			"truncation is visible without the rest of the body being read",
			out.BytesRead, cap+1, cap)
	}
}

// TestWithConfigDoesNotReplaceACallerSuppliedFetcher is a wiring hazard with a real
// failure mode.
//
// WithFetcher hands the transport to the caller, so a test can script outcomes
// nobody can produce over a real network. If WithConfig then rebuilt the built-in
// fetcher, the caller's fetcher would be silently discarded the moment anything
// reconfigured the manager -- and the test would stop testing anything, which is
// the version of this bug that survives review.
func TestWithConfigDoesNotReplaceACallerSuppliedFetcher(t *testing.T) {
	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))

	var calls atomic.Int64
	m = m.WithFetcher(func(context.Context, string) ([]byte, Outcome) {
		calls.Add(1)
		return []byte("scripted"), Outcome{StatusCode: 200}
	})

	cfg := DefaultConfig()
	cfg.MaxBodyBytes = 1
	m = m.WithConfig(cfg)

	body, out := m.Fetch(context.Background(), "https://nowhere.example/never-dns")
	if calls.Load() != 1 {
		t.Errorf("the caller's fetcher ran %d times after a reconfigure, want 1", calls.Load())
	}
	if out.StatusCode != 200 || string(body) != "scripted" {
		t.Errorf("Fetch = %q/%d, want the scripted fetcher's answer", body, out.StatusCode)
	}
}

// TestWithConfigRebuildsTheBuiltInFetcher is the mirror: a caller who never
// installed one must still get a fetcher built from the new configuration, or
// raising MAX_REDIRECTS in the environment would do nothing at all.
//
// Written against a redirect chain rather than a body size so that the assertion is
// about the *rebuilt* fetcher: a body-size change is invisible on a small response,
// and a test that passes for the wrong reason is worse than no test.
func TestWithConfigRebuildsTheBuiltInFetcher(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Two hops: enough to be refused by a limit of 1 and followed by a limit of 5.
		if hits.Add(1) <= 2 {
			http.Redirect(w, r, srv.URL+"/hop", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("arrived"))
	}))
	defer srv.Close()

	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithFetchClient(srv.Client())

	// The default limit follows the chain to the end.
	_, out := m.Fetch(context.Background(), srv.URL)
	if out.StatusCode != 200 {
		t.Fatalf("with the default limit: StatusCode = %d, want 200", out.StatusCode)
	}
	// The hop count is what turns "redirect limit reached" into a decision.
	// Classify reads it and refuses the page permanently, so a fetch that reported
	// zero redirects would report a perfectly good 200 from the far end of a chain
	// the caller had told us to stop following.
	if out.Redirects != 2 {
		t.Errorf("with the default limit: Redirects = %d, want 2", out.Redirects)
	}

	hits.Store(0)
	cfg := DefaultConfig()
	cfg.MaxRedirects = 1
	m = m.WithConfig(cfg)

	_, out = m.Fetch(context.Background(), srv.URL)
	if out.StatusCode != http.StatusFound {
		t.Errorf("after WithConfig with MaxRedirects=1: StatusCode = %d, want the 302, "+
			"so the fetcher was rebuilt around the new configuration", out.StatusCode)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("the server saw %d requests, want 2", got)
	}
	if out.Redirects != 2 {
		t.Errorf("after WithConfig with MaxRedirects=1: Redirects = %d, want 2 -- the "+
			"hop count is what Classify uses to tell a refused chain from a redirect "+
			"that was followed", out.Redirects)
	}
}

func TestFetchWithoutATransportIsAClassifiableFailure(t *testing.T) {
	// A manager assembled as a struct literal rather than through New. A nil
	// dereference here would take the process down over one worker's copy, so the
	// failure has to arrive as an Outcome Classify can read.
	st := NewMemoryState()
	m := &PolicyManager{state: st, log: testLogger(), cfg: DefaultConfig(), now: time.Now}

	body, out := m.Fetch(context.Background(), "https://example.com/")
	if out.Err == nil {
		t.Fatal("Err = nil for a manager with no transport")
	}
	if !errors.Is(out.Err, errNoFetcher) {
		t.Errorf("Err = %v, want errNoFetcher", out.Err)
	}
	if body != nil {
		t.Errorf("Body = %q, want nil", body)
	}
}

// TestDiscardRetiresAndCounts covers the post-fetch refusal: a page that was
// fetched perfectly and is not worth indexing.
//
// Before Discard existed this was a bare `return` in the crawl loop, which means
// the URL stayed in the frontier's history unrecorded and came back on every
// rediscovery, and "how much are we indexing and how much are we dropping" had no
// answer at all.
func TestDiscardRetiresAndCounts(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/fr"

	for _, reason := range []Reason{ReasonLanguageNotEnglish, ReasonBodyUnparseable} {
		if err := m.Discard(ctx, target, reason); err != nil {
			t.Fatalf("Discard(%q): %v", reason, err)
		}
		visited, err := st.IsVisited(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if !visited {
			t.Fatalf("%s did not retire the URL", reason)
		}
	}

	stats := st.Stats("example.com")
	for _, reason := range []Reason{ReasonLanguageNotEnglish, ReasonBodyUnparseable} {
		if stats[reason] != 1 {
			t.Errorf("stats[%s] = %d, want 1 (all: %v)", reason, stats[reason], stats)
		}
	}
}

func TestDiscardRefusesToGuessAReason(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/fr"

	if err := m.Discard(ctx, target, ""); !errors.Is(err, errNoDiscardReason) {
		t.Errorf("Discard with no reason = %v, want errNoDiscardReason", err)
	}
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("the URL was retired anyway; there is no default reason that would be " +
			"better than refusing, because an uncounted refusal is invisible")
	}
	if n := st.StatsTotal("example.com"); n != 0 {
		t.Errorf("counted %d reasons for a refused discard, want 0", n)
	}
}

func TestDiscardReportsAFailedRetire(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	ctx := context.Background()

	st.FailOn = map[string]error{"MarkVisited": errors.New("redis went away")}
	if err := m.Discard(ctx, "https://example.com/fr", ReasonBodyUnparseable); err == nil {
		t.Fatal("expected an error when the URL could not be retired")
	}
}
