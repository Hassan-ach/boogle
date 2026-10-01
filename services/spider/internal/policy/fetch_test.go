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

	if ua != defaultUserAgent(m.cfg) {
		t.Errorf("User-Agent on the wire = %q, want %q", ua, defaultUserAgent(m.cfg))
	}
}

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

func TestFetchKeepsTheStatusWhenTheBodyFailsHalfwayThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
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
	if out.BytesRead != cap+1 {
		t.Errorf("BytesRead = %d, want exactly %d -- one byte past the %d cap, so "+
			"truncation is visible without the rest of the body being read",
			out.BytesRead, cap+1, cap)
	}
}

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

func TestWithConfigRebuildsTheBuiltInFetcher(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Redirect(w, r, srv.URL+"/hop", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("arrived"))
	}))
	defer srv.Close()

	m, _, _ := atClock(t, time.Date(2026, 6, 5, 8, 0, 0, 0, time.UTC))
	m = m.WithFetchClient(srv.Client())

	_, out := m.Fetch(context.Background(), srv.URL)
	if out.StatusCode != 200 {
		t.Fatalf("with the default limit: StatusCode = %d, want 200", out.StatusCode)
	}
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
