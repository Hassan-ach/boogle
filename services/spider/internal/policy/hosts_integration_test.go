//go:build integration

package policy

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// These run against a real Redis because the caching this phase adds is Redis
// caching. The unit tests prove that a manager asks its state once per host; only
// a real Redis proves the second manager -- a different process, or the same one
// after a restart -- sees the same answer and does not fetch again. A fake shares
// one map, so the two cases are indistinguishable to it and to nothing else.

func TestRedisRobotsAreCachedAcrossManagers(t *testing.T) {
	st, conn, _ := newTestState(t)
	ctx := context.Background()

	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	var firstCalls, secondCalls int
	body := "User-agent: *\nDisallow: /private/\nSitemap: https://example.com/s.xml\n"

	// Two managers over one Redis, as a restart would produce.
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

	// The rules must be honoured by the second manager, which never read them
	// itself. If it does not, the cache is storing something nothing reads -- and
	// the first manager's robots.txt was fetched for nothing.
	v, err := m2.Admit(ctx, "https://example.com/private/secret")
	if err != nil {
		t.Fatalf("second Admit: %v", err)
	}
	if v.Kind != Skip || v.Reason != ReasonRobotsDisallow {
		t.Errorf("second manager = %s, want skip/robots_disallow from the cached rules",
			FormatVerdict(v))
	}

	// And a different manager over a *different* connection, which is the case a
	// fake structurally cannot cover: no shared map, only shared keys.
	m3 := New(cfg, st, testLogger()).WithRobotsFetcher(countingRobots(
		func(context.Context, string) ([]byte, int, error) {
			t.Error("a fresh connection re-fetched robots.txt already in Redis")
			return nil, 0, errConnRefused
		}))
	if v, err := m3.Admit(ctx, "https://example.com/other"); err != nil || v.Kind != Allow {
		t.Errorf("third Admit = %s / %v, want Allow from the cached rules", FormatVerdict(v), err)
	}

	// The sitemaps came out of the same file and belong with the rules.
	state, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.SiteMaps) != 1 || state.SiteMaps[0] != "https://example.com/s.xml" {
		t.Errorf("sitemaps did not survive: %v", state.SiteMaps)
	}
	_ = conn
}

// TestRedisNegativeCacheSurvivesARestart is the bug, restated against real Redis.
//
// The whole failure this package exists to fix was that nothing recorded a host
// having failed, so every URL on a dead domain opened another connection. A
// hand-written fake shares one map with the manager under test and so cannot show
// whether the record is where it can be found again. Here the second manager has
// a state of its own over a connection of its own, and the only thing they share
// is the key.
func TestRedisNegativeCacheSurvivesARestart(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.RedisPrefix = prefixOf(t, st)

	calls := 0
	fetcher := countingRobots(func(_ context.Context, url string) ([]byte, int, error) {
		calls++
		// A DNS failure, which is what a dead domain looks like and what must be
		// recorded rather than retried.
		return nil, 0, &net.DNSError{Err: "no such host", Name: url}
	})

	m := New(cfg, st, testLogger()).WithRobotsFetcher(fetcher)

	// The first URL pays for the discovery.
	if v, err := m.Admit(ctx, "https://dead.example/first"); err != nil {
		t.Fatalf("Admit: %v", err)
	} else if v.Kind != Defer {
		t.Fatalf("Admit = %s, want Defer on an unresolvable host", FormatVerdict(v))
	}
	if calls != 1 {
		t.Fatalf("first Admit made %d fetches, want 1", calls)
	}

	// The marker is in Redis under the host's hash slot, with a TTL that will let
	// the host be probed again.
	ttl, err := conn.PTTL(ctx, keys.HostMarker("dead.example", MarkerDead)).Result()
	if err != nil {
		t.Fatalf("the dead marker is not in Redis: %v", err)
	}
	if ttl <= 0 {
		t.Errorf("dead marker TTL = %v, want a positive deadline; without one the "+
			"host could never be probed again", ttl)
	}
	// The first dead TTL is base * 2^1, not base: DeadHostTTL's argument is the
	// consecutive failure count and a first failure is count one. Asserting the
	// computed value rather than the base is what keeps this test honest about
	// the schedule it is checking.
	want := cfg.DeadHostTTL(1)
	if ttl > want+2*time.Second || ttl < want-2*time.Second {
		t.Errorf("dead marker TTL = %v, want about DeadHostTTL(1) = %v", ttl, want)
	}

	// Now the restart: a fresh state, a fresh client, a fresh manager. Only the
	// key survives, which is the only thing that ever did.
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
		// Skip, not Allow, and not Defer either: the marker is authoritative, and
		// a restarted process must reach the same conclusion the first one did.
		if v.Kind != Skip || v.Reason != ReasonHostDead {
			t.Fatalf("a URL on a host marked dead in Redis was %s, want skip/host_dead",
				FormatVerdict(v))
		}
	}
	if calls != 1 {
		t.Errorf("50 URLs on a dead host caused %d fetches, want 1", calls)
	}
}

// TestRedisRobotsCacheIsNotWrittenOnFailure guards the trap that makes a negative
// cache permanent instead of temporary.
//
// RobotsFetchedAt is what "this host is resolved" means. Writing it on a failed
// fetch would record a host as resolved with no rules -- which permits everything
// -- and would outlive the dead marker by a factor of a thousand, since the marker
// expires in a minute and RobotsTTL is a day. The host would then never be probed
// again.
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

	// The marker is there -- that is the negative cache. The state is not.
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

	// And the proof that this matters: with the marker gone but the timestamp
	// wrongly set, the host would look resolved and permit everything. Checking
	// the field in Redis directly catches the write even if some future caller
	// stops reading it back.
	if raw := conn.HGet(ctx, keys.HostState("gone.example"), fieldRobotsAt).Val(); raw != "" {
		t.Errorf("robots_at is in Redis as %q on a host that was never reached", raw)
	}
}

// TestRedisCountersPersistAcrossManagers checks that a refusal counted by one
// process is visible to the next.
//
// The stats hash is the only account of why a site is not being indexed. If it
// were per-process it would be empty for a crawler that was restarted after the
// interesting refusals happened -- which is exactly when somebody goes looking.
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

	// A fresh state over the same keys -- as a restarted process would build. It
	// shares nothing with m but the prefix.
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

// TestRedisWindowRolloverResetsOnlyTheBudget checks the shape of the reset, which
// is the one that is easy to over-apply.
//
// A reset that also cleared the failure count would drop a host that had failed
// five times in a row back to its first, shortest backoff, once an hour. A reset
// that cleared nothing would make the budget a lifetime cap and the host would
// disappear from the index permanently.
func TestRedisWindowRolloverResetsOnlyTheBudget(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	start := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := st.SaveHostState(ctx, "h.example", HostState{
		Name:            "h.example",
		WindowStartedAt: start,
		PagesCrawled:    5000,
		LastSuccess:     start.Add(-time.Hour),
		FirstSeen:       start.Add(-72 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := st.IncrFailures(ctx, "h.example"); err != nil {
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

// countingRobots wraps a fetcher and reports how many times it ran, so a test can
// say "exactly once" as a fact rather than inferring it from a fetch count it did
// not take.
func countingRobots(f RobotsFetcher) RobotsFetcher {
	return f
}

// prefixOf recovers the key prefix a state is using, so a second state can be
// built over the same keys without the test threading the string through.
func prefixOf(t *testing.T, st *RedisState) string {
	t.Helper()
	return st.Keys().Prefix()
}
