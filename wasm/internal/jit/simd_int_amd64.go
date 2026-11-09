package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The integer lane instructions on amd64.

// vbinNot is vbin with the result complemented: a comparison that SSE has
// only the negation of.
func (c *compiler) vbinNot(op amd64.VOp) {
	c.vbin(op)
	c.vnot(c.stack[len(c.stack)-1].xmm())
}

// vbinRevNot is vbinRev with the result complemented.
func (c *compiler) vbinRevNot(op amd64.VOp) {
	c.vbinRev(op)
	c.vnot(c.stack[len(c.stack)-1].xmm())
}

// vcmpU compiles an unsigned comparison: x >= y where max(x, y) == x
// (minmax the maximum), x <= y where min(x, y) == x, complemented if not.
func (c *compiler) vcmpU(minmax, eq amd64.VOp, not bool) {
	a := c.a
	y := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	t := c.vtemp()
	a.SSEOp(amd64.MovAPD, t, xa)
	yb := c.readX(y)
	a.Vec(minmax, t, yb)
	a.Vec(eq, t, xa)
	c.freeX(xa)
	c.free(y)
	if not {
		c.vnot(t)
	}
	c.pushReg(v128, uint8(t))
}

// vconstInto loads the v128 lo, hi into a temporary, which it returns.
func (c *compiler) vconstInto(lo, hi uint64) amd64.XReg {
	t := c.vtemp()
	c.loadVConst(t, lo, hi)
	return t
}

// vop1 applies op with the constant splat w (both halves) as its source to
// the top v128.
func (c *compiler) vopConst(op amd64.VOp, x amd64.XReg, w uint64) {
	c.loadVConst(xTmp, w, w)
	c.a.Vec(op, x, xTmp)
}

