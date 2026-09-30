package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// The search handler is where an internal failure turns into a response body, so
// it is the boundary these tests are about. The previous version of every error
// path here was:
//
//     return c.String(http.StatusInternalServerError, fmt.Sprint("err: %w", err))
//
// Two things were wrong with that. `%w` is a verb for fmt.Errorf, not for
// fmt.Sprint, so Sprint emitted it literally and the response body began with
// "err: %w". And the driver error was spliced in whole, host and port included:
// a database that was down produced a 500 whose body told the user exactly which
// host and port it could not reach. The AppError type exists specifically to keep
// that text internal, and writing err.Error() into the response threw it away.

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeStore struct {
	totalPages int
	data       *store.Data

	totalErr error
	dataErr  error

	// recorded arguments
	gotWords  []string
	gotPage   int
	totalCall int
	dataCall  int
}

func (f *fakeStore) GetTotalPages(_ context.Context, q []string) (int, error) {
	f.totalCall++
	f.gotWords = q
	if f.totalErr != nil {
		return 0, f.totalErr
	}
	return f.totalPages, nil
}

func (f *fakeStore) GetData(_ context.Context, words []string, pageNum int) (*store.Data, error) {
	f.dataCall++
	f.gotWords = words
	f.gotPage = pageNum
	if f.dataErr != nil {
		return nil, f.dataErr
	}
	return f.data, nil
}

type fakeRanker struct {
	pages []*model.Page
	err   error
}

func (f *fakeRanker) Rank(*store.Data) ([]*model.Page, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pages, nil
}

type fakeSpeller struct{ suggestions []string }

func (f *fakeSpeller) GetSuggestions(string) []string { return f.suggestions }

// ── helpers ─────────────────────────────────────────────────────────────────

func newTestHandler(s store.Store, r *fakeRanker, sp *fakeSpeller) *SearchingHandler {
	return NewSearchHandler(s, r, sp)
}

// call runs the handler the way echo would: on an error it is handed to
// HandleError, which is the whole point of returning it rather than writing it.
func call(t *testing.T, h *SearchingHandler, target string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.HTTPErrorHandler = HandleError

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)

	e.GET("/", func(c *echo.Context) error { return h.Handle(c) })
	e.ServeHTTP(rec, req)
	return rec
}

// a query failure carrying the sort of detail that must never reach a browser.
var dbIsDown = apperror.Internal(errors.New(
	"dial tcp 127.0.0.1:5432: connect: connection refused (password=hunter2)",
))

// ── error paths ─────────────────────────────────────────────────────────────

// TestASearchFailureDoesNotLeakTheDatabaseError is the regression test for the
// whole handler.
func TestASearchFailureDoesNotLeakTheDatabaseError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *fakeStore
	}{
		{"counting the results", &fakeStore{totalErr: dbIsDown}},
		{"fetching the results", &fakeStore{dataErr: dbIsDown}},
		{"ranking the results", &fakeStore{totalPages: 1, data: &store.Data{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRanker{}
			if tc.name == "ranking the results" {
				r.err = dbIsDown
			}
			rec := call(t, newTestHandler(tc.store, r, &fakeSpeller{}), "/?query=hello")

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}

			body := rec.Body.String()
			// The two things that actually leaked before.
			if strings.Contains(body, "%w") {
				t.Errorf("response body contains a literal %%w: %q", body)
			}
			for _, secret := range []string{
				"connection refused",
				"127.0.0.1",
				"5432",
				"hunter2",
				"dial tcp",
			} {
				if strings.Contains(body, secret) {
					t.Errorf("response body leaks %q: %q", secret, body)
				}
			}
		})
	}
}

// The rendered page still has to say something. A 500 with an empty body is just
// as broken as one full of internals, from the user's point of view.
func TestASearchFailureRendersTheErrorPage(t *testing.T) {
	h := newTestHandler(&fakeStore{totalErr: dbIsDown}, &fakeRanker{}, &fakeSpeller{})
	rec := call(t, h, "/?query=hello")

	body := strings.TrimSpace(rec.Body.String())
	if body == "" {
		t.Fatal("the 500 response has an empty body")
	}
	if !strings.Contains(strings.ToLower(body), "error") &&
		!strings.Contains(strings.ToLower(body), "500") {
		t.Errorf("the error page says nothing recognisable: %q", body)
	}
}

// An error must reach HandleError, not be swallowed into a 200. A handler that
// returned nil after a failure would render an empty result page, which is
// indistinguishable from a search that genuinely matched nothing.
func TestASearchFailureIsNotReportedAsAnEmptyResultPage(t *testing.T) {
	h := newTestHandler(&fakeStore{dataErr: dbIsDown}, &fakeRanker{}, &fakeSpeller{})
	rec := call(t, h, "/?query=hello")

	if rec.Code == http.StatusOK {
		t.Errorf("a failed search returned 200: %q", rec.Body.String())
	}
}

