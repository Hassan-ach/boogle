package policy

import (
	"context"
	"time"
)

// The crawl-facing half of the policy manager: what to fetch next, and where a
// URL goes afterwards.
//
// These are the only entry points the crawl loop uses to touch the frontier.
// Keeping them here rather than in the loop means the ordering rule -- promote
// before pop -- is stated once, and cannot be forgotten by a second caller.

// Next is one unit of work for the crawl loop, and the reason it got it.
type Next struct {
	// URL is the raw URL to fetch, exactly as it was discovered. Not
	// normalised: normalisation is a policy question, and the policy question
	// is answered by Admit, which is the next call the loop makes.
	URL string

	// Idle reports that the frontier is definitively empty. The loop should
	// stop asking this round rather than treat it as an error or, worse, spin.
	Idle bool

	// Promoted is how many parked URLs became available on this call. It is
	// here so the loop can log a crawl that is doing nothing but draining
	// retries, which looks identical to a crawl that is stuck.
	Promoted int
}

// TakeNext promotes due retries and then hands back the next URL to crawl.
//
// Promotion runs first and unconditionally. There is no timer anywhere in this
// design: a parked URL becomes available because a crawl happens to look for it.
// That is a deliberate simplification over a prober goroutine -- there is no
// process to keep alive, no leader to elect, and no sweep to miss -- but it has
// an obvious failure mode, which is that nothing ever looks. Running it here,
// on the path that every crawl already takes, is what makes the looking
// unconditional.
//
// A promotion failure is not fatal to the pop. The two sets are independent: a
// URL already in the frontier is still worth crawling even if the delayed set
// could not be drained this round. The error is reported on the Next so the loop
// can log it, and the pop proceeds.
func (m *PolicyManager) TakeNext(ctx context.Context) (Next, error) {
	batch := m.cfg.DelayedPromoteBatch
	if batch < 1 {
		batch = defaultPromoteBatch
	}

	promoted, err := m.state.PromoteDelayed(ctx, m.now(), batch)
	if err != nil {
		m.log.Warn("could not promote due retries; the frontier is crawled without them",
			"error", err)
	}

	popBatch := m.cfg.FrontierPopBatch
	if popBatch < 1 {
		popBatch = defaultPopBatch
	}

	got, err := m.state.PopFrontier(ctx, popBatch)
	if err != nil {
		// A frontier that cannot be read is not an empty frontier. Reporting it
		// as one would make the loop idle cleanly and come straight back, which
		// reads as a healthy crawl doing no work while it is in fact blind.
		return Next{}, err
	}

	return Next{
		URL:      got.URL,
		Idle:     !got.Found && got.Exhausted,
		Promoted: promoted,
	}, nil
}

// DrainFrontier takes URLs until the frontier is empty, returning how many came
// out. It exists for tests and for a caller that wants the whole set rather than
// one at a time; the crawl loop does not use it, because a single caller
// draining everything would serialise the workers it is meant to feed.
func (m *PolicyManager) DrainFrontier(ctx context.Context, limit int) ([]string, error) {
	popBatch := m.cfg.FrontierPopBatch
	if popBatch < 1 {
		popBatch = defaultPopBatch
	}

	var out []string
	for i := 0; limit <= 0 || i < limit; i++ {
		got, err := m.state.PopFrontier(ctx, popBatch)
		if err != nil {
			return out, err
		}
		if !got.Found {
			// Budget-limited scan of a stale frontier: the entries are gone but
			// unvisited work may sit behind them, so keep going.
			if got.Exhausted {
				return out, nil
			}
			continue
		}
		out = append(out, got.URL)
	}
	return out, nil
}

// Discover adds links found on a crawled page to the frontier.
//
// Each link increments the URL's score, which is why a page many pages link to
// is crawled before one linked from a single place. That is the cheapest
// priority signal available: it is free, it needs no second pass over the
// corpus, and it is a reasonable proxy for "pages worth indexing first".
//
// Links that cannot be queued are logged and skipped rather than failing the
// page. One malformed link out of two hundred should not cost the other hundred
// ninety-nine, and the cost of a lost link is small next to the cost of dropping
// a whole page's worth of discovery.
func (m *PolicyManager) Discover(ctx context.Context, links []string) {
	if len(links) == 0 {
		return
	}
	if err := m.state.Enqueue(ctx, links...); err != nil {
		m.log.Warn("could not enqueue discovered links",
			"count", len(links), "error", err)
	}
}

// Park defers a URL to a due time, the whole of the retry mechanism.
//
// There is no goroutine waiting for it and no timer that will fire. A parked URL
// is one sorted-set member, and it comes back because the next crawl promotes
// it. A parked URL nobody promotes simply never returns, which is correct: if
// the crawl has stopped, nothing is crawling.
func (m *PolicyManager) Park(ctx context.Context, url string, due time.Time) error {
	if err := m.state.EnqueueDelayed(ctx, url, due); err != nil {
		return err
	}
	return nil
}

// Retire marks a URL permanently uninteresting.
//
// This is what makes a Skip terminal. Without it, a skipped URL stays in the
// frontier and is handed out again on the next pop, rejected by the same rule,
// for as long as the crawl runs -- the exact shape of the loop this package was
// written to remove. The distinction between Defer and Skip is entirely whether
// this gets called.
func (m *PolicyManager) Retire(ctx context.Context, url string) error {
	return m.state.MarkVisited(ctx, url)
}
