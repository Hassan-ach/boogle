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

func TestRedisPopFrontierSkipsVisitedAndKeepsGoing(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	const stale = "https://example.com/seen"
	const fresh = "https://example.com/fresh"

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

	if _, err := conn.ZScore(ctx, keys.Frontier(), stale).Result(); err == nil {
		t.Error("the stale entry is still in the frontier after being skipped")
	}
}

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

func TestRedisPopFrontierOrdersByScore(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

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
	if want := entries/budget + 1; calls != want {
		t.Errorf("drained in %d calls, want %d -- more calls than entries/budget means "+
			"some call consumed nothing", calls, want)
	}
}

func TestRedisPopFrontierEmptyStringMemberIsSafe(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

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

func TestRedisPromoteDelayedWithNonPositiveBatch(t *testing.T) {
	for _, batch := range []int{0, -1} {
		t.Run(fmt.Sprintf("batch=%d", batch), func(t *testing.T) {
			st, conn, keys := newTestState(t)
			ctx := context.Background()
			now := time.Now()

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

	n, err := st.PromoteDelayed(ctx, now, 250)
	if err != nil {
		t.Fatal(err)
	}
	if n != due {
		t.Errorf("promoted %d of %d in one pass, want all %d", n, due, due)
	}

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
	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("fourth pass promoted %d, want 0", n)
	}
}

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

func inFrontier(t *testing.T, conn redis.Cmdable, keys Keyspace, url string) bool {
	t.Helper()
	_, err := conn.ZScore(context.Background(), keys.Frontier(), url).Result()
	return err == nil
}

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
