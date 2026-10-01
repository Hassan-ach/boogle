package policy

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAdmitEnforcesThePageBudget(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)
	ctx := context.Background()

	fetch := &fakeRobots{reply: map[string]robotsResponse{"big.example": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	const budget = 3
	cfg := m.Config()
	cfg.MaxPagesPerHost = budget
	m = m.WithConfig(cfg)

	if v := mustAdmit(t, m, uniqueURL("big.example", "/a")); v.Kind != Allow {
		t.Fatalf("first Admit = %s, want Allow", FormatVerdict(v))
	}
	spendPages(t, st, "big.example", budget)

	v := mustAdmit(t, m, uniqueURL("big.example", "/b"))
	assertVerdict(t, v, Skip, ReasonHostBudgetExhausted)
	if v.Until.IsZero() {
		t.Error("an exhausted host refused the URL with no Until; nothing could schedule its return")
	}

	fetch.forbidCalls(t)
	if got := mustAdmit(t, m, uniqueURL("big.example", "/c")).Reason; got != ReasonHostBudgetExhausted {
		if got != ReasonHostCold {
			t.Errorf("reason = %q, want %q", got, ReasonHostCold)
		}
	}
	if n := fetch.callCount("big.example"); n != 1 {
		t.Errorf("a cold host was re-fetched: %d robots fetches, want 1", n)
	}

	clock.Advance(m.cfg.HostColdPeriod + time.Second)
	if got := mustAdmit(t, m, uniqueURL("big.example", "/d")).Kind; got != Allow {
		t.Fatalf("after the cold period Admit = %v, want Allow", got)
	}

	st2, err := st.HostState(ctx, "big.example")
	if err != nil {
		t.Fatalf("read host state: %v", err)
	}
	if st2.PagesCrawled != 0 {
		t.Errorf("pages_crawled = %d after a window rolled over, want 0", st2.PagesCrawled)
	}
	if !st2.WindowStartedAt.Equal(clock.Now()) {
		t.Errorf("window_started_at = %v, want %v", st2.WindowStartedAt, clock.Now())
	}
}

func TestAdmitBudgetCountsPerWindowNotForEver(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)
	ctx := context.Background()

	fetch := &fakeRobots{reply: map[string]robotsResponse{"big.example": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	cfg := m.Config()
	cfg.MaxPagesPerHost = 1
	m = m.WithConfig(cfg)

	if v := mustAdmit(t, m, uniqueURL("big.example", "/a")); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	spendPages(t, st, "big.example", 1)
	windowStarted := clock.Now()

	clock.Advance(m.cfg.HostColdPeriod - time.Second)
	st.ClearMarker(ctx, "big.example", MarkerCold)
	if kind, reason := admitReason(t, m, uniqueURL("big.example", "/b")); kind == Allow {
		t.Errorf("Admit inside the window = %v/%s, want a refusal", kind, reason)
	}

	_ = windowStarted
	clock.Advance(2 * time.Second)
	st.ClearMarker(ctx, "big.example", MarkerCold)
	if v := mustAdmit(t, m, uniqueURL("big.example", "/c")); v.Kind != Allow {
		t.Errorf("Admit after the window = %s, want Allow", FormatVerdict(v))
	}
}

func TestAdmitZeroColdPeriodDoesNotResetEveryURL(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)

	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"big.example": {body: ""}},
		status: 200,
	}).fetcher())

	cfg := m.Config()
	cfg.MaxPagesPerHost = 2
	cfg.HostColdPeriod = 0
	m = m.WithConfig(cfg)

	if v := mustAdmit(t, m, uniqueURL("big.example", "/a")); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	spendPages(t, st, "big.example", 2)

	for range 5 {
		clock.Advance(time.Millisecond)
		st.ClearMarker(context.Background(), "big.example", MarkerCold)
		if v := mustAdmit(t, m, uniqueURL("big.example", "/b")); v.Kind == Allow {
			t.Fatal("a zero cold period reset the budget on every URL, making the budget unenforceable")
		}
	}
}

