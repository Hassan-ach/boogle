package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
	"time"
)

// ErrRedirectLoop is returned by the transport when a redirect chain exceeds the
// configured limit. It is declared here rather than in utils because it is a
// policy signal: the transport detects it, but only the policy decides it is
// fatal.
var ErrRedirectLoop = errors.New("redirect limit exceeded")

// classify turns an observed fetch into a decision. It is pure: no Redis, no
// clock, no network. Everything that touches state happens in Classify, which
// calls this first and only then acts on the answer.
//
// The order of the two failure paths matters and is not arbitrary:
//
//   - A status code, when present, is the server's final answer and outranks any
//     transport error. A redirect chain that ends in a 404 is a 404, even though
//     the client reported an error reaching it.
//   - With no status at all, the error is the only evidence there is.
func classify(out Outcome, attempts, maxRetry int, cfg *Config) *Action {
	kind, reason := categorize(out, cfg)

	// The attempt ceiling applies to retries only. A permanent outcome is
	// permanent whatever the history: a 404 on the tenth try is exactly as
	// final as a 404 on the first, and gating it behind the attempt count would
	// re-fetch a page we have already proven is gone.
	//
	// maxRetry is the total number of attempts allowed, not the number of
	// retries after the first. No clamp is needed for a bad value: attempts+1 is
	// at least 1, so a maxRetry of 0 or -7 already reads as "at most one attempt"
	// and this attempt was that one.
	if kind == ActBackoff && attempts+1 >= maxRetry {
		// This was the last attempt we were willing to make, and it failed.
		// Parking it again would mean retrying forever, so retire it.
		return &Action{Kind: ActPermanent, Reason: ReasonAttemptsExhausted}
	}

	act := &Action{Kind: kind, Reason: reason}
	if kind == ActBackoff {
		act.RetryAfter = retryAfterFor(reason, attempts, cfg)
	}
	return act
}

// categorize maps one Outcome to a kind and a reason, with no side effects.
func categorize(out Outcome, cfg *Config) (ActionKind, Reason) {
	// A redirect loop is checked before the status because the transport may
	// have stopped on a 302 that is itself a symptom of the loop. A bare
	// Redirects count over the limit is kept as a backstop for transports that
	// report the count but not a sentinel.
	if errors.Is(out.Err, ErrRedirectLoop) {
		return ActPermanent, ReasonRedirectLoop
	}
	if cfg.MaxRedirects > 0 && out.Redirects > cfg.MaxRedirects {
		return ActPermanent, ReasonRedirectLoop
	}

	if out.StatusCode != 0 {
		return categorizeStatus(out, cfg)
	}

	if out.Err != nil {
		return categorizeError(out.Err)
	}

	// No status and no error means the transport told us nothing, which should
	// be impossible. Backoff is the safe answer: the attempt ceiling above
	// guarantees this terminates rather than spinning, so the worst case is a
	// few wasted delays rather than a URL that is dropped or a crawl that hangs.
	return ActBackoff, ReasonTimeout
}

// categorizeStatus maps an HTTP status to a decision.
func categorizeStatus(out Outcome, cfg *Config) (ActionKind, Reason) {
	code := out.StatusCode

	switch {
	case code >= 200 && code < 300:
		// A body that hit the cap is a truncated page, and indexing half a page
		// is worse than not indexing it: the text runs off mid-sentence and
		// every term frequency downstream is wrong. The transport reads one
		// byte past the cap specifically so "exactly at the cap" and "truncated
		// at the cap" can be told apart.
		if cfg.MaxBodyBytes > 0 && out.BytesRead > cfg.MaxBodyBytes {
			return ActPermanent, ReasonBodyTooLarge
		}
		if !acceptableContentType(out.ContentType) {
			return ActPermanent, ReasonContentTypeRejected
		}
		return ActSuccess, ReasonFetchOK

	case code == 304:
		// Not modified. We do not send conditional requests, so this is odd,
		// but it is a statement that the content is fine rather than a failure.
		return ActSuccess, ReasonFetchOK

	case code >= 300 && code < 400:
		// The transport follows redirects, so a 3xx surfacing here means the
		// chain stopped at this response. Retrying walks the same chain and
		// lands in the same place.
		return ActPermanent, ReasonRedirectLoop

	case code == 400 || code == 405 || code == 451:
		return ActPermanent, ReasonBadRequest

	case code == 401:
		return ActPermanent, ReasonUnauthorized

	case code == 403:
		// Permanent for this URL: the same credentials will not appear on the
		// retry. Note that a host answering 403 to a robots.txt fetch is a
		// different matter and is handled by EnsureHost, not here.
		return ActPermanent, ReasonForbidden

	case code == 404:
		return ActPermanent, ReasonNotFound

	case code == 410:
		return ActPermanent, ReasonGone

	case code == 429:
		// The server is explicitly asking us to slow down. This is the one 4xx
		// that is genuinely transient, and treating it as permanent would
		// abandon a page that exists.
		return ActBackoff, ReasonRateLimited

	case code >= 500 && code < 600:
		return ActBackoff, ReasonServerError

	case code >= 400 && code < 500:
		// An unlisted 4xx is still a client error. Repeating the request
		// cannot change the answer, and repeating a 4xx is what gets a crawler
		// blocked.
		return ActPermanent, ReasonBadRequest

	default:
		// 1xx, or a code outside every range. We have no idea what this means,
		// so try again later rather than discarding the URL.
		return ActBackoff, ReasonServerError
	}
}

