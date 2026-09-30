package ranking

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/google/uuid"
)

// page builds a page whose only varying attributes are the tie-breakers, so a
// test can force an exact tie on every key the comparator looks at.
func page(title string, words int, prScore, tfidf float64) *model.Page {
	return pageWithURL(title, words, 0, prScore, tfidf)
}

// pageWithURL gives each page a distinct URL and ID, so the only thing that can
// separate two of them is one of the comparator's keys.
func pageWithURL(title string, words, n int, prScore, tfidf float64) *model.Page {
	w := make(map[string]int, words)
	for i := 0; i < words; i++ {
		w[fmt.Sprintf("w%d", i)] = 1
	}
	return &model.Page{
		ID:       uuid.New(),
		URL:      fmt.Sprintf("https://%s/%03d", title, n),
		PRScore:  prScore,
		Words:    w,
		MetaData: model.MetaData{Title: title},
	}
}

// TestLessIsAsymmetric is the regression test for the comparator defect.
//
// The old comparator ended in `return -1`, so for two pages that tied on score,
// word count and title it reported both cmp(a, b) == -1 and cmp(b, a) == -1.
// slices.SortStableFunc fed that inconsistent answer straight into the sort, and
// the resulting order depended on the input order -- which came from a Go map,
// so it changed from run to run.
func TestLessIsAsymmetric(t *testing.T) {
	// Same score, same word count, same title, same URL: the only thing left to
	// separate them is the ID fallback.
	for i := 0; i < 500; i++ {
		a := page("same", 3, 0.5, 0.5)
		b := page("same", 3, 0.5, 0.5)
		a.GlobalScore = 0.5
		b.GlobalScore = 0.5

		aLessB := less(a, b)
		bLessA := less(b, a)

		if aLessB && bLessA {
			t.Fatalf("less(a, b) and less(b, a) are both true: the ordering is not antisymmetric")
		}
		if !aLessB && !bLessA {
			t.Fatalf("neither page sorts first: the ordering is not total")
		}
	}
}

func TestLessIsIrreflexive(t *testing.T) {
	p := page("self", 2, 0.4, 0.4)
	p.GlobalScore = 0.4

	if less(p, p) {
		t.Error("less(p, p) = true; a page must never precede itself")
	}
}

func TestLessOrdersByScoreFirst(t *testing.T) {
	high := page("zzz-high-title", 1, 0.9, 0.1)
	low := page("aaa-low-title", 99, 0.1, 0.9)
	high.GlobalScore = 0.9
	low.GlobalScore = 0.1

	if !less(high, low) {
		t.Error("the higher scoring page must sort first")
	}
	if less(low, high) {
		t.Error("the lower scoring page must not sort first")
	}
}

func TestLessBreaksFullTiesOnWordCount(t *testing.T) {
	many := page("aaa", 5, 0.5, 0.5)
	few := page("zzz", 2, 0.5, 0.5)
	many.GlobalScore = 0.5
	few.GlobalScore = 0.5

	if !less(many, few) {
		t.Error("with equal scores the longer match must come first")
	}
	if less(few, many) {
		t.Error("with equal scores the shorter match must not come first")
	}
}

func TestLessBreaksRemainingTiesOnTitle(t *testing.T) {
	zTitle := page("ZZZ", 4, 0.5, 0.5)
	aTitle := page("AAA", 4, 0.5, 0.5)
	zTitle.GlobalScore = 0.5
	aTitle.GlobalScore = 0.5

	if !less(zTitle, aTitle) {
		t.Error("with equal scores and word counts the later title must come first")
	}
	if less(aTitle, zTitle) {
		t.Error("with equal scores and word counts the earlier title must not come first")
	}
}

func TestLessIsTransitive(t *testing.T) {
	pages := []*model.Page{
		page("c", 1, 0.3, 0.3),
		page("a", 3, 0.9, 0.9),
		page("b", 1, 0.9, 0.9),
		page("a", 2, 0.5, 0.5),
		page("c", 3, 0.5, 0.5),
		page("b", 2, 0.9, 0.9),
	}
	for i := range pages {
		pages[i].GlobalScore = pages[i].PRScore
	}

	for i := 0; i < len(pages); i++ {
		for j := 0; j < len(pages); j++ {
			for k := 0; k < len(pages); k++ {
				if less(pages[i], pages[j]) && less(pages[j], pages[k]) && !less(pages[i], pages[k]) {
					t.Fatalf("less is not transitive for %q < %q < %q",
						pages[i].MetaData.Title,
						pages[j].MetaData.Title,
						pages[k].MetaData.Title)
				}
			}
		}
	}
}