func TestAdmitResetsFailClosed(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)

	m = m.WithRobotsFetcher((&fakeRobots{
		reply:  map[string]robotsResponse{"big.example": {body: ""}},
		status: 200,
	}).fetcher())

	cfg := m.Config()
	cfg.MaxPagesPerHost = 1
	m = m.WithConfig(cfg)

	if v := mustAdmit(t, m, uniqueURL("big.example", "/a")); v.Kind != Allow {
		t.Fatalf("Admit = %s, want Allow", FormatVerdict(v))
	}
	spendPages(t, st, "big.example", 1)

	clock.Advance(m.cfg.HostColdPeriod + time.Second)
	st.FailOn = map[string]error{"ResetWindow": errRedisDown}

	v, err := m.Admit(context.Background(), uniqueURL("big.example", "/b"))
	if err != nil {
		t.Fatalf("a failed window reset produced an error rather than a verdict: %v", err)
	}
	assertVerdict(t, v, Defer, ReasonPolicyUnavailable)
	want := now.Add(m.cfg.HostColdPeriod + time.Second).Add(m.cfg.URLBackoff(1))
	if !v.Until.Equal(want) {
		t.Errorf("Until = %v, want %v -- one URLBackoff interval from now", v.Until, want)
	}
}

func TestAdmitUnresolvedHostDefersRatherThanSkips(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"slow.example": {err: &net.DNSError{Err: "no such host", Name: "slow.example"}}},
	}).fetcher())
	ctx := context.Background()

	const target = "https://slow.example/a"
	kind, _ := admitReason(t, m, target)
	if kind != Defer {
		t.Fatalf("kind = %v, want Defer", kind)
	}
	if visited, err := st.IsVisited(ctx, target); err != nil {
		t.Fatalf("is visited: %v", err)
	} else if visited {
		t.Error("a URL on an unresolvable host was marked visited; the host is lost from the index")
	}

	if n := st.Stats("slow.example")[ReasonHostMetadataUnknown]; n != 1 {
		t.Errorf("stats[slow.example][host_metadata_unavailable] = %d, want 1", n)
	}
}

func TestAdmitStopsReachingTheNetworkForAnUnreachableHost(t *testing.T) {
	m, _ := newTestManager(t)
	fetch := &fakeRobots{
		reply: map[string]robotsResponse{
			"gone.example": {err: &net.DNSError{Err: "no such host", Name: "gone.example"}},
		},
	}
	m = m.WithRobotsFetcher(fetch.fetcher())

	if kind, _ := admitReason(t, m, "https://gone.example/first"); kind != Defer {
		t.Fatalf("first Admit = %v, want Defer", kind)
	}
	if n := fetch.callCount("gone.example"); n != 1 {
		t.Fatalf("first Admit made %d fetches, want 1", n)
	}

	fetch.forbidCalls(t)
	for range 30 {
		if kind, _ := admitReason(t, m, uniqueURL("gone.example", "/page")); kind == Allow {
			t.Fatal("a URL on a host we know is unreachable was allowed")
		}
	}
	if n := fetch.callCount("gone.example"); n != 1 {
		t.Errorf("30 URLs on a dead host caused %d fetches, want 1", n)
	}
}

func TestAdmitRetriesAnUnreachableHostAfterTheMarkerExpires(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, _, clock := atClock(t, now)

	fetch := &fakeRobots{
		reply: map[string]robotsResponse{
			"flaky.example": {err: &net.DNSError{Err: "no such host", Name: "flaky.example"}},
		},
	}
	m = m.WithRobotsFetcher(fetch.fetcher())

	if kind, _ := admitReason(t, m, "https://flaky.example/first"); kind != Defer {
		t.Fatalf("first Admit = %v, want Defer", kind)
	}
	before := fetch.callCount("flaky.example")

	clock.Advance(time.Second)
	admitReason(t, m, uniqueURL("flaky.example", "/x"))
	if n := fetch.callCount("flaky.example"); n != before {
		t.Errorf("a fetch happened inside the dead window: %d, want %d", n, before)
	}

	clock.Advance(m.cfg.DeadHostTTL(1) + time.Second)
	admitReason(t, m, uniqueURL("flaky.example", "/y"))
	if n := fetch.callCount("flaky.example"); n != before+1 {
		t.Errorf("the host was never probed again: %d fetches, want %d", n, before+1)
	}
}

