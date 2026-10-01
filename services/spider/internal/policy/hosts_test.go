package policy

import (
	"context"
	"crypto/tls"
	"net"
	"syscall"
	"testing"
	"time"
)

// TestAdmitEnforcesThePageBudget walks the whole cold-window machine from the plan
// with an injected clock, because every part of it is a claim about time and
// sleeping through it would take an hour.
//
// The sequence is: a host is crawled up to its budget, the next URL finds the
// budget spent and puts the host cold, URLs during the cold period are refused
// without a network call, and when the cold period elapses the host is crawled
// again from zero. The last part is the one that is easy to get wrong, and the
// wrong version is the default: a budget that never resets means a host is
// crawled once, ever, and quietly disappears from the index.
func TestAdmitEnforcesThePageBudget(t *testing.T) {
	now := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	m, st, clock := atClock(t, now)
	ctx := context.Background()

	fetch := &fakeRobots{reply: map[string]robotsResponse{"big.example": {body: ""}}, status: 200}
	m = m.WithRobotsFetcher(fetch.fetcher())

	// A small budget, so the test is about the machine rather than about counting
	// to five thousand.
	const budget = 3
	cfg := m.Config()
	cfg.MaxPagesPerHost = budget
	m = m.WithConfig(cfg)

	// One URL resolves the host and spends nothing yet.
	if v := mustAdmit(t, m, uniqueURL("big.example", "/a")); v.Kind != Allow {
		t.Fatalf("first Admit = %s, want Allow", FormatVerdict(v))
	}
	// Crawled pages are counted by Classify, which is not in this phase; the count
	// is set here directly so the test is about Admit's half.
	spendPages(t, st, "big.example", budget)

	v := mustAdmit(t, m, uniqueURL("big.example", "/b"))
	assertVerdict(t, v, Skip, ReasonHostBudgetExhausted)
	if v.Until.IsZero() {
		t.Error("an exhausted host refused the URL with no Until; nothing could schedule its return")
	}

	// Cold: one Redis read, no network.
	fetch.forbidCalls(t)
	if got := mustAdmit(t, m, uniqueURL("big.example", "/c")).Reason; got != ReasonHostBudgetExhausted {
		// The cold marker now short-circuits ahead of the budget check, which is
		// the cheaper of the two and gives a better reason.
		if got != ReasonHostCold {
			t.Errorf("reason = %q, want %q", got, ReasonHostCold)
		}
	}
	if n := fetch.callCount("big.example"); n != 1 {
		t.Errorf("a cold host was re-fetched: %d robots fetches, want 1", n)
	}

	// The cold period expires: the host comes back, and its budget is whole again.
	// The clock is moved rather than the marker waited out, so this is exact.
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

// TestAdmitBudgetCountsPerWindowNotForEver states the same rule as a boundary, at
// both edges.
//
// One second short of the window ending, the host is still out of budget. One
// second past it, it is in. There is no third state, and no gradual drawdown.
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

	// Just inside the window.
	clock.Advance(m.cfg.HostColdPeriod - time.Second)
	st.ClearMarker(ctx, "big.example", MarkerCold)
	if kind, reason := admitReason(t, m, uniqueURL("big.example", "/b")); kind == Allow {
		t.Errorf("Admit inside the window = %v/%s, want a refusal", kind, reason)
	}

	// Just outside it.
	_ = windowStarted
	clock.Advance(2 * time.Second)
	st.ClearMarker(ctx, "big.example", MarkerCold)
	if v := mustAdmit(t, m, uniqueURL("big.example", "/c")); v.Kind != Allow {
		t.Errorf("Admit after the window = %s, want Allow", FormatVerdict(v))
	}
}

// TestAdmitZeroColdPeriodDoesNotResetEveryURL guards the configuration that would
// silently disable the budget.
//
// HOST_COLD_PERIOD_SEC=0 means the window is zero long, so a rollover is due on
// every URL and the count is cleared before it is read. A host could then be
// crawled without limit -- the same runaway this package exists to prevent,
// reached by setting a variable to zero.
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

	// Every URL after the budget is spent must still be refused. A rollover on each
	// one would allow it.
	for range 5 {
		clock.Advance(time.Millisecond)
		st.ClearMarker(context.Background(), "big.example", MarkerCold)
		if v := mustAdmit(t, m, uniqueURL("big.example", "/b")); v.Kind == Allow {
			t.Fatal("a zero cold period reset the budget on every URL, making the budget unenforceable")
		}
	}
}

