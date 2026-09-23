package ranking

import (
	"math"
	"testing"
)

func TestDotProduct(t *testing.T) {
	tests := []struct {
		name     string
		vecA     []float64
		vecB     []float64
		expected float64
	}{
		{
			name:     "identical non-zero vectors",
			vecA:     []float64{1.0, 2.0, 3.0},
			vecB:     []float64{1.0, 2.0, 3.0},
			expected: 14.0, // 1*1 + 2*2 + 3*3 = 14
		},
		{
			name:     "orthogonal vectors",
			vecA:     []float64{1.0, 0.0},
			vecB:     []float64{0.0, 1.0},
			expected: 0.0,
		},
		{
			name:     "empty vectors",
			vecA:     []float64{},
			vecB:     []float64{},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dotProduct(tt.vecA, tt.vecB)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("dotProduct() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestMagnitude(t *testing.T) {
	tests := []struct {
		name     string
		vec      []float64
		expected float64
	}{
		{
			name:     "3D vector (3, 4, 0)",
			vec:      []float64{3.0, 4.0, 0.0},
			expected: 5.0,
		},
		{
			name:     "zero vector",
			vec:      []float64{0.0, 0.0},
			expected: 0.0,
		},
		{
			name:     "empty vector",
			vec:      []float64{},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := magnitude(tt.vec)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("magnitude() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		vecA     []float64
		vecB     []float64
		expected float64
	}{
		{
			name:     "parallel vectors",
			vecA:     []float64{1.0, 2.0},
			vecB:     []float64{2.0, 4.0},
			expected: 1.0,
		},
		{
			name:     "orthogonal vectors",
			vecA:     []float64{1.0, 0.0},
			vecB:     []float64{0.0, 1.0},
			expected: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.vecA, tt.vecB)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("cosineSimilarity() = %v, expected %v", got, tt.expected)
			}
		})
	}
}
