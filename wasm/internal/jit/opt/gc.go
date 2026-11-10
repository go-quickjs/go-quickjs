package opt

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// GC objects. A struct's fields and an array's elements are read and
// written in place, by reference instructions (OpRef) on the slot that
// holds the object: a number read is a value, a reference read goes into
// its position's slot, as every reference does. The tests and casts read
// the slot too. What allocates, and array.fill, array.copy and the
// array.init instructions, run in Go (OpGoOp), whose allocations charge the
// instance's budget: their numeric operands are staged in the call area,
// each at its place among the operands, their references stay in their
// positions' slots (Ctx.RArgs), and a new object is left in the first
// operand's position's slot.

// gcAux is the index (Aux above the kind) of a field's or an element's
// access: the offset of a struct field's cell, its storage type, and
// whether a packed one is read sign-extended.
func gcAux(off int32, storage module.Kind, signed bool) uint64 {
	x := uint64(uint32(off)) | uint64(storage)<<32
	if signed {
		x |= 1 << 40
	}
	return x
}

// gcOff, gcStorage and gcSigned are the parts of a gcAux.
func gcOff(idx uint64) int32           { return int32(uint32(idx)) }
func gcStorage(idx uint64) module.Kind { return module.Kind(idx >> 32) }
func gcSigned(idx uint64) bool         { return idx>>40&1 != 0 }

// testAux is the index of a test or a cast against heap type h, null
// matching it where nullable.
func testAux(h module.HeapType, nullable bool) uint64 {
	x := uint64(h)
	if nullable {
		x |= 1 << 32
	}
	return x
}

// cellOf is the offset of the cell of field fi of struct type ti from the
// struct's start: the header's cell first, a v128 taking two.
func cellOf(m *module.Module, ti, fi uint32) int32 {
	n := int32(1)
	for _, f := range m.Types[ti].Struct.Fields[:fi] {
		if f.Type.Kind == module.KindV128 {
			n += 2
		} else {
			n++
		}
	}
	return 16 * n
}

// storageSize is the bytes an array element of storage type t takes.
func storageSize(t module.Kind) int {
	switch t {
	case module.KindI8:
		return 1
	case module.KindI16:
		return 2
	case module.KindI32, module.KindF32:
		return 4
	}
	return 8
}

// heapTest is what a test of a non-null reference against a heap type
// checks.
type heapTest uint8

const (
	// testAll and testNone: any reference is, or none is, of the type.
	testAll heapTest = iota
	testNone
	// testI31: an i31.
	testI31
	// testEq, testStruct, testArray: an i31 or a struct or an array; a
	// struct; an array.
	testEq
	testStruct
	testArray
	// testIndex: an object whose type's supertypes at depth are canon.
	testIndex
)

// heapTestOf is what a non-null reference is tested for to be of heap type
// h, and for a defined type, its canonical index and the depth of its
// supertypes.
func heapTestOf(m *module.Module, h module.HeapType) (test heapTest, canon uint32, depth int) {
	if h.IsIndex() {
		ci := m.Canonical[h.Index()]
		for s := module.Canonical(ci).Super; s != module.NoCanonical; s = module.Canonical(s).Super {
			depth++
		}
		return testIndex, uint32(ci), depth
	}
	switch h {
	case module.HeapAny, module.HeapExtern, module.HeapFunc, module.HeapExn:
		// A reference is only tested against types of its own hierarchy.
		return testAll, 0, 0
	case module.HeapI31:
		return testI31, 0, 0
	case module.HeapEq:
		return testEq, 0, 0
	case module.HeapStruct:
		return testStruct, 0, 0
	case module.HeapArray:
		return testArray, 0, 0
	}
	return testNone, 0, 0
}

// heapTypeOf reads a heap type.
func (r *reader) heapTypeOf() module.HeapType {
	v := r.s64()
	if v >= 0 {
		return module.HeapType(v)
	}
	h, _ := module.HeapFromCode(byte(v) & 0x7f)
	return h
}

// checkGC refuses GC instruction op, its immediates read, where this tier
// does not build it: one past the GC proposal's, or an access of a v128
// field or element.
func checkGC(m *module.Module, r *reader, op opcode.Opcode) {
	if op < opcode.StructNew || op > opcode.I31GetU {
		refuse(RefuseGC, "%s", op.Name())
	}
	switch op {
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU, opcode.StructSet:
		ti := r.u32()
		if m.Types[ti].Struct.Fields[r.u32()].Type.Kind == module.KindV128 {
			refuse(RefuseSIMD, "%s of a v128 field", op.Name())
		}
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU, opcode.ArraySet:
		if m.Types[r.u32()].Array.Elem.Type.Kind == module.KindV128 {
			refuse(RefuseSIMD, "%s of v128 elements", op.Name())
		}
	case opcode.StructNew:
		for _, f := range m.Types[r.u32()].Struct.Fields {
			if f.Type.Kind == module.KindV128 {
				refuse(RefuseSIMD, "%s of a v128 field", op.Name())
			}
		}
	case opcode.ArrayNew, opcode.ArrayNewFixed, opcode.ArrayFill:
		if m.Types[r.u32()].Array.Elem.Type.Kind == module.KindV128 {
			refuse(RefuseSIMD, "%s of v128 elements", op.Name())
		}
		if op == opcode.ArrayNewFixed {
			r.u32()
		}
	default:
		r.skipGC(op)
	}
}

