package result

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/a-h/templ"
	"github.com/google/uuid"
)

func render(t *testing.T, component templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func sample() []*model.Page {
	return []*model.Page{{
		ID:       uuid.New(),
		URL:      "https://example.test/",
		MetaData: model.MetaData{Title: "example"},
	}}
}

func TestTheHtmxResponseIsAFragmentWithoutASecondResultsID(t *testing.T) {
	body := render(t, ShowAll(sample(), 1, 1, 1, true))

	if strings.Contains(body, "<html") {
		t.Error("the htmx response is a whole document; it would be swapped inside #results")
	}
	if strings.Contains(body, `id="results"`) {
		t.Error("the htmx response repeats the swap target's id")
	}
	if !strings.Contains(body, "example") {
		t.Error("the fragment lost the result")
	}
}

func TestTheFullResponseContainsExactlyOneResultsTarget(t *testing.T) {
	body := render(t, ShowAll(sample(), 1, 1, 1, false))

	if !strings.Contains(body, "<html") {
		t.Error("the full page is not a document")
	}
	if got := strings.Count(body, `id="results"`); got != 1 {
		t.Errorf("the full page contains %d #results elements, want 1", got)
	}
}

func TestThePlaceholderTabsAlsoReturnFragmentsForHtmx(t *testing.T) {
	for name, body := range map[string]string{
		"images": render(t, ShowImages(nil, true)),
		"graph":  render(t, ShowGraph(nil, true)),
	} {
		if strings.Contains(body, "<html") || strings.Contains(body, `id="results"`) {
			t.Errorf("%s htmx response is not a fragment", name)
		}
	}
}

func TestPaginationIsPartOfTheSwappedFragment(t *testing.T) {
	fragment := render(t, ShowAll(sample(), 60, 3, 2, true))

	if !strings.Contains(fragment, "goToPage") {
		t.Fatal("the htmx fragment has no pagination, so paging stops after a swap")
	}
	if strings.Contains(fragment, `id="results"`) {
		t.Error("the fragment repeats #results")
	}
}
