package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// The GC operations whose operands are the instruction's own fields
// (internal/regdef's values, its comment the fields'): a struct's field
// and an array's element by its storage kind, an array's length, ref.i31,
// i31.get, and ref.test and ref.cast with their type's record in C. The
// record forms (reg_gc.go) remain where a field or a record past 16 bits
// is named. These are the forms native code runs itself.
const (
	xSGet8S   regOp = regdef.XSGet8S
	xSGet8U   regOp = regdef.XSGet8U
	xSGet16S  regOp = regdef.XSGet16S
	xSGet16U  regOp = regdef.XSGet16U
	xSGet32   regOp = regdef.XSGet32
	xSGet64   regOp = regdef.XSGet64
	xSGet128  regOp = regdef.XSGet128
	xSGetRef  regOp = regdef.XSGetRef
	xSSet8    regOp = regdef.XSSet8
	xSSet16   regOp = regdef.XSSet16
	xSSet32   regOp = regdef.XSSet32
	xSSet64   regOp = regdef.XSSet64
	xSSet128  regOp = regdef.XSSet128
	xSSetRef  regOp = regdef.XSSetRef
	xAGet8S   regOp = regdef.XAGet8S
	xAGet8U   regOp = regdef.XAGet8U
	xAGet16S  regOp = regdef.XAGet16S
	xAGet16U  regOp = regdef.XAGet16U
	xAGet32   regOp = regdef.XAGet32
	xAGet64   regOp = regdef.XAGet64
	xAGet128  regOp = regdef.XAGet128
	xAGetRef  regOp = regdef.XAGetRef
	xASet8    regOp = regdef.XASet8
	xASet16   regOp = regdef.XASet16
	xASet32   regOp = regdef.XASet32
	xASet64   regOp = regdef.XASet64
	xASet128  regOp = regdef.XASet128
	xASetRef  regOp = regdef.XASetRef
	xALen     regOp = regdef.XALen
	xRefI31F  regOp = regdef.XRefI31
	xI31GetS  regOp = regdef.XI31GetS
	xI31GetU  regOp = regdef.XI31GetU
	xRefTestF regOp = regdef.XRefTest
	xRefCastF regOp = regdef.XRefCast
)

// gcKind is the index of a storage type's operation among a get's eight
// (i8 and i16 signed and unsigned, 32, 64 and 128 bits, a reference) and a
// set's six (i8, i16, 32, 64, 128, a reference).
func gcGetKind(t module.ValueType, signed bool) regOp {
	switch t.Kind {
	case module.KindI8:
		if signed {
			return 0
		}
		return 1
	case module.KindI16:
		if signed {
			return 2
		}
		return 3
	case module.KindI32, module.KindF32:
		return 4
	case module.KindV128:
		return 6
	case module.KindRef:
		return 7
	}
	return 5
}

func gcSetKind(t module.ValueType) regOp {
	switch t.Kind {
	case module.KindI8:
		return 0
	case module.KindI16:
		return 1
	case module.KindI32, module.KindF32:
		return 2
	case module.KindV128:
		return 4
	case module.KindRef:
		return 5
	}
	return 3
}

// fastResult emits the fast operation x of fields b and cc, its result of
// type t in position h's slot, A, and pushes it.
func (c *regCompiler) fastResult(x regOp, t module.ValueType, h int, b, cc uint64) {
	dst := c.posSlot(t, h)
	pc := c.emit(rExt, uint8(x), field(t, dst), b, cc)
	c.push(rentry{t: t, slot: dst, local: -1, def: pc})
}

// The ways native code tests a non-null reference for a heap type
// (regGC.mode), regdef's: not at all, leaving it to Go; by its type's
// supertypes, for a type index of a depth a type keeps them to (rtt.supers),
// Go walking the chain past it; as an i31, a struct, an array, an eqref
// (an i31, a struct or an array); or as any reference, for a top type.
const (
	castGo     = regdef.CastGo
	castIndex  = regdef.CastIndex
	castI31    = regdef.CastI31
	castStruct = regdef.CastStruct
	castArray  = regdef.CastArray
	castEq     = regdef.CastEq
	castTop    = regdef.CastTop
)

