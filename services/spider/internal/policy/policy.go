package policy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

type VerdictKind int

const (
	// Allow is the zero value, so a Verdict whose Kind was never set permits
	// the URL. Fail-open on purpose: a missing verdict must not stall a crawl.
	Allow VerdictKind = iota
	Defer
	Skip
)

func (k VerdictKind) String() string {
	switch k {
	case Allow:
		return "allow"
	case Defer:
		return "defer"
	case Skip:
		return "skip"
	}
	return "unknown"
}

// Verdict is the policy manager's answer for one URL: Allow releases it now,
// Defer parks it until Until, Skip retires it for good. The Reason rides along
// so the state layer can count refusals without re-deriving them.
type Verdict struct {
	Kind   VerdictKind
	Reason Reason
	Host   *entity.Host
	Until  time.Time
}

type ActionKind int

const (
	// ActSuccess is the zero value, so an unpopulated Action reports success
	// and clears the backoff rather than penalising a URL.
	ActSuccess ActionKind = iota
	ActBackoff
	ActPermanent
)

func (k ActionKind) String() string {
	switch k {
	case ActSuccess:
		return "success"
	case ActBackoff:
		return "backoff"
	case ActPermanent:
		return "permanent"
	}
	return "unknown"
}

// Action records a completed fetch: ActSuccess clears the backoff, ActBackoff
// schedules a retry, ActPermanent gives up on the URL.
type Action struct {
	Kind       ActionKind
	Reason     Reason
	RetryAfter time.Duration
}

// Outcome is what a fetch actually produced. Classification reads only these
// fields, so the Fetcher can be swapped without touching the policy tables.
type Outcome struct {
	StatusCode  int
	Err         error
	ContentType string
	BytesRead   int
	Redirects   int
	FinalURL    string
}

type Reason string

const (
	ReasonOK Reason = "ok"

	ReasonVisited                 Reason = "visited"
	ReasonMalformedURL            Reason = "malformed_url"
	ReasonNonHTTPScheme           Reason = "non_http_scheme"
	ReasonHostDead                Reason = "host_dead"
	ReasonHostCold                Reason = "host_cold"
	ReasonHostCoolingDown         Reason = "host_cooling_down"
	ReasonHostBudgetExhausted     Reason = "host_budget_exhausted"
	ReasonHostMetadataUnknown     Reason = "host_metadata_unavailable"
	ReasonRobotsDisallow          Reason = "robots_disallow"
	ReasonPathDisallowed          Reason = "path_disallowed"
	ReasonExtensionSkipped        Reason = "extension_skipped"
	ReasonLanguageNotEnglish      Reason = "language_not_english"
	ReasonNotInScope              Reason = "not_in_scope"
	ReasonQueryParamFiltered      Reason = "query_param_filtered"
	ReasonPolicyUnavailable       Reason = "policy_unavailable"
	ReasonRobotsUnreachableReason Reason = "robots_unreachable"

	ReasonFetchOK             Reason = "fetch_ok"
	ReasonNotFound            Reason = "not_found"
	ReasonGone                Reason = "gone"
	ReasonForbidden           Reason = "forbidden"
	ReasonUnauthorized        Reason = "unauthorized"
	ReasonBadRequest          Reason = "bad_request"
	ReasonServerError         Reason = "server_error"
	ReasonRateLimited         Reason = "rate_limited"
	ReasonDNSFailure          Reason = "dns_failure"
	ReasonConnectionRefused   Reason = "connection_refused"
	ReasonTLSError            Reason = "tls_error"
	ReasonTimeout             Reason = "timeout"
	ReasonRedirectLoop        Reason = "redirect_loop"
	ReasonBodyTooLarge        Reason = "body_too_large"
	ReasonContentTypeRejected Reason = "content_type_unsupported"
	ReasonAttemptsExhausted   Reason = "attempts_exhausted"
	ReasonBodyUnparseable     Reason = "body_unparseable"
)

type BackoffConfig struct {
	DeadHostBase   time.Duration
	DeadHostMaxExp int
	DeadProbeAfter time.Duration

	HostCooldownBase   time.Duration
	HostCooldownMaxExp int

	URLBackoffBase time.Duration
	URLBackoffMax  time.Duration
}

func DefaultBackoffConfig() BackoffConfig {
	return BackoffConfig{
		DeadHostBase:       60 * time.Second,
		DeadHostMaxExp:     6,
		DeadProbeAfter:     300 * time.Second,
		HostCooldownBase:   10 * time.Second,
		HostCooldownMaxExp: 3,
		URLBackoffBase:     30 * time.Second,
		URLBackoffMax:      900 * time.Second,
	}
}

type Config struct {
	BackoffConfig

	RedisPrefix string

	UserAgent string

	Rules RuleSet

	MaxPagesPerHost int

	URLMaxAttempts int

	HostColdPeriod time.Duration

	MinCrawlDelay time.Duration

	RobotsTTL time.Duration

	URLStateTTL time.Duration

	FrontierPopBatch int

	DelayedPromoteBatch int

	MaxRedirects int

	MaxBodyBytes int
}

func DefaultConfig() Config {
	return Config{
		BackoffConfig:       DefaultBackoffConfig(),
		RedisPrefix:         "boogle:spider",
		UserAgent:           defaultBotUserAgent,
		Rules:               DefaultRules(),
		MaxPagesPerHost:     5000,
		URLMaxAttempts:      3,
		HostColdPeriod:      time.Hour,
		MinCrawlDelay:       time.Second,
		RobotsTTL:           24 * time.Hour,
		URLStateTTL:         7 * 24 * time.Hour,
		FrontierPopBatch:    16,
		DelayedPromoteBatch: 100,
		MaxRedirects:        10,
		MaxBodyBytes:        10 << 20,
	}
}

