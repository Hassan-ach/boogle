package ranking

import (
	"fmt"
	"slices"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"
)

// less breaks score ties deterministically, ending at the page ID. Without that
// final key, two pages with identical scores could swap between requests and
// make pagination repeat or skip results.
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

// sort blends each page's TF-IDF score with its PageRank into GlobalScore:
// factor weights TF-IDF and 1-factor weights PageRank. Callers pass 0.5.
func sort(pages map[*model.Page]float64,
	factor float64,
) ([]*model.Page, error) {
	pgs := make([]*model.Page, 0, len(pages))
	for p := range pages {
		p.GlobalScore = factor*pages[p] + (1-factor)*p.PRScore
		pgs = append(pgs, p)
	}

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

// docVector builds a length-N TF-IDF vector indexed by the word mapper. Words the
// mapper does not know panic: the mapper is built from the same corpus, so a miss
// means the store returned data the indexer never wrote.
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
