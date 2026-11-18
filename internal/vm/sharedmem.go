package vm

import (
	"errors"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/atomicwait"
)

// Shared memory.
//
// A SharedArrayBuffer's bytes are a SharedMemory, which buffers in other
// runtimes -- other agents, running on other goroutines -- may be views of
// too. Two things follow. The memory must not move while another agent can
// see it, so a growable one reserves all it may ever grow to once it is
// shared, and grows in place from then on. And an Atomics operation on it must
// be atomic in fact rather than by there being no one else to see it, so it
// is done with sync/atomic, on memory allocated in 64-bit words so that every
// element an Atomics operation can name is aligned for it.
//
// A plain read or write of shared memory through a typed array is an ordinary
// one, and may tear, as the language's memory model allows.

// SharedMemory is the memory behind a SharedArrayBuffer, which may be shared
// between runtimes.
type SharedMemory struct {
	mu sync.Mutex
	// mem is the memory reserved, of which the first length bytes are the
	// buffer's. Before the memory is shared it may be replaced to grow;
	// after, it never is.
	mem    []byte
	length atomic.Int64
	// max is a growable buffer's maxByteLength, and -1 for one that cannot
	// grow.
	max int64
	// pinned records that the memory has been shared, and so may not move.
	pinned bool
	// host is set for memory a host package owns (HostSharedMemory).
	host *sharedHost
}

// waiter is an agent waiting in Atomics.wait or Atomics.waitAsync, in the
// process's table of waiters, which WebAssembly's waits share.
type waiter = atomicwait.Waiter

// waitTimer is how long a timeout of ms milliseconds is, and false for one
// too long to be anything but forever: a time.Duration holds 292 years, and
// a longer timeout overflowed it into one that had already run out.
func waitTimer(ms float64) (time.Duration, bool) {
	const most = float64(math.MaxInt64 / int64(time.Millisecond))
	if math.IsInf(ms, 1) || ms >= most {
		return 0, false
	}
	return time.Duration(ms * float64(time.Millisecond)), true
}

// alignedBytes allocates n bytes, and room for reserve, in 64-bit words.
func alignedBytes(n, reserve int) []byte {
	reserve = max(n, reserve)
	if reserve == 0 {
		return []byte{}
	}
	words := make([]uint64, (reserve+7)/8)
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(words))), len(words)*8)[:n:reserve]
}

// newSharedMemory makes the memory of a new SharedArrayBuffer: n bytes, and a
// growable one's maximum.
func newSharedMemory(n int, max int64) *SharedMemory {
	m := &SharedMemory{mem: alignedBytes(n, n), max: max}
	m.length.Store(int64(n))
	return m
}

// bytes is the memory's current extent.
func (m *SharedMemory) bytes() []byte {
	return m.mem[:m.length.Load()]
}

// grow makes the memory n bytes long, and reports false, changing nothing,
// when it is longer than that already: another agent may have grown it since
// the caller looked, and the check has to be made where no other can.
func (m *SharedMemory) grow(n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int64(n) < m.length.Load() {
		return false
	}
	if n > cap(m.mem) {
		// Only memory no other agent sees can move, and memory that is
		// shared has reserved its maximum.
		grown := alignedBytes(n, n)
		copy(grown, m.mem[:m.length.Load()])
		m.mem = grown
	}
	if int64(n) > m.length.Load() {
		m.length.Store(int64(n))
	}
	return true
}

// maxShareableReserve bounds what a growable buffer may reserve to be
// shared: all of it is allocated when it is.
const maxShareableReserve = 1 << 30

// errTooLargeToShare is a growable buffer whose maximum is more than can be
// reserved.
var errTooLargeToShare = errors.New("the SharedArrayBuffer's maxByteLength is too large to share")

// pin makes the memory stay where it is from now on, reserving a growable
// buffer's maximum.
func (m *SharedMemory) pin() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pinned {
		return nil
	}
	if m.max > int64(cap(m.mem)) {
		if m.max > maxShareableReserve {
			return errTooLargeToShare
		}
		n := int(m.length.Load())
		reserved := alignedBytes(n, int(m.max))
		copy(reserved, m.mem[:n])
		m.mem = reserved
	}
	m.pinned = true
	return nil
}

