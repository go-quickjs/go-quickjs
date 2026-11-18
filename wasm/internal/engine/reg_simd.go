package engine

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's SIMD instructions: the operators without immediates are
// generated from simdgen's table (reg_simd_gen.go), the rest written here,
// all in rSIMD's table but the memory accesses, which are rExt's generic
// accesses (regEA).

// The signatures of the generated operators.
const (
	simdUnary = iota
	simdBinary
	simdTernary
	simdShift
	simdTest
)

// The hand-written operations of rSIMD's table, after the generated ones.
// Their values are internal/regdef's.
const (
	sShuffle     = regdef.SShuffle     // A = shuffle of B and C by the lanes in the two words after
	sExtractI8S  = regdef.SExtractI8S  // A = lane C of B
	sExtractI8U  = regdef.SExtractI8U  //
	sExtractI16S = regdef.SExtractI16S //
	sExtractI16U = regdef.SExtractI16U //
	sExtract32   = regdef.SExtract32   //
	sExtract64   = regdef.SExtract64   //
	sReplace8    = regdef.SReplace8    // A = B with lane (the word after) set to C
	sReplace16   = regdef.SReplace16   //
	sReplace32   = regdef.SReplace32   //
	sReplace64   = regdef.SReplace64   //
	sSplat8      = regdef.SSplat8      // A = B in every lane
	sSplat16     = regdef.SSplat16     //
	sSplat32     = regdef.SSplat32     //
	sSplat64     = regdef.SSplat64     //
)

// The SIMD memory operations of the second table (internal/regdef's
// values).
const (
	xV128Load      regOp = regdef.XV128Load      // A = the load of B's address, its opcode in the word after
	xV128Store     regOp = regdef.XV128Store     // store of B at A's address
	xV128LoadLane  regOp = regdef.XV128LoadLane  // A = C with a lane loaded from B's address
	xV128StoreLane regOp = regdef.XV128StoreLane // store of a lane of B at A's address
)

// simdOp compiles a SIMD instruction, reporting whether op is one.
func (c *regCompiler) simdOp(op opcode.Opcode) bool {
	if op.Prefix() != opcode.PrefixSIMD {
		return false
	}
	switch {
	case op == opcode.S128Const:
		lo := c.r.fixed64()
		hi := c.r.fixed64()
		c.push(rentry{t: module.V128, konst: true, c: lo, hi: hi, local: -1, def: -1})
	case op == opcode.I8x16Shuffle:
		lo := binary.LittleEndian.Uint64(c.r.b[c.r.pc:])
		hi := binary.LittleEndian.Uint64(c.r.b[c.r.pc+8:])
		c.r.pc += 16
		y := c.pop()
		x := c.pop()
		h := len(c.stack)
		xs := c.read(x, h)
		ys := c.read(y, h+1)
		c.def(module.V128, h, c.emit(rSIMD, sShuffle, nv(c.numSlot(h)), xs, ys, lo, hi))
	case op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane:
		c.lane(op, uint64(c.r.byte()))
	case op >= opcode.I8x16Splat && op <= opcode.F64x2Splat:
		x := c.pop()
		h := len(c.stack)
		xs := c.read(x, h)
		var s uint8
		switch op {
		case opcode.I8x16Splat:
			s = sSplat8
		case opcode.I16x8Splat:
			s = sSplat16
		case opcode.I32x4Splat, opcode.F32x4Splat:
			s = sSplat32
		default:
			s = sSplat64
		}
		c.def(module.V128, h, c.emit(rSIMD, s, nv(c.numSlot(h)), xs, 0))
	case op >= opcode.S128LoadMem && op <= opcode.S128Load64Splat,
		op == opcode.S128Load32Zero, op == opcode.S128Load64Zero:
		mem, off := c.r.memarg()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		c.def(module.V128, h, c.emit(rExt, xV128Load, nv(c.numSlot(h)), as, 0, uint64(mem)|uint64(op)<<32, off))
	case op == opcode.S128StoreMem:
		mem, off := c.r.memarg()
		v := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		vs := c.read(v, h+1)
		c.emit(rExt, xV128Store, as, vs, 0, uint64(mem)|uint64(op)<<32, off)
	case op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane:
		mem, off := c.r.memarg()
		lane := uint64(c.r.byte())
		v := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		vs := c.read(v, h+1)
		imm := uint64(mem) | uint64(op)<<32 | lane<<56
		if op <= opcode.S128Load64Lane {
			c.def(module.V128, h, c.emit(rExt, xV128LoadLane, nv(c.numSlot(h)), as, vs, imm, off))
		} else {
			c.emit(rExt, xV128StoreLane, as, vs, 0, imm, off)
		}
	default:
		x, kind, ok := regSIMDOp(op)
		if !ok {
			return false
		}
		c.simdGen(x, kind)
	}
	return true
}

