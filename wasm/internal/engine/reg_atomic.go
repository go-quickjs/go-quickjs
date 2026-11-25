package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's atomic instructions: one rExt operation, xAtomic, whose
// handler does what its opcode says. The word after it holds the memory and
// the opcode, the next the offset; the next two the slots, 32 bits each, of
// the result and the address, and of the value (the expected value, a
// notify's count) and a third operand (a compare-exchange's replacement, a
// wait's timeout).
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
	ops, _, h := c.operands(n)
	ops = append(ops, 0, 0)
	dst := uint64(c.numSlot(h))
	c.emit(rExt, xAtomic, 0, 0, 0, uint64(mem)|uint64(op)<<32, off, dst|uint64(ops[0])<<32, uint64(ops[1])|uint64(ops[2])<<32)
	if hasRes {
		c.pushSlot(res, h)
	}
	return true
}

func rhAtomic(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	op := opcode.Opcode(f.rc.code[pc+1] >> 32)
	da, vt := f.rc.code[pc+3], f.rc.code[pc+4]
	res := numAt(l, uint32(da))
	m, ea := regEAOf(num(l, uint32(da>>32)), pc, f)
	at := regAt(pc)
	v := num(l, uint32(vt))
	third := num(l, uint32(vt>>32))
	switch op {
	case opcode.AtomicNotify:
		atomicSpan(m, ea, 4, at)
		*res = uint64(notify(m, ea, uint32(v)))
		return pc + 5
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		width := uint64(4)
		if op == opcode.I64AtomicWait {
			width = 8
		}
		if !m.Shared {
			trapAt(TrapAtomicsWaitNotAllowed, at)
		}
		b := atomicSpan(m, ea, width, at)
		*res = uint64(wait(f.x, m, b, ea, width, v, int64(third)))
		return pc + 5
	}
	width, is64 := atomicWidth(op)
	var r uint64
	switch {
	case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
		r = atomicLoad(atomicSpan(m, ea, width, at), ea, width)
	case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
		atomicRMW(atomicSpan(m, ea, width, at), ea, width, func(uint64) uint64 { return v })
		return pc + 5
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
	*res = r
	return pc + 5
}

func init() {
	regExtTable[xAtomic] = rhAtomic
}