// sharedMemoryOf returns a buffer's shared memory, bringing the view of it up
// to date with growth another agent made.
func (b *arrayBufferData) sharedMemory() *SharedMemory {
	if b.block != nil {
		if n := int(b.block.length.Load()); n != len(b.bytes) || cap(b.bytes) != cap(b.block.mem) {
			b.bytes = b.block.bytes()
		}
	}
	return b.block
}

// SharedMemoryOf returns the memory behind a SharedArrayBuffer, pinned so that
// another runtime can be given a view of it, and whether v is one.
func (r *Runtime) SharedMemoryOf(v Value) (*SharedMemory, bool, error) {
	if !v.IsObject() || v.Object().class != ClassArrayBuffer {
		return nil, false, nil
	}
	b, ok := v.Object().dataOf().(*arrayBufferData)
	if !ok || b.block == nil {
		return nil, false, nil
	}
	if err := b.block.pin(); err != nil {
		return nil, true, err
	}
	// Pinning may have moved the memory, which only this runtime saw.
	b.sharedMemory()
	r.bufferChanged()
	return b.block, true, nil
}

// NewSharedArrayBuffer makes a SharedArrayBuffer of this runtime over memory
// another runtime shared.
func (r *Runtime) NewSharedArrayBuffer(m *SharedMemory) Value {
	o := newObject(r.proto.sharedArrayBuffer, ClassArrayBuffer)
	b := &arrayBufferData{bytes: m.bytes(), shared: true, block: m, external: m.host != nil}
	if m.max >= 0 {
		b.resizable, b.maxByteLength = true, m.max
	}
	o.setData(b)
	return Obj(o)
}

// --- Atomic access -----------------------------------------------------------

// nativeLittle reports whether the machine stores integers little-endian,
// which is how a typed array's elements are laid out whatever the machine.
var nativeLittle = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// le32 and le64 turn a word as the machine reads it into the value its bytes
// hold little-endian, and back.
func le32(v uint32) uint32 {
	if nativeLittle {
		return v
	}
	return bits.ReverseBytes32(v)
}

func le64(v uint64) uint64 {
	if nativeLittle {
		return v
	}
	return bits.ReverseBytes64(v)
}

// word32 and word64 point at the aligned word at a byte offset.
func word32(b []byte, at int) *uint32 { return (*uint32)(unsafe.Pointer(&b[at])) }
func word64(b []byte, at int) *uint64 { return (*uint64)(unsafe.Pointer(&b[at])) }

// sharedLoad reads an element of shared memory atomically.
func sharedLoad(b []byte, at, size int) uint64 {
	switch size {
	case 8:
		return le64(atomic.LoadUint64(word64(b, at)))
	case 4:
		return uint64(le32(atomic.LoadUint32(word32(b, at))))
	}
	// A byte or a half-word is read from the word it is part of.
	w := le32(atomic.LoadUint32(word32(b, at&^3)))
	return uint64(w>>(uint(at&3)*8)) & sizeMask(size)
}

// sharedUpdate replaces an element of shared memory atomically with what op
// makes of it, returning what it was.
func sharedUpdate(b []byte, at, size int, op func(old uint64) uint64) uint64 {
	switch size {
	case 8:
		p := word64(b, at)
		for {
			cur := atomic.LoadUint64(p)
			old := le64(cur)
			if atomic.CompareAndSwapUint64(p, cur, le64(op(old))) {
				return old
			}
		}
	case 4:
		p := word32(b, at)
		for {
			cur := atomic.LoadUint32(p)
			old := uint64(le32(cur))
			if atomic.CompareAndSwapUint32(p, cur, le32(uint32(op(old)))) {
				return old
			}
		}
	}
	// A byte or a half-word is updated within the word it is part of, which
	// is swapped whole.
	p := word32(b, at&^3)
	shift := uint(at&3) * 8
	mask := uint32(sizeMask(size)) << shift
	for {
		cur := atomic.LoadUint32(p)
		w := le32(cur)
		old := uint64(w&mask) >> shift
		next := w&^mask | uint32(op(old)&sizeMask(size))<<shift
		if atomic.CompareAndSwapUint32(p, cur, le32(next)) {
			return old
		}
	}
}