// simdGen compiles a generated SIMD operator, x of rSIMD's table.
func (c *regCompiler) simdGen(x regOp, kind int) {
	switch kind {
	case simdUnary, simdTest:
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		t := module.V128
		if kind == simdTest {
			t = module.I32
		}
		c.def(t, h, c.emit(rSIMD, x, c.dst(t, h), as, 0))
	case simdBinary, simdShift:
		b := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		bs := c.read(b, h+1)
		c.def(module.V128, h, c.emit(rSIMD, x, nv(c.numSlot(h)), as, bs))
	case simdTernary:
		z := c.pop()
		b := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		bs := c.read(b, h+1)
		zs := c.read(z, h+2)
		c.def(module.V128, h, c.emitD(rSIMD, x, nv(c.numSlot(h)), as, bs, zs))
	}
}

// lane compiles extract_lane and replace_lane of lane n.
func (c *regCompiler) lane(op opcode.Opcode, n uint64) {
	var s uint8
	switch op {
	case opcode.I8x16ReplaceLane:
		s = sReplace8
	case opcode.I16x8ReplaceLane:
		s = sReplace16
	case opcode.I32x4ReplaceLane, opcode.F32x4ReplaceLane:
		s = sReplace32
	case opcode.I64x2ReplaceLane, opcode.F64x2ReplaceLane:
		s = sReplace64
	}
	if s != 0 {
		x := c.pop()
		v := c.pop()
		h := len(c.stack)
		vs := c.read(v, h)
		xs := c.read(x, h+1)
		c.def(module.V128, h, c.emit(rSIMD, s, nv(c.numSlot(h)), vs, xs, n))
		return
	}
	t := module.I32
	switch op {
	case opcode.I8x16ExtractLaneS:
		s = sExtractI8S
	case opcode.I8x16ExtractLaneU:
		s = sExtractI8U
	case opcode.I16x8ExtractLaneS:
		s = sExtractI16S
	case opcode.I16x8ExtractLaneU:
		s = sExtractI16U
	case opcode.I32x4ExtractLane:
		s = sExtract32
	case opcode.F32x4ExtractLane:
		s, t = sExtract32, module.F32
	case opcode.I64x2ExtractLane:
		s, t = sExtract64, module.I64
	case opcode.F64x2ExtractLane:
		s, t = sExtract64, module.F64
	}
	v := c.pop()
	h := len(c.stack)
	vs := c.read(v, h)
	c.def(t, h, c.emit(rSIMD, s, ns(c.numSlot(h)), vs, n))
}

func rhSShuffle(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	a, b := rv(l, w>>32), rv(l, w>>48)
	var ab [32]uint8
	binary.LittleEndian.PutUint64(ab[0:], a.Lo)
	binary.LittleEndian.PutUint64(ab[8:], a.Hi)
	binary.LittleEndian.PutUint64(ab[16:], b.Lo)
	binary.LittleEndian.PutUint64(ab[24:], b.Hi)
	var lanes [16]uint8
	binary.LittleEndian.PutUint64(lanes[0:], f.rc.code[pc+1])
	binary.LittleEndian.PutUint64(lanes[8:], f.rc.code[pc+2])
	var r [16]uint8
	for i, k := range lanes {
		r[i] = ab[k&31]
	}
	setV(l, w>>16, fromU8(r))
	return pc + 3
}