func TestEnsureHostTreatsATimeoutAsSlowNotGone(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		marker  MarkerKind
		notMark MarkerKind
	}{
		{"timeout", &timeoutError{}, MarkerCooldown, MarkerDead},
		{"dns failure", &net.DNSError{Err: "no such host"}, MarkerDead, MarkerCooldown},
		{"connection refused", errConnRefused, MarkerDead, MarkerCooldown},
		{"tls failure", errTLSHandshake, MarkerDead, MarkerCooldown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{
				reply: map[string]robotsResponse{"h.example": {err: tc.err}},
			}).fetcher())
			ctx := context.Background()

			if _, err := m.EnsureHost(ctx, "h.example"); err == nil {
				t.Fatal("EnsureHost succeeded on an unreachable host")
			}

			markers, err := st.Markers(ctx, "h.example")
			if err != nil {
				t.Fatalf("markers: %v", err)
			}
			if _, ok := markers[tc.marker]; !ok {
				t.Errorf("marker %v was not set; got %v", tc.marker, markerNames(markers))
			}
			if _, ok := markers[tc.notMark]; ok {
				t.Errorf("marker %v was set; a host that failed this way must not get it", tc.notMark)
			}
		})
	}
}

func TestEnsureHostDoesNotCacheAFailure(t *testing.T) {
	m, st := newTestManager(t)
	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"h.example": {err: errConnRefused}},
	}).fetcher())
	ctx := context.Background()

	if _, err := m.EnsureHost(ctx, "h.example"); err == nil {
		t.Fatal("EnsureHost succeeded on an unreachable host")
	}

	state, err := st.HostState(ctx, "h.example")
	if err != nil {
		t.Fatalf("read host state: %v", err)
	}
	if !state.RobotsFetchedAt.IsZero() {
		t.Errorf("robots_at = %v; a host we never reached was recorded as resolved",
			state.RobotsFetchedAt)
	}
}

func TestEnsureHostTreatsAMissingRobotsAsPermission(t *testing.T) {
	for _, status := range []int{200, 204, 301, 400, 401, 403, 404, 410} {
		m, _ := newTestManager(t)
		m = m.WithRobotsFetcher((&fakeRobots{
			reply: map[string]robotsResponse{"h.example": {body: "", status: status}},
		}).fetcher())

		state, err := m.EnsureHost(context.Background(), "h.example")
		if err != nil {
			t.Errorf("status %d: EnsureHost returned %v, want success", status, err)
			continue
		}
		if state.RobotsFetchedAt.IsZero() {
			t.Errorf("status %d: the host was not recorded as resolved", status)
		}
		if got := NewRobotsRules(state.Allow, state.Disallow).Refuses("/anything"); got {
			t.Errorf("status %d: a host with no rules refused a path", status)
		}
	}
}

func TestEnsureHostCooldownOnARefusingHost(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   Reason
	}{
		{503, ReasonServerError},
		{500, ReasonServerError},
		{429, ReasonRateLimited},
	} {
		t.Run(itoa(tc.status), func(t *testing.T) {
			m, st := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{
				reply: map[string]robotsResponse{"h.example": {body: "boom", status: tc.status}},
			}).fetcher())
			ctx := context.Background()

			_, err := m.EnsureHost(ctx, "h.example")
			if err == nil {
				t.Fatalf("status %d: EnsureHost succeeded on a refusing host", tc.status)
			}
			if reason, ok := RobotsRefusedReason(err); !ok || reason != tc.want {
				t.Errorf("status %d: RobotsRefusedReason = %q/%v, want %q", tc.status, reason, ok, tc.want)
			}

			markers, err := st.Markers(ctx, "h.example")
			if err != nil {
				t.Fatalf("markers: %v", err)
			}
			if _, ok := markers[MarkerCooldown]; !ok {
				t.Errorf("status %d: no cooldown was set; markers = %v", tc.status, markerNames(markers))
			}
			if _, ok := markers[MarkerDead]; ok {
				t.Errorf("status %d: an overloaded host was marked dead", tc.status)
			}
		})
	}
}

func TestEnsureHostFloorsCrawlDelay(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"declared", "User-agent: *\nCrawl-delay: 7\n", 7 * time.Second},
		{"below the floor", "User-agent: *\nCrawl-delay: 0\n", time.Second},
		{"absent", "User-agent: *\nDisallow: /x\n", time.Second},
		{"fractional is ignored, not truncated", "User-agent: *\nCrawl-delay: 1.5\n", time.Second},
		{"a comment does not become the value", "User-agent: *\nCrawl-delay: # be nice\n", time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestManager(t)
			m = m.WithRobotsFetcher((&fakeRobots{
				reply: map[string]robotsResponse{"h.example": {body: tc.body}},
			}).fetcher())

			state, err := m.EnsureHost(context.Background(), "h.example")
			if err != nil {
				t.Fatalf("EnsureHost: %v", err)
			}
			if state.CrawlDelay != tc.want {
				t.Errorf("crawl delay = %v, want %v", state.CrawlDelay, tc.want)
			}
		})
	}
}

