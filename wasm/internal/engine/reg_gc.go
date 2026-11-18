package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's GC instructions. Each is an rExt operation whose
// destination, if it has one, is A, and whose types and operands are a
// record of the code's (regGC) that BC names -- or, for a branch, whose BC
// is its offset and whose record is named by the word after.

// regGC is a GC instruction's record: what its operands are and what its
// types say.
type regGC struct {
	rt *rtt
	// ops are the operands' slots, in the instruction's order; vals the
	// values a struct.new or array.new_fixed stores, with their kinds.
	ops  []uint32
	vals []regMove
	// types are struct.new's fields' types; et a field's or an element's
	// storage type, signed whether a packed one is read as signed, k the
	// kind of value read or stored, cell a field's cell.
	types  []module.ValueType
	et     module.ValueType
	signed bool
	k      uint8
	cell   int
	read   func(c *Ref) Val
	// seg is a segment; data says it is a data segment.
	seg  uint32
	data bool
	// h and nullable are a test's or a cast's target; m the module whose
	// types h names.
	h        module.HeapType
	nullable bool
	m        *module.Module
}

// The GC operations of the second table.
const (
	xStructNew        regOp = 112 + iota // reference A = a struct of the record's values
	xStructNewDefault                    // reference A = a struct of default fields
	xStructGet                           // A = a field of struct ops[0]
	xStructSet                           // a field of struct ops[0] = ops[1]
	xArrayNew                            // reference A = an array of ops[1] elements of ops[0]
	xArrayNewDefault                     // reference A = an array of ops[0] default elements
	xArrayNewFixed                       // reference A = an array of the record's values
	xArrayNewSegment                     // reference A = an array from a segment, at ops[0], ops[1] elements
	xArrayGet                            // A = element ops[1] of array ops[0]
	xArraySet                            // element ops[1] of array ops[0] = ops[2]
	xArrayLen                            // A = the length of array ops[0]
	xArrayFill                           // array.fill: array, start, value, count
	xArrayCopy                           // array.copy: array, start, array, start, count
	xArrayInit                           // array.init_data and array.init_elem: array, start, source, count
	xRefI31                              // reference A = the i31 of ops[0]
	xI31Get                              // A = the i31 ops[0], signed or not
	xRefTest                             // A = whether reference ops[0] is of the record's type
	xRefCast                             // trap unless reference ops[0] is of the record's type
	xBrOnCast                            // if reference A is of the type of record (the word after): pc += BC
	xBrOnCastFail                        // if reference A is not of that type: pc += BC
)

// gcRec appends a GC record and returns its index.
func (c *regCompiler) gcRec(g regGC) uint32 {
	c.out.gc = append(c.out.gc, g)
	return uint32(len(c.out.gc) - 1)
}

// operands pops n operands and returns their slots, bottom first, and the
// position they began at.
func (c *regCompiler) operands(n int) ([]uint32, int) {
	base := len(c.stack) - n
	ops := make([]uint32, n)
	for i, e := range c.stack[base:] {
		ops[i] = uint32(c.slotOf(e, base+i))
	}
	c.stack = c.stack[:base]
	return ops, base
}

// gcResult emits GC operation x of record g writing a value of type t at
// position h, and pushes it.
func (c *regCompiler) gcResult(x uint8, t module.ValueType, h int, g regGC) {
	c.def(t, h, c.emitBC(rExt, x, c.dst(t, h), c.gcRec(g)))
}

