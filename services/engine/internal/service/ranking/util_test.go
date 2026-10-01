package ranking

import (
	"math"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"
	"github.com/google/uuid"
)

func TestDocVector(t *testing.T) {
	wordMapper := util.NewWordMapper()
	wordMapper.MapValue("search")
	wordMapper.MapValue("engine")
	wordMapper.MapValue("go")

	wordIdf := map[string]float64{
		"search": 1.5,
		"engine": 2.0,
		"go":     3.0,
	}

	page := &model.Page{
		Words: map[string]int{
			"search": 2,
			"go":     1,
		},
	}

	vec := docVector(page, wordIdf, wordMapper, 3)

	expected := []float64{3.0, 0.0, 3.0}

	if len(vec) != len(expected) {
		t.Fatalf("expected vector length %d, got %d", len(expected), len(vec))
	}

	for i, want := range expected {
		if math.Abs(vec[i]-want) > 1e-9 {
			t.Errorf("vector[%d] = %f, want %f", i, vec[i], want)
		}
	}
}

func TestQueryVector(t *testing.T) {
	wordMapper := util.NewWordMapper()
	wordMapper.MapValue("search")
	wordMapper.MapValue("engine")

	wordIdf := map[string]float64{
		"search": 1.5,
		"engine": 2.0,
	}

	query := []string{"search", "search", "engine"}

	vec := queryVector(query, wordIdf, wordMapper)

	expected := []float64{3.0, 2.0, 0.0}

	if len(vec) != len(expected) {
		t.Fatalf("expected vector length %d, got %d", len(expected), len(vec))
	}

	for i, want := range expected {
		if math.Abs(vec[i]-want) > 1e-9 {
			t.Errorf("vector[%d] = %f, want %f", i, vec[i], want)
		}
	}
}

func TestSort(t *testing.T) {
	t.Run("sort by global score descending", func(t *testing.T) {
		page1 := &model.Page{ID: uuid.New(), URL: "https://a.com", PRScore: 0.8}
		page2 := &model.Page{ID: uuid.New(), URL: "https://b.com", PRScore: 0.2}

		pages := map[*model.Page]float64{
			page1: 0.2,
			page2: 0.9,
		}

		sorted, err := sort(pages, 0.5)
		if err != nil {
			t.Fatalf("sort() returned unexpected error: %v", err)
		}

		if len(sorted) != 2 {
			t.Fatalf("expected 2 sorted pages, got %d", len(sorted))
		}

		if sorted[0].URL != "https://b.com" || sorted[1].URL != "https://a.com" {
			t.Errorf("expected [b.com, a.com], got [%s, %s]", sorted[0].URL, sorted[1].URL)
		}

		if math.Abs(sorted[0].GlobalScore-0.55) > 1e-9 {
			t.Errorf("page2 global score = %f, want 0.55", sorted[0].GlobalScore)
		}
	})

	t.Run("tie-breaker by word count descending", func(t *testing.T) {
		page1 := &model.Page{
			ID:      uuid.New(),
			URL:     "https://few-words.com",
			PRScore: 0.5,
			Words:   map[string]int{"a": 1},
		}
		page2 := &model.Page{
			ID:      uuid.New(),
			URL:     "https://many-words.com",
			PRScore: 0.5,
			Words:   map[string]int{"a": 1, "b": 2, "c": 3},
		}

		pages := map[*model.Page]float64{
			page1: 0.5,
			page2: 0.5,
		}

		sorted, err := sort(pages, 0.5)
		if err != nil {
			t.Fatalf("sort() returned unexpected error: %v", err)
		}

		if sorted[0].URL != "https://many-words.com" {
			t.Errorf("expected page with more words first, got %s", sorted[0].URL)
		}
	})

	t.Run("tie-breaker by title comparison", func(t *testing.T) {
		page1 := &model.Page{
			ID:       uuid.New(),
			URL:      "https://title-a.com",
			PRScore:  0.5,
			Words:    map[string]int{"a": 1},
			MetaData: model.MetaData{Title: "AAA Title"},
		}
		page2 := &model.Page{
			ID:       uuid.New(),
			URL:      "https://title-z.com",
			PRScore:  0.5,
			Words:    map[string]int{"a": 1},
			MetaData: model.MetaData{Title: "ZZZ Title"},
		}

		pages := map[*model.Page]float64{
			page1: 0.5,
			page2: 0.5,
		}

		sorted, err := sort(pages, 0.5)
		if err != nil {
			t.Fatalf("sort() returned unexpected error: %v", err)
		}

		if len(sorted) != 2 {
			t.Fatalf("expected 2 sorted pages, got %d", len(sorted))
		}

		if sorted[0].MetaData.Title != "ZZZ Title" {
			t.Errorf("expected 'ZZZ Title' first based on sort order logic, got '%s'", sorted[0].MetaData.Title)
		}
	})
}
