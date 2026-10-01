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

type fakeStore struct {
	totalPages int
	data       *store.Data

	totalErr error
	dataErr  error

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

func newTestHandler(s store.Store, r *fakeRanker, sp *fakeSpeller) *SearchingHandler {
	return NewSearchHandler(s, r, sp)
}

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

var dbIsDown = apperror.Internal(errors.New(
	"dial tcp 127.0.0.1:5432: connect: connection refused (password=hunter2)",
))

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

func TestASearchFailureIsNotReportedAsAnEmptyResultPage(t *testing.T) {
	h := newTestHandler(&fakeStore{dataErr: dbIsDown}, &fakeRanker{}, &fakeSpeller{})
	rec := call(t, h, "/?query=hello")

	if rec.Code == http.StatusOK {
		t.Errorf("a failed search returned 200: %q", rec.Body.String())
	}
}

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

func TestThePageParameterIsTranslatedToAZeroBasedOffset(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"/?query=hello", 0},
		{"/?query=hello&page=1", 0},
		{"/?query=hello&page=3", 2},
		{"/?query=hello&page=0", 0},
		{"/?query=hello&page=-4", 0},
		{"/?query=hello&page=abc", 0},
	} {
		s := &fakeStore{totalPages: 9, data: &store.Data{}}
		h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

		call(t, h, tc.query)

		if s.gotPage != tc.want {
			t.Errorf("%s: store got pageNum %d, want %d", tc.query, s.gotPage, tc.want)
		}
	}
}

func TestTheResultSetIsAlwaysPaged(t *testing.T) {
	s := &fakeStore{totalPages: 3, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

	call(t, h, "/?query=hello")

	if s.dataCall != 1 {
		t.Errorf("GetData was called %d times, want 1", s.dataCall)
	}
}

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
