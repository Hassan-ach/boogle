package ranking

import (
	"math"
	"testing"
)

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name   string
		inputA []float64
		inputB []float64
		want   float64
	}{
		{
			name:   "cosine similarity of two vectors",
			inputA: []float64{3, 2, 0, 5},
			inputB: []float64{1, 0, 0, 0},
			want:   0.49,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.inputA, tt.inputB)

			if math.Abs(got-tt.want) > 0.01 {
				t.Errorf("cosineSimilarity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDotProduct(t *testing.T) {

	tests := []struct {
		name   string
		inputA []float64
		inputB []float64
		want   float64
	}{
		{
			name:   "dot product of two vectors",
			inputA: []float64{1, 2, 3},
			inputB: []float64{4, 5, 6},
			want:   32,
		},
		{
			name:   "dot product of two vectors with negative values",
			inputA: []float64{-1, -2, -3},
			inputB: []float64{-4, -5, -6},
			want:   32,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			got := dotProduct(tt.inputA, tt.inputB)
			if got != tt.want {
				t.Errorf("dotProduct() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMagnitude(t *testing.T) {

	tests := []struct {
		name  string
		input []float64
		want  float64
	}{
		{
			name:  "magnitude of zero vector",
			input: []float64{0, 0, 0},
			want:  0,
		},
		{
			name:  "magnitude of unit vector",
			input: []float64{1, 0, 0},
			want:  1,
		},
		{
			name:  "magnitude of vector with negative values",
			input: []float64{-3, -4},
			want:  5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := magnitude(tt.input)
			if got != tt.want {
				t.Errorf("magnitude() = %v, want %v", got, tt.want)
			}
		})
	}
}
