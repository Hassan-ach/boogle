package util

import (
	"testing"

	"github.com/google/uuid"
)

func TestPageMapperAssignsDenseIndicesInInsertionOrder(t *testing.T) {
	m := NewPageMapper()
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	m.MapValue(a)
	m.MapValue(b)
	m.MapValue(c)

	for i, want := range []uuid.UUID{a, b, c} {
		got, ok := m.GetIndex(want)
		if !ok {
			t.Fatalf("GetIndex(%v) reported the id as unmapped", want)
		}
		if got != i {
			t.Errorf("GetIndex(%v) = %d, want %d", want, got, i)
		}
	}
	if m.GetSize() != 3 {
		t.Errorf("GetSize() = %d, want 3", m.GetSize())
	}
}

func TestPageMapperIgnoresDuplicateIds(t *testing.T) {
	m := NewPageMapper()
	a := uuid.New()

	m.MapValue(a)
	m.MapValue(a)
	m.MapValue(a)

	if m.GetSize() != 1 {
		t.Errorf("GetSize() = %d after three identical inserts, want 1", m.GetSize())
	}
	idx, ok := m.GetIndex(a)
	if !ok || idx != 0 {
		t.Errorf("GetIndex = (%d, %v), want (0, true)", idx, ok)
	}
}

func TestPageMapperGetIndexReportsMissingIds(t *testing.T) {
	m := NewPageMapper()
	m.MapValue(uuid.New())

	idx, ok := m.GetIndex(uuid.New())
	if ok {
		t.Error("GetIndex must report an unmapped id as absent")
	}
	if idx != 0 {
		t.Errorf("GetIndex returned index %d for a missing id, want the zero value", idx)
	}
}

func TestPageMapperGetValueByIndexRoundTrips(t *testing.T) {
	m := NewPageMapper()
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		ids[i] = uuid.New()
		m.MapValue(ids[i])
	}

	for i, want := range ids {
		got, ok := m.GetValueByIndex(i)
		if !ok {
			t.Fatalf("GetValueByIndex(%d) reported an out-of-range index", i)
		}
		if got != want {
			t.Errorf("GetValueByIndex(%d) = %v, want %v", i, got, want)
		}
	}
}

func TestPageMapperGetValueByIndexRejectsOutOfRange(t *testing.T) {
	m := NewPageMapper()
	m.MapValue(uuid.New())

	for _, idx := range []int{-1, 1, 2, 1 << 30} {
		got, ok := m.GetValueByIndex(idx)
		if ok {
			t.Errorf("GetValueByIndex(%d) accepted an out-of-range index", idx)
		}
		if got != uuid.Nil {
			t.Errorf("GetValueByIndex(%d) = %v, want uuid.Nil", idx, got)
		}
	}
}

func TestPageMapperGetValueByIndexOnEmptyMapper(t *testing.T) {
	m := NewPageMapper()

	if _, ok := m.GetValueByIndex(0); ok {
		t.Error("an empty mapper must not report index 0 as valid")
	}
	if m.GetSize() != 0 {
		t.Errorf("GetSize() = %d, want 0", m.GetSize())
	}
	if len(m.GetValues()) != 0 {
		t.Error("GetValues() must be empty for an empty mapper")
	}
}

func TestPageMapperGetValuesReturnsACopy(t *testing.T) {
	m := NewPageMapper()
	a, b := uuid.New(), uuid.New()
	m.MapValue(a)
	m.MapValue(b)

	values := m.GetValues()
	values[0] = uuid.Nil

	if got, _ := m.GetValueByIndex(0); got != a {
		t.Errorf("GetValueByIndex(0) = %v after the caller mutated its slice, want %v", got, a)
	}
	if got := m.GetValues(); got[0] != a {
		t.Errorf("GetValues()[0] = %v, want %v", got[0], a)
	}
}

func TestPageMapperGetValuesIsInIndexOrder(t *testing.T) {
	m := NewPageMapper()
	var ids []uuid.UUID
	for i := 0; i < 4; i++ {
		id := uuid.New()
		ids = append(ids, id)
		m.MapValue(id)
	}

	values := m.GetValues()
	if len(values) != len(ids) {
		t.Fatalf("GetValues() returned %d ids, want %d", len(values), len(ids))
	}
	for _, want := range ids {
		idx, _ := m.GetIndex(want)
		if values[idx] != want {
			t.Errorf("GetValues()[%d] = %v, want %v (GetIndex said %d)", idx, values[idx], want, idx)
		}
	}
}

