package policy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Errors the in-memory state returns for conditions a real Redis reports as
// errors rather than values.
var (
	errMemoryClosed  = errors.New("state is closed")
	errEmptyHost     = errors.New("empty host")
	errUnknownMarker = errors.New("unknown marker kind")
)

// PolicyManager owns every decision the spider makes about what to crawl.
//
// It is the only thing in the tree that may answer "should this be fetched".
// The crawl loop asks it twice per URL -- once before the fetch, once after --
// and does nothing on its own judgement, which is what makes the behaviour
// auditable in one place instead of scattered across a normalizer, an HTTP
// helper and a store method that three of them called and one of which nothing
// read.
//
// # Failing closed
//
// Redis holds the only record of what has been crawled, so an unreachable state
// means the crawl does not know what it has already done. Every method here
// treats an error as a refusal. A caller that treated an outage as "no state,
// carry on" would re-fetch every URL in the frontier, ignore every cooldown and
// every dead marker, and do it at full speed -- against hosts already known to
// be failing. An outage has to stop the crawl, not blind it.
type PolicyManager struct {
	cfg   Config
	state State
	log   *slog.Logger
	// now is injectable so that cooldowns, cold windows and backoffs can be
	// tested by moving time rather than by sleeping through it.
	now func() time.Time
	// fetchRobots reads a robots.txt. It is a field rather than a direct call so
	// tests can exercise host resolution without a network; New installs a real
	// one and WithRobotsFetcher replaces it.
	fetchRobots RobotsFetcher
}

// New returns a PolicyManager over the given state.
//
// The state is an interface so tests can drive the rules with a stub, and so the
// rules can be tested with no database anywhere. The caller retains ownership of
// the state and closes it.
func New(cfg Config, state State, logger *slog.Logger) *PolicyManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &PolicyManager{
		cfg:   cfg,
		state: state,
		log:   logger,
		now:   time.Now,
		fetchRobots: newHTTPRobotsFetcher(
			nil, defaultUserAgent(cfg)),
	}
}

// defaultUserAgent builds the token the crawler identifies itself with.
//
// It is derived from Config.UserAgent when set, so the string a robots.txt is
// matched against and the string sent on the wire are the same value. A site
// writing "User-agent: BoogleBot" addresses us by that token, and the match
// failing because the two strings were spelled differently is the kind of bug
// that presents as "robots.txt ignores us" with nothing in the logs.
func defaultUserAgent(cfg Config) string {
	ua := strings.TrimSpace(cfg.UserAgent)
	if ua == "" {
		return defaultBotUserAgent
	}
	if strings.ContainsAny(ua, "/ \t") {
		// Already a full header value; a robots.txt token is a prefix of it and
		// matching is a substring test, so it is sent as written.
		return ua
	}
	return ua + "/1.0 (+https://boogle.example/bot)"
}

// defaultBotUserAgent is the name the crawler answers to in robots.txt groups.
const defaultBotUserAgent = "BoogleBot"

// WithClock returns a copy of the manager reading time from now. It exists for
// tests; production uses time.Now.
func (m *PolicyManager) WithClock(now func() time.Time) *PolicyManager {
	cp := *m
	if now != nil {
		cp.now = now
	}
	return &cp
}

// Config exposes the manager's configuration.
func (m *PolicyManager) Config() Config { return m.cfg }

// Now reads the manager's clock.
func (m *PolicyManager) Now() time.Time { return m.now() }

// WithConfig returns a copy of the manager running a different configuration.
//
// It exists because the interesting configurations are not the shipped one. A
// test that wants a budget of three cannot wait for five thousand pages, and a
// test for a cold window cannot sleep for an hour; both are the same code path
// with a smaller number in it. Production constructs Config once from the
// environment.
//
// The robots fetcher is *not* rebuilt from the new configuration. It was built
// with a user agent derived from the old one, and rebuilding it here would
// silently repoint a fetcher a caller had already installed through
// WithRobotsFetcher.
func (m *PolicyManager) WithConfig(cfg Config) *PolicyManager {
	cp := *m
	cp.cfg = cfg
	return &cp
}

// State exposes the underlying state, for the crawl loop's own use and for
// tests asserting what was written.
func (m *PolicyManager) State() State { return m.state }

