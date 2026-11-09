package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The float lane instructions and the conversions on arm64, with the FPCR
// Go leaves: the vector instructions process NaNs and order zeros lane by
// lane as the scalar ones the interpreter's operators compile to.

// vmask applies a bitwise operation with the constant m in both halves to
// the top v128: abs and neg, which change no NaN's payload.
func (c *compiler) vmask(op arm64.VOp, m uint64) {
	c.vunary(v128, func(x arm64.FReg) {
		c.loadVConst(xTmp, m, m)
		c.a.V3(op, arm64.V8, x, x, xTmp)
	})
}

// vpminmax compiles pmin (y < x ? y : x) and pmax (x < y ? y : x): the
// comparison's mask selects y's lanes.
func (c *compiler) vpminmax(s arm64.VSize, isMax bool) {
	c.vbinWith(func(r, x, y arm64.FReg) {
		if isMax {
			c.a.V3(arm64.VFcmgt, s, r, y, x)
		} else {
			c.a.V3(arm64.VFcmgt, s, r, x, y)
		}
		c.a.V3(arm64.VBsl, arm64.V8, r, y, x)
	})
}

// simdFloat compiles a float lane instruction or a conversion natively,
// reporting whether op is one it compiles.
func (c *compiler) simdFloat(op opcode.Opcode) bool {
	a := c.a
	s := arm64.V32
	if op.Name()[:5] == "f64x2" {
		s = arm64.V64
	}
	switch op {
	case opcode.F32x4Add, opcode.F64x2Add:
		c.vbin(arm64.VFadd, s)
	case opcode.F32x4Sub, opcode.F64x2Sub:
		c.vbin(arm64.VFsub, s)
	case opcode.F32x4Mul, opcode.F64x2Mul:
		c.vbin(arm64.VFmul, s)
	case opcode.F32x4Div, opcode.F64x2Div:
		c.vbin(arm64.VFdiv, s)
	case opcode.F32x4Min, opcode.F64x2Min:
		c.vbin(arm64.VFmin, s)
	case opcode.F32x4Max, opcode.F64x2Max:
		c.vbin(arm64.VFmax, s)
	case opcode.F32x4Pmin, opcode.F64x2Pmin:
		c.vpminmax(s, false)
	case opcode.F32x4Pmax, opcode.F64x2Pmax:
		c.vpminmax(s, true)
	case opcode.F32x4Sqrt, opcode.F64x2Sqrt:
		c.v2(arm64.VFsqrt, s)
	case opcode.F32x4Abs:
		c.vmask(arm64.VBic, splat32(0x80000000))
	case opcode.F64x2Abs:
		c.vmask(arm64.VBic, 1<<63)
	case opcode.F32x4Neg:
		c.vmask(arm64.VEor, splat32(0x80000000))
	case opcode.F64x2Neg:
		c.vmask(arm64.VEor, 1<<63)
	case opcode.F32x4Ceil, opcode.F64x2Ceil:
		c.v2(arm64.VFrintp, s)
	case opcode.F32x4Floor, opcode.F64x2Floor:
		c.v2(arm64.VFrintm, s)
	case opcode.F32x4Trunc, opcode.F64x2Trunc:
		c.v2(arm64.VFrintz, s)
	case opcode.F32x4NearestInt, opcode.F64x2NearestInt:
		c.v2(arm64.VFrintn, s)
	case opcode.F32x4Eq, opcode.F64x2Eq:
		c.vbin(arm64.VFcmeq, s)
	case opcode.F32x4Ne, opcode.F64x2Ne:
		c.vbinNot(arm64.VFcmeq, s)
	case opcode.F32x4Gt, opcode.F64x2Gt:
		c.vbin(arm64.VFcmgt, s)
	case opcode.F32x4Ge, opcode.F64x2Ge:
		c.vbin(arm64.VFcmge, s)
	case opcode.F32x4Lt, opcode.F64x2Lt:
		c.vbinRev(arm64.VFcmgt, s)
	case opcode.F32x4Le, opcode.F64x2Le:
		c.vbinRev(arm64.VFcmge, s)

	case opcode.F32x4DemoteF64x2Zero:
		c.v2(arm64.VFcvtn, arm64.V64)
	case opcode.F64x2PromoteLowF32x4:
		c.v2(arm64.VFcvtl, arm64.V64)
	case opcode.F32x4SConvertI32x4:
		c.v2(arm64.VScvtf, arm64.V32)
	case opcode.F32x4UConvertI32x4:
		c.v2(arm64.VUcvtf, arm64.V32)
	case opcode.F64x2ConvertLowI32x4S:
		c.vunary(v128, func(x arm64.FReg) {
			a.VShiftImm(arm64.VSshll, arm64.V32, x, x, 0)
			a.V2(arm64.VScvtf, arm64.V64, x, x)
		})
	case opcode.F64x2ConvertLowI32x4U:
		c.vunary(v128, func(x arm64.FReg) {
			a.VShiftImm(arm64.VUshll, arm64.V32, x, x, 0)
			a.V2(arm64.VUcvtf, arm64.V64, x, x)
		})
	case opcode.I32x4SConvertF32x4:
		// FCVTZS and FCVTZU saturate, and give 0 for NaN.
		c.v2(arm64.VFcvtzs, arm64.V32)
	case opcode.I32x4UConvertF32x4:
		c.v2(arm64.VFcvtzu, arm64.V32)
	case opcode.I32x4TruncSatF64x2SZero:
		c.vunary(v128, func(x arm64.FReg) {
			a.V2(arm64.VFcvtzs, arm64.V64, x, x)
			a.V2(arm64.VSqxtn, arm64.V32, x, x)
		})
	case opcode.I32x4TruncSatF64x2UZero:
		c.vunary(v128, func(x arm64.FReg) {
			a.V2(arm64.VFcvtzu, arm64.V64, x, x)
			a.V2(arm64.VUqxtn, arm64.V32, x, x)
		})
	default:
		return c.simdRelaxed(op)
	}
	return true
}
