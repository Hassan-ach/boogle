package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
	"uuid"
)

// The store's use of policy state is where the frontier used to be reached
// through the cache, and the old code there was quietly lossy. So these tests are
// about what the store writes and in what order, not about the store in general.

// fakeDB records the calls persistPage makes. It is hand-written rather than
// generated: five methods, and a mock generator would be a new dependency to fake
// something this small.
type fakeDB struct {
	pageID uuid.UUID
	err    error

	withTxCalls int
	inserted    []string
	edgesFrom   string
	edgesTo     []string
}

func (f *fakeDB) InsertPage(ctx context.Context, tx *sql.Tx, page *entity.Page) (uuid.UUID, error) {
	if f.err != nil {
		return uuid.Nil(), f.err
	}
	return f.pageID, nil
}

func (f *fakeDB) InsertGraphEdges(ctx context.Context, tx *sql.Tx, from string, to []string) error {
	f.edgesFrom = from
	f.edgesTo = to
	return f.err
}

func (f *fakeDB) InsertURLs(ctx context.Context, tx *sql.Tx, urls []string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inserted = urls
	ids := make([]string, len(urls))
	for i := range urls {
		ids[i] = urls[i]
	}
	return ids, nil
}

func (f *fakeDB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	f.withTxCalls++
	return fn(nil)
}

func (f *fakeDB) Close() {}

// fakeCache records host-metadata writes.
type fakeCache struct {
	written map[string]*entity.Host
	err     error
}

func (c *fakeCache) AddHostMetaData(ctx context.Context, h string, host *entity.Host) error {
	if c.err != nil {
		return c.err
	}
	if c.written == nil {
		c.written = map[string]*entity.Host{}
	}
	c.written[h] = host
	return nil
}

func (c *fakeCache) GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error) {
	host, ok := c.written[h]
	return host, ok, nil
}

func (c *fakeCache) Close() {}