func TestEnsureHostRejectsAHostItCannotBuildARobotsURLFor(t *testing.T) {
	for _, host := range []string{"", "   ", "example.com/path", "user@example.com", "example.com?q", "a b.example"} {
		m, _ := newTestManager(t)
		fetch := &fakeRobots{reply: map[string]robotsResponse{}, status: 200}
		m = m.WithRobotsFetcher(fetch.fetcher())

		if _, err := m.EnsureHost(context.Background(), host); err == nil {
			t.Errorf("EnsureHost(%q) succeeded", host)
		}
		if n := fetch.totalCalls(); n != 0 {
			t.Errorf("EnsureHost(%q) made %d fetches, want 0", host, n)
		}
	}
}

func TestEnsureHostKeepsAHostsCountersAcrossARobotsReRead(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)
	ctx := context.Background()

	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"h.example": {body: "User-agent: *\nDisallow: /old\n"}},
	}).fetcher())

	if _, err := m.EnsureHost(ctx, "h.example"); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}

	spendPages(t, st, "h.example", 42)
	for range 5 {
		if _, err := st.RecordFailure(ctx, "h.example"); err != nil {
			t.Fatalf("incr failures: %v", err)
		}
	}

	clock.Advance(m.cfg.RobotsTTL + time.Second)
	fetch := &fakeRobots{
		reply: map[string]robotsResponse{"h.example": {body: "User-agent: *\nDisallow: /new\n"}},
	}
	m = m.WithRobotsFetcher(fetch.fetcher())
	if _, err := m.EnsureHost(ctx, "h.example"); err != nil {
		t.Fatalf("second EnsureHost: %v", err)
	}

	state, err := st.HostState(ctx, "h.example")
	if err != nil {
		t.Fatalf("read host state: %v", err)
	}
	if state.PagesCrawled != 42 {
		t.Errorf("pages_crawled = %d after a robots re-read, want 42", state.PagesCrawled)
	}
	if state.ConsecFailures != 5 {
		t.Errorf("consec_failures = %d after a robots re-read, want 5", state.ConsecFailures)
	}
	if !NewRobotsRules(state.Allow, state.Disallow).Refuses("/new") {
		t.Error("the re-read robots.txt did not replace the old rules")
	}
	if NewRobotsRules(state.Allow, state.Disallow).Refuses("/old") {
		t.Error("the old rules survived a re-read")
	}
}

func TestEnsureHostPreservesTheFirstSeenStamp(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)
	ctx := context.Background()

	m = m.WithRobotsFetcher((&fakeRobots{
		reply: map[string]robotsResponse{"h.example": {body: ""}},
	}).fetcher())

	if _, err := m.EnsureHost(ctx, "h.example"); err != nil {
		t.Fatalf("EnsureHost: %v", err)
	}
	first, err := st.HostState(ctx, "h.example")
	if err != nil {
		t.Fatalf("read host state: %v", err)
	}
	if first.FirstSeen.IsZero() {
		t.Fatal("first_seen was not recorded")
	}

	clock.Advance(m.cfg.RobotsTTL + time.Second)
	if _, err := m.EnsureHost(ctx, "h.example"); err != nil {
		t.Fatalf("second EnsureHost: %v", err)
	}
	second, err := st.HostState(ctx, "h.example")
	if err != nil {
		t.Fatalf("read host state: %v", err)
	}
	if !second.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("first_seen = %v, want %v", second.FirstSeen, first.FirstSeen)
	}
}

func TestEnsureHostEmptyHostIsAnError(t *testing.T) {
	m, _ := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	for _, host := range []string{"", "  "} {
		if _, err := m.EnsureHost(context.Background(), host); err == nil {
			t.Errorf("EnsureHost(%q) succeeded", host)
		}
	}
	if n := fetch.totalCalls(); n != 0 {
		t.Errorf("an empty host caused %d fetches, want 0", n)
	}
}

