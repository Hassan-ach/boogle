package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name     string
		out      Outcome
		attempts int
		maxRetry int
		wantKind ActionKind
		wantWhy  Reason
	}{
		{
			name:     "200 is a success",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", BytesRead: 4096},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "299 is still a success",
			out:      Outcome{StatusCode: 299, ContentType: "text/html"},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "304 means the content is fine",
			out:      Outcome{StatusCode: 304},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "absent content type is accepted",
			out:      Outcome{StatusCode: 200, ContentType: ""},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "xhtml is accepted",
			out:      Outcome{StatusCode: 200, ContentType: "application/xhtml+xml; charset=utf-8"},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},

		{
			name:     "404 is permanent",
			out:      Outcome{StatusCode: 404},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonNotFound,
		},
		{
			name:     "410 is permanent",
			out:      Outcome{StatusCode: 410},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonGone,
		},
		{
			name:     "403 is permanent for this url",
			out:      Outcome{StatusCode: 403},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonForbidden,
		},
		{
			name:     "401 is permanent",
			out:      Outcome{StatusCode: 401},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonUnauthorized,
		},
		{
			name:     "400 is permanent",
			out:      Outcome{StatusCode: 400},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonBadRequest,
		},
		{
			name:     "405 is permanent",
			out:      Outcome{StatusCode: 405},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonBadRequest,
		},
		{
			name:     "451 is permanent",
			out:      Outcome{StatusCode: 451},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonBadRequest,
		},
		{
			name:     "an unlisted 4xx is still a client error",
			out:      Outcome{StatusCode: 418},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonBadRequest,
		},
		{
			name:     "permanence is not gated by attempt count",
			out:      Outcome{StatusCode: 404},
			attempts: 9,
			maxRetry: 10,
			wantKind: ActPermanent,
			wantWhy:  ReasonNotFound,
		},
		{
			name:     "a permanent body verdict survives an exhausted budget",
			out:      Outcome{StatusCode: 200, ContentType: "application/pdf"},
			attempts: 4,
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonContentTypeRejected,
		},

		{
			name:     "429 backs off rather than abandoning a page that exists",
			out:      Outcome{StatusCode: 429},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonRateLimited,
		},
		{
			name:     "500 backs off",
			out:      Outcome{StatusCode: 500},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonServerError,
		},
		{
			name:     "503 backs off",
			out:      Outcome{StatusCode: 503},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonServerError,
		},
		{
			name:     "599 backs off",
			out:      Outcome{StatusCode: 599},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonServerError,
		},
		{
			name:     "an unrecognised code backs off rather than dropping the url",
			out:      Outcome{StatusCode: 102},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonServerError,
		},

		{
			name:     "dns failure is a dead host",
			out:      Outcome{Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonDNSFailure,
		},
		{
			name:     "connection refused is a dead host",
			out:      Outcome{Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonConnectionRefused,
		},
		{
			name:     "connection reset is a dead host",
			out:      Outcome{Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonConnectionRefused,
		},
		{
			name:     "unreachable network is a dead host",
			out:      Outcome{Err: &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonConnectionRefused,
		},
		{
			name:     "unknown certificate authority is a tls failure",
			out:      Outcome{Err: x509.UnknownAuthorityError{}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTLSError,
		},
		{
			name:     "expired certificate is a tls failure",
			out:      Outcome{Err: x509.CertificateInvalidError{Reason: x509.Expired}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTLSError,
		},
		{
			name:     "hostname mismatch is a tls failure",
			out:      Outcome{Err: x509.HostnameError{Host: "example.com"}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTLSError,
		},
		{
			name:     "garbage on the wire is a tls failure",
			out:      Outcome{Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTLSError,
		},
		{
			name:     "a wrapped verification failure is a tls failure",
			out:      Outcome{Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTLSError,
		},
		{
			name:     "an unrecognised error backs off rather than dropping the url",
			out:      Outcome{Err: errors.New("something we have never seen")},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonServerError,
		},

		{
			name:     "a net timeout is not a dead host",
			out:      Outcome{Err: timeoutErr{msg: "i/o timeout"}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTimeout,
		},
		{
			name:     "a context deadline is not a dead host",
			out:      Outcome{Err: context.DeadlineExceeded},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTimeout,
		},
		{
			name:     "a timing-out dns lookup is a timeout, not a dead host",
			out:      Outcome{Err: &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTimeout,
		},
		{
			name:     "a timeout wrapped by url.Error is still a timeout",
			out:      Outcome{Err: &urlError{err: timeoutErr{msg: "i/o timeout"}}},
			maxRetry: 5,
			wantKind: ActBackoff,
			wantWhy:  ReasonTimeout,
		},

		{
			name:     "a redirect loop is permanent",
			out:      Outcome{Err: ErrRedirectLoop, StatusCode: 302},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			name:     "a redirect loop ending on a 200 is still permanent",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", Err: ErrRedirectLoop},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			name:     "an over-limit hop count beats a 200 status",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", Redirects: 11},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			name:     "an over-limit redirect count is a loop even without the sentinel",
			out:      Outcome{StatusCode: 200, Redirects: 11},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			name:     "a redirect count exactly at the limit is allowed",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", Redirects: 10},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "a surfaced 3xx is permanent",
			out:      Outcome{StatusCode: 301},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},

		{
			name:     "a body past the cap is truncated and dropped",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", BytesRead: cfg.MaxBodyBytes + 1},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonBodyTooLarge,
		},
		{
			name:     "a body exactly at the cap is not truncation",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", BytesRead: cfg.MaxBodyBytes},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			name:     "a pdf served with a 200 is refused",
			out:      Outcome{StatusCode: 200, ContentType: "application/pdf"},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonContentTypeRejected,
		},
		{
			name:     "an image served with a 200 is refused",
			out:      Outcome{StatusCode: 200, ContentType: "image/png"},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonContentTypeRejected,
		},
		{
			name:     "content type comparison ignores case and spacing",
			out:      Outcome{StatusCode: 200, ContentType: "  TEXT/HTML ; charset=UTF-8 "},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.out, tc.attempts, tc.maxRetry, &cfg)
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %v, want %v (reason %q)", got.Kind, tc.wantKind, got.Reason)
			}
			if got.Reason != tc.wantWhy {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

func TestClassifyAttemptCeiling(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name     string
		attempts int
		maxRetry int
		wantKind ActionKind
		wantWhy  Reason
	}{
		{"first of five attempts still backs off", 0, 5, ActBackoff, ReasonServerError},
		{"fourth of five attempts still backs off", 3, 5, ActBackoff, ReasonServerError},
		{"fifth of five attempts is the last one", 4, 5, ActPermanent, ReasonAttemptsExhausted},
		{"past the ceiling stays permanent", 12, 5, ActPermanent, ReasonAttemptsExhausted},
		{"maxRetry of one permits exactly one attempt", 0, 1, ActPermanent, ReasonAttemptsExhausted},
		{"maxRetry of zero permits exactly one attempt", 0, 0, ActPermanent, ReasonAttemptsExhausted},
		{"negative maxRetry permits exactly one attempt", 0, -7, ActPermanent, ReasonAttemptsExhausted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(Outcome{StatusCode: 503}, tc.attempts, tc.maxRetry, &cfg)
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.Reason != tc.wantWhy {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

func TestClassifyStatusOutranksError(t *testing.T) {
	cfg := DefaultConfig()

	got := classify(Outcome{
		StatusCode: 404,
		Err:        errors.New("some transport complaint"),
	}, 0, 5, &cfg)

	if got.Kind != ActPermanent {
		t.Errorf("kind = %v, want %v", got.Kind, ActPermanent)
	}
	if got.Reason != ReasonNotFound {
		t.Errorf("reason = %q, want %q -- a status is the server's final answer", got.Reason, ReasonNotFound)
	}
}

func TestClassifyTimeoutDoesNotUseTheDeadSchedule(t *testing.T) {
	cfg := DefaultConfig()

	refused := classify(Outcome{Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, 0, 5, &cfg)
	timeout := classify(Outcome{Err: timeoutErr{msg: "i/o timeout"}}, 0, 5, &cfg)

	if refused.Reason != ReasonConnectionRefused {
		t.Fatalf("refused reason = %q, want %q", refused.Reason, ReasonConnectionRefused)
	}
	if timeout.Reason != ReasonTimeout {
		t.Fatalf("timeout reason = %q, want %q", timeout.Reason, ReasonTimeout)
	}
	if refused.RetryAfter <= timeout.RetryAfter {
		t.Errorf("refused retry %v should outlast timeout retry %v: a refused host is "+
			"gone and its urls must wait for the dead marker to expire, while a slow "+
			"host is fine and only needs a page-level delay",
			refused.RetryAfter, timeout.RetryAfter)
	}
}

func TestClassifyDeadRetryOutlivesDeadMarker(t *testing.T) {
	cfg := DefaultConfig()

	for attempts := 0; attempts < 8; attempts++ {
		got := classify(Outcome{Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, attempts, 20, &cfg)

		marker := cfg.DeadHostTTL(attempts + 1)
		if got.RetryAfter < marker {
			t.Errorf("attempts=%d: retry in %v, dead marker lasts %v -- the url would "+
				"come back to a host that is still marked dead and be skipped for good",
				attempts, got.RetryAfter, marker)
		}
		if got.RetryAfter < cfg.DeadProbeAfter {
			t.Errorf("attempts=%d: retry in %v, below the %v probe floor",
				attempts, got.RetryAfter, cfg.DeadProbeAfter)
		}
	}
}

func TestClassifyBackoffGrows(t *testing.T) {
	cfg := DefaultConfig()

	prev := time.Duration(0)
	for attempts := 0; attempts < 4; attempts++ {
		got := classify(Outcome{StatusCode: 503}, attempts, 20, &cfg)
		if got.RetryAfter <= prev {
			t.Errorf("attempts=%d: retry %v did not grow past %v", attempts, got.RetryAfter, prev)
		}
		prev = got.RetryAfter
	}
}

func TestClassifyBackoffIsAlwaysPositive(t *testing.T) {
	cfg := DefaultConfig()

	outcomes := []Outcome{
		{StatusCode: 503},
		{StatusCode: 429},
		{Err: timeoutErr{msg: "i/o timeout"}},
		{Err: context.DeadlineExceeded},
		{Err: &net.DNSError{Err: "no such host"}},
		{Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
		{Err: x509.UnknownAuthorityError{}},
		{},
	}

	for _, out := range outcomes {
		for _, attempts := range []int{0, 1, 5, 50} {
			got := classify(out, attempts, 100, &cfg)
			if got.Kind != ActBackoff {
				continue
			}
			if got.RetryAfter <= 0 {
				t.Errorf("outcome %+v attempts=%d: RetryAfter = %v, must be positive",
					out, attempts, got.RetryAfter)
			}
		}
	}
}

func TestClassifyNoEvidenceDoesNotLoop(t *testing.T) {
	cfg := DefaultConfig()

	got := classify(Outcome{}, 0, 5, &cfg)
	if got.Kind != ActBackoff {
		t.Errorf("kind = %v, want %v -- dropping the url would lose a page we never tried", got.Kind, ActBackoff)
	}

	got = classify(Outcome{}, 4, 5, &cfg)
	if got.Kind != ActPermanent {
		t.Errorf("kind = %v, want %v -- the attempt ceiling must still apply", got.Kind, ActPermanent)
	}
}

func TestClassifySuccessCarriesNoRetryDelay(t *testing.T) {
	cfg := DefaultConfig()

	got := classify(Outcome{StatusCode: 200, ContentType: "text/html"}, 0, 5, &cfg)
	if got.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v on a success, want 0", got.RetryAfter)
	}
}

func TestRetryAfterDefaultIsThePageSchedule(t *testing.T) {
	cfg := DefaultConfig()

	got := retryAfterFor(Reason("some_reason_added_later"), 2, &cfg)
	if want := cfg.URLBackoff(2); got != want {
		t.Errorf("unlisted reason retry = %v, want the page schedule %v", got, want)
	}
	if got <= 0 {
		t.Errorf("unlisted reason retry = %v, must be positive", got)
	}
}

func TestRetryAfterNeverZero(t *testing.T) {
	cfg := DefaultConfig()

	reasons := []Reason{
		ReasonRateLimited, ReasonServerError, ReasonTimeout,
		ReasonDNSFailure, ReasonConnectionRefused, ReasonTLSError,
		ReasonFetchOK, ReasonNotFound, Reason(""),
	}
	for _, r := range reasons {
		for _, attempts := range []int{0, 1, 3, 7, 50} {
			if d := retryAfterFor(r, attempts, &cfg); d <= 0 {
				t.Errorf("retryAfterFor(%q, %d) = %v, must be positive", r, attempts, d)
			}
		}
	}
}

func TestKindStrings(t *testing.T) {
	verdicts := map[VerdictKind]string{Allow: "allow", Defer: "defer", Skip: "skip"}
	for kind, want := range verdicts {
		if got := kind.String(); got != want {
			t.Errorf("VerdictKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
	if got := VerdictKind(99).String(); got != "unknown" {
		t.Errorf("out-of-range verdict = %q, want %q", got, "unknown")
	}

	actions := map[ActionKind]string{ActSuccess: "success", ActBackoff: "backoff", ActPermanent: "permanent"}
	for kind, want := range actions {
		if got := kind.String(); got != want {
			t.Errorf("ActionKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
	if got := ActionKind(99).String(); got != "unknown" {
		t.Errorf("out-of-range action = %q, want %q", got, "unknown")
	}
}

type urlError struct{ err error }

func (e *urlError) Error() string { return fmt.Sprintf("Get %q: %v", "http://example.com", e.err) }
func (e *urlError) Unwrap() error { return e.err }

func mustClassify(t *testing.T, m *PolicyManager, rawURL string, out Outcome) *Action {
	t.Helper()
	act, err := m.Classify(context.Background(), rawURL, out)
	if err != nil {
		t.Fatalf("Classify(%q) returned an error: %v", rawURL, err)
	}
	if act == nil {
		t.Fatalf("Classify(%q) returned a nil action", rawURL)
	}
	return act
}

func TestClassifySuccessCountsThePageAndClearsTheFailures(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/ok"

	if _, err := st.RecordFailure(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}

	act := mustClassify(t, m, target, Outcome{StatusCode: 200, ContentType: "text/html"})
	if act.Kind != ActSuccess {
		t.Fatalf("Kind = %v, want success", act.Kind)
	}
	if act.Reason != ReasonFetchOK {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonFetchOK)
	}

	state, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if state.PagesCrawled != 1 {
		t.Errorf("PagesCrawled = %d, want 1", state.PagesCrawled)
	}
	if state.ConsecFailures != 0 {
		t.Errorf("ConsecFailures = %d, want 0; a host that recovered must start its "+
			"next backoff from the first step", state.ConsecFailures)
	}
	if !state.LastSuccess.Equal(now) {
		t.Errorf("LastSuccess = %v, want %v", state.LastSuccess, now)
	}

	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 0 {
		t.Errorf("attempts = %d after a success, want 0", urlState.Attempts)
	}
}

func TestClassifySuccessDoesNotRetireTheURL(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/ok"

	mustClassify(t, m, target, Outcome{StatusCode: 200})

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("a successful fetch marked the URL visited; a page that then fails to " +
			"persist would never be fetched again")
	}
}

func TestClassifyRetiresA404(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/gone"

	act := mustClassify(t, m, target, Outcome{StatusCode: 404})
	if act.Kind != ActPermanent {
		t.Fatalf("Kind = %v, want permanent", act.Kind)
	}
	if act.Reason != ReasonNotFound {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonNotFound)
	}

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a 404 was not retired; it will be fetched again on every rediscovery")
	}

	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 0 {
		t.Errorf("attempts = %d for a retired URL, want 0: the record is dead weight "+
			"waiting for its TTL", urlState.Attempts)
	}
}

func TestClassifyRetiresBeforeItClears(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/gone"

	if _, err := st.BumpAttempts(ctx, target); err != nil {
		t.Fatal(err)
	}
	st.FailOn = map[string]error{"ClearURLState": errors.New("redis went away mid-write")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 404})
	if err == nil {
		t.Fatal("expected an error when the decision could not be fully recorded")
	}
	if act != nil {
		t.Fatalf("Action = %+v, want nil: the retiring write landed but the decision "+
			"was only half recorded, so it is not a decision", act)
	}

	st.FailOn = nil
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("the URL was not retired even though the write that clears its record " +
			"failed; a failed clear must not undo the retiring write")
	}
}

func TestClassifyParksABackoffWithTheComputedDueTime(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, c := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/flaky"

	act := mustClassify(t, m, target, Outcome{StatusCode: 503})
	if act.Kind != ActBackoff {
		t.Fatalf("Kind = %v, want backoff", act.Kind)
	}
	if act.Reason != ReasonServerError {
		t.Errorf("Reason = %q, want %q", act.Reason, ReasonServerError)
	}
	if act.RetryAfter != m.cfg.URLBackoffBase {
		t.Errorf("RetryAfter = %v, want the base %v for a first failure", act.RetryAfter, m.cfg.URLBackoffBase)
	}

	urlState, err := st.URLState(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if urlState.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", urlState.Attempts)
	}

	if n, err := st.FrontierLen(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("frontier length = %d, want 0; a parked URL belongs in the delayed set", n)
	}

	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Idle {
		t.Errorf("TakeNext handed out %q before the retry was due", next.URL)
	}

	c.Advance(act.RetryAfter + time.Second)
	next, err = m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Idle || next.URL != target {
		t.Errorf("TakeNext = %+v, want %q once the retry was due", next, target)
	}
}

func TestClassifyGrowsTheDelayWithEachFailure(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, _, c := atClock(t, now)
	const target = "https://example.com/flaky"

	var last time.Duration
	for i := range 2 {
		act := mustClassify(t, m, target, Outcome{StatusCode: 503})
		if act.RetryAfter <= last {
			t.Fatalf("failure %d: RetryAfter = %v, want more than the previous %v",
				i+1, act.RetryAfter, last)
		}
		last = act.RetryAfter
		c.Advance(act.RetryAfter)
	}
}

func TestClassifyRetiresAURLThatHasRunOutOfAttempts(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, c := atClock(t, now)
	ctx := context.Background()
	const target = "https://example.com/never-works"

	var last *Action
	for range m.cfg.URLMaxAttempts {
		last = mustClassify(t, m, target, Outcome{StatusCode: 503})
		if last.Kind == ActPermanent {
			break
		}
		c.Advance(last.RetryAfter)
	}
	if last.Kind != ActPermanent {
		t.Fatalf("after %d attempts the kind is %v, want permanent", m.cfg.URLMaxAttempts, last.Kind)
	}
	if last.Reason != ReasonAttemptsExhausted {
		t.Errorf("Reason = %q, want %q: "+
			"a URL given up on for running out of attempts is a different fact from a "+
			"URL that answered 404, and the stats hash has to be able to say so",
			last.Reason, ReasonAttemptsExhausted)
	}

	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a URL that ran out of attempts was not retired; it would be retried for ever")
	}
}

func TestClassifyMarksADeadHostOnlyForConnectionFailures(t *testing.T) {
	tests := []struct {
		name       string
		out        Outcome
		wantDead   bool
		wantReason Reason
	}{
		{
			name:       "a refused dial means the host is gone",
			out:        Outcome{Err: errConnRefused},
			wantDead:   true,
			wantReason: ReasonConnectionRefused,
		},
		{
			name:       "a TLS failure means the host is gone",
			out:        Outcome{Err: errTLSHandshake},
			wantDead:   true,
			wantReason: ReasonTLSError,
		},
		{
			name:       "a timeout must never mark a host dead",
			out:        Outcome{Err: &timeoutError{}},
			wantDead:   false,
			wantReason: ReasonTimeout,
		},
		{
			name:       "a 503 must never mark a host dead",
			out:        Outcome{StatusCode: 503},
			wantDead:   false,
			wantReason: ReasonServerError,
		},
		{
			name:       "rate limiting must never mark a host dead",
			out:        Outcome{StatusCode: 429},
			wantDead:   false,
			wantReason: ReasonRateLimited,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
			m, st, _ := atClock(t, now)
			ctx := context.Background()
			host := "probe-" + itoa(len(tc.name)) + ".example"
			target := "https://" + host + "/page"

			act := mustClassify(t, m, target, tc.out)
			if act.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", act.Reason, tc.wantReason)
			}

			markers, err := st.Markers(ctx, host)
			if err != nil {
				t.Fatal(err)
			}
			_, dead := markers[MarkerDead]
			_, cooling := markers[MarkerCooldown]
			if dead != tc.wantDead {
				t.Errorf("dead marker present = %v, want %v (markers: %s)",
					dead, tc.wantDead, markerNames(markers))
			}
			if !tc.wantDead && !cooling {
				t.Errorf("no marker at all was raised for a failing host; markers: %s",
					markerNames(markers))
			}
		})
	}
}

func TestClassifyNeverMarksAHostDeadForATimeout(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "slow.example"
	const target = "https://slow.example/page"

	mustClassify(t, m, target, Outcome{Err: &timeoutError{}})

	markers, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if _, dead := markers[MarkerDead]; dead {
		t.Error("a timeout marked the host dead; its URLs would be answered from Redis " +
			"for an hour because it was slow once")
	}
	if _, cooling := markers[MarkerCooldown]; !cooling {
		t.Errorf("a timeout should cool the host down, got markers: %s", markerNames(markers))
	}
}

func TestTheCooldownRespectsTheRobotsCrawlDelay(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "h.example"
	const target = "https://h.example/page"

	if err := st.SaveHostState(ctx, host, HostState{
		Name:            host,
		CrawlDelay:      30 * time.Second,
		MaxPages:        100,
		RobotsFetchedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	mustClassify(t, m, target, Outcome{StatusCode: 503})

	markers, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	ttl, ok := markers[MarkerCooldown]
	if !ok {
		t.Fatalf("no cooldown was raised; markers: %s", markerNames(markers))
	}
	floor := m.cfg.HostCooldown(1, 30*time.Second)
	if ttl != floor {
		t.Errorf("cooldown ttl = %v, want %v (the site's Crawl-delay as the floor)", ttl, floor)
	}
	if ttl < 30*time.Second {
		t.Errorf("cooldown ttl = %v, want at least the 30s the site asked for", ttl)
	}
}

func TestTheDeadMarkerGrowsWithEachConsecutiveFailure(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "gone.example"

	mustClassify(t, m, "https://gone.example/a", Outcome{Err: &net.DNSError{Err: "no such host"}})
	first, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	firstTTL := first[MarkerDead]

	mustClassify(t, m, "https://gone.example/b", Outcome{Err: &net.DNSError{Err: "no such host"}})
	second, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.ClearMarker(ctx, host, MarkerDead); err != nil {
		t.Fatal(err)
	}
	mustClassify(t, m, "https://gone.example/c", Outcome{Err: &net.DNSError{Err: "no such host"}})
	third, err := st.Markers(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if third[MarkerDead] <= firstTTL {
		t.Errorf("the dead ttl is %v after three host failures, want more than the %v "+
			"it was after one", third[MarkerDead], firstTTL)
	}
	if second[MarkerDead] != firstTTL {
		t.Errorf("the ttl changed under a live marker: %v then %v; a marker that "+
			"extended itself would keep a host out for ever after enough failures",
			firstTTL, second[MarkerDead])
	}
}

func TestClassifyCountsEveryOutcomeAgainstItsHost(t *testing.T) {
	tests := []struct {
		name string
		out  Outcome
		want Reason
	}{
		{"a success", Outcome{StatusCode: 200}, ReasonFetchOK},
		{"a 404", Outcome{StatusCode: 404}, ReasonNotFound},
		{"a 403", Outcome{StatusCode: 403}, ReasonForbidden},
		{"a 503", Outcome{StatusCode: 503}, ReasonServerError},
		{"a refused dial", Outcome{Err: &net.DNSError{Err: "no such host"}}, ReasonDNSFailure},
		{"a timeout", Outcome{Err: &timeoutError{}}, ReasonTimeout},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
			host := "counted.example"
			target := "https://" + host + "/p?n=" + strconv.Itoa(len(tc.name))

			act := mustClassify(t, m, target, tc.out)
			if act.Reason != tc.want {
				t.Fatalf("Reason = %q, want %q", act.Reason, tc.want)
			}
			stats := st.Stats(host)
			if stats[tc.want] != 1 {
				t.Errorf("stats[%s] = %d, want 1 (all: %v)", tc.want, stats[tc.want], stats)
			}
		})
	}
}

func TestACountedOutcomeSurvivesAFailedCount(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://counted.example/p"

	st.FailOn = map[string]error{"CountReason": errors.New("redis went away")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 404})
	if err != nil {
		t.Fatalf("a failed count turned into an error: %v", err)
	}
	if act.Kind != ActPermanent {
		t.Errorf("Kind = %v, want permanent; the decision must be applied even when it "+
			"could not be counted", act.Kind)
	}
	st.FailOn = nil
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a failed count stopped the URL being retired")
	}
}

func TestClassifyRefusesToDecideWhenItCannotReadTheAttemptCount(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "https://example.com/p"

	st.FailOn = map[string]error{"URLState": errors.New("redis went away")}

	act, err := m.Classify(ctx, target, Outcome{StatusCode: 503})
	if err == nil {
		t.Fatal("expected an error; the attempt count is an input to the decision")
	}
	if act != nil {
		t.Errorf("Action = %+v, want nil: nothing was decided, so there is nothing to return", act)
	}
	if !errors.Is(err, st.FailOn["URLState"]) {
		t.Errorf("error = %v, want it to wrap the state failure so the cause survives", err)
	}
}

func TestClassifyReportsAFailedRecordWrite(t *testing.T) {
	tests := []struct {
		name   string
		failOn string
		out    Outcome
	}{
		{"a success that cannot be counted", "RecordSuccess", Outcome{StatusCode: 200}},
		{"a success whose url record cannot be cleared", "ClearURLState", Outcome{StatusCode: 200}},
		{"a backoff that cannot be parked", "EnqueueDelayed", Outcome{StatusCode: 503}},
		{"a failure that cannot be counted against the host", "RecordFailure", Outcome{StatusCode: 503}},
		{"an attempt that cannot be counted", "BumpAttempts", Outcome{StatusCode: 503}},
		{"a cooldown whose crawl delay cannot be read", "HostState", Outcome{StatusCode: 503}},
		{"a permanent outcome that cannot be retired", "MarkVisited", Outcome{StatusCode: 404}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
			ctx := context.Background()
			const target = "https://example.com/p"

			st.FailOn = map[string]error{tc.failOn: errors.New("redis went away")}

			act, err := m.Classify(ctx, target, tc.out)
			if err == nil {
				t.Fatalf("expected an error when %s failed", tc.failOn)
			}
			if act != nil {
				t.Errorf("Action = %+v, want nil alongside the error", act)
			}
		})
	}
}

func TestClassifyRejectsAnEmptyURL(t *testing.T) {
	m, _, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))

	for _, raw := range []string{"", "   "} {
		if _, err := m.Classify(context.Background(), raw, Outcome{StatusCode: 200}); !errors.Is(err, errEmptyURL) {
			t.Errorf("Classify(%q) error = %v, want errEmptyURL", raw, err)
		}
	}
}

func TestClassifyNeedsNoHostToStillDecide(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const target = "mailto:someone@example.com"

	act := mustClassify(t, m, target, Outcome{StatusCode: 404})
	if act.Kind != ActPermanent {
		t.Errorf("Kind = %v, want permanent", act.Kind)
	}
	visited, err := st.IsVisited(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a hostless URL should still be retirable")
	}
}

func TestTheHostIsLowercasedForTheRecord(t *testing.T) {
	for _, raw := range []string{
		"https://EXAMPLE.com/A",
		"https://example.com/A",
	} {
		if got := hostOfURL(raw); got != "example.com" {
			t.Errorf("hostOfURL(%q) = %q, want %q", raw, got, "example.com")
		}
	}
}

func TestHostOfURLReportsNothingForSomethingUnparseable(t *testing.T) {
	for _, raw := range []string{"", "://nope", "https://"} {
		if got := hostOfURL(raw); got != "" {
			t.Errorf("hostOfURL(%q) = %q, want an empty string", raw, got)
		}
	}
}

var _ net.Error = timeoutError{}
