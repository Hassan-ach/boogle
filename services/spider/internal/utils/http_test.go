package utils

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetReqMakesExactlyOneRequest(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			if _, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{}); err != nil {
				t.Fatalf("GetReq: %v", err)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("made %d requests, want exactly 1", got)
			}
		})
	}
}

func TestGetReqReportsAFailedStatusWithoutAnError(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{})
			if err != nil {
				t.Fatalf("GetReq returned an error for a %d: %v", status, err)
			}
			if res.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", res.StatusCode, status)
			}
		})
	}
}

func TestGetReqReturnsTheBodyAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body text"))
	}))
	defer srv.Close()

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if string(res.Body) != "body text" {
		t.Errorf("Body = %q, want %q", res.Body, "body text")
	}
	if res.BytesRead != len("body text") {
		t.Errorf("BytesRead = %d, want %d", res.BytesRead, len("body text"))
	}
	if res.ContentType != "text/html; charset=utf-8" {
		t.Errorf("ContentType = %q, want the raw header", res.ContentType)
	}
}

func TestGetReqSendsCrawlerHeaders(t *testing.T) {
	var gotUA, gotAccept, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotLang = r.Header.Get("Accept-Language")
	}))
	defer srv.Close()

	if _, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{}); err != nil {
		t.Fatalf("GetReq: %v", err)
	}

	if !strings.Contains(gotUA, "Mozilla/5.0") {
		t.Errorf("User-Agent = %q, want a browser string by default", gotUA)
	}
	if gotAccept != "text/html" {
		t.Errorf("Accept = %q, want text/html", gotAccept)
	}
	if gotLang == "" {
		t.Error("Accept-Language was not set")
	}
}

func TestGetReqUsesTheStatedUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	if _, err := GetReq(context.Background(), srv.Client(), srv.URL,
		GetOptions{UserAgent: "BoogleBot/1.0"}); err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if gotUA != "BoogleBot/1.0" {
		t.Errorf("User-Agent = %q, want the one the caller stated", gotUA)
	}
}

func TestGetReqRejectsAMalformedURL(t *testing.T) {
	res, err := GetReq(context.Background(), http.DefaultClient, "://not a url", GetOptions{})
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if res != nil {
		t.Errorf("Response = %+v, want nil for a request that was never sent", res)
	}
	if !strings.Contains(err.Error(), "request initialization failed") {
		t.Errorf("error = %v, want it to name the request construction failure", err)
	}
}

func TestGetReqRequiresAClient(t *testing.T) {
	res, err := GetReq(context.Background(), nil, "https://example.com", GetOptions{})
	if err == nil {
		t.Fatal("expected an error for a nil client")
	}
	if res != nil {
		t.Errorf("Response = %+v, want nil", res)
	}
}

func TestGetReqHonoursCancellation(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := GetReq(ctx, srv.Client(), srv.URL, GetOptions{})
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled so a caller can tell "+
			"a shutdown from a dead host", err)
	}
	if res != nil {
		t.Errorf("Response = %+v, want nil", res)
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the server saw %d requests on a cancelled context, want 0", got)
	}
}

func TestGetReqReportsConnectionFailures(t *testing.T) {
	res, err := GetReq(context.Background(), http.DefaultClient, "http://127.0.0.1:0/", GetOptions{})
	if err == nil {
		t.Fatal("expected an error for an unreachable host")
	}
	if res != nil {
		t.Errorf("Response = %+v, want nil when no response was received", res)
	}
	if errors.Unwrap(err) == nil {
		t.Errorf("error has no wrapped cause, so no caller can classify it: %v", err)
	}
}

func redirectChain(t *testing.T, hops int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if int(n) > hops {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("%s/hop%d", srv.URL, n))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestGetReqStopsAtTheRedirectLimit(t *testing.T) {
	const limit = 3
	srv, hits := redirectChain(t, 20)

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{MaxRedirects: limit})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if got := hits.Load(); got != limit+1 {
		t.Errorf("the server saw %d requests, want %d (the limit plus the refused hop)",
			got, limit+1)
	}
	if res.Redirects != limit+1 {
		t.Errorf("Redirects = %d, want %d so the caller can see the chain was cut",
			res.Redirects, limit+1)
	}
	if res.StatusCode != http.StatusFound {
		t.Errorf("StatusCode = %d, want the 302 that refused the hop rather than a "+
			"fabricated failure", res.StatusCode)
	}
}

