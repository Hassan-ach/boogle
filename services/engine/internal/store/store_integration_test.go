//go:build integration

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	"github.com/Hassan-ach/boogle/services/engine/internal/config/store"

	_ "github.com/lib/pq"
)

func dsn() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://admin:1234@localhost:5432/boogle_test?sslmode=disable"
}

func connect(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("postgres", dsn())
	if err != nil {
		t.Skipf("cannot open %s: %v", dsn(), err)
	}
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		t.Skipf("no database at %s: %v", dsn(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func storeConfig(t *testing.T) store.StoreConfig {
	t.Helper()
	u, err := url.Parse(dsn())
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL is not a URL: %v", err)
	}
	port := 5432
	if p := u.Port(); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
			t.Fatalf("bad port %q: %v", p, err)
		}
	}
	password, _ := u.User.Password()
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		name = "boogle_test"
	}
	return store.StoreConfig{
		DB: store.PsqlConfig{
			Host:         u.Hostname(),
			Port:         port,
			User:         u.User.Username(),
			Password:     password,
			DBName:       name,
			MaxOpenConns: 4,
			MaxIdleConns: 4,
		},
		PageSize: 20,
	}
}

func newStore(t *testing.T) PsqlStore {
	t.Helper()
	connect(t)
	return NewStore(storeConfig(t))
}

type page struct {
	urlID  string
	pageID string
}