// simdInt compiles an integer lane instruction natively, reporting whether
// op is one it compiles.
func (c *compiler) simdInt(op opcode.Opcode) bool {
	a := c.a
	switch op {
	case opcode.I8x16Add:
		c.vbin(amd64.Paddb)
	case opcode.I16x8Add:
		c.vbin(amd64.Paddw)
	case opcode.I32x4Add:
		c.vbin(amd64.Paddd)
	case opcode.I64x2Add:
		c.vbin(amd64.Paddq)
	case opcode.I8x16Sub:
		c.vbin(amd64.Psubb)
	case opcode.I16x8Sub:
		c.vbin(amd64.Psubw)
	case opcode.I32x4Sub:
		c.vbin(amd64.Psubd)
	case opcode.I64x2Sub:
		c.vbin(amd64.Psubq)
	case opcode.I8x16AddSatS:
		c.vbin(amd64.Paddsb)
	case opcode.I8x16AddSatU:
		c.vbin(amd64.Paddusb)
	case opcode.I8x16SubSatS:
		c.vbin(amd64.Psubsb)
	case opcode.I8x16SubSatU:
		c.vbin(amd64.Psubusb)
	case opcode.I16x8AddSatS:
		c.vbin(amd64.Paddsw)
	case opcode.I16x8AddSatU:
		c.vbin(amd64.Paddusw)
	case opcode.I16x8SubSatS:
		c.vbin(amd64.Psubsw)
	case opcode.I16x8SubSatU:
		c.vbin(amd64.Psubusw)
	case opcode.I16x8Mul:
		c.vbin(amd64.Pmullw)
	case opcode.I32x4Mul:
		c.vbin(amd64.Pmulld)
	case opcode.I64x2Mul:
		// lo(x)*lo(y) + (hi(x)*lo(y) + lo(x)*hi(y)) << 32, each product
		// of 32-bit halves.
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		t1, t2 := c.vtemp(), c.vtemp()
		a.SSEOp(amd64.MovAPD, t1, xa)
		a.VecShift(amd64.PsrlqImm, t1, 32)
		a.Vec(amd64.Pmuludq, t1, yb)
		a.SSEOp(amd64.MovAPD, t2, yb)
		a.VecShift(amd64.PsrlqImm, t2, 32)
		a.Vec(amd64.Pmuludq, t2, xa)
		a.Vec(amd64.Paddq, t1, t2)
		a.VecShift(amd64.PsllqImm, t1, 32)
		a.Vec(amd64.Pmuludq, xa, yb)
		a.Vec(amd64.Paddq, xa, t1)
		c.freeX(t1)
		c.freeX(t2)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I8x16Neg, opcode.I16x8Neg, opcode.I32x4Neg, opcode.I64x2Neg:
		sub := map[opcode.Opcode]amd64.VOp{opcode.I8x16Neg: amd64.Psubb, opcode.I16x8Neg: amd64.Psubw,
			opcode.I32x4Neg: amd64.Psubd, opcode.I64x2Neg: amd64.Psubq}[op]
		v := c.pop()
		x := c.readX(v)
		r := c.vtemp()
		a.Vec(amd64.Pxor, r, r)
		a.Vec(sub, r, x)
		c.free(v)
		c.pushReg(v128, uint8(r))
	case opcode.I8x16Abs:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Pabsb, x, x) })
	case opcode.I16x8Abs:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Pabsw, x, x) })
	case opcode.I32x4Abs:
		c.vunary(v128, func(x amd64.XReg) { a.Vec(amd64.Pabsd, x, x) })
	case opcode.I64x2Abs:
		// (x ^ s) - s, s each lane's sign spread through it.
		c.vunary(v128, func(x amd64.XReg) {
			a.VecImm(amd64.Pshufd, xTmp, x, 0xF5)
			a.VecShift(amd64.PsradImm, xTmp, 31)
			a.Vec(amd64.Pxor, x, xTmp)
			a.Vec(amd64.Psubq, x, xTmp)
		})
	case opcode.I8x16MinS:
		c.vbin(amd64.Pminsb)
	case opcode.I8x16MinU:
		c.vbin(amd64.Pminub)
	case opcode.I8x16MaxS:
		c.vbin(amd64.Pmaxsb)
	case opcode.I8x16MaxU:
		c.vbin(amd64.Pmaxub)
	case opcode.I16x8MinS:
		c.vbin(amd64.Pminsw)
	case opcode.I16x8MinU:
		c.vbin(amd64.Pminuw)
	case opcode.I16x8MaxS:
		c.vbin(amd64.Pmaxsw)
	case opcode.I16x8MaxU:
		c.vbin(amd64.Pmaxuw)
	case opcode.I32x4MinS:
		c.vbin(amd64.Pminsd)
	case opcode.I32x4MinU:
		c.vbin(amd64.Pminud)
	case opcode.I32x4MaxS:
		c.vbin(amd64.Pmaxsd)
	case opcode.I32x4MaxU:
		c.vbin(amd64.Pmaxud)
	case opcode.I8x16RoundingAverageU:
		c.vbin(amd64.Pavgb)
	case opcode.I16x8RoundingAverageU:
		c.vbin(amd64.Pavgw)
	case opcode.I8x16Popcnt:
		// Each nibble's count from a table of sixteen.
		const table, table2 = 0x0302020102010100, 0x0403030203020201
		v := c.pop()
		x := c.intoX(v)
		lo := c.vtemp()
		t := c.vtemp()
		c.loadVConst(xTmp, splat8(0x0f), splat8(0x0f))
		a.SSEOp(amd64.MovAPD, lo, x)
		a.Vec(amd64.Pand, lo, xTmp)
		a.VecShift(amd64.PsrlwImm, x, 4)
		a.Vec(amd64.Pand, x, xTmp)
		c.loadVConst(t, table, table2)
		a.Vec(amd64.Pshufb, t, lo)
		c.loadVConst(lo, table, table2)
		a.Vec(amd64.Pshufb, lo, x)
		a.Vec(amd64.Paddb, lo, t)
		c.freeX(x)
		c.freeX(t)
		c.pushReg(v128, uint8(lo))
	case opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue:
		// No lane equal to 0.
		eq := map[opcode.Opcode]amd64.VOp{opcode.I8x16AllTrue: amd64.Pcmpeqb, opcode.I16x8AllTrue: amd64.Pcmpeqw,
			opcode.I32x4AllTrue: amd64.Pcmpeqd, opcode.I64x2AllTrue: amd64.Pcmpeqq}[op]
		v := c.pop()
		x := c.intoX(v)
		r := c.allocG(0)
		a.Op(amd64.Xor, r, r, false)
		a.Vec(amd64.Pxor, xTmp, xTmp)
		a.Vec(eq, xTmp, x)
		a.Vec(amd64.Ptest, xTmp, xTmp)
		a.Setcc(amd64.CondE, r)
		c.freeX(x)
		c.pushReg(i32, uint8(r))
	case opcode.I8x16BitMask, opcode.I16x8BitMask, opcode.I32x4BitMask, opcode.I64x2BitMask:
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		switch op {
		case opcode.I8x16BitMask:
			a.VecMask(amd64.Pmovmskb, r, x)
		case opcode.I16x8BitMask:
			// The lanes narrowed to bytes keep their signs.
			a.SSEOp(amd64.MovAPD, xTmp, x)
			a.Vec(amd64.Packsswb, xTmp, xTmp)
			a.VecMask(amd64.Pmovmskb, r, xTmp)
			a.MovZX8(r, r)
		case opcode.I32x4BitMask:
			a.VecMask(amd64.Movmskps, r, x)
		default:
			a.VecMask(amd64.Movmskpd, r, x)
		}
		c.free(v)
		c.pushReg(i32, uint8(r))
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		c.vshift(op)

	case opcode.I8x16SConvertI16x8:
		c.vbin(amd64.Packsswb)
	case opcode.I8x16UConvertI16x8:
		c.vbin(amd64.Packuswb)
	case opcode.I16x8SConvertI32x4:
		c.vbin(amd64.Packssdw)
	case opcode.I16x8UConvertI32x4:
		c.vbin(amd64.Packusdw)
	case opcode.I16x8SConvertI8x16Low, opcode.I16x8SConvertI8x16High, opcode.I16x8UConvertI8x16Low, opcode.I16x8UConvertI8x16High,
		opcode.I32x4SConvertI16x8Low, opcode.I32x4SConvertI16x8High, opcode.I32x4UConvertI16x8Low, opcode.I32x4UConvertI16x8High,
		opcode.I64x2SConvertI32x4Low, opcode.I64x2SConvertI32x4High, opcode.I64x2UConvertI32x4Low, opcode.I64x2UConvertI32x4High:
		ext, high := extendOp(op)
		c.vunary(v128, func(x amd64.XReg) {
			if high {
				a.VecImm(amd64.Pshufd, x, x, 0xEE)
			}
			a.Vec(ext, x, x)
		})
	case opcode.I16x8ExtMulLowI8x16S, opcode.I16x8ExtMulHighI8x16S, opcode.I16x8ExtMulLowI8x16U, opcode.I16x8ExtMulHighI8x16U:
		// Each half widened, then the 16-bit products.
		ext, high := extendOp(map[opcode.Opcode]opcode.Opcode{
			opcode.I16x8ExtMulLowI8x16S: opcode.I16x8SConvertI8x16Low, opcode.I16x8ExtMulHighI8x16S: opcode.I16x8SConvertI8x16High,
			opcode.I16x8ExtMulLowI8x16U: opcode.I16x8UConvertI8x16Low, opcode.I16x8ExtMulHighI8x16U: opcode.I16x8UConvertI8x16High}[op])
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.intoX(y)
		for _, r := range []amd64.XReg{xa, yb} {
			if high {
				a.VecImm(amd64.Pshufd, r, r, 0xEE)
			}
			a.Vec(ext, r, r)
		}
		a.Vec(amd64.Pmullw, xa, yb)
		c.freeX(yb)
		c.pushReg(v128, uint8(xa))
	case opcode.I32x4ExtMulLowI16x8S, opcode.I32x4ExtMulHighI16x8S, opcode.I32x4ExtMulLowI16x8U, opcode.I32x4ExtMulHighI16x8U:
		// The low and high halves of the 16-bit products, interleaved.
		mulhi := amd64.Pmulhw
		if op == opcode.I32x4ExtMulLowI16x8U || op == opcode.I32x4ExtMulHighI16x8U {
			mulhi = amd64.Pmulhuw
		}
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		t := c.vtemp()
		a.SSEOp(amd64.MovAPD, t, xa)
		a.Vec(amd64.Pmullw, xa, yb)
		a.Vec(mulhi, t, yb)
		if op == opcode.I32x4ExtMulLowI16x8S || op == opcode.I32x4ExtMulLowI16x8U {
			a.Vec(amd64.Punpcklwd, xa, t)
		} else {
			a.Vec(amd64.Punpckhwd, xa, t)
		}
		c.freeX(t)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I64x2ExtMulLowI32x4S, opcode.I64x2ExtMulHighI32x4S, opcode.I64x2ExtMulLowI32x4U, opcode.I64x2ExtMulHighI32x4U:
		// The half's two lanes moved to the even lanes, which PMULDQ and
		// PMULUDQ multiply.
		mul, sel := amd64.Pmuldq, uint8(0x50)
		if op == opcode.I64x2ExtMulLowI32x4U || op == opcode.I64x2ExtMulHighI32x4U {
			mul = amd64.Pmuludq
		}
		if op == opcode.I64x2ExtMulHighI32x4S || op == opcode.I64x2ExtMulHighI32x4U {
			sel = 0xFA
		}
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		a.VecImm(amd64.Pshufd, xa, xa, sel)
		a.VecImm(amd64.Pshufd, xTmp, yb, sel)
		a.Vec(mul, xa, xTmp)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I16x8ExtAddPairwiseI8x16S:
		// PMADDUBSW multiplies its destination's bytes unsigned, its
		// source's signed: ones times the lanes.
		v := c.pop()
		x := c.readX(v)
		r := c.vconstInto(splat8(1), splat8(1))
		a.Vec(amd64.Pmaddubsw, r, x)
		c.free(v)
		c.pushReg(v128, uint8(r))
	case opcode.I16x8ExtAddPairwiseI8x16U:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pmaddubsw, x, splat8(1)) })
	case opcode.I32x4ExtAddPairwiseI16x8S:
		c.vunary(v128, func(x amd64.XReg) { c.vopConst(amd64.Pmaddwd, x, splat16(1)) })
	case opcode.I32x4ExtAddPairwiseI16x8U:
		// The lanes made signed by subtracting 0x8000, summed, and the
		// 0x10000 taken from each sum added back.
		c.vunary(v128, func(x amd64.XReg) {
			c.vopConst(amd64.Pxor, x, splat16(0x8000))
			c.vopConst(amd64.Pmaddwd, x, splat16(1))
			c.vopConst(amd64.Paddd, x, splat32(0x10000))
		})
	case opcode.I32x4DotI16x8S:
		c.vbin(amd64.Pmaddwd)
	case opcode.I16x8Q15MulRSatS:
		// PMULHRSW does not saturate: its only result past the range,
		// 0x8000 of 0x8000 * 0x8000, is complemented.
		c.vbin(amd64.Pmulhrsw)
		x := c.stack[len(c.stack)-1].xmm()
		c.loadVConst(xTmp, splat16(0x8000), splat16(0x8000))
		a.Vec(amd64.Pcmpeqw, xTmp, x)
		a.Vec(amd64.Pxor, x, xTmp)

	case opcode.I8x16Eq:
		c.vbin(amd64.Pcmpeqb)
	case opcode.I16x8Eq:
		c.vbin(amd64.Pcmpeqw)
	case opcode.I32x4Eq:
		c.vbin(amd64.Pcmpeqd)
	case opcode.I64x2Eq:
		c.vbin(amd64.Pcmpeqq)
	case opcode.I8x16Ne:
		c.vbinNot(amd64.Pcmpeqb)
	case opcode.I16x8Ne:
		c.vbinNot(amd64.Pcmpeqw)
	case opcode.I32x4Ne:
		c.vbinNot(amd64.Pcmpeqd)
	case opcode.I64x2Ne:
		c.vbinNot(amd64.Pcmpeqq)
	case opcode.I8x16GtS:
		c.vbin(amd64.Pcmpgtb)
	case opcode.I16x8GtS:
		c.vbin(amd64.Pcmpgtw)
	case opcode.I32x4GtS:
		c.vbin(amd64.Pcmpgtd)
	case opcode.I64x2GtS:
		c.vbin(amd64.Pcmpgtq)
	case opcode.I8x16LtS:
		c.vbinRev(amd64.Pcmpgtb)
	case opcode.I16x8LtS:
		c.vbinRev(amd64.Pcmpgtw)
	case opcode.I32x4LtS:
		c.vbinRev(amd64.Pcmpgtd)
	case opcode.I64x2LtS:
		c.vbinRev(amd64.Pcmpgtq)
	case opcode.I8x16LeS:
		c.vbinNot(amd64.Pcmpgtb)
	case opcode.I16x8LeS:
		c.vbinNot(amd64.Pcmpgtw)
	case opcode.I32x4LeS:
		c.vbinNot(amd64.Pcmpgtd)
	case opcode.I64x2LeS:
		c.vbinNot(amd64.Pcmpgtq)
	case opcode.I8x16GeS:
		c.vbinRevNot(amd64.Pcmpgtb)
	case opcode.I16x8GeS:
		c.vbinRevNot(amd64.Pcmpgtw)
	case opcode.I32x4GeS:
		c.vbinRevNot(amd64.Pcmpgtd)
	case opcode.I64x2GeS:
		c.vbinRevNot(amd64.Pcmpgtq)
	case opcode.I8x16GeU:
		c.vcmpU(amd64.Pmaxub, amd64.Pcmpeqb, false)
	case opcode.I8x16LtU:
		c.vcmpU(amd64.Pmaxub, amd64.Pcmpeqb, true)
	case opcode.I8x16LeU:
		c.vcmpU(amd64.Pminub, amd64.Pcmpeqb, false)
	case opcode.I8x16GtU:
		c.vcmpU(amd64.Pminub, amd64.Pcmpeqb, true)
	case opcode.I16x8GeU:
		c.vcmpU(amd64.Pmaxuw, amd64.Pcmpeqw, false)
	case opcode.I16x8LtU:
		c.vcmpU(amd64.Pmaxuw, amd64.Pcmpeqw, true)
	case opcode.I16x8LeU:
		c.vcmpU(amd64.Pminuw, amd64.Pcmpeqw, false)
	case opcode.I16x8GtU:
		c.vcmpU(amd64.Pminuw, amd64.Pcmpeqw, true)
	case opcode.I32x4GeU:
		c.vcmpU(amd64.Pmaxud, amd64.Pcmpeqd, false)
	case opcode.I32x4LtU:
		c.vcmpU(amd64.Pmaxud, amd64.Pcmpeqd, true)
	case opcode.I32x4LeU:
		c.vcmpU(amd64.Pminud, amd64.Pcmpeqd, false)
	case opcode.I32x4GtU:
		c.vcmpU(amd64.Pminud, amd64.Pcmpeqd, true)

	case opcode.I8x16Swizzle:
		// An index past 15 made one with its top bit set, which PSHUFB
		// selects 0 for.
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.intoX(y)
		c.vopConst(amd64.Paddusb, yb, splat8(0x70))
		a.Vec(amd64.Pshufb, xa, yb)
		c.freeX(yb)
		c.pushReg(v128, uint8(xa))
	case opcode.I8x16Shuffle:
		var lanes [16]byte
		copy(lanes[:], c.r.b[c.r.pc:c.r.pc+16])
		c.r.pc += 16
		c.shuffle(lanes)
	default:
		return false
	}
	return true
}

