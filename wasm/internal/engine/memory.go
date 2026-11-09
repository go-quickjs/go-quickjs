// Package engine runs WebAssembly: the runtime objects a module's instances
// are made of -- memories, tables, globals, tags, references and the objects
// they point to -- instantiation, and the interpreter.
package engine

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// PageSize is the size of a WebAssembly page.
const PageSize = 1 << 16

// osPage is the granularity memory is committed in: a whole number of OS
// pages on every platform, as a memory grows by whole wasm pages.
const osPage = PageSize

// Engine limits on a memory's size, in pages: 4 GiB for an i32 memory and 16
// GiB for an i64 one on 64-bit hosts, 2 GiB less a page on 32-bit ones.
var (
	MaxMemory32Pages = uint64(65_536)
	MaxMemory64Pages = uint64(262_144)
)

func init() {
	if bits.UintSize == 32 {
		MaxMemory32Pages, MaxMemory64Pages = 32_767, 32_767
	}
}

// Memory is a linear memory.
//
// Where the platform allows, a memory reserves the address space of its
// maximum when it is made and commits pages as it grows, so that its bytes
// never move: a view of them stays valid across a grow, and a shared memory
// can be read by other threads while it grows. Elsewhere it is a Go slice,
// reallocated when it grows past its capacity; a shared memory there is made
// at its maximum size at once.
//
// A reserved memory returns its reservation to the OS when the Memory is
// collected, so whatever holds its bytes must keep the Memory reachable.
type Memory struct {
	// buf is the memory's bytes. For an unshared memory len(buf) is its size;
	// for a shared one buf spans the maximum and size holds the size, which
	// other threads read while it grows.
	buf  []byte
	size atomic.Uint64

	// Max is the maximum size in pages: the declared maximum, or the engine's
	// limit without one.
	Max    uint64
	Is64   bool
	Shared bool
	// HasMax reports a declared maximum.
	HasMax bool

	// reservation is the OS reservation buf is in, or nil.
	reservation []byte
	// mu serializes grows of a shared memory.
	mu sync.Mutex

	// OnGrow, if set, is called after the memory grows, with its old and new
	// sizes in bytes. A host keeping views of the memory refreshes them.
	OnGrow func(oldBytes, newBytes uint64)

	// charge is what the memory holds of a budget, if it is charged to one.
	charge *charge
}

// SetBudget charges the memory, and its growth from now on, to b,
// reporting false, and charging nothing, if its present size does not fit.
func (m *Memory) SetBudget(b *Budget) bool {
	c, ok := newCharge(m, b, int64(m.Size()))
	m.charge = c
	return ok
}

// NewMemory makes a memory of initial pages, growable to max pages. It
// returns nil if the memory cannot be allocated.
func NewMemory(initial, max uint64, hasMax, is64, shared bool) *Memory {
	limit := MaxMemory32Pages
	if is64 {
		limit = MaxMemory64Pages
	}
	if !hasMax || max > limit {
		max = limit
	}
	if initial > max {
		return nil
	}
	m := &Memory{Max: max, Is64: is64, Shared: shared, HasMax: hasMax}
	if reservable {
		r, err := reserve(max * PageSize)
		if err == nil {
			if err := commit(r, 0, initial*PageSize); err != nil {
				release(r)
				return nil
			}
			m.reservation = r
			if shared {
				m.buf = r
			} else {
				m.buf = r[:initial*PageSize]
			}
			m.size.Store(initial * PageSize)
			runtime.AddCleanup(m, release, r)
			return m
		}
	}
	n := initial * PageSize
	if shared {
		n = max * PageSize
	}
	if n > uint64(maxSlice) {
		return nil
	}
	if shared {
		m.buf = make([]byte, n)
	} else {
		m.buf = make([]byte, initial*PageSize)
	}
	m.size.Store(initial * PageSize)
	return m
}

// maxSlice is the largest slice the platform can make.
const maxSlice = int(^uint(0) >> 1)

// Bytes returns the memory's bytes at its current size. The slice of an
// unshared memory is valid until the memory grows; a shared memory's is
// valid as long as the memory, but other threads may grow the memory past it.
func (m *Memory) Bytes() []byte {
	if m.Shared {
		return m.buf[:m.size.Load()]
	}
	return m.buf
}

// Size returns the memory's size in bytes.
func (m *Memory) Size() uint64 {
	if m.Shared {
		return m.size.Load()
	}
	return uint64(len(m.buf))
}

// Pages returns the memory's size in pages.
func (m *Memory) Pages() uint64 { return m.Size() / PageSize }

// Grow grows the memory by delta pages, returning its old size in pages, or
// -1 if it cannot: past its maximum, or the memory is not to be had.
func (m *Memory) Grow(delta uint64) int64 {
	if m.Shared {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	old := m.Pages()
	if delta > m.Max-old {
		return -1
	}
	if delta == 0 {
		// A grow by nothing still tells a host keeping views: memory.grow
		// refreshes them whatever it adds.
		if m.OnGrow != nil {
			m.OnGrow(old*PageSize, old*PageSize)
		}
		return int64(old)
	}
	newPages := old + delta
	oldBytes, newBytes := old*PageSize, newPages*PageSize
	if !m.charge.take(int64(delta * PageSize)) {
		return -1
	}
	switch {
	case m.reservation != nil:
		if err := commit(m.reservation, oldBytes, newBytes); err != nil {
			m.charge.giveBack(int64(delta * PageSize))
			return -1
		}
		if !m.Shared {
			m.buf = m.reservation[:newBytes]
		}
	case m.Shared:
		// Made at its maximum.
	default:
		if newBytes > uint64(maxSlice) {
			m.charge.giveBack(int64(delta * PageSize))
			return -1
		}
		if newBytes <= uint64(cap(m.buf)) {
			m.buf = m.buf[:newBytes]
			clear(m.buf[oldBytes:])
		} else {
			c := max(newBytes, min(2*uint64(cap(m.buf)), m.Max*PageSize))
			if c > uint64(maxSlice) {
				c = newBytes
			}
			grown := make([]byte, newBytes, c)
			copy(grown, m.buf)
			m.buf = grown
		}
	}
	m.size.Store(newBytes)
	if m.OnGrow != nil {
		m.OnGrow(oldBytes, newBytes)
	}
	return int64(old)
}

// Base returns the address of the memory's first byte, for native code, or
// nil for a memory with no bytes and no reservation.
func (m *Memory) Base() unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(m.buf)) }
