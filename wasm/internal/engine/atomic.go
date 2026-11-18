package engine

import (
	"math/bits"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/atomicwait"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The threads proposal's atomic instructions.
//
// An atomic access is to an address aligned to its width, which is checked
// before its bounds. The read-modify-write operators are compare-and-swap
// loops on the aligned 32- or 64-bit word holding the accessed bytes; memory
// is little-endian, so on a big-endian host the word's bytes are swapped
// around each operation.

// bigEndian reports a big-endian host.
var bigEndian = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 0
}()

func le32(v uint32) uint32 {
	if bigEndian {
		return bits.ReverseBytes32(v)
	}
	return v
}

func le64(v uint64) uint64 {
	if bigEndian {
		return bits.ReverseBytes64(v)
	}
	return v
}

// atomicSpan returns memory m's bytes after checking that the access of
// width bytes at ea, by the instruction at offset at, is aligned and in
// bounds.
func atomicSpan(m *Memory, ea, width uint64, at uint64) []byte {
	if ea&(width-1) != 0 {
		trapAt(TrapUnalignedAccess, at)
	}
	b := m.bytes()
	if ea > uint64(len(b)) || width > uint64(len(b))-ea {
		trapAt(TrapMemOutOfBounds, at)
	}
	return b
}

func word32(b []byte, ea uint64) *uint32 { return (*uint32)(unsafe.Pointer(&b[ea])) }
func word64(b []byte, ea uint64) *uint64 { return (*uint64)(unsafe.Pointer(&b[ea])) }

// atomicLoad loads width bytes at ea.
func atomicLoad(b []byte, ea, width uint64) uint64 {
	switch width {
	case 8:
		return le64(atomic.LoadUint64(word64(b, ea)))
	case 4:
		return uint64(le32(atomic.LoadUint32(word32(b, ea))))
	}
	w := le32(atomic.LoadUint32(word32(b, ea&^3)))
	shift := (ea & 3) * 8
	return uint64(w>>shift) & (1<<(width*8) - 1)
}

// atomicRMW applies op to the width bytes at ea atomically, returning the
// old value. It is a store when op ignores the old value.
func atomicRMW(b []byte, ea, width uint64, op func(old uint64) uint64) uint64 {
	switch width {
	case 8:
		p := word64(b, ea)
		for {
			raw := atomic.LoadUint64(p)
			old := le64(raw)
			if atomic.CompareAndSwapUint64(p, raw, le64(op(old))) {
				return old
			}
		}
	case 4:
		p := word32(b, ea)
		for {
			raw := atomic.LoadUint32(p)
			old := uint64(le32(raw))
			if atomic.CompareAndSwapUint32(p, raw, le32(uint32(op(old)))) {
				return old
			}
		}
	}
	p := word32(b, ea&^3)
	shift := (ea & 3) * 8
	mask := uint32(1<<(width*8)-1) << shift
	for {
		raw := atomic.LoadUint32(p)
		w := le32(raw)
		old := uint64((w & mask) >> shift)
		nw := w&^mask | uint32(op(old)<<shift)&mask
		if atomic.CompareAndSwapUint32(p, raw, le32(nw)) {
			return old
		}
	}
}