func TestEnsureHostNonPositiveRobotsTTLDoesNotReFetch(t *testing.T) {
	m, _ := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{"h.example": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	cfg := m.Config()
	cfg.RobotsTTL = 0
	m = m.WithConfig(cfg)

	for range 5 {
		if _, err := m.EnsureHost(context.Background(), "h.example"); err != nil {
			t.Fatalf("EnsureHost: %v", err)
		}
	}
	if n := fetch.callCount("h.example"); n != 1 {
		t.Errorf("a zero robots TTL caused %d fetches for 5 calls, want 1", n)
	}
}

func TestEnsureHostUnreadableStateIsAnError(t *testing.T) {
	m, st := newTestManager(t)
	fetch := &fakeRobots{reply: map[string]robotsResponse{}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())
	st.FailOn = map[string]error{"HostState": errRedisDown}

	if _, err := m.EnsureHost(context.Background(), "h.example"); err == nil {
		t.Fatal("EnsureHost succeeded with an unreadable state")
	}
	if n := fetch.totalCalls(); n != 0 {
		t.Errorf("an unreadable state caused %d fetches, want 0", n)
	}
}

var (
	errConnRefused  = &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	errTLSHandshake = &tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}
)

func spendPages(t *testing.T, st *MemoryState, host string, n int) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	rec := st.host[host]
	rec.Name = host
	rec.PagesCrawled = n
	st.host[host] = rec
}

func markerNames(m map[MarkerKind]time.Duration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.String())
	}
	return out
}

type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}

func robotsWithSiteMap(host, body string) *fakeRobots {
	return &fakeRobots{reply: map[string]robotsResponse{host: {body: body}}, status: 200}
}

type recordingResolver struct {
	entries []string

	mu    sync.Mutex
	calls int
	hosts []string
}

func (r *recordingResolver) resolve(_ context.Context, base *url.URL, _ []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.hosts = append(r.hosts, base.Host)
	return r.entries
}

func (r *recordingResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestASitemapIsReadOncePerRobotsReading(t *testing.T) {
	now := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	m, st, c := atClock(t, now)
	ctx := context.Background()
	const host = "mapped.example"

	res := &recordingResolver{entries: []string{
		"https://mapped.example/a",
		"https://mapped.example/b",
	}}
	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nSitemap: https://mapped.example/sitemap.xml\n").fetcher())
	m = m.WithSiteMapResolver(res.resolve)

	for range 3 {
		if _, err := m.EnsureHost(ctx, host); err != nil {
			t.Fatalf("EnsureHost: %v", err)
		}
		c.Advance(time.Hour)
	}

	if got := res.callCount(); got != 1 {
		t.Errorf("the resolver ran %d times for three robots readings, want 1", got)
	}
	if len(res.hosts) != 1 || res.hosts[0] != host {
		t.Errorf("resolved against %v, want [%s]: a sitemap resolved against any host "+
			"but the one that advertised it can pull a whole site into the frontier",
			res.hosts, host)
	}

	for _, want := range []string{"https://mapped.example/a", "https://mapped.example/b"} {
		found := false
		for url := range st.Frontier() {
			if url == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s did not reach the frontier", want)
		}
	}
}

func TestASitemapIsReadAgainAfterTheTTLExpires(t *testing.T) {
	now := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	m, _, c := atClock(t, now)
	ctx := context.Background()
	const host = "mapped.example"

	res := &recordingResolver{entries: []string{"https://mapped.example/a"}}
	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nSitemap: https://mapped.example/sitemap.xml\n").fetcher())
	m = m.WithSiteMapResolver(res.resolve)

	if _, err := m.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	if _, err := m.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	if got := res.callCount(); got != 1 {
		t.Fatalf("the resolver ran %d times inside the TTL, want 1", got)
	}

	c.Advance(m.cfg.RobotsTTL + time.Second)
	if _, err := m.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}
	if got := res.callCount(); got != 2 {
		t.Errorf("the resolver ran %d times after the TTL expired, want 2: a site that "+
			"adds a page and updates its sitemap would never be found again", got)
	}
}

