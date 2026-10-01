package spellchecker

import (
	"reflect"
	"strings"
	"testing"
)

type fakeDict struct {
	correct map[string]bool
	suggs   map[string][]string
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

	want := []string{"apple", "mispeled", "zapping", "zebra", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("suggestionsFor() = %v, want %v", got, want)
	}
}

func TestSuggestionsAreDeterministicAcrossCalls(t *testing.T) {
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

	if !reflect.DeepEqual(got, []string{"apple", "mispeled", "zebra"}) {
		t.Errorf("suggestionsFor() = %v, want [apple mispeled zebra]", got)
	}
}

func TestAtMostThreeSuggestionsPerMisspelledWord(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"mispeled": {"one", "two", "three", "four", "five"},
	})

	got := suggestionsFor("mispeled", d, 3)

	if !reflect.DeepEqual(got, []string{"mispeled", "one", "three", "two"}) {
		t.Errorf("suggestionsFor() = %v, want [mispeled one three two]", got)
	}
}

func TestTheLimitIsRespectedPerWordNotInTotal(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"aaaa": {"a1", "a2", "a3", "a4"},
		"bbbb": {"b1", "b2", "b3", "b4"},
	})

	got := suggestionsFor("aaaa bbbb", d, 3)

	want := []string{"a1", "a2", "a3", "aaaa", "b1", "b2", "b3", "bbbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("suggestionsFor() = %v, want %v", got, want)
	}
}

func TestApostrophesAreFilteredOut(t *testing.T) {
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

	for _, w := range got {
		if strings.TrimSpace(w) == "" {
			t.Errorf("suggestionsFor() = %v, which contains a blank entry", got)
		}
	}
	if !reflect.DeepEqual(got, []string{"real", "x"}) {
		t.Errorf("suggestionsFor() = %v, want [real x]", got)
	}
}

func TestTheQueryIsSplitOnSpaces(t *testing.T) {
	d := newFake([]string{}, map[string][]string{
		"alpha": {"alfa"},
		"beta":  {"bta"},
	})

	got := suggestionsFor("alpha beta", d, 3)

	if !reflect.DeepEqual(d.ordered, []string{"alpha", "beta"}) {
		t.Errorf("Check was called with %v, want each word separately", d.ordered)
	}
	if !reflect.DeepEqual(got, []string{"alfa", "alpha", "beta", "bta"}) {
		t.Errorf("suggestionsFor() = %v, want [alfa alpha beta bta]", got)
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

func TestAnUnknownWordIsNotDroppedFromTheQuery(t *testing.T) {
	d := newFake([]string{}, map[string][]string{})

	got := suggestionsFor("zzzzzz", d, 3)

	if !reflect.DeepEqual(got, []string{"zzzzzz"}) {
		t.Errorf("suggestionsFor() = %v, want the word as typed: [zzzzzz]", got)
	}
}

func TestAWordWithNoSuggestionsDoesNotProduceABlankEntry(t *testing.T) {
	d := newFake([]string{}, map[string][]string{})

	got := suggestionsFor("zzzzzz", d, 3)

	for _, w := range got {
		if strings.TrimSpace(w) == "" {
			t.Errorf("suggestionsFor() = %v, which contains a blank entry", got)
		}
	}
}

func TestRepeatedWordsAppearOnce(t *testing.T) {
	d := newFake([]string{"cat"}, nil)

	got := suggestionsFor("cat cat cat", d, 3)

	if !reflect.DeepEqual(got, []string{"cat"}) {
		t.Errorf("suggestionsFor() = %v, want [cat]", got)
	}
}

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
