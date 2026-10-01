package spider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/messaging"
	"github.com/Hassan-ach/boogle/services/spider/internal/parser"
	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
	"uuid"
)

// The crawl loop, tested.
//
// Everything here is about the seam between the loop and the policy manager. The
// loop is the only place a page is fetched, so the only thing worth asserting about
// it is which URLs it decides not to fetch -- and, before this was wired, it decided
// several of them itself: it looked up host metadata, generated it when missing
// (which fetched robots.txt), applied its own path rules, and slept in a retry loop.
// Every one of those was a crawl decision made outside the one place that owns
// them, and a test of the loop cannot exist at all until they are gone.

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeStore records what the loop decided to persist.
type fakeStore struct {
	mu    sync.Mutex
	pages []*entity.Page
	// hosts records the host record the loop handed over with each page, so a test
	// can check the loop is not carrying a host it made up.
	hosts []string

	// pageID is what Persist returns. uuid.Nil is the loop's "not persisted" signal,
	// so it defaults to a real id and a test asks for Nil explicitly.
	pageID uuid.UUID
}

func newFakeStore() *fakeStore {
	return &fakeStore{pageID: uuid.New()}
}

func (s *fakeStore) Init([]string) error { return nil }

func (s *fakeStore) Close() {}

func (s *fakeStore) Persist(_ context.Context, page *entity.Page, host *entity.Host) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages = append(s.pages, page)
	if host != nil {
		s.hosts = append(s.hosts, host.Name)
	}
	return s.pageID
}

func (s *fakeStore) persisted() []*entity.Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*entity.Page(nil), s.pages...)
}

func (s *fakeStore) persistedHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hosts...)
}

// fakeQueue is a messaging pair that records what was published.
type fakeQueue struct{}

func (fakeQueue) NewMessagingChannel() (messaging.MessagingChannel, error) {
	return &fakeChannel{}, nil
}
func (fakeQueue) DeclareQueue(string) error { return nil }
func (fakeQueue) Close()                    {}

type fakeChannel struct {
	mu        sync.Mutex
	published []string
	closed    bool
}

func (c *fakeChannel) Publish(queueName string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.published = append(c.published, queueName+":"+string(body))
	return nil
}

func (c *fakeChannel) Consume(string, func([]byte) error) error { return nil }

func (c *fakeChannel) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *fakeChannel) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.published...)
}

// failingParser fails on demand, for the "fetched fine, would not parse" path. That
// path is unreachable with the real parser for a body the test controls, and it is
// the path that most needs testing: an unparseable body that is not retired is
// re-fetched for ever.
type failingParser struct {
	err error
}

func (p failingParser) ParseHTML(io.Reader, string) (*entity.Page, error) {
	return nil, p.err
}

// ── the site under test ──────────────────────────────────────────────────────

// site is an httptest server that answers robots.txt and serves pages, and counts
// every request per path. The counts are the whole point: "was this URL fetched?" is
// not answerable from state alone.
type site struct {
	srv *httptest.Server

	mu       sync.Mutex
	hits     map[string]int
	robots   string
	pages    map[string]pageReply
	uaOnPage map[string]string
}

type pageReply struct {
	status      int
	contentType string
	body        string
}

func newSite(t *testing.T) *site {
	t.Helper()
	s := &site{
		hits:     map[string]int{},
		robots:   "User-agent: *\nAllow: /\n",
		pages:    map[string]pageReply{},
		uaOnPage: map[string]string{},
	}
	// TLS, not plain HTTP, and not as a convenience: the policy manager canonicalises
	// every URL to https before anything else looks at it, so a plain-HTTP test server
	// would be unreachable under the crawler's own spelling of its own URLs. Testing
	// against http:// would mean testing a loop that fetches something other than what
	// it was given.
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		ua := r.Header.Get("User-Agent")
		s.mu.Unlock()

		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(s.robots))
			return
		}

		s.mu.Lock()
		reply, ok := s.pages[r.URL.Path]
		s.uaOnPage[r.URL.Path] = ua
		s.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if reply.contentType != "" {
			w.Header().Set("Content-Type", reply.contentType)
		}
		status := reply.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) serve(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages[path] = pageReply{status: http.StatusOK, contentType: "text/html", body: body}
}

