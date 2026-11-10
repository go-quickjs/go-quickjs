package jit

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The float lane instructions and the conversions on amd64.

// f32s and f64s are a float32's and a float64's bits in every lane.
func f32s(f float32) uint64 { return splat32(math.Float32bits(f)) }
func f64s(f float64) uint64 { return math.Float64bits(f) }

// vcmpf compiles a float comparison: CMPPS or CMPPD with predicate p, of
// x and y, or of y and x if rev.
func (c *compiler) vcmpf(op amd64.VOp, p uint8, rev bool) {
	y := c.pop()
	x := c.pop()
	if rev {
		x, y = y, x
	}
	xa := c.intoX(x)
	yb := c.readX(y)
	c.a.VecImm(op, xa, yb, p)
	c.free(y)
	c.pushReg(v128, uint8(xa))
}

// vminmax compiles min and max: a NaN in either lane gives the lanes' sum,
// x's lanes the destination, the NaN the interpreter picks; equal lanes the OR (min) or AND (max) of their
// bits, so that -0 is below +0; others the smaller or larger. MINPS and
// MAXPS give their second operand for a NaN or equal lanes: both orders
// ORed or ANDed give the zeros' answer, and the sum replaces the unordered
// lanes.
func (c *compiler) vminmax(minmax, add, cmp amd64.VOp, isMax bool) {
	a := c.a
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	yb := c.intoX(y)
	t := c.vtemp()
	n := c.vtemp()
	a.SSEOp(amd64.MovAPD, t, xa)
	a.Vec(minmax, t, yb)
	a.SSEOp(amd64.MovAPD, n, yb)
	a.Vec(minmax, n, xa)
	if isMax {
		a.Vec(amd64.Andps, t, n)
	} else {
		a.Vec(amd64.Orps, t, n)
	}
	// n = the sum, xTmp = the unordered lanes.
	a.SSEOp(amd64.MovAPD, n, xa)
	a.Vec(add, n, yb)
	a.SSEOp(amd64.MovAPD, xTmp, xa)
	a.VecImm(cmp, xTmp, yb, amd64.CmpUnord)
	a.Vec(amd64.Andps, n, xTmp)
	a.Vec(amd64.Andnps, xTmp, t)
	a.Vec(amd64.Orps, xTmp, n)
	a.SSEOp(amd64.MovAPD, xa, xTmp)
	c.freeX(yb)
	c.freeX(t)
	c.freeX(n)
	c.pushReg(v128, uint8(xa))
}

