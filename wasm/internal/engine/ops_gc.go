package engine

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// fieldGet returns a reader of a field of storage type t from a cell, as the
// value type it is read as: packed fields extended as signed asks.
func cellToVal(t module.ValueType, signed bool) func(c *Ref) Val {
	switch t.Kind {
	case module.KindI8:
		if signed {
			return func(c *Ref) Val { return Val{N: uint64(uint32(int32(int8(c.i))))} }
		}
		return func(c *Ref) Val { return Val{N: uint64(uint8(c.i))} }
	case module.KindI16:
		if signed {
			return func(c *Ref) Val { return Val{N: uint64(uint32(int32(int16(c.i))))} }
		}
		return func(c *Ref) Val { return Val{N: uint64(uint16(c.i))} }
	case module.KindV128:
		return func(c *Ref) Val {
			next := (*Ref)(unsafe.Add(unsafe.Pointer(c), unsafe.Sizeof(Ref{})))
			return Val{N: c.i, Hi: next.i}
		}
	case module.KindRef:
		return func(c *Ref) Val { return Val{R: *c} }
	}
	return func(c *Ref) Val { return Val{N: c.i} }
}

// valToCell stores v, of storage type t, into a cell.
func valToCell(t module.ValueType, c *Ref, v Val) {
	switch t.Kind {
	case module.KindI8:
		c.i = uint64(uint8(v.N))
	case module.KindI16:
		c.i = uint64(uint16(v.N))
	case module.KindV128:
		c.i = v.N
		(*Ref)(unsafe.Add(unsafe.Pointer(c), unsafe.Sizeof(Ref{}))).i = v.Hi
	case module.KindRef:
		*c = v.R
	case module.KindI32, module.KindF32:
		c.i = uint64(uint32(v.N))
	default:
		c.i = v.N
	}
}

// arrayGet reads element i of an array of storage type t.
func arrayGet(a *Array, t module.ValueType, i int, signed bool) Val {
	switch t.Kind {
	case module.KindRef:
		return Val{R: a.r[i]}
	case module.KindI8:
		if signed {
			return Val{N: uint64(uint32(int32(int8(a.b[i]))))}
		}
		return Val{N: uint64(a.b[i])}
	case module.KindI16:
		v := binary.LittleEndian.Uint16(a.b[2*i:])
		if signed {
			return Val{N: uint64(uint32(int32(int16(v))))}
		}
		return Val{N: uint64(v)}
	case module.KindI32, module.KindF32:
		return Val{N: uint64(binary.LittleEndian.Uint32(a.b[4*i:]))}
	case module.KindV128:
		return Val{N: binary.LittleEndian.Uint64(a.b[16*i:]), Hi: binary.LittleEndian.Uint64(a.b[16*i+8:])}
	}
	return Val{N: binary.LittleEndian.Uint64(a.b[8*i:])}
}

// arraySet writes element i of an array of storage type t.
func arraySet(a *Array, t module.ValueType, i int, v Val) {
	switch t.Kind {
	case module.KindRef:
		a.r[i] = v.R
	case module.KindI8:
		a.b[i] = byte(v.N)
	case module.KindI16:
		binary.LittleEndian.PutUint16(a.b[2*i:], uint16(v.N))
	case module.KindI32, module.KindF32:
		binary.LittleEndian.PutUint32(a.b[4*i:], uint32(v.N))
	case module.KindV128:
		binary.LittleEndian.PutUint64(a.b[16*i:], v.N)
		binary.LittleEndian.PutUint64(a.b[16*i+8:], v.Hi)
	default:
		binary.LittleEndian.PutUint64(a.b[8*i:], v.N)
	}
}

// pushVal pushes a tree producing the value of type t that get returns.
func (c *compiler) pushVal(t module.ValueType, get func(*frame) Val) {
	switch t.Kind {
	case module.KindI32:
		c.pushTree(t, ti32(func(f *frame) uint32 { return uint32(get(f).N) }))
	case module.KindI64:
		c.pushTree(t, ti64(func(f *frame) uint64 { return get(f).N }))
	case module.KindF32:
		c.pushTree(t, tf32(func(f *frame) float32 { return math.Float32frombits(uint32(get(f).N)) }))
	case module.KindF64:
		c.pushTree(t, tf64(func(f *frame) float64 { return math.Float64frombits(get(f).N) }))
	case module.KindV128:
		c.pushTree(t, tv128(func(f *frame) V128 { v := get(f); return V128{v.N, v.Hi} }))
	default:
		c.pushTree(t, tref(func(f *frame) Ref { return get(f).R }))
	}
}