// extendOp is the PMOVSX or PMOVZX of an extension, and whether it extends
// the high half.
func extendOp(op opcode.Opcode) (amd64.VOp, bool) {
	switch op {
	case opcode.I16x8SConvertI8x16Low:
		return amd64.Pmovsxbw, false
	case opcode.I16x8SConvertI8x16High:
		return amd64.Pmovsxbw, true
	case opcode.I16x8UConvertI8x16Low:
		return amd64.Pmovzxbw, false
	case opcode.I16x8UConvertI8x16High:
		return amd64.Pmovzxbw, true
	case opcode.I32x4SConvertI16x8Low:
		return amd64.Pmovsxwd, false
	case opcode.I32x4SConvertI16x8High:
		return amd64.Pmovsxwd, true
	case opcode.I32x4UConvertI16x8Low:
		return amd64.Pmovzxwd, false
	case opcode.I32x4UConvertI16x8High:
		return amd64.Pmovzxwd, true
	case opcode.I64x2SConvertI32x4Low:
		return amd64.Pmovsxdq, false
	case opcode.I64x2SConvertI32x4High:
		return amd64.Pmovsxdq, true
	case opcode.I64x2UConvertI32x4Low:
		return amd64.Pmovzxdq, false
	}
	return amd64.Pmovzxdq, true
}