// --- the fail-closed surface ---
//
// These are the only two ways a verdict is produced. Keeping them narrow is the
// point: a refusal for any reason, including a broken cache, goes through
// refuse, and admit is the single place a refusal can be accidentally turned
// into an allow.

// refuse produces a terminal verdict and counts the reason against the host.
//
// Counting is best-effort by design. The counter is observability, and losing a
// count during a partial outage must not turn a correct refusal into an error the
// caller retries -- and a retry of a refusal is exactly the loop this package
// exists to stop.
func (m *PolicyManager) refuse(ctx context.Context, host, url string, kind VerdictKind, reason Reason, until time.Time) (*Verdict, error) {
	if err := m.state.CountReason(ctx, host, reason); err != nil {
		m.log.Warn("could not record refusal reason",
			"host", host, "url", url, "reason", reason, "error", err)
	}
	return &Verdict{Kind: kind, Reason: reason, Until: until}, nil
}

// unavailable is the verdict for a state read that failed.
//
// It is Defer, not Skip and emphatically not Allow. Skip would be terminal, and
// the URL might be perfectly good once Redis is back. Allow is the failure mode
// this package exists to prevent: it would fetch a URL whose cooldown, dead
// marker and page budget were all unreadable, which is the same as having no
// policy at all.
//
// Until is left zero deliberately. A Defer with no due time is the caller's cue
// that this URL should simply not be taken from the frontier this round; the
// value is unknown because the thing that would tell us is what is broken.
func (m *PolicyManager) unavailable(ctx context.Context, host, url string, err error) (*Verdict, error) {
	m.log.Error("policy state unavailable, refusing to crawl blind",
		"host", host, "url", url, "error", err)
	// The host is unknown here as often as not, so a per-host count would land
	// nowhere useful. The error log is the record.
	return m.refuse(ctx, host, url, Defer, ReasonPolicyUnavailable, time.Time{})
}

// hostGate reads a host's markers and budget and answers whether its URLs may be
// fetched now.
//
// It is the cheap half of Admit: everything here is one round trip or one hash
// read, and none of it needs a regex or a network call. That ordering is the
// whole fix for a crawl that used to park every worker on one dead domain,
// re-fetching its robots.txt for every URL it owned, forever. After the first
// failure that cost was up to fifteen seconds of HTTP per URL; it is now a single
// marker read.
//
// The returned bool is "carry on with the rules". A false return carries the
// verdict, which is either a refusal or a Defer with a due time.
func (m *PolicyManager) hostGate(ctx context.Context, host string) (HostState, *Verdict, error) {
	markers, err := m.state.Markers(ctx, host)
	if err != nil {
		v, verr := m.unavailable(ctx, host, "", err)
		return HostState{}, v, verr
	}

	// Read in AllMarkers order, which is cheapest and most decisive first. A
	// dead host is answered here without touching the state hash at all.
	if ttl, ok := markers[MarkerDead]; ok {
		// Terminal, so the caller marks the URL visited to stop the frontier
		// handing it out -- Admit is the layer that knows which URL it is. The
		// *host* carries the TTL, and the URL that failed the fetch is parked by
		// Classify for at least as long as that TTL, so when the marker expires
		// the next Admit for this host is the one probe. Dropping every URL with
		// no surviving one is how a domain gets abandoned for good.
		return HostState{}, &Verdict{
			Kind:   Skip,
			Reason: ReasonHostDead,
			Until:  m.now().Add(ttl),
		}, nil
	}

	if ttl, ok := markers[MarkerCold]; ok {
		return HostState{}, &Verdict{
			Kind:   Skip,
			Reason: ReasonHostCold,
			Until:  m.now().Add(ttl),
		}, nil
	}

	if ttl, ok := markers[MarkerCooldown]; ok {
		// Defer, not Skip. The host is answering, just not happily, and this is
		// the one marker whose expiry is expected to bring the host back. A Skip
		// here would strand every URL on a host that recovers in ten seconds.
		return HostState{}, &Verdict{
			Kind:   Defer,
			Reason: ReasonHostCoolingDown,
			Until:  m.now().Add(ttl),
		}, nil
	}

	st, err := m.state.HostState(ctx, host)
	if err != nil {
		v, verr := m.unavailable(ctx, host, "", err)
		return HostState{}, v, verr
	}
	return st, nil, nil
}
