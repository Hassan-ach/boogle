//go:build integration

package policy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The two frontier scripts are the part of this package a fake cannot vouch for.
//
// Everything else in the frontier is Go: which fields a verdict has, when a URL
// is retired, what the loop does with an idle answer. Those are testable against
// MemoryState and are tested there. What MemoryState cannot tell us is whether
// ZPOPMAX inside a script actually removes the member, whether SISMEMBER sees a
// set the same script wrote, whether ZADD NX really leaves an existing score
// alone, and whether unpack survives a batch at the top of its range. Those are
// properties of Redis, and a fake that agreed with us would be evidence of
// nothing.
//
// The original frontier bug lived precisely in that gap. The script popped an
// entry, found it visited, and returned false. That behaviour was correct
// against every hand-written model of it, and wrong against Redis.

// drain pops until the frontier reports itself empty, failing if it does not
// terminate. Every test that needs an empty frontier uses this rather than a
// fixed number of calls, so a regression that reintroduces a non-terminating
// pop shows up as a test failure instead of as a subtly wrong assertion.
func drain(t *testing.T, st *RedisState, budget int) []string {
	t.Helper()
	ctx := context.Background()

	var out []string
	for i := 0; i < 1000; i++ {
		got, err := st.PopFrontier(ctx, budget)
		if err != nil {
			t.Fatalf("pop %d: %v", i, err)
		}
		if got.Found {
			out = append(out, got.URL)
			continue
		}
		if got.Exhausted {
			return out
		}
		// A budget-limited scan with nothing found: keep going, it will drain.
	}
	t.Fatalf("frontier did not report exhaustion within 1000 pops of budget %d", budget)
	return nil
}

