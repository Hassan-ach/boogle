package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/state"
	"uuid"
)

type fakeDB struct {
	pageID uuid.UUID
	err    error

	urlsErr     error
	edgesErr    error
	returnedIDs map[string]string

	withTxCalls int
	inserted    []string
	edgesFrom   string
	edgesTo     []string
}

func (f *fakeDB) InsertPage(ctx context.Context, tx *sql.Tx, page *entity.Page) (uuid.UUID, string, error) {
	if f.err != nil {
		return uuid.Nil(), "", f.err
	}
	return f.pageID, "page-url-id", nil
}

func (f *fakeDB) InsertGraphEdges(ctx context.Context, tx *sql.Tx, from string, to []string) error {
	if f.edgesErr != nil {
		return f.edgesErr
	}
	f.edgesFrom = from
	f.edgesTo = to
	return f.err
}

func (f *fakeDB) InsertURLs(ctx context.Context, tx *sql.Tx, urls []string) (map[string]string, error) {
	if f.urlsErr != nil {
		return nil, f.urlsErr
	}
	if f.err != nil {
		return nil, f.err
	}
	f.inserted = urls
	if f.returnedIDs != nil {
		return f.returnedIDs, nil
	}
	ids := make(map[string]string, len(urls))
	for i, u := range urls {
		// Deliberately not derived from position: the caller must map by URL.
		ids[u] = fmt.Sprintf("id-%d", len(urls)-i)
	}
	return ids, nil
}

func (f *fakeDB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	f.withTxCalls++
	return fn(nil)
}

func (f *fakeDB) Close() {}

type fakeCache struct{}

func (c *fakeCache) Close() {}

func newTestStore(t *testing.T) (*Store, *state.MemoryState) {
	t.Helper()
	st := state.NewMemoryState()
	return &Store{
		db:    &fakeDB{pageID: uuid.New()},
		cache: &fakeCache{},
		st:    st,
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

func TestPersistPageMapsEdgesByURLNotByPosition(t *testing.T) {
	s, _ := newTestStore(t)
	db := s.db.(*fakeDB)

	page := &entity.Page{
		URL:   "https://example.com/page",
		Links: []string{"https://example.com/a", "https://example.com/b"},
	}
	if _, err := s.persistPage(context.Background(), page); err != nil {
		t.Fatal(err)
	}

	if db.withTxCalls != 1 {
		t.Errorf("persist used %d transactions, want 1", db.withTxCalls)
	}
	if db.edgesFrom != "page-url-id" {
		t.Errorf("edges start at %q, want the page's URL id", db.edgesFrom)
	}
	// fakeDB returns reversed ids, so a positional implementation would attach
	// the wrong URL to every edge. Assert order, not set membership.
	want := []string{"id-2", "id-1"}
	if len(db.edgesTo) != len(want) {
		t.Fatalf("inserted %d edges, want %d: %v", len(db.edgesTo), len(want), db.edgesTo)
	}
	for i := range want {
		if db.edgesTo[i] != want[i] {
			t.Errorf("edge %d target = %q, want %q (order follows page.Links)",
				i, db.edgesTo[i], want[i])
		}
	}
}

func TestPersistPageFailsWhenALinkHasNoURLID(t *testing.T) {
	s, _ := newTestStore(t)
	db := s.db.(*fakeDB)
	db.returnedIDs = map[string]string{"https://example.com/a": "id-a"}

	page := &entity.Page{
		URL:   "https://example.com/page",
		Links: []string{"https://example.com/a", "https://example.com/missing"},
	}
	if _, err := s.persistPage(context.Background(), page); err == nil {
		t.Fatal("persistPage silently dropped a link with no id")
	}
}

func TestPersistPageRollsBackTheWholeTransactionOnALinkFailure(t *testing.T) {
	for name, configure := range map[string]func(*fakeDB){
		"urls":  func(db *fakeDB) { db.urlsErr = errors.New("url upsert failed") },
		"edges": func(db *fakeDB) { db.edgesErr = errors.New("edge insert failed") },
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore(t)
			db := s.db.(*fakeDB)
			configure(db)

			page := &entity.Page{URL: "https://example.com/page", Links: []string{"https://example.com/a"}}
			if _, err := s.persistPage(context.Background(), page); err == nil {
				t.Fatal("persistPage reported success")
			}
			if db.withTxCalls != 1 {
				t.Errorf("persist used %d transactions, want exactly 1", db.withTxCalls)
			}
		})
	}
}

func TestPersistPageSkipsASelfLink(t *testing.T) {
	s, _ := newTestStore(t)
	db := s.db.(*fakeDB)

	page := &entity.Page{
		URL:   "https://example.com/page",
		Links: []string{"https://example.com/page", "https://example.com/a"},
	}
	if _, err := s.persistPage(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	if len(db.edgesTo) != 1 {
		t.Errorf("inserted %d edges for one link plus a self-link, want 1", len(db.edgesTo))
	}
}
