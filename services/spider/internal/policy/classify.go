package policy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var ErrRedirectLoop = errors.New("redirect limit exceeded")

func classify(out Outcome, attempts, maxRetry int, cfg *Config) *Action {
	kind, reason := categorize(out, cfg)

	if kind == ActBackoff && attempts+1 >= maxRetry {
		return &Action{Kind: ActPermanent, Reason: ReasonAttemptsExhausted}
	}

	act := &Action{Kind: kind, Reason: reason}
	if kind == ActBackoff {
		act.RetryAfter = retryAfterFor(reason, attempts, cfg)
	}
	return act
}

func categorize(out Outcome, cfg *Config) (ActionKind, Reason) {
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

	return ActBackoff, ReasonTimeout
}

// categorizeStatus decides from the status code alone. A 2xx is only accepted
// after the body-size and content-type gates pass, so those two checks belong
// inside the success branch rather than before the switch.
func categorizeStatus(out Outcome, cfg *Config) (ActionKind, Reason) {
	code := out.StatusCode

	switch {
	case code >= 200 && code < 300:
		if cfg.MaxBodyBytes > 0 && out.BytesRead > cfg.MaxBodyBytes {
			return ActPermanent, ReasonBodyTooLarge
		}
		if !acceptableContentType(out.ContentType) {
			return ActPermanent, ReasonContentTypeRejected
		}
		return ActSuccess, ReasonFetchOK

	case code == 304:
		return ActSuccess, ReasonFetchOK

	case code >= 300 && code < 400:
		return ActPermanent, ReasonRedirectLoop

	case code == 400 || code == 405 || code == 451:
		return ActPermanent, ReasonBadRequest

	case code == 401:
		return ActPermanent, ReasonUnauthorized

	case code == 403:
		return ActPermanent, ReasonForbidden

	case code == 404:
		return ActPermanent, ReasonNotFound

	case code == 410:
		return ActPermanent, ReasonGone

	case code == 429:
		return ActBackoff, ReasonRateLimited

	case code >= 500 && code < 600:
		return ActBackoff, ReasonServerError

	case code >= 400 && code < 500:
		return ActPermanent, ReasonBadRequest

	default:
		return ActBackoff, ReasonServerError
	}
}

// categorizeError maps a transport failure to a retry decision. Each TLS error
// type is matched twice, in value and pointer form, because net/http and crypto
// tls return whichever one the failing layer produced and errors.As needs both
// target types to see them. The final fallback is ActBackoff, so an
// unrecognised error never permanently retires a URL.
func categorizeError(err error) (ActionKind, Reason) {
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return ActBackoff, ReasonTimeout
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ActBackoff, ReasonDNSFailure
	}

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

	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNABORTED) {
		return ActBackoff, ReasonConnectionRefused
	}

	return ActBackoff, ReasonServerError
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func acceptableContentType(ct string) bool {
	if ct == "" {
		return true
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "text/html", "application/xhtml+xml", "application/xhtml":
		return true
	}
	return false
}

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

var (
	errEmptyURL        = errors.New("empty url")
	errNoDiscardReason = errors.New("a discarded page must say why")
)

func (m *PolicyManager) Classify(ctx context.Context, rawURL string, out Outcome) (*Action, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errEmptyURL
	}
	host := hostOfURL(rawURL)

	urlState, err := m.state.URLState(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("read the retry record for %s: %w", rawURL, err)
	}

	act := classify(out, urlState.Attempts, m.cfg.URLMaxAttempts, &m.cfg)

	m.countReason(ctx, host, act.Reason)

	switch act.Kind {
	case ActSuccess:
		if err := m.recordSuccess(ctx, host, rawURL); err != nil {
			return nil, err
		}
	case ActBackoff:
		if err := m.recordBackoff(ctx, host, rawURL, act); err != nil {
			return nil, err
		}
	case ActPermanent:
		if err := m.recordPermanent(ctx, host, rawURL); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("classify returned an unknown action %v for %s", act.Kind, rawURL)
	}
	return act, nil
}

