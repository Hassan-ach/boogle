package util

import "github.com/google/uuid"

type Mapper[k comparable] interface {
	MapValue(id k)
	GetIndex(id k) (int, bool)
	GetValues() []k
	GetSize() int
	GetValueByIndex(idx int) (k, bool)
}

type PageMapper struct {
	index     map[uuid.UUID]int
	reverse   []uuid.UUID
	nextIndex int
}

func NewPageMapper() *PageMapper {
	return &PageMapper{
		index:     make(map[uuid.UUID]int),
		reverse:   make([]uuid.UUID, 0, 1024),
		nextIndex: 0,
	}
}

func (m *PageMapper) MapValue(id uuid.UUID) {
	if _, exists := m.index[id]; !exists {
		m.index[id] = m.nextIndex
		m.reverse = append(m.reverse, id)
		m.nextIndex++
	}
}

func (m PageMapper) GetIndex(id uuid.UUID) (int, bool) {
	idx, ok := m.index[id]
	return idx, ok
}

func (m PageMapper) GetValues() []uuid.UUID {
	dst := make([]uuid.UUID, len(m.reverse))
	copy(dst, m.reverse)
	return dst
}

func (m PageMapper) GetSize() int {
	return m.nextIndex
}

func (m PageMapper) GetValueByIndex(idx int) (uuid.UUID, bool) {
	if idx < 0 || idx >= len(m.reverse) {
		return uuid.Nil, false
	}
	return m.reverse[idx], true
}

type WordMapper struct {
	index   map[string]int
	reverse []string
	nextIdx int
}

func NewWordMapper() *WordMapper {
	return &WordMapper{
		index:   make(map[string]int),
		reverse: make([]string, 0, 8),
		nextIdx: 0,
	}
}

func (m *WordMapper) MapValue(word string) {
	if _, exists := m.index[word]; !exists {
		m.index[word] = m.nextIdx
		m.reverse = append(m.reverse, word)
		m.nextIdx++
	}
}

func (m WordMapper) GetIndex(word string) (int, bool) {
	idx, ok := m.index[word]
	return idx, ok
}

func (m WordMapper) GetValues() []string {
	dst := make([]string, len(m.reverse))
	copy(dst, m.reverse)
	return dst
}

func (m WordMapper) GetSize() int {
	return m.nextIdx
}

func (m WordMapper) GetValueByIndex(idx int) (string, bool) {
	if idx < 0 || idx >= len(m.reverse) {
		return "", false
	}
	return m.reverse[idx], true
}
