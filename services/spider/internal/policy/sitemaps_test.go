package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Sitemap discovery is the third thing this package moved out of the crawl loop,
// and it is the one with the nastiest history: sitemaps were expanded on the
// "no metadata for this host" branch, which is the branch every URL of a dead
// domain took. So the tests below are as much about *once* as about *at all*.

func robotsWithSiteMap(host, body string) *fakeRobots {
	return &fakeRobots{reply: map[string]robotsResponse{host: {body: body}}, status: 200}
}

// recordingResolver is a SiteMapResolver that returns a fixed list and remembers
// that it was called.
type recordingResolver struct {
	entries []string

	mu    sync.Mutex
	calls int
	// hosts records the host of each base it was handed, so a test can check that
	// a sitemap is resolved against the host that advertised it.
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

// TestASitemapIsReadOncePerRobotsReading is the load-bearing property.
//
// A sitemap's entries are enqueued with an inlink-derived priority, so expanding
// the same file twice does not merely re-add the URLs -- it raises the score of
// every URL that has not been crawled yet, once per pass. A host re-read daily
// would therefore be rescored daily, and any URL that took longer than a day to be
// reached would never be reached at all. The claim is what stops that.
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
		// Inside the TTL, so these reads are served from cache and the second and
		// third must not expand anything either.
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

	// And the entries reached the frontier.
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

// TestASitemapIsReadAgainAfterTheTTLExpires is the other half: the claim is not a
// permanent "already done". A site that adds a page and updates its sitemap expects
// the crawler to find it, and a host crawled for months would never look again.
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

// TestAHostWithNoSiteMapResolverStillCrawls covers the default. A manager used only
// to answer Admit has no business spending requests on discovery, so the absence of
// a resolver must be a quiet no-op -- not an error, and not a claim taken (which
// would burn the once-per-TTL for nothing).
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

	// The claim must not have been taken: with no resolver the work was skipped
	// rather than done, and skipping is not claiming.
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

// TestSitemapEntriesAreFilteredByTheSameRules is the reason filterLocally was
// extracted rather than written a second time.
//
// The divergence this prevents is specific: a sitemap filter that forgot the
// extension table would queue every PDF the site lists, and a full URL table would
// let a sitemap reintroduce a URL that robots.txt disallows -- a site could write
// "Disallow: /private" and then list /private/a in its sitemap, and be obeyed in
// one path and not the other.
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

	// And the refusals were counted, so an operator can see how much of the site's
	// own advertised surface this crawler declines.
	stats := st.Stats(host)
	if stats[ReasonRobotsDisallow] != 1 {
		t.Errorf("robots_disallow = %d, want 1 (all: %v)", stats[ReasonRobotsDisallow], stats)
	}
	if stats[ReasonExtensionSkipped] != 1 {
		t.Errorf("extension_skipped = %d, want 1 (all: %v)", stats[ReasonExtensionSkipped], stats)
	}
}

// TestSitemapFilteringCostsNoRedisPerEntry is a performance contract written as a
// test, because the cost is invisible in a review.
//
// A sitemap routinely lists fifty thousand URLs. Running the full gate over one --
// which is what Admit does, correctly, for a URL about to be fetched -- would mean a
// hundred thousand Redis round trips inside the call deciding whether this host is
// crawlable at all, on the crawl path, before a single page of it has been
// admitted. The state-dependent rules are not skipped so much as deferred to pop
// time, where they have to be asked anyway.
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

	// The two reads Admit makes per URL (steps 3 and 4: visited, then markers),
	// failed. They are the ones a per-entry gate would make fifty thousand times,
	// and the ones a deferred rule would not make at all -- the state-dependent
	// rules are re-asked when the URL is popped, which is where they have to be
	// asked anyway.
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

// TestTheSitemapClaimIsHandedOutOnceUnderConcurrency is the property the claim has
// to have at the moment it is actually contended, which is twenty workers meeting a
// host none of them have seen.
func TestTheSitemapClaimIsDecidedByWhichRobotsReadingItIs(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryState()
	first := time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	// The newer reading is acted on first.
	won, err := st.ClaimSiteMaps(ctx, "racing.example", later)
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("the first claim on a host nobody has claimed was refused")
	}

	// The older reading, arriving afterwards, must lose: its sitemaps are the ones
	// already queued.
	won, err = st.ClaimSiteMaps(ctx, "racing.example", first)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("a robots.txt reading older than the claimed one was allowed to queue " +
			"the same sitemaps again")
	}

	// The same reading again also loses: that is the plain exactly-once case, and the
	// one thing a flag did get right.
	won, err = st.ClaimSiteMaps(ctx, "racing.example", later)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Error("the same robots.txt reading claimed its sitemaps twice")
	}

	// And a genuinely newer reading wins, so a site that updates its sitemap is
	// picked up.
	won, err = st.ClaimSiteMaps(ctx, "racing.example", later.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Error("a newer robots.txt reading was refused: a site that publishes a new " +
			"page and updates its sitemap would never be found again")
	}
}

