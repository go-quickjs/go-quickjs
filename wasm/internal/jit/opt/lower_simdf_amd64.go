package opt

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The float lane instructions, the conversions and the relaxed
// instructions on amd64: the baseline's sequences (simd_float_amd64.go,
// simd_relaxed_amd64.go).

// simdFloat lowers a float lane instruction or a conversion, reporting
// whether op is one.
func (g *gen) simdFloat(v *Value, op opcode.Opcode, x, y *Value, t func(int) amd64.XReg) bool {
	a := g.a
	switch op {
	case opcode.F32x4Add:
		g.vbin(v, amd64.Addps, x, y)
	case opcode.F32x4Sub:
		g.vbin(v, amd64.Subps, x, y)
	case opcode.F32x4Mul:
		g.vbin(v, amd64.Mulps, x, y)
	case opcode.F32x4Div:
		g.vbin(v, amd64.Divps, x, y)
	case opcode.F64x2Add:
		g.vbin(v, amd64.Addpd, x, y)
	case opcode.F64x2Sub:
		g.vbin(v, amd64.Subpd, x, y)
	case opcode.F64x2Mul:
		g.vbin(v, amd64.Mulpd, x, y)
	case opcode.F64x2Div:
		g.vbin(v, amd64.Divpd, x, y)
	case opcode.F32x4Sqrt:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Sqrtps, c, c) })
	case opcode.F64x2Sqrt:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Sqrtpd, c, c) })
	case opcode.F32x4Abs:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pand, c, t(0), splat32(0x7fffffff)) })
	case opcode.F64x2Abs:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pand, c, t(0), 1<<63-1) })
	case opcode.F32x4Neg:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pxor, c, t(0), splat32(0x80000000)) })
	case opcode.F64x2Neg:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pxor, c, t(0), 1<<63) })
	case opcode.F32x4Ceil, opcode.F32x4Floor, opcode.F32x4Trunc, opcode.F32x4NearestInt:
		mode := map[opcode.Opcode]uint8{opcode.F32x4Ceil: amd64.RoundCeil, opcode.F32x4Floor: amd64.RoundFloor,
			opcode.F32x4Trunc: amd64.RoundTrunc, opcode.F32x4NearestInt: amd64.RoundNearest}[op]
		g.vun(v, func(c amd64.XReg) { a.VecImm(amd64.Roundps, c, c, mode|8) })
	case opcode.F64x2Ceil, opcode.F64x2Floor, opcode.F64x2Trunc, opcode.F64x2NearestInt:
		mode := map[opcode.Opcode]uint8{opcode.F64x2Ceil: amd64.RoundCeil, opcode.F64x2Floor: amd64.RoundFloor,
			opcode.F64x2Trunc: amd64.RoundTrunc, opcode.F64x2NearestInt: amd64.RoundNearest}[op]
		g.vun(v, func(c amd64.XReg) { a.VecImm(amd64.Roundpd, c, c, mode|8) })
	case opcode.F32x4Eq:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpEQ, x, y)
	case opcode.F32x4Ne:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpNEQ, x, y)
	case opcode.F32x4Lt:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpLT, x, y)
	case opcode.F32x4Le:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpLE, x, y)
	case opcode.F32x4Gt:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpLT, y, x)
	case opcode.F32x4Ge:
		g.vcmpf(v, amd64.Cmpps, amd64.CmpLE, y, x)
	case opcode.F64x2Eq:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpEQ, x, y)
	case opcode.F64x2Ne:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpNEQ, x, y)
	case opcode.F64x2Lt:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpLT, x, y)
	case opcode.F64x2Le:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpLE, x, y)
	case opcode.F64x2Gt:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpLT, y, x)
	case opcode.F64x2Ge:
		g.vcmpf(v, amd64.Cmppd, amd64.CmpLE, y, x)
	case opcode.F32x4Min:
		g.vminmax(v, amd64.Minps, amd64.Addps, amd64.Cmpps, false, t)
	case opcode.F32x4Max:
		g.vminmax(v, amd64.Maxps, amd64.Addps, amd64.Cmpps, true, t)
	case opcode.F64x2Min:
		g.vminmax(v, amd64.Minpd, amd64.Addpd, amd64.Cmppd, false, t)
	case opcode.F64x2Max:
		g.vminmax(v, amd64.Maxpd, amd64.Addpd, amd64.Cmppd, true, t)
	case opcode.F32x4Pmin:
		// y < x ? y : x is MINPS of y and x; pmax MAXPS.
		g.vbin(v, amd64.Minps, y, x)
	case opcode.F32x4Pmax:
		g.vbin(v, amd64.Maxps, y, x)
	case opcode.F64x2Pmin:
		g.vbin(v, amd64.Minpd, y, x)
	case opcode.F64x2Pmax:
		g.vbin(v, amd64.Maxpd, y, x)

	case opcode.F32x4DemoteF64x2Zero:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvtpd2ps, c, c) })
	case opcode.F64x2PromoteLowF32x4:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvtps2pd, c, c) })
	case opcode.F32x4SConvertI32x4:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvtdq2ps, c, c) })
	case opcode.F32x4UConvertI32x4:
		// The high 16 bits converted and scaled, exactly, plus the low 16
		// converted: one rounding, of the sum.
		g.vun(v, func(c amd64.XReg) {
			a.SSEOp(amd64.MovAPD, t(0), c)
			a.VecShift(amd64.PsrldImm, t(0), 16)
			a.VecShift(amd64.PslldImm, c, 16)
			a.VecShift(amd64.PsrldImm, c, 16)
			a.Vec(amd64.Cvtdq2ps, t(0), t(0))
			g.vopConst(amd64.Mulps, t(0), t(1), f32s(65536))
			a.Vec(amd64.Cvtdq2ps, c, c)
			a.Vec(amd64.Addps, c, t(0))
		})
	case opcode.F64x2ConvertLowI32x4S:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvtdq2pd, c, c) })
	case opcode.F64x2ConvertLowI32x4U:
		// Each lane the low word of the double 2**52 + u, less 2**52.
		g.vun(v, func(c amd64.XReg) {
			g.vopConst(amd64.Punpckldq, c, t(0), splat32(0x43300000))
			g.vopConst(amd64.Subpd, c, t(0), f64s(1<<52))
		})
	case opcode.I32x4SConvertF32x4:
		g.vun(v, func(c amd64.XReg) { g.truncSatF32S(c, t(0), t(1)) })
	case opcode.I32x4UConvertF32x4:
		g.vun(v, func(c amd64.XReg) { g.truncSatF32U(c, t(0), t(1), t(2)) })
	case opcode.I32x4TruncSatF64x2SZero:
		// NaN made 0 and the lanes clamped above; CVTTPD2DQ gives
		// 0x80000000 below, and clears the upper half.
		g.vun(v, func(c amd64.XReg) {
			a.SSEOp(amd64.MovAPD, t(0), c)
			a.VecImm(amd64.Cmppd, t(0), t(0), amd64.CmpEQ)
			a.Vec(amd64.Andps, c, t(0))
			g.vopConst(amd64.Minpd, c, t(0), f64s(2147483647))
			a.Vec(amd64.Cvttpd2dq, c, c)
		})
	case opcode.I32x4TruncSatF64x2UZero:
		g.vun(v, func(c amd64.XReg) { g.truncSatF64U(c, t(0), t(1)) })
	default:
		return false
	}
	return true
}

