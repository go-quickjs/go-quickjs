//go:build amd64 || arm64

package jit

import (
	"math/bits"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// GC objects. A struct is cells of two words, a reference's or a number's
// in its second word, the first cell its header; an array is a header, its
// length, and its elements, packed numbers or references, in memory of
// their own (Layout). Native code reads and writes fields and elements,
// tests and casts, makes i31s, makes structs and arrays from the pools the
// engine fills (structNew, arrayNew) and arrays from segments
// (arrayNewSeg); what else allocates runs in Go from the allocation stub
// (StubOp), and what copies or fills ranges in Go (ExitOp), whose
// allocations charge the instance's budget as the interpreter's do.

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
		c.structNew(c.r.u32(), op == opcode.StructNew, at)
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		ti := c.r.u32()
		if n := c.stack[len(c.stack)-1]; n.k == kConst {
			if class, ok := arrayPooled(m, l, ti, uint32(n.c), op == opcode.ArrayNew); ok {
				c.arrayNew(op, ti, uint32(n.c), class, at)
				break
			}
		} else if op == opcode.ArrayNewDefault && lengthPooled(m, l, ti) &&
			c.refSlotOff(len(c.stack)-1)/16 < AllocLength {
			c.arrayNewLength(ti, at)
			break
		}
		if op == opcode.ArrayNew && lengthPooled(m, l, ti) && c.refSlotOff(len(c.stack)-1)/16 < AllocLength {
			c.arrayNewFill(ti, at)
			break
		}
		c.goOp(op, at, uint64(ti), 0)
	case opcode.ArrayNewFixed:
		ti, n := c.r.u32(), c.r.u32()
		if class, ok := arrayPooled(m, l, ti, n, false); ok {
			c.arrayNew(op, ti, n, class, at)
			break
		}
		c.goOp(op, at, uint64(ti), uint64(n))
	case opcode.ArrayFill:
		c.goOp(op, at, uint64(c.r.u32()), 0)
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		ti, seg := c.r.u32(), c.r.u32()
		if segPooled(m, l, ti, seg) && c.refSlotOff(len(c.stack)-2)/16 < AllocLength {
			c.arrayNewSeg(ti, seg, op == opcode.ArrayNewData, at)
			break
		}
		c.goOp(op, at, uint64(ti), uint64(seg))
	case opcode.ArrayCopy, opcode.ArrayInitData, opcode.ArrayInitElem:
		x := uint64(c.r.u32())
		c.goOp(op, at, x, uint64(c.r.u32()))
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti, fi := c.r.u32(), c.r.u32()
		ft := m.Types[ti].Struct.Fields[fi].Type
		off := c.cellOf(ti, fi)
		p := c.nonNull(c.pop(), at, 0)
		if ft.Kind == module.KindV128 {
			x := c.allocX(0)
			c.loadCells(x, p, off+8)
			c.freeG(p)
			c.pushReg(v128, uint8(x))
			return true
		}
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
		if ft.Kind == module.KindV128 {
			x := c.intoX(v)
			c.storeCells(p, off+8, x)
			c.freeX(x)
		} else if ft.IsRef() {
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
		if et.Kind == module.KindV128 {
			// A v128 takes 16 bytes, as a reference does.
			c.elemAddr(p, i, l.ArrayElems, oob)
			c.freeG(i)
			x := c.allocX(0)
			c.loadVecAt(x, p, 0)
			c.freeG(p)
			c.pushReg(v128, uint8(x))
			return true
		}
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
		if et.Kind == module.KindV128 {
			c.elemAddr(p, i, l.ArrayElems, oob)
			c.freeG(i)
			x := c.intoX(v)
			c.storeVecAt(p, 0, x)
			c.freeX(x)
		} else if et.IsRef() {
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

// StubOp reports whether the engine runs instruction op, which native
// code leaves to Go (ExitOp), from the allocation stub where it can
// (Request.AllocStub): the instructions that make an object, whose only
// traps are of a length or a segment's range, which the engine checks
// before it makes the object, leaving those to the exit.
func StubOp(op opcode.Opcode) bool {
	switch op {
	case opcode.StructNew, opcode.StructNewDefault, opcode.ArrayNew, opcode.ArrayNewDefault,
		opcode.ArrayNewFixed, opcode.ArrayNewData, opcode.ArrayNewElem:
		return true
	}
	return false
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
	c.poolTake(p, q, ti, -1, slow)
	off := int32(16)
	for i, f := range fs {
		switch {
		case f.Type.IsRef():
			src := c.refOf(vals[i])
			c.copyWord(mem{p, off}, src)
			c.copyWord(mem{p, off + 8}, mem{src.base, src.off + 8})
		case f.Type.Kind == module.KindV128:
			// Its halves in the payload words of its two cells.
			c.storeCells(p, off+8, c.readX(vals[i]))
			off += 16
		default:
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
		c.exitAlloc(resume, uint64(ti), uint64(at))
		c.resumeAt(resume)
		c.jmp(retry)
	})
	c.pushRef()
}

// arrayNewMost is the most elements array.new stores natively, its value
// stored into each in turn.
const arrayNewMost = 16

// arrayPooled reports whether native code makes the arrays of type ti and
// length n from the engine's pools (Layout.ArrayClass) -- arrays of numbers
// or references, not v128s, whose elements take at most what the pools'
// largest blocks have room for, and for array.new (fill) no more than
// arrayNewMost of them -- and the class of their blocks.
func arrayPooled(m *module.Module, l *Layout, ti, n uint32, fill bool) (int, bool) {
	if fill && n > arrayNewMost {
		return 0, false
	}
	u := (uint64(n)*uint64(elemSize(m.Types[ti].Array.Elem.Type.Kind)) + 15) / 16
	if u >= uint64(len(l.ArrayClass)) {
		return 0, false
	}
	return int(l.ArrayClass[u]), true
}

// lengthPooled reports whether native code makes the arrays of type ti of
// a length not constant from the engine's pools, where they have room for
// them: arrays of numbers or references, not v128s.
func lengthPooled(m *module.Module, l *Layout, ti uint32) bool {
	return len(l.ArrayClass) > 0
}

// arrayNewLength compiles array.new_default of type ti of a length not
// constant: an array whose elements take at most what the pools' largest
// blocks have room for is taken from the level's pool of its class's
// (arrayClassAt, poolTakeClass), as arrayNew takes one, its length stored;
// the length is left in Arg2 for the exits. A longer one, or one past the
// most an array may have, exits for the engine to make it, or trap, as the
// interpreter does, into the operand's position's slot (ExitAlloc,
// AllocMake). Every entry is in its slot or a constant across the exits.
func (c *compiler) arrayNewLength(ti uint32, at uint32) {
	l := c.req.Layout
	base := len(c.stack) - 1
	for h := range c.stack {
		if c.stack[h].k != kConst {
			c.spill(h)
		}
	}
	length := c.stack[base]
	c.stack = c.stack[:base]
	slot := uint64(c.refSlotOff(base) / 16)
	p := c.allocG(0)
	q := c.allocG(1 << p)
	k := c.allocG(1<<p | 1<<q)
	retry, slow, big, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(retry)
	c.loadLength(k, length, true)
	c.arrayClassAt(k, uint8(bits.TrailingZeros(uint(elemSize(c.m.Types[ti].Array.Elem.Type.Kind)))), big)
	c.poolTakeClass(p, q, k, ti, slow)
	c.loadLength(k, length, false)
	c.storeReg(p, l.ArrayLen, k)
	c.refPut(c.refPos(base), regWord(p), immWord(0))
	c.ptrPut(mem{q, 0}, immWord(0))
	c.a.Bind(done)
	c.freeG(p)
	c.freeG(q)
	c.freeG(k)
	c.later(func() {
		c.a.Bind(slow)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|AllocLength<<32, uint64(at))
		c.resumeAt(resume)
		c.jmp(retry)
	})
	c.later(func() {
		c.a.Bind(big)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|(AllocMake|slot)<<32, uint64(at))
		c.resumeAt(resume)
		c.jmp(done)
	})
	c.pushRef()
}

// arrayNewFill compiles array.new of type ti of a length not constant, or
// past what arrayNew stores one by one: the array taken from the pool of
// its class as arrayNewLength takes one, or, longer, made by the engine
// into the length's position's reference slot (ExitAlloc, AllocMake) and
// moved to the value's after; then the value stored into each element in
// a loop -- a reference plainly, as a new struct's field is -- before the
// array is in its slot. Every entry is in its slot or a constant across
// the exits.
func (c *compiler) arrayNewFill(ti uint32, at uint32) {
	l := c.req.Layout
	et := c.m.Types[ti].Array.Elem.Type
	base := len(c.stack) - 2
	for h := range c.stack {
		if c.stack[h].k != kConst {
			c.spill(h)
		}
	}
	val, length := c.stack[base], c.stack[base+1]
	c.stack = c.stack[:base]
	slot := uint64(c.refSlotOff(base+1) / 16)
	p := c.allocG(0)
	q := c.allocG(1 << p)
	k := c.allocG(1<<p | 1<<q)
	e := c.allocG(1<<p | 1<<q | 1<<k)
	retry, slow, big, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	size := int32(elemSize(et.Kind))
	// fillElems stores the value into each of the k elements of array p.
	fillElems := func() {
		c.loadWord(e, mem{p, l.ArrayElems})
		if et.Kind == module.KindV128 {
			x := c.readX(val)
			c.fillLoop(e, k, size, func() { c.storeVecAt(e, 0, x) })
			return
		}
		c.fillLoop(e, k, size, func() {
			if et.IsRef() {
				src := c.refOf(val)
				c.copyWord(mem{e, 0}, src)
				c.copyWord(mem{e, 8}, mem{src.base, src.off + 8})
			} else {
				c.storeField(et.Kind, val, e, 0, false)
			}
		})
	}
	c.a.Bind(retry)
	c.loadLength(k, length, true)
	c.arrayClassAt(k, uint8(bits.TrailingZeros(uint(size))), big)
	c.poolTakeClass(p, q, k, ti, slow)
	c.loadLength(k, length, false)
	c.storeReg(p, l.ArrayLen, k)
	fillElems()
	c.refPut(c.refPos(base), regWord(p), immWord(0))
	c.ptrPut(mem{q, 0}, immWord(0))
	c.a.Bind(done)
	c.freeG(p)
	c.freeG(q)
	c.freeG(k)
	c.freeG(e)
	c.later(func() {
		c.a.Bind(slow)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|AllocLength<<32, uint64(at))
		c.resumeAt(resume)
		c.jmp(retry)
	})
	c.later(func() {
		c.a.Bind(big)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|(AllocMake|slot)<<32, uint64(at))
		c.resumeAt(resume)
		c.loadWord(p, c.refPos(base+1))
		c.loadLength(k, length, false)
		fillElems()
		c.refCopy(c.refPos(base), c.refPos(base+1))
		c.jmp(done)
	})
	c.pushRef()
}