// regLane returns lane C, of width bits, of the v128 in field B.
func regLane(l unsafe.Pointer, w uint64, bits int) uint64 {
	hi, sh := laneAt(int(uint16(w>>48)), bits)
	mask := uint64(math.MaxUint64)
	if bits < 64 {
		mask = 1<<bits - 1
	}
	return getLane(rv(l, w>>32), hi, sh, mask)
}

func rhSExtractI8S(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(int32(int8(regLane(l, w, 8)))))
	return pc + 1
}

func rhSExtractI8U(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = regLane(l, w, 8)
	return pc + 1
}

func rhSExtractI16S(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(int32(int16(regLane(l, w, 16)))))
	return pc + 1
}

func rhSExtractI16U(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = regLane(l, w, 16)
	return pc + 1
}

func rhSExtract32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = regLane(l, w, 32)
	return pc + 1
}

func rhSExtract64(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = regLane(l, w, 64)
	return pc + 1
}

// regReplace sets lane (the word after pc), of width bits, of the v128 in
// field B to the scalar in field C, into field A.
func regReplace(l unsafe.Pointer, w uint64, pc int32, f *frame, bits int) int32 {
	hi, sh := laneAt(int(f.rc.code[pc+1]), bits)
	mask := uint64(math.MaxUint64)
	if bits < 64 {
		mask = 1<<bits - 1
	}
	setV(l, w>>16, setLane(rv(l, w>>32), hi, sh, mask, *rs(l, w>>48)))
	return pc + 2
}

func rhSReplace8(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regReplace(l, w, pc, f, 8)
}

func rhSReplace16(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regReplace(l, w, pc, f, 16)
}

func rhSReplace32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regReplace(l, w, pc, f, 32)
}

func rhSReplace64(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regReplace(l, w, pc, f, 64)
}

func rhSSplat8(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := uint64(uint8(*rs(l, w>>32))) * 0x0101010101010101
	setV(l, w>>16, V128{x, x})
	return pc + 1
}

func rhSSplat16(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := uint64(uint16(*rs(l, w>>32))) * 0x0001000100010001
	setV(l, w>>16, V128{x, x})
	return pc + 1
}

func rhSSplat32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := uint64(uint32(*rs(l, w>>32))) * 0x0000000100000001
	setV(l, w>>16, V128{x, x})
	return pc + 1
}

func rhSSplat64(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := *rs(l, w>>32)
	setV(l, w>>16, V128{x, x})
	return pc + 1
}

// vecLoad loads the v128 vector load op reads from memory m at ea, trapping
// as the instruction at offset at: a whole vector, its lanes widened, a
// lane splatted, or a lane with zeros above it.
func vecLoad(op opcode.Opcode, m *Memory, ea uint64, at uint32) V128 {
	switch op {
	case opcode.S128LoadMem:
		b := m.span(ea, 16, at)
		return V128{binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint64(b[8:])}
	case opcode.S128Load8x8S, opcode.S128Load8x8U:
		b := m.span(ea, 8, at)
		var r [8]uint16
		for i := range r {
			if op == opcode.S128Load8x8S {
				r[i] = uint16(int8(b[i]))
			} else {
				r[i] = uint16(b[i])
			}
		}
		return fromU16(r)
	case opcode.S128Load16x4S, opcode.S128Load16x4U:
		b := m.span(ea, 8, at)
		var r [4]uint32
		for i := range r {
			x := binary.LittleEndian.Uint16(b[2*i:])
			if op == opcode.S128Load16x4S {
				r[i] = uint32(int16(x))
			} else {
				r[i] = uint32(x)
			}
		}
		return fromU32(r)
	case opcode.S128Load32x2S:
		b := m.span(ea, 8, at)
		return V128{uint64(int32(binary.LittleEndian.Uint32(b))), uint64(int32(binary.LittleEndian.Uint32(b[4:])))}
	case opcode.S128Load32x2U:
		b := m.span(ea, 8, at)
		return V128{uint64(binary.LittleEndian.Uint32(b)), uint64(binary.LittleEndian.Uint32(b[4:]))}
	case opcode.S128Load8Splat:
		x := uint64(m.span(ea, 1, at)[0]) * 0x0101010101010101
		return V128{x, x}
	case opcode.S128Load16Splat:
		x := uint64(binary.LittleEndian.Uint16(m.span(ea, 2, at))) * 0x0001000100010001
		return V128{x, x}
	case opcode.S128Load32Splat:
		x := uint64(binary.LittleEndian.Uint32(m.span(ea, 4, at))) * 0x0000000100000001
		return V128{x, x}
	case opcode.S128Load64Splat:
		x := binary.LittleEndian.Uint64(m.span(ea, 8, at))
		return V128{x, x}
	case opcode.S128Load32Zero:
		return V128{uint64(binary.LittleEndian.Uint32(m.span(ea, 4, at))), 0}
	case opcode.S128Load64Zero:
		return V128{binary.LittleEndian.Uint64(m.span(ea, 8, at)), 0}
	}
	panic("wasm: not a vector load")
}

