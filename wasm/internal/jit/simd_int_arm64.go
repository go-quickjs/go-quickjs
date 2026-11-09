package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The integer lane instructions on arm64.

// vbinNot is vbin with the result complemented.
func (c *compiler) vbinNot(op arm64.VOp, s arm64.VSize) {
	c.vbin(op, s)
	x := c.stack[len(c.stack)-1].xmm()
	c.a.V2(arm64.VNot, arm64.V8, x, x)
}

// vbinWith compiles a binary operation into a new register: r = f(x, y).
func (c *compiler) vbinWith(f func(r, x, y arm64.FReg)) {
	y := c.pop()
	x := c.pop()
	xa := c.readXIn(x, xTmp)
	yb := c.readXIn(y, xTmp2)
	r := c.vtemp()
	f(r, xa, yb)
	c.free(x)
	c.free(y)
	c.pushReg(v128, uint8(r))
}

// lanesOf is the lane size of an integer instruction, by its shape.
func lanesOf(op opcode.Opcode) arm64.VSize {
	switch name := op.Name(); {
	case name[:5] == "i8x16":
		return arm64.V8
	case name[:5] == "i16x8":
		return arm64.V16
	case name[:5] == "i32x4", name[:5] == "f32x4":
		return arm64.V32
	}
	return arm64.V64
}

