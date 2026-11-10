package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The relaxed SIMD instructions on arm64, with the interpreter's results
// there (engine/relaxed_other.go): those of the ASIMD instructions that
// compute them, most the deterministic ones'.

// simdRelaxed compiles a relaxed SIMD instruction natively, reporting
// whether it did.
func (c *compiler) simdRelaxed(op opcode.Opcode) bool {
	a := c.a
	switch op {
	case opcode.I8x16RelaxedSwizzle:
		y := c.pop()
		x := c.pop()
		xa := c.intoX(x)
		yb := c.readX(y)
		a.Tbl(xa, xa, 1, yb)
		c.free(y)
		c.pushReg(v128, uint8(xa))
	case opcode.I32x4RelaxedTruncF32x4S:
		c.v2(arm64.VFcvtzs, arm64.V32)
	case opcode.I32x4RelaxedTruncF32x4U:
		c.v2(arm64.VFcvtzu, arm64.V32)
	case opcode.I32x4RelaxedTruncF64x2SZero:
		c.vunary(v128, func(x arm64.FReg) {
			a.V2(arm64.VFcvtzs, arm64.V64, x, x)
			a.V2(arm64.VSqxtn, arm64.V32, x, x)
		})
	case opcode.I32x4RelaxedTruncF64x2UZero:
		c.vunary(v128, func(x arm64.FReg) {
			a.V2(arm64.VFcvtzu, arm64.V64, x, x)
			a.V2(arm64.VUqxtn, arm64.V32, x, x)
		})
	case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
		// FMLA into the addend, the factors in their order: one
		// rounding, and the NaN of the interpreter's fmaPick32 and
		// fmaPick64 -- a signaling NaN of the addend, the first factor
		// or the second ahead of a quiet one, in that order, and the
		// default NaN of a quiet addend and an infinity times a zero.
		// nmadd's first factor is negated first, a NaN's sign flipped
		// as FNEG flips it.
		s := arm64.V64
		if op == opcode.F32x4Qfma || op == opcode.F32x4Qfms {
			s = arm64.V32
		}
		z := c.pop()
		y := c.pop()
		x := c.pop()
		zc := c.intoX(z)
		xa := c.readXIn(x, xTmp)
		yb := c.readXIn(y, xTmp2)
		if op == opcode.F32x4Qfms || op == opcode.F64x2Qfms {
			a.V2(arm64.VFneg, s, xTmp, xa)
			xa = xTmp
		}
		a.V3(arm64.VFmla, s, zc, xa, yb)
		c.free(x)
		c.free(y)
		c.pushReg(v128, uint8(zc))
	case opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect, opcode.I32x4RelaxedLaneSelect,
		opcode.I64x2RelaxedLaneSelect:
		// BSL: every bit of the mask selects, as bitselect.
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
	case opcode.F32x4RelaxedMin:
		c.vbin(arm64.VFmin, arm64.V32)
	case opcode.F32x4RelaxedMax:
		c.vbin(arm64.VFmax, arm64.V32)
	case opcode.F64x2RelaxedMin:
		c.vbin(arm64.VFmin, arm64.V64)
	case opcode.F64x2RelaxedMax:
		c.vbin(arm64.VFmax, arm64.V64)
	case opcode.I16x8RelaxedQ15MulRS:
		c.vbin(arm64.VSqrdmulh, arm64.V16)
	case opcode.I16x8DotI8x16I7x16S, opcode.I32x4DotI8x16I7x16AddS:
		// The products of signed bytes, each pair's sum wrapped to 16
		// bits; for the add, those summed in pairs to 32 bits, plus the
		// addend.
		var z value
		add := op == opcode.I32x4DotI8x16I7x16AddS
		if add {
			z = c.pop()
		}
		c.vbinWith(func(r, x, y arm64.FReg) {
			t := c.vtemp()
			a.V3(arm64.VSmull, arm64.V8, r, x, y)
			a.V3(arm64.VSmull|arm64.VQ, arm64.V8, t, x, y)
			a.V3(arm64.VAddp, arm64.V16, r, r, t)
			c.freeX(t)
		})
		if add {
			r := c.stack[len(c.stack)-1].xmm()
			a.V2(arm64.VSaddlp, arm64.V16, r, r)
			zc := c.readX(z)
			a.V3(arm64.VAdd, arm64.V32, r, r, zc)
			c.free(z)
		}
	default:
		return false
	}
	return true
}
