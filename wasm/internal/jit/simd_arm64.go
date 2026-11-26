package jit

import (
	"encoding/binary"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions on arm64: ASIMD, a v128 in a V register of the
// cache or of a local's home, or in the two slots of its position.

// storeVec stores v128 v into the 16 bytes at [base + off].
func (c *compiler) storeVec(v value, base arm64.Reg, off int32) {
	a := c.a
	switch v.k {
	case kConst:
		for i, w := range [2]uint64{v.c, v.hi} {
			r := arm64.ZR
			if w != 0 {
				a.MovImm(rTmp, w)
				r = rTmp
			}
			a.Store(base, off+int32(8*i), r)
		}
	case kReg:
		a.StoreQ(base, off, v.xmm())
	case kSlot:
		if base == rL && off == v.off {
			return
		}
		a.LoadQ(xTmp, rL, v.off)
		a.StoreQ(base, off, xTmp)
	default:
		panic("wasm jit: flags stored")
	}
}

// loadVec loads v128 v into x.
func (c *compiler) loadVec(x arm64.FReg, v value) {
	a := c.a
	switch v.k {
	case kConst:
		c.loadVConst(x, v.c, v.hi)
	case kSlot:
		a.LoadQ(x, rL, v.off)
	case kReg:
		if x != v.xmm() {
			a.VMov(x, v.xmm())
		}
	default:
		panic("wasm jit: flags loaded")
	}
}

// loadVConst loads the v128 lo, hi into x: zeros made, any other from the
// code's pool, in reach of the load however long the code is
// (LoadQTable).
func (c *compiler) loadVConst(x arm64.FReg, lo, hi uint64) {
	if lo == 0 && hi == 0 {
		c.a.V3(arm64.VEor, arm64.V8, x, x, x)
		return
	}
	c.a.LoadQTable(x, c.vconst(lo, hi))
}

// newVConst makes the label of the constant v128 lo, hi: a table of the
// assembler's, which it places in a pool near its loads, or after the
// code (emitVConsts).
func (c *compiler) newVConst(lo, hi uint64) arm64.Label { return c.a.Table(v128Bytes(lo, hi)) }

// v128Bytes is the 16 bytes of the v128 lo, hi.
func v128Bytes(lo, hi uint64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b, lo)
	binary.LittleEndian.PutUint64(b[8:], hi)
	return b
}

// emitVConsts places the v128 constants no pool has placed, after the
// code, in the order they were first used.
func (c *compiler) emitVConsts() { c.a.Tables() }

// vtemp returns a free V register for an instruction's own use, to give
// back with freeX.
func (c *compiler) vtemp() arm64.FReg { return c.allocX(0) }

// vbin compiles a binary operation of one instruction on lanes of size s:
// x = x op y.
func (c *compiler) vbin(op arm64.VOp, s arm64.VSize) {
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	yb := c.readX(y)
	c.a.V3(op, s, xa, xa, yb)
	c.free(y)
	c.pushReg(v128, uint8(xa))
}

// vbinRev is vbin with the operands the other way round: y op x.
func (c *compiler) vbinRev(op arm64.VOp, s arm64.VSize) {
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	yb := c.readX(y)
	c.a.V3(op, s, xa, yb, xa)
	c.free(y)
	c.pushReg(v128, uint8(xa))
}

// vunary applies f to the top value, in a register of its own, its result
// of type t.
func (c *compiler) vunary(t module.Kind, f func(x arm64.FReg)) {
	v := c.pop()
	x := c.intoX(v)
	f(x)
	c.pushReg(t, uint8(x))
}

// v2 applies a one-instruction operation on lanes of size s to the top
// v128.
func (c *compiler) v2(op arm64.VOp, s arm64.VSize) {
	c.vunary(v128, func(x arm64.FReg) { c.a.V2(op, s, x, x) })
}

// laneSize is the VSize of lanes of n bytes.
func laneSize(n uint8) arm64.VSize {
	switch n {
	case 1:
		return arm64.V8
	case 2:
		return arm64.V16
	case 4:
		return arm64.V32
	}
	return arm64.V64
}