// skipGC skips GC instruction op's immediates.
func (r *reader) skipGC(op opcode.Opcode) {
	switch op {
	case opcode.StructNew, opcode.StructNewDefault, opcode.ArrayNew, opcode.ArrayNewDefault,
		opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU, opcode.ArraySet, opcode.ArrayFill:
		r.u32()
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU, opcode.StructSet,
		opcode.ArrayNewFixed, opcode.ArrayNewData, opcode.ArrayNewElem, opcode.ArrayCopy,
		opcode.ArrayInitData, opcode.ArrayInitElem:
		r.u32()
		r.u32()
	case opcode.RefTest, opcode.RefTestNull, opcode.RefCast, opcode.RefCastNull:
		r.heapType()
	case opcode.BrOnCast, opcode.BrOnCastFail:
		r.pc++
		r.u32()
		r.heapType()
		r.heapType()
	}
}

// gcInstr builds a GC instruction, reporting whether op is one. A
// reference it pops or pushes is in its position's reference slot.
func (bd *builder) gcInstr(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixGC {
		return false
	}
	r := &bd.r
	m := bd.m
	h := len(bd.stack)
	switch op {
	case opcode.StructNew, opcode.StructNewDefault, opcode.ArrayNew, opcode.ArrayNewDefault, opcode.ArrayFill:
		bd.gcGo(op, r.u32(), 0, at)
	case opcode.ArrayNewFixed, opcode.ArrayNewData, opcode.ArrayNewElem, opcode.ArrayCopy,
		opcode.ArrayInitData, opcode.ArrayInitElem:
		x := r.u32()
		bd.gcGo(op, x, r.u32(), at)
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti, fi := r.u32(), r.u32()
		ft := m.Types[ti].Struct.Fields[fi].Type
		aux := gcAux(cellOf(m, ti, fi), ft.Kind, op == opcode.StructGetS)
		bd.pop()
		if ft.IsRef() {
			bd.refOp(bd.b, refStructGet, bd.rpos(h-1), bd.rpos(h-1), aux, TNone, at)
			bd.pushRef()
			break
		}
		bd.push(bd.refOp(bd.b, refStructGet, 0, bd.rpos(h-1), aux, typeOf(ft.Unpacked(), "field"), at))
	case opcode.StructSet:
		ti, fi := r.u32(), r.u32()
		ft := m.Types[ti].Struct.Fields[fi].Type
		aux := gcAux(cellOf(m, ti, fi), ft.Kind, false)
		v := bd.pop()
		bd.pop()
		if bd.isRef(v) {
			bd.refOp(bd.b, refStructSet, bd.rpos(h-2), bd.rpos(h-1), aux, TNone, at)
			break
		}
		bd.refOp(bd.b, refStructSet, bd.rpos(h-2), 0, aux, TNone, at, v)
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU:
		et := m.Types[r.u32()].Array.Elem.Type
		aux := gcAux(0, et.Kind, op == opcode.ArrayGetS)
		idx := bd.pop()
		bd.pop()
		if et.IsRef() {
			bd.refOp(bd.b, refArrayGet, bd.rpos(h-2), bd.rpos(h-2), aux, TNone, at, idx)
			bd.pushRef()
			break
		}
		bd.push(bd.refOp(bd.b, refArrayGet, 0, bd.rpos(h-2), aux, typeOf(et.Unpacked(), "element"), at, idx))
	case opcode.ArraySet:
		et := m.Types[r.u32()].Array.Elem.Type
		aux := gcAux(0, et.Kind, false)
		v := bd.pop()
		idx := bd.pop()
		bd.pop()
		if bd.isRef(v) {
			bd.refOp(bd.b, refArraySet, bd.rpos(h-3), bd.rpos(h-1), aux, TNone, at, idx)
			break
		}
		bd.refOp(bd.b, refArraySet, bd.rpos(h-3), 0, aux, TNone, at, idx, v)
	case opcode.ArrayLen:
		bd.pop()
		bd.push(bd.refOp(bd.b, refArrayLen, 0, bd.rpos(h-1), 0, TI32, at))
	case opcode.RefTest, opcode.RefTestNull:
		ht := r.heapTypeOf()
		bd.pop()
		bd.push(bd.refOp(bd.b, refTest, 0, bd.rpos(h-1), testAux(ht, op == opcode.RefTestNull), TI32, 0))
	case opcode.RefCast, opcode.RefCastNull:
		ht := r.heapTypeOf()
		bd.refOp(bd.b, refCast, 0, bd.rpos(h-1), testAux(ht, op == opcode.RefCastNull), TNone, at)
	case opcode.BrOnCast, opcode.BrOnCastFail:
		flags := r.byte()
		depth := int(r.u32())
		r.heapType()
		ht := r.heapTypeOf()
		cond := bd.refOp(bd.b, refTest, 0, bd.rpos(h-1), testAux(ht, flags&2 != 0), TI32, 0)
		if op == opcode.BrOnCastFail {
			cond = bd.value(OpEqz, TI32, cond)
		}
		// The reference stays where it is on both ways.
		bd.brIfOn(cond, depth)
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
		// The same reference, of the other hierarchy.
	case opcode.RefI31:
		x := bd.pop()
		bd.refOp(bd.b, refI31, bd.rpos(h-1), 0, 0, TNone, 0, x)
		bd.pushRef()
	case opcode.I31GetS, opcode.I31GetU:
		bd.pop()
		var signed uint64
		if op == opcode.I31GetS {
			signed = 1
		}
		bd.push(bd.refOp(bd.b, refI31Get, 0, bd.rpos(h-1), signed, TI32, at))
	default:
		return false
	}
	return true
}