func newTestStore(t *testing.T) (*Store, *policy.MemoryState) {
	t.Helper()
	st := policy.NewMemoryState()
	return &Store{
		db:    &fakeDB{pageID: uuid.New()},
		cache: &fakeCache{},
		state: st,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st
}

func TestStoreInitSeedsAnEmptyFrontier(t *testing.T) {
	s, st := newTestStore(t)

	starters := []string{"https://example.com/", "https://example.org/"}
	if err := s.Init(starters); err != nil {
		t.Fatal(err)
	}

	if got := len(st.Frontier()); got != len(starters) {
		t.Errorf("frontier holds %d urls, want %d", got, len(starters))
	}
}

// TestStoreInitDoesNotSeedOverExistingWork is the startup case that matters. A
// second Init -- a restart, a redeploy -- must not re-enqueue the start URLs on
// top of a frontier that already has work, because every URL in it would have its
// priority counted up again and the start URLs would be crawled twice for no
// reason.
func TestStoreInitDoesNotSeedOverExistingWork(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()

	starters := []string{"https://example.com/", "https://example.org/"}
	if err := s.Init(starters); err != nil {
		t.Fatal(err)
	}

	// And a URL already crawled is not work.
	if err := st.Enqueue(ctx, "https://example.net/discovered"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkVisited(ctx, "https://example.net/discovered"); err != nil {
		t.Fatal(err)
	}
	before := len(st.Frontier())

	if err := s.Init(starters); err != nil {
		t.Fatal(err)
	}
	if got := len(st.Frontier()); got != before {
		t.Errorf("frontier holds %d urls after a second Init, want %d", got, before)
	}
	for _, u := range starters {
		if score, ok := st.Frontier()[u]; !ok {
			t.Errorf("%s was dropped from the frontier", u)
		} else if score != 1 {
			t.Errorf("%s has priority %v after a second Init, want 1", u, score)
		}
	}
}

// TestStoreInitFailsClosedOnAnUnreadableFrontier is the bug the old CountUrls had.
//
// It returned 0 on any error, which Init read as "the frontier is empty" and so
// answered "seed it". So a Redis blip at startup -- the one moment Redis is most
// likely to be briefly unavailable -- re-enqueued every start URL on top of a
// frontier that was already full. Nothing failed loudly; the crawl just started
// from the front of the queue with inflated priorities.
//
// Only the length read fails here. If the whole state were broken the seed would
// fail too and the test would pass for the wrong reason: it would be the write
// failing, not the decision to write.
func TestStoreInitFailsClosedOnAnUnreadableFrontier(t *testing.T) {
	s, st := newTestStore(t)
	st.FailOn = map[string]error{"FrontierLen": errors.New("redis is unreachable")}

	if err := s.Init([]string{"https://example.com/"}); err == nil {
		t.Fatal("Init reported success with an unreadable frontier")
	}
	// The seed must not have been attempted, so nothing was written.
	if got := len(st.Frontier()); got != 0 {
		t.Errorf("frontier holds %d urls after a failed read; the start urls were "+
			"seeded onto a frontier that could not be confirmed empty", got)
	}
}

// TestStorePersistPageDecidesNothingAboutTheCrawl is the test that pins where the
// crawl decisions are not.
//
// This function used to end by marking the page visited and enqueueing its links,
// and both were crawl decisions made by a database layer. A store that writes rows
// is not entitled to decide when a URL has been seen or what the crawl should look
// at next, and doing it here had costs that only showed in aggregate: nothing
// counted a refusal, because nothing here could refuse; the decision could not be
// retried, observed or reasoned about from anywhere except the function that
// happened to insert the row; and a URL the gate would have refused still cost a
// Redis write on its way to being discarded at the pop.
//
// So the loop does both now, through the policy manager, after this returns and only
// if the insert committed. What is left here is a row.
//
// The properties those two writes carried are not lost with them. A self-linking
// page is still crawled once and not twice --
// TestTheLoopDoesNotFetchAPageTwiceWhenItIsAlreadyVisited, at the layer that now
// owns it -- and a visited link is still queued and then dropped at the pop --
// TestDiscoverQueuesAURLItHasAlreadyCrawled, likewise.
func TestStorePersistPageDecidesNothingAboutTheCrawl(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()

	const url = "https://example.com/page"
	page := &entity.Page{
		URL:   url,
		Links: []string{"https://example.com/a", url, "https://example.com/b"},
	}

	if _, err := s.persistPage(ctx, page); err != nil {
		t.Fatal(err)
	}

	visited, err := st.IsVisited(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if visited {
		t.Error("the store marked a stored page visited; the loop does that, once " +
			"the insert has committed and through a call that can be counted")
	}
	if got := len(st.Frontier()); got != 0 {
		t.Errorf("the store enqueued %d links; the loop does that, and only the "+
			"links policy.AdmitLinks approved", got)
	}
}

// TestStorePersistPageStopsAtAFailedInsert checks that a database failure does not
// mark the page visited. The reverse order would lose the page: marked visited,
// not persisted, and never crawled again.
func TestStorePersistPageStopsAtAFailedInsert(t *testing.T) {
	s, st := newTestStore(t)
	s.db = &fakeDB{err: errors.New("disk is full")}
	ctx := context.Background()

	page := &entity.Page{URL: "https://example.com/page", Links: []string{"https://example.com/a"}}
	if _, err := s.persistPage(ctx, page); err == nil {
		t.Fatal("persistPage reported success on a failed insert")
	}

	if visited, err := st.IsVisited(ctx, page.URL); err != nil {
		t.Fatal(err)
	} else if visited {
		t.Error("a page that was never persisted is marked visited; it is lost for good")
	}
	if got := len(st.Frontier()); got != 0 {
		t.Errorf("frontier holds %d urls after a failed persist, want 0", got)
	}
}

// TestStorePersistHostNoLongerWritesAWaitedMarker pins the removal of a write that
// nothing read.
//
// AddToWaitedHost set a TTL key that no code path consulted -- the spider had no
// rate limiting at all, and the Crawl-delay it parsed from robots.txt went into the
// same dead field. A write that nothing reads is worse than no write: it looks
// like rate limiting in a code review, and it is why the crawl-delay bug survived
// so long.
func TestStorePersistHostNoLongerWritesAWaitedMarker(t *testing.T) {
	s, _ := newTestStore(t)

	host := &entity.Host{Name: "example.com", Delay: 5}
	s.persistHost(context.Background(), host)

	cache, ok := s.cache.(*fakeCache)
	if !ok {
		t.Fatal("unexpected cache type")
	}
	if _, ok := cache.written["example.com"]; !ok {
		t.Error("host metadata was not written")
	}
}

func TestStoreGetHostMetaData(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	host := &entity.Host{Name: "example.com", Delay: 5}
	s.persistHost(ctx, host)

	got, found, err := s.GetHostMetaData(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("host metadata was not found")
	}
	if got.Delay != 5 {
		t.Errorf("Delay = %d, want 5", got.Delay)
	}

	if _, found, err := s.GetHostMetaData(ctx, "absent.com"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("a host that was never written was reported as found")
	}
}
