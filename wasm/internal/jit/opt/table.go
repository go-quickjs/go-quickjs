package opt

import "math/bits"

// u64map is a hash table of uint64 keys, open-addressed, that a compile
// empties in constant time: a slot holds an entry only while its stamp is
// the table's. Kept in a workspace, it keeps its arrays for the next
// compile, where a Go map's clear costs its whole capacity again on every
// compile after a large one; a small function uses the arrays' first
// slots alone.
type u64map[V any] struct {
	keys  []uint64
	vals  []V
	stamp []uint32
	gen   uint32
	n     int
	mask  int
	shift uint8
	// The entries being placed again as the table grows.
	tmpKeys  []uint64
	tmpVals  []V
	tmpStamp []uint32
}

// reset empties m for about hint entries.
func (m *u64map[V]) reset(hint int) {
	size := 16
	for size < 2*hint {
		size *= 2
	}
	m.n = 0
	m.size(size)
	m.gen++
	if m.gen == 0 {
		clear(m.stamp)
		m.gen = 1
	}
}

// size uses size slots, which it makes if m has fewer.
func (m *u64map[V]) size(size int) {
	if len(m.keys) < size {
		m.keys, m.vals, m.stamp, m.gen = make([]uint64, size), make([]V, size), make([]uint32, size), 0
	}
	m.mask = size - 1
	m.shift = uint8(64 - bits.TrailingZeros(uint(size)))
}

func (m *u64map[V]) slot(k uint64) int {
	return int((k * 0x9e3779b97f4a7c15) >> m.shift)
}

// get returns k's value, and whether it has one.
func (m *u64map[V]) get(k uint64) (V, bool) {
	for i := m.slot(k); m.stamp[i] == m.gen; i = (i + 1) & m.mask {
		if m.keys[i] == k {
			return m.vals[i], true
		}
	}
	var zero V
	return zero, false
}

// set makes v k's value.
func (m *u64map[V]) set(k uint64, v V) {
	i := m.slot(k)
	for ; m.stamp[i] == m.gen; i = (i + 1) & m.mask {
		if m.keys[i] == k {
			m.vals[i] = v
			return
		}
	}
	m.keys[i], m.vals[i], m.stamp[i] = k, v, m.gen
	m.n++
	if 2*m.n > m.mask+1 {
		m.grow()
	}
}

// grow doubles the slots m uses, its entries placed again.
func (m *u64map[V]) grow() {
	n := m.mask + 1
	keys := append(m.tmpKeys[:0], m.keys[:n]...)
	vals := append(m.tmpVals[:0], m.vals[:n]...)
	stamp := append(m.tmpStamp[:0], m.stamp[:n]...)
	m.tmpKeys, m.tmpVals, m.tmpStamp = keys, vals, stamp
	gen := m.gen
	m.size(2 * n)
	m.gen++
	if m.gen == 0 {
		clear(m.stamp)
		m.gen = 1
	}
	m.n = 0
	for i := range keys {
		if stamp[i] == gen {
			m.set(keys[i], vals[i])
		}
	}
}

// at is k's value, or the zero value.
func (m *u64map[V]) at(k uint64) V {
	v, _ := m.get(k)
	return v
}
