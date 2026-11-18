package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's atomic instructions: one rExt operation, xAtomic, whose
// handler does what its opcode says. A is the result, B the address, C the
// value, the expected value or a notify's count; the word after holds the
// memory and the opcode, the next the offset, and the last a third
// operand's slot -- a compare-exchange's replacement, a wait's timeout.
const xAtomic regOp = 160

// atomicOp compiles an atomic instruction, reporting whether op is one.
func (c *regCompiler) atomicOp(op opcode.Opcode) bool {
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
	n := 2
	res, hasRes := module.I32, true
	switch op {
	case opcode.AtomicNotify:
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		n = 3
	default:
		if _, is64 := atomicWidth(op); is64 {
			res = module.I64
		}
		switch {
		case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
			n = 1
		case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
			hasRes = false
		case rmwKind(op) == '?':
			n = 3
		}
	}
	ops, h := c.operands(n)
	var b, cc, third uint64
	b = c.near(ops[0])
	if n > 1 {
		cc = c.near(ops[1])
	}
	if n > 2 {
		third = c.near(ops[2])
	}
	dst := uint64(0)
	if hasRes {
		dst = ns(c.numSlot(h))
	}
	pc := c.emit(rExt, xAtomic, dst, b, cc, uint64(mem)|uint64(op)<<32, off, third)
	if hasRes {
		c.def(res, h, pc)
	}
	return true
}

// near returns slot s as a field, bailing out where it is past 16 bits.
func (c *regCompiler) near(s uint32) uint64 {
	if s > 0xffff {
		panic(regBail("a slot past 65,535"))
	}
	return uint64(s)
}

func rhAtomic(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	op := opcode.Opcode(f.rc.code[pc+1] >> 32)
	m, ea := regEA(l, w>>32, pc, f)
	at := regAt(pc)
	v := *rs(l, w>>48)
	third := *rs(l, f.rc.code[pc+3])
	switch op {
	case opcode.AtomicNotify:
		atomicSpan(m, ea, 4, at)
		*rs(l, w>>16) = uint64(notify(m, ea, uint32(v)))
		return pc + 4
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		width := uint64(4)
		if op == opcode.I64AtomicWait {
			width = 8
		}
		if !m.Shared {
			trapAt(TrapAtomicsWaitNotAllowed, at)
		}
		b := atomicSpan(m, ea, width, at)
		*rs(l, w>>16) = uint64(wait(f.x, m, b, ea, width, v, int64(third)))
		return pc + 4
	}
	width, is64 := atomicWidth(op)
	var r uint64
	switch {
	case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
		r = atomicLoad(atomicSpan(m, ea, width, at), ea, width)
	case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
		atomicRMW(atomicSpan(m, ea, width, at), ea, width, func(uint64) uint64 { return v })
		return pc + 4
	case rmwKind(op) == '?':
		mask := uint64(1)<<(width*8) - 1
		if width == 8 {
			mask = ^uint64(0)
		}
		want, nv := v&mask, third&mask
		r = atomicRMW(atomicSpan(m, ea, width, at), ea, width, func(old uint64) uint64 {
			if old == want {
				return nv
			}
			return old
		})
	default:
		kind := rmwKind(op)
		r = atomicRMW(atomicSpan(m, ea, width, at), ea, width, func(old uint64) uint64 {
			switch kind {
			case '+':
				return old + v
			case '-':
				return old - v
			case '&':
				return old & v
			case '|':
				return old | v
			case '^':
				return old ^ v
			}
			return v
		})
	}
	if !is64 {
		r = uint64(uint32(r))
	}
	*rs(l, w>>16) = r
	return pc + 4
}

func init() {
	regExtTable[xAtomic] = rhAtomic
}