// TestWithRobotsClientSharesTheConnectionPool is about the wiring rather than the
// rules, and it is a test because the thing it protects is invisible from the
// outside: with two clients there are two pools and two timeout policies, and
// nothing at a glance says so.
//
// The observable half is the certificate. A test server's certificate is trusted
// only by its own client, so a manager that kept its default fetcher would fail
// every robots.txt read here as a TLS error -- which is the shape of a dead host,
// and would turn every sitemap test above into a test of the dead-host path.
func TestWithRobotsClientSharesTheConnectionPool(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	}))
	defer srv.Close()

	m, _ := newTestManager(t)
	// Before: the manager's own client, which does not trust this certificate.
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

// TestWithRobotsClientIgnoresANilClient is the degenerate case, and getting it wrong
// is a nil dereference on the crawl's hot path: every URL of an unresolvable domain
// goes through it.
func TestWithRobotsClientIgnoresANilClient(t *testing.T) {
	m, _ := newTestManager(t)
	before := m.fetchRobots
	if got := m.WithRobotsClient(nil).fetchRobots; got == nil {
		t.Fatal("WithRobotsClient(nil) removed the fetcher entirely")
	} else if before == nil {
		t.Fatal("test setup: the manager should have a fetcher to begin with")
	}
}

// TestHostKeyKeepsThePortThatIdentifiesTheSite is about the string a host's records
// are filed under, and both halves of it are load-bearing.
//
// The port has to be kept, because the robots.txt URL is built from this string: a
// crawler reading example.com's rules while crawling example.com:8080 is reading a
// different site's instructions, and on the usual case it reads nothing at all and
// reads the connection failure as proof the domain is dead.
//
// And the scheme's default port has to go, because https://example.com and
// https://example.com:443 are the same site, and filing them separately hands one
// site two page budgets, two stats hashes and two sets of robots rules -- the same
// doubling that the lowercasing below exists to prevent.
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

// TestAClaimWithNoReadingIsRefused is the degenerate case. A caller with no reading
// to attribute the claim to cannot be the worker for a particular robots.txt, and
// letting it through on "no timestamp beats nothing" would make every call without
// one a winner -- which is the state the record is in before any reading exists.
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

// TestExactlyOneWorkerExpandsASiteMapsReading is the property the claim has at the
// moment it is actually contended: twenty workers, one robots.txt, one pass over the
// sitemap.
//
// It calls discoverSiteMaps directly rather than going through EnsureHost, and that
// is the whole design of the test. EnsureHost on a brand-new host is a read, a
// network fetch and a write, and how many of twenty workers manage to read before
// the first one writes is a property of the scheduler: some of them take the cached
// path afterwards and never reach discovery at all, so a test at that level measures
// the width of the read-modify-write race window rather than the claim. It will
// report one resolver call today and two after a coffee machine is plugged in, and
// both of those numbers say nothing about whether the claim works.
//
// At this level every worker is past the read and holds the same reading, which is
// the situation the claim exists for. So the count here is exact.
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

	// One reading, shared by every worker, exactly as it is after a single
	// EnsureHost has saved it.
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

// TestConcurrentReadersOfANewHostExpandItsSitemapsOnce is the same property one
// layer out, with the parts that are genuinely nondeterministic left out.
//
// Eight workers meet a host none of them has seen, each reads the (empty) record,
// and each is racing the others to be the one that writes it. How many of them read
// before the first write is not knowable in advance, so this test does not count
// resolver calls -- it counts the thing the count would have damaged.
//
// A sitemap entry enters the frontier by inlink priority, and this entry has exactly
// one inlink: the sitemap itself. If two workers expand the same reading, that score
// becomes two, and the page jumps the queue ahead of pages a hundred other sites
// linked to. The score is therefore a readout of how many times the file was
// expanded, and the assertion below is the one that cannot be satisfied by accident.
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
			// Errors are ignored deliberately: every worker asking is the case under
			// test, and a failure of one worker's request is not what this asserts.
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
	// One link points at this page, from the sitemap, so one is the only honest
	// score. Anything higher is the same reading expanded twice.
	if got != 1 {
		t.Errorf("the sitemap entry's priority is %v, want 1: %v reads expanded the "+
			"same robots.txt, and a page whose score climbs on every re-read is a "+
			"page that is never crawled", got, got)
	}
}
