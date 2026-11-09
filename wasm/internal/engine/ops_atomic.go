package engine

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
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
func atomicSpan(m *Memory, ea, width uint64, at uint32) []byte {
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

// atomicOps compiles the atomic instructions, reporting whether op is one.
func (c *compiler) atomicOps(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixAtomic {
		return false
	}
	switch op {
	case opcode.AtomicFence:
		c.r.byte()
		return true
	case opcode.Pause:
		return true
	}
	mem, off := c.r.memarg()
	m := memAt(mem)
	result := func(is64 bool, get func(*frame) uint64) {
		if is64 {
			c.pushTree(module.I64, ti64(get))
		} else {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(get(f)) }))
		}
	}
	switch op {
	case opcode.AtomicNotify:
		n := c.i32(c.pop())
		ea := c.addr(c.pop(), mem, off, at)
		c.pushTree(module.I32, ti32(func(f *frame) uint32 {
			e := ea(f)
			cnt := n(f)
			mm := m(f)
			atomicSpan(mm, e, 4, at)
			return notify(mm, e, cnt)
		}))
		return true
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		width := uint64(4)
		if op == opcode.I64AtomicWait {
			width = 8
		}
		timeout := c.i64(c.pop())
		expected := c.lowBits(c.pop())
		ea := c.addr(c.pop(), mem, off, at)
		c.pushTree(module.I32, ti32(func(f *frame) uint32 {
			e := ea(f)
			want := expected(f)
			t := int64(timeout(f))
			mm := m(f)
			if !mm.Shared {
				trapAt(TrapAtomicsWaitNotAllowed, at)
			}
			b := atomicSpan(mm, e, width, at)
			return wait(f.x, mm, b, e, width, want, t)
		}))
		return true
	}
	width, is64 := atomicWidth(op)
	switch {
	case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
		ea := c.addr(c.pop(), mem, off, at)
		result(is64, func(f *frame) uint64 {
			e := ea(f)
			return atomicLoad(atomicSpan(m(f), e, width, at), e, width)
		})
	case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
		v := c.lowBits(c.pop())
		ea := c.addr(c.pop(), mem, off, at)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := v(f)
			atomicRMW(atomicSpan(m(f), e, width, at), e, width, func(uint64) uint64 { return val })
			return 0
		})
	case rmwKind(op) == '?':
		repl := c.lowBits(c.pop())
		exp := c.lowBits(c.pop())
		ea := c.addr(c.pop(), mem, off, at)
		mask := uint64(1)<<(width*8) - 1
		if width == 8 {
			mask = ^uint64(0)
		}
		result(is64, func(f *frame) uint64 {
			e := ea(f)
			want, nv := exp(f)&mask, repl(f)&mask
			return atomicRMW(atomicSpan(m(f), e, width, at), e, width, func(old uint64) uint64 {
				if old == want {
					return nv
				}
				return old
			})
		})
	default:
		kind := rmwKind(op)
		v := c.lowBits(c.pop())
		ea := c.addr(c.pop(), mem, off, at)
		result(is64, func(f *frame) uint64 {
			e := ea(f)
			val := v(f)
			return atomicRMW(atomicSpan(m(f), e, width, at), e, width, func(old uint64) uint64 {
				switch kind {
				case '+':
					return old + val
				case '-':
					return old - val
				case '&':
					return old & val
				case '|':
					return old | val
				case '^':
					return old ^ val
				}
				return val
			})
		})
	}
	return true
}

// waiters holds, for each address of each shared memory, the threads
// waiting on it, first come first woken.
var waiters struct {
	sync.Mutex
	m map[waitKey][]chan struct{}
}

type waitKey struct {
	m  *Memory
	ea uint64
}

// wait implements memory.atomic.wait32 and wait64: 1 if the value at ea is
// not want, 2 if timeout nanoseconds pass (none if negative), 0 if a notify
// wakes the thread first.
func wait(x *exec, m *Memory, b []byte, ea, width, want uint64, timeout int64) uint32 {
	waiters.Lock()
	if atomicLoad(b, ea, width) != want {
		waiters.Unlock()
		return 1
	}
	ch := make(chan struct{})
	k := waitKey{m, ea}
	if waiters.m == nil {
		waiters.m = make(map[waitKey][]chan struct{})
	}
	waiters.m[k] = append(waiters.m[k], ch)
	waiters.Unlock()

	var expired <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(time.Duration(timeout))
		defer t.Stop()
		expired = t.C
	}
	var done <-chan struct{}
	if x.ctx != nil {
		done = x.ctx.Done()
	}
	select {
	case <-ch:
		return 0
	case <-expired:
	case <-done:
	}
	// Leave the queue, unless a notify took the thread out of it meanwhile.
	waiters.Lock()
	q := waiters.m[k]
	for i, w := range q {
		if w == ch {
			waiters.m[k] = append(q[:i:i], q[i+1:]...)
			if len(waiters.m[k]) == 0 {
				delete(waiters.m, k)
			}
			waiters.Unlock()
			if done != nil && x.ctx.Err() != nil {
				x.pollContext()
			}
			return 2
		}
	}
	waiters.Unlock()
	return 0
}

// notify wakes up to n threads waiting at ea of memory m, returning how
// many it woke.
func notify(m *Memory, ea uint64, n uint32) uint32 {
	if !m.Shared {
		return 0
	}
	waiters.Lock()
	defer waiters.Unlock()
	k := waitKey{m, ea}
	q := waiters.m[k]
	woken := uint32(0)
	for len(q) > 0 && woken < n {
		close(q[0])
		q = q[1:]
		woken++
	}
	if len(q) == 0 {
		delete(waiters.m, k)
	} else {
		waiters.m[k] = q
	}
	return woken
}
