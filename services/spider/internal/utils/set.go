package utils

import (
	"fmt"
	"sort"
)

// ordered is the constraint on T needed for GetAll to hand back a stable order.
// It is narrower than `comparable`, so a Set over an unordered comparable (a
// struct, say) would need this widened to cmp.Ordered.
type ordered interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64 | ~string
}

type Set[T ordered] struct {
	elements map[T]bool
}

func NewSet[T ordered]() *Set[T] {
	return &Set[T]{
		elements: map[T]bool{},
	}
}

func NewSetFromSlice[T ordered](slice []T) *Set[T] {
	s := NewSet[T]()
	s.BatchAdd(slice...)
	return s
}

// Add inserts t and reports whether it was already present. The name suggests
// the opposite, so it is spelled out here; no caller reads the result today.
func (s *Set[T]) Add(t T) bool {
	_, alreadyPresent := s.elements[t]
	s.elements[t] = true
	return alreadyPresent
}

func (s *Set[T]) BatchAdd(t ...T) {
	//
	for _, v := range t {
		s.elements[v] = true
	}
}

// GetAll returns every element in ascending order.
//
// The order is part of the contract, not an accident. The spider drains this
// list against a per-host page budget, so ranging over the map meant which
// links got crawled before the budget ran out changed on every process start,
// and re-running a crawl produced a different index.
func (s *Set[T]) GetAll() []T {
	t := make([]T, 0, len(s.elements))
	for v := range s.elements {
		t = append(t, v)
	}
	sort.Slice(t, func(i, j int) bool { return t[i] < t[j] })
	return t
}

func (s Set[T]) Contains(t T) bool {
	return s.elements[t]
}

func (s Set[T]) Print() {
	for url, ok := range s.elements {
		if ok {
			fmt.Println(url)
		}
	}
}

func (s Set[T]) Len() int {
	return len(s.elements)
}

// GetSize is Len under the name the rest of the utils in this package uses.
func (s *Set[T]) GetSize() int {
	return len(s.elements)
}