func TestAHostWithNoSiteMapResolverStillCrawls(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const host = "mapped.example"

	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nSitemap: https://mapped.example/sitemap.xml\n").fetcher())

	st2, err := m.EnsureHost(ctx, host)
	if err != nil {
		t.Fatalf("EnsureHost on a host with no resolver: %v", err)
	}
	if len(st2.SiteMaps) != 1 {
		t.Errorf("SiteMaps = %v, want the advertised list still recorded", st2.SiteMaps)
	}
	if _, err := m.Admit(ctx, "https://mapped.example/page"); err != nil {
		t.Fatal(err)
	}

	rec, err := st.HostState(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.SiteMapsClaimedAt.IsZero() {
		t.Errorf("the sitemap claim was taken at %v even though nothing was read, so "+
			"a resolver installed later would never fire for this host",
			rec.SiteMapsClaimedAt)
	}
}

func TestSitemapEntriesAreFilteredByTheSameRules(t *testing.T) {
	now := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "mapped.example"

	res := &recordingResolver{entries: []string{
		"https://mapped.example/keep-me",
		"https://mapped.example/manual.pdf",
		"https://mapped.example/login",
		"https://mapped.example/private/secret",
		"https://mapped.example/Keep-Case",
		"://not a url",
	}}
	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nDisallow: /private\nSitemap: https://mapped.example/sitemap.xml\n").fetcher())
	m = m.WithSiteMapResolver(res.resolve)

	if _, err := m.EnsureHost(ctx, host); err != nil {
		t.Fatal(err)
	}

	frontier := st.Frontier()
	want := map[string]bool{
		"https://mapped.example/keep-me":   false,
		"https://mapped.example/Keep-Case": false,
	}
	for url := range frontier {
		if _, ok := want[url]; ok {
			want[url] = true
		}
	}
	for url, seen := range want {
		if !seen {
			t.Errorf("%s was refused from a sitemap; a sitemap is a hint, not an "+
				"exemption from the site's own rules", url)
		}
	}
	for _, refused := range []string{
		"https://mapped.example/manual.pdf",
		"https://mapped.example/login",
		"https://mapped.example/private/secret",
	} {
		if _, ok := frontier[refused]; ok {
			t.Errorf("%s reached the frontier from a sitemap", refused)
		}
	}

	stats := st.Stats(host)
	if stats[ReasonRobotsDisallow] != 1 {
		t.Errorf("robots_disallow = %d, want 1 (all: %v)", stats[ReasonRobotsDisallow], stats)
	}
	if stats[ReasonExtensionSkipped] != 1 {
		t.Errorf("extension_skipped = %d, want 1 (all: %v)", stats[ReasonExtensionSkipped], stats)
	}
}

func TestSitemapFilteringCostsNoRedisPerEntry(t *testing.T) {
	now := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	m, st, _ := atClock(t, now)
	ctx := context.Background()
	const host = "mapped.example"

	entries := make([]string, 1000)
	for i := range entries {
		entries[i] = "https://mapped.example/page-" + itoa(i)
	}
	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nSitemap: https://mapped.example/sitemap.xml\n").fetcher())
	m = m.WithSiteMapResolver((&recordingResolver{entries: entries}).resolve)

	st.FailOn = map[string]error{
		"IsVisited": errSitemapFilterWentToRedis,
		"Markers":   errSitemapFilterWentToRedis,
	}

	if _, err := m.EnsureHost(ctx, host); err != nil {
		t.Fatalf("EnsureHost with every read failing: %v", err)
	}

	if got := len(st.Frontier()); got != len(entries) {
		t.Errorf("%d of %d entries reached the frontier", got, len(entries))
	}
}

var errSitemapFilterWentToRedis = &sentinelError{"sitemaps are filtered without reading state"}

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

func TestTheSitemapClaimIsDecidedByWhichRobotsReadingItIs(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryState()
	first := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	won, err := st.ClaimSiteMaps(ctx, "racing.example", later)
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the first claim on a host nobody has claimed was refused")
	}

	won, err = st.ClaimSiteMaps(ctx, "racing.example", first)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a robots.txt reading older than the claimed one was allowed to queue " +
			"the same sitemaps again")
	}

	won, err = st.ClaimSiteMaps(ctx, "racing.example", later)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("the same robots.txt reading claimed its sitemaps twice")
	}

	won, err = st.ClaimSiteMaps(ctx, "racing.example", later.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Error("a newer robots.txt reading was refused: a site that publishes a new " +
			"page and updates its sitemap would never be found again")
	}
}