// gcOp compiles a GC instruction, reporting whether op is one.
func (c *regCompiler) gcOp(op opcode.Opcode) bool {
	m := c.m
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		ti := c.r.u32()
		g := regGC{rt: rttOf(m.Canonical[ti])}
		t := module.Ref(module.HeapIndex(ti))
		if op == opcode.StructNewDefault {
			c.gcResult(xStructNewDefault, t, len(c.stack), g)
			return true
		}
		fields := m.Types[ti].Struct.Fields
		base := len(c.stack) - len(fields)
		for i, e := range c.stack[base:] {
			g.types = append(g.types, fields[i].Type)
			g.vals = append(g.vals, regMove{from: uint32(c.slotOf(e, base+i)), k: kindOf(e.t)})
		}
		c.stack = c.stack[:base]
		c.gcResult(xStructNew, t, base, g)
	case opcode.StructGet, opcode.StructGetS, opcode.StructGetU:
		ti := c.r.u32()
		fi := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		ft := m.Types[ti].Struct.Fields[fi].Type
		ops, h := c.operands(1)
		t := ft.Unpacked()
		c.gcResult(xStructGet, t, h, regGC{ops: ops, cell: rt.cells[fi], read: cellToVal(ft, op == opcode.StructGetS), k: kindOf(t)})
	case opcode.StructSet:
		ti := c.r.u32()
		fi := c.r.u32()
		rt := rttOf(m.Canonical[ti])
		ft := m.Types[ti].Struct.Fields[fi].Type
		ops, _ := c.operands(2)
		c.emitBC(rExt, xStructSet, 0, c.gcRec(regGC{ops: ops, cell: rt.cells[fi], et: ft, k: kindOf(ft.Unpacked())}))
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		ti := c.r.u32()
		g := regGC{rt: rttOf(m.Canonical[ti]), et: m.Types[ti].Array.Elem.Type}
		t := module.Ref(module.HeapIndex(ti))
		x := xArrayNewDefault
		n := 1
		if op == opcode.ArrayNew {
			x, n = xArrayNew, 2
			g.k = kindOf(c.stack[len(c.stack)-2].t)
		}
		var h int
		g.ops, h = c.operands(n)
		c.gcResult(x, t, h, g)
	case opcode.ArrayNewFixed:
		ti := c.r.u32()
		cnt := int(c.r.u32())
		g := regGC{rt: rttOf(m.Canonical[ti]), et: m.Types[ti].Array.Elem.Type}
		base := len(c.stack) - cnt
		for i, e := range c.stack[base:] {
			g.vals = append(g.vals, regMove{from: uint32(c.slotOf(e, base+i)), k: kindOf(e.t)})
		}
		c.stack = c.stack[:base]
		c.gcResult(xArrayNewFixed, module.Ref(module.HeapIndex(ti)), base, g)
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		ti := c.r.u32()
		g := regGC{rt: rttOf(m.Canonical[ti]), seg: c.r.u32(), data: op == opcode.ArrayNewData}
		var h int
		g.ops, h = c.operands(2)
		c.gcResult(xArrayNewSegment, module.Ref(module.HeapIndex(ti)), h, g)
	case opcode.ArrayGet, opcode.ArrayGetS, opcode.ArrayGetU:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		ops, h := c.operands(2)
		t := et.Unpacked()
		c.gcResult(xArrayGet, t, h, regGC{ops: ops, et: et, signed: op == opcode.ArrayGetS, k: kindOf(t)})
	case opcode.ArraySet:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		ops, _ := c.operands(3)
		c.emitBC(rExt, xArraySet, 0, c.gcRec(regGC{ops: ops, et: et, k: kindOf(et.Unpacked())}))
	case opcode.ArrayLen:
		ops, h := c.operands(1)
		c.gcResult(xArrayLen, module.I32, h, regGC{ops: ops})
	case opcode.ArrayFill:
		ti := c.r.u32()
		et := m.Types[ti].Array.Elem.Type
		ops, _ := c.operands(4)
		c.emitBC(rExt, xArrayFill, 0, c.gcRec(regGC{ops: ops, et: et, k: kindOf(et.Unpacked())}))
	case opcode.ArrayCopy:
		dti := c.r.u32()
		c.r.u32()
		ops, _ := c.operands(5)
		c.emitBC(rExt, xArrayCopy, 0, c.gcRec(regGC{ops: ops, et: m.Types[dti].Array.Elem.Type}))
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		ti := c.r.u32()
		g := regGC{et: m.Types[ti].Array.Elem.Type, seg: c.r.u32(), data: op == opcode.ArrayInitData}
		g.ops, _ = c.operands(4)
		c.emitBC(rExt, xArrayInit, 0, c.gcRec(g))
	case opcode.RefI31:
		ops, h := c.operands(1)
		c.gcResult(xRefI31, module.Ref(module.HeapI31), h, regGC{ops: ops})
	case opcode.I31GetS, opcode.I31GetU:
		ops, h := c.operands(1)
		c.gcResult(xI31Get, module.I32, h, regGC{ops: ops, signed: op == opcode.I31GetS})
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
		e := c.pop()
		hp := module.HeapAny
		if op == opcode.ExternConvertAny {
			hp = module.HeapExtern
		}
		e.t = module.RefMaybeNull(hp, e.t.Nullable)
		e.def = -1
		c.push(e)
	case opcode.RefTest, opcode.RefTestNull, opcode.RefCast, opcode.RefCastNull:
		hp := c.r.heapType()
		nullable := op == opcode.RefTestNull || op == opcode.RefCastNull
		g := regGC{h: hp, nullable: nullable, m: m}
		if op == opcode.RefTest || op == opcode.RefTestNull {
			var h int
			g.ops, h = c.operands(1)
			c.gcResult(xRefTest, module.I32, h, g)
			return true
		}
		e := c.pop()
		h := len(c.stack)
		s := c.slotOf(e, h)
		g.ops = []uint32{uint32(s)}
		c.emitBC(rExt, xRefCast, 0, c.gcRec(g))
		c.push(rentry{t: module.RefMaybeNull(hp, nullable), slot: s, local: e.local, def: -1})
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

// brOnCast compiles br_on_cast, and br_on_cast_fail (fail), to heap type h.
func (c *regCompiler) brOnCast(fail bool, depth int, h module.HeapType, nullable bool) {
	ti := len(c.ctrl) - 1 - depth
	t := c.ctrl[ti]
	n := len(t.labelTypes())
	top := len(c.stack) - 1
	if c.stack[top].konst {
		c.materialize(top)
	}
	ref := c.stack[top]
	es := c.stack[len(c.stack)-n:]
	rec := uint64(c.gcRec(regGC{h: h, nullable: nullable, m: c.m}))
	taken, skip := xBrOnCast, xBrOnCastFail
	if fail {
		taken, skip = xBrOnCastFail, xBrOnCast
	}
	if ti != 0 && !c.moves(t, es, false) && !c.leaving(ti) {
		c.branchTo(t, rfix{pc: c.emitBC(rExt, taken, rf(ref.slot), 0, rec), kind: fixRel32})
	} else {
		x := rfix{pc: c.emitBC(rExt, skip, rf(ref.slot), 0, rec), kind: fixRel32}
		c.branchOut(ti, es)
		c.bind([]rfix{x})
	}
	if fail {
		c.stack[top].t = module.RefMaybeNull(h, nullable)
	}
}

// regRec returns the GC record BC names.
func (f *frame) regRec(w uint64) *regGC { return &f.rc.gc[uint32(w>>32)] }

// nonNullAt returns reference slot s of f, trapping on null as the
// instruction at pc.
func (f *frame) nonNullAt(s uint32, pc int32) Ref {
	r := f.r[s]
	if r.IsNull() {
		trapAt(TrapNullDereference, regAt(pc))
	}
	return r
}

// num returns numeric slot s.
func num(l unsafe.Pointer, s uint32) uint64 { return *(*uint64)(unsafe.Add(l, uintptr(s)<<3)) }

func rhStructNew(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	r := newStruct(g.rt)
	for i, v := range g.vals {
		valToCell(g.types[i], cellAt(r.p, g.rt.cells[i]), getVal(l, f, v.k, v.from))
	}
	f.inst.budget.chargeGC(f.x, structSize(g.rt))
	*f.rr(w >> 16) = r
	return pc + 1
}

func rhStructNewDefault(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	f.inst.budget.chargeGC(f.x, structSize(g.rt))
	*f.rr(w >> 16) = newStruct(g.rt)
	return pc + 1
}

func rhStructGet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	r := f.nonNullAt(g.ops[0], pc)
	putVal(l, f, g.k, uint32(uint16(w>>16)), g.read(cellAt(r.p, g.cell)))
	return pc + 1
}