// castTarget sets how native code tests g's heap type h.
func (c *regCompiler) castTarget(g *regGC, h module.HeapType) {
	switch h {
	case module.HeapI31:
		g.mode = castI31
	case module.HeapStruct:
		g.mode = castStruct
	case module.HeapArray:
		g.mode = castArray
	case module.HeapEq:
		g.mode = castEq
	case module.HeapAny, module.HeapExtern, module.HeapFunc, module.HeapExn:
		g.mode = castTop
	}
	if !h.IsIndex() {
		return
	}
	rt := rttOf(c.m.Canonical[h.Index()])
	if rt.depth >= superPrefix {
		// Past the supertypes a type keeps at hand, Go walks the chain.
		g.mode = castGo
		return
	}
	g.mode, g.depth, g.canon = castIndex, rt.depth, rt.canon
}

// fastObj returns the object the reference slot field e names, trapping
// on null as the instruction at pc.
func fastObj(f *frame, e uint64, pc int32) unsafe.Pointer {
	p := f.rr(e).p
	if p == nil {
		trapAt(TrapNullDereference, regAt(pc))
	}
	return p
}

// fastCell is the cell C of the struct the reference slot B names.
func fastCell(w uint64, pc int32, f *frame, b uint64) *Ref {
	return cellAt(fastObj(f, b, pc), int(uint16(w>>48)))
}

// fastElem is the address of the element, of size bytes, of the array the
// reference slot field a names at the index in the numeric slot field i,
// trapping out of bounds.
func fastElem(l unsafe.Pointer, f *frame, a, i uint64, size uintptr, pc int32) unsafe.Pointer {
	arr := (*Array)(fastObj(f, a, pc))
	x := uint32(*rs(l, i))
	if uint64(x) >= uint64(arr.n) {
		trapAt(TrapArrayOutOfBounds, regAt(pc))
	}
	return unsafe.Add(arr.e, uintptr(x)*size)
}

