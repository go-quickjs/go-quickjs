package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The relaxed SIMD instructions on amd64, with the interpreter's results on
// this CPU (engine/relaxed_x86.go): those of the SSE instructions that
// compute them, and of the AVX ones it has (CPU).

// simdRelaxed compiles a relaxed SIMD instruction natively, reporting
// whether it did. relaxed_madd and relaxed_nmadd on a CPU with FMA3 are
// fused, which the code does with no VEX encoding, and left to Go.
func (c *compiler) simdRelaxed(op opcode.Opcode) bool {
	a := c.a
	switch op {
	case opcode.I8x16RelaxedSwizzle:
		// PSHUFB: 0 for an index with its top bit set, else the byte its
		// low four bits number.
		c.vbin(amd64.Pshufb)
	case opcode.I32x4RelaxedTruncF32x4S:
		// NaN and the lanes out of range give 0x80000000.
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvttps2dq, x, x) })
	case opcode.I32x4RelaxedTruncF32x4U:
		c.vunary(v128, c.truncSatF32U)
	case opcode.I32x4RelaxedTruncF64x2SZero:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvttpd2dq, x, x) })
	case opcode.I32x4RelaxedTruncF64x2UZero:
		c.vunary(v128, c.truncSatF64U)
	case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
		if CPU.RelaxedFused {
			return false
		}
		// The product rounded, then added to the addend, or the addend
		// less it: the NaN of the interpreter's p + c or c - p, each
		// instruction's first operand its destination.
		mul, add, sub := amd64.Mulps, amd64.Addps, amd64.Subps
		if op == opcode.F64x2Qfma || op == opcode.F64x2Qfms {
			mul, add, sub = amd64.Mulpd, amd64.Addpd, amd64.Subpd
		}
		z := c.pop()
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		a.Vec(mul, xa, yb)
		c.free(y)
		zc := c.intoX(z)
		if op == opcode.F32x4Qfma || op == opcode.F64x2Qfma {
			a.Vec(add, xa, zc)
			c.freeX(zc)
			c.pushReg(v128, uint8(xa))
		} else {
			a.Vec(sub, zc, xa)
			c.freeX(xa)
			c.pushReg(v128, uint8(zc))
		}
	case opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect, opcode.I32x4RelaxedLaneSelect,
		opcode.I64x2RelaxedLaneSelect:
		// The top bit of each byte (PBLENDVB), 32-bit lane (BLENDVPS) or
		// 64-bit lane (BLENDVPD) of the mask spread through it, then a
		// bitselect.
		m := c.pop()
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.intoX(y)
		mc := c.readX(m)
		t := c.vtemp()
		switch op {
		case opcode.I32x4RelaxedLaneSelect:
			a.SSEOp(amd64.MovAPD, t, mc)
			a.VecShift(amd64.PsradImm, t, 31)
		case opcode.I64x2RelaxedLaneSelect:
			a.VecImm(amd64.Pshufd, t, mc, 0xF5)
			a.VecShift(amd64.PsradImm, t, 31)
		default:
			a.Vec(amd64.Pxor, t, t)
			a.Vec(amd64.Pcmpgtb, t, mc)
		}
		a.Vec(amd64.Pxor, xa, yb)
		a.Vec(amd64.Pand, xa, t)
		a.Vec(amd64.Pxor, xa, yb)
		c.freeX(t)
		c.freeX(yb)
		c.free(m)
		c.pushReg(v128, uint8(xa))
	case opcode.F32x4RelaxedMin:
		// x < y ? x : y, the second operand for a NaN or equal lanes.
		c.vbin(amd64.Minps)
	case opcode.F32x4RelaxedMax:
		c.vbin(amd64.Maxps)
	case opcode.F64x2RelaxedMin:
		c.vbin(amd64.Minpd)
	case opcode.F64x2RelaxedMax:
		c.vbin(amd64.Maxpd)
	case opcode.I16x8RelaxedQ15MulRS:
		// PMULHRSW, which does not saturate.
		c.vbin(amd64.Pmulhrsw)
	case opcode.I16x8DotI8x16I7x16S:
		// PMADDUBSW takes its destination's bytes unsigned: y's.
		c.vbinRev(amd64.Pmaddubsw)
	case opcode.I32x4DotI8x16I7x16AddS:
		c.relaxedDotAdd()
	default:
		return false
	}
	return true
}

// relaxedDotAdd compiles relaxed_dot_i8x16_i7x16_add_s: with AVX-VNNI the
// four products of each 32-bit lane's bytes, x's signed and y's unsigned
// (signed too with AVX-VNNI-INT8), summed in 32 bits with the addend,
// which PMADDWD makes of the even and the odd bytes widened to 16 bits;
// else the sums of pairs of products saturated to 16 bits (PMADDUBSW),
// summed in pairs (PMADDWD of ones) and added.
func (c *compiler) relaxedDotAdd() {
	a := c.a
	z := c.pop()
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	yb := c.intoX(y)
	if !CPU.RelaxedDotSigned && !CPU.RelaxedDotVNNI {
		a.Vec(amd64.Pmaddubsw, yb, xa)
		c.vopConst(amd64.Pmaddwd, yb, splat16(1))
		zc := c.readX(z)
		a.Vec(amd64.Paddd, yb, zc)
		c.free(z)
		c.freeX(xa)
		c.pushReg(v128, uint8(yb))
		return
	}
	xo, yo := c.vtemp(), c.vtemp()
	// x's odd bytes and even bytes, sign-extended to 16 bits.
	a.SSEOp(amd64.MovAPD, xo, xa)
	a.VecShift(amd64.PsrawImm, xo, 8)
	a.VecShift(amd64.PsllwImm, xa, 8)
	a.VecShift(amd64.PsrawImm, xa, 8)
	// y's likewise, zero-extended unless signed.
	a.SSEOp(amd64.MovAPD, yo, yb)
	if CPU.RelaxedDotSigned {
		a.VecShift(amd64.PsrawImm, yo, 8)
		a.VecShift(amd64.PsllwImm, yb, 8)
		a.VecShift(amd64.PsrawImm, yb, 8)
	} else {
		a.VecShift(amd64.PsrlwImm, yo, 8)
		c.vopConst(amd64.Pand, yb, splat16(0xff))
	}
	a.Vec(amd64.Pmaddwd, xa, yb)
	a.Vec(amd64.Pmaddwd, xo, yo)
	a.Vec(amd64.Paddd, xa, xo)
	c.freeX(xo)
	c.freeX(yo)
	c.freeX(yb)
	zc := c.readX(z)
	a.Vec(amd64.Paddd, xa, zc)
	c.free(z)
	c.pushReg(v128, uint8(xa))
}
