package ranking

import (
	"fmt"

	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"
)

// tfIdf scores every page against the query as cosine similarity between the
// page's TF-IDF vector and the query's. Pages are placed in a dense matrix by
// their mapper index, so scoring is O(pages * query terms) rather than a scan
// per page.
func tfIdf(
	data *store.Data,
) (map[*model.Page]float64, error) {
	wordIdf := data.Idf
	pages := data.Pages
	pageMapper := data.PageMapper
	wordMapper := data.WordMapper
	query := wordMapper.GetValues()

	M := make([][]float64, len(pages))
	for _, page := range pages {
		// The mapper is built from the same page list as this loop, so a miss
		// means Data was assembled inconsistently. That is a bug, not a runtime
		// condition to recover from, hence the panic.
		idx, ok := pageMapper.GetIndex(page.ID)
		if !ok {
			panic(fmt.Sprintf("page with ID %s not found in page mapper", page.ID))
		}

		M[idx] = docVector(page, wordIdf, wordMapper, len(query))
	}

	queryVec := queryVector(query, wordIdf, wordMapper)
	docScores := make(map[*model.Page]float64, len(pages))
	for _, page := range pages {
		idx, ok := pageMapper.GetIndex(page.ID)
		if !ok {
			panic(fmt.Sprintf("page with ID %s not found in page mapper", page.ID))
		}
		score := cosineSimilarity(M[idx], queryVec)
		docScores[page] = score
	}

	return docScores, nil
}

func tf(query []string) map[string]int {
	tf := make(map[string]int, len(query))

	for _, word := range query {
		tf[word]++
	}
	return tf
}

// normalizeTFIDF rescales raw cosine similarity to 0..1 so it can be blended
// with PageRank, which is on its own scale. This changes the order within the
// cosine scores but not the final ranking, since sort multiplies by a positive
// weight.
//
// When every page scores identically the range is zero, so all scores become 0
// and ordering falls entirely to PageRank.
func normalizeTFIDF(pages map[*model.Page]float64) {
	if len(pages) == 0 {
		return
	}

	var minTFIDF, maxTFIDF float64
	first := true

	for _, v := range pages {
		if first {
			minTFIDF = v
			maxTFIDF = v
			first = false
			continue
		}
		if v > maxTFIDF {
			maxTFIDF = v
		}
		if v < minTFIDF {
			minTFIDF = v
		}
	}

	dom := maxTFIDF - minTFIDF

	for p, v := range pages {
		if dom == 0 {
			pages[p] = 0
		} else {
			pages[p] = (v - minTFIDF) / dom
		}
	}
}
