package utils

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetReqZeroAttemptsStillMakesOneRequest is the regression test for the
// reachable-from-data defect.
//
// host.MaxRetry is stored per host in Postgres, and `fetchAndParse` passes it
// straight through as maxRetry. A row with MaxRetry = 0 -- an older schema, a
// hand-edited row, a zero-value JSON blob -- used to make `for attempt := range
// maxRetry` iterate zero times and return
//
//	all 0 retries failed: %!w(<nil>)
//
// so every page under that host failed forever with an error that named no
// cause. One attempt is the floor: a crawl is never worse than trying once.
func TestGetReqZeroAttemptsStillMakesOneRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()

	for _, maxAttempts := range []int{0, -1, -100} {
		hits.Store(0)
		body, code, err := GetReq(srv.Client(), srv.URL, maxAttempts, 0)
		if err != nil {
			t.Fatalf("maxAttempts=%d: unexpected error: %v", maxAttempts, err)
		}
		if code != http.StatusOK {
			t.Errorf("maxAttempts=%d: status = %d, want 200", maxAttempts, code)
		}
		if string(body) != "<html>hello</html>" {
			t.Errorf("maxAttempts=%d: body = %q", maxAttempts, body)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("maxAttempts=%d: made %d requests, want exactly 1", maxAttempts, got)
		}
	}
}

func TestGetReqReturnsTheBodyAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body text"))
	}))
	defer srv.Close()

	body, code, err := GetReq(srv.Client(), srv.URL, 1, 0)
	if err != nil {
		t.Fatalf("GetReq() error: %v", err)
	}
	if code != 200 {
		t.Errorf("status = %d, want 200", code)
	}
	if string(body) != "body text" {
		t.Errorf("body = %q, want %q", body, "body text")
	}
}

func TestGetReqSendsCrawlerHeaders(t *testing.T) {
	// Many sites return 403 to the Go default User-Agent, so this is load
	// bearing rather than cosmetic.
	var gotUA, gotAccept, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotLang = r.Header.Get("Accept-Language")
	}))
	defer srv.Close()

	if _, _, err := GetReq(srv.Client(), srv.URL, 1, 0); err != nil {
		t.Fatalf("GetReq() error: %v", err)
	}

	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q, want a browser string", gotUA)
	}
	if gotAccept != "text/html" {
		t.Errorf("Accept = %q, want text/html", gotAccept)
	}
	if gotLang == "" {
		t.Error("Accept-Language was not set")
	}
}

func TestGetReqRejectsAMalformedURL(t *testing.T) {
	// No request is even attempted; the caller needs to know the URL is bad
	// rather than that the network is down.
	_, code, err := GetReq(http.DefaultClient, "://not a url", 3, 0)
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if code != 0 {
		t.Errorf("status = %d, want 0 for a request that was never sent", code)
	}
	if !strings.Contains(err.Error(), "request initialization failed") {
		t.Errorf("error = %v, want it to name the request construction failure", err)
	}
}

func TestGetReqRequiresAClient(t *testing.T) {
	// A nil client would panic inside the transport. A clear error is better.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetReq panicked on a nil client: %v", r)
		}
	}()

	if _, _, err := GetReq(nil, "https://example.com", 1, 0); err == nil {
		t.Error("expected an error for a nil client")
	}
}

// ── attempt accounting ───────────────────────────────────────────────────────

// TestMaxAttemptsIsTotalAttemptsNotRetries pins the contract the spider's
// callers rely on: the value is the number of requests made, not the number of
// retries after the first one.
func TestMaxAttemptsIsTotalAttemptsNotRetries(t *testing.T) {
	for _, tc := range []struct{ maxAttempts, wantHits int }{
		{1, 1},
		{2, 2},
		{3, 3},
		{5, 5},
	} {
		t.Run(fmt.Sprintf("maxAttempts=%d", tc.maxAttempts), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()

			_, _, err := GetReq(srv.Client(), srv.URL, tc.maxAttempts, 0)
			if err == nil {
				t.Fatal("expected an error after exhausting the attempts")
			}
			if got := hits.Load(); got != int32(tc.wantHits) {
				t.Errorf("made %d requests, want %d", got, tc.wantHits)
			}
		})
	}
}

func TestGetReqRetriesServerErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("recovered"))
	}))
	defer srv.Close()

	body, code, err := GetReq(srv.Client(), srv.URL, 3, 0)
	if err != nil {
		t.Fatalf("GetReq() error: %v", err)
	}
	if code != 200 {
		t.Errorf("status = %d, want 200", code)
	}
	if string(body) != "recovered" {
		t.Errorf("body = %q, want %q", body, "recovered")
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("made %d requests, want 3", got)
	}
}

func TestGetReqRetriesRateLimiting(t *testing.T) {
	// 429 is the server explicitly saying "come back later", so it is retried.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, code, err := GetReq(srv.Client(), srv.URL, 2, 0); err != nil || code != 200 {
		t.Fatalf("GetReq() = (%d, %v), want (200, nil)", code, err)
	}
}

