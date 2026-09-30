// Package policy owns every decision the spider makes about what to crawl.
//
// It exists because those decisions were previously scattered across the crawl
// loop, the URL normalizer, the HTTP helper and the store, and because of that
// several of them were not made at all. The crawl-delay from robots.txt was
// parsed, stored, and then never read. MaxPages and PagesCrawled were written
// and never compared. The Allow rules were parsed and discarded. A dead domain
// re-fetched its robots.txt for every URL it owned, forever, because the failure
// was never recorded.
//
// The split is: utils answers "what URL is this?" and policy answers "should we
// crawl it?". Nothing outside this package decides whether a URL is fetched.
//
// State lives entirely in Redis. That is a deliberate trade -- a flush costs a
// re-crawl -- and it carries an obligation: every failure path here fails
// *closed*. A Redis error returns Defer or Skip, never Allow. An unreachable
// cache must stop the crawl, not blind it.
package policy

import (
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

// VerdictKind is the answer to "may I crawl this URL right now".
type VerdictKind int

const (
	// Allow: crawl it now.
	Allow VerdictKind = iota
	// Defer: not now, but not never. The URL goes into the delayed set with a
	// due time and comes back on its own. This is the verdict for a host that is
	// merely cooling down -- refusing permanently would strand every URL on it.
	Defer
	// Skip: permanently uninteresting. The URL is marked visited so the frontier
	// stops handing it out. Terminal, and only correct for decisions that will
	// not change: a dead host, an exhausted budget, a robots exclusion.
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

// Verdict is a crawl decision plus the reason for it.
//
// The reason is not decoration. Once a host goes cold or a domain is marked
// dead, the only useful question is why, and a bare enum leaves that
// unanswerable. Reasons are counted per host in the stats hash so "what is this
// crawler refusing, and how much" is a question with an answer.
type Verdict struct {
	Kind   VerdictKind
	Reason Reason
	// Host is the resolved host metadata, or nil when the host was never
	// resolved (a malformed URL, or a host whose robots.txt could not be had).
	Host *entity.Host
	// Until is set for Defer only: when the URL becomes eligible again. A Defer
	// with a zero Until would mean "later, but when?", which is a deadlock.
	Until time.Time
}

// ActionKind is the answer to "what does this fetch outcome mean".
type ActionKind int

const (
	// ActSuccess: counters reset, links extracted, URL left the frontier for
	// good.
	ActSuccess ActionKind = iota
	// ActBackoff: transient. Requeue into the delayed set with a due time and
	// increment the host's failure counters.
	ActBackoff
	// ActPermanent: a dead end. Mark visited and never look at it again.
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

// Action is what to do with a completed fetch.
type Action struct {
	Kind   ActionKind
	Reason Reason
	// RetryAfter is set for ActBackoff only.
	RetryAfter time.Duration
}

// Outcome is everything observed about one fetch. The transport fills it in and
// Classify reads it.
//
// Fields exist for the failure modes a status code cannot express: a redirect
// loop, a truncated body, a PDF served with a 200. Before this existed every one
// of those was reported as "no error" and the page went into the index.
type Outcome struct {
	// StatusCode is 0 when no response was ever received -- a dial, TLS or DNS
	// failure. A non-zero StatusCode is the server's definitive answer and takes
	// precedence over Err.
	StatusCode int
	// Err is the transport error, if any. Nil on a completed response even when
	// the status was an error: a 404 is not a transport failure.
	Err error
	// ContentType is the response media type, used to reject non-HTML bodies
	// that arrived with a 200.
	ContentType string
	// BytesRead is how much of the body was actually read. Compared against
	// MaxResponseBytes to spot a body that was cut off at the cap.
	BytesRead int
	// Redirects counts hops followed.
	Redirects int
	// FinalURL is where the chain ended, after redirects.
	FinalURL string
}

// Reason is a bounded machine-readable explanation. Precise error text is not
// here -- it goes in the URL state hash, which has room for it -- so that this
// stays a small enumerable set that can be counted per host.
type Reason string

const (
	// Allow reasons.
	ReasonOK Reason = "ok"

	// Admit reasons.
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

	// Classify reasons.
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
)

// Backoff configuration. The schedules in backoff.go are pure functions of a
// counter and this struct; nothing here reaches Redis, which is what makes them
// testable without a database.
type BackoffConfig struct {
	// DeadHostBase is the first dead-host TTL. Each consecutive connection-level
	// failure doubles it.
	DeadHostBase time.Duration
	// DeadHostMaxExp caps the doubling exponent. 6 with a 60s base tops out at
	// 64 minutes; without a cap a long outage would push a host out for weeks.
	DeadHostMaxExp int
	// DeadProbeAfter is how long a URL is parked after its host was marked dead.
	// It is the earliest the host could be probed again, so it must be at least
	// DEAD_HOST_BASE or the TTL would expire before the URL returns.
	DeadProbeAfter time.Duration

	// HostCooldownBase is the first degraded-host cooldown, floored by whatever
	// Crawl-delay robots.txt asked for.
	HostCooldownBase time.Duration
	// HostCooldownMaxExp caps the degraded-host doubling.
	HostCooldownMaxExp int

	// URLBackoffBase is the first per-URL retry delay.
	URLBackoffBase time.Duration
	// URLBackoffMax clamps the per-URL doubling.
	URLBackoffMax time.Duration
}

// DefaultBackoffConfig returns the values in the plan's §9.
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

// Config is everything the policy manager is tunable with. It is a plain struct
// with no behaviour so that a test can construct one literal, and so that
// loading it from the environment stays a separate, testable concern.
type Config struct {
	BackoffConfig

	// MaxPagesPerHost is the page budget for one host *per window*. See
	// AdmitBudget: a host that exhausts it goes cold, and its counter resets
	// when the cold period ends. Treating it as a lifetime cap would mean the
	// host re-cools the instant it wakes and is never crawled again.
	MaxPagesPerHost int

	// HostColdPeriod is how long an exhausted host stays cold.
	HostColdPeriod time.Duration

	// MinCrawlDelay is a floor applied to whatever Crawl-delay robots.txt
	// declares. A robots.txt asking for zero, or omitting the directive, must
	// not mean "no delay" -- that is how a crawler earns a ban.
	MinCrawlDelay time.Duration

	// RobotsTTL is how long a fetched robots.txt is trusted before being
	// re-read. Rules change; an hour-old copy is a reasonable compromise
	// between respecting a change promptly and re-fetching per URL.
	RobotsTTL time.Duration

	// URLStateTTL is how long per-URL retry bookkeeping survives. Abandoned
	// state has to expire on its own or a long crawl accumulates one hash per
	// URL it ever touched.
	URLStateTTL time.Duration

	// FrontierPopBatch is how many frontier entries one pop may examine while
	// looking for an unvisited one. This is a *scan budget*, not a retry count,
	// and the old REDIS_MAX_RETRY conflated the two.
	FrontierPopBatch int

	// DelayedPromoteBatch caps one promote pass. Kept modest because the script
	// passes the promoted set to ZREM through Lua's unpack, which is bounded by
	// the Lua stack; the caller loops until a pass comes back short.
	DelayedPromoteBatch int

	// MaxRedirects is the longest redirect chain worth following.
	MaxRedirects int

	// MaxBodyBytes is the largest response body worth reading. A body that
	// reached this size is treated as truncated and the page is dropped, since
	// indexing half a page poisons every term frequency on it.
	MaxBodyBytes int
}

// DefaultConfig returns the plan's §9 defaults. Every field has a value, so a
// Config built this way behaves correctly with no environment set at all.
func DefaultConfig() Config {
	return Config{
		BackoffConfig:       DefaultBackoffConfig(),
		MaxPagesPerHost:     5000,
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