// simdNative compiles SIMD instruction op natively, reporting whether it
// did; it emits nothing when it does not.
func (c *compiler) simdNative(op opcode.Opcode, at uint32) bool {
	a := c.a
	switch op {
	case opcode.S128LoadMem, opcode.S128Load8x8S, opcode.S128Load8x8U, opcode.S128Load16x4S,
		opcode.S128Load16x4U, opcode.S128Load32x2S, opcode.S128Load32x2U, opcode.S128Load8Splat,
		opcode.S128Load16Splat, opcode.S128Load32Splat, opcode.S128Load64Splat, opcode.S128Load32Zero,
		opcode.S128Load64Zero:
		c.vload(op, at)
	case opcode.S128StoreMem:
		mem, off := c.r.memarg()
		v := c.pop()
		addr := c.pop()
		// The address first: its check takes the scratch registers.
		base, idx := c.address(addr, mem, off, 16, at, false)
		x := c.readX(v)
		a.StoreQIdx(base, idx, x)
		c.free(v)
		c.freeG(idx)
		c.freeBase(base)
	case opcode.S128Load8Lane, opcode.S128Load16Lane, opcode.S128Load32Lane, opcode.S128Load64Lane:
		mem, off := c.r.memarg()
		lane := c.r.byte()
		size := memLaneBytes(op)
		v := c.pop()
		addr := c.pop()
		x := c.intoX(v)
		base, idx := c.address(addr, mem, off, uint64(size), at, false)
		c.loadInt(idx, base, idx, size)
		a.InsG(laneSize(size), x, lane, idx)
		c.freeG(idx)
		c.freeBase(base)
		c.pushReg(v128, uint8(x))
	case opcode.S128Store8Lane, opcode.S128Store16Lane, opcode.S128Store32Lane, opcode.S128Store64Lane:
		mem, off := c.r.memarg()
		lane := c.r.byte()
		size := memLaneBytes(op)
		v := c.pop()
		addr := c.pop()
		base, idx := c.address(addr, mem, off, uint64(size), at, false)
		x := c.readX(v)
		a.Umov(laneSize(size), rTmp, x, lane)
		switch size {
		case 1:
			a.Store8Idx(base, idx, rTmp)
		case 2:
			a.Store16Idx(base, idx, rTmp)
		case 4:
			a.Store32Idx(base, idx, rTmp)
		default:
			a.StoreIdx(base, idx, rTmp)
		}
		c.free(v)
		c.freeG(idx)
		c.freeBase(base)

	case opcode.I8x16Splat, opcode.I16x8Splat, opcode.I32x4Splat, opcode.I64x2Splat:
		v := c.pop()
		r := c.readG(v, rTmp)
		x := c.vtemp()
		a.Dup(splatSize(op), x, r)
		c.free(v)
		c.pushReg(v128, uint8(x))
	case opcode.F32x4Splat:
		c.vunary(v128, func(x arm64.FReg) { a.DupElem(arm64.V32, x, x, 0) })
	case opcode.F64x2Splat:
		c.vunary(v128, func(x arm64.FReg) { a.DupElem(arm64.V64, x, x, 0) })

	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU,
		opcode.I32x4ExtractLane, opcode.I64x2ExtractLane:
		lane := c.r.byte()
		s := laneSize(laneBytes(op))
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		switch op {
		case opcode.I8x16ExtractLaneS, opcode.I16x8ExtractLaneS:
			a.Smov(s, r, x, lane)
		default:
			a.Umov(s, r, x, lane)
		}
		c.free(v)
		c.pushReg(laneKind(op), uint8(r))
	case opcode.F32x4ExtractLane, opcode.F64x2ExtractLane:
		// The float alone in the register, the rest of it clear.
		lane := c.r.byte()
		s := laneSize(laneBytes(op))
		c.vunary(laneKind(op), func(x arm64.FReg) { a.DupScalar(s, x, x, lane) })
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane, opcode.I64x2ReplaceLane:
		lane := c.r.byte()
		sv := c.pop()
		v := c.pop()
		x := c.intoX(v)
		r := c.readG(sv, rTmp)
		a.InsG(laneSize(laneBytes(op)), x, lane, r)
		c.free(sv)
		c.pushReg(v128, uint8(x))
	case opcode.F32x4ReplaceLane, opcode.F64x2ReplaceLane:
		lane := c.r.byte()
		sv := c.pop()
		v := c.pop()
		x := c.intoX(v)
		y := c.readX(sv)
		a.InsElem(laneSize(laneBytes(op)), x, lane, y, 0)
		c.free(sv)
		c.pushReg(v128, uint8(x))

	case opcode.S128Not:
		c.v2(arm64.VNot, arm64.V8)
	case opcode.S128And:
		c.vbin(arm64.VAnd, arm64.V8)
	case opcode.S128Or:
		c.vbin(arm64.VOrr, arm64.V8)
	case opcode.S128Xor:
		c.vbin(arm64.VEor, arm64.V8)
	case opcode.S128AndNot:
		c.vbin(arm64.VBic, arm64.V8)
	case opcode.S128Select:
		// BSL keeps x's bits where the mask's are set, y's elsewhere.
		m := c.pop()
		y := c.pop()
		x := c.pop()
		mc := c.intoX(m)
		xa := c.readXIn(x, xTmp)
		yb := c.readXIn(y, xTmp2)
		a.V3(arm64.VBsl, arm64.V8, mc, xa, yb)
		c.free(x)
		c.free(y)
		c.pushReg(v128, uint8(mc))
	case opcode.V128AnyTrue:
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		a.V2(arm64.VUmaxv, arm64.V8, xTmp, x)
		a.Umov(arm64.V8, r, xTmp, 0)
		c.free(v)
		a.CmpImm(r, 0, false)
		a.Cset(r, arm64.NE)
		c.pushReg(i32, uint8(r))
	default:
		return c.simdInt(op)
	}
	return true
}