func rhStructSet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	r := f.nonNullAt(g.ops[0], pc)
	valToCell(g.et, cellAt(r.p, g.cell), getVal(l, f, g.k, g.ops[1]))
	return pc + 1
}

func rhArrayNew(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	v := getVal(l, f, g.k, g.ops[0])
	a := newArray(g.rt, uint32(num(l, g.ops[1])), f.inst.budget, f.x, regAt(pc))
	for i := 0; i < a.n; i++ {
		arraySet(a, g.et, i, v)
	}
	*f.rr(w >> 16) = ArrayRef(a)
	return pc + 1
}

func rhArrayNewDefault(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	*f.rr(w >> 16) = ArrayRef(newArray(g.rt, uint32(num(l, g.ops[0])), f.inst.budget, f.x, regAt(pc)))
	return pc + 1
}

func rhArrayNewFixed(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	cnt := uint32(len(g.vals))
	a := newArray(g.rt, cnt, nil, nil, regAt(pc))
	for i, v := range g.vals {
		arraySet(a, g.et, i, getVal(l, f, v.k, v.from))
	}
	f.inst.budget.chargeGC(f.x, arraySize(g.rt, cnt))
	*f.rr(w >> 16) = ArrayRef(a)
	return pc + 1
}

func rhArrayNewSegment(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	src, cnt := uint32(num(l, g.ops[0])), uint32(num(l, g.ops[1]))
	if g.data {
		d := f.inst.datas[g.seg]
		if uint64(src)+uint64(cnt)*uint64(g.rt.elemSize) > uint64(len(d)) {
			trapAt(TrapDataSegmentOutOfBounds, regAt(pc))
		}
		a := newArray(g.rt, cnt, f.inst.budget, f.x, regAt(pc))
		copy(a.bytes(), d[src:])
		*f.rr(w >> 16) = ArrayRef(a)
		return pc + 1
	}
	e := f.inst.elems[g.seg]
	if uint64(src)+uint64(cnt) > uint64(len(e)) {
		trapAt(TrapElementSegmentOutOfBounds, regAt(pc))
	}
	a := newArray(g.rt, cnt, f.inst.budget, f.x, regAt(pc))
	copy(a.refs(), e[src:])
	*f.rr(w >> 16) = ArrayRef(a)
	return pc + 1
}