// segPooled reports whether native code makes the arrays of type ti from
// segment seg itself (arrayNewSeg): an array of numbers but v128s, or of
// references, where the layout gives the segments' entries and the
// pools' classes, and the segment's entry is within a 32-bit offset.
func segPooled(m *module.Module, l *Layout, ti, seg uint32) bool {
	return l.Segment > 0 && len(l.ArrayClass) > 0 && int64(seg)*int64(l.Segment) < 1<<30
}

// arrayNewSeg compiles array.new_data (data) or array.new_elem of type ti
// from segment seg, of a length not constant: the segment's range -- the
// offset and the length's elements, in bytes for data, in references for
// elements -- checked against its length (Ctx.Datas, Ctx.Elems; a
// dropped segment's is 0), a trap past it; the array taken from the pool
// of its class as arrayNewLength takes one, its length stored, or, past
// the pools, made by the engine into the offset's position's reference
// slot (ExitAlloc, AllocMake); and the segment's bytes or references
// copied into its elements -- a reference plainly, as a new struct's
// field is, the segment holding it meanwhile -- before the array is in its
// slot. Every entry is in its slot or a constant across the exits.
func (c *compiler) arrayNewSeg(ti, seg uint32, data bool, at uint32) {
	l := c.req.Layout
	et := c.m.Types[ti].Array.Elem.Type
	base := len(c.stack) - 2
	for h := range c.stack {
		if c.stack[h].k != kConst {
			c.spill(h)
		}
	}
	src, length := c.stack[base], c.stack[base+1]
	c.stack = c.stack[:base]
	slot := uint64(c.refSlotOff(base) / 16)
	p := c.allocG(0)
	q := c.allocG(1 << p)
	k := c.allocG(1<<p | 1<<q)
	e := c.allocG(1<<p | 1<<q | 1<<k)
	shift := uint8(bits.TrailingZeros(uint(elemSize(et.Kind))))
	segs, trap, unit := offDatas, uint64(TrapDataSegmentOutOfBounds), shift
	if !data {
		segs, trap, unit = offElems, TrapElementSegmentOutOfBounds, 0
	}
	entry := int32(seg) * l.Segment
	retry, slow, big, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.a.Bind(retry)
	c.loadLength(k, length, true)
	c.segRange(e, k, src, segs, entry, unit, c.trap(trap, at))
	c.arrayClassAt(k, shift, big)
	c.poolTakeClass(p, q, k, ti, slow)
	c.loadLength(k, length, false)
	c.storeReg(p, l.ArrayLen, k)
	// The pool's word's address waits in the context's Arg3, q copying.
	c.storeReg(rCtx, offArg0+24, q)
	c.segCopy(p, k, e, q, src, segs, entry, shift, data)
	c.loadWord(q, mem{rCtx, offArg0 + 24})
	c.refPut(c.refPos(base), regWord(p), immWord(0))
	c.ptrPut(mem{q, 0}, immWord(0))
	c.a.Bind(done)
	c.freeG(p)
	c.freeG(q)
	c.freeG(k)
	c.freeG(e)
	c.later(func() {
		c.a.Bind(slow)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|AllocLength<<32, uint64(at))
		c.resumeAt(resume)
		c.jmp(retry)
	})
	c.later(func() {
		c.a.Bind(big)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|(AllocMake|slot)<<32, uint64(at))
		c.resumeAt(resume)
		c.loadWord(p, c.refPos(base))
		c.loadLength(k, length, false)
		c.segCopy(p, k, e, q, src, segs, entry, shift, data)
		c.jmp(done)
	})
	c.pushRef()
}