func (s *site) serveStatus(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pages[path] = pageReply{status: status, contentType: "text/html", body: "nope"}
}

func (s *site) setRobots(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.robots = body
}

func (s *site) hitsOn(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *site) userAgentOn(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uaOnPage[path]
}

// pageURL is the site's own address for a path, with the host substituted for
// host-based rules.
func (s *site) pageURL(path string) string {
	return s.srv.URL + path
}

// ── the loop under test ──────────────────────────────────────────────────────

// harness is a Spider with everything expensive replaced: an in-memory policy
// state, a scriptable clock, an httptest site, and a fake store.
type harness struct {
	spider *Spider
	state  *policy.MemoryState
	policy *policy.PolicyManager
	site   *site
	store  *fakeStore
	clock  *testClock
	logs   *utils.Logger
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newHarness builds the loop with the expensive pieces replaced: an in-memory policy
// state, a scriptable clock, an httptest site, and a fake store. A nil parser means
// the real one.
func newHarness(t *testing.T, s *site, p htmlParser, clock *testClock) *harness {
	t.Helper()
	return newHarnessWithConfig(t, s, p, clock, nil)
}

// newHarnessWithConfig is newHarness with the policy configuration adjusted, for the
// few tests whose point is a setting rather than the wiring.
func newHarnessWithConfig(
	t *testing.T, s *site, p htmlParser, clock *testClock, adjust func(*policy.Config),
) *harness {
	t.Helper()

	cfg := policy.DefaultConfig()
	if adjust != nil {
		adjust(&cfg)
	}
	state := policy.NewMemoryStateAt(clock.Now)
	logs := utils.NewDiscardLogger()

	// Both clients are the test server's, which is what production does too -- and
	// the reason it has to be the test server's is that it trusts the server's
	// certificate. A policy manager left on its own client would fail every
	// robots.txt fetch here as a TLS error, mark every test host dead, and turn
	// every test below into a test of the dead-host path.
	pm := policy.New(cfg, state, logs.Logger).WithClock(clock.Now).
		WithRobotsClient(s.srv.Client()).
		WithFetchClient(s.srv.Client())

	if p == nil {
		p = parser.NewParser(s.srv.Client(), logs)
	}

	st := newFakeStore()
	return &harness{
		state:  state,
		policy: pm,
		site:   s,
		store:  st,
		clock:  clock,
		logs:   logs,
		spider: &Spider{
			config:         &config.Config{},
			httpClient:     s.srv.Client(),
			mq:             fakeQueue{},
			parser:         p,
			store:          st,
			policy:         pm,
			ctx:            context.Background(),
			crawlerTimeout: 10 * time.Second,
			crawlerDelay:   time.Millisecond,
			fetchpool:      make(chan struct{}, 4),
			logger:         logs,
		},
	}
}

// crawl runs one round of the loop.
func (h *harness) crawl(t *testing.T) {
	t.Helper()
	h.spider.crawl(0)
}

// enqueue puts a URL in the frontier, the way a discovery would.
func (h *harness) enqueue(t *testing.T, urls ...string) {
	t.Helper()
	if err := h.state.Enqueue(context.Background(), urls...); err != nil {
		t.Fatal(err)
	}
}

// frontierLen is how much work the round had, and how much it has left.
func (h *harness) frontierLen(t *testing.T) int64 {
	t.Helper()
	n, err := h.state.FrontierLen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// delayed is what is parked, and when it is due. A deferred URL lives here between
// rounds, so this is where "it will be tried again" is either true or not.
func (h *harness) delayed(t *testing.T) map[string]time.Time {
	t.Helper()
	return h.state.Delayed()
}

const englishPage = `<!doctype html><html lang="en"><head><title>t</title></head>` +
	`<body><p>hello</p></body></html>`

// ── the tests ────────────────────────────────────────────────────────────────

// TestARefusedURLIsNeverFetched is the first of the three properties the loop has
// to have, and the one that used to be false.
//
// The loop used to decide for itself: it looked up host metadata, and if the host
// was not in the store it went and generated some -- fetching robots.txt, then the
// sitemaps it advertised, for every URL of a host it had never seen. So the fetch of
// a disallowed URL was not merely wasteful, it happened *after* the check that was
// supposed to prevent it, on a branch that only ran for unknown hosts.
//
// The count is the assertion. Not "the policy layer said no", but "the request
// never left".
func TestARefusedURLIsNeverFetched(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	s.setRobots("User-agent: *\nDisallow: /private\nAllow: /\n")

	h := newHarness(t, s, nil, clock)
	const refused = "/private/report"
	s.serve(refused, englishPage)

	h.enqueue(t, s.pageURL(refused))
	h.crawl(t)

	if got := s.hitsOn(refused); got != 0 {
		t.Errorf("the refused URL was fetched %d times, want 0: the loop decided "+
			"something the policy layer decides", got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages, want none", len(got))
	}
	// And it is terminal, so rediscovering it costs no request at all.
	visited, err := h.state.IsVisited(context.Background(), s.pageURL(refused))
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("the refused URL was not retired; it comes back on every rediscovery")
	}
}

// TestADeadHostCostsNothingPerURL is the infinite loop this whole change exists to
// stop.
//
// A host that is gone is marked once. After that, every URL it owns is answered
// from the marker in O(1), with no request of any kind. The bug was that the
// dead-host case was not consulted at all: a URL from an unresolvable domain
// triggered host metadata generation, which fetched robots.txt, which failed, which
// triggered host metadata generation on the next URL, for ever -- one request per
// URL, growing with the size of the corpus.
func TestADeadHostCostsNothingPerURL(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)

	// A host that is not the test server and does not resolve.
	const host = "gone.invalid"
	h := newHarness(t, s, nil, clock)

	pages := make([]string, 20)
	for i := range pages {
		pages[i] = fmt.Sprintf("https://%s/page-%d", host, i)
		h.enqueue(t, pages[i])
	}
	for range pages {
		h.crawl(t)
	}

	// The test server's own robots.txt is never involved: these URLs are on another
	// host, so any request this loop made to /robots.txt would be the bug.
	if got := s.hitsOn("/robots.txt"); got != 0 {
		t.Errorf("robots.txt was fetched %d times for URLs on %s", got, host)
	}

	markers, err := h.state.Markers(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if _, dead := markers[policy.MarkerDead]; !dead {
		t.Errorf("the host was never marked dead (markers: %v), so every one of its "+
			"URLs will try again", markers)
	}

	// Each URL is terminal or parked, and none of them is left in the frontier to be
	// picked up again: after twenty rounds the frontier is empty.
	remaining := h.state.Frontier()
	for url := range remaining {
		if strings.Contains(url, host) {
			t.Errorf("%s is still in the frontier after its host was found to be gone", url)
		}
	}
}

// TestADeferredURLIsParkedWithARealDueTime covers the Defer half of the gate.
//
// The bug this replaces: a host that answered 429 or 503 was simply not crawled,
// with nothing recorded, so the next discovery of any of its URLs was another
// request. Parking it means the retry is scheduled rather than forgotten, and the
// due time is derived from the host's own failure count.
func TestADeferredURLIsParkedWithARealDueTime(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	s.serve("/slow", "wait")

	h := newHarness(t, s, nil, clock)

	// The host answers, then goes quiet: one page counted, then a 503.
	s.serve("/first", englishPage)
	const busy = "/busy"
	s.serveStatus(busy, http.StatusServiceUnavailable)

	h.enqueue(t, s.pageURL("/first"))
	h.crawl(t)
	if got := s.hitsOn("/first"); got != 1 {
		t.Fatalf("the first page was fetched %d times, want 1", got)
	}

	h.enqueue(t, s.pageURL(busy))
	h.crawl(t)

	if got := s.hitsOn(busy); got != 1 {
		t.Errorf("the failing URL was fetched %d times, want exactly 1: a retry loop "+
			"inside one worker is what this design removed", got)
	}
	if got := h.store.persisted(); len(got) != 1 {
		t.Fatalf("persisted %d pages, want only the successful one", len(got))
	}

	// The host is cooling down, so a second URL on it is deferred without a request.
	h.enqueue(t, s.pageURL("/other"))
	h.crawl(t)
	if got := s.hitsOn("/other"); got != 0 {
		t.Errorf("a URL on a cooling host was fetched %d times, want 0", got)
	}

	// And it is parked, with a due time in the future.
	delayed := h.state.Delayed()
	due, ok := delayed[s.pageURL("/other")]
	if !ok {
		t.Fatalf("the deferred URL was not parked (%v); it has simply been dropped, "+
			"so a site that recovers is never crawled again", delayed)
	}
	if !due.After(clock.Now()) {
		t.Errorf("parked until %v, which is not after %v: the retry would fire "+
			"immediately and the cooldown would be a no-op", due, clock.Now())
	}
}

// TestA404IsTerminal is the property the attempt ceiling is a backstop for, and the
// one whose absence is the loop in its simplest form: a 404 that is not recorded is
// a 404 that is fetched again every time anything links to it.
func TestA404IsTerminal(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const missing = "/gone"
	// Not served at all: the test server answers 404 for anything unserved.

	h := newHarness(t, s, nil, clock)

	// Discovered repeatedly, which is what a popular link does.
	for range 5 {
		h.enqueue(t, s.pageURL(missing))
		h.crawl(t)
	}

	if got := s.hitsOn(missing); got != 1 {
		t.Errorf("a 404 was fetched %d times across five rediscoveries, want 1", got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages, want none", len(got))
	}
	stats := h.state.Stats(hostOf(s.srv.URL))
	if stats[policy.ReasonNotFound] != 1 {
		t.Errorf("not_found = %d, want 1 (all: %v)", stats[policy.ReasonNotFound], stats)
	}
}

// TestANonEnglishPageIsDroppedAndCounted is one of the two post-fetch refusals, and
// it is here because the loop used to report it as a failed fetch.
//
// Every non-English page in the corpus logged "Failed to fetch and parse page" and
// counted nothing, which is the same blindness as a dead domain: an operator reading
// the logs could not tell a site that had gone away from a site whose pages are in
// the wrong language.
func TestANonEnglishPageIsDroppedAndCounted(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/fr"
	s.serve(path, `<!doctype html><html lang="fr"><body><p>bonjour</p></body></html>`)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))

	// Rediscovery must not cost a second fetch.
	for range 3 {
		h.enqueue(t, s.pageURL(path))
		h.crawl(t)
	}

	if got := s.hitsOn(path); got != 1 {
		t.Errorf("a non-English page was fetched %d times, want 1", got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages, want none", len(got))
	}
	stats := h.state.Stats(hostOf(s.srv.URL))
	if stats[policy.ReasonLanguageNotEnglish] != 1 {
		t.Errorf("language_not_english = %d, want 1 (all: %v)",
			stats[policy.ReasonLanguageNotEnglish], stats)
	}
}

// TestAnUnparseableBodyIsDroppedAndCounted is the other, and it needed a parser
// that fails on demand to be reachable at all. It is the sharpest version of the
// same failure: the fetch succeeded, so nothing in the transport layer objects, and
// if the loop just returns then the URL is still a live candidate forever.
func TestAnUnparseableBodyIsDroppedAndCounted(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/broken"
	s.serve(path, "<html><body>")

	h := newHarness(t, s,
		failingParser{err: errors.New("unexpected EOF in a <p> element")}, clock)

	for range 3 {
		h.enqueue(t, s.pageURL(path))
		h.crawl(t)
	}

	if got := s.hitsOn(path); got != 1 {
		t.Errorf("an unparseable page was fetched %d times, want 1", got)
	}
	stats := h.state.Stats(hostOf(s.srv.URL))
	if stats[policy.ReasonBodyUnparseable] != 1 {
		t.Errorf("body_unparseable = %d, want 1 (all: %v)",
			stats[policy.ReasonBodyUnparseable], stats)
	}
	// And the failure is not recorded as a fetch failure, which is what it used to
	// be: it did not fail to be fetched.
	if stats[policy.ReasonServerError] != 0 || stats[policy.ReasonTimeout] != 0 {
		t.Errorf("an unparseable body was recorded as a transport failure: %v", stats)
	}
}

// TestASuccessfulPageIsPersistedOnceAndPublished is the positive case, and it also
// pins the two things the loop still owns: the page goes to the store, and a job
// goes to the indexer.
func TestASuccessfulPageIsPersistedOnceAndPublished(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	pages := h.store.persisted()
	if len(pages) != 1 {
		t.Fatalf("persisted %d pages, want 1", len(pages))
	}
	if pages[0].URL != s.pageURL(path) {
		t.Errorf("persisted URL = %q, want %q", pages[0].URL, s.pageURL(path))
	}
	if pages[0].StatusCode != 200 {
		t.Errorf("persisted StatusCode = %d, want 200: the loop was handing the store "+
			"a page with no status", pages[0].StatusCode)
	}

	// The host record came from the policy manager, not from the loop's own
	// construction. A loop that made one up would carry a budget of ten pages.
	hosts := h.store.persistedHosts()
	if len(hosts) != 1 || hosts[0] != hostOf(s.srv.URL) {
		t.Errorf("persisted host = %v, want [%s]: the host record has to be the one "+
			"the policy manager resolved", hosts, hostOf(s.srv.URL))
	}
}

// TestAnOutcomeThatCouldNotBeRecordedIsNotPersisted is the third case, and the one
// the other two do not reach.
//
// Both neighbouring tests exercise the happy paths: the page persisted, or the
// database refusing it. Neither asks what happens when the *policy* write fails --
// Redis goes away between the fetch and the classification, which is a routine
// thing for a cache to do and not a rare one.
//
// The answer has to be that nothing is persisted and nothing is published, even
// though the fetch succeeded and the body parsed. Classify returning an error means
// the page's state and the host's counters never moved: nobody recorded that this
// URL was crawled, or that the host served it. Publishing it anyway would hand the
// indexer a page the crawl has no record of, and the budget and the stats would both
// be quietly wrong for as long as the Redis outage lasted.
func TestAnOutcomeThatCouldNotBeRecordedIsNotPersisted(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	ch := &fakeChannel{}
	h := newHarness(t, s, nil, clock)
	h.spider.mq = &queueOpening{channel: ch}

	// Everything else works, including taking the URL off the frontier and the fetch
	// itself. Only the write that records the outcome fails.
	h.state.FailOn = map[string]error{"RecordSuccess": errors.New("redis went away")}

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 1 {
		t.Fatalf("the page was fetched %d times, want 1: this test is about what "+
			"happens after the fetch, and a fetch that did not happen tests nothing",
			got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages for an outcome nothing recorded, want none", len(got))
	}
	if got := ch.messages(); len(got) != 0 {
		t.Errorf("published %v for an outcome nothing recorded, want nothing", got)
	}
}

// TestAPageThatFailsToPersistStaysCrawlable is the asymmetry, and it is deliberate.
//
// Classify already counted the page and cleared its retries when the fetch
// succeeded. The store marks it visited only once the insert commits, so a database
// error leaves it crawlable -- which is what should happen, because the alternative
// is a database outage silently deleting every page the crawl fetched in the window.
func TestAPageThatFailsToPersistStaysCrawlable(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	ch := &fakeChannel{}
	h := newHarness(t, s, nil, clock)
	h.spider.mq = &queueOpening{channel: ch}
	h.spider.store = &fakeStore{pageID: uuid.Nil()}

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	// The published message is the assertion that carries weight. The visited check
	// below would pass for free: nothing in this loop marks a URL visited, the store
	// does, and this store is a fake that never does. A test whose only assertion is
	// trivially true is worse than no test, because it looks like coverage.
	if got := ch.messages(); len(got) != 0 {
		t.Errorf("published %v for a page that was never stored, want nothing: "+
			"the downstream indexer is told to crawl a page id that does not exist",
			got)
	}

	visited, err := h.state.IsVisited(context.Background(), s.pageURL(path))
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("a page that was never stored was marked visited; a database outage " +
			"would then delete every page fetched during it, silently and for ever")
	}
}

// TestTheLoopDoesNotFetchAPageTwiceWhenItIsAlreadyVisited is the cheap version of
// the same idea: the loop's own gate catches it before any request.
func TestTheLoopDoesNotFetchAPageTwiceWhenItIsAlreadyVisited(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)

	// Already visited, as it would be after a successful crawl.
	if err := h.state.MarkVisited(context.Background(), s.pageURL(path)); err != nil {
		t.Fatal(err)
	}
	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("an already-visited URL was fetched %d times, want 0", got)
	}
}