// vcmpf lowers a float comparison: CMPPS or CMPPD with predicate p, of x
// and y.
func (g *gen) vcmpf(v *Value, op amd64.VOp, p uint8, x, y *Value) {
	c, rs := g.vprep(v, x, y)
	g.a.VecImm(op, c, rs[0], p)
	g.vend(v, c)
}

// vminmax lowers min and max: a NaN in either lane gives the lanes' sum,
// x's lanes the destination, the NaN the interpreter picks; equal lanes
// the OR (min) or AND (max) of their bits, so that -0 is below +0; others
// the smaller or larger. MINPS and MAXPS give their second operand for a
// NaN or equal lanes: both orders ORed or ANDed give the zeros' answer,
// and the sum replaces the unordered lanes.
func (g *gen) vminmax(v *Value, minmax, add, cmp amd64.VOp, isMax bool, t func(int) amd64.XReg) {
	a := g.a
	c, rs := g.vprep(v, v.Args[0], v.Args[1])
	yb, tt, n, xt := rs[0], t(0), t(1), t(2)
	a.SSEOp(amd64.MovAPD, tt, c)
	a.Vec(minmax, tt, yb)
	a.SSEOp(amd64.MovAPD, n, yb)
	a.Vec(minmax, n, c)
	if isMax {
		a.Vec(amd64.Andps, tt, n)
	} else {
		a.Vec(amd64.Orps, tt, n)
	}
	// n = the sum, xt = the unordered lanes.
	a.SSEOp(amd64.MovAPD, n, c)
	a.Vec(add, n, yb)
	a.SSEOp(amd64.MovAPD, xt, c)
	a.VecImm(cmp, xt, yb, amd64.CmpUnord)
	a.Vec(amd64.Andps, n, xt)
	a.Vec(amd64.Andnps, xt, tt)
	a.Vec(amd64.Orps, xt, n)
	a.SSEOp(amd64.MovAPD, c, xt)
	g.vend(v, c)
}