func TestGetReqFollowsAChainUnderTheLimit(t *testing.T) {
	srv, hits := redirectChain(t, 2)

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{MaxRedirects: 5})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("the server saw %d requests, want 3", got)
	}
	if res.Redirects != 2 {
		t.Errorf("Redirects = %d, want 2", res.Redirects)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if !strings.HasSuffix(res.FinalURL, "/hop2") {
		t.Errorf("FinalURL = %q, want the URL the chain ended at", res.FinalURL)
	}
}

func TestGetReqLeavesTheCallersRedirectPolicyAlone(t *testing.T) {
	sentinel := errors.New("caller policy")

	t.Run("a stated limit takes precedence", func(t *testing.T) {
		srv, hits := redirectChain(t, 1)
		client := srv.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return sentinel }

		if _, err := GetReq(context.Background(), client, srv.URL, GetOptions{MaxRedirects: 5}); err != nil {
			t.Fatalf("GetReq with a limit: %v", err)
		}
		if got := hits.Load(); got != 2 {
			t.Errorf("the server saw %d requests, want 2", got)
		}
	})

	t.Run("no limit leaves the caller's policy in force", func(t *testing.T) {
		srv, _ := redirectChain(t, 1)
		client := srv.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return sentinel }

		if _, err := GetReq(context.Background(), client, srv.URL, GetOptions{}); !errors.Is(err, sentinel) {
			t.Errorf("error = %v, want the caller's own redirect policy to still be in force", err)
		}
	})
}

func TestGetReqReportsATruncatedBodyAsTruncated(t *testing.T) {
	const cap = 4096
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", cap*2)))
	}))
	defer srv.Close()

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{MaxBytes: cap})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if res.BytesRead <= cap {
		t.Errorf("BytesRead = %d, want more than the %d cap so truncation is visible",
			res.BytesRead, cap)
	}
	if len(res.Body) != cap+1 {
		t.Errorf("read %d bytes, want the cap plus the one byte that proves it", len(res.Body))
	}
}

func TestGetReqAcceptsAResponseAtExactlyTheLimit(t *testing.T) {
	const cap = 4096
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("b", cap)))
	}))
	defer srv.Close()

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{MaxBytes: cap})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if res.BytesRead != cap {
		t.Errorf("BytesRead = %d, want exactly the %d cap: a body that ends at the cap "+
			"is complete, and treating it as truncated would drop every page of exactly "+
			"that size", res.BytesRead, cap)
	}
}

func TestGetReqFallsBackToThePackageCap(t *testing.T) {
	for _, stated := range []int{0, -1} {
		t.Run(fmt.Sprintf("MaxBytes=%d", stated), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(strings.Repeat("c", 1024)))
			}))
			defer srv.Close()

			res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{MaxBytes: stated})
			if err != nil {
				t.Fatalf("GetReq: %v", err)
			}
			if res.BytesRead != 1024 {
				t.Errorf("BytesRead = %d, want 1024", res.BytesRead)
			}
		})
	}
}

func TestGetReqDefaultCapIsUsed(t *testing.T) {
	if MaxResponseBytes <= 0 {
		t.Fatal("MaxResponseBytes must be positive; it is the last bound on a read")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("small"))
	}))
	defer srv.Close()

	res, err := GetReq(context.Background(), srv.Client(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("GetReq: %v", err)
	}
	if res.BytesRead != 5 {
		t.Errorf("BytesRead = %d, want 5", res.BytesRead)
	}
}

func TestGetReqNilContextDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	res, err := GetReq(nil, srv.Client(), srv.URL, GetOptions{})
	if err != nil {
		t.Fatalf("GetReq with a nil context: %v", err)
	}
	if res == nil {
		t.Error("Response = nil, want the exchange to have happened")
	}
}

func TestGetReqUsesAControlledDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	client := srv.Client()
	client.Timeout = 50 * time.Millisecond

	start := time.Now()
	res, err := GetReq(context.Background(), client, srv.URL, GetOptions{})
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if res != nil {
		t.Errorf("Response = %+v, want nil", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("elapsed %v, want the client's 50ms timeout to be honoured", elapsed)
	}
}