// TestTheUserAgentOnTheWireIsTheCrawlersOwnName ties the loop to the robots match.
// A site that writes "Disallow: /" for this crawler and sees a different token on
// the wire is a site being crawled against its instructions, and neither half of
// that is visible from one side.
func TestTheUserAgentOnTheWireIsTheCrawlersOwnName(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	const ua = "BoogleBot/1.0 (+https://boogle.test/bot)"
	h := newHarnessWithConfig(t, s, nil, clock, func(cfg *policy.Config) {
		cfg.UserAgent = ua
	})

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.userAgentOn(path); got != ua {
		t.Errorf("User-Agent = %q, want %q", got, ua)
	}
}

// TestAnUnreachablePolicyStoreFetchesNothing is the fail-closed rule at the level
// that matters.
//
// Redis is what the loop asks whether a URL may be fetched. If the answer cannot be
// had, the answer is no -- and "no" here has to mean no *request*, not "the check
// was skipped and the fetch proceeded".
func TestAnUnreachablePolicyStoreFetchesNothing(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))

	h.state.FailWith = errors.New("redis is unreachable")
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("the page was fetched %d times with the policy store unreachable, "+
			"want 0: a gate that cannot answer must not answer yes", got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages, want none", len(got))
	}
}

// TestAGateThatFailsWhereTheGateIsAsked fetches the sharper version of the test
// above, and the reason to have both.
//
// FailWith takes the whole store down, so the frontier is unreachable too: the URL
// is never taken off it and the loop never reaches the gate at all. That passes for
// the right-looking reason -- nothing was fetched -- while testing nothing about
// what the loop does when Admit itself is the thing that fails.
//
// Here only IsVisited fails, which is the first thing Admit asks. PopFrontier still
// works, so the URL is genuinely taken, genuinely fetched-in-waiting, and the only
// thing standing between it and a request is the gate's handling of its own error.
func TestAGateThatFailsWhereTheGateIsAsked(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))

	// Admit's first state read fails, and nothing else does.
	h.state.FailOn = map[string]error{"IsVisited": errors.New("one field of redis is unreachable")}

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("the page was fetched %d times when Admit could not answer, want 0: "+
			"a gate that cannot answer must not answer yes", got)
	}
	// Not fetched, and not lost either. The URL was already off the frontier by the
	// time the gate answered, so a deferred URL only comes back if it was parked --
	// and a dropped one is not rediscovered during the outage, because the pages
	// that would rediscover it are gated on the same store. One unreachable field
	// would otherwise cost the crawl its whole queue, and leave no trace.
	if got := len(h.delayed(t)); got != 1 {
		t.Errorf("%d urls are waiting to be retried after an unreadable policy store, "+
			"want 1: the URL was taken off the frontier and has to be put back on the "+
			"delayed set or it is simply gone", got)
	}
}