// nonNull returns a reader of reference e, the operand of the instruction
// at offset at, that traps on null.
func (c *compiler) nonNull(e entry, at uint32) tref {
	v := c.ref(e)
	return func(f *frame) Ref {
		r := v(f)
		if r.IsNull() {
			trapAt(TrapNullDereference, at)
		}
		return r
	}
}

// arrayAndIndex returns readers of the array operand and the index operand
// of the instruction at offset at.
func (c *compiler) arrayAndIndex(arr, idx entry, at uint32) (tref, ti32) {
	return c.nonNull(arr, at), c.i32(idx)
}

// checkArray traps, as the instruction at offset at, unless [i, i+n) lies
// within array a.
func checkArray(a *Array, i, n, at uint32) {
	if uint64(i)+uint64(n) > uint64(a.n) {
		trapAt(TrapArrayOutOfBounds, at)
	}
}

// gcOps compiles the GC instructions, reporting whether op is one.
func (c *compiler) gcOps(op opcode.Opcode, at uint32) bool {
	m := c.m
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		ti := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		fields := m.Types[ti].Struct.Fields
		t := module.Ref(module.HeapIndex(ti))
		if op == opcode.StructNewDefault {
			c.pushTree(t, tref(func(f *frame) Ref { f.inst.budget.chargeGC(f.x, structSize(rt)); return newStruct(rt) }))
			return true
		}
		es := c.popN(len(fields))
		vals := make([]func(*frame) Val, len(es))
		for i, e := range es {
			vals[i] = c.val(e)
		}
		types := make([]module.ValueType, len(fields))
		for i, fl := range fields {
			types[i] = fl.Type
		}
		c.pushTree(t, tref(func(f *frame) Ref {
			r := newStruct(rt)
			for i, v := range vals {
				valToCell(types[i], cellAt(r.p, rt.cells[i]), v(f))
			}
			// Charged once the operands, which run first, have run.
			f.inst.budget.chargeGC(f.x, structSize(rt))
			return r
		}))
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti := c.r.u32()
		fi := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		ft := m.Types[ti].Struct.Fields[fi].Type
		cell := rt.cells[fi]
		read := cellToVal(ft, op == opcode.StructGetS)
		s := c.nonNull(c.pop(), at)
		c.pushVal(ft.Unpacked(), func(f *frame) Val { return read(cellAt(s(f).p, cell)) })
	case opcode.StructSet:
		ti := c.r.u32()
		fi := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		ft := m.Types[ti].Struct.Fields[fi].Type
		cell := rt.cells[fi]
		v := c.val(c.pop())
		s := c.nonNull(c.pop(), at)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			r := s(f)
			valToCell(ft, cellAt(r.p, cell), v(f))
			return 0
		})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		ti := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		n := c.i32(c.pop())
		t := module.Ref(module.HeapIndex(ti))
		if op == opcode.ArrayNewDefault {
			c.pushTree(t, tref(func(f *frame) Ref { return ArrayRef(newArray(rt, n(f), f.inst.budget, f.x, at)) }))
			return true
		}
		v := c.val(c.pop())
		c.pushTree(t, tref(func(f *frame) Ref {
			val := v(f)
			a := newArray(rt, n(f), f.inst.budget, f.x, at)
			for i := 0; i < a.n; i++ {
				arraySet(a, et, i, val)
			}
			return ArrayRef(a)
		}))
	case opcode.ArrayNewFixed:
		ti := c.r.u32()
		cnt := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		es := c.popN(int(cnt))
		vals := make([]func(*frame) Val, len(es))
		for i, e := range es {
			vals[i] = c.val(e)
		}
		c.pushTree(module.Ref(module.HeapIndex(ti)), tref(func(f *frame) Ref {
			a := newArray(rt, cnt, nil, nil, at)
			for i, v := range vals {
				arraySet(a, et, i, v(f))
			}
			// Charged once the operands, which run first, have run.
			f.inst.budget.chargeGC(f.x, arraySize(rt, cnt))
			return ArrayRef(a)
		}))
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		ti := c.r.u32()
		seg := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		n := c.i32(c.pop())
		s := c.i32(c.pop())
		data := op == opcode.ArrayNewData
		c.pushTree(module.Ref(module.HeapIndex(ti)), tref(func(f *frame) Ref {
			src, cnt := s(f), n(f)
			if data {
				d := f.inst.datas[seg]
				size := uint64(rt.elemSize)
				if uint64(src)+uint64(cnt)*size > uint64(len(d)) {
					trapAt(TrapDataSegmentOutOfBounds, at)
				}
				a := newArray(rt, cnt, f.inst.budget, f.x, at)
				copy(a.b, d[src:])
				return ArrayRef(a)
			}
			e := f.inst.elems[seg]
			if uint64(src)+uint64(cnt) > uint64(len(e)) {
				trapAt(TrapElementSegmentOutOfBounds, at)
			}
			a := newArray(rt, cnt, f.inst.budget, f.x, at)
			_ = et
			copy(a.r, e[src:])
			return ArrayRef(a)
		}))
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		signed := op == opcode.ArrayGetS
		idx := c.pop()
		arr, i := c.arrayAndIndex(c.pop(), idx, at)
		c.pushVal(et.Unpacked(), func(f *frame) Val {
			a := asArray(arr(f))
			x := i(f)
			if uint64(x) >= uint64(a.n) {
				trapAt(TrapArrayOutOfBounds, at)
			}
			return arrayGet(a, et, int(x), signed)
		})
	case opcode.ArraySet:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		v := c.val(c.pop())
		idx := c.pop()
		arr, i := c.arrayAndIndex(c.pop(), idx, at)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			a := asArray(arr(f))
			x := i(f)
			val := v(f)
			if uint64(x) >= uint64(a.n) {
				trapAt(TrapArrayOutOfBounds, at)
			}
			arraySet(a, et, int(x), val)
			return 0
		})
	case opcode.ArrayLen:
		arr := c.nonNull(c.pop(), at)
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(asArray(arr(f)).n) }))
	case opcode.ArrayFill:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		n := c.i32(c.pop())
		v := c.val(c.pop())
		d := c.i32(c.pop())
		arr := c.nonNull(c.pop(), at)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			a := asArray(arr(f))
			dst, val, cnt := d(f), v(f), n(f)
			checkArray(a, dst, cnt, at)
			for i := uint32(0); i < cnt; i++ {
				arraySet(a, et, int(dst+i), val)
			}
			return 0
		})
	case opcode.ArrayCopy:
		dti := c.r.u32()
		c.r.u32()
		et := m.Types[dti].Array.Elem.Type
		n := c.i32(c.pop())
		s := c.i32(c.pop())
		sarr := c.nonNull(c.pop(), at)
		d := c.i32(c.pop())
		darr := c.nonNull(c.pop(), at)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			da := asArray(darr(f))
			dst := d(f)
			sa := asArray(sarr(f))
			src, cnt := s(f), n(f)
			checkArray(da, dst, cnt, at)
			checkArray(sa, src, cnt, at)
			if et.IsRef() {
				copy(da.r[dst:dst+cnt], sa.r[src:src+cnt])
			} else {
				size := uint32(et.Size())
				copy(da.b[dst*size:(dst+cnt)*size], sa.b[src*size:(src+cnt)*size])
			}
			return 0
		})
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		ti := c.r.u32()
		seg := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		n := c.i32(c.pop())
		s := c.i32(c.pop())
		d := c.i32(c.pop())
		arr := c.nonNull(c.pop(), at)
		data := op == opcode.ArrayInitData
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			a := asArray(arr(f))
			dst, src, cnt := d(f), s(f), n(f)
			checkArray(a, dst, cnt, at)
			if data {
				b := f.inst.datas[seg]
				size := uint64(et.Size())
				if uint64(src)+uint64(cnt)*size > uint64(len(b)) {
					trapAt(TrapDataSegmentOutOfBounds, at)
				}
				copy(a.b[uint64(dst)*size:], b[src:uint64(src)+uint64(cnt)*size])
				return 0
			}
			e := f.inst.elems[seg]
			if uint64(src)+uint64(cnt) > uint64(len(e)) {
				trapAt(TrapElementSegmentOutOfBounds, at)
			}
			copy(a.r[dst:dst+cnt], e[src:src+cnt])
			return 0
		})
	case opcode.RefI31:
		v := c.i32(c.pop())
		c.pushTree(module.Ref(module.HeapI31), tref(func(f *frame) Ref { return I31(v(f)) }))
	case opcode.I31GetS, opcode.I31GetU:
		r := c.nonNull(c.pop(), at)
		if op == opcode.I31GetS {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(r(f).I31Signed()) }))
		} else {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return r(f).I31Unsigned() }))
		}
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
		e := c.pop()
		h := module.HeapAny
		if op == opcode.ExternConvertAny {
			h = module.HeapExtern
		}
		e.t = module.RefMaybeNull(h, e.t.Nullable)
		c.push(e)
	case opcode.RefTest, opcode.RefTestNull, opcode.RefCast, opcode.RefCastNull:
		h := c.r.heapType()
		nullable := op == opcode.RefTestNull || op == opcode.RefCastNull
		e := c.pop()
		v := c.ref(e)
		test := func(r Ref) bool {
			if r.IsNull() {
				return nullable
			}
			return RefIsSub(r, h, m)
		}
		if op == opcode.RefTest || op == opcode.RefTestNull {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return b2i(test(v(f))) }))
			return true
		}
		c.pushTree(module.RefMaybeNull(h, nullable), tref(func(f *frame) Ref {
			r := v(f)
			if !test(r) {
				trapAt(TrapIllegalCast, at)
			}
			return r
		}))
	case opcode.BrOnCast, opcode.BrOnCastFail:
		flags := c.r.byte()
		depth := int(c.r.u32())
		c.r.heapType()
		h2 := c.r.heapType()
		c.brOnCast(op == opcode.BrOnCastFail, depth, h2, flags&2 != 0)
	default:
		return false
	}
	return true
}

