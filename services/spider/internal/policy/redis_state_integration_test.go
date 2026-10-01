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

const redisTestDB = 15

var prefixCounter atomic.Int64

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
		t.Skipf("redis unavailable at %s:%s (start it with `just infra-up`): %v", addr, port, err)
	}

	prefix := fmt.Sprintf("boogle:spider:test:%d:%d", time.Now().UnixNano(), prefixCounter.Add(1))
	state := NewRedisState(conn, prefix, time.Hour)
	keys := state.Keys()

	t.Cleanup(func() {
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

func TestRedisStateMarkersCostOneRoundTrip(t *testing.T) {
	_, conn, _ := newTestState(t)
	ctx := context.Background()

	hook := &countingHook{counts: map[string]int{}}
	conn.AddHook(hook)

	st := NewRedisState(conn, "boogle:spider:roundtrip:"+strconv.FormatInt(time.Now().UnixNano(), 10), time.Hour)
	if _, err := st.Markers(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}

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
		Status:          "degraded",
		WindowStartedAt: first,
		RobotsFetchedAt: first.Add(time.Hour),
		FirstSeen:       first.Add(-24 * time.Hour),
		Allow:           []string{"/wiki/", "/docs"},
		Disallow:        []string{"/private/", "/admin/"},
		SiteMaps:        []string{"https://example.com/sitemap.xml", "https://example.com/news.xml"},
	}
	if err := st.SaveHostState(ctx, "example.com", saved); err != nil {
		t.Fatal(err)
	}

	for range 42 {
		if err := st.RecordSuccess(ctx, "example.com", first.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := st.RecordFailure(ctx, "example.com"); err != nil {
			t.Fatal(err)
		}
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
	if strings.Join(got.Allow, ",") != "/wiki/,/docs" {
		t.Errorf("Allow = %v, want [/wiki/ /docs]", got.Allow)
	}
	if strings.Join(got.Disallow, ",") != "/private/,/admin/" {
		t.Errorf("Disallow = %v, want [/private/ /admin/]", got.Disallow)
	}
	if strings.Join(got.SiteMaps, ",") != "https://example.com/sitemap.xml,https://example.com/news.xml" {
		t.Errorf("SiteMaps = %v, want both sitemaps in order", got.SiteMaps)
	}
}

func TestRedisStateHostStateOmitsUnsetTimestamps(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	real := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.SaveHostState(ctx, "example.com", HostState{
		Name: "example.com", WindowStartedAt: real, RobotsFetchedAt: real,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveHostState(ctx, "example.com", HostState{Name: "example.com", MaxPages: 10}); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.WindowStartedAt.Equal(real) {
		t.Errorf("WindowStartedAt = %v, want the earlier %v preserved", got.WindowStartedAt, real)
	}
	if !got.RobotsFetchedAt.Equal(real) {
		t.Errorf("RobotsFetchedAt = %v, want the earlier %v preserved", got.RobotsFetchedAt, real)
	}
	if got.MaxPages != 10 {
		t.Errorf("MaxPages = %d, want the update to land", got.MaxPages)
	}
}

func TestRedisStateUnknownHostIsNotAnError(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

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
		got, err := st.RecordFailure(ctx, "example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got != i {
			t.Errorf("RecordFailure = %d, want %d", got, i)
		}
	}
	state, _ := st.HostState(ctx, "example.com")
	if state.ConsecFailures != 3 {
		t.Errorf("ConsecFailures = %d, want 3", state.ConsecFailures)
	}
	if state.Status != statusDegraded {
		t.Errorf("Status = %q after three failures, want %q", state.Status, statusDegraded)
	}

	at := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	if err := st.RecordSuccess(ctx, "example.com", at); err != nil {
		t.Fatal(err)
	}
	state, _ = st.HostState(ctx, "example.com")
	if state.ConsecFailures != 0 {
		t.Errorf("ConsecFailures = %d after a success, want 0", state.ConsecFailures)
	}
	if state.PagesCrawled != 1 {
		t.Errorf("PagesCrawled = %d, want 1", state.PagesCrawled)
	}
	if !state.LastSuccess.Equal(at) {
		t.Errorf("LastSuccess = %v, want %v", state.LastSuccess, at)
	}
	if state.Status != statusReady {
		t.Errorf("Status = %q after a success, want %q", state.Status, statusReady)
	}

	if err := st.ResetWindow(ctx, "example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	state, _ = st.HostState(ctx, "example.com")
	if state.PagesCrawled != 0 {
		t.Errorf("PagesCrawled = %d after window reset, want 0", state.PagesCrawled)
	}
}

func TestRedisStateSaveDoesNotTouchCounters(t *testing.T) {
	st, client, keys := newTestState(t)
	ctx := context.Background()

	for range 2 {
		if err := st.RecordSuccess(ctx, "example.com", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordFailure(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}

	stale, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	stale.PagesCrawled = 0
	stale.ConsecFailures = 0
	stale.LastSuccess = time.Time{}
	if err := st.SaveHostState(ctx, "example.com", stale); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.PagesCrawled != 2 {
		t.Errorf("PagesCrawled = %d after a save of a stale record, want 2", got.PagesCrawled)
	}
	if got.ConsecFailures != 1 {
		t.Errorf("ConsecFailures = %d after a save of a stale record, want 1", got.ConsecFailures)
	}
	if got.LastSuccess.IsZero() {
		t.Error("LastSuccess was cleared by a save of a stale record")
	}

	if err := st.SaveHostState(ctx, "example.com", HostState{
		Name: "example.com", MaxPages: 42, CrawlDelay: 3 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := client.HGetAll(ctx, keys.HostState("example.com")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if raw[fieldMaxPages] != "42" {
		t.Errorf("max_pages = %q after a save, want 42", raw[fieldMaxPages])
	}
	if raw[fieldCrawlDelay] != "3000" {
		t.Errorf("crawl_delay_ms = %q after a save, want 3000", raw[fieldCrawlDelay])
	}
}

func TestRedisClaimSiteMapsHandsOutOneClaim(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	readAt := time.Now().UTC().Truncate(time.Second)

	const workers = 8
	var won int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := st.ClaimSiteMaps(ctx, "example.com", readAt)
			if err != nil {
				t.Errorf("ClaimSiteMaps: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&won); got != 1 {
		t.Errorf("%d of %d workers won the sitemap claim, want exactly 1", got, workers)
	}
}

func TestRedisClaimSiteMapsDecidesByReading(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	won, err := st.ClaimSiteMaps(ctx, "example.com", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the first claim was refused")
	}

	won, err = st.ClaimSiteMaps(ctx, "example.com", base)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a stale robots.txt reading claimed the sitemaps again")
	}

	won, err = st.ClaimSiteMaps(ctx, "example.com", base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Error("a newer robots.txt reading was refused, so a site that publishes a " +
			"page and updates its sitemap would never be found again")
	}

	won, err = st.ClaimSiteMaps(ctx, "example.com", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a claim with no robots.txt reading behind it was granted")
	}
	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SiteMapsClaimedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("SiteMapsClaimedAt = %v after a claim with no reading, want the "+
			"earlier claim %v to be untouched", got.SiteMapsClaimedAt, base.Add(2*time.Hour))
	}
}

func TestRedisSaveHostStateDoesNotEraseTheSitemapClaim(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	readAt := time.Now().UTC().Truncate(time.Second)

	stale, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}

	won, err := st.ClaimSiteMaps(ctx, "example.com", readAt)
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the first claim was refused")
	}

	stale = stale.WithRobots("example.com", []string{"/"}, nil,
		[]string{"https://example.com/s.xml"}, 0, readAt)
	if err := st.SaveHostState(ctx, "example.com", stale); err != nil {
		t.Fatal(err)
	}

	got, err := st.HostState(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SiteMapsClaimedAt.Equal(readAt) {
		t.Errorf("SiteMapsClaimedAt = %v, want %v: saving the host record erased a claim "+
			"another worker was holding, and the same robots.txt would be expanded again",
			got.SiteMapsClaimedAt, readAt)
	}

	won, err = st.ClaimSiteMaps(ctx, "example.com", readAt)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("the claim was open again after an unrelated host write")
	}
}

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

func TestRedisStateSetMarkerDoesNotExtend(t *testing.T) {
	st, _, _ := newTestState(t)
	ctx := context.Background()

	if err := st.SetMarker(ctx, "example.com", MarkerDead, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
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
	if state.URL != url {
		t.Errorf("URL = %q, want %q", state.URL, url)
	}

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

	if a.Keys().Frontier() == b.Keys().Frontier() {
		t.Errorf("both prefixes produced the key %q", a.Keys().Frontier())
	}
}