func seed(t *testing.T, conn *sql.DB, name string, rank float64) page {
	t.Helper()
	u := fmt.Sprintf("https://%s-%s.test/", name, strings.ReplaceAll(t.Name(), "/", "-"))
	var urlID string
	if err := conn.QueryRow(
		"INSERT INTO urls (url) VALUES ($1) RETURNING id", u,
	).Scan(&urlID); err != nil {
		t.Fatalf("insert url: %v", err)
	}

	meta, err := json.Marshal(map[string]any{
		"url":         u,
		"title":       "seeded " + name,
		"description": "a page seeded by an integration test",
		"keywords":    []string{"seeded", name},
		"crawledAt":   "2026-01-02T03:04:05Z",
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	var pageID string
	if err := conn.QueryRow(
		"INSERT INTO pages (url_id, html, metadata, indexed) VALUES ($1, $2, $3, TRUE) RETURNING id",
		urlID, "<html><body>seed</body></html>", meta,
	).Scan(&pageID); err != nil {
		t.Fatalf("insert page: %v", err)
	}

	if _, err := conn.Exec(
		"INSERT INTO page_rank (url_id, score) VALUES ($1, $2) ON CONFLICT (url_id) DO UPDATE SET score = EXCLUDED.score",
		urlID, rank,
	); err != nil {
		t.Fatalf("insert rank: %v", err)
	}

	t.Cleanup(func() {
		_, _ = conn.Exec("DELETE FROM urls WHERE id = $1", urlID)
	})
	return page{urlID: urlID, pageID: pageID}
}

func indexWords(t *testing.T, conn *sql.DB, p page, tf map[string]int) {
	t.Helper()
	for word, count := range tf {
		var wordID string
		err := conn.QueryRow(
			"INSERT INTO words (word) VALUES ($1) ON CONFLICT (word) DO UPDATE SET word = EXCLUDED.word RETURNING id",
			word,
		).Scan(&wordID)
		if err != nil {
			t.Fatalf("insert word %q: %v", word, err)
		}
		if _, err := conn.Exec(
			"INSERT INTO page_word (page_id, word_id, tf) VALUES ($1, $2, $3) ON CONFLICT (page_id, word_id) DO UPDATE SET tf = EXCLUDED.tf",
			p.pageID, wordID, count,
		); err != nil {
			t.Fatalf("link word %q: %v", word, err)
		}
	}
}

func TestGetDataReturnsTheMatchingPageWithItsWords(t *testing.T) {
	conn := connect(t)
	s := newStore(t)

	p := seed(t, conn, "match", 0.5)
	indexWords(t, conn, p, map[string]int{"pagerank": 4, "postgres": 2})

	data, err := s.GetData(context.Background(), []string{"pagerank", "postgres"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}

	var found bool
	for _, got := range data.Pages {
		if got.ID.String() != p.pageID {
			continue
		}
		found = true
		if got.URL == "" {
			t.Error("page came back with an empty URL")
		}
		if got.PRScore != 0.5 {
			t.Errorf("PRScore = %v, want 0.5", got.PRScore)
		}
		if got.Words["pagerank"] != 4 {
			t.Errorf("tf for pagerank = %d, want 4", got.Words["pagerank"])
		}
		if got.Words["postgres"] != 2 {
			t.Errorf("tf for postgres = %d, want 2", got.Words["postgres"])
		}
		if got.MetaData.Title != "seeded match" {
			t.Errorf("MetaData.Title = %q, want %q", got.MetaData.Title, "seeded match")
		}
		if len(got.MetaData.Keywords) != 2 {
			t.Errorf("MetaData.Keywords = %v, want 2 entries", got.MetaData.Keywords)
		}
		if got.MetaData.CrawledAt.IsZero() {
			t.Error("MetaData.CrawledAt did not decode")
		}
	}
	if !found {
		t.Fatalf("the seeded page was not among the %d results", len(data.Pages))
	}
}

func TestGetDataExcludesPagesThatMatchNoQueryWord(t *testing.T) {
	conn := connect(t)
	s := newStore(t)

	hit := seed(t, conn, "hit", 0.9)
	indexWords(t, conn, hit, map[string]int{"needle": 1})
	miss := seed(t, conn, "miss", 0.9)
	indexWords(t, conn, miss, map[string]int{"haystack": 1})

	data, err := s.GetData(context.Background(), []string{"needle"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}

	for _, got := range data.Pages {
		if got.ID.String() == miss.pageID {
			t.Fatalf("a page matching none of the query words was returned: %s", got.URL)
		}
	}
	if len(data.Pages) == 0 {
		t.Fatal("the page that does match was not returned")
	}
}

func TestGetDataOrdersByWordCountThenByPageRank(t *testing.T) {
	conn := connect(t)
	s := newStore(t)

	both := seed(t, conn, "both", 0.10)
	indexWords(t, conn, both, map[string]int{"alpha": 1, "beta": 1})

	oneHigh := seed(t, conn, "one-high", 0.90)
	indexWords(t, conn, oneHigh, map[string]int{"alpha": 1})

	oneLow := seed(t, conn, "one-low", 0.20)
	indexWords(t, conn, oneLow, map[string]int{"beta": 1})

	data, err := s.GetData(context.Background(), []string{"alpha", "beta"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}

	pos := map[string]int{}
	for i, got := range data.Pages {
		switch got.ID.String() {
		case both.pageID:
			pos["both"] = i
		case oneHigh.pageID:
			pos["oneHigh"] = i
		case oneLow.pageID:
			pos["oneLow"] = i
		}
	}
	for _, k := range []string{"both", "oneHigh", "oneLow"} {
		if _, ok := pos[k]; !ok {
			t.Fatalf("%s is missing from the results", k)
		}
	}
	if pos["both"] > pos["oneHigh"] {
		t.Errorf("the page matching both words ranked below one matching a single word")
	}
	if pos["oneHigh"] > pos["oneLow"] {
		t.Errorf("a higher PageRank ranked below a lower one: %v", pos)
	}
}

func TestGetDataCollectsTheIdfOfEveryWordItReturns(t *testing.T) {
	conn := connect(t)
	s := newStore(t)

	p := seed(t, conn, "idf", 0.5)
	indexWords(t, conn, p, map[string]int{"weighted": 1, "unweighted": 1})

	if _, err := conn.Exec(
		"UPDATE words SET idf = 2.5 WHERE word = 'weighted'",
	); err != nil {
		t.Fatalf("set idf: %v", err)
	}

	data, err := s.GetData(context.Background(), []string{"weighted", "unweighted"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}

	if got, ok := data.Idf["weighted"]; !ok || got != 2.5 {
		t.Errorf("Idf[\"weighted\"] = %v (present=%t), want 2.5", got, ok)
	}
	if _, ok := data.Idf["unweighted"]; !ok {
		t.Error("a word on a matching page is missing from the Idf map")
	}
}

func TestGetDataWithNoWordsReturnsNothing(t *testing.T) {
	conn := connect(t)
	s := newStore(t)
	seed(t, conn, "emptyquery", 0.5)

	data, err := s.GetData(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if len(data.Pages) != 0 {
		t.Errorf("a query with no words returned %d pages", len(data.Pages))
	}
}

func TestGetDataPagesThroughTheResultSet(t *testing.T) {
	conn := connect(t)

	conf := storeConfig(t)
	conf.PageSize = 2
	paged := NewStore(conf)

	for i := 0; i < 5; i++ {
		p := seed(t, conn, fmt.Sprintf("paged%d", i), 0.5)
		indexWords(t, conn, p, map[string]int{"pageme": 1})
	}

	var seen []string
	for pageNum := 0; pageNum < 3; pageNum++ {
		data, err := paged.GetData(context.Background(), []string{"pageme"}, pageNum)
		if err != nil {
			t.Fatalf("GetData page %d: %v", pageNum, err)
		}
		if len(data.Pages) > conf.PageSize {
			t.Errorf("page %d returned %d rows, more than PageSize=%d",
				pageNum, len(data.Pages), conf.PageSize)
		}
		for _, p := range data.Pages {
			seen = append(seen, p.ID.String())
		}
	}

	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Errorf("page %v appeared on two pages: %v", id, seen)
		}
		unique[id] = true
	}
	if len(seen) == 0 {
		t.Error("paging through the result set returned nothing at all")
	}
}

func TestGetTotalPagesCountsEveryMatchNotJustOnePage(t *testing.T) {
	conn := connect(t)
	s := newStore(t)

	p := seed(t, conn, "counted", 0.5)
	indexWords(t, conn, p, map[string]int{"countme": 1})

	total, err := s.GetTotalPages(context.Background(), []string{"countme"})
	if err != nil {
		t.Fatalf("GetTotalPages: %v", err)
	}
	if total < 1 {
		t.Errorf("GetTotalPages = %d, want at least 1 for a matching page", total)
	}
}

func TestGetTotalPagesIsZeroWhenNothingMatches(t *testing.T) {
	s := newStore(t)

	total, err := s.GetTotalPages(context.Background(), []string{"nothingmatchesthisword"})
	if err != nil {
		t.Fatalf("GetTotalPages: %v", err)
	}
	if total != 0 {
		t.Errorf("GetTotalPages = %d, want 0", total)
	}
}

func TestGetTotalPagesWithNoWordsIsZero(t *testing.T) {
	s := newStore(t)

	total, err := s.GetTotalPages(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTotalPages: %v", err)
	}
	if total != 0 {
		t.Errorf("GetTotalPages with no query words = %d, want 0", total)
	}
}

func TestGetDataReportsQueryFailuresAsAnAppError(t *testing.T) {
	connect(t)
	s := newStore(t)

	if err := s.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := s.GetData(context.Background(), []string{"anything"}, 0)
	if err == nil {
		t.Fatal("GetData succeeded against a closed pool")
	}

	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error is %T, want *apperror.AppError", err)
	}
	if appErr.Code != http.StatusInternalServerError {
		t.Errorf("Code = %d, want %d", appErr.Code, http.StatusInternalServerError)
	}

	if got := err.Error(); got != "internal server error" {
		t.Errorf("rendered error = %q, want the generic message", got)
	}

	if appErr.Err == nil {
		t.Fatal("AppError.Err is nil; the cause is lost and the 500 is unexplainable")
	}
	if !errors.Is(err, appErr.Err) {
		t.Error("errors.Is cannot reach the cause through the AppError")
	}
	if !strings.Contains(appErr.Err.Error(), "failed to execute query") {
		t.Errorf("cause = %q, want it to name the failed query", appErr.Err)
	}
}

func TestGetTotalPagesReportsQueryFailuresAsAnAppError(t *testing.T) {
	connect(t)
	s := newStore(t)

	if err := s.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := s.GetTotalPages(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("GetTotalPages succeeded against a closed pool")
	}

	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error is %T, want *apperror.AppError", err)
	}
	if got := err.Error(); got != "internal server error" {
		t.Errorf("rendered error = %q, want the generic message", got)
	}
}
