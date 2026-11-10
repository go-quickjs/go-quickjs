package opt

import "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"

// The threads proposal's atomic instructions. The accesses are values of
// their own (OpAtomicLoad ... OpAtomicCmpxchg), each an effect that stays
// where it is, in its order among the stores, the calls and the others:
// none is shared, moved or removed, BCE takes none as checked nor leaves
// one unchecked, and the builder forgets what it knew of memory at each,
// as at a fence. memory.atomic.wait32, wait64 and notify are instructions
// Go runs, the memory's index in Aux's upper half and the offset in Aux2
// (goImms).

// atomicAccess is an atomic access instruction's operation, width in bytes
// and type, or a width of 0 for another instruction.
func atomicAccess(op opcode.Opcode) (Op, uint8, Type) {
	var width uint8
	t := TI32
	switch op {
	case opcode.I32AtomicLoad, opcode.I32AtomicStore, opcode.I32AtomicAdd, opcode.I32AtomicSub,
		opcode.I32AtomicAnd, opcode.I32AtomicOr, opcode.I32AtomicXor, opcode.I32AtomicExchange,
		opcode.I32AtomicCompareExchange:
		width = 4
	case opcode.I64AtomicLoad, opcode.I64AtomicStore, opcode.I64AtomicAdd, opcode.I64AtomicSub,
		opcode.I64AtomicAnd, opcode.I64AtomicOr, opcode.I64AtomicXor, opcode.I64AtomicExchange,
		opcode.I64AtomicCompareExchange:
		width, t = 8, TI64
	case opcode.I32AtomicLoad8U, opcode.I32AtomicStore8U, opcode.I32AtomicAdd8U, opcode.I32AtomicSub8U,
		opcode.I32AtomicAnd8U, opcode.I32AtomicOr8U, opcode.I32AtomicXor8U, opcode.I32AtomicExchange8U,
		opcode.I32AtomicCompareExchange8U:
		width = 1
	case opcode.I32AtomicLoad16U, opcode.I32AtomicStore16U, opcode.I32AtomicAdd16U, opcode.I32AtomicSub16U,
		opcode.I32AtomicAnd16U, opcode.I32AtomicOr16U, opcode.I32AtomicXor16U, opcode.I32AtomicExchange16U,
		opcode.I32AtomicCompareExchange16U:
		width = 2
	case opcode.I64AtomicLoad8U, opcode.I64AtomicStore8U, opcode.I64AtomicAdd8U, opcode.I64AtomicSub8U,
		opcode.I64AtomicAnd8U, opcode.I64AtomicOr8U, opcode.I64AtomicXor8U, opcode.I64AtomicExchange8U,
		opcode.I64AtomicCompareExchange8U:
		width, t = 1, TI64
	case opcode.I64AtomicLoad16U, opcode.I64AtomicStore16U, opcode.I64AtomicAdd16U, opcode.I64AtomicSub16U,
		opcode.I64AtomicAnd16U, opcode.I64AtomicOr16U, opcode.I64AtomicXor16U, opcode.I64AtomicExchange16U,
		opcode.I64AtomicCompareExchange16U:
		width, t = 2, TI64
	case opcode.I64AtomicLoad32U, opcode.I64AtomicStore32U, opcode.I64AtomicAdd32U, opcode.I64AtomicSub32U,
		opcode.I64AtomicAnd32U, opcode.I64AtomicOr32U, opcode.I64AtomicXor32U, opcode.I64AtomicExchange32U,
		opcode.I64AtomicCompareExchange32U:
		width, t = 4, TI64
	default:
		return OpInvalid, 0, TNone
	}
	var o Op
	switch {
	case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
		o = OpAtomicLoad
	case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
		o = OpAtomicStore
	case op >= opcode.I32AtomicAdd && op <= opcode.I64AtomicAdd32U:
		o = OpAtomicAdd
	case op >= opcode.I32AtomicSub && op <= opcode.I64AtomicSub32U:
		o = OpAtomicSub
	case op >= opcode.I32AtomicAnd && op <= opcode.I64AtomicAnd32U:
		o = OpAtomicAnd
	case op >= opcode.I32AtomicOr && op <= opcode.I64AtomicOr32U:
		o = OpAtomicOr
	case op >= opcode.I32AtomicXor && op <= opcode.I64AtomicXor32U:
		o = OpAtomicXor
	case op >= opcode.I32AtomicExchange && op <= opcode.I64AtomicExchange32U:
		o = OpAtomicXchg
	default:
		o = OpAtomicCmpxchg
	}
	return o, width, t
}

// atomicOp builds an atomic instruction, reporting whether op is one.
func (bd *builder) atomicOp(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixAtomic {
		return false
	}
	r := &bd.r
	switch op {
	case opcode.AtomicFence:
		r.byte()
		bd.value(OpFence, TNone)
		bd.forget()
		return true
	case opcode.Pause:
		return true
	case opcode.AtomicNotify, opcode.I32AtomicWait, opcode.I64AtomicWait:
		mem, off := r.memarg()
		nargs := 3
		if op == opcode.AtomicNotify {
			nargs = 2
		}
		v := bd.goOp(op, off, nargs, TI32, at)
		v.Aux |= uint64(mem) << 32
		return true
	}
	o, width, t := atomicAccess(op)
	if width == 0 {
		refuse(RefuseAtomics, "%s", op.Name())
	}
	mem, off := r.memarg()
	var v *Value
	switch o {
	case OpAtomicLoad:
		addr := bd.pop()
		v = bd.value(o, t, addr)
		bd.push(v)
	case OpAtomicStore:
		x := bd.pop()
		addr := bd.pop()
		v = bd.value(o, TNone, addr, x)
	case OpAtomicCmpxchg:
		repl := bd.pop()
		exp := bd.pop()
		addr := bd.pop()
		v = bd.value(o, t, addr, exp, repl)
		bd.push(v)
	default:
		x := bd.pop()
		addr := bd.pop()
		v = bd.value(o, t, addr, x)
		bd.push(v)
	}
	v.Aux, v.Aux2, v.Size, v.At = off, bd.memAux(mem), width, at
	bd.forget()
	return true
}