// simdInt compiles an integer lane instruction natively, reporting whether
// op is one it compiles.
func (c *compiler) simdInt(op opcode.Opcode) bool {
	a := c.a
	s := lanesOf(op)
	switch op {
	case opcode.I8x16Add, opcode.I16x8Add, opcode.I32x4Add, opcode.I64x2Add:
		c.vbin(arm64.VAdd, s)
	case opcode.I8x16Sub, opcode.I16x8Sub, opcode.I32x4Sub, opcode.I64x2Sub:
		c.vbin(arm64.VSub, s)
	case opcode.I8x16AddSatS, opcode.I16x8AddSatS:
		c.vbin(arm64.VSqadd, s)
	case opcode.I8x16AddSatU, opcode.I16x8AddSatU:
		c.vbin(arm64.VUqadd, s)
	case opcode.I8x16SubSatS, opcode.I16x8SubSatS:
		c.vbin(arm64.VSqsub, s)
	case opcode.I8x16SubSatU, opcode.I16x8SubSatU:
		c.vbin(arm64.VUqsub, s)
	case opcode.I16x8Mul, opcode.I32x4Mul:
		c.vbin(arm64.VMul, s)
	case opcode.I64x2Mul:
		// ASIMD has no product of 64-bit lanes: each lane's in a general
		// register.
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		r := c.allocG(0)
		for lane := uint8(0); lane < 2; lane++ {
			a.Umov(arm64.V64, rTmp, xa, lane)
			a.Umov(arm64.V64, r, yb, lane)
			a.Mul(rTmp, rTmp, r)
			a.InsG(arm64.V64, xa, lane, rTmp)
		}
		c.freeG(r)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I8x16Neg, opcode.I16x8Neg, opcode.I32x4Neg, opcode.I64x2Neg:
		c.v2(arm64.VNeg, s)
	case opcode.I8x16Abs, opcode.I16x8Abs, opcode.I32x4Abs, opcode.I64x2Abs:
		c.v2(arm64.VAbs, s)
	case opcode.I8x16MinS, opcode.I16x8MinS, opcode.I32x4MinS:
		c.vbin(arm64.VSmin, s)
	case opcode.I8x16MinU, opcode.I16x8MinU, opcode.I32x4MinU:
		c.vbin(arm64.VUmin, s)
	case opcode.I8x16MaxS, opcode.I16x8MaxS, opcode.I32x4MaxS:
		c.vbin(arm64.VSmax, s)
	case opcode.I8x16MaxU, opcode.I16x8MaxU, opcode.I32x4MaxU:
		c.vbin(arm64.VUmax, s)
	case opcode.I8x16RoundingAverageU, opcode.I16x8RoundingAverageU:
		c.vbin(arm64.VUrhadd, s)
	case opcode.I8x16Popcnt:
		c.v2(arm64.VCnt, arm64.V8)
	case opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue:
		// No lane equal to 0: no byte of the lanes' comparisons with 0 set.
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		a.V2(arm64.VCmeq0, s, xTmp2, x)
		a.V2(arm64.VUmaxv, arm64.V8, xTmp2, xTmp2)
		a.Umov(arm64.V8, r, xTmp2, 0)
		c.free(v)
		a.CmpImm(r, 0, false)
		a.Cset(r, arm64.EQ)
		c.pushReg(i32, uint8(r))
	case opcode.I8x16BitMask, opcode.I16x8BitMask, opcode.I32x4BitMask, opcode.I64x2BitMask:
		// Each negative lane made its bit, and the bits summed.
		v := c.pop()
		x := c.readX(v)
		r := c.allocG(0)
		a.V2(arm64.VCmlt0, s, xTmp2, x)
		c.free(v)
		switch op {
		case opcode.I8x16BitMask:
			c.loadVConst(xTmp, 0x8040201008040201, 0x8040201008040201)
			a.V3(arm64.VAnd, arm64.V8, xTmp2, xTmp2, xTmp)
			// The high half's bytes interleaved with the low half's: each
			// 16-bit lane low byte low half, high byte high half.
			a.Ext(xTmp, xTmp2, xTmp2, 8)
			a.V3(arm64.VZip1, arm64.V8, xTmp2, xTmp2, xTmp)
			a.V2(arm64.VAddv, arm64.V16, xTmp2, xTmp2)
			a.Umov(arm64.V16, r, xTmp2, 0)
		case opcode.I16x8BitMask:
			c.loadVConst(xTmp, 0x0008000400020001, 0x0080004000200010)
			a.V3(arm64.VAnd, arm64.V8, xTmp2, xTmp2, xTmp)
			a.V2(arm64.VAddv, arm64.V16, xTmp2, xTmp2)
			a.Umov(arm64.V16, r, xTmp2, 0)
		case opcode.I32x4BitMask:
			c.loadVConst(xTmp, 0x0000000200000001, 0x0000000800000004)
			a.V3(arm64.VAnd, arm64.V8, xTmp2, xTmp2, xTmp)
			a.V2(arm64.VAddv, arm64.V32, xTmp2, xTmp2)
			a.Umov(arm64.V32, r, xTmp2, 0)
		default:
			c.loadVConst(xTmp, 1, 2)
			a.V3(arm64.VAnd, arm64.V8, xTmp2, xTmp2, xTmp)
			a.V3(arm64.VAddp, arm64.V64, xTmp2, xTmp2, xTmp2)
			a.Umov(arm64.V64, r, xTmp2, 0)
		}
		c.pushReg(i32, uint8(r))
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		c.vshift(op, s)

	case opcode.I8x16SConvertI16x8, opcode.I8x16UConvertI16x8, opcode.I16x8SConvertI32x4, opcode.I16x8UConvertI32x4:
		// x's lanes narrowed into the lower half, y's into the upper.
		n := arm64.VSqxtn
		if op == opcode.I8x16UConvertI16x8 || op == opcode.I16x8UConvertI32x4 {
			n = arm64.VSqxtun
		}
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		a.V2(n, s, xa, xa)
		a.V2(n|arm64.VQ, s, xa, yb)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I16x8SConvertI8x16Low, opcode.I32x4SConvertI16x8Low, opcode.I64x2SConvertI32x4Low:
		c.vunary(v128, func(x arm64.FReg) { a.VShiftImm(arm64.VSshll, s-1, x, x, 0) })
	case opcode.I16x8SConvertI8x16High, opcode.I32x4SConvertI16x8High, opcode.I64x2SConvertI32x4High:
		c.vunary(v128, func(x arm64.FReg) { a.VShiftImm(arm64.VSshll|arm64.VQ, s-1, x, x, 0) })
	case opcode.I16x8UConvertI8x16Low, opcode.I32x4UConvertI16x8Low, opcode.I64x2UConvertI32x4Low:
		c.vunary(v128, func(x arm64.FReg) { a.VShiftImm(arm64.VUshll, s-1, x, x, 0) })
	case opcode.I16x8UConvertI8x16High, opcode.I32x4UConvertI16x8High, opcode.I64x2UConvertI32x4High:
		c.vunary(v128, func(x arm64.FReg) { a.VShiftImm(arm64.VUshll|arm64.VQ, s-1, x, x, 0) })
	case opcode.I16x8ExtMulLowI8x16S, opcode.I32x4ExtMulLowI16x8S, opcode.I64x2ExtMulLowI32x4S:
		c.vbin(arm64.VSmull, s-1)
	case opcode.I16x8ExtMulHighI8x16S, opcode.I32x4ExtMulHighI16x8S, opcode.I64x2ExtMulHighI32x4S:
		c.vbin(arm64.VSmull|arm64.VQ, s-1)
	case opcode.I16x8ExtMulLowI8x16U, opcode.I32x4ExtMulLowI16x8U, opcode.I64x2ExtMulLowI32x4U:
		c.vbin(arm64.VUmull, s-1)
	case opcode.I16x8ExtMulHighI8x16U, opcode.I32x4ExtMulHighI16x8U, opcode.I64x2ExtMulHighI32x4U:
		c.vbin(arm64.VUmull|arm64.VQ, s-1)
	case opcode.I16x8ExtAddPairwiseI8x16S, opcode.I32x4ExtAddPairwiseI16x8S:
		c.v2(arm64.VSaddlp, s-1)
	case opcode.I16x8ExtAddPairwiseI8x16U, opcode.I32x4ExtAddPairwiseI16x8U:
		c.v2(arm64.VUaddlp, s-1)
	case opcode.I32x4DotI16x8S:
		// The products of each half, then the sums of their pairs.
		c.vbinWith(func(r, x, y arm64.FReg) {
			t := c.vtemp()
			a.V3(arm64.VSmull, arm64.V16, r, x, y)
			a.V3(arm64.VSmull|arm64.VQ, arm64.V16, t, x, y)
			a.V3(arm64.VAddp, arm64.V32, r, r, t)
			c.freeX(t)
		})
	case opcode.I16x8Q15MulRSatS:
		c.vbin(arm64.VSqrdmulh, arm64.V16)

	case opcode.I8x16Eq, opcode.I16x8Eq, opcode.I32x4Eq, opcode.I64x2Eq:
		c.vbin(arm64.VCmeq, s)
	case opcode.I8x16Ne, opcode.I16x8Ne, opcode.I32x4Ne, opcode.I64x2Ne:
		c.vbinNot(arm64.VCmeq, s)
	case opcode.I8x16GtS, opcode.I16x8GtS, opcode.I32x4GtS, opcode.I64x2GtS:
		c.vbin(arm64.VCmgt, s)
	case opcode.I8x16LtS, opcode.I16x8LtS, opcode.I32x4LtS, opcode.I64x2LtS:
		c.vbinRev(arm64.VCmgt, s)
	case opcode.I8x16GeS, opcode.I16x8GeS, opcode.I32x4GeS, opcode.I64x2GeS:
		c.vbin(arm64.VCmge, s)
	case opcode.I8x16LeS, opcode.I16x8LeS, opcode.I32x4LeS, opcode.I64x2LeS:
		c.vbinRev(arm64.VCmge, s)
	case opcode.I8x16GtU, opcode.I16x8GtU, opcode.I32x4GtU:
		c.vbin(arm64.VCmhi, s)
	case opcode.I8x16LtU, opcode.I16x8LtU, opcode.I32x4LtU:
		c.vbinRev(arm64.VCmhi, s)
	case opcode.I8x16GeU, opcode.I16x8GeU, opcode.I32x4GeU:
		c.vbin(arm64.VCmhs, s)
	case opcode.I8x16LeU, opcode.I16x8LeU, opcode.I32x4LeU:
		c.vbinRev(arm64.VCmhs, s)

	case opcode.I8x16Swizzle:
		// TBL selects 0 for an index past the table's 16 bytes.
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		a.Tbl(xa, xa, 1, yb)
		c.free(y)
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

// shuffle compiles i8x16.shuffle: TBL of the table of x and y, copied into
// the scratch registers V30 and V31, which are consecutive.
func (c *compiler) shuffle(lanes [16]byte) {
	a := c.a
	var idx [2]uint64
	for i, l := range lanes {
		idx[i/8] |= uint64(l&31) << (8 * (i % 8))
	}
	y := c.pop()
	x := c.pop()
	c.loadX(xTmp2, x)
	c.loadX(xTmp, y)
	c.free(x)
	c.free(y)
	r := c.vtemp()
	c.loadVConst(r, idx[0], idx[1])
	a.Tbl(r, xTmp2, 2, r)
	c.pushReg(v128, uint8(r))
}

// vshift compiles a shift of every lane of size s by an i32 count, taken
// modulo the lanes' width: by an immediate where the count is a constant,
// else by SSHL or USHL of the count, negated to shift right.
func (c *compiler) vshift(op opcode.Opcode, s arm64.VSize) {
	a := c.a
	bits := uint64(8) << s
	left := op == opcode.I8x16Shl || op == opcode.I16x8Shl || op == opcode.I32x4Shl || op == opcode.I64x2Shl
	signed := op == opcode.I8x16ShrS || op == opcode.I16x8ShrS || op == opcode.I32x4ShrS || op == opcode.I64x2ShrS
	n := c.pop()
	v := c.pop()
	if n.k == kConst {
		k := uint8(n.c & (bits - 1))
		x := c.intoX(v)
		switch {
		case k == 0:
		case left:
			a.VShiftImm(arm64.VShl, s, x, x, k)
		case signed:
			a.VShiftImm(arm64.VSshr, s, x, x, k)
		default:
			a.VShiftImm(arm64.VUshr, s, x, x, k)
		}
		c.pushReg(v128, uint8(x))
		return
	}
	r := c.readG(n, rTmp)
	a.LogicImm(arm64.And, rTmp, r, bits-1, false)
	c.free(n)
	if !left {
		a.Neg(rTmp, rTmp, false)
	}
	x := c.intoX(v)
	a.Dup(s, xTmp, rTmp)
	op3 := arm64.VUshl
	if signed {
		op3 = arm64.VSshl
	}
	a.V3(op3, s, x, x, xTmp)
	c.pushReg(v128, uint8(x))
}