// --- Waiting -------------------------------------------------------------------

// waitKey is what the memory's agents wait on in the table of waiters: the
// memory, or, for memory a host owns, its owner, whose own waiters -- a
// WebAssembly memory's -- wait there too, as do those of every other view
// of it.
func (m *SharedMemory) waitKey() any {
	if m.host != nil && m.host.owner != nil {
		return m.host.owner
	}
	return m
}

// waitTimeout is the duration a timeout of ms milliseconds waits, negative
// for forever.
func waitTimeout(ms float64) time.Duration {
	if d, ok := waitTimer(ms); ok {
		return d
	}
	return -1
}

// equalAt reports whether the element at a byte offset holds v.
func (m *SharedMemory) equalAt(at, size int, v uint64) func() bool {
	return func() bool { return sharedLoad(m.bytes(), at, size) == v&sizeMask(size) }
}

// wait blocks while the element at a byte offset holds v, until another agent
// notifies it, the time runs out, or done or abort is closed. It reports
// whether it waited at all, and whether it was notified.
func (m *SharedMemory) wait(at, size int, v uint64, timeout float64, done, abort <-chan struct{}) (waited, notified bool) {
	res := atomicwait.Wait(m.waitKey(), uint64(at), m.equalAt(at, size, v), waitTimeout(timeout), done, abort)
	return res != atomicwait.NotEqual, res == atomicwait.OK
}

// waitAsync begins an asynchronous wait on the element at a byte offset. It
// answers at once, with "not-equal" or -- for a timeout of zero -- with
// "timed-out", or else returns the waiter and calls deliver later, from
// whichever goroutine notifies it or finds its time is up, with "ok" or
// "timed-out".
func (m *SharedMemory) waitAsync(at, size int, v uint64, timeout float64, deliver func(string)) (*waiter, string) {
	w, res := atomicwait.WaitAsync(m.waitKey(), uint64(at), m.equalAt(at, size, v), waitTimeout(timeout),
		func(res atomicwait.Result) { deliver(waitResult(res)) })
	if w == nil {
		return nil, waitResult(res)
	}
	return w, ""
}

// waitResult is what Atomics.wait answers for a wait that ended so.
func waitResult(res atomicwait.Result) string {
	switch res {
	case atomicwait.OK:
		return "ok"
	case atomicwait.NotEqual:
		return "not-equal"
	}
	return "timed-out"
}

// asyncWaits is a runtime's waiters in Atomics.waitAsync that have not been
// told, which it takes off their lists when it closes: they would take a
// notify meant for a live agent, and keep the runtime's heap reachable.
type asyncWaits struct {
	mu      sync.Mutex
	waiters map[*waiter]struct{}
	closed  bool
}

// add records a waiter, or cancels it at once for a runtime that has
// closed. It is safe to call from any goroutine.
func (a *asyncWaits) add(w *waiter) {
	a.mu.Lock()
	if !a.closed {
		if a.waiters == nil {
			a.waiters = map[*waiter]struct{}{}
		}
		a.waiters[w] = struct{}{}
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	atomicwait.Cancel(w)
}

// done forgets a waiter that has been told. It is safe to call from any
// goroutine.
func (a *asyncWaits) done(w *waiter) {
	a.mu.Lock()
	delete(a.waiters, w)
	a.mu.Unlock()
}

// close cancels every waiter left. The runtime's lock is let go before the
// table's is taken.
func (a *asyncWaits) close() {
	a.mu.Lock()
	a.closed = true
	waiters := a.waiters
	a.waiters = nil
	a.mu.Unlock()
	for w := range waiters {
		atomicwait.Cancel(w)
	}
}

// notify wakes up to count of the agents waiting on a byte offset, the
// longest waiting first -- WebAssembly's on the same memory among them --
// and returns how many it woke.
func (m *SharedMemory) notify(at int, count float64) int {
	n := uint64(math.MaxUint64)
	if count < 1<<63 {
		n = uint64(count)
	}
	return int(atomicwait.Notify(m.waitKey(), uint64(at), n))
}
