package state

import (
	"context"
	"errors"
	"time"
)

type MarkerKind int

const (
	MarkerCooldown MarkerKind = iota
	MarkerDead
	MarkerCold
)

// Host statuses. A host is ready until enough consecutive failures make it
// degraded.
const (
	StatusReady    = "ready"
	StatusDegraded = "degraded"
)

// ErrEmptyHost is returned by the host-keyed operations when given no host. It
// is exported because a caller holding a host name has to be able to recognise
// this case rather than treat it as a backend failure.
var ErrEmptyHost = errors.New("empty host")

// Markers are independent TTL keys rather than one status field, so a host can
// be dead and cooling down simultaneously. AllMarkers is the complete set that
// State.Markers reports.
var AllMarkers = [...]MarkerKind{MarkerDead, MarkerCold, MarkerCooldown}

var (
	errMemoryClosed  = errors.New("state is closed")
	errUnknownMarker = errors.New("unknown marker kind")
)

// State is the durable half of the crawler: the frontier, the visited set,
// per-URL attempts and per-host budgets. MemoryState and RedisState both
// implement it, and both must be safe for concurrent use.
//
// The two implementations are not interchangeable in every respect, and one
// difference is load-bearing: SaveHostState must not disturb the fields the
// counters own, because another process sharing the same Redis can move them
// between a caller's read and its save.
type State interface {
	MarkVisited(ctx context.Context, urls ...string) error
	IsVisited(ctx context.Context, url string) (bool, error)

	Enqueue(ctx context.Context, urls ...string) error
	EnqueueAt(ctx context.Context, url string, priority float64) error
	EnqueueDelayed(ctx context.Context, url string, due time.Time) error
	FrontierLen(ctx context.Context) (int64, error)

	PopFrontier(ctx context.Context, budget int) (PopResult, error)
	PromoteDelayed(ctx context.Context, now time.Time, batch int) (int, error)

	URLState(ctx context.Context, url string) (URLState, error)
	BumpAttempts(ctx context.Context, url string) (int, error)
	ClearURLState(ctx context.Context, url string) error

	HostState(ctx context.Context, host string) (HostState, error)

	// SaveHostState persists the fields a robots.txt reading determines:
	// CrawlDelay, MaxPages, WindowStartedAt, RobotsFetchedAt, FirstSeen,
	// Status, Allow, Disallow and SiteMaps.
	//
	// It must not disturb the fields the counters own -- PagesCrawled,
	// ConsecFailures, LastSuccess and SiteMapsClaimedAt. Those move under
	// RecordSuccess, RecordFailure and ClaimSiteMaps, which can run against the
	// same host from another spider process between a caller's read and its
	// save. RedisState gets this for free by never writing them; an in-memory
	// store that assigns the whole struct has to carry them over explicitly.
	SaveHostState(ctx context.Context, host string, st HostState) error

	RecordSuccess(ctx context.Context, host string, at time.Time) error
	RecordFailure(ctx context.Context, host string) (int, error)
	ResetWindow(ctx context.Context, host string, at time.Time) error
	ClaimSiteMaps(ctx context.Context, host string, at time.Time) (bool, error)

	Markers(ctx context.Context, host string) (map[MarkerKind]time.Duration, error)
	SetMarker(ctx context.Context, host string, kind MarkerKind, ttl time.Duration) error
	ClearMarker(ctx context.Context, host string, kind MarkerKind) error

	CountReason(ctx context.Context, host string, reason Reason) error

	Close() error
}

type HostState struct {
	Name string

	CrawlDelay time.Duration

	MaxPages int

	PagesCrawled int

	WindowStartedAt time.Time

	ConsecFailures int

	Status string

	RobotsFetchedAt time.Time

	FirstSeen   time.Time
	LastSuccess time.Time

	Allow    []string
	Disallow []string

	SiteMaps []string

	SiteMapsClaimedAt time.Time

	// compiled caches the compiled form of Allow/Disallow. Rules are compiled
	// once and reused across the URLs of a page, which matters because the
	// frontier admits URLs one at a time and a large sitemap admits thousands
	// against a single HostState.
	//
	// The type is an interface rather than policy.RobotsRules so that this
	// package does not have to know how rules are compiled -- only that they
	// were. Nil means "not compiled", which is the case for every HostState that
	// came back out of a store rather than off a robots.txt reading; the caller
	// that knows the rules compiles them.
	compiled PathRules
}

// PathRules decides whether robots.txt allows a path. policy.RobotsRules is the
// implementation; this package only stores and passes it along.
type PathRules interface {
	// Allows reports whether path is allowed, and the rule that decided it.
	Allows(path string) (allowed bool, rule string)
	Refuses(path string) bool
}

// Rules returns the compiled rules carried by this HostState, or nil if it has
// none. A nil return is normal, not an error: RedisState rebuilds HostState from
// hash fields on every read and so never carries a compiled value across a
// round trip. Callers must compile from Allow/Disallow when they get nil.
func (h HostState) Rules() PathRules {
	return h.compiled
}

// WithCompiled attaches compiled rules to this HostState. Kept separate from
// WithRobots because compiling is policy's job: WithRobots records what
// robots.txt said, and this records the result of reading it back.
func (h HostState) WithCompiled(rules PathRules) HostState {
	h.compiled = rules
	return h
}

func (h HostState) WithRobots(name string, allow, disallow, siteMaps []string, crawlDelay time.Duration, fetchedAt time.Time) HostState {
	out := h
	out.Name = name
	out.Allow = allow
	out.Disallow = disallow
	out.SiteMaps = siteMaps
	out.CrawlDelay = crawlDelay
	out.RobotsFetchedAt = fetchedAt
	if out.WindowStartedAt.IsZero() {
		out.WindowStartedAt = fetchedAt
	}
	return out
}

func (h HostState) BudgetExhausted(globalMax int) bool {
	limit := globalMax
	if h.MaxPages > 0 && (limit <= 0 || h.MaxPages < limit) {
		limit = h.MaxPages
	}
	if limit <= 0 {
		return false
	}
	return h.PagesCrawled >= limit
}

// PopResult reports what one PopFrontier call did. The pop consumes what it
// reads and yields at most one URL, skipping already-visited entries and
// counting them in VisitedSkipped. Exhausted means the frontier ran dry, so the
// caller can stop asking.
type PopResult struct {
	URL string

	Found bool

	Exhausted bool

	VisitedSkipped int
}

type URLState struct {
	URL        string
	Attempts   int
	LastStatus int
	LastError  string
	EnqueuedAt time.Time
}

// Reason is a label for a refusal, so a host's stats can be read as "why are
// these urls not being crawled". The values are policy's vocabulary; the state
// layer only counts them and never interprets one.
type Reason string
