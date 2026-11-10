//go:build amd64 || arm64

package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// GC objects. A struct is cells of two words, a reference's or a number's
// in its second word, the first cell its header; an array is a header, its
// length, and its elements, packed numbers or references, in memory of
// their own (Layout). Native code reads and writes fields and elements,
// tests and casts, makes i31s, and makes structs from the pools the engine
// fills (structNew); what else allocates, and what copies or fills
// ranges, runs in Go (ExitOp), whose allocations charge the instance's
// budget as the interpreter's do.

// cellOf returns the offset of the cell of field fi of struct type ti
// from the struct's start: the header's cell first, a v128 taking two.
func (c *compiler) cellOf(ti, fi uint32) int32 {
	n := int32(1)
	for _, f := range c.m.Types[ti].Struct.Fields[:fi] {
		if f.Type.Kind == module.KindV128 {
			n += 2
		} else {
			n++
		}
	}
	return 16 * n
}

// storageSize returns the bytes an array element of storage type t takes.
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

// nonNull loads the pointer word of reference v into a register of the
// instruction's own, trapping on null.
func (c *compiler) nonNull(v value, at uint32, avoid regMask) reg {
	p := c.allocG(avoid)
	m := c.refOf(v)
	c.loadWord(p, m)
	c.jumpZero(p, c.trap(TrapNullDereference, at))
	return p
}

// gcOp compiles the GC instructions, reporting whether op is one.
func (c *compiler) gcOp(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixGC {
		return false
	}
	m := c.m
	l := c.req.Layout
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		ti := c.r.u32()
		if !pooled(m, ti) {
			c.goOp(op, at, uint64(ti), 0)
			break
		}
		c.structNew(ti, op == opcode.StructNew, at)
	case opcode.ArrayNew, opcode.ArrayNewDefault, opcode.ArrayFill:
		c.goOp(op, at, uint64(c.r.u32()), 0)
	case opcode.ArrayNewFixed, opcode.ArrayNewData, opcode.ArrayNewElem, opcode.ArrayCopy,
		opcode.ArrayInitData, opcode.ArrayInitElem:
		x := uint64(c.r.u32())
		c.goOp(op, at, x, uint64(c.r.u32()))
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti, fi := c.r.u32(), c.r.u32()
		ft := m.Types[ti].Struct.Fields[fi].Type
		off := c.cellOf(ti, fi)
		p := c.nonNull(c.pop(), at, 0)
		if ft.IsRef() {
			c.refCopy(c.refPos(len(c.stack)), mem{p, off})
			c.freeG(p)
			c.pushRef()
			return true
		}
		c.loadField(ft.Unpacked().Kind, ft.Kind, op == opcode.StructGetS, p, off+8)
		c.freeG(p)
	case opcode.StructSet:
		ti, fi := c.r.u32(), c.r.u32()
		ft := m.Types[ti].Struct.Fields[fi].Type
		off := c.cellOf(ti, fi)
		v := c.pop()
		p := c.nonNull(c.pop(), at, c.regsOf(v))
		if ft.IsRef() {
			c.refCopy(mem{p, off}, c.refOf(v))
		} else {
			// The payload word holds the value zero-extended.
			c.storeField(ft.Kind, v, p, off+8, true)
		}
		c.freeG(p)
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU:
		et := m.Types[c.r.u32()].Array.Elem.Type
		idx := c.pop()
		p := c.nonNull(c.pop(), at, c.regsOf(idx))
		i := c.intoG(idx, 1<<p)
		oob := c.trap(TrapArrayOutOfBounds, at)
		if et.IsRef() {
			c.elemAddr(p, i, l.ArrayElems, oob)
			c.freeG(i)
			c.refCopy(c.refPos(len(c.stack)), mem{p, 0})
			c.freeG(p)
			c.pushRef()
			return true
		}
		c.packedAddr(p, i, et.Kind, oob)
		c.freeG(i)
		c.loadField(et.Unpacked().Kind, et.Kind, op == opcode.ArrayGetS, p, 0)
		c.freeG(p)
	case opcode.ArraySet:
		et := m.Types[c.r.u32()].Array.Elem.Type
		v := c.pop()
		idx := c.pop()
		p := c.nonNull(c.pop(), at, c.regsOf(v)|c.regsOf(idx))
		i := c.intoG(idx, 1<<p|c.regsOf(v))
		oob := c.trap(TrapArrayOutOfBounds, at)
		if et.IsRef() {
			c.elemAddr(p, i, l.ArrayElems, oob)
			c.freeG(i)
			c.refCopy(mem{p, 0}, c.refOf(v))
		} else {
			c.packedAddr(p, i, et.Kind, oob)
			c.freeG(i)
			c.storeField(et.Kind, v, p, 0, false)
		}
		c.freeG(p)
	case opcode.ArrayLen:
		p := c.nonNull(c.pop(), at, 0)
		c.loadField(module.KindI32, module.KindI32, false, p, l.ArrayLen)
		c.freeG(p)
	case opcode.RefTest, opcode.RefTestNull:
		h := c.r.heapType()
		v := c.pop()
		c.setCond(c.refTest(c.refOf(v), h, op == opcode.RefTestNull))
	case opcode.RefCast, opcode.RefCastNull:
		h := c.r.heapType()
		v := c.stack[len(c.stack)-1]
		c.jcc(c.refTest(c.refOf(v), h, op == opcode.RefCastNull).Invert(), c.trap(TrapIllegalCast, at))
	case opcode.BrOnCast, opcode.BrOnCastFail:
		flags := c.r.byte()
		depth := int(c.r.u32())
		c.r.heapType()
		h := c.r.heapType()
		v := c.stack[len(c.stack)-1]
		cond := c.refTest(c.refOf(v), h, flags&2 != 0)
		if op == opcode.BrOnCastFail {
			cond = cond.Invert()
		}
		c.brOn(depth, cond)
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
		// The same reference, of the other hierarchy.
	case opcode.RefI31:
		r := c.intoG(c.pop(), 0)
		c.andImm(r, 0x7fff_ffff)
		c.refPut(c.refPos(len(c.stack)), immWord(l.I31Mark), regWord(r))
		c.freeG(r)
		c.pushRef()
	case opcode.I31GetS, opcode.I31GetU:
		v := c.pop()
		p := c.nonNull(v, at, 0)
		m := c.refOf(v)
		c.loadField(module.KindI32, module.KindI32, false, m.base, m.off+8)
		c.freeG(p)
		if op == opcode.I31GetS {
			c.signExtend31(c.stack[len(c.stack)-1].gpr())
		}
	default:
		return false
	}
	return true
}