// TestAnEmptyFrontierDoesNothingAtAll is the idle path. Idle is a definitive "there
// is nothing to do", and treating it as a failure to try again would spin twenty
// workers against an empty ZSET for the whole life of the process.
func TestAnEmptyFrontierDoesNothingAtAll(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)

	h := newHarness(t, s, nil, clock)
	h.crawl(t)

	if got := s.hitsOn("/robots.txt"); got != 0 {
		t.Errorf("robots.txt was fetched %d times with an empty frontier, want 0", got)
	}
	if got := h.store.persisted(); len(got) != 0 {
		t.Errorf("persisted %d pages, want none", len(got))
	}
}

func TestTheMessagingChannelIsClosedAfterEveryRound(t *testing.T) {
	// A leaked AMQP channel is a leaked socket and a leaked consumer slot. Twenty
	// workers at one round a second is twenty channels a second.
	ch := &fakeChannel{}
	mq := &queueOpening{channel: ch}

	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	h := newHarness(t, s, nil, clock)
	h.spider.mq = mq

	for range 3 {
		h.crawl(t)
	}

	if got := mq.opens(); got != 3 {
		t.Errorf("opened %d channels for three rounds, want 3", got)
	}
	if !ch.closed {
		t.Error("the channel was not closed at the end of the round")
	}
}

