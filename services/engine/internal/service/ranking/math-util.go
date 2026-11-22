package ranking

import (
	"math"
)

// cosineSimilarity assumes equal-length, non-negative vectors.
//
// A zero magnitude is normal: a single-term query for a word whose IDF was
// clamped to 0 has no scored terms. Dividing produced NaN, which propagated
// through normalizeTFIDF and made the comparator return 0 for every pair, so
// results came out in map-iteration order (M-02). A neutral 0 keeps the blend
// finite and lets the deterministic tie-breaks decide.
func cosineSimilarity(vecA, vecB []float64) float64 {
	denom := magnitude(vecA) * magnitude(vecB)
	if denom == 0 || math.IsNaN(denom) || math.IsInf(denom, 0) {
		return 0
	}
	score := dotProduct(vecA, vecB) / denom
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}

func dotProduct(vecA, vecB []float64) float64 {
	dot := 0.0
	for i := range vecA {
		dot += vecA[i] * vecB[i]
	}
	if math.IsNaN(dot) {
		return 0
	}

	return dot
}

func magnitude(vec []float64) float64 {
	sum := 0.0
	for _, v := range vec {
		sum += v * v
	}
	if math.IsNaN(sum) || sum <= 0 {
		return 0
	}
	return math.Sqrt(sum)
}
