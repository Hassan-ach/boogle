package ranking

import (
	"fmt"
	"slices"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"
)

// less reports whether a must be ordered before b, best match first.
//
// It has to be a total order, and above all asymmetric. The old comparator's
// `return -1` fall-through made less(a, b) and less(b, a) both true for pages
// that tied on every key, so the sorted order depended on the input order --
// and the input came out of a Go map, so the same query could come back ranked
// differently run to run.
//
// Score, word count and title are the meaningful keys. URL and ID are the
// deterministic backstop for the case where two distinct pages agree on all
// three, so no pair is ever left to the randomised map iteration order.
func less(a, b *model.Page) bool {
	switch {
	case a.GlobalScore != b.GlobalScore:
		return a.GlobalScore > b.GlobalScore
	case len(a.Words) != len(b.Words):
		return len(a.Words) > len(b.Words)
	case a.MetaData.Title != b.MetaData.Title:
		return a.MetaData.Title > b.MetaData.Title
	case a.URL != b.URL:
		return a.URL > b.URL
	default:
		return a.ID.String() > b.ID.String()
	}
}

func sort(pages map[*model.Page]float64,
	factor float64,
) ([]*model.Page, error) {
	pgs := make([]*model.Page, 0, len(pages))
	for p := range pages {
		p.GlobalScore = factor*pages[p] + (1-factor)*p.PRScore
		pgs = append(pgs, p)
	}

	// Deterministic even though the input is a map: every pair of distinct
	// pages is separated by `less`, so Go's randomised map iteration order
	// cannot leak into the result.
	slices.SortStableFunc(pgs, func(a, b *model.Page) int {
		switch {
		case less(a, b):
			return -1
		case less(b, a):
			return 1
		default:
			return 0
		}
	})

	return pgs, nil
}

func docVector(
	page *model.Page,
	wordIdf map[string]float64,
	wordMapper util.Mapper[string],
	N int,
) []float64 {
	vec := make([]float64, N)

	for w, tf := range page.Words {
		idf := wordIdf[w]
		wIdx, ok := wordMapper.GetIndex(w)
		if !ok {
			// this should never happen
			panic(fmt.Sprintf("word %s not found in word mapper", w))
		}
		vec[wIdx] = float64(tf) * idf
	}
	return vec
}

func queryVector(
	query []string,
	wordIdf map[string]float64,
	wordMapper util.Mapper[string],
) []float64 {
	queryTF := tf(query)
	return docVector(&model.Page{Words: queryTF}, wordIdf, wordMapper, len(query))
}