func rhArrayGet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	a := asArray(f.nonNullAt(g.ops[0], pc))
	x := uint32(num(l, g.ops[1]))
	if uint64(x) >= uint64(a.n) {
		trapAt(TrapArrayOutOfBounds, regAt(pc))
	}
	putVal(l, f, g.k, uint32(uint16(w>>16)), arrayGet(a, g.et, int(x), g.signed))
	return pc + 1
}

func rhArraySet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	a := asArray(f.nonNullAt(g.ops[0], pc))
	x := uint32(num(l, g.ops[1]))
	if uint64(x) >= uint64(a.n) {
		trapAt(TrapArrayOutOfBounds, regAt(pc))
	}
	arraySet(a, g.et, int(x), getVal(l, f, g.k, g.ops[2]))
	return pc + 1
}

func rhArrayLen(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	*rs(l, w>>16) = uint64(uint32(asArray(f.nonNullAt(g.ops[0], pc)).n))
	return pc + 1
}

func rhArrayFill(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	a := asArray(f.nonNullAt(g.ops[0], pc))
	dst, val, cnt := uint32(num(l, g.ops[1])), getVal(l, f, g.k, g.ops[2]), uint32(num(l, g.ops[3]))
	checkArray(a, dst, cnt, regAt(pc))
	for i := uint32(0); i < cnt; i++ {
		arraySet(a, g.et, int(dst+i), val)
	}
	return pc + 1
}