// brOnCast compiles br_on_cast and br_on_cast_fail to target heap type h.
func (c *compiler) brOnCast(fail bool, depth int, h module.HeapType, nullable bool) {
	m := c.m
	t := c.ctrl[len(c.ctrl)-1-depth]
	types := t.labelTypes()
	c.flushBelow(0)
	for i := 0; i < len(c.stack); i++ {
		c.materialize(i)
	}
	ref := c.stack[len(c.stack)-1]
	rs := ref.slot
	base := len(c.stack) - len(types)
	var copies []stmt
	for i, vt := range types {
		if s := copySlot(vt, c.slotEntry(vt, base+i).slot, c.slotEntry(vt, t.base+i).slot); s != nil {
			copies = append(copies, s)
		}
	}
	sig := int32(depth + 1)
	c.emit(func(f *frame) int32 {
		r := f.r[rs]
		ok := false
		if r.IsNull() {
			ok = nullable
		} else {
			ok = RefIsSub(r, h, m)
		}
		if ok != fail {
			for _, s := range copies {
				s(f)
			}
			return sig
		}
		return 0
	})
	// Falling through, the reference is of the type the cast leaves.
	target := module.RefMaybeNull(h, nullable)
	if fail {
		c.stack[len(c.stack)-1].t = target
	}
}