// TestAdmitResetsFailClosed covers the one write whose failure cannot be tolerated.
//
// A rollover that fails leaves the count on hand unreadable: it might be the
// expired window's count, which would refuse a host that has finished its window,
// or it might be the new one's, which would fetch past a real budget. Neither is
// knowable, so the answer is to stop and let the next URL try again.
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
	// Not merely "not Allow". The claim is that the crawl *stops*, because the
	// count on hand is the expired window's and enforcing it would refuse a host
	// that has finished its window. Proceeding with that count, or clearing it and
	// carrying on, would both fetch past a budget that may or may not exist.
	assertVerdict(t, v, Defer, ReasonPolicyUnavailable)
	// The deadline is one backoff interval, and it is not a claim about this host.
	// The host's own schedule is unreadable -- that is what failed -- so nothing
	// here says when *it* may be crawled again. What is known is when to ask
	// again, and that is the same interval a failed fetch waits out.
	//
	// It has to be a deadline rather than nothing at all. The URL is already off the
	// frontier by the time this verdict exists, so a zero Until is not "leave it
	// alone for this round": it is deleted, and nothing rediscovers it during the
	// outage because every page that would is gated on the same store.
	//
	// See TestEveryDeferComesWithADueTime, which is the same property stated over
	// every path that produces a Defer rather than over one of them.
	want := now.Add(m.cfg.HostColdPeriod + time.Second).Add(m.cfg.URLBackoff(1))
	if !v.Until.Equal(want) {
		t.Errorf("Until = %v, want %v -- one URLBackoff interval from now", v.Until, want)
	}
}

// TestAdmitUnresolvedHostDefersRatherThanSkips is the verdict that keeps a slow
// host in the index.
//
// A host we could not fetch robots.txt from is deferred: the URL may be perfectly
// fine in ten seconds. Skip would mark it visited, and a marked URL is never
// offered again, so one unreachable robots.txt would delete every URL ever
// discovered on that host -- silently, and permanently.
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

	// And the reason is counted, because "how many hosts are we failing to resolve"
	// is the first question an operator asks about a crawl that looks stalled.
	if n := st.Stats("slow.example")[ReasonHostMetadataUnknown]; n != 1 {
		t.Errorf("stats[slow.example][host_metadata_unavailable] = %d, want 1", n)
	}
}

// TestAdmitStopsReachingTheNetworkForAnUnreachableHost is the negative cache, and
// the whole point of this phase.
//
// One failure, and every later URL on that host is answered from Redis. Before
// this, the failure was recorded nowhere that survived the request, so the next URL
// opened another connection, and the next twenty after that did the same -- with
// twenty workers, that is twenty simultaneous connection attempts to a domain that
// does not resolve.
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

	// The negative cache: no further fetch, whatever the outcome of the Admit.
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

// TestAdmitRetriesAnUnreachableHostAfterTheMarkerExpires is the other half: the
// negative cache must have an end, or a domain that came back would never be
// crawled again.
//
// There is no background prober. The next URL on the host after the marker
// expires *is* the probe, which is why the marker has to expire at all.
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

	// Long before the marker expires: still no fetch.
	clock.Advance(time.Second)
	admitReason(t, m, uniqueURL("flaky.example", "/x"))
	if n := fetch.callCount("flaky.example"); n != before {
		t.Errorf("a fetch happened inside the dead window: %d, want %d", n, before)
	}

	// Past the dead marker's TTL. The next Admit is the probe, and it is allowed to
	// make the call.
	clock.Advance(m.cfg.DeadHostTTL(1) + time.Second)
	admitReason(t, m, uniqueURL("flaky.example", "/y"))
	if n := fetch.callCount("flaky.example"); n != before+1 {
		t.Errorf("the host was never probed again: %d fetches, want %d", n, before+1)
	}
}

// TestEnsureHostTreatsATimeoutAsSlowNotGone is the single most consequential
// distinction in the package, isolated here so nothing else has to be true for it
// to be exercised.
//
// A timeout and a refused connection look identical from the client and are
// opposite decisions. A slow site is exactly the one worth crawling politely, so a
// timeout gets a cooldown and the host is crawled again. Marking it dead would
// abandon every under-loaded site on the web.
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

// TestEnsureHostDoesNotCacheAFailure is the trap that makes a negative cache
// quietly permanent.
//
// Writing a host state on failure would record a fetch timestamp, and the marker
// would expire in a minute while RobotsTTL is a day -- so the next EnsureHost would
// find a "fresh" record for a host it never managed to read and never try again.
// The host would be permanently uncrawlable, which looks exactly like a site with
// nothing to offer.
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

