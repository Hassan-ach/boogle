package policy

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The frontier is where URLs are lost. The original script popped the
// highest-scoring entry, found it already visited, and returned false -- the
// popped entry was already gone from the ZSET, so it was destroyed rather than
// handed back. The caller could not distinguish that from an empty frontier, so
// it looped maxRetry times, and every URL it burned on the way was a URL the
// crawl would never see.
//
// These tests use MemoryState because the semantics under test are the Go side's
// -- what the caller does with Found, Exhausted and VisitedSkipped. The Lua
// itself is tested against a real Redis, where a fake cannot prove that ZPOPMAX
// and SISMEMBER behave as the script assumes.

// TestPopFrontierTakesHighestPriorityFirst is the ordering guarantee: the
// frontier is scored by inlink count precisely so that a page many pages link to
// is crawled before one linked from a single place.
func TestPopFrontierTakesHighestPriorityFirst(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	// Enqueue "common" five times, "mid" twice, "rare" once.
	for i := 0; i < 5; i++ {
		if err := st.Enqueue(ctx, "https://example.com/common"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := st.Enqueue(ctx, "https://example.com/mid"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Enqueue(ctx, "https://example.com/rare"); err != nil {
		t.Fatal(err)
	}

	want := []string{"common", "mid", "rare"}
	for i, expect := range want {
		got, err := st.PopFrontier(ctx, 16)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Found {
			t.Fatalf("pop %d found nothing, want %q", i, expect)
		}
		if got.URL != "https://example.com/"+expect {
			t.Errorf("pop %d = %q, want %q", i, got.URL, "https://example.com/"+expect)
		}
	}
}

// TestPopFrontierRemovesWhatItReturns is the property the original script got
// wrong. A popped URL must be gone from the frontier, or the same page is crawled
// once per pop until the crawl times out.
func TestPopFrontierRemovesWhatItReturns(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	const entries = 3
	for i := 0; i < entries; i++ {
		if err := st.Enqueue(ctx, "https://example.com/page"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < entries; i++ {
		got, err := st.PopFrontier(ctx, 16)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Found {
			t.Fatalf("pop %d found nothing; a returned URL was not removed from the frontier", i)
		}
	}

	// The frontier is empty now, so the next pop must say so rather than
	// handing back one of the three.
	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Errorf("pop returned %q a fourth time from a three-entry frontier", got.URL)
	}
}

// TestPopFrontierSkipsVisitedWithoutLosingWork is the core fix. Entries already
// visited are discarded, but a *later* unvisited entry must still come out. The
// original script destroyed the entry it skipped and gave up, so a frontier that
// was mostly stale produced nothing at all while quietly losing the URLs it
// skipped over.
func TestPopFrontierSkipsVisitedWithoutLosingWork(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	// Highest priority, and already visited.
	if err := st.Enqueue(ctx, "https://example.com/seen", "https://example.com/seen", "https://example.com/seen"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkVisited(ctx, "https://example.com/seen"); err != nil {
		t.Fatal(err)
	}
	// Lower priority, not visited.
	if err := st.Enqueue(ctx, "https://example.com/fresh"); err != nil {
		t.Fatal(err)
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found {
		t.Fatal("pop found nothing; unvisited work behind a stale entry was lost")
	}
	if got.URL != "https://example.com/fresh" {
		t.Errorf("pop = %q, want the unvisited url behind the stale entry", got.URL)
	}
	if got.VisitedSkipped != 1 {
		t.Errorf("VisitedSkipped = %d, want 1 -- the stale entry should be counted, not "+
			"silently destroyed", got.VisitedSkipped)
	}
}

// TestPopFrontierExhaustedIsDistinctFromNotFound is what makes the crawl loop
// terminate. Both cases return Found=false, and confusing them either idles the
// crawl with unvisited URLs still in the frontier, or loops forever asking about
// an empty one.
func TestPopFrontierExhaustedIsDistinctFromNotFound(t *testing.T) {
	t.Run("an empty frontier reports exhausted", func(t *testing.T) {
		_, st := newTestManager(t)
		got, err := st.PopFrontier(context.Background(), 16)
		if err != nil {
			t.Fatal(err)
		}
		if got.Found {
			t.Error("Found on an empty frontier")
		}
		if !got.Exhausted {
			t.Error("Exhausted = false on an empty frontier; the caller would keep " +
				"asking about a frontier with nothing in it")
		}
	})

	t.Run("a budget-limited scan of stale entries is not exhausted", func(t *testing.T) {
		_, st := newTestManager(t)
		ctx := context.Background()

		// Ten stale entries at the top, then one real one underneath.
		for i := 0; i < 10; i++ {
			url := "https://example.com/seen" + string(rune('a'+i))
			if err := st.Enqueue(ctx, url, url, url, url); err != nil {
				t.Fatal(err)
			}
			if err := st.MarkVisited(ctx, url); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Enqueue(ctx, "https://example.com/fresh"); err != nil {
			t.Fatal(err)
		}

		// A budget of 4 cannot reach past the stale entries.
		got, err := st.PopFrontier(ctx, 4)
		if err != nil {
			t.Fatal(err)
		}
		if got.Found {
			t.Fatal("a budget of 4 should not have reached the url past ten stale entries")
		}
		if got.Exhausted {
			t.Error("Exhausted = true with unvisited work still in the frontier; the " +
				"crawl would go idle and those urls would never come back")
		}
		if got.VisitedSkipped != 4 {
			t.Errorf("VisitedSkipped = %d, want 4", got.VisitedSkipped)
		}
	})
}

// TestPopFrontierEventuallyDrainsAnAllStaleFrontier checks the loop terminates.
// Every entry is visited, so every pop burns budget finding nothing useful, but
// each one consumes entries, so the frontier reaches empty in bounded calls.
func TestPopFrontierEventuallyDrainsAnAllStaleFrontier(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	const entries = 50
	for i := 0; i < entries; i++ {
		url := "https://example.com/seen" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if err := st.Enqueue(ctx, url); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkVisited(ctx, url); err != nil {
			t.Fatal(err)
		}
	}

	const budget = 8
	const maxCalls = entries/budget + 5
	calls := 0
	for {
		calls++
		if calls > maxCalls {
			t.Fatalf("frontier did not drain after %d calls with a budget of %d", calls, budget)
		}
		got, err := st.PopFrontier(ctx, budget)
		if err != nil {
			t.Fatal(err)
		}
		if got.Found {
			t.Fatal("an all-visited frontier produced a url")
		}
		if got.Exhausted {
			break
		}
	}
	if calls > maxCalls {
		t.Errorf("took %d calls to drain, want fewer than %d", calls, maxCalls)
	}
}

// TestToStringAndToIntCoverWhatRedisActuallySends is a table over the conversions
// because the failure mode they guard against is silent.
//
// Redis returns every element of a Lua table as a bulk string, so the counts come
// back as []byte("1"). Code written for the integer case reads them as 0, and a 0
// "found" flag means every pop reports nothing to do -- the crawl never runs, and
// nothing anywhere reports an error. The integration suite would catch that too,
// but attributing it here is the difference between a five-second and a
// five-minute investigation.
func TestToStringAndToIntCoverWhatRedisActuallySends(t *testing.T) {
	t.Run("toString", func(t *testing.T) {
		for _, tc := range []struct {
			in   any
			want string
		}{
			{nil, ""},
			{"abc", "abc"},
			{[]byte("abc"), "abc"},
			{int64(42), "42"},
			{float64(42), "42"},
			{3.9, "3"},
			{true, "true"},
		} {
			if got := toString(tc.in); got != tc.want {
				t.Errorf("toString(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("toInt", func(t *testing.T) {
		for _, tc := range []struct {
			in   any
			want int
		}{
			{nil, 0},
			{int64(7), 7},
			{float64(7), 7},
			{"7", 7},
			{[]byte("7"), 7},
			{"", 0},
			{"not a number", 0},
			{true, 0},
		} {
			if got := toInt(tc.in); got != tc.want {
				t.Errorf("toInt(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		}
	})
}

// TestPopFrontierWithNonPositiveBudget pins what a nonsense budget does.
//
// Taken literally, a budget of zero means "do nothing", which would stop the
// entire crawl without any error -- and FRONTIER_POP_BATCH comes from the
// environment, so a zero is a realistic input. It resolves to the package default
// instead, because a nonsensical configuration value should degrade to the
// documented behaviour rather than to a degenerate one.
//
// Both failure directions are checked, because either one alone would be a bug:
//
//   - resolving to nothing stalls the crawl while looking idle;
//   - resolving to unlimited reintroduces the original bug, where a pop consumed
//     visited entries without limit and never terminated.
//
// The bound is only visible against more stale entries than the default can skip
// in one call, so that is what this sets up. RedisState resolves it identically,
// and the integration suite has the same test against the real script.
func TestPopFrontierWithNonPositiveBudget(t *testing.T) {
	for _, budget := range []int{0, -1} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			_, st := newTestManager(t)
			ctx := context.Background()

			// More stale entries than the default batch can skip, each at a
			// higher score than the real one so it is popped first.
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

			// And the crawl is not stalled: draining what is left still finds the
			// work, one call per stale entry as before.
			drained := drainMemory(t, st, 4)
			if len(drained) != 1 || drained[0] != "https://example.com/fresh" {
				t.Errorf("draining the remainder gave %v, want the untouched url", drained)
			}
		})
	}
}

// drainMemory pops until the frontier reports itself empty, so a test that needs
// an empty frontier does not depend on how many calls that takes.
func drainMemory(t *testing.T, st *MemoryState, budget int) []string {
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

// TestPromoteDelayedRespectsDueTime is the retry mechanism. A URL parked for a
// retry must not come back early -- that is the hot loop wearing a backoff's
// clothes -- and must come back once its time has passed.
// TestPopFrontierBreaksScoreTiesByMember is what makes this fake usable for
// testing the script's behaviour at all.
//
// Redis orders equal-score members lexicographically, so two URLs discovered once
// each come out in a fixed order. The fake has to agree, or a test that asserts
// *which* of two equally-ranked URLs is returned would pass or fail for a reason
// that has nothing to do with the code under test -- and, worse, would keep passing
// after the scoring logic changed underneath it.
//
// This also pins the determinism itself. An order that varies between runs makes
// every ordering assertion in the package a coin flip.
func TestPopFrontierBreaksScoreTiesByMember(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	// Enqueued in reverse order, so nothing about insertion order can explain the
	// result. All at score 1.
	for _, u := range []string{"zebra", "mango", "apple", "kiwi"} {
		if err := st.Enqueue(ctx, "https://example.com/"+u); err != nil {
			t.Fatal(err)
		}
	}

	got := st.FrontierOrder()
	want := []string{
		"https://example.com/apple",
		"https://example.com/kiwi",
		"https://example.com/mango",
		"https://example.com/zebra",
	}
	if len(got) != len(want) {
		t.Fatalf("frontier holds %d urls, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i], want[i])
		}
	}

	// And the pop follows the same order.
	for i, expect := range want {
		pop, err := st.PopFrontier(ctx, 16)
		if err != nil {
			t.Fatal(err)
		}
		if pop.URL != expect {
			t.Errorf("pop %d = %q, want %q", i, pop.URL, expect)
		}
	}
}

func TestPromoteDelayedRespectsDueTime(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const url = "https://example.com/retry"
	if err := st.EnqueueDelayed(ctx, url, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Not due yet.
	n, err := st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("promoted %d before the url was due", n)
	}
	if _, ok := st.Frontier()["https://example.com/retry"]; ok {
		t.Error("a url was made available before its retry was due")
	}

	// Exactly due counts as due.
	n, err = st.PromoteDelayed(ctx, now.Add(30*time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("promoted %d at the due time, want 1", n)
	}
	if _, ok := st.Frontier()["https://example.com/retry"]; !ok {
		t.Error("the url did not reach the frontier when due")
	}
}

// TestPromoteDelayedIsIdempotent matters because the crawl calls promote on every
// tick and there is no record of whether it has already run. Promoting twice
// would double the work available and, worse, could double-count priorities.
func TestPromoteDelayedIsIdempotent(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const url = "https://example.com/retry"
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}

	// The first pass has real work to do; the ones after it must find none.
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
			t.Errorf("pass %d promoted %d urls; promotion must be idempotent", i, n)
		}
	}
	if score := st.Frontier()[url]; score != 0 {
		t.Errorf("frontier score = %v after repeated promotion, want 0", score)
	}
	if left := len(st.Delayed()); left != 0 {
		t.Errorf("%d urls still parked after promotion", left)
	}
}

// TestMemoryPromoteDelayedEntersAtZero is the anti-starvation property, on the
// fake. The integration suite has the same test against the Lua; this one exists
// so a regression in either implementation is attributed to that implementation
// rather than to whichever happens to run first.
func TestMemoryPromoteDelayedEntersAtZero(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const url = "https://example.com/retry"
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteDelayed(ctx, now, 100); err != nil {
		t.Fatal(err)
	}

	if score := st.Frontier()[url]; score != 0 {
		t.Errorf("promoted url has priority %v, want 0 -- a backlog of failing urls "+
			"would otherwise outrank real new work", score)
	}

	// And a single fresh discovery outranks it immediately.
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

// TestPromoteDelayedPreservesPriorityOnRediscovery is why the script uses ZADD
// NX. A URL parked for a retry may be discovered again in the meantime; NX keeps
// the score it earned from those inlinks instead of resetting it to zero.
func TestPromoteDelayedPreservesPriorityOnRediscovery(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const url = "https://example.com/both"
	// Discovered 30 times while parked.
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

	score := st.Frontier()[url]
	if score != 30 {
		t.Errorf("frontier score = %v after promotion, want 30 -- the retry demoted a "+
			"page that thirty pages link to", score)
	}
}

// TestPromoteDelayedRespectsBatchLimit covers the Lua 5.1 unpack constraint. The
// batch is a stack-safety limit, so it must be honoured exactly rather than
// promoting everything that happens to be due.
func TestPromoteDelayedRespectsBatchLimit(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const due = 25
	for i := 0; i < due; i++ {
		if err := st.EnqueueDelayed(ctx, "https://example.com/u"+string(rune('a'+i)), now); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.PromoteDelayed(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("promoted %d with a batch of 10, want exactly 10", n)
	}
	if left := len(st.Delayed()); left != due-10 {
		t.Errorf("%d urls still parked, want %d", left, due-10)
	}

	// The caller loops, and the second pass takes the rest.
	n, err = st.PromoteDelayed(ctx, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != due-10 {
		t.Errorf("second pass promoted %d, want %d", n, due-10)
	}
	if left := len(st.Delayed()); left != 0 {
		t.Errorf("%d urls still parked after draining, want 0", left)
	}
}

// TestPromoteDelayedAndPopRoundTrip is the end-to-end path a retry takes: parked,
// due, back in the frontier, and popped out of it exactly once.
func TestPromoteDelayedAndPopRoundTrip(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	const url = "https://example.com/flaky"
	if err := st.EnqueueDelayed(ctx, url, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteDelayed(ctx, now, 100); err != nil {
		t.Fatal(err)
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.URL != url {
		t.Fatalf("pop = %+v, want %q", got, url)
	}
	// And it is not offered twice.
	again, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if again.Found {
		t.Errorf("the retried url was popped twice: %q", again.URL)
	}
	if !again.Exhausted {
		t.Error("the frontier should be empty after the only url was taken")
	}
}

// TestFrontierRoundTripFailsClosed is the safety property applied to the new
// operations. Both are on the hot path and both can fail; treating a failure as
// "nothing available" would be right, but treating it as "keep going" would mean
// the crawl fetches URLs whose cooldowns and visited state it could not read.
func TestFrontierRoundTripFailsClosed(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	st.FailWith = errRedisDown

	if _, err := st.PopFrontier(ctx, 16); err == nil {
		t.Error("PopFrontier returned no error on a broken state")
	}
	if _, err := st.PromoteDelayed(ctx, time.Now(), 100); err == nil {
		t.Error("PromoteDelayed returned no error on a broken state")
	}
	if _, err := st.FrontierLen(ctx); err == nil {
		t.Error("FrontierLen returned no error on a broken state")
	}
}

// TestPopFrontierWithAnInconsistentScriptAnswer covers the decoder, which is the
// one piece of the Redis path testable without a Redis.
//
// Both contradictions below would be handed straight to the crawl loop as-is, and
// the first would be worse than a crash: "" parses as a relative URL, so a fetch
// of it resolves to the crawler's own host. A loud error costs one round; a
// silent empty URL costs a request to yourself on every pop.
//
// The wrong-field-count case is the same argument in the other direction. Guessing
// which field means what does not produce an error, it produces a crawl that
// quietly does nothing -- the hardest failure in this package to notice, because
// an idle crawl and a broken one look identical from the outside.
func TestPopFrontierWithAnInconsistentScriptAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
	}{
		{"found with an empty url", []any{"", int64(1), int64(0), int64(0)}},
		{"a url with no found flag", []any{"https://example.com/a", int64(0), int64(0), int64(0)}},
		{"too few fields", []any{"https://example.com/a", int64(1), int64(0)}},
		{"too many fields", []any{"https://example.com/a", int64(1), int64(0), int64(0), "extra"}},
		{"not an array at all", "https://example.com/a"},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePopResult(tc.raw)
			if err == nil {
				t.Fatalf("parsePopResult(%v) = %+v with no error", tc.raw, got)
			}
			if got.Found {
				t.Error("a PopResult was returned alongside the error; the caller " +
					"might use it anyway")
			}
		})
	}
}

// TestPopFrontierWithAWellFormedScriptAnswer is the other half: the decoder must
// not reject the three shapes the script actually produces, or the checks above
// are just a way of refusing to work.
func TestPopFrontierWithAWellFormedScriptAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want PopResult
	}{
		{"a url", []any{"https://example.com/a", int64(1), int64(0), int64(0)},
			PopResult{URL: "https://example.com/a", Found: true}},
		{"exhausted after skipping", []any{"", int64(0), int64(1), int64(3)},
			PopResult{Exhausted: true, VisitedSkipped: 3}},
		{"budget ran out", []any{"", int64(0), int64(0), int64(16)},
			PopResult{VisitedSkipped: 16}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePopResult(tc.raw)
			if err != nil {
				t.Fatalf("parsePopResult(%v): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("parsePopResult(%v) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPopResultToleratesBytesAndStrings is a property of how Redis actually
// replies, not of the script.
//
// Every element of a Lua table comes back as a bulk string, so the counts arrive
// as []byte("1") rather than as an int. A decoder written for the int case
// silently reads them as 0 -- which would make every pop report "not found" and
// the crawl would never run at all, with no error anywhere to explain it. The
// integration suite would catch that; this pins it at the unit level so the
// failure is attributed here rather than there.
func TestPopResultToleratesBytesAndStrings(t *testing.T) {
	got, err := parsePopResult([]any{[]byte("https://example.com/a"), []byte("1"), []byte("0"), []byte("2")})
	if err != nil {
		t.Fatal(err)
	}
	want := PopResult{URL: "https://example.com/a", Found: true, VisitedSkipped: 2}
	if got != want {
		t.Errorf("parsePopResult = %+v, want %+v", got, want)
	}
}

var errRedisDown = errTest("redis is unreachable")

type errTest string

func (e errTest) Error() string { return string(e) }