// elemSize returns the bytes an array element of storage type t takes: a
// reference's two words, or its number's, a v128's 16 bytes.
func elemSize(t module.Kind) int {
	if t == module.KindRef || t == module.KindV128 {
		return 16
	}
	return storageSize(t)
}

// arrayNew compiles array.new_fixed of n elements, or array.new or
// array.new_default of length n, a constant, of type ti, from the level's
// pool of the arrays of class (Ctx.Pools, poolTake), as structNew does a
// struct: the array taken, its length stored and its elements -- the
// operands of array.new_fixed in order, n times array.new's value, nothing
// for array.new_default, whose elements the pool's arrays have zero -- a
// reference plainly, as a new struct's field is; the array's reference
// stored into the first operand's position's slot through the barrier, and
// the pool's word of it cleared through the barrier.
func (c *compiler) arrayNew(op opcode.Opcode, ti, n uint32, class int, at uint32) {
	l := c.req.Layout
	et := c.m.Types[ti].Array.Elem.Type
	nops := 1
	switch op {
	case opcode.ArrayNewFixed:
		nops = int(n)
	case opcode.ArrayNew:
		nops = 2
	}
	base := len(c.stack) - nops
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
	c.poolTake(p, q, ti, class, slow)
	c.storeWord(p, l.ArrayLen, uint64(n))
	size := int32(elemSize(et.Kind))
	for i := int32(0); i < int32(n) && op != opcode.ArrayNewDefault; i++ {
		v := vals[0]
		if op == opcode.ArrayNewFixed {
			v = vals[i]
		}
		off := l.ArrayBlock + i*size
		switch {
		case et.IsRef():
			src := c.refOf(v)
			c.copyWord(mem{p, off}, src)
			c.copyWord(mem{p, off + 8}, mem{src.base, src.off + 8})
		case et.Kind == module.KindV128:
			c.storeVecAt(p, off, c.readX(v))
		default:
			c.storeField(et.Kind, v, p, off, false)
		}
	}
	c.refPut(c.refPos(base), regWord(p), immWord(0))
	c.ptrPut(mem{q, 0}, immWord(0))
	c.freeG(p)
	c.freeG(q)
	c.later(func() {
		c.a.Bind(slow)
		resume := c.a.NewLabel()
		c.exitAlloc(resume, uint64(ti)|uint64(class+1)<<32, uint64(at))
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
		return testIndex, uint32(ci), int(module.Canonical(ci).Depth)
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