// TestEnsureHostTreatsAMissingRobotsAsPermission covers the 4xx case, which is
// most of the web.
//
// A host with no robots.txt publishes no rules. Treating the 404 as a failure made
// every such host look unreachable, and the crawl spent its whole time proving
// that hosts which have no robots.txt are not answering.
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
		// A host that publishes no rules is not restricted by them.
		if got := NewRobotsRules(state.Allow, state.Disallow).Refuses("/anything"); got {
			t.Errorf("status %d: a host with no rules refused a path", status)
		}
	}
}

// TestEnsureHostCooldownOnARefusingHost separates "gone" from "not now".
//
// A 503 is an overloaded site, not an absent one. It gets a cooldown, so the next
// URL is a Defer rather than another connection, and the host is still crawled
// later. A 429 says the same thing more explicitly.
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

// TestEnsureHostFloorsCrawlDelay is the rate limit that was never applied.
//
// Crawl-delay was parsed, written to a key nothing read, and the spider
// therefore had no rate limiting at all. It is also floored: a robots.txt asking
// for zero, or omitting the directive, must not mean "no delay".
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

// TestEnsureHostRejectsAHostItCannotBuildARobotsURLFor keeps a nonsense host out of
// the fetch path.
//
// The host came out of a URL, so in principle it is always a hostname. In practice
// a URL with a userinfo section, a path or a space produces a "host" that is none
// of those things, and requesting "https://user@evil.example/x/robots.txt" is a
// request to a host nobody chose.
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

// TestEnsureHostKeepsAHostsCountersAcrossARobotsReRead is a regression guard on a
// merge that is easy to get wrong.
//
// The robots.txt and the counters are rewritten by different events: the rules on
// a schedule measured in hours, the page and failure counts on every fetch.
// Overwriting the record on a rules re-read reset both, which handed every host a
// fresh page budget once a day and dropped a host that had failed five times in a
// row back to its first, shortest backoff.
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

	// The site's rules change, and the TTL expires.
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
	// And the new rules took effect, which is the point of re-reading at all.
	if !NewRobotsRules(state.Allow, state.Disallow).Refuses("/new") {
		t.Error("the re-read robots.txt did not replace the old rules")
	}
	if NewRobotsRules(state.Allow, state.Disallow).Refuses("/old") {
		t.Error("the old rules survived a re-read")
	}
}

// TestEnsureHostPreservesTheFirstSeenStamp keeps a host's history intact.
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

// TestEnsureHostEmptyHostIsAnError covers the one input no caller should pass, so
// it fails loudly rather than fetching "https:///robots.txt".
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

// TestEnsureHostNonPositiveRobotsTTLDoesNotReFetch guards the configuration that
// would restore the original bug.
//
// A zero TTL means "re-fetch for every URL", which is exactly what caching was
// added to stop, and nothing else would go wrong -- the crawl would simply be slow
// again in a way no log line mentions. It is read as "never re-read", because a
// stale rule that still says "Disallow: /private" errs towards politeness while a
// re-fetch storm errs towards load.
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

// TestEnsureHostUnreadableStateIsAnError checks the one read that has no
// interpretation as a value.
//
// An unknown host is a normal condition, so HostState returns an empty record for
// one. A *failed* read is not, and treating it as an empty record would fetch
// robots.txt for every URL on every host during a Redis outage.
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

// These three failures are wrapped in the shapes production actually sees. A bare
// error would take the "unrecognised" branch in categorizeError and the test would
// pass without touching the classification it exists to check.
var (
	errConnRefused  = &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	errTLSHandshake = &tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}
)

// --- helpers used only by these tests ---

// spendPages puts a host in the state a long crawl would have left it in, so a
// test about the budget does not have to run a crawl to get there.
//
// It writes the record directly rather than calling RecordSuccess n times. Both
// reach the same field; only one of them keeps the test about the budget rather
// than about how the count got there, and 5000 calls in a unit test is a lot of
// noise for no coverage. The companion integration test seeds a real hash the
// same way, for the same reason.
func spendPages(t *testing.T, st *MemoryState, host string, n int) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	rec := st.host[host]
	rec.Name = host
	rec.PagesCrawled = n
	st.host[host] = rec
}

// markerNames renders a marker set for a failure message, since MarkerKind is an
// int and "expected [1 2 3] got [2]" is not a sentence.
func markerNames(m map[MarkerKind]time.Duration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.String())
	}
	return out
}

// timeoutError is a net.Error that times out, which is the shape a slow host's
// failure arrives in.
type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }
func (timeoutError) Temporary() bool {
	return true
}
