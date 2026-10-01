package policy

import (
	"context"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

type MarkerKind int

const (
	MarkerCooldown MarkerKind = iota
	MarkerDead
	MarkerCold
)

// Markers are independent TTL keys rather than one status field, so a host can
// be dead and cooling down simultaneously. AllMarkers is the complete set that
// State.Markers reports.
var AllMarkers = [...]MarkerKind{MarkerDead, MarkerCold, MarkerCooldown}

// State is the durable half of the policy manager: the frontier, the visited
// set, per-URL attempts and per-host budgets. MemoryState and RedisState both
// implement it, and both must be safe for concurrent use.
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
