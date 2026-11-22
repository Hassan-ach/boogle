package handlers

import (
	"context"
	"errors"
	"fmt"
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
	totalResults int
	data         *store.Data

	totalErr error
	dataErr  error

	gotWords  []string
	totalCall int
	dataCall  int
	pageSize  int
}

func (f *fakeStore) GetTotalResults(_ context.Context, q []string) (int, error) {
	f.totalCall++
	f.gotWords = q
	if f.totalErr != nil {
		return 0, f.totalErr
	}
	return f.totalResults, nil
}

func (f *fakeStore) GetData(_ context.Context, words []string) (*store.Data, error) {
	f.dataCall++
	f.gotWords = words
	if f.dataErr != nil {
		return nil, f.dataErr
	}
	return f.data, nil
}

func (f *fakeStore) PageSize() int {
	if f.pageSize > 0 {
		return f.pageSize
	}
	return 20
}

func (f *fakeStore) CandidateLimit() int { return 100 }

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

func (f *fakeSpeller) GetSuggestions(q string) []string {
	if f.suggestions != nil {
		return f.suggestions
	}
	// Echo the input so the query caps are observable.
	return strings.Fields(q)
}

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
		{"ranking the results", &fakeStore{totalResults: 1, data: &store.Data{}}},
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
			Title: title,
		},
	}
}

func TestASuccessfulSearchRendersTheResults(t *testing.T) {
	page := samplePage("a result")

	s := &fakeStore{
		totalResults: 1,
		data:         &store.Data{Idf: map[string]float64{"hello": 2.5}},
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
	s := &fakeStore{totalResults: 1, data: &store.Data{}}
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

func TestThePageParameterSelectsASliceOfTheRankedWindow(t *testing.T) {
	pages := make([]*model.Page, 60)
	for i := range pages {
		pages[i] = samplePage(fmt.Sprintf("result-%02d", i))
	}

	s := &fakeStore{totalResults: 60, pageSize: 20, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{pages: pages}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello&page=2")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The ranked window is fetched once and sliced in Go; the SQL pass no longer
	// decides which results belong to page 2.
	if s.dataCall != 1 {
		t.Errorf("GetData was called %d times, want 1", s.dataCall)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "result-20") {
		t.Errorf("page 2 does not contain the 21st ranked result: %q", body)
	}
	if strings.Contains(body, "result-00") {
		t.Errorf("page 2 contains a page-1 result: %q", body)
	}
}

func TestAnOutOfRangePageRendersTheLastPageInsteadOfADeadEnd(t *testing.T) {
	pages := make([]*model.Page, 45)
	for i := range pages {
		pages[i] = samplePage(fmt.Sprintf("result-%02d", i))
	}

	s := &fakeStore{totalResults: 45, pageSize: 20, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{pages: pages}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello&page=99")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "3 / 3") {
		t.Errorf("a stale page parameter was not clamped to the last page: %q", body)
	}
	if !strings.Contains(body, "result-44") {
		t.Errorf("the last page does not contain the last result: %q", body)
	}
}

func TestThePageCountIsNotTheResultCount(t *testing.T) {
	pages := make([]*model.Page, 60)
	for i := range pages {
		pages[i] = samplePage(fmt.Sprintf("result-%02d", i))
	}
	s := &fakeStore{totalResults: 60, pageSize: 20, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{pages: pages}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello")

	body := rec.Body.String()
	if strings.Contains(body, "1 / 60") {
		t.Errorf("pagination advertises one page per match: %q", body)
	}
	if !strings.Contains(body, "1 / 3") {
		t.Errorf("pagination does not show three pages for 60 results at 20 per page: %q", body)
	}
}

func TestTheResultWindowIsFetchedOnce(t *testing.T) {
	s := &fakeStore{totalResults: 3, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

	call(t, h, "/?query=hello")

	if s.dataCall != 1 {
		t.Errorf("GetData was called %d times, want 1", s.dataCall)
	}
}

func TestTheQueryIsCappedBeforeItReachesTheSpeller(t *testing.T) {
	t.Run("runes", func(t *testing.T) {
		s := &fakeStore{data: &store.Data{}}
		h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

		call(t, h, "/?query="+strings.Repeat("x", 500))

		if len(s.gotWords) != 1 {
			t.Fatalf("speller received %v", s.gotWords)
		}
		if got := len([]rune(s.gotWords[0])); got > maxQueryRunes {
			t.Errorf("token length = %d, want <= %d", got, maxQueryRunes)
		}
	})

	t.Run("tokens", func(t *testing.T) {
		s := &fakeStore{data: &store.Data{}}
		h := newTestHandler(s, &fakeRanker{}, &fakeSpeller{})

		tokens := make([]string, 15)
		for i := range tokens {
			tokens[i] = fmt.Sprintf("word%d", i)
		}
		call(t, h, "/?query="+strings.Join(tokens, "+"))

		if len(s.gotWords) != maxQueryTokens {
			t.Errorf("speller received %d tokens, want %d", len(s.gotWords), maxQueryTokens)
		}
	})
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
	s := &fakeStore{totalResults: 1, data: &store.Data{}}
	h := newTestHandler(s, &fakeRanker{pages: []*model.Page{samplePage("fallback")}}, &fakeSpeller{})

	rec := call(t, h, "/?query=hello&tab=nonsense")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if s.dataCall != 1 {
		t.Errorf("an unknown tab did not reach the search path")
	}
}