// shuffle compiles i8x16.shuffle: byte i of the result is byte lanes[i] of
// x's bytes followed by y's, each half's selected by PSHUFB with 0 where
// the other half's lane is.
func (c *compiler) shuffle(lanes [16]byte) {
	a := c.a
	var m1, m2 [2]uint64
	fromX, fromY := false, false
	for i, l := range lanes {
		l &= 31
		b1, b2 := uint64(0x80), uint64(0x80)
		if l < 16 {
			b1, fromX = uint64(l), true
		} else {
			b2, fromY = uint64(l-16), true
		}
		m1[i/8] |= b1 << (8 * (i % 8))
		m2[i/8] |= b2 << (8 * (i % 8))
	}
	y := c.pop()
	x := c.pop()
	switch {
	case !fromY:
		xa := c.intoX(x)
		c.loadVConst(xTmp, m1[0], m1[1])
		a.Vec(amd64.Pshufb, xa, xTmp)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case !fromX:
		yb := c.intoX(y)
		c.loadVConst(xTmp, m2[0], m2[1])
		a.Vec(amd64.Pshufb, yb, xTmp)
		c.free(x)
		c.pushReg(v128, uint8(yb))
	default:
		xa := c.intoX(x)
		yb := c.intoX(y)
		c.loadVConst(xTmp, m1[0], m1[1])
		a.Vec(amd64.Pshufb, xa, xTmp)
		c.loadVConst(xTmp, m2[0], m2[1])
		a.Vec(amd64.Pshufb, yb, xTmp)
		a.Vec(amd64.Por, xa, yb)
		c.freeX(yb)
		c.pushReg(v128, uint8(xa))
	}
}

