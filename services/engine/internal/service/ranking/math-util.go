package ranking

import (
	"math"
)

// cosineSimilarity assumes equal-length, non-negative vectors. A zero magnitude
// (a query with no scored terms) returns 0, keeping the blend finite so the
// deterministic tie-breaks decide.
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
