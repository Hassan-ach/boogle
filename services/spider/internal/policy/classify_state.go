package policy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Errors Classify and Discard can return without having touched any state.
var (
	errEmptyURL        = errors.New("empty url")
	errNoDiscardReason = errors.New("a discarded page must say why")
)

// The stateful half of classification.
//
// classify() above is the pure half: an Outcome plus a counter in, an Action out,
// with nothing to decide about and nothing to write. This file is where the Action
// is applied, and it exists as its own file because the difference matters. The
// pure half can be exhaustively tabulated; this half is the one that touches
// Redis, so every line of it is either a state change the policy depends on or a
// failure path, and both need to be read carefully rather than skimmed.

// Classify turns an observed fetch into a recorded decision.
//
// It is the second of the two gates and the mirror of Admit. Admit asks "may this
// be fetched" and answers before a request exists; this asks "what did that
// request mean" and answers after. They are deliberately separate because they
// need different state -- a visited set and markers versus attempt counts and
// failure counters -- and combining them would produce a function whose failure
// modes cannot be reasoned about separately.
//
// # What it writes
//
//	ActSuccess   counters reset, page counted, the URL's retry record discarded
//	ActBackoff   the URL parked until now+RetryAfter, host marked dead or cooling
//	ActPermanent the URL retired, its retry record discarded
//
// Each of those is what makes the decision stick. A decision that was computed but
// not written is not a decision: a 404 that was never recorded is a 404 that gets
// fetched again the next time anything links to it, which is the loop this package
// exists to remove.
//
// # Failing
//
// A state read that fails means we do not know how many attempts this URL has had,
// and deciding anyway is deciding blind. A state write that fails means the
// decision is not recorded.
//
// Either way the answer is (nil, err), never (act, err), and that is the whole
// contract: **an Action is a decision that has been applied, and nothing else.**
// Returning both would let a caller that forgot the error check act on a decision
// that never reached Redis -- indexing a page whose crawl the policy layer does not
// know about, then counting and fetching it again on every rediscovery. The Action
// is withheld rather than returned-alongside-an-error because the caller already
// holds everything it would have logged (the URL it passed in, the Outcome it
// observed), and a nil Action with a non-nil error cannot be misread.
//
// Neither failure is turned into more work, which is the package's rule: an
// unreachable Redis must not produce a fetch it did not authorise, and a fetch whose
// outcome could not be recorded must not be retried as though nothing happened.
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

	// maxAttempts is the ceiling, and a non-positive one already reads as "one
	// attempt" -- classify handles that without a clamp, because a host record with
	// zero in it must not be able to make a URL permanently unfetchable either way.
	act := classify(out, urlState.Attempts, m.cfg.URLMaxAttempts, &m.cfg)

	// Counted before anything is written, because the count is the record of the
	// observation and it survives a write that did not. Best-effort: losing a count
	// during an outage must not become an error the caller retries, and a retry of a
	// recorded decision is the loop this package is about.
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
		// An ActionKind this build does not know about. There is no correct write
		// for it, and "do nothing" would leave the URL in limbo.
		return nil, fmt.Errorf("classify returned an unknown action %v for %s", act.Kind, rawURL)
	}
	return act, nil
}

// recordSuccess applies a successful page: one more against the host's window,
// its failure count cleared, and the URL's retry record gone.
//
// The URL is not marked visited here. It is marked when the page is persisted,
// which is the only point at which the fetch has produced anything worth keeping;
// a page fetched and then lost to a database error is a page worth fetching again.
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

