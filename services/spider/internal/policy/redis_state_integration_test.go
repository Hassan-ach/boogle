//go:build integration

package policy

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// These run against a real Redis because the things most worth testing here are
// exactly the things a fake would have to lie about: TTLs actually expiring,
// SET NX actually not extending, PTTL's two distinct negative answers, and
// pipelines genuinely batching. A hand-written fake proves the fake.
//
// Each test gets its own key prefix and its own logical database, so a run never
// touches another test's keys and never touches the spider's working set.

// redisTestDB is the logical database reserved for integration tests. The
// spider's own configuration defaults to DB 1, so 15 is out of its way.
const redisTestDB = 15

var prefixCounter atomic.Int64

// newTestState returns a state over a unique prefix, and registers cleanup that
// removes only that prefix's keys.
func newTestState(t *testing.T) (*RedisState, *redis.Client, Keyspace) {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost"
	}
	port := os.Getenv("REDIS_PORT")
	if port == "" {
		port = "6379"
	}

	conn := redis.NewClient(&redis.Options{
		Addr:     addr + ":" + port,
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       redisTestDB,
	})
	if err := conn.Ping(context.Background()).Err(); err != nil {
		// Skipping rather than failing: `just test-integration` runs in
		// environments where the compose stack may not be up, and a hard failure
		// here would look like a code defect.
		t.Skipf("redis unavailable at %s:%s (start it with `just infra-up`): %v", addr, port, err)
	}

	prefix := fmt.Sprintf("boogle:spider:test:%d:%d", time.Now().UnixNano(), prefixCounter.Add(1))
	state := NewRedisState(conn, prefix, time.Hour)
	keys := state.Keys()

	t.Cleanup(func() {
		// Scan rather than KEYS: KEYS blocks the server, and a test suite that
		// blocks the Redis it is testing against is its own outage.
		ctx := context.Background()
		var cursor uint64
		for {
			batch, next, err := conn.Scan(ctx, cursor, "{"+prefix+"}*", 500).Result()
			if err != nil {
				break
			}
			if len(batch) > 0 {
				conn.Del(ctx, batch...)
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		conn.Close()
	})

	return state, conn, keys
}

func TestRedisStateFrontier(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	if err := st.Enqueue(ctx, "https://example.com/a", "https://example.com/b", ""); err != nil {
		t.Fatal(err)
	}
	n, err := st.FrontierLen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("FrontierLen = %d, want 2 (the empty string must be dropped)", n)
	}

	// A second enqueue increments, so priority is an inlink count.
	if err := st.Enqueue(ctx, "https://example.com/a"); err != nil {
		t.Fatal(err)
	}
	score, err := conn.ZScore(ctx, keys.Frontier(), "https://example.com/a").Result()
	if err != nil {
		t.Fatal(err)
	}
	if score != 2 {
		t.Errorf("priority = %v, want 2 after two enqueues", score)
	}

	// EnqueueAt sets an exact priority instead.
	if err := st.EnqueueAt(ctx, "https://example.com/b", 42); err != nil {
		t.Fatal(err)
	}
	score, _ = conn.ZScore(ctx, keys.Frontier(), "https://example.com/b").Result()
	if score != 42 {
		t.Errorf("priority = %v, want 42 from EnqueueAt", score)
	}
}

