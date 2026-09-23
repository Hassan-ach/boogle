package ranking

import (
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"
	"github.com/google/uuid"
)

func TestTF(t *testing.T) {
	tests := []struct {
		name     string
		query    []string
		expected map[string]int
	}{
		{
			name:     "empty query",
			query:    []string{},
			expected: map[string]int{},
		},
		{
			name:     "single word",
			query:    []string{"search"},
			expected: map[string]int{"search": 1},
		},
		{
			name:     "multiple unique words",
			query:    []string{"search", "engine", "boogle"},
			expected: map[string]int{"search": 1, "engine": 1, "boogle": 1},
		},
		{
			name:     "repeated words",
			query:    []string{"search", "engine", "search", "search"},
			expected: map[string]int{"search": 3, "engine": 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tf(tt.query)
			if len(got) != len(tt.expected) {
				t.Fatalf("tf() returned map of length %d, want %d", len(got), len(tt.expected))
			}
			for k, wantVal := range tt.expected {
				if gotVal, ok := got[k]; !ok || gotVal != wantVal {
					t.Errorf("tf()[%q] = %d, want %d", k, gotVal, wantVal)
				}
			}
		})
	}
}

func TestNormalizeTFIDF(t *testing.T) {
	page1 := &model.Page{ID: uuid.New(), URL: "https://example.com/1"}
	page2 := &model.Page{ID: uuid.New(), URL: "https://example.com/2"}
	page3 := &model.Page{ID: uuid.New(), URL: "https://example.com/3"}

	t.Run("empty pages map", func(t *testing.T) {
		emptyMap := map[*model.Page]float64{}
		normalizeTFIDF(emptyMap)
		if len(emptyMap) != 0 {
			t.Errorf("expected empty map, got len %d", len(emptyMap))
		}
	})

	t.Run("all pages with same score", func(t *testing.T) {
		pages := map[*model.Page]float64{
			page1: 5.0,
			page2: 5.0,
		}
		normalizeTFIDF(pages)
		for p, score := range pages {
			if score != 0 {
				t.Errorf("expected score 0 for page %s when max==min, got %f", p.URL, score)
			}
		}
	})

	t.Run("varied scores normalization", func(t *testing.T) {
		pages := map[*model.Page]float64{
			page1: 10.0, // min -> 0.0
			page2: 20.0, // mid -> (20-10)/(30-10) = 0.5
			page3: 30.0, // max -> 1.0
		}
		normalizeTFIDF(pages)

		if pages[page1] != 0.0 {
			t.Errorf("expected min page score 0.0, got %f", pages[page1])
		}
		if pages[page2] != 0.5 {
			t.Errorf("expected mid page score 0.5, got %f", pages[page2])
		}
		if pages[page3] != 1.0 {
			t.Errorf("expected max page score 1.0, got %f", pages[page3])
		}
	})
}

func TestTFIDF(t *testing.T) {
	pageID1 := uuid.New()
	pageID2 := uuid.New()

	page1 := &model.Page{
		ID:    pageID1,
		URL:   "https://golang.org",
		Words: map[string]int{"golang": 3, "search": 1},
	}
	page2 := &model.Page{
		ID:    pageID2,
		URL:   "https://example.com",
		Words: map[string]int{"search": 2},
	}

	query := []string{"golang", "search"}
	idf := map[string]float64{
		"golang": 2.5,
		"search": 1.0,
	}

	wordMapper := util.NewWordMapper()
	for _, w := range query {
		wordMapper.MapValue(w)
	}

	pageMapper := util.NewPageMapper()
	pageMapper.MapValue(pageID1)
	pageMapper.MapValue(pageID2)

	data := &store.Data{
		Pages:      []*model.Page{page1, page2},
		Idf:        idf,
		WordMapper: wordMapper,
		PageMapper: pageMapper,
	}

	scores, err := tfIdf(data)
	if err != nil {
		t.Fatalf("tfIdf() unexpected error: %v", err)
	}

	if len(scores) != 2 {
		t.Fatalf("expected 2 page scores, got %d", len(scores))
	}

	// page1 has golang and search, page2 only has search.
	// page1 should have a higher cosine similarity with the query vector.
	if scores[page1] <= scores[page2] {
		t.Errorf("expected page1 score (%f) to be greater than page2 score (%f)", scores[page1], scores[page2])
	}
}
