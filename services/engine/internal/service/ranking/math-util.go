package ranking

import (
	"fmt"
	"math"
)

// cosineSimilarity assumes equal-length, non-negative vectors. It can divide by
// zero when a document or the query has no scored terms at all; normalizeTFIDF
// absorbs that case downstream.
func cosineSimilarity(vecA, vecB []float64) float64 {
	return dotProduct(vecA, vecB) / (magnitude(vecA) * magnitude(vecB))
}

func dotProduct(vecA, vecB []float64) float64 {
	dot := 0.0
	for i := range vecA {
		dot += vecA[i] * vecB[i]
	}
	// A NaN here means an IDF or term-frequency value was garbage. Refusing to
	// continue beats returning NaN scores, which sort unpredictably and would
	// quietly poison the result page instead of failing loudly.
	if math.IsNaN(dot) {
		panic(fmt.Sprintf("dot product is NaN for vecA: %v, vecB: %v", vecA, vecB))
	}

	return dot
}

func magnitude(vec []float64) float64 {
	sum := 0.0
	for _, v := range vec {
		sum += v * v
	}
	if math.IsNaN(sum) {
		panic(fmt.Sprintf("magnitude sum is NaN for vec: %v", vec))
	}
	return math.Sqrt(sum)
}
