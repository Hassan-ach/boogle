package spellchecker

import (
	"sort"
	"strings"

	"github.com/trustmaster/go-aspell"
)

// dictionary is the slice of aspell this service actually uses. Depending on
// the interface rather than *aspell.Speller keeps the suggestion logic
// testable without the C library.
type dictionary interface {
	Check(word string) bool
	Suggest(word string) []string
}

type AspellSpeller struct {
	speller dictionary
}

func NewAspellSpellingService() (AspellSpeller, error) {
	speller, err := aspell.NewSpeller(map[string]string{
		"lang": "en_US",
	})
	if err != nil {
		return AspellSpeller{}, err
	}

	return AspellSpeller{
		speller: &speller,
	}, nil
}

// GetSuggestions takes a query string and returns a list of unique words that are either correctly spelled or suggested by the speller.
//
// The result is sorted. It used to be built from a map and returned in Go's
// randomised iteration order, so the same query produced a different suggestion
// list on every call -- which made results uncacheable, broke any test that
// compared them, and shuffled the "did you mean" ordering users see.
func (s AspellSpeller) GetSuggestions(q string) []string {
	return suggestionsFor(q, s.speller, 3)
}

func suggestionsFor(q string, speller dictionary, maxSuggestions int) []string {
	wordList := strings.Split(q, " ")
	suggestions := make(map[string]struct{}, len(wordList))

	for _, word := range wordList {
		if word == "" {
			continue
		}

		if speller.Check(word) {
			suggestions[strings.ToLower(word)] = struct{}{}
			continue
		}

		for i, w := range speller.Suggest(word) {
			if i >= maxSuggestions {
				break
			}
			suggestions[strings.ToLower(w)] = struct{}{}
		}
	}

	res := make([]string, 0, len(suggestions))
	for w := range suggestions {
		if strings.TrimSpace(w) == "" || strings.ContainsRune(w, '\'') {
			continue
		}
		res = append(res, w)
	}

	// Deterministic output: a randomised order here means the same query can
	// return a different list on every call.
	sort.Strings(res)

	return res
}
