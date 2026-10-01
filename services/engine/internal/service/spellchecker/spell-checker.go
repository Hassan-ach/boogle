package spellchecker

import (
	"sort"
	"strings"

	"github.com/trustmaster/go-aspell"
)

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

		suggestions[strings.ToLower(word)] = struct{}{}

		if speller.Check(word) {
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

	sort.Strings(res)

	return res
}