// pooled reports whether native code makes the structs of type ti from
// the engine's pools: a type without v128 fields.
func pooled(m *module.Module, ti uint32) bool {
	for _, f := range m.Types[ti].Struct.Fields {
		if f.Type.Kind == module.KindV128 {
			return false
		}
	}
	return true
}

// structNew compiles struct.new, or, without fields, struct.new_default,
// of type ti: a struct taken from the level's pool of the type's (Ctx.Pools,
// poolTake), each field stored from its operand, the struct's reference
// stored into the first operand's position's slot through the barrier,
// and the pool's word of it cleared through the barrier, which shades the
// struct. The struct is in the pool until then, so that the collector sees
// it throughout. A reference field is stored plainly while the collector
// marks too, as the barrier's deletion part has nothing to shade -- the
// struct's fields are null until then -- and its insertion part is for a
// pointer only a goroutine's stack holds, where this one is in its
// operand's slot, which keeps it for the collector until a store through
// the barrier overwrites it. An empty pool, or a level without pools,
// exits for the engine to fill it (ExitAlloc), and the code takes from it
// again: every entry is in its slot or a constant, and stays so across
// the exit.
func (c *compiler) structNew(ti uint32, fields bool, at uint32) {
	fs := c.m.Types[ti].Struct.Fields
	if !fields {
		fs = nil
	}
	base := len(c.stack) - len(fs)
	for h := range c.stack {
		if h < base || c.stack[h].k != kConst {
			c.spill(h)
		}
	}
	vals := append([]value(nil), c.stack[base:]...)
	c.stack = c.stack[:base]
	p := c.allocG(0)
	q := c.allocG(1 << p)
	retry, slow := c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(retry)
	c.poolTake(p, q, ti, slow)
	off := int32(16)
	for i, f := range fs {
		if f.Type.IsRef() {
			src := c.refOf(vals[i])
			c.copyWord(mem{p, off}, src)
			c.copyWord(mem{p, off + 8}, mem{src.base, src.off + 8})
		} else {
			c.storeField(f.Type.Kind, vals[i], p, off+8, true)
		}
		off += 16
	}
	c.refPut(c.refPos(base), regWord(p), immWord(0))
	c.ptrPut(mem{q, 0}, immWord(0))
	c.freeG(p)
	c.freeG(q)
	c.later(func() {
		c.a.Bind(slow)
		resume := c.a.NewLabel()
		c.exit(ExitAlloc, resume, uint64(ti), uint64(at))
		c.resumeAt(resume)
		c.jmp(retry)
	})
	c.pushRef()
}

// regsOf returns the general register v holds, if any, as a mask.
func (c *compiler) regsOf(v value) regMask {
	if v.k == kReg && !isFloat(v.t) {
		return 1 << v.r
	}
	return 0
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

// heapTestOf returns what a non-null reference is tested for to be of heap
// type h, and for a defined type, its canonical index and the depth of its
// supertypes.
func (c *compiler) heapTestOf(h module.HeapType) (test heapTest, canon uint32, depth int) {
	if h.IsIndex() {
		ci := c.m.Canonical[h.Index()]
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