// gcGo builds GC instruction op with immediates x and y, which Go runs:
// its numeric operands are its Args, Pos the first operand's position, in
// whose reference slot an object it makes is left.
func (bd *builder) gcGo(op opcode.Opcode, x, y uint32, at uint32) *Value {
	n, result := gcOperandCount(bd.m, op, x, y)
	pos := len(bd.stack) - n
	args := bd.args[:0]
	for _, a := range bd.stack[pos:] {
		if !bd.isRef(a) {
			args = append(args, a)
			continue
		}
		bd.f.Refs = true
	}
	bd.args = args
	v := bd.value(OpGoOp, TNone, args...)
	v.Aux, v.Aux2, v.At, v.Pos = uint64(op), uint64(x)|uint64(y)<<32, at, int32(pos)
	if result {
		bd.f.Refs = true
	}
	bd.stack = bd.stack[:pos]
	if result {
		bd.pushRef()
	}
	// What Go does here writes no memory and no global: what the block
	// knows of them holds.
	return v
}

// gcOperandCount is the number of operands of GC instruction op with
// immediates x and y, which Go runs, or of throw, throw_ref and rethrow,
// and whether it leaves a reference.
func gcOperandCount(m *module.Module, op opcode.Opcode, x, y uint32) (int, bool) {
	switch op {
	case opcode.StructNew:
		return len(m.Types[x].Struct.Fields), true
	case opcode.StructNewDefault:
		return 0, true
	case opcode.ArrayNew, opcode.ArrayNewData, opcode.ArrayNewElem:
		return 2, true
	case opcode.ArrayNewDefault:
		return 1, true
	case opcode.ArrayNewFixed:
		return int(y), true
	case opcode.ArrayFill, opcode.ArrayInitData, opcode.ArrayInitElem:
		return 4, false
	case opcode.ArrayCopy:
		return 5, false
	case opcode.Throw:
		return len(m.TagType(x).Params), false
	case opcode.ThrowRef:
		return 1, false
	}
	return 0, false
}

// gcOperandIsRef reports whether operand i of GC instruction op with
// immediates x and y, which Go runs, or of a throw, is a reference.
func gcOperandIsRef(m *module.Module, op opcode.Opcode, x, y uint32, i int) bool {
	switch op {
	case opcode.StructNew:
		return m.Types[x].Struct.Fields[i].Type.IsRef()
	case opcode.ArrayNew:
		return i == 0 && m.Types[x].Array.Elem.Type.IsRef()
	case opcode.ArrayNewFixed:
		return m.Types[x].Array.Elem.Type.IsRef()
	case opcode.ArrayFill:
		return i == 0 || i == 2 && m.Types[x].Array.Elem.Type.IsRef()
	case opcode.ArrayCopy:
		return i == 0 || i == 2
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		return i == 0
	case opcode.Throw:
		return m.TagType(x).Params[i].IsRef()
	case opcode.ThrowRef:
		return true
	}
	return false
}

// isGCGo reports whether instruction Go runs v is a GC instruction, a
// throw, a throw_ref or a rethrow: its operands are counted and placed by
// the module's types (gcOperandCount), and its immediates are Aux2's words.
func isGCGo(v *Value) bool {
	op := opcode.Opcode(v.Aux)
	return op.Prefix() == opcode.PrefixGC || op == opcode.Throw || op == opcode.ThrowRef || op == opcode.Rethrow
}
