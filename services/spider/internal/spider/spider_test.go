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

type fakeStore struct {
	mu    sync.Mutex
	pages []*entity.Page
	hosts []string

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

type failingParser struct {
	err error
}

func (p failingParser) ParseHTML(io.Reader, string) (*entity.Page, error) {
	return nil, p.err
}

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

func (s *site) pageURL(path string) string {
	return s.srv.URL + path
}

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

func newHarness(t *testing.T, s *site, p htmlParser, clock *testClock) *harness {
	t.Helper()
	return newHarnessWithConfig(t, s, p, clock, nil)
}

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

func (h *harness) crawl(t *testing.T) {
	t.Helper()
	h.spider.crawl(0)
}

func (h *harness) enqueue(t *testing.T, urls ...string) {
	t.Helper()
	if err := h.state.Enqueue(context.Background(), urls...); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) frontierLen(t *testing.T) int64 {
	t.Helper()
	n, err := h.state.FrontierLen(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) delayed(t *testing.T) map[string]time.Time {
	t.Helper()
	return h.state.Delayed()
}

const englishPage = `<!doctype html><html lang="en"><head><title>t</title></head>` +
	`<body><p>hello</p></body></html>`

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
	visited, err := h.state.IsVisited(context.Background(), s.pageURL(refused))
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("the refused URL was not retired; it comes back on every rediscovery")
	}
}

func TestADeadHostCostsNothingPerURL(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)

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

	remaining := h.state.Frontier()
	for url := range remaining {
		if strings.Contains(url, host) {
			t.Errorf("%s is still in the frontier after its host was found to be gone", url)
		}
	}
}

func TestADeferredURLIsParkedWithARealDueTime(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	s.serve("/slow", "wait")

	h := newHarness(t, s, nil, clock)

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

	h.enqueue(t, s.pageURL("/other"))
	h.crawl(t)
	if got := s.hitsOn("/other"); got != 0 {
		t.Errorf("a URL on a cooling host was fetched %d times, want 0", got)
	}

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

func TestA404IsTerminal(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const missing = "/gone"

	h := newHarness(t, s, nil, clock)

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

func TestANonEnglishPageIsDroppedAndCounted(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/fr"
	s.serve(path, `<!doctype html><html lang="fr"><body><p>bonjour</p></body></html>`)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))

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
	if stats[policy.ReasonServerError] != 0 || stats[policy.ReasonTimeout] != 0 {
		t.Errorf("an unparseable body was recorded as a transport failure: %v", stats)
	}
}

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

	hosts := h.store.persistedHosts()
	if len(hosts) != 1 || hosts[0] != hostOf(s.srv.URL) {
		t.Errorf("persisted host = %v, want [%s]: the host record has to be the one "+
			"the policy manager resolved", hosts, hostOf(s.srv.URL))
	}
}

func TestAnOutcomeThatCouldNotBeRecordedIsNotPersisted(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	ch := &fakeChannel{}
	h := newHarness(t, s, nil, clock)
	h.spider.mq = &queueOpening{channel: ch}

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

func TestAPageThatFailsToPersistStaysCrawlable(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, `<!doctype html><html lang="en"><head><title>t</title></head>`+
		`<body><a href="/child">child</a></body></html>`)

	ch := &fakeChannel{}
	h := newHarness(t, s, nil, clock)
	h.spider.mq = &queueOpening{channel: ch}
	h.spider.store = &fakeStore{pageID: uuid.Nil()}

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

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

	if got := h.frontierLen(t); got != 0 {
		t.Errorf("frontier holds %d urls after a page that failed to persist was "+
			"expanded; its links are discovered only once the page itself is stored", got)
	}
}

func TestAStoredPageIsRetiredAndItsLinksDiscovered(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/index"
	s.serve(path, `<!doctype html><html lang="en"><head><title>t</title></head><body>`+
		`<a href="/child">child</a>`+
		`<a href="/admin/panel">panel</a>`+
		`<a href="/report.pdf">report</a>`+
		`<a href="/second">second</a>`+
		`</body></html>`)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	visited, err := h.state.IsVisited(context.Background(), s.pageURL(path))
	if err != nil {
		t.Fatal(err)
	}
	if !visited {
		t.Error("a page that was stored was not marked visited, so the next round " +
			"fetches it again and indexes it twice")
	}

	frontier := h.state.Frontier()
	for _, want := range []string{"/child", "/second"} {
		if _, ok := frontier[s.pageURL(want)]; !ok {
			t.Errorf("%s was not discovered; the frontier holds %v", want, frontier)
		}
	}
	for _, refused := range []string{"/admin/panel", "/report.pdf"} {
		if _, ok := frontier[s.pageURL(refused)]; ok {
			t.Errorf("%s reached the frontier, so a page the rules refuse is one "+
				"round trip away from being fetched", refused)
		}
	}

	stats := h.state.Stats(hostOf(s.srv.URL))
	for reason, what := range map[policy.Reason]string{
		policy.ReasonPathDisallowed:   "/admin/panel",
		policy.ReasonExtensionSkipped: "/report.pdf",
	} {
		if stats[reason] == 0 {
			t.Errorf("nothing counted under %q after %s was declined; the stats "+
				"hash is the only record of why a site is under-indexed",
				reason, what)
		}
	}

	pages := h.store.persisted()
	if len(pages) != 1 {
		t.Fatalf("persisted %d pages, want 1", len(pages))
	}
	for _, link := range pages[0].Links {
		if strings.Contains(link, "/admin/") || strings.HasSuffix(link, ".pdf") {
			t.Errorf("the stored page links to %q, which the gate refused; the "+
				"graph would then credit a page the crawl never fetched", link)
		}
	}
}

func TestTheLoopDoesNotFetchAPageTwiceWhenItIsAlreadyVisited(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)

	if err := h.state.MarkVisited(context.Background(), s.pageURL(path)); err != nil {
		t.Fatal(err)
	}
	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("an already-visited URL was fetched %d times, want 0", got)
	}
}

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

func TestAGateThatFailsWhereTheGateIsAsked(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s := newSite(t)
	const path = "/good"
	s.serve(path, englishPage)

	h := newHarness(t, s, nil, clock)
	h.enqueue(t, s.pageURL(path))

	h.state.FailOn = map[string]error{"IsVisited": errors.New("one field of redis is unreachable")}

	h.enqueue(t, s.pageURL(path))
	h.crawl(t)

	if got := s.hitsOn(path); got != 0 {
		t.Errorf("the page was fetched %d times when Admit could not answer, want 0: "+
			"a gate that cannot answer must not answer yes", got)
	}
	if got := len(h.delayed(t)); got != 1 {
		t.Errorf("%d urls are waiting to be retried after an unreadable policy store, "+
			"want 1: the URL was taken off the frontier and has to be put back on the "+
			"delayed set or it is simply gone", got)
	}
}

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