func TestRedisStateDelayedIsScoredByDueTime(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	due := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := st.EnqueueDelayed(ctx, "https://example.com/retry", due); err != nil {
		t.Fatal(err)
	}

	score, err := conn.ZScore(ctx, keys.Delayed(), "https://example.com/retry").Result()
	if err != nil {
		t.Fatal(err)
	}
	if int64(score) != due.Unix() {
		t.Errorf("score = %v, want the due unix time %d", score, due.Unix())
	}

	// And it is findable by score, which is the whole mechanism: one
	// ZRANGEBYSCORE finds everything that has come due, with no timer and no
	// per-URL bookkeeping.
	now := strconv.FormatInt(time.Now().Unix(), 10)
	dueEntries, err := conn.ZRangeByScore(ctx, keys.Delayed(), &redis.ZRangeBy{
		Min: "-inf", Max: now,
	}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(dueEntries) != 0 {
		t.Errorf("a url due in an hour came back as due now: %v", dueEntries)
	}
}

func TestRedisStateVisited(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	const a, b = "https://example.com/a", "https://example.com/b"

	if err := st.MarkVisited(ctx, a, b, ""); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{a, b} {
		got, err := st.IsVisited(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if !got {
			t.Errorf("IsVisited(%q) = false, want true", u)
		}
	}
	if got, _ := st.IsVisited(ctx, "https://example.com/other"); got {
		t.Error("an unvisited url reported visited")
	}

	// Checked against the set directly rather than through IsVisited, which
	// short-circuits on an empty URL and would hide the very thing being tested.
	// An empty member would match a URL that failed to parse, which is exactly
	// the case the visited set is meant to exclude.
	n, err := conn.SCard(ctx, keys.Visited()).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("visited set holds %d members, want 2 -- the empty string must be dropped", n)
	}
	member, err := conn.SIsMember(ctx, keys.Visited(), "").Result()
	if err != nil {
		t.Fatal(err)
	}
	if member {
		t.Error("the empty string is a member of the visited set; a url that failed to " +
			"parse would be treated as already crawled")
	}
}

// countingHook records how many commands of each kind a client issued.
type countingHook struct {
	mu     sync.Mutex
	counts map[string]int
}

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.record(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		// One batch = one round trip. The individual commands are counted here
		// rather than in ProcessHook, because a pipelined command never reaches
		// ProcessHook at all.
		h.record("multi")
		for _, cmd := range cmds {
			h.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (h *countingHook) record(name string) {
	h.mu.Lock()
	h.counts[name]++
	h.mu.Unlock()
}

func (h *countingHook) count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[name]
}

// TestRedisStateMarkersCostOneRoundTrip is a performance property stated as a
// test because it is the property the design is built around. Markers runs
// before every single fetch, so reading the three of them one at a time would
// triple the round trips on the hottest path in the crawler. No assertion on
// values can catch it, because the values are identical either way -- what
// differs is how many batches they arrive in.
func TestRedisStateMarkersCostOneRoundTrip(t *testing.T) {
	_, conn, _ := newTestState(t)
	ctx := context.Background()

	hook := &countingHook{counts: map[string]int{}}
	conn.AddHook(hook)

	st := NewRedisState(conn, "boogle:spider:roundtrip:"+strconv.FormatInt(time.Now().UnixNano(), 10), time.Hour)
	if _, err := st.Markers(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}

	// All three reads have to happen, and they have to happen as one batch. A
	// pipelined call is a single MULTI/EXEC, so the pipeline count is the
	// round-trip count.
	if got := hook.count("pttl"); got != len(AllMarkers) {
		t.Errorf("issued %d PTTL commands, want %d -- a marker would be missed", got, len(AllMarkers))
	}
	if got := hook.count("multi"); got != 1 {
		t.Errorf("issued %d batches, want 1: three round trips per fetch on the hottest "+
			"path in the crawler", got)
	}
}

func TestRedisStateHostStateRoundTrips(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	first := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	saved := HostState{
		Name:            "example.com",
		CrawlDelay:      2500 * time.Millisecond,
		MaxPages:        500,
		PagesCrawled:    42,
		ConsecFailures:  3,
		Status:          "degraded",
		WindowStartedAt: first,
		RobotsFetchedAt: first.Add(time.Hour),
		FirstSeen:       first.Add(-24 * time.Hour),
		LastSuccess:     first.Add(2 * time.Hour),
		Allow:           []string{"/wiki/", "/docs"},
		Disallow:        []string{"/private/", "/admin/"},
		SiteMaps:        []string{"https://example.com/sitemap.xml", "https://example.com/news.xml"},
	}
	if err := st.SaveHostState(ctx, "example.com", saved); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}

	if got.Name != "example.com" || got.MaxPages != 500 || got.PagesCrawled != 42 {
		t.Errorf("counters did not round-trip: %+v", got)
	}
	if got.ConsecFailures != 3 {
		t.Errorf("ConsecFailures = %d, want 3", got.ConsecFailures)
	}
	if got.CrawlDelay != 2500*time.Millisecond {
		t.Errorf("CrawlDelay = %v, want 2.5s", got.CrawlDelay)
	}
	if got.Status != "degraded" {
		t.Errorf("Status = %q, want %q", got.Status, "degraded")
	}
	if !got.WindowStartedAt.Equal(first) || !got.LastSuccess.Equal(first.Add(2*time.Hour)) {
		t.Errorf("timestamps did not round-trip: window=%v lastSuccess=%v", got.WindowStartedAt, got.LastSuccess)
	}
	// The robots rules are the ones the old code parsed and threw away, so they
	// have to survive a round trip intact.
	if strings.Join(got.Allow, ",") != "/wiki/,/docs" {
		t.Errorf("Allow = %v, want [/wiki/ /docs]", got.Allow)
	}
	if strings.Join(got.Disallow, ",") != "/private/,/admin/" {
		t.Errorf("Disallow = %v, want [/private/ /admin/]", got.Disallow)
	}
	// Sitemaps are collected rather than obeyed, because nothing consumes them
	// yet -- but collecting them at all was the point of reading the file, and a
	// round trip that drops them throws away the only copy.
	if strings.Join(got.SiteMaps, ",") != "https://example.com/sitemap.xml,https://example.com/news.xml" {
		t.Errorf("SiteMaps = %v, want both sitemaps in order", got.SiteMaps)
	}
}

// TestRedisStateHostStateOmitsUnsetTimestamps guards against writing the epoch
// over a real timestamp. A HostState built for one purpose can easily have an
// empty LastSuccess, and saving it must not wipe a real one.
func TestRedisStateHostStateOmitsUnsetTimestamps(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	real := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.SaveHostState(ctx, "example.com", HostState{Name: "example.com", LastSuccess: real}); err != nil {
		t.Fatal(err)
	}
	// Save again without the timestamp set.
	if err := st.SaveHostState(ctx, "example.com", HostState{Name: "example.com", MaxPages: 10}); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSuccess.Equal(real) {
		t.Errorf("LastSuccess = %v, want the earlier %v preserved", got.LastSuccess, real)
	}
	if got.MaxPages != 10 {
		t.Errorf("MaxPages = %d, want the update to land", got.MaxPages)
	}
}

func TestRedisStateUnknownHostIsNotAnError(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	// Admit resolves an unknown host by fetching robots.txt. An error here would
	// make every first visit look like a Redis outage.
	got, err := st.HostState(ctx, "never-seen.example")
	if err != nil {
		t.Fatalf("an unknown host returned an error: %v", err)
	}
	if got.Name != "never-seen.example" {
		t.Errorf("Name = %q, want the host echoed back", got.Name)
	}
	if got.PagesCrawled != 0 || got.ConsecFailures != 0 {
		t.Errorf("unknown host came back populated: %+v", got)
	}
}

func TestRedisStateCounters(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		got, err := st.IncrFailures(ctx, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got != i {
			t.Errorf("IncrFailures = %d, want %d", got, i)
		}
	}
	if err := st.ResetFailures(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if st, _ := st.HostState(ctx, "example.com"); st.ConsecFailures != 0 {
		t.Errorf("ConsecFailures = %d after reset, want 0", st.ConsecFailures)
	}

	pages, err := st.IncrPagesCrawled(ctx, "example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Errorf("IncrPagesCrawled = %d, want 1", pages)
	}
	if err := st.ResetWindow(ctx, "example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	state, _ := st.HostState(ctx, "example.com")
	if state.PagesCrawled != 0 {
		t.Errorf("PagesCrawled = %d after window reset, want 0", state.PagesCrawled)
	}
}

// TestRedisStateMarkersExpire is the property the whole skip-a-dead-domain
// design rests on. If a marker did not actually expire, a host marked dead once
// would be skipped for the rest of the crawl and never probed again.
func TestRedisStateMarkersExpire(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	for _, kind := range AllMarkers {
		if err := st.SetMarker(ctx, "example.com", kind, 150*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		markers, err := st.Markers(ctx, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := markers[kind]; !ok {
			t.Fatalf("%s marker was not set", kind)
		}
	}

	// All three are read in one call, which is what keeps a dead host costing
	// one round trip rather than three.
	markers, err := st.Markers(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != len(AllMarkers) {
		t.Errorf("read %d markers, want %d: %v", len(markers), len(AllMarkers), markers)
	}
	for kind, ttl := range markers {
		if ttl <= 0 || ttl > 200*time.Millisecond {
			t.Errorf("%s ttl = %v, want a positive value near 150ms", kind, ttl)
		}
	}

	time.Sleep(250 * time.Millisecond)

	markers, err = st.Markers(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Errorf("markers survived their TTL: %v -- a dead host would never be probed again", markers)
	}
}

// TestRedisStateSetMarkerDoesNotExtend is the concurrency guard. Twenty workers
// can decide a host is dead in the same second, and with a plain SET each would
// push the expiry out to its own "now plus ttl".
func TestRedisStateSetMarkerDoesNotExtend(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerDead, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// A later worker with a higher failure count asking for a much longer TTL.
	if err := st.SetMarker(ctx, "example.com", MarkerDead, time.Hour); err != nil {
		t.Fatal(err)
	}

	markers, err := st.Markers(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	ttl := markers[MarkerDead]
	if ttl > 2*time.Second {
		t.Errorf("marker ttl = %v, want about 1.7s -- SetMarker must not extend an "+
			"existing deadline", ttl)
	}
}

func TestRedisStateClearMarker(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerCooldown, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearMarker(ctx, "example.com", MarkerCooldown); err != nil {
		t.Fatal(err)
	}
	exists, err := conn.Exists(ctx, keys.HostMarker("example.com", MarkerCooldown)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Error("the marker key still exists after ClearMarker")
	}
}

func TestRedisStateURLAttempts(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()
	const url = "https://example.com/flaky"

	// No record is normal, not an error.
	zero, err := st.URLState(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if zero.Attempts != 0 {
		t.Errorf("Attempts = %d for an unseen url, want 0", zero.Attempts)
	}

	for want := 1; want <= 3; want++ {
		got, err := st.BumpAttempts(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("BumpAttempts = %d, want %d", got, want)
		}
	}

	state, err := st.URLState(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if state.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", state.Attempts)
	}
	// The URL is stored alongside the hash so a key found in a keyspace scan can
	// be traced back to its member.
	if state.URL != url {
		t.Errorf("URL = %q, want %q", state.URL, url)
	}

	// The bookkeeping expires on its own, or a long crawl accumulates one hash
	// per URL it ever touched.
	ttl, err := conn.TTL(ctx, keys.URLState(url)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("url state ttl = %v, want a positive value up to the configured hour", ttl)
	}

	if err := st.ClearURLState(ctx, url); err != nil {
		t.Fatal(err)
	}
	exists, _ := conn.Exists(ctx, keys.URLState(url)).Result()
	if exists != 0 {
		t.Error("url state survived ClearURLState")
	}
}

func TestRedisStateCountReason(t *testing.T) {
	st, conn, keys := newTestState(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := st.CountReason(ctx, "example.com", ReasonPathDisallowed); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CountReason(ctx, "example.com", ReasonExtensionSkipped); err != nil {
		t.Fatal(err)
	}
	if err := st.CountReason(ctx, "example.com", ReasonExtensionSkipped); err != nil {
		t.Fatal(err)
	}

	disallowed, err := conn.HGet(ctx, keys.Stats("example.com"), string(ReasonPathDisallowed)).Int()
	if err != nil {
		t.Fatal(err)
	}
	if disallowed != 5 {
		t.Errorf("path_disallowed = %d, want 5", disallowed)
	}
	ext, _ := conn.HGet(ctx, keys.Stats("example.com"), string(ReasonExtensionSkipped)).Int()
	if ext != 2 {
		t.Errorf("extension_skipped = %d, want 2", ext)
	}
}

// TestRedisStatePrefixesAreIsolated is why the prefix is configurable at all.
// Two spiders sharing one Redis -- or a spider and its test run -- must not see
// each other's keys. Getting this wrong would let one crawl's visited set hide
// another's frontier, and the second spider would sit idle forever.
func TestRedisStatePrefixesAreIsolated(t *testing.T) {
	_, conn, _ := newTestState(t)
	ctx := context.Background()

	newState := func(prefix string) *RedisState {
		return NewRedisState(conn, prefix, time.Hour)
	}
	a := newState("boogle:spider:iso:a:" + strconv.FormatInt(time.Now().UnixNano(), 10))
	b := newState("boogle:spider:iso:b:" + strconv.FormatInt(time.Now().UnixNano(), 10))

	const url = "https://example.com/shared-path"
	if err := a.MarkVisited(ctx, url); err != nil {
		t.Fatal(err)
	}
	if err := a.Enqueue(ctx, "https://example.com/only-in-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetMarker(ctx, "example.com", MarkerDead, time.Hour); err != nil {
		t.Fatal(err)
	}

	// b shares the same host and the same URL, and must see none of it.
	visited, err := b.IsVisited(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("one prefix's visited set is visible to another; a shared URL would be " +
			"silently skipped by the second spider")
	}
	if n, err := b.FrontierLen(ctx); err != nil || n != 0 {
		t.Errorf("one prefix's frontier is visible to another: len=%d err=%v", n, err)
	}
	markers, err := b.Markers(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Errorf("one prefix's markers are visible to another: %v", markers)
	}

	// And the key names genuinely differ, not just the data.
	if a.Keys().Frontier() == b.Keys().Frontier() {
		t.Errorf("both prefixes produced the key %q", a.Keys().Frontier())
	}
}
