package policy

import (
	"context"
	"testing"
	"time"
)

func TestTakeNextPromotesDueRetriesBeforePopping(t *testing.T) {
	m, st := newTestManagerAt(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	const url = "https://example.com/retry"
	if err := m.Park(ctx, url, m.Now()); err != nil {
		t.Fatal(err)
	}

	if n, err := st.FrontierLen(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("frontier holds %d urls before any TakeNext", n)
	}

	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.Idle {
		t.Fatal("TakeNext reported idle with a due retry parked; nothing else would " +
			"ever promote it, so this URL is lost")
	}
	if next.URL != url {
		t.Errorf("TakeNext = %q, want the promoted retry %q", next.URL, url)
	}
	if next.Promoted != 1 {
		t.Errorf("Promoted = %d, want 1", next.Promoted)
	}
}

func TestTakeNextDoesNotPromoteEarlyRetries(t *testing.T) {
	m, st := newTestManagerAt(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	if err := m.Park(ctx, "https://example.com/later", m.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !next.Idle {
		t.Errorf("TakeNext returned %q for a retry an hour away", next.URL)
	}
	if next.Promoted != 0 {
		t.Errorf("Promoted = %d, want 0", next.Promoted)
	}
	if len(st.Delayed()) != 1 {
		t.Errorf("the future retry left the delayed set; it would never come back")
	}
}

func TestTakeNextReportsIdleOnlyWhenTheFrontierIsEmpty(t *testing.T) {
	t.Run("idle on an empty frontier", func(t *testing.T) {
		m, _ := newTestManager(t)
		next, err := m.TakeNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !next.Idle {
			t.Error("Idle = false on an empty frontier; the loop would keep asking")
		}
		if next.URL != "" {
			t.Errorf("URL = %q on an idle frontier", next.URL)
		}
	})

	t.Run("not idle when a budget-limited scan leaves work behind", func(t *testing.T) {
		m, st := newTestManager(t)
		ctx := context.Background()

		for i := 0; i < 40; i++ {
			url := "https://example.com/seen" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if err := st.Enqueue(ctx, url, url, url); err != nil {
				t.Fatal(err)
			}
			if err := st.MarkVisited(ctx, url); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Enqueue(ctx, "https://example.com/fresh"); err != nil {
			t.Fatal(err)
		}

		next, err := m.TakeNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if next.Idle {
			t.Error("Idle = true with unvisited work behind the stale entries; the " +
				"crawl would stop and those urls would never be crawled")
		}
		if next.URL != "" {
			t.Errorf("URL = %q from a budget-limited scan", next.URL)
		}
	})

	t.Run("an error is not idle", func(t *testing.T) {
		m, st := newTestManager(t)
		st.FailWith = errRedisDown

		next, err := m.TakeNext(context.Background())
		if err == nil {
			t.Error("TakeNext returned no error on a broken frontier")
		}
		if next.Idle {
			t.Error("Idle = true on a broken frontier; the loop would report a " +
				"completed crawl while it is in fact blind")
		}
	})
}

func TestTakeNextSurvivesAPromoteFailure(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	if err := st.Enqueue(ctx, "https://example.com/queued"); err != nil {
		t.Fatal(err)
	}
	st.FailOn = map[string]error{"PromoteDelayed": errRedisDown}

	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatalf("a promote failure stopped the crawl: %v", err)
	}
	if next.Idle {
		t.Fatal("TakeNext reported idle; there was a URL queued")
	}
	if next.URL != "https://example.com/queued" {
		t.Errorf("TakeNext = %q, want the queued url", next.URL)
	}
	if next.Promoted != 0 {
		t.Errorf("Promoted = %d after a failed promote, want 0", next.Promoted)
	}
}

func TestDrainFrontierTakesEverythingThenStops(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if err := st.Enqueue(ctx, "https://example.com/p"+string(rune('a'+i%26))+string(rune('a'+i/26))); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.DrainFrontier(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 25 {
		t.Errorf("drained %d urls, want 25", len(got))
	}

	if left, err := st.FrontierLen(ctx); err != nil {
		t.Fatal(err)
	} else if left != 0 {
		t.Errorf("%d urls left after draining", left)
	}
	again, err := m.DrainFrontier(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second drain returned %d urls from an empty frontier", len(again))
	}
}

func TestDrainFrontierRespectsItsLimit(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := st.Enqueue(ctx, "https://example.com/p"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.DrainFrontier(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("drained %d urls with a limit of 3", len(got))
	}
	if left, _ := st.FrontierLen(ctx); left != 7 {
		t.Errorf("%d urls left, want 7 -- the limit took more than it should", left)
	}
}

func TestDiscoverQueuesAURLItHasAlreadyCrawled(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	const seen = "https://example.com/old"
	if err := st.MarkVisited(ctx, seen); err != nil {
		t.Fatal(err)
	}

	m.Discover(ctx, []string{seen})
	if _, ok := st.Frontier()[seen]; !ok {
		t.Fatal("Discover dropped a visited link; the pop's check is the one that " +
			"was meant to decide this")
	}

	got, err := st.PopFrontier(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Errorf("the pop handed out %q, which had already been crawled", got.URL)
	}
	if !got.Exhausted {
		t.Error("Exhausted = false once the only entry was discarded as visited")
	}
}

func TestDiscoverQueuesLinksAndToleratesAFailure(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	links := []string{"https://example.com/a", "https://example.com/b", "https://example.com/c"}
	m.Discover(ctx, links)

	order := st.FrontierOrder()
	if len(order) != len(links) {
		t.Fatalf("frontier holds %d urls after discovering %d", len(order), len(links))
	}

	st.FailWith = errRedisDown
	m.Discover(ctx, []string{"https://example.com/d"})
	m.Discover(ctx, nil)
}

func TestParkAndRetireReportTheirFailures(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()
	st.FailWith = errRedisDown

	if err := m.Park(ctx, "https://example.com/a", time.Now()); err == nil {
		t.Error("Park reported success on a broken state; the url would be crawled " +
			"immediately instead of after its backoff")
	}
	if err := m.Retire(ctx, "https://example.com/b"); err == nil {
		t.Error("Retire reported success on a broken state; the url would never leave " +
			"the frontier")
	}
}

func TestTakeNextReportsAPopFailure(t *testing.T) {
	m, st := newTestManager(t)
	st.FailOn = map[string]error{"PopFrontier": errRedisDown}

	if _, err := m.TakeNext(context.Background()); err == nil {
		t.Error("TakeNext reported success with an unreadable frontier")
	}
}

func TestDrainFrontierReportsAFailureWithWhatItAlreadyTook(t *testing.T) {
	m, st := newTestManager(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := st.Enqueue(ctx, "https://example.com/p"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	st.FailOn = map[string]error{"PopFrontier": errRedisDown}

	got, err := m.DrainFrontier(ctx, 0)
	if err == nil {
		t.Fatal("DrainFrontier reported success on a broken frontier")
	}
	if len(got) != 0 {
		t.Errorf("DrainFrontier returned %d urls alongside its error, want 0: "+
			"a failed pop produces no url", len(got))
	}
}

func TestParkAndRetireAreTheTwoHalvesOfAVerdict(t *testing.T) {
	m, st := newTestManagerAt(t, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()

	if err := m.Park(ctx, "https://example.com/deferred", m.Now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire(ctx, "https://example.com/skipped"); err != nil {
		t.Fatal(err)
	}

	if err := st.Enqueue(ctx, "https://example.com/skipped", "https://example.com/skipped"); err != nil {
		t.Fatal(err)
	}
	next, err := m.TakeNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.URL == "https://example.com/skipped" {
		t.Error("a retired url was handed out; Skip is not terminal")
	}
}