// simdFloat compiles a float lane instruction or a conversion natively,
// reporting whether op is one it compiles.
func (c *compiler) simdFloat(op opcode.Opcode) bool {
	a := c.a
	switch op {
	case opcode.F32x4Add:
		c.vbin(amd64.Addps)
	case opcode.F32x4Sub:
		c.vbin(amd64.Subps)
	case opcode.F32x4Mul:
		c.vbin(amd64.Mulps)
	case opcode.F32x4Div:
		c.vbin(amd64.Divps)
	case opcode.F64x2Add:
		c.vbin(amd64.Addpd)
	case opcode.F64x2Sub:
		c.vbin(amd64.Subpd)
	case opcode.F64x2Mul:
		c.vbin(amd64.Mulpd)
	case opcode.F64x2Div:
		c.vbin(amd64.Divpd)
	case opcode.F32x4Sqrt:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Sqrtps, x, x) })
	case opcode.F64x2Sqrt:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Sqrtpd, x, x) })
	case opcode.F32x4Abs:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pand, x, splat32(0x7fffffff)) })
	case opcode.F64x2Abs:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pand, x, 1<<63-1) })
	case opcode.F32x4Neg:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pxor, x, splat32(0x80000000)) })
	case opcode.F64x2Neg:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pxor, x, 1<<63) })
	case opcode.F32x4Ceil, opcode.F32x4Floor, opcode.F32x4Trunc, opcode.F32x4NearestInt:
		mode := map[opcode.Opcode]uint8{opcode.F32x4Ceil: amd64.RoundCeil, opcode.F32x4Floor: amd64.RoundFloor,
			opcode.F32x4Trunc: amd64.RoundTrunc, opcode.F32x4NearestInt: amd64.RoundNearest}[op]
		c.vunary(v128, func(x amd64.XReg) { a.VecImm(amd64.Roundps, x, x, mode|8) })
	case opcode.F64x2Ceil, opcode.F64x2Floor, opcode.F64x2Trunc, opcode.F64x2NearestInt:
		mode := map[opcode.Opcode]uint8{opcode.F64x2Ceil: amd64.RoundCeil, opcode.F64x2Floor: amd64.RoundFloor,
			opcode.F64x2Trunc: amd64.RoundTrunc, opcode.F64x2NearestInt: amd64.RoundNearest}[op]
		c.vunary(v128, func(x amd64.XReg) { a.VecImm(amd64.Roundpd, x, x, mode|8) })
	case opcode.F32x4Eq:
		c.vcmpf(amd64.Cmpps, amd64.CmpEQ, false)
	case opcode.F32x4Ne:
		c.vcmpf(amd64.Cmpps, amd64.CmpNEQ, false)
	case opcode.F32x4Lt:
		c.vcmpf(amd64.Cmpps, amd64.CmpLT, false)
	case opcode.F32x4Le:
		c.vcmpf(amd64.Cmpps, amd64.CmpLE, false)
	case opcode.F32x4Gt:
		c.vcmpf(amd64.Cmpps, amd64.CmpLT, true)
	case opcode.F32x4Ge:
		c.vcmpf(amd64.Cmpps, amd64.CmpLE, true)
	case opcode.F64x2Eq:
		c.vcmpf(amd64.Cmppd, amd64.CmpEQ, false)
	case opcode.F64x2Ne:
		c.vcmpf(amd64.Cmppd, amd64.CmpNEQ, false)
	case opcode.F64x2Lt:
		c.vcmpf(amd64.Cmppd, amd64.CmpLT, false)
	case opcode.F64x2Le:
		c.vcmpf(amd64.Cmppd, amd64.CmpLE, false)
	case opcode.F64x2Gt:
		c.vcmpf(amd64.Cmppd, amd64.CmpLT, true)
	case opcode.F64x2Ge:
		c.vcmpf(amd64.Cmppd, amd64.CmpLE, true)
	case opcode.F32x4Min:
		c.vminmax(amd64.Minps, amd64.Addps, amd64.Cmpps, false)
	case opcode.F32x4Max:
		c.vminmax(amd64.Maxps, amd64.Addps, amd64.Cmpps, true)
	case opcode.F64x2Min:
		c.vminmax(amd64.Minpd, amd64.Addpd, amd64.Cmppd, false)
	case opcode.F64x2Max:
		c.vminmax(amd64.Maxpd, amd64.Addpd, amd64.Cmppd, true)
	case opcode.F32x4Pmin:
		// y < x ? y : x is MINPS of y and x; pmax MAXPS.
		c.vbinRev(amd64.Minps)
	case opcode.F32x4Pmax:
		c.vbinRev(amd64.Maxps)
	case opcode.F64x2Pmin:
		c.vbinRev(amd64.Minpd)
	case opcode.F64x2Pmax:
		c.vbinRev(amd64.Maxpd)

	case opcode.F32x4DemoteF64x2Zero:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvtpd2ps, x, x) })
	case opcode.F64x2PromoteLowF32x4:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvtps2pd, x, x) })
	case opcode.F32x4SConvertI32x4:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvtdq2ps, x, x) })
	case opcode.F32x4UConvertI32x4:
		// The high 16 bits converted and scaled, exactly, plus the low 16
		// converted: one rounding, of the sum.
		c.vunary(v128, func(x amd64.XReg) {
			t := c.vtemp()
			a.SSEOp(amd64.MovAPD, t, x)
			a.VecShift(amd64.PsrldImm, t, 16)
			a.VecShift(amd64.PslldImm, x, 16)
			a.VecShift(amd64.PsrldImm, x, 16)
			a.Vec(amd64.Cvtdq2ps, t, t)
			c.vopConst(amd64.Mulps, t, f32s(65536))
			a.Vec(amd64.Cvtdq2ps, x, x)
			a.Vec(amd64.Addps, x, t)
			c.freeX(t)
		})
	case opcode.F64x2ConvertLowI32x4S:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Cvtdq2pd, x, x) })
	case opcode.F64x2ConvertLowI32x4U:
		// Each lane the low word of the double 2**52 + u, less 2**52.
		c.vunary(v128, func(x amd64.XReg) {
			c.vopConst(amd64.Punpckldq, x, splat32(0x43300000))
			c.vopConst(amd64.Subpd, x, f64s(1<<52))
		})
	case opcode.I32x4SConvertF32x4:
		c.vunary(v128, c.truncSatF32S)
	case opcode.I32x4UConvertF32x4:
		c.vunary(v128, c.truncSatF32U)
	case opcode.I32x4TruncSatF64x2SZero:
		// NaN made 0 and the lanes clamped above; CVTTPD2DQ gives
		// 0x80000000 below, and clears the upper half.
		c.vunary(v128, func(x amd64.XReg) {
			a.SSEOp(amd64.MovAPD, xTmp, x)
			a.VecImm(amd64.Cmppd, xTmp, xTmp, amd64.CmpEQ)
			a.Vec(amd64.Andps, x, xTmp)
			c.vopConst(amd64.Minpd, x, f64s(2147483647))
			a.Vec(amd64.Cvttpd2dq, x, x)
		})
	case opcode.I32x4TruncSatF64x2UZero:
		c.vunary(v128, c.truncSatF64U)
	default:
		return c.simdRelaxed(op)
	}
	return true
}