func TestRedisPopFrontierOnEmptyFrontierReportsExhausted(t *testing.T) {
	st, _, _ := newTestState(t)

	got, err := st.PopFrontier(context.Background(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Errorf("Found = true with URL %q on an empty frontier", got.URL)
	}
	if !got.Exhausted {
		t.Error("Exhausted = false on an empty frontier; the crawl loop would treat " +
			"this as work still to come and ask again forever")
	}
	if got.VisitedSkipped != 0 {
		t.Errorf("VisitedSkipped = %d on an empty frontier, want 0", got.VisitedSkipped)
	}
}

// TestRedisPopFrontierSkipsVisitedAndKeepsGoing is the core fix, against real
// Redis.
//
// The old script popped the highest entry, saw it was visited, and returned
// false. Two things went wrong at once: the caller could not tell that from an
// empty frontier, and the entry it popped was already gone. So a frontier that
// was mostly stale produced no work at all, while silently destroying the URLs it
// had skipped over -- a URL could be destroyed between being discovered and
// being crawled, with nothing logged.
func TestRedisPopFrontierSkipsVisitedAndKeepsGoing(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	const stale = "https://example.com/seen"
	const fresh = "https://example.com/fresh"

	// The stale entry has the higher score, so it is popped first.
	if err := st.Enqueue(ctx, stale, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkVisited(ctx, stale); err != nil {
		t.Fatal(err)
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found {
		t.Fatal("pop found nothing; unvisited work behind a stale entry was lost")
	}
	if got.URL != fresh {
		t.Errorf("pop = %q, want %q -- the pop gave up at the stale entry", got.URL, fresh)
	}
	if got.VisitedSkipped != 1 {
		t.Errorf("VisitedSkipped = %d, want 1", got.VisitedSkipped)
	}

	// The stale entry is gone, which is correct: it is in the visited set, and
	// leaving it in the frontier would mean re-examining it on every pop.
	if _, err := conn.ZScore(ctx, keys.Frontier(), stale).Result(); err == nil {
		t.Error("the stale entry is still in the frontier after being skipped")
	}
}

// TestRedisPopFrontierRemovesTheReturnedMember is the property that makes the
// old script lossy. ZPOPMAX has to actually remove the member inside the script,
// and it does -- but "the script returns a URL" and "the script removed a URL"
// are separate claims and a regression in either is invisible to the other test.
func TestRedisPopFrontierRemovesTheReturnedMember(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	const url = "https://example.com/once"
	if err := st.Enqueue(ctx, url); err != nil {
		t.Fatal(err)
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.URL != url {
		t.Fatalf("pop = %+v, want %q", got, url)
	}

	if _, err := conn.ZScore(ctx, keys.Frontier(), url).Result(); err == nil {
		t.Error("the returned URL is still in the frontier; it would be crawled again")
	}
	// And a second pop says so, rather than handing it back.
	again, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if again.Found {
		t.Errorf("the second pop returned %q from a one-entry frontier", again.URL)
	}
	if !again.Exhausted {
		t.Error("Exhausted = false after the only entry was taken")
	}
}

// TestRedisPopFrontierOrdersByScore checks ZPOPMAX really is a max-pop against
// the real ordering, which is what makes priority mean anything. A test against
// a fake that sorted correctly would pass even if the script asked for ZPOPMIN.
func TestRedisPopFrontierOrdersByScore(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	// Ten links, one link, five links.
	for i := 0; i < 10; i++ {
		if err := st.Enqueue(ctx, "https://example.com/popular"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Enqueue(ctx, "https://example.com/rare"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := st.Enqueue(ctx, "https://example.com/mid"); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"popular", "mid", "rare"}
	for i, expect := range want {
		got, err := st.PopFrontier(ctx, 16)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Found {
			t.Fatalf("pop %d found nothing, want %q", i, expect)
		}
		if got.URL != "https://example.com/"+expect {
			t.Errorf("pop %d = %q, want %q -- ZPOPMAX is not taking the highest score",
				i, got.URL, "https://example.com/"+expect)
		}
	}
}

// TestRedisPopFrontierBudgetIsHonouredAndTerminates is the property that makes
// the drain bounded, and it is the one that the in-memory fake could not
// honestly check.
//
// Each iteration pops exactly one entry, so a budget of N can consume at most N
// entries however bad the frontier is. The consequence is that a frontier of
// entirely-visited entries drains in a bounded number of calls and then reports
// exhausted, rather than looping. An implementation that checked visited *after*
// removing without removing, or that re-read the frontier inside the loop, would
// either spin or burn more than the budget.
func TestRedisPopFrontierBudgetIsHonouredAndTerminates(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	const entries = 60
	for i := 0; i < entries; i++ {
		url := fmt.Sprintf("https://example.com/seen%02d", i)
		if err := st.Enqueue(ctx, url); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkVisited(ctx, url); err != nil {
			t.Fatal(err)
		}
	}

	const budget = 8

	// The first call is budget-limited: entries are consumed but nothing useful
	// is found, and the frontier is not yet empty.
	first, err := st.PopFrontier(ctx, budget)
	if err != nil {
		t.Fatal(err)
	}
	if first.Found {
		t.Fatalf("an all-visited frontier produced %q", first.URL)
	}
	if first.Exhausted {
		t.Error("Exhausted = true with 52 entries still in the frontier")
	}
	if first.VisitedSkipped != budget {
		t.Errorf("VisitedSkipped = %d, want exactly %d -- the budget was not honoured",
			first.VisitedSkipped, budget)
	}

	// And it terminates: drain the rest and count the calls.
	calls := 1
	for {
		got, err := st.PopFrontier(ctx, budget)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if calls > 100 {
			t.Fatal("frontier did not drain within 100 calls")
		}
		if got.Found {
			t.Fatalf("an all-visited frontier produced %q", got.URL)
		}
		if got.Exhausted {
			break
		}
	}
	// 60 entries at 8 per call, plus the call that discovers the empty frontier.
	if want := entries/budget + 1; calls != want {
		t.Errorf("drained in %d calls, want %d -- more calls than entries/budget means "+
			"some call consumed nothing", calls, want)
	}
}

// TestRedisPopFrontierEmptyStringMemberIsSafe covers a case the type system
// cannot. An empty URL sorts as a valid member, and returning one would hand the
// caller a string that parses as a relative URL -- producing a request to the
// crawler's own host. Enqueue drops empties, but the frontier may be written by
// the migration in a later phase, and the script must not be the thing that
// decides an empty string is a URL.
func TestRedisPopFrontierEmptyStringMemberIsSafe(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	// Written directly, bypassing the guard Enqueue applies.
	if err := conn.ZAdd(ctx, keys.Frontier(), redis.Z{
		Score:  100,
		Member: "",
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := st.Enqueue(ctx, "https://example.com/real"); err != nil {
		t.Fatal(err)
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got.Found && got.URL == "" {
		t.Fatal("the script returned the empty member as a URL; the caller would " +
			"fetch a relative URL resolving to itself")
	}
	// Whether it is found or skipped, the real URL must still come out.
	if !got.Found {
		got, err = st.PopFrontier(ctx, 16)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !got.Found || got.URL != "https://example.com/real" {
		t.Errorf("the real url was lost behind an empty member: %+v", got)
	}
}

// TestRedisPopFrontierWithNonPositiveBudget pins the same resolution the fake
// applies.
//
// The fake resolves a nonsense budget too, so a test against the fake would pass
// whether or not RedisState did -- and the two did disagree, in a way only a test
// covering both could have found.
//
// Both failure directions are checked. Resolving to nothing stalls the crawl while
// it looks idle; resolving to unlimited reintroduces the original unbounded scan.
// The bound is only visible against more stale entries than the default can skip
// in one call, so that is what this sets up.
func TestRedisPopFrontierWithNonPositiveBudget(t *testing.T) {
	for _, budget := range []int{0, -1} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			st, _, _ := newTestState(t)
			ctx := context.Background()

			for i := 0; i < defaultPopBatch+4; i++ {
				url := fmt.Sprintf("https://example.com/seen%02d", i)
				if err := st.Enqueue(ctx, url, url); err != nil {
					t.Fatal(err)
				}
				if err := st.MarkVisited(ctx, url); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Enqueue(ctx, "https://example.com/fresh"); err != nil {
				t.Fatal(err)
			}

			got, err := st.PopFrontier(ctx, budget)
			if err != nil {
				t.Fatal(err)
			}
			if got.Found {
				t.Errorf("budget %d reached %q past %d stale entries; a nonsense budget "+
					"must resolve to the default scan, not to an unlimited one",
					budget, got.URL, defaultPopBatch+4)
			}
			if got.VisitedSkipped != defaultPopBatch {
				t.Errorf("VisitedSkipped = %d at budget %d, want the default %d",
					got.VisitedSkipped, budget, defaultPopBatch)
			}
			if got.Exhausted {
				t.Errorf("Exhausted at budget %d with unvisited work still in the frontier", budget)
			}

			// And the crawl is not stalled: the work is still there for a later
			// call to find.
			for {
				next, err := st.PopFrontier(ctx, 16)
				if err != nil {
					t.Fatal(err)
				}
				if next.Found {
					if next.URL != "https://example.com/fresh" {
						t.Errorf("pop = %q, want the untouched url", next.URL)
					}
					break
				}
				if next.Exhausted {
					t.Fatal("the fresh url was lost; a nonsense budget stalled the crawl")
				}
			}
		})
	}
}

// TestRedisPromoteDelayedWithNonPositiveBatch is the same class of input on the
// other script, with a worse failure mode. A zero promote batch means the delayed
// set is never drained, so every URL that ever failed is parked forever and the
// crawl looks like it completed. The retries are not wrong, they simply never
// happen -- indistinguishable from a healthy crawl with nothing to retry.
func TestRedisPromoteDelayedWithNonPositiveBatch(t *testing.T) {
	// A fresh state per batch, so each one is measured against a full delayed
	// set. Sharing one would make the second call a short pass for an unrelated
	// reason -- the first having already drained most of it.
	for _, batch := range []int{0, -1} {
		t.Run(fmt.Sprintf("batch=%d", batch), func(t *testing.T) {
			st, conn, keys := newTestState(t)
			ctx := context.Background()
			now := time.Now()

			// More than the default batch, so a nonsense batch resolving to one
			// would be visibly different from one resolving to the default.
			const due = defaultPromoteBatch + 10
			for i := 0; i < due; i++ {
				if err := st.EnqueueDelayed(ctx, fmt.Sprintf("https://example.com/r%03d", i), now); err != nil {
					t.Fatal(err)
				}
			}

			n, err := st.PromoteDelayed(ctx, now, batch)
			if err != nil {
				t.Fatal(err)
			}
			if n != defaultPromoteBatch {
				t.Errorf("batch %d promoted %d, want the default %d -- a nonsense batch "+
					"must not park every retry forever", batch, n, defaultPromoteBatch)
			}

			// A short pass is the caller's stop condition, so drain the rest and
			// confirm nothing was stranded.
			total := n
			for {
				more, err := st.PromoteDelayed(ctx, now, 100)
				if err != nil {
					t.Fatal(err)
				}
				total += more
				if more < 100 {
					break
				}
			}
			if total != due {
				t.Errorf("promoted %d in total, want %d", total, due)
			}
			if left, err := conn.ZCard(ctx, keys.Delayed()).Result(); err != nil {
				t.Fatal(err)
			} else if left != 0 {
				t.Errorf("%d urls are still parked; a retry nobody promotes never comes back", left)
			}
		})
	}
}

func TestRedisPromoteDelayedRespectsDueTime(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const url = "https://example.com/retry"
	due := now.Add(30 * time.Second)
	if err := st.EnqueueDelayed(ctx, url, due); err != nil {
		t.Fatal(err)
	}

	// Not due.
	n, err := st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("promoted %d before the url was due", n)
	}
	if inFrontier(t, conn, keys, url) {
		t.Error("a url became available before its retry was due; that is the hot " +
			"loop wearing a backoff's clothes")
	}
	if parked(t, conn, keys, url) == false {
		t.Error("the url left the delayed set without reaching the frontier")
	}

	// Due. The score is a whole second, so this passes the boundary exactly.
	n, err = st.PromoteDelayed(ctx, due, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("promoted %d at the due time, want 1", n)
	}
	if !inFrontier(t, conn, keys, url) {
		t.Error("the url did not reach the frontier when due")
	}
	if parked(t, conn, keys, url) {
		t.Error("the url is still parked after being promoted")
	}
}

// TestRedisPromoteDelayedEntersAtZero is the anti-starvation property. Retries go
// in at the bottom of the queue so a large backlog of failing URLs cannot keep
// real new work from being crawled, and so a failing host is retried politely
// rather than immediately in a tight loop.
func TestRedisPromoteDelayedEntersAtZero(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const url = "https://example.com/retry"
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteDelayed(ctx, now, 100); err != nil {
		t.Fatal(err)
	}

	score, err := conn.ZScore(ctx, keys.Frontier(), url).Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		t.Errorf("promoted url has priority %v, want 0", score)
	}

	// And a real discovery outranks it immediately.
	if err := st.Enqueue(ctx, "https://example.com/new"); err != nil {
		t.Fatal(err)
	}
	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.URL != "https://example.com/new" {
		t.Errorf("pop = %+v, want the fresh discovery to outrank the retry", got)
	}
}

// TestRedisPromoteDelayedPreservesPriorityOnRediscovery is why the script uses
// ZADD NX.
//
// A URL parked for a retry may be discovered again while it waits -- a link to it
// can be found on any of the pages crawled in the meantime. NX keeps the score
// those inlinks earned. A plain ZADD would overwrite it with 0, demoting a page
// that thirty other pages link to, on the sole ground that it had one flaky
// fetch. That is a small thing to get wrong and it would be invisible: the URL is
// still crawled, just much later than its importance warrants.
func TestRedisPromoteDelayedPreservesPriorityOnRediscovery(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const url = "https://example.com/both"
	for i := 0; i < 30; i++ {
		if err := st.Enqueue(ctx, url); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteDelayed(ctx, now, 100); err != nil {
		t.Fatal(err)
	}

	score, err := conn.ZScore(ctx, keys.Frontier(), url).Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != 30 {
		t.Errorf("priority = %v after promotion, want 30 -- the retry demoted a page "+
			"thirty pages link to", score)
	}
}

// TestRedisPromoteDelayedIsIdempotent matters because the crawl promotes on every
// tick with no record of whether it already did. Promoting twice would either
// double the available work or, worse, double-count a URL's priority on every
// pass until it outranked real work.
func TestRedisPromoteDelayedIsIdempotent(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const url = "https://example.com/retry"
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}

	first, err := st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("first pass promoted %d, want 1", first)
	}

	for i := 0; i < 5; i++ {
		n, err := st.PromoteDelayed(ctx, now, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("pass %d promoted %d urls; promotion is not idempotent", i, n)
		}
	}

	score, err := conn.ZScore(ctx, keys.Frontier(), url).Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != 0 {
		t.Errorf("priority = %v after five redundant passes, want 0", score)
	}
}

// TestRedisPromoteDelayedRespectsBatchLimit is the Lua 5.1 constraint.
//
// The script hands the promoted set to ZREM through unpack, which is bounded by
// the Lua stack (LUAI_MAXCSTACK, 8000 slots in practice). A batch that is not
// capped exactly would either overflow the stack -- a Lua error, surfaced as a
// promote failure, so the whole retry mechanism silently stops -- or silently
// promote a partial set while reporting a count the caller then loops on. The
// batch is therefore a correctness boundary, not a tuning knob.
func TestRedisPromoteDelayedRespectsBatchLimit(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const due = 250
	for i := 0; i < due; i++ {
		url := fmt.Sprintf("https://example.com/u%03d", i)
		if err := st.EnqueueDelayed(ctx, url, now); err != nil {
			t.Fatal(err)
		}
	}

	// A batch that would need 500 args through unpack.
	n, err := st.PromoteDelayed(ctx, now, 250)
	if err != nil {
		t.Fatal(err)
	}
	if n != due {
		t.Errorf("promoted %d of %d in one pass, want all %d", n, due, due)
	}

	// The smaller, configured batch is the one that matters.
	if err := conn.Del(ctx, keys.Frontier(), keys.Delayed()).Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < due; i++ {
		url := fmt.Sprintf("https://example.com/v%03d", i)
		if err := st.EnqueueDelayed(ctx, url, now); err != nil {
			t.Fatal(err)
		}
	}

	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Errorf("promoted %d with a batch of 100, want exactly 100", n)
	}
	if left, err := conn.ZCard(ctx, keys.Delayed()).Result(); err != nil {
		t.Fatal(err)
	} else if left != due-100 {
		t.Errorf("%d urls still parked, want %d", left, due-100)
	}

	// The caller loops, and the next pass takes the remainder.
	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Errorf("second pass promoted %d, want 100", n)
	}
	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != due-200 {
		t.Errorf("third pass promoted %d, want %d", n, due-200)
	}
	// A short pass is the caller's stop condition, so the fourth must be empty.
	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("fourth pass promoted %d, want 0", n)
	}
}

// TestRedisPromoteAllDueLoopsUntilShortPass covers the Go-side loop. A batched
// script plus a looping caller is the whole design, and a caller that stopped
// after one pass would leave a large backlog parked for a very long time --
// the retries are not wrong, they are just not happening, which looks identical
// to a healthy crawl with nothing to retry.
func TestRedisPromoteAllDueLoopsUntilShortPass(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const due = 550
	for i := 0; i < due; i++ {
		url := fmt.Sprintf("https://example.com/p%03d", i)
		if err := st.EnqueueDelayed(ctx, url, now); err != nil {
			t.Fatal(err)
		}
	}

	total, err := st.PromoteAllDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if total != due {
		t.Errorf("PromoteAllDue moved %d, want %d -- a single pass would have moved 100", total, due)
	}
	if left, err := conn.ZCard(ctx, keys.Delayed()).Result(); err != nil {
		t.Fatal(err)
	} else if left != 0 {
		t.Errorf("%d urls still parked after draining", left)
	}
	if inFrontierLen(t, conn, keys) != due {
		t.Errorf("frontier holds %d urls, want %d", inFrontierLen(t, conn, keys), due)
	}
}

// TestRedisPromoteAllDueLeavesFutureRetriesAlone is the other half: the loop must
// stop at the boundary, not at zero. Promoting a retry that is not due yet is
// the same as not having a backoff at all.
func TestRedisPromoteAllDueLeavesFutureRetriesAlone(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	if err := st.EnqueueDelayed(ctx, "https://example.com/soon", now); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueDelayed(ctx, "https://example.com/later", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	total, err := st.PromoteAllDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("PromoteAllDue moved %d, want 1", total)
	}
	if inFrontier(t, conn, keys, "https://example.com/later") {
		t.Error("a retry an hour away was promoted; the backoff is not being honoured")
	}
	if !parked(t, conn, keys, "https://example.com/later") {
		t.Error("the future retry was dropped from the delayed set instead of left parked")
	}
}

// TestRedisPopFrontierIsAtomicUnderConcurrency is why the script exists at all.
//
// The read-and-decide here is "is this member in the visited set", and a
// round trip in the middle of it would let a second worker see the same frontier
// state and pop the same URL. With twenty crawlers that is twenty fetches of one
// page -- the fetch is the expensive part, so the duplication costs real
// bandwidth and real politeness to the site.
//
// This asserts every URL is handed out exactly once, not merely that the total
// count matches.
func TestRedisPopFrontierIsAtomicUnderConcurrency(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	const total = 400
	for i := 0; i < total; i++ {
		if err := st.Enqueue(ctx, fmt.Sprintf("https://example.com/p%03d", i)); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 16
	var mu sync.Mutex
	seen := make(map[string]int, total)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := st.PopFrontier(ctx, 8)
				if err != nil {
					t.Errorf("concurrent pop: %v", err)
					return
				}
				if !got.Found {
					if got.Exhausted {
						return
					}
					continue
				}
				mu.Lock()
				seen[got.URL]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Errorf("%d distinct urls were handed out, want %d -- %d were lost or duplicated",
			len(seen), total, total-len(seen))
	}
	for url, n := range seen {
		if n != 1 {
			t.Errorf("%s was handed out %d times; two crawlers fetched the same page", url, n)
		}
	}
}

// TestRedisPromoteDelayedIsAtomicUnderConcurrency covers the other script's
// reason for existing.
//
// Move-then-remove would let two callers promote the same URL, double-counting
// it. Remove-then-move would lose it outright if the process died between the
// two. Both are invisible in a single-threaded test and both are what a
// non-atomic implementation would do in production.
func TestRedisPromoteDelayedIsAtomicUnderConcurrency(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	now := time.Now()

	const total = 300
	for i := 0; i < total; i++ {
		if err := st.EnqueueDelayed(ctx, fmt.Sprintf("https://example.com/d%03d", i), now); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// A short pass means the delayed set has nothing more that is
				// due. Stopping on exactly that is the loop's stop condition.
				n, err := st.PromoteDelayed(ctx, now, 16)
				if err != nil {
					t.Errorf("concurrent promote: %v", err)
					return
				}
				if n < 16 {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Every URL is now in the frontier exactly once, and gone from the delayed
	// set. A double promotion shows as a count above the total; a lost one shows
	// as a URL in both sets or in neither.
	frontierLen, err := conn.ZCard(ctx, keys.Frontier()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if frontierLen != total {
		t.Errorf("frontier holds %d urls, want %d", frontierLen, total)
	}
	delayedLeft, err := conn.ZCard(ctx, keys.Delayed()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if delayedLeft != 0 {
		t.Errorf("%d urls are still parked after eight workers drained the set", delayedLeft)
	}

	// And the promoted scores are 0, which a ZADD without NX would also give
	// here -- so this is about the count, checked above.
	for _, url := range []string{"https://example.com/d000", "https://example.com/d299"} {
		score, err := conn.ZScore(ctx, keys.Frontier(), url).Result()
		if err != nil {
			t.Errorf("%s is missing from the frontier: %v", url, err)
			continue
		}
		if score != 0 {
			t.Errorf("%s has priority %v, want 0", url, score)
		}
	}
}

// inFrontier reports whether a URL is a member of the frontier.
func inFrontier(t *testing.T, conn redis.Cmdable, keys Keyspace, url string) bool {
	t.Helper()
	_, err := conn.ZScore(context.Background(), keys.Frontier(), url).Result()
	return err == nil
}

// parked reports whether a URL is still in the delayed set.
func parked(t *testing.T, conn redis.Cmdable, keys Keyspace, url string) bool {
	t.Helper()
	_, err := conn.ZScore(context.Background(), keys.Delayed(), url).Result()
	return err == nil
}

func inFrontierLen(t *testing.T, conn redis.Cmdable, keys Keyspace) int64 {
	t.Helper()
	n, err := conn.ZCard(context.Background(), keys.Frontier()).Result()
	if err != nil {
		t.Fatal(err)
	}
	return n
}
