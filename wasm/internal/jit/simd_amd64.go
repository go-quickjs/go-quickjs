package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions on amd64: SSE2, SSSE3, SSE4.1 and SSE4.2 (the
// "cpu" refusal without them), a v128 in an XMM register of the cache or
// of a local's home, or in the two slots of its position.

// storeVec stores v128 v into the 16 bytes at [base + off].
func (c *compiler) storeVec(v value, base amd64.Reg, off int32) {
	a := c.a
	switch v.k {
	case kConst:
		for i, w := range [2]uint64{v.c, v.hi} {
			if int64(w) == int64(int32(w)) {
				a.StoreImm(base, off+int32(8*i), int32(w))
				continue
			}
			a.MovImm(rTmp, w)
			a.Store(base, off+int32(8*i), rTmp)
		}
	case kReg:
		a.StoreVec(base, off, v.xmm())
	case kSlot:
		if base == rL && off == v.off {
			return
		}
		a.LoadVec(xTmp, rL, v.off)
		a.StoreVec(base, off, xTmp)
	default:
		panic("wasm jit: flags stored")
	}
}

// loadVec loads v128 v into x.
func (c *compiler) loadVec(x amd64.XReg, v value) {
	a := c.a
	switch v.k {
	case kConst:
		c.loadVConst(x, v.c, v.hi)
	case kSlot:
		a.LoadVec(x, rL, v.off)
	case kReg:
		if x != v.xmm() {
			a.SSEOp(amd64.MovAPD, x, v.xmm())
		}
	default:
		panic("wasm jit: flags loaded")
	}
}

// loadVConst loads the v128 lo, hi into x: zeros and ones made, any other
// from the code's pool.
func (c *compiler) loadVConst(x amd64.XReg, lo, hi uint64) {
	switch {
	case lo == 0 && hi == 0:
		c.a.Vec(amd64.Pxor, x, x)
	case lo == ^uint64(0) && hi == ^uint64(0):
		c.a.Vec(amd64.Pcmpeqd, x, x)
	default:
		c.a.LoadVecLabel(x, c.vconst(lo, hi))
	}
}

// emitVConsts emits the pool of v128 constants, after the code.
func (c *compiler) emitVConsts() {
	for _, k := range c.vconstOrder {
		c.a.Bind(c.vconsts[k])
		c.a.Quad(k[0])
		c.a.Quad(k[1])
	}
}

// vtemp returns a free XMM register for an instruction's own use, to give
// back with freeX.
func (c *compiler) vtemp() amd64.XReg { return c.allocX(0) }

// vbin compiles a binary operation of one instruction: x op= y.
func (c *compiler) vbin(op amd64.VOp) {
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	yb := c.readX(y)
	c.a.Vec(op, xa, yb)
	c.free(y)
	c.pushReg(v128, uint8(xa))
}

// vbinRev compiles a binary operation of one instruction with its operands
// the other way round: y op= x, the result y.
func (c *compiler) vbinRev(op amd64.VOp) {
	y := c.pop()
	x := c.pop()
	yb := c.intoX(y)
	xa := c.readX(x)
	c.a.Vec(op, yb, xa)
	c.free(x)
	c.pushReg(v128, uint8(yb))
}

// vunary applies f to the top v128, in a register of its own.
func (c *compiler) vunary(t module.Kind, f func(x amd64.XReg)) {
	v := c.pop()
	x := c.intoX(v)
	f(x)
	c.pushReg(t, uint8(x))
}

// vnot complements x, with the scratch register.
func (c *compiler) vnot(x amd64.XReg) {
	c.a.Vec(amd64.Pcmpeqd, xTmp, xTmp)
	c.a.Vec(amd64.Pxor, x, xTmp)
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
		x := c.intoX(v)
		base, idx, disp := c.address(addr, mem, off, 16, at, 0, false)
		a.StoreVecIdx(base, idx, disp, x)
		c.freeX(x)
		c.freeG(idx)
		c.freeBase(base)
	case opcode.S128Load8Lane, opcode.S128Load16Lane, opcode.S128Load32Lane, opcode.S128Load64Lane:
		mem, off := c.r.memarg()
		lane := c.r.byte()
		size := memLaneBytes(op)
		v := c.pop()
		addr := c.pop()
		x := c.intoX(v)
		base, idx, disp := c.address(addr, mem, off, uint64(size), at, 0, false)
		c.loadInt(idx, base, idx, disp, size)
		a.Pinsr(size, x, idx, lane)
		c.freeG(idx)
		c.freeBase(base)
		c.pushReg(v128, uint8(x))
	case opcode.S128Store8Lane, opcode.S128Store16Lane, opcode.S128Store32Lane, opcode.S128Store64Lane:
		mem, off := c.r.memarg()
		lane := c.r.byte()
		size := memLaneBytes(op)
		v := c.pop()
		addr := c.pop()
		x := c.intoX(v)
		r := c.allocG(0)
		a.Pextr(size, r, x, lane)
		c.freeX(x)
		base, idx, disp := c.address(addr, mem, off, uint64(size), at, 1<<r, false)
		switch size {
		case 1:
			a.Store8Idx(base, idx, disp, r)
		case 2:
			a.Store16Idx(base, idx, disp, r)
		case 4:
			a.Store32Idx(base, idx, disp, r)
		default:
			a.StoreIdx(base, idx, 1, disp, r)
		}
		c.freeG(r)
		c.freeG(idx)
		c.freeBase(base)

	case opcode.I8x16Splat, opcode.I16x8Splat, opcode.I32x4Splat, opcode.I64x2Splat:
		v := c.pop()
		r := c.intoG(v, 0)
		x := c.vtemp()
		c.splatG(op, x, r)
		c.freeG(r)
		c.pushReg(v128, uint8(x))
	case opcode.F32x4Splat:
		c.vunary(v128, func(x amd64.XReg) { a.VecImm(amd64.Pshufd, x, x, 0) })
	case opcode.F64x2Splat:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Punpcklqdq, x, x) })

	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU,
		opcode.I32x4ExtractLane, opcode.I64x2ExtractLane:
		lane := c.r.byte()
		size := laneBytes(op)
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		a.Pextr(size, r, x, lane)
		switch op {
		case opcode.I8x16ExtractLaneS:
			a.Movsx8(r, r, false)
		case opcode.I16x8ExtractLaneS:
			a.Movsx16(r, r, false)
		}
		c.free(v)
		c.pushReg(laneKind(op), uint8(r))
	case opcode.F32x4ExtractLane:
		lane := c.r.byte()
		c.vunary(f32, func(x amd64.XReg) {
			if lane != 0 {
				a.VecImm(amd64.Pshufd, x, x, lane)
			}
		})
	case opcode.F64x2ExtractLane:
		lane := c.r.byte()
		c.vunary(f64, func(x amd64.XReg) {
			if lane != 0 {
				a.Vec(amd64.Movhlps, x, x)
			}
		})
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane, opcode.I64x2ReplaceLane:
		lane := c.r.byte()
		s := c.pop()
		v := c.pop()
		x := c.intoX(v)
		r := c.intoG(s, 0)
		a.Pinsr(laneBytes(op), x, r, lane)
		c.freeG(r)
		c.pushReg(v128, uint8(x))
	case opcode.F32x4ReplaceLane:
		lane := c.r.byte()
		s := c.pop()
		v := c.pop()
		x := c.intoX(v)
		y := c.readX(s)
		a.VecImm(amd64.Insertps, x, y, lane<<4)
		c.free(s)
		c.pushReg(v128, uint8(x))
	case opcode.F64x2ReplaceLane:
		lane := c.r.byte()
		s := c.pop()
		v := c.pop()
		x := c.intoX(v)
		y := c.readX(s)
		if lane == 0 {
			a.Vec(amd64.Movsd, x, y)
		} else {
			a.Vec(amd64.Movlhps, x, y)
		}
		c.free(s)
		c.pushReg(v128, uint8(x))

	case opcode.S128Not:
		c.vunary(v128, c.vnot)
	case opcode.S128And:
		c.vbin(amd64.Pand)
	case opcode.S128Or:
		c.vbin(amd64.Por)
	case opcode.S128Xor:
		c.vbin(amd64.Pxor)
	case opcode.S128AndNot:
		c.vbinRev(amd64.Pandn)
	case opcode.S128Select:
		// x&m | y&^m is ((x^y)&m)^y.
		m := c.pop()
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.intoX(y)
		mc := c.readX(m)
		a.Vec(amd64.Pxor, xa, yb)
		a.Vec(amd64.Pand, xa, mc)
		a.Vec(amd64.Pxor, xa, yb)
		c.freeX(yb)
		c.free(m)
		c.pushReg(v128, uint8(xa))
	case opcode.V128AnyTrue:
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		a.Op(amd64.Xor, r, r, false)
		a.Vec(amd64.Ptest, x, x)
		a.Setcc(amd64.CondNE, r)
		c.free(v)
		c.pushReg(i32, uint8(r))
	default:
		return c.simdInt(op)
	}
	return true
}

