package utils

import (
	"sort"
	"testing"
)

func TestGetAllIsDeterministic(t *testing.T) {
	build := func() *Set[string] {
		s := NewSet[string]()
		for i := 0; i < 50; i++ {
			s.Add(string(rune('a' + i%26)))
			s.Add(string(rune('A' + i%26)))
		}
		return s
	}

	first := build().GetAll()
	want := append([]string(nil), first...)
	sort.Strings(want)

	for i := 0; i < 200; i++ {
		got := build().GetAll()
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d elements, want %d", i, len(got), len(want))
		}
		gotSorted := append([]string(nil), got...)
		sort.Strings(gotSorted)
		for j := range want {
			if gotSorted[j] != want[j] {
				t.Fatalf("run %d: element set changed: %v vs %v", i, gotSorted, want)
			}
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d: position %d = %q, want %q\n got %v\nwant %v",
					i, j, got[j], first[j], got, first)
			}
		}
	}
}

func TestGetAllIsSorted(t *testing.T) {
	s := NewSet[string]()
	for _, v := range []string{"zebra", "apple", "mango", "banana"} {
		s.Add(v)
	}

	got := s.GetAll()

	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("GetAll() is not sorted: %v", got)
		}
	}
}

func TestGetAllOnAnEmptySet(t *testing.T) {
	got := NewSet[int]().GetAll()

	if len(got) != 0 {
		t.Errorf("GetAll() = %v, want an empty slice", got)
	}
}

func TestGetAllDoesNotAliasInternalState(t *testing.T) {
	s := NewSet[string]()
	s.BatchAdd("a", "b", "c")

	first := s.GetAll()
	first[0] = "mutated"

	second := s.GetAll()
	for _, v := range second {
		if v == "mutated" {
			t.Fatalf("GetAll() handed out the backing array: %v", second)
		}
	}
	if len(second) != 3 {
		t.Errorf("len = %d, want 3", len(second))
	}
}

func TestAddReportsWhetherTheValueWasAlreadyPresent(t *testing.T) {
	s := NewSet[string]()

	if s.Add("a") {
		t.Error("Add of a new value reported it as already present")
	}
	if !s.Add("a") {
		t.Error("Add of an existing value did not report it as already present")
	}
	if s.GetSize() != 1 {
		t.Errorf("size = %d, want 1", s.GetSize())
	}
}

func TestContains(t *testing.T) {
	s := NewSet[string]()
	s.BatchAdd("a", "b")

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"a", true},
		{"b", true},
		{"c", false},
		{"", false},
		{"A", false},
	} {
		if got := s.Contains(tc.value); got != tc.want {
			t.Errorf("Contains(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestNewSetFromSlice(t *testing.T) {
	s := NewSetFromSlice([]string{"x", "y", "x", "z", "y"})

	if s.GetSize() != 3 {
		t.Errorf("GetSize() = %d, want 3", s.GetSize())
	}
	got := s.GetAll()
	want := []string{"x", "y", "z"}
	if len(got) != len(want) {
		t.Fatalf("GetAll() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GetAll()[%d] = %q, want %q (want %v)", i, got[i], want[i], want)
		}
	}
}

func TestNewSetFromSliceOnEmpty(t *testing.T) {
	if got := NewSetFromSlice([]int{}).GetSize(); got != 0 {
		t.Errorf("GetSize() = %d, want 0", got)
	}
}

func TestSetWorksForNonStringTypes(t *testing.T) {
	s := NewSet[int]()
	s.BatchAdd(3, 1, 2, 1)

	got := s.GetAll()
	if len(got) != 3 {
		t.Fatalf("GetAll() = %v, want 3 elements", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("GetAll() is not sorted: %v", got)
		}
	}
}