func TestWithRobotsClientSharesTheConnectionPool(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	}))
	defer srv.Close()

	m, _ := newTestManager(t)
	if _, err := m.EnsureHost(context.Background(), "127.0.0.1"); err == nil {
		t.Fatal("expected the default client to fail an untrusted certificate, so " +
			"this test would not have distinguished it from the one below")
	}

	m = m.WithRobotsClient(srv.Client())
	st2, err := m.EnsureHost(context.Background(), srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("EnsureHost over the shared client: %v", err)
	}
	if len(st2.Disallow) != 1 || st2.Disallow[0] != "/private" {
		t.Errorf("Disallow = %v, want the rule the shared client just read", st2.Disallow)
	}
}

func TestWithRobotsClientIgnoresANilClient(t *testing.T) {
	m, _ := newTestManager(t)
	before := m.fetchRobots
	if got := m.WithRobotsClient(nil).fetchRobots; got == nil {
		t.Fatal("WithRobotsClient(nil) removed the fetcher entirely")
	} else if before == nil {
		t.Fatal("test setup: the manager should have a fetcher to begin with")
	}
}

func TestHostKeyKeepsThePortThatIdentifiesTheSite(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"no port", "https://example.com/a", "example.com"},
		{"uppercase host", "https://ExAmPlE.CoM/a", "example.com"},
		{"a non-default port is part of the site", "https://example.com:8080/a", "example.com:8080"},
		{"https's own default port is not", "https://example.com:443/a", "example.com"},
		{"http's own default port is not", "http://example.com:80/a", "example.com"},
		{"a port that is only the default for the other scheme is kept",
			"http://example.com:443/a", "example.com:443"},
		{"a query or fragment does not reach the host", "https://example.com/a?b=:1", "example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostOfURL(tc.raw); got != tc.want {
				t.Errorf("hostOfURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	if got := hostKey(nil); got != "" {
		t.Errorf("hostKey(nil) = %q, want the empty string", got)
	}
	if got := hostOfURL("://not a url"); got != "" {
		t.Errorf("hostOfURL on an unparseable url = %q, want the empty string", got)
	}
}

func TestAClaimWithNoReadingIsRefused(t *testing.T) {
	st := NewMemoryState()
	won, err := st.ClaimSiteMaps(context.Background(), "unset.example", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a claim with no robots.txt reading behind it was granted")
	}
}

func TestExactlyOneWorkerExpandsASiteMapsReading(t *testing.T) {
	m, _, _ := atClock(t, time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const host = "contended.example"

	var calls atomic.Int64
	res := func(context.Context, *url.URL, []string) []string {
		calls.Add(1)
		return []string{"https://contended.example/a"}
	}
	m = m.WithSiteMapResolver(res)

	readAt := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	st := HostState{
		Name:            host,
		SiteMaps:        []string{"https://contended.example/sitemap.xml"},
		RobotsFetchedAt: readAt,
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			m.discoverSiteMaps(ctx, st)
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the resolver ran %d times for %d workers holding one robots.txt "+
			"reading, want 1: a second pass does not re-add the same urls, it raises "+
			"the score of the ones not crawled yet, once per robots re-read", got, workers)
	}
}

func TestConcurrentReadersOfANewHostExpandItsSitemapsOnce(t *testing.T) {
	m, st, _ := atClock(t, time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC))
	ctx := context.Background()
	const host = "contended.example"

	res := func(context.Context, *url.URL, []string) []string {
		return []string{"https://contended.example/a"}
	}
	m = m.WithRobotsFetcher(robotsWithSiteMap(host,
		"User-agent: *\nSitemap: https://contended.example/sitemap.xml\n").fetcher())
	m = m.WithSiteMapResolver(res)

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = m.EnsureHost(ctx, host)
		}()
	}
	close(start)
	wg.Wait()

	frontier := st.Frontier()
	got, ok := frontier["https://contended.example/a"]
	if !ok {
		t.Fatalf("the sitemap entry never reached the frontier; frontier: %v", frontier)
	}
	if got != 1 {
		t.Errorf("the sitemap entry's priority is %v, want 1: %v reads expanded the "+
			"same robots.txt, and a page whose score climbs on every re-read is a "+
			"page that is never crawled", got, got)
	}
}