// evalConstGC evaluates a GC instruction of a constant expression.
func evalConstGC(inst *Instance, op opcode.Opcode, r *reader, stack *[]Val) (Val, bool) {
	m := inst.Module
	pop := func() Val {
		s := *stack
		v := s[len(s)-1]
		*stack = s[:len(s)-1]
		return v
	}
	push := func(v Val) { *stack = append(*stack, v) }
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		ti := r.u32()
		rt := rttOf(m.Canonical[ti])
		inst.budget.chargeGC(nil, structSize(rt))
		s := newStruct(rt)
		if op == opcode.StructNew {
			fields := m.Types[ti].Struct.Fields
			vals := make([]Val, len(fields))
			for i := len(fields) - 1; i >= 0; i-- {
				vals[i] = pop()
			}
			for i, fl := range fields {
				valToCell(fl.Type, cellAt(s.p, rt.cells[i]), vals[i])
			}
		}
		push(Val{R: s})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		ti := r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		n := uint32(pop().N)
		var v Val
		if op == opcode.ArrayNew {
			v = pop()
		}
		a := newArray(rt, n, inst.budget, nil, 0)
		if op == opcode.ArrayNew {
			for i := 0; i < a.n; i++ {
				arraySet(a, et, i, v)
			}
		}
		push(Val{R: ArrayRef(a)})
	case opcode.ArrayNewFixed:
		ti := r.u32()
		n := r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		vals := make([]Val, n)
		for i := int(n) - 1; i >= 0; i-- {
			vals[i] = pop()
		}
		a := newArray(rt, n, inst.budget, nil, 0)
		for i, v := range vals {
			arraySet(a, et, i, v)
		}
		push(Val{R: ArrayRef(a)})
	case opcode.RefI31:
		push(Val{R: I31(uint32(pop().N))})
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
	default:
		return Val{}, false
	}
	return Val{}, true
}