// TestAnUnopenableMessagingChannelDoesNotFetch covers the ordering. The channel is
// opened before the URL is taken, so a broker outage cannot consume frontier
// entries -- and dropping them would be a silent deletion of the crawl queue.
func TestAnUnopenableMessagingChannelDoesNotFetch(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)
	h.spider.mq = &queueOpening{err: errors.New("broker is down")}
	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("the page was fetched %d times with no messaging channel, want 0", got)
	}
	remaining := h.state.Frontier()
	if _, ok := remaining[s.pageURL(path)]; !ok {
		t.Error("the URL left the frontier despite the round being abandoned")
	}
}

// queueOpening is a MessagingQueue whose channel can be made to fail, and which
// counts its opens.
type queueOpening struct {
	channel *fakeChannel
	err     error

	mu   sync.Mutex
	open int
}

func (q *queueOpening) NewMessagingChannel() (messaging.MessagingChannel, error) {
	if q.err != nil {
		return nil, q.err
	}
	q.mu.Lock()
	q.open++
	q.mu.Unlock()
	return q.channel, nil
}

func (q *queueOpening) DeclareQueue(string) error { return nil }
func (q *queueOpening) Close()                    {}

func (q *queueOpening) opens() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.open
}

// hostOf is the loop-side spelling of the host a URL belongs to, port included: the
// policy manager files its records under host:port, because that is also what the
// robots.txt URL is built from.
//
// Written out here rather than shared with the policy package on purpose. The tests
// use it to check that the loop and the policy layer agree about which site a page
// belongs to, and a shared helper would make that agreement true by construction --
// which is the one thing these assertions are for.
func hostOf(raw string) string {
	trimmed := raw
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		trimmed = u.Host
	} else {
		for _, prefix := range []string{"http://", "https://"} {
			trimmed = strings.TrimPrefix(trimmed, prefix)
		}
		if i := strings.IndexByte(trimmed, '/'); i >= 0 {
			trimmed = trimmed[:i]
		}
	}
	return strings.ToLower(trimmed)
}
