//go:build integration

package policy

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestRedisRobotsAreCachedAcrossManagers(t *testing.T) {
	st, conn, _ := newTestState(t)
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	var firstCalls, secondCalls int
	body := "User-agent: *\nDisallow: /private/\nSitemap: https://example.com/s.xml\n"

	m1 := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			firstCalls++
			return []byte(body), 200, nil
		}))
	m2 := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			secondCalls++
			t.Error("a second manager re-fetched robots.txt already in Redis")
			return nil, 0, errConnRefused
		}))

	if v, err := m1.Admit(ctx, "https://example.com/article"); err != nil || v.Kind != Allow {
		t.Fatalf("first Admit = %s / %v, want Allow", FormatVerdict(v), err)
	}
	if firstCalls != 1 {
		t.Fatalf("the first manager made %d fetches, want 1", firstCalls)
	}

	v, err := m2.Admit(ctx, "https://example.com/private/secret")
	if err != nil {
		t.Fatalf("second Admit: %v", err)
	}
	if v.Kind != Skip || v.Reason != ReasonRobotsDisallow {
		t.Errorf("second manager = %s, want skip/robots_disallow from the cached rules",
			FormatVerdict(v))
	}

	m3 := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			t.Error("a fresh connection re-fetched robots.txt already in Redis")
			return nil, 0, errConnRefused
		}))
	if v, err := m3.Admit(ctx, "https://example.com/other"); err != nil || v.Kind != Allow {
		t.Errorf("third Admit = %s / %v, want Allow from the cached rules", FormatVerdict(v), err)
	}

	state, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SiteMaps) != 1 || state.SiteMaps[0] != "https://example.com/s.xml" {
		t.Errorf("sitemaps did not survive: %v", state.SiteMaps)
	}
	_ = conn
}

func TestRedisNegativeCacheSurvivesARestart(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	calls := 0
	fetcher := countingRobots(func(_ context.Context, url string) ([]byte, int, error) {
		calls++
		return nil, 0, &net.DNSError{Err: "no such host", Name: url}
	})

	m := New(cfg, st, testLogger()).WithRobotsFetcher(fetcher)

	if v, err := m.Admit(ctx, "https://dead.example/first"); err != nil {
		t.Fatalf("Admit: %v", err)
	} else if v.Kind != Defer {
		t.Fatalf("Admit = %s, want Defer on an unresolvable host", FormatVerdict(v))
	}
	if calls != 1 {
		t.Fatalf("first Admit made %d fetches, want 1", calls)
	}

	ttl, err := conn.PTTL(ctx, keys.HostMarker("dead.example", MarkerDead)).Result()
	if err != nil {
		t.Fatalf("the dead marker is not in Redis: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("dead marker TTL = %v, want a positive deadline; without one the "+
			"host could never be probed again", ttl)
	}
	want := cfg.DeadHostTTL(1)
	if ttl > want+2*time.Second || ttl < want-2*time.Second {
		t.Errorf("dead marker TTL = %v, want about DeadHostTTL(1) = %v", ttl, want)
	}

	fresh := NewRedisState(conn, prefixOf(t, st), time.Hour)
	m2 := New(cfg, fresh, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			t.Error("a restarted crawler re-fetched robots.txt for a host already marked dead")
			return nil, 0, errConnRefused
		}))

	for range 50 {
		v, err := m2.Admit(ctx, uniqueURL("dead.example", "/page"))
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if v.Kind != Skip || v.Reason != ReasonHostDead {
			t.Fatalf("a URL on a host marked dead in Redis was %s, want skip/host_dead",
				FormatVerdict(v))
		}
	}
	if calls != 1 {
		t.Errorf("50 URLs on a dead host caused %d fetches, want 1", calls)
	}
}

func TestRedisRobotsCacheIsNotWrittenOnFailure(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	m := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			return nil, 0, errConnRefused
		}))

	if _, err := m.EnsureHost(ctx, "gone.example"); err == nil {
		t.Fatal("EnsureHost succeeded on an unreachable host")
	}

	state, err := st.HostState(ctx, "gone.example")
	if err != nil {
		t.Fatal(err)
	}
	if !state.RobotsFetchedAt.IsZero() {
		t.Errorf("robots_at = %v on a host that was never reached", state.RobotsFetchedAt)
	}
	if len(state.Allow) != 0 || len(state.Disallow) != 0 {
		t.Errorf("rules = %v/%v on a host that was never reached",
			state.Allow, state.Disallow)
	}

	if raw := conn.HGet(ctx, keys.HostState("gone.example"), fieldRobotsAt).Val(); raw != "" {
		t.Errorf("robots_at is in Redis as %q on a host that was never reached", raw)
	}
}

func TestRedisCountersPersistAcrossManagers(t *testing.T) {
	st, conn, _ := newTestState(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	body := "User-agent: *\nDisallow: /private/\n"
	m := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) { return []byte(body), 200, nil }))

	for range 3 {
		if _, err := m.Admit(ctx, uniqueURL("example.com", "/private/x")); err != nil {
			t.Fatal(err)
		}
	}

	fresh := NewRedisState(conn, prefixOf(t, st), time.Hour)
	defer fresh.Close()

	stats, err := conn.HGetAll(ctx, fresh.Keys().Stats("example.com")).Result()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, raw := range stats {
		n, _ := strconv.Atoi(raw)
		total += n
	}
	if total != 3 {
		t.Errorf("a restarted crawler sees %d refusals, want 3", total)
	}
	if stats[string(ReasonRobotsDisallow)] != "3" {
		t.Errorf("robots_disallow = %q, want 3; %v", stats[string(ReasonRobotsDisallow)], stats)
	}
}

func TestRedisWindowRolloverResetsOnlyTheBudget(t *testing.T) {
	st, client, keys := newTestState(t)
	ctx := context.Background()

	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := st.SaveHostState(ctx, "h.example", HostState{
		Name:            "h.example",
		WindowStartedAt: start,
		FirstSeen:       start.Add(-72 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.HSet(ctx, keys.HostState("h.example"),
		fieldPagesCrawled, 5000,
		fieldLastSuccess, start.Add(-time.Hour).Unix(),
	).Err(); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := st.RecordFailure(ctx, "h.example"); err != nil {
			t.Fatal(err)
		}
	}

	resetAt := start.Add(90 * time.Minute)
	if err := st.ResetWindow(ctx, "h.example", resetAt); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "h.example")
	if err != nil {
		t.Fatal(err)
	}
	if got.PagesCrawled != 0 {
		t.Errorf("pages_crawled = %d after a rollover, want 0", got.PagesCrawled)
	}
	if !got.WindowStartedAt.Equal(resetAt) {
		t.Errorf("window_started_at = %v, want %v", got.WindowStartedAt, resetAt)
	}
	if got.ConsecFailures != 5 {
		t.Errorf("consec_failures = %d after a rollover, want 5; an hourly "+
			"budget must not reset the failure backoff", got.ConsecFailures)
	}
	if got.FirstSeen.IsZero() || got.LastSuccess.IsZero() {
		t.Errorf("a rollover cleared the host's history: %+v", got)
	}
}

func countingRobots(f RobotsFetcher) RobotsFetcher {
	return f
}

func prefixOf(t *testing.T, st *RedisState) string {
	t.Helper()
	return st.Keys().Prefix()
}