// recordBackoff parks the URL and says so on the host.
//
// The three writes are in the order that keeps the crawl honest if one fails. The
// attempt count goes first because it is the input to the next decision: a URL
// whose failures were not counted gets a fresh budget every time. The marker goes
// before the park because a parked URL is a promise that something will come back
// for it, and the marker is what stops the *rest* of the host being fetched in the
// meantime.
func (m *PolicyManager) recordBackoff(ctx context.Context, host, url string, act *Action) error {
	if _, err := m.state.BumpAttempts(ctx, url); err != nil {
		return fmt.Errorf("count a failed attempt at %s: %w", url, err)
	}

	if host != "" {
		// The host's own consecutive count, not this URL's attempt count: two URLs
		// on one failing host failing together should push the host's exclusion out
		// twice as far, and a per-URL count would see only one failure each.
		failures, err := m.state.RecordFailure(ctx, host)
		if err != nil {
			return fmt.Errorf("record a failure on %s: %w", host, err)
		}
		if failures < 1 {
			// Not reachable from a state that honours its own increments, but a
			// zero here would silently shorten every schedule it feeds.
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

// markAfterFailure raises the marker a failure implies, if any.
//
// The distinction this encodes is the important one in the whole package:
//
//   - No response at all -- DNS, dial, TLS -- is a host that is gone. Its URLs are
//     answered from Redis until the marker expires, and the next Admit after that
//     is the probe.
//   - A response that was a failure -- 429, 5xx, a timeout -- is a host that is
//     there and unhappy. It gets a cooldown, which is short and expected to end
//     with the host answering again.
//
// Timeouts belong in the second group and it is not a detail: a slow host marked
// dead for an hour is a site removed from the index because it once took thirty
// seconds to answer, and no operator would find that in a log.
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
		// The robots.txt delay is the floor on the first cooldown, so the host state
		// is read for it. One extra hash read on the failure path, which is the
		// right place to spend one: acceptance criterion 2 is that a site's
		// Crawl-delay is visible in the marker's TTL, and reading it from anywhere
		// else would be a second source of truth.
		st, err := m.state.HostState(ctx, host)
		if err != nil {
			return fmt.Errorf("read host state to cool %s down: %w", host, err)
		}
		marker = MarkerCooldown
		ttl = m.cfg.HostCooldown(failures, st.CrawlDelay)
	default:
		// Nothing in particular is wrong with this host beyond this URL.
		return nil
	}

	if err := m.state.SetMarker(ctx, host, marker, ttl); err != nil {
		return fmt.Errorf("mark %s %s: %w", host, marker, err)
	}
	m.log.Info("host marked after a failed fetch",
		"host", host, "marker", marker, "ttl", ttl, "reason", reason, "failures", failures)
	return nil
}

// recordPermanent retires the URL and forgets its attempts.
//
// Retired before cleared, and that order is the whole difference: a URL marked
// visited whose retry record survived is a dead hash waiting for its TTL, whereas a
// URL whose record was cleared but not retired is fetched again.
func (m *PolicyManager) recordPermanent(ctx context.Context, host, url string) error {
	if err := m.state.MarkVisited(ctx, url); err != nil {
		return fmt.Errorf("retire %s: %w", url, err)
	}
	if err := m.state.ClearURLState(ctx, url); err != nil {
		return fmt.Errorf("clear the retry record for %s: %w", url, err)
	}
	return nil
}

// Discard retires a page that was fetched successfully and turned out not to be
// worth indexing -- a document in another language, or a body the parser could not
// make into a page.
//
// It is a policy decision and belongs here rather than in the crawl loop for one
// reason: the URL has to stop being a candidate *and* the reason has to be
// counted. A loop that only did the first would leave "how much are we indexing
// and how much are we dropping, and why" unanswerable, which is the same blindness
// that let every other refusal in this codebase go unrecorded.
//
// A parse failure is the motivating case. It used to be reported as a failed
// fetch, so a page that downloaded perfectly and then failed to parse was
// indistinguishable in the logs from one whose host was gone, and neither was
// counted anywhere.
func (m *PolicyManager) Discard(ctx context.Context, url string, reason Reason) error {
	host := hostOfURL(url)
	if reason == "" {
		// A refusal with no reason counts under the empty string, which is a key no
		// one queries. There is no default that would be better than saying so.
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

// countReason records a refusal, ignoring a failure to record it.
func (m *PolicyManager) countReason(ctx context.Context, host string, reason Reason) {
	if host == "" || reason == "" {
		return
	}
	if err := m.state.CountReason(ctx, host, reason); err != nil {
		m.log.Warn("could not record a refusal reason",
			"host", host, "reason", reason, "error", err)
	}
}

// hostOfURL reads the host a URL belongs to, or "" when there is none.
//
// Lowercased, because a host is one record and "Example.com" and "example.com"
// being two of them is how a crawl ends up with two page budgets, two stats
// hashes and two sets of robots rules for one site.
func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return hostKey(u)
}

// hostKey is the string a host's records are filed under.
//
// The port is part of it, and that is not a detail. The key names a site, and the
// port is part of which site: a crawler reading rules from example.com while
// crawling example.com:8080 is reading a different site's instructions, and on the
// common case -- a host with no such site -- it is reading nothing at all and
// treating the connection failure as proof the domain is dead. Since this string is
// also what the robots.txt URL is built from, dropping the port asks for the rules
// of a host that may not exist, once per URL, which is the loop this package exists
// to remove.
//
// The scheme's default port is dropped, so that "https://example.com:443/a" and
// "https://example.com/a" are one site and one budget rather than two.
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
