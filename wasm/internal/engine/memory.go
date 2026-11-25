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
// can be read by other threads while it grows. On 32-bit platforms only a
// shared memory does. Elsewhere a memory is a Go slice, reallocated when it
// grows past its capacity; a shared memory there is made at its maximum size
// at once.
//
// A reserved memory returns its reservation to the OS when the Memory is
// collected, so whatever holds its bytes must keep the Memory reachable,
// or hold the memory (Hold) while it uses them.
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

	// hostView and hostGrown are what a host keeps with a shared memory,
	// made once by HostView: a view of the memory, and what each grow tells
	// of the memory's new size, under mu, before Grow returns.
	hostView  any
	hostGrown func(newBytes uint64)

	// cleanup returns the reservation to the OS when the Memory is
	// collected, unless a host holds the memory; holds counts the hosts'
	// holds (Hold), under mu.
	cleanup runtime.Cleanup
	holds   int
}

// Hold takes a host's hold on the memory: while one is held, the
// memory's bytes are not released, even once the Memory is collected, so
// that a view of them the host gave out cannot outlive them while the
// host is still using the memory. Drop gives a hold back; once the last is
// given back the bytes go when the Memory is collected, as they would have
// without a hold. A host that never gives a hold back keeps the bytes for
// as long as the process runs.
func (m *Memory) Hold() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds++
	if m.holds == 1 {
		m.cleanup.Stop()
		m.cleanup = runtime.Cleanup{}
	}
}

// Drop gives back a hold Hold took.
func (m *Memory) Drop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds--
	if m.holds == 0 && m.reservation != nil {
		m.cleanup = runtime.AddCleanup(m, release, m.reservation)
	}
}

// NewMemoryBudget makes a memory as NewMemory does, charged to b -- its
// size, and its growth from then on -- charging b first, so that a memory
// that does not fit is refused before any of it is allocated or reserved:
// it returns nil and false then, and nil and true for a memory that cannot
// be allocated.
func NewMemoryBudget(initial, max uint64, hasMax, is64, shared bool, b *Budget) (*Memory, bool) {
	n := int64(initial * PageSize)
	if mx := memoryMax(max, hasMax, is64); shared && !reserved(mx, shared) {
		// A shared memory that is a Go slice is made at its maximum.
		n = int64(mx * PageSize)
	}
	c, ok := reserveCharge(b, n)
	if !ok {
		return nil, false
	}
	m := NewMemory(initial, max, hasMax, is64, shared)
	if m == nil {
		c.drop()
		return nil, true
	}
	m.charge = c
	keepCharge(c, m)
	return m, true
}

// memoryMax is the maximum in pages of a memory declaring max, or none: the
// engine's limit at most.
func memoryMax(max uint64, hasMax, is64 bool) uint64 {
	limit := MaxMemory32Pages
	if is64 {
		limit = MaxMemory64Pages
	}
	if !hasMax || max > limit {
		max = limit
	}
	return max
}

// reserved reports whether a memory of max pages is made in a reservation
// of the OS's address space. On a 32-bit platform a reservation of each
// memory's maximum would soon fill the address space, so an unshared memory
// is a Go slice there. A shared one is made at its maximum either way, and a
// reservation that cannot be had fails, where a Go allocation that cannot be
// had ends the process. A memory that can have no pages has nothing to
// reserve.
func reserved(max uint64, shared bool) bool {
	return canReserve && max > 0 && (bits.UintSize == 64 || shared)
}

// NewMemory makes a memory of initial pages, growable to max pages. It
// returns nil if the memory cannot be allocated.
func NewMemory(initial, max uint64, hasMax, is64, shared bool) *Memory {
	max = memoryMax(max, hasMax, is64)
	if initial > max {
		return nil
	}
	m := &Memory{Max: max, Is64: is64, Shared: shared, HasMax: hasMax}
	if reserved(max, shared) {
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
			m.cleanup = runtime.AddCleanup(m, release, r)
			return m
		}
		if shared {
			// A Go slice of the same size would not be had either.
			return nil
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
// Its capacity is its length: what lies past is not the memory's yet, nor,
// in a reservation, memory at all.
func (m *Memory) Bytes() []byte {
	if m.Shared {
		n := m.size.Load()
		return m.buf[:n:n]
	}
	return m.buf[:len(m.buf):len(m.buf)]
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

// Span returns a shared memory's bytes up to its maximum, which never move:
// those past its size are not the memory's yet, nor, in a reservation,
// memory at all, and are not to be touched. It returns nil for an unshared
// memory.
func (m *Memory) Span() []byte {
	if !m.Shared {
		return nil
	}
	return m.buf
}

// HostView returns a shared memory's host view, made by make the first time
// it is asked for, which also returns what the memory tells of each grow
// from then on, with its new size in bytes. make and the function it
// returns are called with the memory's grows held off, and must not grow
// it.
func (m *Memory) HostView(make func() (view any, grown func(newBytes uint64))) any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hostView == nil {
		m.hostView, m.hostGrown = make()
	}
	return m.hostView
}

// Unbudget stops charging a shared memory's growth to the budget it is
// charged to, keeping what it has charged: once other runtimes can grow the
// memory, from goroutines of their own, its growth is no longer one budget's
// to bound as it happens.
func (m *Memory) Unbudget() {
	m.mu.Lock()
	m.charge = nil
	m.mu.Unlock()
}

// GrowTo grows a shared memory to pages pages, reporting false if it cannot:
// it is larger than that already -- another thread may have grown it since
// the caller looked -- or the pages are past its maximum or not to be had.
// It reports true for a memory of pages pages already.
func (m *Memory) GrowTo(pages uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.Pages()
	return pages >= old && m.grow(pages-old) >= 0
}

// Grow grows the memory by delta pages, returning its old size in pages, or
// -1 if it cannot: past its maximum, or the memory is not to be had.
func (m *Memory) Grow(delta uint64) int64 {
	if m.Shared {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	return m.grow(delta)
}

// grow is Grow, with a shared memory's lock held.
func (m *Memory) grow(delta uint64) int64 {
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
	// A shared memory that is a Go slice was charged its maximum when made.
	charged := int64(delta * PageSize)
	if m.Shared && m.reservation == nil {
		charged = 0
	}
	if !m.charge.take(charged) {
		return -1
	}
	switch {
	case m.reservation != nil:
		if err := commit(m.reservation, oldBytes, newBytes); err != nil {
			m.charge.giveBack(charged)
			return -1
		}
		if !m.Shared {
			m.buf = m.reservation[:newBytes]
		}
	case m.Shared:
		// Made at its maximum.
	default:
		if newBytes > uint64(maxSlice) {
			m.charge.giveBack(charged)
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
	if m.hostGrown != nil {
		m.hostGrown(newBytes)
	}
	if m.OnGrow != nil {
		m.OnGrow(oldBytes, newBytes)
	}
	return int64(old)
}

// Base returns the address of the memory's first byte, for native code, or
// nil for a memory with no bytes and no reservation.
func (m *Memory) Base() unsafe.Pointer { return unsafe.Pointer(unsafe.SliceData(m.buf)) }