func init() {
	sget := func(get func(c *Ref) uint64) regHandler {
		return func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*rs(l, w>>16) = get(fastCell(w, pc, f, w>>32))
			return pc + 1
		}
	}
	sset := func(set func(c *Ref, v uint64)) regHandler {
		return func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			set(fastCell(w, pc, f, w>>16), *rs(l, w>>32))
			return pc + 1
		}
	}
	aget := func(size uintptr, get func(p unsafe.Pointer) uint64) regHandler {
		return func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*rs(l, w>>16) = get(fastElem(l, f, w>>32, w>>48, size, pc))
			return pc + 1
		}
	}
	aset := func(size uintptr, set func(p unsafe.Pointer, v uint64)) regHandler {
		return func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			set(fastElem(l, f, w>>16, w>>32, size, pc), *rs(l, w>>48))
			return pc + 1
		}
	}
	next := func(c *Ref) *Ref { return (*Ref)(unsafe.Add(unsafe.Pointer(c), unsafe.Sizeof(Ref{}))) }
	for x, h := range map[regOp]regHandler{
		xSGet8S:  sget(func(c *Ref) uint64 { return uint64(uint32(int32(int8(c.i)))) }),
		xSGet8U:  sget(func(c *Ref) uint64 { return uint64(uint8(c.i)) }),
		xSGet16S: sget(func(c *Ref) uint64 { return uint64(uint32(int32(int16(c.i)))) }),
		xSGet16U: sget(func(c *Ref) uint64 { return uint64(uint16(c.i)) }),
		xSGet32:  sget(func(c *Ref) uint64 { return c.i }),
		xSGet64:  sget(func(c *Ref) uint64 { return c.i }),
		xSGet128: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			c := fastCell(w, pc, f, w>>32)
			setV(l, w>>16, V128{c.i, next(c).i})
			return pc + 1
		},
		xSGetRef: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*f.rr(w >> 16) = *fastCell(w, pc, f, w>>32)
			return pc + 1
		},
		xSSet8:  sset(func(c *Ref, v uint64) { c.i = uint64(uint8(v)) }),
		xSSet16: sset(func(c *Ref, v uint64) { c.i = uint64(uint16(v)) }),
		xSSet32: sset(func(c *Ref, v uint64) { c.i = uint64(uint32(v)) }),
		xSSet64: sset(func(c *Ref, v uint64) { c.i = v }),
		xSSet128: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			c := fastCell(w, pc, f, w>>16)
			v := rv(l, w>>32)
			c.i, next(c).i = v.Lo, v.Hi
			return pc + 1
		},
		xSSetRef: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*fastCell(w, pc, f, w>>16) = *f.rr(w >> 32)
			return pc + 1
		},
		xAGet8S:  aget(1, func(p unsafe.Pointer) uint64 { return uint64(uint32(int32(*(*int8)(p)))) }),
		xAGet8U:  aget(1, func(p unsafe.Pointer) uint64 { return uint64(*(*uint8)(p)) }),
		xAGet16S: aget(2, func(p unsafe.Pointer) uint64 { return uint64(uint32(int32(*(*int16)(p)))) }),
		xAGet16U: aget(2, func(p unsafe.Pointer) uint64 { return uint64(*(*uint16)(p)) }),
		xAGet32:  aget(4, func(p unsafe.Pointer) uint64 { return uint64(*(*uint32)(p)) }),
		xAGet64:  aget(8, func(p unsafe.Pointer) uint64 { return *(*uint64)(p) }),
		xAGet128: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			p := (*[2]uint64)(fastElem(l, f, w>>32, w>>48, 16, pc))
			setV(l, w>>16, V128{p[0], p[1]})
			return pc + 1
		},
		xAGetRef: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*f.rr(w >> 16) = *(*Ref)(fastElem(l, f, w>>32, w>>48, unsafe.Sizeof(Ref{}), pc))
			return pc + 1
		},
		xASet8:  aset(1, func(p unsafe.Pointer, v uint64) { *(*uint8)(p) = uint8(v) }),
		xASet16: aset(2, func(p unsafe.Pointer, v uint64) { *(*uint16)(p) = uint16(v) }),
		xASet32: aset(4, func(p unsafe.Pointer, v uint64) { *(*uint32)(p) = uint32(v) }),
		xASet64: aset(8, func(p unsafe.Pointer, v uint64) { *(*uint64)(p) = v }),
		xASet128: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			p := (*[2]uint64)(fastElem(l, f, w>>16, w>>32, 16, pc))
			v := rv(l, w>>48)
			p[0], p[1] = v.Lo, v.Hi
			return pc + 1
		},
		xASetRef: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*(*Ref)(fastElem(l, f, w>>16, w>>32, unsafe.Sizeof(Ref{}), pc)) = *f.rr(w >> 48)
			return pc + 1
		},
		xALen: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*rs(l, w>>16) = uint64(uint32((*Array)(fastObj(f, w>>32, pc)).n))
			return pc + 1
		},
		xRefI31F: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*f.rr(w >> 16) = I31(uint32(*rs(l, w>>32)))
			return pc + 1
		},
		xI31GetS: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			fastObj(f, w>>32, pc)
			*rs(l, w>>16) = uint64(uint32(f.rr(w >> 32).I31Signed()))
			return pc + 1
		},
		xI31GetU: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			fastObj(f, w>>32, pc)
			*rs(l, w>>16) = uint64(f.rr(w >> 32).I31Unsigned())
			return pc + 1
		},
		xRefTestF: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			*rs(l, w>>16) = uint64(b2i(f.rc.gc[uint16(w>>48)].test(*f.rr(w >> 32))))
			return pc + 1
		},
		xRefCastF: func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
			if !f.rc.gc[uint16(w>>48)].test(*f.rr(w >> 16)) {
				trapAt(TrapIllegalCast, regAt(pc))
			}
			return pc + 1
		},
	} {
		regExtTable[x] = h
	}
	for x, d := range map[regOp]string{
		xSGet8S: "nrc", xSGet8U: "nrc", xSGet16S: "nrc", xSGet16U: "nrc", xSGet32: "nrc", xSGet64: "nrc",
		xSGet128: "vrc", xSGetRef: "rrc",
		xSSet8: "rnc", xSSet16: "rnc", xSSet32: "rnc", xSSet64: "rnc", xSSet128: "rvc", xSSetRef: "rrc",
		xAGet8S: "nrn", xAGet8U: "nrn", xAGet16S: "nrn", xAGet16U: "nrn", xAGet32: "nrn", xAGet64: "nrn",
		xAGet128: "vrn", xAGetRef: "rrn",
		xASet8: "rnn", xASet16: "rnn", xASet32: "rnn", xASet64: "rnn", xASet128: "rnv", xASetRef: "rnr",
		xALen: "nr-", xRefI31F: "rn-", xI31GetS: "nr-", xI31GetU: "nr-", xRefTestF: "nrq", xRefCastF: "r-q",
	} {
		regExtDescs[x] = rd(d)
	}
}
