package policy

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPopFrontierTakesHighestPriorityFirst(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

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

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Errorf("pop returned %q a fourth time from a three-entry frontier", got.URL)
	}
}

func TestPopFrontierSkipsVisitedWithoutLosingWork(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

	if err := st.Enqueue(ctx, "https://example.com/seen", "https://example.com/seen", "https://example.com/seen"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkVisited(ctx, "https://example.com/seen"); err != nil {
		t.Fatal(err)
	}
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

func TestPopFrontierWithNonPositiveBudget(t *testing.T) {
	for _, budget := range []int{0, -1} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			_, st := newTestManager(t)
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

			drained := drainMemory(t, st, 4)
			if len(drained) != 1 || drained[0] != "https://example.com/fresh" {
				t.Errorf("draining the remainder gave %v, want the untouched url", drained)
			}
		})
	}
}

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

func TestPopFrontierBreaksScoreTiesByMember(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()

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

func TestPromoteDelayedIsIdempotent(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

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

func TestPromoteDelayedPreservesPriorityOnRediscovery(t *testing.T) {
	_, st := newTestManager(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

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

	score := st.Frontier()[url]
	if score != 30 {
		t.Errorf("frontier score = %v after promotion, want 30 -- the retry demoted a "+
			"page that thirty pages link to", score)
	}
}

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