func TestGetReqDoesNotRetryClientErrors(t *testing.T) {
	// Retrying a 403 or 404 is what gets a crawler blocked.
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			body, code, err := GetReq(srv.Client(), srv.URL, 5, 0)
			if err == nil {
				t.Fatal("expected an error for a 4xx response")
			}
			if body != nil {
				t.Errorf("body = %q, want nil for a 4xx response", body)
			}
			if code != status {
				t.Errorf("status = %d, want %d", code, status)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("made %d requests, want exactly 1; 4xx must not be retried", got)
			}
		})
	}
}

func TestGetReqReportsTheLastStatusOnExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, code, err := GetReq(srv.Client(), srv.URL, 2, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the caller can see what the server said", code)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want it to mention the status", err)
	}
}

func TestGetReqErrorsNeverWrapNil(t *testing.T) {
	// `fmt.Errorf("...: %w", err)` with a nil err renders "%!w(<nil>)", which
	// hides the real cause from every log line and every error check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	for _, maxAttempts := range []int{0, 1, 3} {
		_, _, err := GetReq(srv.Client(), srv.URL, maxAttempts, 0)
		if err == nil {
			t.Fatalf("maxAttempts=%d: expected an error", maxAttempts)
		}
		if strings.Contains(err.Error(), "%!w(<nil>)") || strings.Contains(err.Error(), "<nil>") {
			t.Errorf("maxAttempts=%d: error wraps nil: %v", maxAttempts, err)
		}
		if errors.Unwrap(err) == nil {
			t.Errorf("maxAttempts=%d: error has no wrapped cause: %v", maxAttempts, err)
		}
	}
}

func TestGetReqReportsConnectionFailures(t *testing.T) {
	// Port 0 on the loopback interface is closed; the transport fails before
	// any HTTP status exists.
	_, code, err := GetReq(http.DefaultClient, "http://127.0.0.1:0/", 2, 0)
	if err == nil {
		t.Fatal("expected an error for an unreachable host")
	}
	if code != 0 {
		t.Errorf("status = %d, want 0 when no response was received", code)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Errorf("error = %v, want it to report the attempt count", err)
	}
}

func TestGetReqReportsTheAttemptCountItMade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, _, err := GetReq(srv.Client(), srv.URL, 4, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "4 attempts") {
		t.Errorf("error = %v, want it to say it made 4 attempts", err)
	}
}

// ── body size limit ──────────────────────────────────────────────────────────

func TestGetReqTruncatesOversizedResponses(t *testing.T) {
	const size = MaxResponseBytes + 4096
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 32*1024)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for written := 0; written < size; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	body, _, err := GetReq(srv.Client(), srv.URL, 1, 0)
	if err != nil {
		t.Fatalf("GetReq() error: %v", err)
	}
	if len(body) != MaxResponseBytes {
		t.Errorf("read %d bytes, want the %d byte cap", len(body), MaxResponseBytes)
	}
}

func TestGetReqAcceptsAResponseAtExactlyTheLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("b", 64*1024)
		for written := 0; written < MaxResponseBytes; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	body, _, err := GetReq(srv.Client(), srv.URL, 1, 0)
	if err != nil {
		t.Fatalf("GetReq() error: %v", err)
	}
	if len(body) != MaxResponseBytes {
		t.Errorf("read %d bytes, want %d", len(body), MaxResponseBytes)
	}
}

// ── delay ────────────────────────────────────────────────────────────────────

func TestGetReqWaitsBetweenAttemptsButNotBeforeTheFirst(t *testing.T) {
	var stamps []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamps = append(stamps, time.Now())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	start := time.Now()
	if _, _, err := GetReq(srv.Client(), srv.URL, 3, 1); err == nil {
		t.Fatal("expected an error")
	}

	// Three attempts with a one second delay before the second and third.
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("total elapsed %v, want at least 2s for two delays", elapsed)
	}
	if len(stamps) != 3 {
		t.Fatalf("got %d requests, want 3", len(stamps))
	}
	if first := stamps[0].Sub(start); first > 500*time.Millisecond {
		t.Errorf("the first attempt waited %v; it must not sleep before trying", first)
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 900*time.Millisecond {
		t.Errorf("gap between attempt 1 and 2 was %v, want about 1s", gap)
	}
	if gap := stamps[2].Sub(stamps[1]); gap < 900*time.Millisecond {
		t.Errorf("gap between attempt 2 and 3 was %v, want about 1s", gap)
	}
}

func TestGetReqTreatsANegativeDelayAsNoDelay(t *testing.T) {
	// Delay comes from the host row too, so a negative value is reachable from
	// data. A negative sleep is a no-op anyway, but it must not panic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	start := time.Now()
	if _, _, err := GetReq(srv.Client(), srv.URL, 2, -5); err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("elapsed %v, want no delay for a negative delay", elapsed)
	}
}
