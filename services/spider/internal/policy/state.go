package policy

import (
	"context"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

// MarkerKind is one of the three short-lived per-host states that carry a TTL.
//
// They are separate keys rather than fields on the host hash because each expires
// on its own schedule: a cooldown might be 10 seconds, a dead marker an hour, a
// cold window a day, and all three outlive or underlay the state hash, which
// never expires. Redis only gained per-field expiry in 7.4, so on anything older
// this is the portable way to say "this part of the host's state goes away".
type MarkerKind int

const (
	// MarkerCooldown means the host is answering but failing us. Wait a little.
	MarkerCooldown MarkerKind = iota
	// MarkerDead means the host produced no response at all: DNS, dial or TLS.
	// Its URLs are answered from Redis instead of the network.
	MarkerDead
	// MarkerCold means the host has spent its page budget for this window. It
	// comes back when the window ends, with the budget reset.
	MarkerCold
)

// AllMarkers is the read order used when checking a host. Cheapest and most
// decisive first, so a dead host costs one round trip and no rules.
var AllMarkers = [...]MarkerKind{MarkerDead, MarkerCold, MarkerCooldown}

// State is everything the policy manager needs Redis for.
//
// It is an interface, and the manager takes one, for two reasons. Tests inject a
// stub and exercise the decision logic with no database anywhere. And the
// implementation is free to be wrong in one place without that wrongness leaking
// into the policy rules, which is what makes the rules testable at all.
//
// The interface is deliberately free of policy. Nothing here decides anything;
// every method is a fact about stored state. The decisions live in Admit and
// Classify, and a method like MarkAllowed would be a policy decision wearing a
// storage method's name.
//
// # Failing closed
//
// Every method returns an error, and every caller must treat an error as a
// refusal. This is not defensive style: the state is the only record of what has
// been crawled, so a caller that treats an unreachable Redis as "no state, carry
// on" re-fetches every URL in the frontier, ignores every cooldown and every
// dead marker, and does all of it at full speed. An outage would become a
// crawl storm against hosts already known to be failing.
type State interface {
	// --- terminal URL record ---

	// MarkVisited records URLs as resolved. A visited URL is never returned by
	// the frontier again, so this is what makes a Skip verdict stick.
	MarkVisited(ctx context.Context, urls ...string) error
	// IsVisited reports whether a URL has already been resolved.
	IsVisited(ctx context.Context, url string) (bool, error)

	// --- frontier ---

	// Enqueue adds URLs to the frontier, incrementing their priority by one.
	// Priority is an inlink count, so a URL linked from many places is crawled
	// before one linked from a single page -- which is a crude but real
	// approximation of importance.
	Enqueue(ctx context.Context, urls ...string) error
	// EnqueueAt adds a URL at an exact priority rather than incrementing.
	EnqueueAt(ctx context.Context, url string, priority float64) error
	// EnqueueDelayed parks a URL until a due time instead of making it available
	// now. This is the whole retry mechanism: there is no timer and no goroutine
	// waiting for anything, so a parked URL costs one sorted-set member whether
	// it comes back in a second or a day.
	EnqueueDelayed(ctx context.Context, url string, due time.Time) error
	// FrontierLen reports how many URLs are available, used to decide whether the
	// crawl has work left.
	FrontierLen(ctx context.Context) (int64, error)

	// PopFrontier takes the highest-priority URL that has not been crawled,
	// examining at most budget entries while looking for one. See PopResult for
	// why the answer has to say more than "did you get one".
	PopFrontier(ctx context.Context, budget int) (PopResult, error)
	// PromoteDelayed moves URLs whose retry has come due into the frontier, at
	// most batch per call, and returns how many were moved.
	//
	// The caller loops while the return value equals batch. There is no timer
	// anywhere: a parked URL becomes available because the crawl happens to look,
	// which is why this runs at the top of every crawl.
	PromoteDelayed(ctx context.Context, now time.Time, batch int) (int, error)

	// --- per-URL retry bookkeeping ---

	// URLState reads a URL's retry record. A URL with no record is normal, not
	// an error.
	URLState(ctx context.Context, url string) (URLState, error)
	// BumpAttempts records a failure and returns the new count, which is what
	// the per-URL backoff schedule is a function of.
	BumpAttempts(ctx context.Context, url string) (int, error)
	// ClearURLState discards the record for a URL that succeeded or was retired,
	// so abandoned state does not accumulate for the life of the crawl.
	ClearURLState(ctx context.Context, url string) error

	// --- per-host policy state ---

	// HostState reads a host's record. An unknown host returns a zero HostState
	// and no error: absence is a normal condition, not a failure.
	HostState(ctx context.Context, host string) (HostState, error)
	// SaveHostState writes a host's record.
	//
	// It replaces the fields it is given and leaves the rest alone, and it is
	// NOT safe to use to change a counter. Reading a record, changing one field
	// and writing it back loses whatever another worker changed in between, and
	// twenty workers on one host is the normal case rather than the pathological
	// one. That is what RecordSuccess and RecordFailure are for.
	SaveHostState(ctx context.Context, host string, st HostState) error
	// RecordSuccess records one page crawled successfully on a host: the window's
	// page count goes up, the consecutive-failure count resets, and the host is
	// stamped as working again.
	//
	// One call rather than an increment plus a reset plus a timestamp because
	// those are three round trips and three chances to build the record wrongly.
	RecordSuccess(ctx context.Context, host string, at time.Time) error
	// RecordFailure counts one failed fetch against a host and returns the new
	// total, which is what every backoff schedule is a function of.
	RecordFailure(ctx context.Context, host string) (int, error)
	// ResetWindow starts a new page-budget window at the given time.
	ResetWindow(ctx context.Context, host string, at time.Time) error
	// ClaimSiteMaps claims the sitemaps advertised by the robots.txt read at `at`,
	// and reports whether this caller is the one that gets to act on them.
	//
	// Exactly-once matters because discovery is an increment: sitemap entries are
	// enqueued by inlink priority, so a second pass over the same sitemap does not
	// produce the same frontier, it produces URLs whose score climbs once per
	// robots.txt re-read until they are never crawled at all.
	//
	// The claim is keyed by the reading's timestamp rather than by a flag, and it
	// has to be: a flag would need a separate write to clear it on each re-read, and
	// the clear and the claim cannot both be atomic. `at` is what makes the
	// comparison self-contained -- a caller holding an older reading than the one
	// already claimed loses, which is right, and a caller holding a newer one wins
	// however late it arrives.
	ClaimSiteMaps(ctx context.Context, host string, at time.Time) (bool, error)

	// --- per-host markers ---

	// Markers reads every marker for a host in one round trip and returns those
	// currently set, with their remaining TTL. This is the check every Admit
	// makes, and making it a single call is what keeps a dead host costing one
	// request instead of three.
	Markers(ctx context.Context, host string) (map[MarkerKind]time.Duration, error)
	// SetMarker raises a marker with a TTL. It must not extend an existing
	// marker's expiry -- twenty workers all marking the same host dead would
	// otherwise keep pushing the deadline into the future and a dead domain would
	// never be probed again.
	SetMarker(ctx context.Context, host string, kind MarkerKind, ttl time.Duration) error
	// ClearMarker drops a marker.
	ClearMarker(ctx context.Context, host string, kind MarkerKind) error

	// --- observability ---

	// CountReason records that a URL on this host was refused for a reason. This
	// is what turns a refusal from a log line into a number, and it is the only
	// way to answer "how much of this site are we declining and why".
	CountReason(ctx context.Context, host string, reason Reason) error

	// Close releases the underlying connection.
	Close() error
}

// HostState is the persistent per-host policy record.
//
// It is deliberately not entity.Host. That struct is the in-memory mirror the
// rest of the spider passes around, and three of its fields -- MaxPages,
// PagesCrawled, AllowedUrls -- were written and never read. The distinction
// matters: this type exists to be compared, so a field on it that nothing reads
// is a mistake, whereas a field on the mirror is just a value carried along.
type HostState struct {
	// Name is the hostname this state describes.
	Name string

	// CrawlDelay is the pause this host asked for, from robots.txt. Already
	// floored by Config.MinCrawlDelay, because a robots.txt that omits the
	// directive must not mean "no delay".
	CrawlDelay time.Duration

	// MaxPages is the page budget for one window. It may only ever be lowered
	// below Config.MaxPagesPerHost: a hostile robots.txt must not be able to opt
	// itself into an unbounded crawl.
	MaxPages int

	// PagesCrawled counts pages fetched in the current window. It resets when a
	// cold window ends, which is what makes the budget per-window rather than
	// per-lifetime -- a lifetime cap plus a cold period would mean the host
	// re-cools the instant it wakes and is never crawled again.
	PagesCrawled int

	// WindowStartedAt is when the current window began. The rollover is driven
	// off this rather than off the cold marker alone, so the budget still resets
	// correctly if the marker was evicted or the spider was down for the whole
	// cold period.
	WindowStartedAt time.Time

	// ConsecFailures counts failures since the last success. Every backoff
	// schedule is a function of it, and a success resets it, so a host that
	// recovers and later dies again starts from the first TTL rather than the
	// largest.
	ConsecFailures int

	// Status is a coarse human-facing label -- "active" or "degraded" -- kept
	// for answering whether a host is currently misbehaving. The authoritative
	// gates are the markers; this is the one that survives them expiring.
	Status string

	// RobotsFetchedAt is when robots.txt was last read. Rules are re-read after
	// RobotsTTL rather than per URL.
	RobotsFetchedAt time.Time

	// FirstSeen and LastSuccess bracket the host's crawl history.
	FirstSeen   time.Time
	LastSuccess time.Time

	// Allow and Disallow are the cached robots.txt rules.
	//
	// Allow was parsed and discarded for the life of this codebase, and the
	// result was that a site saying "Allow: /wiki/, Disallow: /" -- the ordinary
	// "come to the good part" pattern -- was blocked in full, because the one
	// rule that would have unblocked it was the one being ignored.
	Allow    []string
	Disallow []string

	// SiteMaps are the Sitemap: URLs from the same robots.txt.
	//
	// They are cached here rather than re-read because robots.txt is the only
	// place a sitemap is advertised, and a host is generally read once and then
	// crawled for hours. Re-fetching it to re-read a list that has not changed
	// is a request per host for no information.
	SiteMaps []string

	// SiteMapsClaimedAt is when the sitemaps advertised by the robots.txt read at
	// RobotsFetchedAt were expanded into the frontier.
	//
	// It is a timestamp rather than a deletion of SiteMaps because the URLs stay
	// useful afterwards: an operator asking "which sitemaps does this site publish"
	// gets an answer from the host record alone, long after the crawl has moved
	// on. A later robots.txt carries a later timestamp and so wins the claim again,
	// which is what re-queues the list -- the site may have published more since.
	SiteMapsClaimedAt time.Time
}

// WithRobots returns a copy of h carrying a freshly read robots.txt, preserving
// the counters a robots.txt knows nothing about.
//
// This exists because the two halves are rewritten by different events. A
// robots.txt is re-read on a schedule measured in hours; the page count and the
// consecutive-failure count are moved by every fetch. Overwriting the record
// wholesale on a robots re-read silently reset both, which handed every host a
// fresh page budget once a day and reset a host that had failed five times in a
// row back to its first backoff -- neither of which anything had decided.
//
// fetchedAt is passed in rather than derived from the record because the record's
// own RobotsFetchedAt is the *previous* read, not this one. Taking the window
// start from it would date the first window to whenever the host was last seen,
// which for a host read twice is the second read -- leaving a host whose window
// start is never set at all, and a budget that can therefore never roll over.
func (h HostState) WithRobots(name string, allow, disallow, siteMaps []string, crawlDelay time.Duration, fetchedAt time.Time) HostState {
	out := h
	out.Name = name
	out.Allow = allow
	out.Disallow = disallow
	out.SiteMaps = siteMaps
	out.CrawlDelay = crawlDelay
	out.RobotsFetchedAt = fetchedAt
	if out.WindowStartedAt.IsZero() {
		// The first window begins when the host is first resolved, not when a
		// later robots.txt arrives.
		out.WindowStartedAt = fetchedAt
	}
	return out
}

// ToEntity renders the state as the in-memory mirror the rest of the spider
// expects. This is the only sanctioned way to produce one, so a caller cannot
// build an entity.Host with a MaxPages nobody chose.
func (h HostState) ToEntity(maxRetry int) *entity.Host {
	return &entity.Host{
		MaxRetry:        maxRetry,
		MaxPages:        h.MaxPages,
		PagesCrawled:    h.PagesCrawled,
		Delay:           int(h.CrawlDelay / time.Second),
		Name:            h.Name,
		AllowedUrls:     h.Allow,
		NotAllowedPaths: h.Disallow,
	}
}

// BudgetExhausted reports whether this host has spent its page budget for the
// current window.
//
// The effective limit is the *smaller* of the host's own limit and the global
// one. Taking whichever was set last would let a host's robots.txt-derived value
// raise the cap, and a hostile robots.txt would then be able to opt itself into
// an unbounded crawl.
func (h HostState) BudgetExhausted(globalMax int) bool {
	limit := globalMax
	if h.MaxPages > 0 && (limit <= 0 || h.MaxPages < limit) {
		limit = h.MaxPages
	}
	if limit <= 0 {
		// A zero budget would make every host instantly exhausted, so treat it
		// as "no limit configured" rather than "crawl nothing".
		return false
	}
	return h.PagesCrawled >= limit
}

// PopResult is the answer to one frontier pop.
//
// The distinction between "found nothing" and "there is nothing left" is the
// whole point of this type, and getting it wrong is what made the original crawl
// loop unbounded. The old script popped the highest-scoring entry, noticed it
// had already been visited, and returned false -- discarding the entry it had
// just popped. The caller could not tell that from an empty frontier, so it
// looped up to maxRetry times burning entries per call, and every entry it
// burned was gone from the frontier forever. A URL could be popped and silently
// destroyed between being discovered and being crawled.
//
// So a pop has to report three things: the URL if it found one, whether the
// frontier is now definitively empty, and how many already-visited entries it
// consumed along the way.
//
//   - Found and URL set: crawl it.
//   - Exhausted: the frontier is empty. The crawl is idle. Stop asking.
//   - Neither: the scan budget ran out on entries that were all already visited.
//     There may still be unvisited work behind them, so call again.
type PopResult struct {
	// URL is the highest-priority unvisited URL, when Found.
	URL string

	// Found reports whether URL was returned.
	Found bool

	// Exhausted reports that the frontier is now definitively empty, so the
	// crawl has no work left. Distinct from "found nothing this call", which is
	// what a budget-limited scan reports while leaving work behind.
	Exhausted bool

	// VisitedSkipped is how many entries this call consumed without producing a
	// URL: already-visited ones, and the empty member that a write bypassing
	// Enqueue can leave behind. It is the loop signal -- a non-zero value with
	// Found false means entries were burned, so there may be more behind them.
	// A persistently non-zero value means the frontier is mostly stale, which
	// is worth knowing but not worth acting on beyond the pop itself.
	VisitedSkipped int
}

// URLState is the per-URL retry bookkeeping: how many times we have tried, and
// what the last failure was.
type URLState struct {
	// URL is the full URL, stored alongside the hash so a key found in a slowlog
	// or a keyspace scan can be traced back without re-hashing every candidate.
	URL string
	// Attempts is how many fetches have failed for this URL.
	Attempts int
	// LastStatus and LastError are the previous failure's HTTP status and text.
	// The reason code is a bounded enum; the precise text lives here, which has
	// room for it.
	LastStatus int
	LastError  string
	EnqueuedAt time.Time
}
