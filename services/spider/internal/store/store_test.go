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

func TestStoreInitDoesNotSeedOverExistingWork(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()

	starters := []string{"https://example.com/", "https://example.org/"}
	if err := s.Init(starters); err != nil {
		t.Fatal(err)
	}

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

func TestStoreInitFailsClosedOnAnUnreadableFrontier(t *testing.T) {
	s, st := newTestStore(t)
	st.FailOn = map[string]error{"FrontierLen": errors.New("redis is unreachable")}

	if err := s.Init([]string{"https://example.com/"}); err == nil {
		t.Fatal("Init reported success with an unreadable frontier")
	}
	if got := len(st.Frontier()); got != 0 {
		t.Errorf("frontier holds %d urls after a failed read; the start urls were "+
			"seeded onto a frontier that could not be confirmed empty", got)
	}
}

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