// splatG fills every lane of x with the low lane-sized bits of r.
func (c *compiler) splatG(op opcode.Opcode, x amd64.XReg, r amd64.Reg) {
	a := c.a
	switch op {
	case opcode.I8x16Splat:
		a.MovDToX(x, r)
		a.Vec(amd64.Pxor, xTmp, xTmp)
		a.Vec(amd64.Pshufb, x, xTmp)
	case opcode.I16x8Splat:
		a.MovDToX(x, r)
		a.VecImm(amd64.Pshuflw, x, x, 0)
		a.Vec(amd64.Punpcklqdq, x, x)
	case opcode.I32x4Splat:
		a.MovDToX(x, r)
		a.VecImm(amd64.Pshufd, x, x, 0)
	default:
		a.MovQToX(x, r)
		a.Vec(amd64.Punpcklqdq, x, x)
	}
}

// loadInt loads the size bytes (1, 2, 4 or 8) at [base + idx + disp] into
// r, zero-extended.
func (c *compiler) loadInt(r, base, idx amd64.Reg, disp int32, size uint8) {
	switch size {
	case 1:
		c.a.LoadU8Idx(r, base, idx, disp)
	case 2:
		c.a.LoadU16Idx(r, base, idx, disp)
	case 4:
		c.a.LoadU32Idx(r, base, idx, disp)
	default:
		c.a.LoadIdx(r, base, idx, 1, disp)
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
	base, idx, disp := c.address(c.pop(), mem, off, uint64(size), at, 0, false)
	x := c.vtemp()
	if size == 16 {
		a.LoadVecIdx(x, base, idx, disp)
	} else {
		c.loadInt(idx, base, idx, disp, size)
		switch op {
		case opcode.S128Load8Splat:
			c.splatG(opcode.I8x16Splat, x, idx)
		case opcode.S128Load16Splat:
			c.splatG(opcode.I16x8Splat, x, idx)
		case opcode.S128Load32Splat:
			c.splatG(opcode.I32x4Splat, x, idx)
		case opcode.S128Load64Splat:
			c.splatG(opcode.I64x2Splat, x, idx)
		case opcode.S128Load32Zero:
			a.MovDToX(x, idx)
		default:
			a.MovQToX(x, idx)
			switch op {
			case opcode.S128Load8x8S:
				a.Vec(amd64.Pmovsxbw, x, x)
			case opcode.S128Load8x8U:
				a.Vec(amd64.Pmovzxbw, x, x)
			case opcode.S128Load16x4S:
				a.Vec(amd64.Pmovsxwd, x, x)
			case opcode.S128Load16x4U:
				a.Vec(amd64.Pmovzxwd, x, x)
			case opcode.S128Load32x2S:
				a.Vec(amd64.Pmovsxdq, x, x)
			case opcode.S128Load32x2U:
				a.Vec(amd64.Pmovzxdq, x, x)
			}
		}
	}
	c.freeG(idx)
	c.freeBase(base)
	c.pushReg(v128, uint8(x))
}
