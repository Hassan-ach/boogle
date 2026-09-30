package spellchecker

import (
	"reflect"
	"strings"
	"testing"
)

// fakeDict is a stand-in for aspell so the suggestion rules can be tested
// without depending on the C library or on which dictionary is installed.
type fakeDict struct {
	correct map[string]bool
	suggs   map[string][]string
	// ordered records every word Check was asked about, so tests can assert the
	// query was actually split rather than passed through whole.
	ordered []string
}

func (f *fakeDict) Check(word string) bool {
	f.ordered = append(f.ordered, word)
	return f.correct[word]
}

func (f *fakeDict) Suggest(word string) []string {
	return f.suggs[word]
}

func newFake(correct []string, suggs map[string][]string) *fakeDict {
	c := make(map[string]bool, len(correct))
	for _, w := range correct {
		c[w] = true
	}
	return &fakeDict{correct: c, suggs: suggs}
}

func TestSuggestionsAreSortedAndDeduplicated(t *testing.T) {
	d := newFake([]string{"zebra", "apple"}, map[string][]string{
		"mispeled": {"zulu", "zebra", "zapping", "zebra"},
	})

	got := suggestionsFor("zebra mispeled apple", d, 3)

	// "zebra" is correct, and is also a suggestion for "mispeled", so it must
	// appear exactly once.
	want := []string{"apple", "zapping", "zebra", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("suggestionsFor() = %v, want %v", got, want)
	}
}

func TestSuggestionsAreDeterministicAcrossCalls(t *testing.T) {
	// Regression: the result came out of a map, so Go's randomised iteration
	// order leaked into the response and the same query answered differently
	// every time.
	d := newFake([]string{}, map[string][]string{
		"qeury": {"query", "queue", "quay", "quick"},
	})

	first := suggestionsFor("qeury", d, 3)
	for i := 0; i < 500; i++ {
		got := suggestionsFor("qeury", d, 3)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d = %v, want the stable %v", i, got, first)
		}
	}
}

func TestSuggestionsAreSortedAscending(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"wrold": {"world", "would", "word", "wold"},
	})

	got := suggestionsFor("wrold", d, 3)

	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("result is not sorted: %v", got)
			break
		}
	}
}

func TestCorrectWordsAreKeptAsIs(t *testing.T) {
	d := newFake([]string{"search", "engine"}, nil)

	got := suggestionsFor("search engine", d, 3)

	if !reflect.DeepEqual(got, []string{"engine", "search"}) {
		t.Errorf("suggestionsFor() = %v, want [engine search]", got)
	}
}

func TestCorrectWordsAreLowercased(t *testing.T) {
	// The index is lowercased by the tokenizer, so a suggestion with different
	// casing would never match anything.
	d := newFake([]string{"GoLang"}, nil)

	got := suggestionsFor("GoLang", d, 3)

	if !reflect.DeepEqual(got, []string{"golang"}) {
		t.Errorf("suggestionsFor() = %v, want [golang]", got)
	}
}

func TestSuggestionsAreLowercased(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"mispeled": {"Zebra", "APPLE"},
	})

	got := suggestionsFor("mispeled", d, 3)

	if !reflect.DeepEqual(got, []string{"apple", "zebra"}) {
		t.Errorf("suggestionsFor() = %v, want [apple zebra]", got)
	}
}

func TestAtMostThreeSuggestionsPerMisspelledWord(t *testing.T) {
	// An unbounded list swamps the result page and costs a full aspell pass.
	d := newFake([]string{}, map[string][]string{
		"mispeled": {"one", "two", "three", "four", "five"},
	})

	got := suggestionsFor("mispeled", d, 3)

	if !reflect.DeepEqual(got, []string{"one", "three", "two"}) {
		t.Errorf("suggestionsFor() = %v, want [one three two]", got)
	}
}

func TestTheLimitIsRespectedPerWordNotInTotal(t *testing.T) {
	// Each misspelled word gets its own budget, so a long query is not starved
	// by the first word's suggestions.
	d := newFake([]string{}, map[string][]string{
		"aaaa": {"a1", "a2", "a3", "a4"},
		"bbbb": {"b1", "b2", "b3", "b4"},
	})

	got := suggestionsFor("aaaa bbbb", d, 3)

	want := []string{"a1", "a2", "a3", "b1", "b2", "b3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("suggestionsFor() = %v, want %v", got, want)
	}
}