func rhArrayCopy(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	da := asArray(f.nonNullAt(g.ops[0], pc))
	dst := uint32(num(l, g.ops[1]))
	sa := asArray(f.nonNullAt(g.ops[2], pc))
	src, cnt := uint32(num(l, g.ops[3])), uint32(num(l, g.ops[4]))
	checkArray(da, dst, cnt, regAt(pc))
	checkArray(sa, src, cnt, regAt(pc))
	if g.et.IsRef() {
		copy(da.refs()[dst:dst+cnt], sa.refs()[src:src+cnt])
	} else {
		size := uint32(g.et.Size())
		copy(da.bytes()[dst*size:(dst+cnt)*size], sa.bytes()[src*size:(src+cnt)*size])
	}
	return pc + 1
}

func rhArrayInit(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	a := asArray(f.nonNullAt(g.ops[0], pc))
	dst, src, cnt := uint32(num(l, g.ops[1])), uint32(num(l, g.ops[2])), uint32(num(l, g.ops[3]))
	checkArray(a, dst, cnt, regAt(pc))
	if g.data {
		b := f.inst.datas[g.seg]
		size := uint64(g.et.Size())
		if uint64(src)+uint64(cnt)*size > uint64(len(b)) {
			trapAt(TrapDataSegmentOutOfBounds, regAt(pc))
		}
		copy(a.bytes()[uint64(dst)*size:], b[src:uint64(src)+uint64(cnt)*size])
		return pc + 1
	}
	e := f.inst.elems[g.seg]
	if uint64(src)+uint64(cnt) > uint64(len(e)) {
		trapAt(TrapElementSegmentOutOfBounds, regAt(pc))
	}
	copy(a.refs()[dst:dst+cnt], e[src:src+cnt])
	return pc + 1
}

func rhRefI31(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*f.rr(w >> 16) = I31(uint32(num(l, f.regRec(w).ops[0])))
	return pc + 1
}

func rhI31Get(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	r := f.nonNullAt(g.ops[0], pc)
	if g.signed {
		*rs(l, w>>16) = uint64(uint32(r.I31Signed()))
	} else {
		*rs(l, w>>16) = uint64(r.I31Unsigned())
	}
	return pc + 1
}

// test reports whether r passes the record's test or cast.
func (g *regGC) test(r Ref) bool {
	if r.IsNull() {
		return g.nullable
	}
	return RefIsSub(r, g.h, g.m)
}

func rhRefTest(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	*rs(l, w>>16) = uint64(b2i(g.test(f.r[g.ops[0]])))
	return pc + 1
}

func rhRefCast(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	g := f.regRec(w)
	if !g.test(f.r[g.ops[0]]) {
		trapAt(TrapIllegalCast, regAt(pc))
	}
	return pc + 1
}

func rhBrOnCast(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if f.rc.gc[f.rc.code[pc+1]].test(*f.rr(w >> 16)) {
		return regTake32(w, pc, f)
	}
	return pc + 2
}

func rhBrOnCastFail(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if !f.rc.gc[f.rc.code[pc+1]].test(*f.rr(w >> 16)) {
		return regTake32(w, pc, f)
	}
	return pc + 2
}

func init() {
	for x, h := range map[regOp]regHandler{
		xStructNew: rhStructNew, xStructNewDefault: rhStructNewDefault, xStructGet: rhStructGet, xStructSet: rhStructSet,
		xArrayNew: rhArrayNew, xArrayNewDefault: rhArrayNewDefault, xArrayNewFixed: rhArrayNewFixed,
		xArrayNewSegment: rhArrayNewSegment, xArrayGet: rhArrayGet, xArraySet: rhArraySet, xArrayLen: rhArrayLen,
		xArrayFill: rhArrayFill, xArrayCopy: rhArrayCopy, xArrayInit: rhArrayInit,
		xRefI31: rhRefI31, xI31Get: rhI31Get, xRefTest: rhRefTest, xRefCast: rhRefCast,
		xBrOnCast: rhBrOnCast, xBrOnCastFail: rhBrOnCastFail,
	} {
		regExtTable[x] = h
	}
	regExtSlowTable[xBrOnCast] = regSlow32
	regExtSlowTable[xBrOnCastFail] = regSlow32
}