// TestSortIsDeterministic is the user-visible symptom: one query, many runs,
// one answer.
func TestSortIsDeterministic(t *testing.T) {
	build := func() map[*model.Page]float64 {
		pages := map[*model.Page]float64{}
		for i := 0; i < 40; i++ {
			// Constant tf-idf and PR score, so every GlobalScore is 0.5. Titles
			// and word counts both cycle with periods coprime to 40, so groups of
			// four pages tie on score, word count *and* title. Without a total
			// ordering those groups land in whatever order the map happened to
			// iterate in, which differs on every run.
			title := fmt.Sprintf("title-%02d", i%4)
			words := i % 3
			pages[pageWithURL(title, words, i, 0.5, 0.5)] = 0.5
		}
		return pages
	}

	want, err := sort(build(), 0.5)
	if err != nil {
		t.Fatalf("sort() error: %v", err)
	}
	wantOrder := order(want)

	// Each iteration rebuilds an equal map with fresh UUIDs, which is exactly
	// what a new request does.
	for run := 0; run < 200; run++ {
		got, err := sort(build(), 0.5)
		if err != nil {
			t.Fatalf("sort() error: %v", err)
		}
		for i := range wantOrder {
			if order1(got[i]) != wantOrder[i] {
				t.Fatalf("run %d: position %d = %q, want %q\n got %v\nwant %v",
					run, i, order1(got[i]), wantOrder[i], order(got), wantOrder)
			}
		}
	}
}

func order1(p *model.Page) string {
	return p.MetaData.Title + "|" + p.URL
}

func order(pages []*model.Page) []string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = order1(p)
	}
	return out
}

func TestSortIsStableForFullyTiedPages(t *testing.T) {
	// Distinct URLs but identical on every comparator key, so only stability can
	// decide the order.
	pages := make([]*model.Page, 0, 10)
	m := make(map[*model.Page]float64, 10)
	for i := 0; i < 10; i++ {
		p := &model.Page{
			ID:      uuid.New(),
			URL:     fmt.Sprintf("https://tie-%02d", i),
			PRScore: 0.5,
			Words:   map[string]int{"a": 1},
		}
		m[p] = 0.5
		pages = append(pages, p)
	}

	got, err := sort(m, 0.5)
	if err != nil {
		t.Fatalf("sort() error: %v", err)
	}

	if len(got) != len(pages) {
		t.Fatalf("got %d pages, want %d", len(got), len(pages))
	}
	for _, p := range got {
		if math.Abs(p.GlobalScore-0.5) > 1e-9 {
			t.Errorf("GlobalScore = %f, want 0.5", p.GlobalScore)
		}
	}
	// No page may be dropped or duplicated.
	seen := make(map[string]bool, len(got))
	for _, p := range got {
		if seen[p.URL] {
			t.Errorf("page %s appears twice", p.URL)
		}
		seen[p.URL] = true
	}
	for _, p := range pages {
		if !seen[p.URL] {
			t.Errorf("page %s was dropped", p.URL)
		}
	}
}

func TestSortProducesNonIncreasingScores(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	m := make(map[*model.Page]float64, 200)
	for i := 0; i < 200; i++ {
		m[page(fmt.Sprintf("p%03d", i), i%7, 0.5, 0.5)] = rng.Float64()
	}

	got, err := sort(m, 0.7)
	if err != nil {
		t.Fatalf("sort() error: %v", err)
	}

	for i := 1; i < len(got); i++ {
		if got[i-1].GlobalScore < got[i].GlobalScore {
			t.Fatalf("scores out of order at %d: %f then %f",
				i, got[i-1].GlobalScore, got[i].GlobalScore)
		}
	}
}

func TestSortGlobalScoreIsTheWeightedBlend(t *testing.T) {
	tests := []struct {
		name   string
		factor float64
		tfidf  float64
		pr     float64
		want   float64
	}{
		{"tfidf only", 1.0, 0.9, 0.1, 0.9},
		{"pr only", 0.0, 0.9, 0.1, 0.1},
		{"even blend", 0.5, 0.8, 0.2, 0.5},
		{"uneven blend", 0.25, 1.0, 0.0, 0.25},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := page("x", 1, tc.pr, tc.tfidf)
			got, err := sort(map[*model.Page]float64{p: tc.tfidf}, tc.factor)
			if err != nil {
				t.Fatalf("sort() error: %v", err)
			}
			if math.Abs(got[0].GlobalScore-tc.want) > 1e-9 {
				t.Errorf("GlobalScore = %f, want %f", got[0].GlobalScore, tc.want)
			}
		})
	}
}

func TestSortOnEmptyInput(t *testing.T) {
	got, err := sort(nil, 0.5)
	if err != nil {
		t.Fatalf("sort() error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d pages, want 0", len(got))
	}
}

func TestSortOnASinglePage(t *testing.T) {
	p := page("only", 1, 0.5, 0.5)
	got, err := sort(map[*model.Page]float64{p: 0.5}, 0.5)
	if err != nil {
		t.Fatalf("sort() error: %v", err)
	}
	if len(got) != 1 || got[0] != p {
		t.Errorf("got %v, want the single input page", got)
	}
}
