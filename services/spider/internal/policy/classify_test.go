package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"
)

// timeoutErr is a net.Error that reports itself as a timeout. It exists because
// the single most consequential rule in classify is that a slow host is not a
// dead host, and that rule is only reachable through a real net.Error -- a
// context.DeadlineExceeded alone would not exercise it.
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
		// --- success ---
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
			// An empty content type is the overwhelmingly common case and must
			// not be a refusal, or most of the web disappears.
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

		// --- permanent: the server gave a final answer ---
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
			// A 404 after nine attempts is exactly as final as a 404 after one.
			// Gating permanence behind the attempt count would re-fetch a page
			// we have already proven is gone.
			name:     "permanence is not gated by attempt count",
			out:      Outcome{StatusCode: 404},
			attempts: 9,
			maxRetry: 10,
			wantKind: ActPermanent,
			wantWhy:  ReasonNotFound,
		},
		{
			// Same for a body too large to index, one attempt short of the
			// ceiling. Truncation is a property of the page, not of how many
			// times we have asked for it.
			name:     "a permanent body verdict survives an exhausted budget",
			out:      Outcome{StatusCode: 200, ContentType: "application/pdf"},
			attempts: 4,
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonContentTypeRejected,
		},

		// --- backoff: the server is answering but unhappy ---
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

		// --- backoff: no response at all ---
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
			// Go 1.20 wrapped certificate verification failures in a dedicated
			// type, so a TLS problem can arrive as this rather than as the bare
			// x509 errors above.
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

		// --- timeouts: slow is not gone ---
		{
			// This is the rule most worth stating twice. A host that once took
			// 30 seconds to answer must not vanish for an hour because of it.
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
			// A DNS lookup that times out also reports itself as a *net.DNSError.
			// Testing DNS before timeouts would classify every slow resolver as
			// a dead domain.
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

		// --- redirect loops ---
		{
			name:     "a redirect loop is permanent",
			out:      Outcome{Err: ErrRedirectLoop, StatusCode: 302},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			// The case that isolates the sentinel. A chain like
			// 301 -> 200 -> 301 -> 200 leaves the last response a 200, so
			// without reading the sentinel a redirect loop would be classified
			// as a success and indexed.
			name:     "a redirect loop ending on a 200 is still permanent",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", Err: ErrRedirectLoop},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},
		{
			// Same isolation for the count-based backstop: a 200 with a hop
			// count over the limit is a loop wearing a success's status.
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
			// Exactly at the limit is within it. Off-by-one here would break
			// every legitimately long redirect chain.
			name:     "a redirect count exactly at the limit is allowed",
			out:      Outcome{StatusCode: 200, ContentType: "text/html", Redirects: 10},
			maxRetry: 5,
			wantKind: ActSuccess,
			wantWhy:  ReasonFetchOK,
		},
		{
			// The transport follows redirects, so a 3xx surfacing means the chain
			// stopped here. Retrying walks the same chain to the same place.
			name:     "a surfaced 3xx is permanent",
			out:      Outcome{StatusCode: 301},
			maxRetry: 5,
			wantKind: ActPermanent,
			wantWhy:  ReasonRedirectLoop,
		},

		// --- bodies we should not index ---
		{
			// Indexing half a page is worse than not indexing it: the text runs
			// off mid-sentence and every term frequency downstream is wrong.
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
			// How a crawler ends up with a PDF's raw bytes in the index.
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

// TestClassifyAttemptCeiling checks that a backoff stops being a backoff once
// the attempts a host allows have been spent. Without this, a permanently
// failing URL is re-queued forever.
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
		// attempts=4 means this is the 5th and last attempt, so the next
		// outcome, whatever it is, retires the URL.
		{"fifth of five attempts is the last one", 4, 5, ActPermanent, ReasonAttemptsExhausted},
		{"past the ceiling stays permanent", 12, 5, ActPermanent, ReasonAttemptsExhausted},
		// maxRetry is the total, not the number of retries after the first.
		{"maxRetry of one permits exactly one attempt", 0, 1, ActPermanent, ReasonAttemptsExhausted},
		// maxRetry comes from a database column, so 0 and negatives are
		// reachable from data. attempts+1 is at least 1, so either reads as "at
		// most one attempt" rather than as an uncrawlable subtree.
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

// TestClassifyStatusOutranksError pins the precedence rule. A chain that ends in
// a 404 is a 404 even though the client reported an error reaching it, and
// treating that as a transport failure would park a dead URL in the delayed set
// and keep re-fetching it.
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

// TestClassifyTimeoutDoesNotUseTheDeadSchedule is the measurable form of "a
// slow host is not a dead host". Both outcomes are ActBackoff, so the kind
// alone cannot tell them apart; the retry schedule can, and Classify uses that
// to decide whether to write a dead marker.
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

// TestClassifyDeadRetryOutlivesDeadMarker checks the number that loses a domain
// if it is wrong. The failing URL is parked for at least as long as the host's
// own dead marker; if it returned sooner, Admit would see the host still dead,
// skip the URL as terminal, and the domain would never be probed again.
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

// TestClassifyBackoffGrows checks that repeated failures of the same kind space
// themselves out, so a host failing us is not hit at a constant rate.
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

// TestClassifyBackoffIsAlwaysPositive guards the delayed set. A zero or
// negative due time would be immediately due forever, which is a hot loop
// wearing a backoff's clothes.
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

// TestClassifyNoEvidenceDoesNotLoop covers the impossible-looking input. With no
// status and no error there is nothing to learn, and backoff is the safe answer
// precisely because the attempt ceiling bounds it.
func TestClassifyNoEvidenceDoesNotLoop(t *testing.T) {
	cfg := DefaultConfig()

	got := classify(Outcome{}, 0, 5, &cfg)
	if got.Kind != ActBackoff {
		t.Errorf("kind = %v, want %v -- dropping the url would lose a page we never tried", got.Kind, ActBackoff)
	}

	// And it must terminate rather than retry forever.
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

// TestRetryAfterDefaultIsThePageSchedule covers the defensive arm of the
// schedule lookup. Every reason that can currently produce a backoff is listed
// explicitly, so this arm is unreachable from classify today -- but it exists
// precisely so that a reason added later without a matching arm gets a
// page-level delay rather than a zero, and a zero due time is immediately due
// forever. A default nobody exercises is a default nobody can rely on.
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

// TestRetryAfterNeverZero is the property the default exists to protect,
// checked across every reason the classifier can emit.
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
	// The strings end up in log fields and Redis counters, so they are part of
	// the contract rather than debug output.
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

// urlError mimics the *url.Error the http client wraps transport failures in,
// to prove the classification survives the wrapping the transport actually
// applies. errors.As and errors.Is both walk it.
type urlError struct{ err error }

func (e *urlError) Error() string { return fmt.Sprintf("Get %q: %v", "http://example.com", e.err) }
func (e *urlError) Unwrap() error { return e.err }