// splatSize is the lane size of a splat.
func splatSize(op opcode.Opcode) arm64.VSize {
	switch op {
	case opcode.I8x16Splat, opcode.S128Load8Splat:
		return arm64.V8
	case opcode.I16x8Splat, opcode.S128Load16Splat:
		return arm64.V16
	case opcode.I32x4Splat, opcode.S128Load32Splat:
		return arm64.V32
	}
	return arm64.V64
}

// loadInt loads the size bytes (1, 2, 4 or 8) at [base + idx] into r,
// zero-extended.
func (c *compiler) loadInt(r, base, idx arm64.Reg, size uint8) {
	switch size {
	case 1:
		c.a.LoadU8Idx(r, base, idx)
	case 2:
		c.a.LoadU16Idx(r, base, idx)
	case 4:
		c.a.LoadU32Idx(r, base, idx)
	default:
		c.a.LoadIdx(r, base, idx, false)
	}
}

// vload compiles the loads of a whole v128, of half of one widened, of a
// splatted lane and of a lane with zeros above it.
func (c *compiler) vload(op opcode.Opcode, at uint32) {
	a := c.a
	mem, off := c.r.memarg()
	var size uint8
	switch op {
	case opcode.S128LoadMem:
		size = 16
	case opcode.S128Load8Splat:
		size = 1
	case opcode.S128Load16Splat:
		size = 2
	case opcode.S128Load32Splat, opcode.S128Load32Zero:
		size = 4
	default:
		size = 8
	}
	base, idx := c.address(c.pop(), mem, off, uint64(size), at, false)
	x := c.vtemp()
	switch op {
	case opcode.S128LoadMem:
		a.LoadQIdx(x, base, idx)
	case opcode.S128Load8Splat, opcode.S128Load16Splat, opcode.S128Load32Splat, opcode.S128Load64Splat:
		c.loadInt(idx, base, idx, size)
		a.Dup(splatSize(op), x, idx)
	case opcode.S128Load32Zero:
		// A load of the S view clears the rest of the register.
		a.LoadSIdx(x, base, idx)
	default:
		a.LoadFIdx(x, base, idx)
		switch op {
		case opcode.S128Load8x8S:
			a.VShiftImm(arm64.VSshll, arm64.V8, x, x, 0)
		case opcode.S128Load8x8U:
			a.VShiftImm(arm64.VUshll, arm64.V8, x, x, 0)
		case opcode.S128Load16x4S:
			a.VShiftImm(arm64.VSshll, arm64.V16, x, x, 0)
		case opcode.S128Load16x4U:
			a.VShiftImm(arm64.VUshll, arm64.V16, x, x, 0)
		case opcode.S128Load32x2S:
			a.VShiftImm(arm64.VSshll, arm64.V32, x, x, 0)
		case opcode.S128Load32x2U:
			a.VShiftImm(arm64.VUshll, arm64.V32, x, x, 0)
		}
	}
	c.freeG(idx)
	c.freeBase(base)
	c.pushReg(v128, uint8(x))
}

// loadCells loads into x the v128 a struct keeps in two cells: its low
// half at [base + off], its high half at [base + off + 16].
func (c *compiler) loadCells(x arm64.FReg, base arm64.Reg, off int32) {
	c.a.LoadF(x, base, off)
	c.a.Load(rTmp, base, off+16)
	c.a.InsG(arm64.V64, x, 1, rTmp)
}

// storeCells stores x as a struct keeps a v128 in two cells: its low half
// at [base + off], its high half at [base + off + 16].
func (c *compiler) storeCells(base arm64.Reg, off int32, x arm64.FReg) {
	c.a.StoreF(base, off, x)
	c.a.Umov(arm64.V64, rTmp, x, 1)
	c.a.Store(base, off+16, rTmp)
}

// loadVecAt and storeVecAt load x from the 16 bytes at [base + off], and
// store it there.
func (c *compiler) loadVecAt(x arm64.FReg, base arm64.Reg, off int32)  { c.a.LoadQ(x, base, off) }
func (c *compiler) storeVecAt(base arm64.Reg, off int32, x arm64.FReg) { c.a.StoreQ(base, off, x) }