// atomicWidth returns an atomic memory operator's access width, and
// whether its result is i64.
func atomicWidth(op opcode.Opcode) (uint64, bool) {
	switch op {
	case opcode.I32AtomicLoad, opcode.I32AtomicStore, opcode.I32AtomicAdd, opcode.I32AtomicSub,
		opcode.I32AtomicAnd, opcode.I32AtomicOr, opcode.I32AtomicXor, opcode.I32AtomicExchange,
		opcode.I32AtomicCompareExchange:
		return 4, false
	case opcode.I64AtomicLoad, opcode.I64AtomicStore, opcode.I64AtomicAdd, opcode.I64AtomicSub,
		opcode.I64AtomicAnd, opcode.I64AtomicOr, opcode.I64AtomicXor, opcode.I64AtomicExchange,
		opcode.I64AtomicCompareExchange:
		return 8, true
	case opcode.I32AtomicLoad8U, opcode.I32AtomicStore8U, opcode.I32AtomicAdd8U, opcode.I32AtomicSub8U,
		opcode.I32AtomicAnd8U, opcode.I32AtomicOr8U, opcode.I32AtomicXor8U, opcode.I32AtomicExchange8U,
		opcode.I32AtomicCompareExchange8U:
		return 1, false
	case opcode.I32AtomicLoad16U, opcode.I32AtomicStore16U, opcode.I32AtomicAdd16U, opcode.I32AtomicSub16U,
		opcode.I32AtomicAnd16U, opcode.I32AtomicOr16U, opcode.I32AtomicXor16U, opcode.I32AtomicExchange16U,
		opcode.I32AtomicCompareExchange16U:
		return 2, false
	case opcode.I64AtomicLoad8U, opcode.I64AtomicStore8U, opcode.I64AtomicAdd8U, opcode.I64AtomicSub8U,
		opcode.I64AtomicAnd8U, opcode.I64AtomicOr8U, opcode.I64AtomicXor8U, opcode.I64AtomicExchange8U,
		opcode.I64AtomicCompareExchange8U:
		return 1, true
	case opcode.I64AtomicLoad16U, opcode.I64AtomicStore16U, opcode.I64AtomicAdd16U, opcode.I64AtomicSub16U,
		opcode.I64AtomicAnd16U, opcode.I64AtomicOr16U, opcode.I64AtomicXor16U, opcode.I64AtomicExchange16U,
		opcode.I64AtomicCompareExchange16U:
		return 2, true
	case opcode.I64AtomicLoad32U, opcode.I64AtomicStore32U, opcode.I64AtomicAdd32U, opcode.I64AtomicSub32U,
		opcode.I64AtomicAnd32U, opcode.I64AtomicOr32U, opcode.I64AtomicXor32U, opcode.I64AtomicExchange32U,
		opcode.I64AtomicCompareExchange32U:
		return 4, true
	}
	return 0, false
}

// rmwKind classifies a read-modify-write operator.
func rmwKind(op opcode.Opcode) byte {
	switch {
	case op >= opcode.I32AtomicAdd && op <= opcode.I64AtomicAdd32U:
		return '+'
	case op >= opcode.I32AtomicSub && op <= opcode.I64AtomicSub32U:
		return '-'
	case op >= opcode.I32AtomicAnd && op <= opcode.I64AtomicAnd32U:
		return '&'
	case op >= opcode.I32AtomicOr && op <= opcode.I64AtomicOr32U:
		return '|'
	case op >= opcode.I32AtomicXor && op <= opcode.I64AtomicXor32U:
		return '^'
	case op >= opcode.I32AtomicExchange && op <= opcode.I64AtomicExchange32U:
		return '='
	case op >= opcode.I32AtomicCompareExchange && op <= opcode.I64AtomicCompareExchange32U:
		return '?'
	}
	return 0
}

// wait implements memory.atomic.wait32 and wait64: 1 if the value at ea is
// not want, 2 if timeout nanoseconds pass (none if negative), 0 if a notify
// wakes the thread first. The thread waits in the process's table of
// waiters, keyed by the memory, where a host's waiters on views of the
// memory -- JavaScript's Atomics.wait on its buffer -- wait too.
func wait(x *exec, m *Memory, b []byte, ea, width, want uint64, timeout int64) uint32 {
	var done <-chan struct{}
	if x.ctx != nil {
		done = x.ctx.Done()
	}
	equal := func() bool { return atomicLoad(b, ea, width) == want }
	switch atomicwait.Wait(m, ea, equal, time.Duration(timeout), done, nil) {
	case atomicwait.OK:
		return 0
	case atomicwait.NotEqual:
		return 1
	}
	if done != nil && x.ctx.Err() != nil {
		x.pollContext()
	}
	return 2
}

// notify wakes up to n threads waiting at ea of memory m, returning how
// many it woke.
func notify(m *Memory, ea uint64, n uint32) uint32 {
	if !m.Shared {
		return 0
	}
	return uint32(atomicwait.Notify(m, ea, uint64(n)))
}
