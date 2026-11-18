package engine

import ()

// bytes returns the memory's bytes at its size.
func (m *Memory) bytes() []byte {
	if m.Shared {
		return m.buf[:m.size.Load()]
	}
	return m.buf
}

// span returns the n bytes at effective address ea, trapping if any is out
// of bounds, as the instruction at offset at.
func (m *Memory) span(ea, n uint64, at uint64) []byte {
	b := m.bytes()
	if ea > uint64(len(b)) || n > uint64(len(b))-ea {
		trapAt(TrapMemOutOfBounds, at)
	}
	return b[ea : ea+n]
}

// checkRange traps with code, as the instruction at offset at, unless
// [start, start+n) lies within size.
func checkRange(start, n, size uint64, code TrapCode, at uint64) {
	if start > size || n > size-start {
		trapAt(code, at)
	}
}
