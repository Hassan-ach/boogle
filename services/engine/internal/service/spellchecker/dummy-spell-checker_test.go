package spellchecker

import (
	"reflect"
	"testing"
)

func TestDummySpeller(t *testing.T) {
	speller := NewDummySpellingService()
	query := "golang search engine"

	suggestions := speller.GetSuggestions(query)
	expected := []string{"golang search engine"}

	if !reflect.DeepEqual(suggestions, expected) {
		t.Errorf("GetSuggestions() = %v, expected %v", suggestions, expected)
	}
}
