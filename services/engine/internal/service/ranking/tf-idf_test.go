package ranking

import (
	"reflect"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/google/uuid"
)

func TestTF(t *testing.T) {
	query := []string{"search", "engine", "search", "boogle"}
	got := tf(query)

	expected := map[string]int{
		"search": 2,
		"engine": 1,
		"boogle": 1,
	}

	if !reflect.DeepEqual(got, expected) {
		t.Errorf("tf() = %v, expected %v", got, expected)
	}
}

func TestNormalizeTFIDF(t *testing.T) {
	p1 := &model.Page{ID: uuid.New(), URL: "http://example.com/1"}
	p2 := &model.Page{ID: uuid.New(), URL: "http://example.com/2"}
	p3 := &model.Page{ID: uuid.New(), URL: "http://example.com/3"}

	pages := map[*model.Page]float64{
		p1: 10.0,
		p2: 20.0,
		p3: 30.0,
	}

	normalizeTFIDF(pages)

	// Min is 10.0, Max is 30.0, Range is 20.0
	// p1: (10-10)/20 = 0.0
	// p2: (20-10)/20 = 0.5
	// p3: (30-10)/20 = 1.0

	if pages[p1] != 0.0 {
		t.Errorf("expected p1 normalized to 0.0, got %f", pages[p1])
	}
	if pages[p2] != 0.5 {
		t.Errorf("expected p2 normalized to 0.5, got %f", pages[p2])
	}
	if pages[p3] != 1.0 {
		t.Errorf("expected p3 normalized to 1.0, got %f", pages[p3])
	}
}

func TestNormalizeTFIDF_EmptyAndEqual(t *testing.T) {
	// Test empty map
	emptyPages := map[*model.Page]float64{}
	normalizeTFIDF(emptyPages)
	if len(emptyPages) != 0 {
		t.Errorf("expected empty map to remain empty")
	}

	// Test equal values (dom == 0)
	p1 := &model.Page{ID: uuid.New()}
	p2 := &model.Page{ID: uuid.New()}
	equalPages := map[*model.Page]float64{
		p1: 5.0,
		p2: 5.0,
	}
	normalizeTFIDF(equalPages)
	if equalPages[p1] != 0.0 || equalPages[p2] != 0.0 {
		t.Errorf("expected equal pages to normalize to 0.0, got %v", equalPages)
	}
}