// truncSatF64U converts x's doubles to unsigned 32-bit integers in its
// lower half, saturating, the upper half cleared: clamped to [0, 2**32-1],
// NaN to 0, truncated, and each lane the low word of 2**52 plus it. z and
// k are its temporaries.
func (g *gen) truncSatF64U(x, z, k amd64.XReg) {
	a := g.a
	a.Vec(amd64.Pxor, z, z)
	a.Vec(amd64.Maxpd, x, z)
	g.vopConst(amd64.Minpd, x, k, f64s(4294967295))
	a.VecImm(amd64.Roundpd, x, x, amd64.RoundTrunc|8)
	g.vopConst(amd64.Addpd, x, k, f64s(1<<52))
	a.VecImm(amd64.Shufps, x, z, 0x88)
}

// truncSatF32S converts x's floats to signed 32-bit integers, saturating:
// NaN made 0 first, and CVTTPS2DQ's 0x80000000 for a lane at or past 2**31
// complemented. t and k are its temporaries.
func (g *gen) truncSatF32S(x, t, k amd64.XReg) {
	a := g.a
	a.SSEOp(amd64.MovAPD, k, x)
	a.VecImm(amd64.Cmpps, k, k, amd64.CmpEQ)
	a.Vec(amd64.Andps, x, k)
	g.loadVConst(t, f32s(2147483648), f32s(2147483648))
	a.VecImm(amd64.Cmpps, t, x, amd64.CmpLE)
	a.Vec(amd64.Cvttps2dq, x, x)
	a.Vec(amd64.Pxor, x, t)
}

// truncSatF32U converts x's floats to unsigned 32-bit integers,
// saturating: NaN and the negative made 0; a lane below 2**31 converted
// as it is, one at or past it less 2**31 with the top bit set after, and
// one at or past 2**32 all ones. t, m and k are its temporaries.
func (g *gen) truncSatF32U(x, t, m, k amd64.XReg) {
	a := g.a
	a.Vec(amd64.Pxor, k, k)
	a.Vec(amd64.Maxps, x, k)
	g.loadVConst(m, f32s(2147483648), f32s(2147483648))
	a.SSEOp(amd64.MovAPD, t, x)
	a.Vec(amd64.Subps, t, m)
	a.VecImm(amd64.Cmpps, m, x, amd64.CmpLE)
	a.Vec(amd64.Cvttps2dq, x, x)
	a.Vec(amd64.Cvttps2dq, t, t)
	a.Vec(amd64.Pand, t, m)
	a.Vec(amd64.Por, x, t)
	a.VecShift(amd64.PsradImm, t, 31)
	a.Vec(amd64.Por, x, t)
}

