package spellchecker

import (
	"sort"
	"strings"
	"sync"

	"github.com/trustmaster/go-aspell"
)

// fallbackMu guards spellers built as a zero value (tests, future call sites).
var fallbackMu sync.Mutex

type dictionary interface {
	Check(word string) bool
	Suggest(word string) []string
}

// AspellSpeller serializes calls into one go-aspell speller. aspell keeps mutable
// per-speller state and go-aspell adds no locking, so concurrent handlers must
// not overlap calls; a mutex is cheaper than a speller pool.
type AspellSpeller struct {
	mu      *sync.Mutex
	speller dictionary
}

func (s AspellSpeller) mutex() *sync.Mutex {
	if s.mu == nil {
		return &fallbackMu
	}
	return s.mu
}

func NewAspellSpellingService() (AspellSpeller, error) {
	speller, err := aspell.NewSpeller(map[string]string{
		"lang": "en_US",
	})
	if err != nil {
		return AspellSpeller{}, err
	}

	return AspellSpeller{
		mu:      &sync.Mutex{},
		speller: &speller,
	}, nil
}

func (s AspellSpeller) GetSuggestions(q string) []string {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()

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