func (c BackoffConfig) DeadHostTTL(attempts int) time.Duration {
	return scale(c.DeadHostBase, attempts, c.DeadHostMaxExp)
}

func (c BackoffConfig) HostCooldown(attempts int, crawlDelay time.Duration) time.Duration {
	floor := c.HostCooldownBase
	if crawlDelay > floor {
		floor = crawlDelay
	}
	return scale(floor, attempts, c.HostCooldownMaxExp)
}

func (c BackoffConfig) URLBackoff(attempt int) time.Duration {
	d := scale(c.URLBackoffBase, attempt, -1)
	if c.URLBackoffMax > 0 && d > c.URLBackoffMax {
		return c.URLBackoffMax
	}
	return d
}

func scale(base time.Duration, n, maxExp int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if n < 0 {
		n = 0
	}
	if maxExp >= 0 && n > maxExp {
		n = maxExp
	}
	// 62 is the largest shift that keeps base << n inside int64; shifting a
	// duration by 63 sets the sign bit, which the overflow check below catches.
	const maxShift = 62
	if n > maxShift {
		n = maxShift
	}
	d := base << uint(n)
	if d <= 0 {
		return time.Duration(1<<62 - 1)
	}
	return d
}

var (
	errMemoryClosed  = errors.New("state is closed")
	errEmptyHost     = errors.New("empty host")
	errUnknownMarker = errors.New("unknown marker kind")
)

// PolicyManager holds every crawl decision the spider makes: whether a URL may
// be fetched, how a host is feeling, and what a finished fetch did to the state.
// It owns policy but not persistence -- the State it is handed does that, so the
// same logic runs against Redis in production and MemoryState in tests.
type PolicyManager struct {
	cfg             Config
	state           State
	log             *slog.Logger
	now             func() time.Time
	fetchRobots     RobotsFetcher
	fetch           Fetcher
	fetchClient     *http.Client
	resolveSiteMaps SiteMapResolver
}

// New builds a PolicyManager over the given state. The returned manager is a
// value that the With* methods copy, so a caller can derive a manager with a
// different clock or fetcher without disturbing the original.
func New(cfg Config, state State, logger *slog.Logger) *PolicyManager {
	if logger == nil {
		logger = slog.Default()
	}
	fetchClient := &http.Client{Timeout: defaultFetchTimeout}
	return &PolicyManager{
		cfg:   cfg,
		state: state,
		log:   logger,
		now:   time.Now,
		fetchRobots: newHTTPRobotsFetcher(
			nil, defaultUserAgent(cfg)),
		fetch:       NewHTTPFetcher(fetchClient, cfg),
		fetchClient: fetchClient,
	}
}

// A bare token such as "BoogleBot" is not a usable UA: robots.txt parsers
// expect "product/version (contact)". Append a contact unless the configured
// value already carries one.
func defaultUserAgent(cfg Config) string {
	ua := strings.TrimSpace(cfg.UserAgent)
	if ua == "" {
		return defaultBotUserAgent
	}
	if strings.ContainsAny(ua, "/ \t") {
		return ua
	}
	return ua + "/1.0 (+https://boogle.example/bot)"
}

const defaultBotUserAgent = "BoogleBot"

func (m *PolicyManager) WithClock(now func() time.Time) *PolicyManager {
	cp := *m
	if now != nil {
		cp.now = now
	}
	return &cp
}

func (m *PolicyManager) Config() Config { return m.cfg }

func (m *PolicyManager) Now() time.Time { return m.now() }

func (m *PolicyManager) WithConfig(cfg Config) *PolicyManager {
	cp := *m
	cp.cfg = cfg
	if m.fetchClient != nil {
		cp.fetch = NewHTTPFetcher(m.fetchClient, cfg)
	}
	return &cp
}

func (m *PolicyManager) State() State { return m.state }

func (m *PolicyManager) refuse(ctx context.Context, host, url string, kind VerdictKind, reason Reason, until time.Time) (*Verdict, error) {
	if err := m.state.CountReason(ctx, host, reason); err != nil {
		m.log.Warn("could not record refusal reason",
			"host", host, "url", url, "reason", reason, "error", err)
	}
	return &Verdict{Kind: kind, Reason: reason, Until: until}, nil
}

func (m *PolicyManager) unavailable(ctx context.Context, host, url string, err error) (*Verdict, error) {
	m.log.Error("policy state unavailable, refusing to crawl blind",
		"host", host, "url", url, "error", err)
	return m.refuse(ctx, host, url, Defer, ReasonPolicyUnavailable,
		m.now().Add(m.cfg.URLBackoff(1)))
}

// hostGate applies the host-level gates in severity order: dead, cold, then
// cooling down. Markers are checked before any further work because they are the
// cheapest possible refusal -- one read, no policy evaluation.
func (m *PolicyManager) hostGate(ctx context.Context, host string) (HostState, *Verdict, error) {
	markers, err := m.state.Markers(ctx, host)
	if err != nil {
		v, verr := m.unavailable(ctx, host, "", err)
		return HostState{}, v, verr
	}

	if ttl, ok := markers[MarkerDead]; ok {
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