// vshift compiles a shift of every lane by an i32 count, taken modulo the
// lanes' width. SSE has no shifts of bytes, and no arithmetic shift right
// of 64-bit lanes.
func (c *compiler) vshift(op opcode.Opcode) {
	a := c.a
	var bits uint64
	switch op {
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU:
		bits = 8
	case opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU:
		bits = 16
	case opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU:
		bits = 32
	default:
		bits = 64
	}
	type ops struct {
		imm amd64.VShift
		reg amd64.VOp
	}
	direct := map[opcode.Opcode]ops{
		opcode.I16x8Shl: {amd64.PsllwImm, amd64.Psllw}, opcode.I16x8ShrS: {amd64.PsrawImm, amd64.Psraw},
		opcode.I16x8ShrU: {amd64.PsrlwImm, amd64.Psrlw}, opcode.I32x4Shl: {amd64.PslldImm, amd64.Pslld},
		opcode.I32x4ShrS: {amd64.PsradImm, amd64.Psrad}, opcode.I32x4ShrU: {amd64.PsrldImm, amd64.Psrld},
		opcode.I64x2Shl: {amd64.PsllqImm, amd64.Psllq}, opcode.I64x2ShrU: {amd64.PsrlqImm, amd64.Psrlq},
	}
	s := c.pop()
	v := c.pop()
	if s.k == kConst {
		n := uint8(s.c & (bits - 1))
		x := c.intoX(v)
		switch d, ok := direct[op]; {
		case n == 0:
		case ok:
			a.VecShift(d.imm, x, n)
		case op == opcode.I64x2ShrS:
			// (x >>u n ^ m) - m, m the sign bit shifted alike.
			m := uint64(1<<63) >> n
			a.VecShift(amd64.PsrlqImm, x, n)
			c.vopConst(amd64.Pxor, x, m)
			c.vopConst(amd64.Psubq, x, m)
		case op == opcode.I8x16Shl:
			a.VecShift(amd64.PsllwImm, x, n)
			c.vopConst(amd64.Pand, x, splat8(0xff<<n))
		case op == opcode.I8x16ShrU:
			a.VecShift(amd64.PsrlwImm, x, n)
			c.vopConst(amd64.Pand, x, splat8(0xff>>n))
		default:
			// I8x16ShrS: shifted unsigned, the sign extended as for i64.
			m := splat8(0x80 >> n)
			a.VecShift(amd64.PsrlwImm, x, n)
			c.vopConst(amd64.Pand, x, splat8(0xff>>n))
			c.vopConst(amd64.Pxor, x, m)
			c.vopConst(amd64.Psubb, x, m)
		}
		c.pushReg(v128, uint8(x))
		return
	}
	r := c.intoG(s, 0)
	x := c.intoX(v)
	a.OpImm(amd64.And, r, int32(bits-1), false)
	cnt := c.vtemp()
	if op == opcode.I8x16ShrS {
		// The bytes doubled into 16-bit lanes, shifted right 8 more.
		a.OpImm(amd64.Add, r, 8, false)
	}
	a.MovDToX(cnt, r)
	c.freeG(r)
	switch d, ok := direct[op]; {
	case ok:
		a.Vec(d.reg, x, cnt)
	case op == opcode.I64x2ShrS:
		c.loadVConst(xTmp, 1<<63, 1<<63)
		a.Vec(amd64.Psrlq, xTmp, cnt)
		a.Vec(amd64.Psrlq, x, cnt)
		a.Vec(amd64.Pxor, x, xTmp)
		a.Vec(amd64.Psubq, x, xTmp)
	default:
		// The bytes widened to 16-bit lanes, shifted, and narrowed.
		lo := c.vtemp()
		a.SSEOp(amd64.MovAPD, lo, x)
		switch op {
		case opcode.I8x16ShrS:
			a.Vec(amd64.Punpcklbw, lo, lo)
			a.Vec(amd64.Punpckhbw, x, x)
			a.Vec(amd64.Psraw, lo, cnt)
			a.Vec(amd64.Psraw, x, cnt)
			a.Vec(amd64.Packsswb, lo, x)
		default:
			a.Vec(amd64.Pxor, xTmp, xTmp)
			a.Vec(amd64.Punpcklbw, lo, xTmp)
			a.Vec(amd64.Punpckhbw, x, xTmp)
			if op == opcode.I8x16Shl {
				a.Vec(amd64.Psllw, lo, cnt)
				a.Vec(amd64.Psllw, x, cnt)
				c.vopConst(amd64.Pand, lo, splat16(0xff))
				c.vopConst(amd64.Pand, x, splat16(0xff))
			} else {
				a.Vec(amd64.Psrlw, lo, cnt)
				a.Vec(amd64.Psrlw, x, cnt)
			}
			a.Vec(amd64.Packuswb, lo, x)
		}
		c.freeX(x)
		x = lo
	}
	c.freeX(cnt)
	c.pushReg(v128, uint8(x))
}