// categorizeError maps a transport error to a decision.
//
// The single most important distinction in this function: timeout and
// connection-refused both mean "no response", but a timeout means *slow* and a
// refused connection means *gone*. Marking a slow host dead would exclude a
// perfectly good site for an hour because it once took 30 seconds to answer.
func categorizeError(err error) (ActionKind, Reason) {
	// Timeouts first, and before DNS. A DNS lookup that times out also reports
	// itself as a *net.DNSError, so testing DNS first would classify every slow
	// resolver as a dead host.
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return ActBackoff, ReasonTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ActBackoff, ReasonDNSFailure
	}

	// Certificate problems mean the host is there but not offering what we asked
	// for. Worth a bounded number of retries in case it is a misconfigured edge
	// node, not worth retrying to the point of the attempt ceiling being the
	// only thing that stops us.
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return ActBackoff, ReasonTLSError
	}
	var pUnknown *x509.UnknownAuthorityError
	if errors.As(err, &pUnknown) {
		return ActBackoff, ReasonTLSError
	}
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		return ActBackoff, ReasonTLSError
	}
	var pCertInvalid *x509.CertificateInvalidError
	if errors.As(err, &pCertInvalid) {
		return ActBackoff, ReasonTLSError
	}
	var hostnameMismatch x509.HostnameError
	if errors.As(err, &hostnameMismatch) {
		return ActBackoff, ReasonTLSError
	}
	var pHostMismatch *x509.HostnameError
	if errors.As(err, &pHostMismatch) {
		return ActBackoff, ReasonTLSError
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return ActBackoff, ReasonTLSError
	}
	var pRecordHeader *tls.RecordHeaderError
	if errors.As(err, &pRecordHeader) {
		return ActBackoff, ReasonTLSError
	}
	var certVerify *tls.CertificateVerificationError
	if errors.As(err, &certVerify) {
		return ActBackoff, ReasonTLSError
	}

	// Reached the host, or could not. Both mean the host is not serving, so
	// both are grounds for a dead marker. These arrive wrapped in *net.OpError
	// from the dialer, which errors.Is unwraps.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNABORTED) {
		return ActBackoff, ReasonConnectionRefused
	}

	// Anything else is unrecognised. Back off rather than give up: an
	// unrecognised error is more often a transient library-level problem than a
	// definitive answer, and the attempt ceiling bounds the cost of being wrong.
	return ActBackoff, ReasonServerError
}

// isTimeout reports whether err is a network timeout, at any depth of wrapping.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// acceptableContentType reports whether a body is worth indexing.
//
// An empty content type is accepted. Plenty of real servers omit it, and
// rejecting on absence would drop pages that are perfectly crawlable; the body
// is parsed as HTML and simply yields no links if it is not. What matters is
// refusing a body that positively identifies itself as something else -- a PDF
// served with a 200, which is how a crawler ends up with a PDF's raw bytes in
// the index.
func acceptableContentType(ct string) bool {
	if ct == "" {
		return true
	}
	// The header may carry parameters: "text/html; charset=utf-8".
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "text/html", "application/xhtml+xml", "application/xhtml":
		return true
	}
	return false
}

// retryAfterFor picks the schedule that matches the failure.
//
// Three schedules exist because the failures have different shapes. A dead host
// is not the failure of one URL, so its retry time has to outlast the host's own
// dead marker -- otherwise the URL returns while the host is still dead, gets
// skipped as terminal, and the domain is never probed again. That is the one
// case where getting the number wrong loses the domain entirely, so it takes the
// maximum of the two.
func retryAfterFor(reason Reason, attempts int, cfg *Config) (d time.Duration) {
	switch reason {
	case ReasonDNSFailure, ReasonConnectionRefused, ReasonTLSError:
		ttl := cfg.DeadHostTTL(attempts + 1)
		if cfg.DeadProbeAfter > ttl {
			return cfg.DeadProbeAfter
		}
		return ttl

	case ReasonRateLimited, ReasonServerError, ReasonTimeout:
		return cfg.URLBackoff(attempts)

	default:
		return cfg.URLBackoff(attempts)
	}
}