func (m *PolicyManager) recordSuccess(ctx context.Context, host, url string) error {
	if host != "" {
		if err := m.state.RecordSuccess(ctx, host, m.now()); err != nil {
			return fmt.Errorf("record a successful page on %s: %w", host, err)
		}
	}
	if err := m.state.ClearURLState(ctx, url); err != nil {
		return fmt.Errorf("clear the retry record for %s: %w", url, err)
	}
	return nil
}

func (m *PolicyManager) recordBackoff(ctx context.Context, host, url string, act *Action) error {
	if _, err := m.state.BumpAttempts(ctx, url); err != nil {
		return fmt.Errorf("count a failed attempt at %s: %w", url, err)
	}

	if host != "" {
		failures, err := m.state.RecordFailure(ctx, host)
		if err != nil {
			return fmt.Errorf("record a failure on %s: %w", host, err)
		}
		if failures < 1 {
			failures = 1
		}
		if err := m.markAfterFailure(ctx, host, act.Reason, failures); err != nil {
			return err
		}
	}

	due := m.now().Add(act.RetryAfter)
	if err := m.Park(ctx, url, due); err != nil {
		return fmt.Errorf("park %s until %s: %w", url, due.UTC().Format("2006-01-02T15:04:05Z07:00"), err)
	}
	return nil
}

func (m *PolicyManager) markAfterFailure(ctx context.Context, host string, reason Reason, failures int) error {
	var (
		marker MarkerKind
		ttl    time.Duration
	)
	switch reason {
	case ReasonDNSFailure, ReasonConnectionRefused, ReasonTLSError:
		marker = MarkerDead
		ttl = m.cfg.DeadHostTTL(failures)
	case ReasonTimeout, ReasonRateLimited, ReasonServerError:
		st, err := m.state.HostState(ctx, host)
		if err != nil {
			return fmt.Errorf("read host state to cool %s down: %w", host, err)
		}
		marker = MarkerCooldown
		ttl = m.cfg.HostCooldown(failures, st.CrawlDelay)
	default:
		return nil
	}

	if err := m.state.SetMarker(ctx, host, marker, ttl); err != nil {
		return fmt.Errorf("mark %s %s: %w", host, marker, err)
	}
	m.log.Info("host marked after a failed fetch",
		"host", host, "marker", marker, "ttl", ttl, "reason", reason, "failures", failures)
	return nil
}

func (m *PolicyManager) recordPermanent(ctx context.Context, host, url string) error {
	if err := m.state.MarkVisited(ctx, url); err != nil {
		return fmt.Errorf("retire %s: %w", url, err)
	}
	if err := m.state.ClearURLState(ctx, url); err != nil {
		return fmt.Errorf("clear the retry record for %s: %w", url, err)
	}
	return nil
}

func (m *PolicyManager) Discard(ctx context.Context, url string, reason Reason) error {
	host := hostOfURL(url)
	if reason == "" {
		return errNoDiscardReason
	}
	m.countReason(ctx, host, reason)
	if err := m.state.MarkVisited(ctx, url); err != nil {
		return fmt.Errorf("discard %s: %w", url, err)
	}
	m.log.Info("page discarded after a successful fetch",
		"url", url, "host", host, "reason", reason)
	return nil
}

func (m *PolicyManager) countReason(ctx context.Context, host string, reason Reason) {
	if host == "" || reason == "" {
		return
	}
	if err := m.state.CountReason(ctx, host, reason); err != nil {
		m.log.Warn("could not record a refusal reason",
			"host", host, "reason", reason, "error", err)
	}
}

func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return hostKey(u)
}

// hostKey is the identity used for every per-host decision and every host key.
// It lowercases the host and drops a default port for the scheme, so https://x:443
// and https://x share one budget instead of being treated as two hosts.
func hostKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	port := u.Port()
	if port == "" {
		return host
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return strings.TrimSuffix(host, ":"+port)
	}
	return host
}