func TestPageMapperSatisfiesTheMapperInterface(t *testing.T) {
	var _ Mapper[uuid.UUID] = NewPageMapper()
	var _ Mapper[string] = NewWordMapper()
}

func TestWordMapperAssignsDenseIndicesInInsertionOrder(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("cat")
	m.MapValue("dog")
	m.MapValue("bird")

	for i, want := range []string{"cat", "dog", "bird"} {
		got, ok := m.GetIndex(want)
		if !ok {
			t.Fatalf("GetIndex(%q) reported the word as unmapped", want)
		}
		if got != i {
			t.Errorf("GetIndex(%q) = %d, want %d", want, got, i)
		}
	}
	if m.GetSize() != 3 {
		t.Errorf("GetSize() = %d, want 3", m.GetSize())
	}
}

func TestWordMapperIgnoresDuplicateWords(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("cat")
	m.MapValue("cat")
	m.MapValue("cat")

	if m.GetSize() != 1 {
		t.Errorf("GetSize() = %d after three identical inserts, want 1", m.GetSize())
	}
	if got := m.GetValues(); len(got) != 1 || got[0] != "cat" {
		t.Errorf("GetValues() = %v, want [cat]", got)
	}
}

func TestWordMapperGetIndexReportsMissingWords(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("cat")

	idx, ok := m.GetIndex("elephant")
	if ok {
		t.Error("GetIndex must report an unmapped word as absent")
	}
	if idx != 0 {
		t.Errorf("GetIndex returned %d for a missing word, want the zero value", idx)
	}
}

func TestWordMapperGetValueByIndexRoundTrips(t *testing.T) {
	m := NewWordMapper()
	words := []string{"alpha", "beta", "gamma"}
	for _, w := range words {
		m.MapValue(w)
	}

	for i, want := range words {
		got, ok := m.GetValueByIndex(i)
		if !ok {
			t.Fatalf("GetValueByIndex(%d) reported an out-of-range index", i)
		}
		if got != want {
			t.Errorf("GetValueByIndex(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestWordMapperGetValueByIndexRejectsOutOfRange(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("cat")

	for _, idx := range []int{-1, 1, 99, 1 << 30} {
		got, ok := m.GetValueByIndex(idx)
		if ok {
			t.Errorf("GetValueByIndex(%d) accepted an out-of-range index", idx)
		}
		if got != "" {
			t.Errorf("GetValueByIndex(%d) = %q, want an empty string", idx, got)
		}
	}
}

func TestWordMapperGetValuesReturnsACopy(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("cat")
	m.MapValue("dog")

	values := m.GetValues()
	values[0] = "mutated"

	if got, _ := m.GetValueByIndex(0); got != "cat" {
		t.Errorf("GetValueByIndex(0) = %q after the caller mutated its slice, want %q", got, "cat")
	}
	if got := m.GetValues(); got[0] != "cat" {
		t.Errorf("GetValues()[0] = %q, want %q", got[0], "cat")
	}
}

func TestWordMapperHandlesTheEmptyString(t *testing.T) {
	m := NewWordMapper()
	m.MapValue("")

	idx, ok := m.GetIndex("")
	if !ok || idx != 0 {
		t.Errorf("GetIndex(\"\") = (%d, %v), want (0, true)", idx, ok)
	}
	if m.GetSize() != 1 {
		t.Errorf("GetSize() = %d, want 1", m.GetSize())
	}
}

func TestWordMapperIndicesAreContiguousFromZero(t *testing.T) {
	m := NewWordMapper()
	words := make([]string, 500)
	for i := range words {
		words[i] = uuid.New().String()
	}
	for _, i := range []int{499, 0, 250, 1, 498, 2} {
		m.MapValue(words[i])
	}
	for _, w := range words {
		m.MapValue(w)
	}

	if m.GetSize() != len(words) {
		t.Fatalf("GetSize() = %d, want %d", m.GetSize(), len(words))
	}
	seen := make([]bool, len(words))
	for _, w := range words {
		idx, ok := m.GetIndex(w)
		if !ok {
			t.Fatalf("GetIndex(%q) reported the word as unmapped", w)
		}
		if idx < 0 || idx >= len(words) {
			t.Fatalf("GetIndex(%q) = %d, outside [0, %d)", w, idx, len(words))
		}
		if seen[idx] {
			t.Fatalf("index %d was handed out twice", idx)
		}
		seen[idx] = true
	}
}