func rhV128Load(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	m, ea := regEA(l, w>>32, pc, f)
	setV(l, w>>16, vecLoad(opcode.Opcode(f.rc.code[pc+1]>>32), m, ea, regAt(pc)))
	return pc + 3
}

func rhV128Store(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	m, ea := regEA(l, w>>16, pc, f)
	x := rv(l, w>>32)
	b := m.span(ea, 16, regAt(pc))
	binary.LittleEndian.PutUint64(b, x.Lo)
	binary.LittleEndian.PutUint64(b[8:], x.Hi)
	return pc + 3
}

// laneBytes returns the width in bytes of a lane load or store's lane, and
// its place in a v128 of the lane the word imm names.
func laneBytes(imm uint64) (n int, hi bool, sh uint, mask uint64) {
	switch opcode.Opcode(uint32(imm >> 32 & 0xffffff)) {
	case opcode.S128Load8Lane, opcode.S128Store8Lane:
		n = 1
	case opcode.S128Load16Lane, opcode.S128Store16Lane:
		n = 2
	case opcode.S128Load32Lane, opcode.S128Store32Lane:
		n = 4
	default:
		n = 8
	}
	hi, sh = laneAt(int(imm>>56), 8*n)
	mask = math.MaxUint64
	if n < 8 {
		mask = 1<<(8*n) - 1
	}
	return n, hi, sh, mask
}

func rhV128LoadLane(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	n, hi, sh, mask := laneBytes(f.rc.code[pc+1])
	m, ea := regEA(l, w>>32, pc, f)
	x := rv(l, w>>48)
	setV(l, w>>16, setLane(x, hi, sh, mask, readLE(m.span(ea, uint64(n), regAt(pc)))))
	return pc + 3
}

func rhV128StoreLane(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	n, hi, sh, mask := laneBytes(f.rc.code[pc+1])
	m, ea := regEA(l, w>>16, pc, f)
	x := rv(l, w>>32)
	writeLE(m.span(ea, uint64(n), regAt(pc)), getLane(x, hi, sh, mask))
	return pc + 3
}

func init() {
	for s, h := range map[regOp]regHandler{
		sShuffle: rhSShuffle, sExtractI8S: rhSExtractI8S, sExtractI8U: rhSExtractI8U,
		sExtractI16S: rhSExtractI16S, sExtractI16U: rhSExtractI16U, sExtract32: rhSExtract32, sExtract64: rhSExtract64,
		sReplace8: rhSReplace8, sReplace16: rhSReplace16, sReplace32: rhSReplace32, sReplace64: rhSReplace64,
		sSplat8: rhSSplat8, sSplat16: rhSSplat16, sSplat32: rhSSplat32, sSplat64: rhSSplat64,
	} {
		regSIMDTable[s] = h
	}
	for x, h := range map[regOp]regHandler{
		xV128Load: rhV128Load, xV128Store: rhV128Store, xV128LoadLane: rhV128LoadLane, xV128StoreLane: rhV128StoreLane,
	} {
		regExtTable[x] = h
	}
}