// An error that is not an AppError at all -- a programming mistake, say -- must
// still be a 500 and still be quiet.
func TestAPlainErrorIsAlsoKeptOffTheWire(t *testing.T) {
	plain := errors.New("something went wrong: dial tcp 10.0.0.5:5432 refused")

	h := newTestHandler(&fakeStore{dataErr: plain}, &fakeRanker{}, &fakeSpeller{})
	rec := call(t, h, "/?query=hello")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("response body leaks the host: %q", rec.Body.String())
	}
}

// ── the happy path ──────────────────────────────────────────────────────────

func samplePage(title string) *model.Page {
	return &model.Page{
		ID:      uuid.New(),
		URL:     "https://example.test/" + title,
		PRScore: 0.5,
		Words:   map[string]int{"hello": 2},
		MetaData: model.MetaData{
			URL:   "https://example.test/" + title,
			Title: title,
		},
	}
}

func TestASuccessfulSearchRendersTheResults(t *testing.T) {
	page := samplePage("a result")

	s := &fakeStore{
		totalPages: 1,
		data:       &store.Data{Idf: map[string]float64{"hello": 2.5}},
	}
	h := newTestHandler(s, &fakeRanker{pages: []*model.Page{page}}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello&tab=all")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "a result") {
		t.Errorf("the result page does not mention the result it was given: %q", body)
	}
}

// The query the speller produced is what gets searched for, and it is the whole
// point of running the words through aspell first.
func TestTheSearchedQueryIsWhateverTheSpellerProduced(t *testing.T) {
	s := &fakeStore{totalPages: 1, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{
		suggestions: []string{"hello", "help", "hell"},
	})

	call(t, h, "/?query=helo")

	for _, want := range []string{"hello", "help", "hell"} {
		found := false
		for _, got := range s.gotWords {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the store was searched for %v, want it to include %q", s.gotWords, want)
		}
	}
}

// `?page` is 1-based in the URL and 0-based in `OFFSET pageNum * PageSize`, so the
// handler has to subtract one. Getting this wrong hands every first-page request
// the second page of results, which looks fine until you notice the first page is
// never shown.
func TestThePageParameterIsTranslatedToAZeroBasedOffset(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"/?query=hello", 0},          // no parameter: the first page
		{"/?query=hello&page=1", 0},   // explicitly the first page
		{"/?query=hello&page=3", 2},   // the third page, as offset 2
		{"/?query=hello&page=0", 0},   // 0 is not a page; fall back to the first
		{"/?query=hello&page=-4", 0},  // neither is a negative number
		{"/?query=hello&page=abc", 0}, // nor is a word
	} {
		s := &fakeStore{totalPages: 9, data: &store.Data{}}
		h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

		call(t, h, tc.query)

		if s.gotPage != tc.want {
			t.Errorf("%s: store got pageNum %d, want %d", tc.query, s.gotPage, tc.want)
		}
	}
}

// A missing `?page` must not be read as page zero *and* then treated as "no
// limit", which is how a full-index scan gets into a search.
func TestTheResultSetIsAlwaysPaged(t *testing.T) {
	s := &fakeStore{totalPages: 3, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

	call(t, h, "/?query=hello")

	if s.dataCall != 1 {
		t.Errorf("GetData was called %d times, want 1", s.dataCall)
	}
}

// The `images` and `graph` tabs short-circuit before any store call, so a
// failure in the database cannot take them down with it.
func TestTheOtherTabsDoNotTouchTheStore(t *testing.T) {
	for _, tab := range []string{"images", "graph"} {
		s := &fakeStore{dataErr: dbIsDown}
		h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

		rec := call(t, h, "/?query=hello&tab="+tab)

		if s.dataCall != 0 || s.totalCall != 0 {
			t.Errorf("tab %q hit the store (%d/%d calls)", tab, s.dataCall, s.totalCall)
		}
		if rec.Code == http.StatusInternalServerError {
			t.Errorf("tab %q returned 500 with an empty store: %q", tab, rec.Body.String())
		}
	}
}

// An unrecognised tab falls through to the all-tab rather than rendering
// nothing, so a bad or hand-edited link still returns results.
func TestAnUnknownTabFallsBackToAllResults(t *testing.T) {
	s := &fakeStore{totalPages: 1, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{pages: []*model.Page{samplePage("fallback")}}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello&tab=nonsense")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if s.dataCall != 1 {
		t.Errorf("an unknown tab did not reach the search path")
	}
}
