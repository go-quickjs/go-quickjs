package engine

// ReserveCapacity makes a memory that is a Go slice hold pages pages, at most
// its maximum, without moving its bytes when it grows to them, so that a view
// of its bytes stays valid across those grows. A memory whose address space is
// reserved, or a shared one, never moves; for them it does nothing, as it does
// when the capacity cannot be had.
func (m *Memory) ReserveCapacity(pages uint64) {
	if m.reservation != nil || m.Shared {
		return
	}
	pages = min(pages, m.Max)
	if pages > uint64(maxSlice)/PageSize {
		return
	}
	n := pages * PageSize
	if n <= uint64(cap(m.buf)) {
		return
	}
	grown := make([]byte, len(m.buf), n)
	copy(grown, m.buf)
	m.buf = grown
}
