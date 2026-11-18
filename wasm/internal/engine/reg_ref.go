package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's references: moves, globals, select, and the reference
// instructions of the function-references proposal.

// refOp compiles a reference instruction, reporting whether op is one.
func (c *regCompiler) refOp(op opcode.Opcode) bool {
	switch op {
	case opcode.RefNull:
		c.pushConst(module.RefNull(c.r.heapType()), 0)
	case opcode.RefIsNull:
		e := c.pop()
		h := len(c.stack)
		s := c.read(e, h)
		c.def(module.I32, h, c.emit(rExt, xRefIsNull, ns(c.numSlot(h)), s, 0))
	case opcode.RefFunc:
		idx := c.r.u32()
		t := module.Ref(module.HeapIndex(c.m.Functions[idx].TypeIndex))
		h := len(c.stack)
		c.def(t, h, c.emitBC(rExt, xRefFunc, c.dst(t, h), idx))
	case opcode.RefEq:
		y := c.pop()
		x := c.pop()
		h := len(c.stack)
		xs := c.read(x, h)
		ys := c.read(y, h+1)
		c.def(module.I32, h, c.emit(rExt, xRefEq, ns(c.numSlot(h)), xs, ys))
	case opcode.RefAsNonNull:
		e := c.pop()
		h := len(c.stack)
		s := c.slotOf(e, h)
		c.emit(rExt, xRefAsNonNull, rf(s), 0, 0)
		if e.konst {
			e = rentry{slot: s, local: -1}
		}
		e.t, e.def = e.t.AsNonNull(), -1
		c.push(e)
	case opcode.BrOnNull, opcode.BrOnNonNull:
		c.brOnNull(op == opcode.BrOnNonNull, int(c.r.u32()))
	default:
		return false
	}
	return true
}

// brOnNull compiles br_on_null, and br_on_non_null (nonNull).
func (c *regCompiler) brOnNull(nonNull bool, depth int) {
	ti := len(c.ctrl) - 1 - depth
	t := c.ctrl[ti]
	n := len(t.labelTypes())
	h := len(c.stack) - 1
	ref := c.stack[h]
	if ref.konst {
		c.materialize(h)
		ref = c.stack[h]
	}
	rs := rf(ref.slot)
	// The values the branch carries: the reference's too where it is
	// br_on_non_null.
	var es []rentry
	if nonNull {
		es = c.stack[len(c.stack)-n:]
	} else {
		es = c.stack[len(c.stack)-1-n : len(c.stack)-1]
	}
	taken, skip := xBrNull, xBrNonNull
	if nonNull {
		taken, skip = xBrNonNull, xBrNull
	}
	if ti != 0 && !c.moves(t, es, false) && !c.leaving(ti) {
		c.branchTo(t, c.jumpExt(taken, rs))
	} else {
		x := c.jumpExt(skip, rs)
		c.branchOut(ti, es)
		c.bind([]rfix{x})
	}
	if nonNull {
		c.pop()
		return
	}
	c.stack[h].t = ref.t.AsNonNull()
}

func rhGlobalGetRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*f.rr(w >> 16) = f.inst.Globals[uint32(w>>32)].Ref
	return pc + 1
}

func rhGlobalSetRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	f.inst.Globals[uint32(w>>32)].Ref = *f.rr(w >> 16)
	return pc + 1
}

func rhGlobalGet128(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.inst.Globals[uint32(w>>32)]
	setV(l, w>>16, V128{g.Num, g.Hi})
	return pc + 1
}

func rhGlobalSet128(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	v := rv(l, w>>16)
	g := f.inst.Globals[uint32(w>>32)]
	g.Num, g.Hi = v.Lo, v.Hi
	return pc + 1
}

func rhSelectRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if *rs(l, w>>48) == 0 {
		*f.rr(w >> 16) = *f.rr(w >> 32)
	}
	return pc + 1
}

func rhSelect128(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if *rs(l, w>>48) == 0 {
		setV(l, w>>16, rv(l, w>>32))
	}
	return pc + 1
}

func rhRefIsNull(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(b2i(f.rr(w >> 32).IsNull()))
	return pc + 1
}

func rhRefFunc(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*f.rr(w >> 16) = FuncRef(f.inst.Funcs[uint32(w>>32)])
	return pc + 1
}

func rhRefEq(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(b2i(f.rr(w >> 32).Eq(*f.rr(w >> 48))))
	return pc + 1
}

func rhRefAsNonNull(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if f.rr(w >> 16).IsNull() {
		trapAt(TrapNullDereference, regAt(pc))
	}
	return pc + 1
}

func rhBrNull(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if f.rr(w >> 16).IsNull() {
		return regTake32(w, pc, f)
	}
	return pc + 1
}

func rhBrNonNull(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if !f.rr(w >> 16).IsNull() {
		return regTake32(w, pc, f)
	}
	return pc + 1
}

func init() {
	for x, h := range map[regOp]regHandler{
		xGlobalGetRef: rhGlobalGetRef, xGlobalSetRef: rhGlobalSetRef,
		xGlobalGet128: rhGlobalGet128, xGlobalSet128: rhGlobalSet128,
		xSelectRef: rhSelectRef, xSelect128: rhSelect128,
		xRefIsNull: rhRefIsNull, xRefFunc: rhRefFunc, xRefEq: rhRefEq, xRefAsNonNull: rhRefAsNonNull,
		xBrNull: rhBrNull, xBrNonNull: rhBrNonNull,
	} {
		regExtTable[x] = h
	}
	regExtSlowTable[xBrNull] = regSlow32
	regExtSlowTable[xBrNonNull] = regSlow32
}