func TestApostrophesAreFilteredOut(t *testing.T) {
	// Aspell loves returning possessives, which are noise in a query box.
	d := newFake([]string{}, map[string][]string{
		"john": {"john's", "john", "jon"},
	})

	got := suggestionsFor("john", d, 3)

	for _, w := range got {
		if strings.ContainsRune(w, '\'') {
			t.Errorf("suggestion %q contains an apostrophe", w)
		}
	}
	if !reflect.DeepEqual(got, []string{"john", "jon"}) {
		t.Errorf("suggestionsFor() = %v, want [john jon]", got)
	}
}

func TestBlankWordsAreDropped(t *testing.T) {
	// Repeated and leading spaces would otherwise be "correct" empty words.
	d := newFake([]string{"alpha", "beta"}, nil)

	got := suggestionsFor("  alpha   beta  ", d, 3)

	if !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Errorf("suggestionsFor() = %v, want [alpha beta]", got)
	}
}

func TestWhitespaceOnlySuggestionsAreDropped(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"x": {" ", "real"},
	})

	got := suggestionsFor("x", d, 3)

	if !reflect.DeepEqual(got, []string{"real"}) {
		t.Errorf("suggestionsFor() = %v, want [real]", got)
	}
}

func TestTheQueryIsSplitOnSpaces(t *testing.T) {
	// Suggesting for the whole query string at once returns nothing useful.
	d := newFake([]string{}, map[string][]string{
		"alpha": {"alfa"},
		"beta":  {"bta"},
	})

	got := suggestionsFor("alpha beta", d, 3)

	if !reflect.DeepEqual(d.ordered, []string{"alpha", "beta"}) {
		t.Errorf("Check was called with %v, want each word separately", d.ordered)
	}
	if !reflect.DeepEqual(got, []string{"alfa", "bta"}) {
		t.Errorf("suggestionsFor() = %v, want [alfa bta]", got)
	}
}

func TestEmptyQueryReturnsNothing(t *testing.T) {
	d := newFake([]string{}, nil)

	if got := suggestionsFor("", d, 3); len(got) != 0 {
		t.Errorf("suggestionsFor(\"\") = %v, want an empty slice", got)
	}
}

func TestQueryOfOnlySpacesReturnsNothing(t *testing.T) {
	d := newFake([]string{}, nil)

	if got := suggestionsFor("   \t  ", d, 3); len(got) != 0 {
		t.Errorf("suggestionsFor(spaces) = %v, want an empty slice", got)
	}
}

func TestAWordWithNoSuggestionsIsSimplyOmitted(t *testing.T) {
	// Aspell returns an empty list for input it cannot parse; that must not
	// produce an empty string in the results.
	d := newFake([]string{}, map[string][]string{})

	got := suggestionsFor("zzzzzz", d, 3)

	if len(got) != 0 {
		t.Errorf("suggestionsFor() = %v, want an empty slice", got)
	}
}

func TestRepeatedWordsAppearOnce(t *testing.T) {
	d := newFake([]string{"cat"}, nil)

	got := suggestionsFor("cat cat cat", d, 3)

	if !reflect.DeepEqual(got, []string{"cat"}) {
		t.Errorf("suggestionsFor() = %v, want [cat]", got)
	}
}

// TestAspellResultsAreStable exercises the real C library, so it is skipped
// when aspell is not installed rather than failing the whole suite.
func TestAspellResultsAreStable(t *testing.T) {
	speller, err := NewAspellSpellingService()
	if err != nil {
		t.Skipf("aspell is unavailable: %v", err)
	}

	first := speller.GetSuggestions("serch engin")
	if len(first) == 0 {
		t.Skip("aspell returned nothing; the en_US dictionary may be missing")
	}

	for i := 0; i < 100; i++ {
		got := speller.GetSuggestions("serch engin")
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("call %d = %v, want the stable %v", i, got, first)
		}
	}
}

func TestAspellIsCaseInsensitive(t *testing.T) {
	speller, err := NewAspellSpellingService()
	if err != nil {
		t.Skipf("aspell is unavailable: %v", err)
	}

	lower := speller.GetSuggestions("search")
	upper := speller.GetSuggestions("SEARCH")

	if !reflect.DeepEqual(lower, upper) {
		t.Errorf("GetSuggestions(\"SEARCH\") = %v, want the same as \"search\" = %v", upper, lower)
	}
}