// truncSatF64U converts x's doubles to unsigned 32-bit integers in its
// lower half, saturating, the upper half cleared: clamped to [0, 2**32-1],
// NaN to 0, truncated, and each lane the low word of 2**52 plus it.
func (c *compiler) truncSatF64U(x amd64.XReg) {
	a := c.a
	z := c.vtemp()
	a.Vec(amd64.Pxor, z, z)
	a.Vec(amd64.Maxpd, x, z)
	c.vopConst(amd64.Minpd, x, f64s(4294967295))
	a.VecImm(amd64.Roundpd, x, x, amd64.RoundTrunc|8)
	c.vopConst(amd64.Addpd, x, f64s(1<<52))
	a.VecImm(amd64.Shufps, x, z, 0x88)
	c.freeX(z)
}

// truncSatF32S converts x's floats to signed 32-bit integers, saturating:
// NaN made 0 first, and CVTTPS2DQ's 0x80000000 for a lane at or past 2**31
// complemented.
func (c *compiler) truncSatF32S(x amd64.XReg) {
	a := c.a
	t := c.vtemp()
	a.SSEOp(amd64.MovAPD, xTmp, x)
	a.VecImm(amd64.Cmpps, xTmp, xTmp, amd64.CmpEQ)
	a.Vec(amd64.Andps, x, xTmp)
	c.loadVConst(t, f32s(2147483648), f32s(2147483648))
	a.VecImm(amd64.Cmpps, t, x, amd64.CmpLE)
	a.Vec(amd64.Cvttps2dq, x, x)
	a.Vec(amd64.Pxor, x, t)
	c.freeX(t)
}

// truncSatF32U converts x's floats to unsigned 32-bit integers,
// saturating: NaN and the negative made 0; a lane below 2**31 converted
// as it is, one at or past it less 2**31 with the top bit set after, and
// one at or past 2**32 all ones.
func (c *compiler) truncSatF32U(x amd64.XReg) {
	a := c.a
	t, m := c.vtemp(), c.vtemp()
	a.Vec(amd64.Pxor, xTmp, xTmp)
	a.Vec(amd64.Maxps, x, xTmp)
	c.loadVConst(m, f32s(2147483648), f32s(2147483648))
	a.SSEOp(amd64.MovAPD, t, x)
	a.Vec(amd64.Subps, t, m)
	a.VecImm(amd64.Cmpps, m, x, amd64.CmpLE)
	a.Vec(amd64.Cvttps2dq, x, x)
	a.Vec(amd64.Cvttps2dq, t, t)
	a.Vec(amd64.Pand, t, m)
	a.Vec(amd64.Por, x, t)
	a.VecShift(amd64.PsradImm, t, 31)
	a.Vec(amd64.Por, x, t)
	c.freeX(t)
	c.freeX(m)
}