// simdRelaxed lowers a relaxed SIMD instruction with the interpreter's
// results on this CPU (engine/relaxed_x86.go), reporting whether op is
// one. relaxed_madd and relaxed_nmadd fused are Go's (simdExits).
func (g *gen) simdRelaxed(v *Value, op opcode.Opcode, x, y *Value, t func(int) amd64.XReg) bool {
	a := g.a
	switch op {
	case opcode.I8x16RelaxedSwizzle:
		// PSHUFB: 0 for an index with its top bit set, else the byte its
		// low four bits number.
		g.vbin(v, amd64.Pshufb, x, y)
	case opcode.I32x4RelaxedTruncF32x4S:
		// NaN and the lanes out of range give 0x80000000.
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvttps2dq, c, c) })
	case opcode.I32x4RelaxedTruncF32x4U:
		g.vun(v, func(c amd64.XReg) { g.truncSatF32U(c, t(0), t(1), t(2)) })
	case opcode.I32x4RelaxedTruncF64x2SZero:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Cvttpd2dq, c, c) })
	case opcode.I32x4RelaxedTruncF64x2UZero:
		g.vun(v, func(c amd64.XReg) { g.truncSatF64U(c, t(0), t(1)) })
	case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
		// Unfused: the product rounded, then added to the addend, or the
		// addend less it: the NaN of the interpreter's p + c or c - p,
		// each instruction's first operand its destination.
		mul, add, sub := amd64.Mulps, amd64.Addps, amd64.Subps
		if op == opcode.F64x2Qfma || op == opcode.F64x2Qfms {
			mul, add, sub = amd64.Mulpd, amd64.Addpd, amd64.Subpd
		}
		c, rs := g.vprep(v, x, y, v.Args[2])
		a.Vec(mul, c, rs[0])
		if op == opcode.F32x4Qfma || op == opcode.F64x2Qfma {
			a.Vec(add, c, rs[1])
		} else {
			a.SSEOp(amd64.MovAPD, t(1), rs[1])
			a.Vec(sub, t(1), c)
			a.SSEOp(amd64.MovAPD, c, t(1))
		}
		g.vend(v, c)
	case opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect, opcode.I32x4RelaxedLaneSelect,
		opcode.I64x2RelaxedLaneSelect:
		// The top bit of each byte (PBLENDVB), 32-bit lane (BLENDVPS) or
		// 64-bit lane (BLENDVPD) of the mask spread through it, then a
		// bitselect.
		c, rs := g.vprep(v, x, y, v.Args[2])
		yb, mc, tt := rs[0], rs[1], t(1)
		switch op {
		case opcode.I32x4RelaxedLaneSelect:
			a.SSEOp(amd64.MovAPD, tt, mc)
			a.VecShift(amd64.PsradImm, tt, 31)
		case opcode.I64x2RelaxedLaneSelect:
			a.VecImm(amd64.Pshufd, tt, mc, 0xF5)
			a.VecShift(amd64.PsradImm, tt, 31)
		default:
			a.Vec(amd64.Pxor, tt, tt)
			a.Vec(amd64.Pcmpgtb, tt, mc)
		}
		a.Vec(amd64.Pxor, c, yb)
		a.Vec(amd64.Pand, c, tt)
		a.Vec(amd64.Pxor, c, yb)
		g.vend(v, c)
	case opcode.F32x4RelaxedMin:
		// x < y ? x : y, the second operand for a NaN or equal lanes.
		g.vbin(v, amd64.Minps, x, y)
	case opcode.F32x4RelaxedMax:
		g.vbin(v, amd64.Maxps, x, y)
	case opcode.F64x2RelaxedMin:
		g.vbin(v, amd64.Minpd, x, y)
	case opcode.F64x2RelaxedMax:
		g.vbin(v, amd64.Maxpd, x, y)
	case opcode.I16x8RelaxedQ15MulRS:
		// PMULHRSW, which does not saturate.
		g.vbin(v, amd64.Pmulhrsw, x, y)
	case opcode.I16x8DotI8x16I7x16S:
		// PMADDUBSW takes its destination's bytes unsigned: y's.
		g.vbin(v, amd64.Pmaddubsw, y, x)
	case opcode.I32x4DotI8x16I7x16AddS:
		g.relaxedDotAdd(v, t)
	default:
		return false
	}
	return true
}

// relaxedDotAdd lowers relaxed_dot_i8x16_i7x16_add_s: with AVX-VNNI the
// four products of each 32-bit lane's bytes, x's signed and y's unsigned
// (signed too with AVX-VNNI-INT8), summed in 32 bits with the addend,
// which PMADDWD makes of the even and the odd bytes widened to 16 bits;
// else the sums of pairs of products saturated to 16 bits (PMADDUBSW),
// summed in pairs (PMADDWD of ones) and added.
func (g *gen) relaxedDotAdd(v *Value, t func(int) amd64.XReg) {
	a := g.a
	x, y, z := v.Args[0], v.Args[1], v.Args[2]
	if !wjit.CPU.RelaxedDotSigned && !wjit.CPU.RelaxedDotVNNI {
		c, rs := g.vprep(v, y, x, z)
		a.Vec(amd64.Pmaddubsw, c, rs[0])
		g.vopConst(amd64.Pmaddwd, c, t(1), splat16(1))
		a.Vec(amd64.Paddd, c, rs[1])
		g.vend(v, c)
		return
	}
	c, rs := g.vprep(v, x, y, z)
	yb, xo, yo, xt := t(1), t(2), t(3), t(4)
	a.SSEOp(amd64.MovAPD, yb, rs[0])
	// x's odd bytes and even bytes, sign-extended to 16 bits.
	a.SSEOp(amd64.MovAPD, xo, c)
	a.VecShift(amd64.PsrawImm, xo, 8)
	a.VecShift(amd64.PsllwImm, c, 8)
	a.VecShift(amd64.PsrawImm, c, 8)
	// y's likewise, zero-extended unless signed.
	a.SSEOp(amd64.MovAPD, yo, yb)
	if wjit.CPU.RelaxedDotSigned {
		a.VecShift(amd64.PsrawImm, yo, 8)
		a.VecShift(amd64.PsllwImm, yb, 8)
		a.VecShift(amd64.PsrawImm, yb, 8)
	} else {
		a.VecShift(amd64.PsrlwImm, yo, 8)
		g.vopConst(amd64.Pand, yb, xt, splat16(0xff))
	}
	a.Vec(amd64.Pmaddwd, c, yb)
	a.Vec(amd64.Pmaddwd, xo, yo)
	a.Vec(amd64.Paddd, c, xo)
	a.Vec(amd64.Paddd, c, rs[1])
	g.vend(v, c)
}
