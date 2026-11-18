package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD operators on arm64: ASIMD (NEON), which every arm64 CPU has,
// over a v128's two slots. Each instruction gives what the operator's Go
// function does on arm64: the float lanes process NaNs and order zeros as
// the scalar instructions the Go functions compile to, FCVTZS and FCVTZU
// saturate and give 0 for NaN as the saturating truncations do, and the
// relaxed operators are what the Go functions give on arm64
// (relaxed_other.go).

// vload is v = the v128 in the slots field i names; vstore stores v
// there. R1 is their scratch.
func (g *armGen) vload(v FReg, i int) {
	g.addr(aC, i)
	g.a.LoadQ(v, aC, 0)
}

func (g *armGen) vstore(i int, v FReg) {
	g.addr(aC, i)
	g.a.StoreQ(aC, 0, v)
}

// vloadD is v = the v128 in the slots the word after names (a ternary
// operator's D).
func (g *armGen) vloadD(v FReg) {
	g.a.LoadU16(aC, aIP, 8)
	g.a.AddLsl(aC, aL, aC, 3)
	g.a.LoadQ(v, aC, 0)
}

// vconst is v = the 128 bits hi:lo.
func (g *armGen) vconst(v FReg, lo, hi uint64) {
	g.a.MovImm(aT, lo)
	g.a.InsG(V64, v, 0, aT)
	g.a.MovImm(aT, hi)
	g.a.InsG(V64, v, 1, aT)
}

// lanes is the lane size of an integer or float operator by its name.
func lanes(name string) VSize {
	switch name[:5] {
	case "I8x16":
		return V8
	case "I16x8":
		return V16
	case "I32x4", "F32x4":
		return V32
	}
	return V64
}

func (g *armGen) simd() {
	a := g.a
	// A = f(V0 = B, V1 = C), the result in the register f returns.
	bin := func(x int, name string, f func() FReg) {
		g.hs(x, name, func() {
			g.vload(V0, 1)
			g.vload(V1, 2)
			r := f()
			g.vstore(0, r)
			g.next(1)
		})
	}
	// op3 is A = B op C, or C op B where rev, negated where not.
	op3 := func(x int, name string, op VOp, s VSize, rev, not bool) {
		bin(x, name, func() FReg {
			if rev {
				a.V3(op, s, V0, V1, V0)
			} else {
				a.V3(op, s, V0, V0, V1)
			}
			if not {
				a.V2(VNot, V8, V0, V0)
			}
			return V0
		})
	}
	// A = f(V0 = B), the result in V0.
	un := func(x int, name string, f func()) {
		g.hs(x, name, func() {
			g.vload(V0, 1)
			f()
			g.vstore(0, V0)
			g.next(1)
		})
	}
	op2 := func(x int, name string, op VOp, s VSize) {
		un(x, name, func() { a.V2(op, s, V0, V0) })
	}

	op3(rd.SS128And, "S128And", VAnd, V8, false, false)
	op3(rd.SS128Or, "S128Or", VOrr, V8, false, false)
	op3(rd.SS128Xor, "S128Xor", VEor, V8, false, false)
	op3(rd.SS128AndNot, "S128AndNot", VBic, V8, false, false)
	op2(rd.SS128Not, "S128Not", VNot, V8)
	// bitselect and the relaxed lane selects: BSL, the mask D selecting B.
	sel := func(x int, name string) {
		g.hs(x, name, func() {
			g.vload(V0, 1)
			g.vload(V1, 2)
			g.vloadD(V2)
			a.V3(VBsl, V8, V2, V0, V1)
			g.vstore(0, V2)
			g.next(2)
		})
	}
	sel(rd.SS128Select, "S128Select")
	sel(rd.SI8x16RelaxedLaneSelect, "I8x16RelaxedLaneSelect")
	sel(rd.SI16x8RelaxedLaneSelect, "I16x8RelaxedLaneSelect")
	sel(rd.SI32x4RelaxedLaneSelect, "I32x4RelaxedLaneSelect")
	sel(rd.SI64x2RelaxedLaneSelect, "I64x2RelaxedLaneSelect")

	type opv struct {
		x    int
		name string
		op   VOp
	}
	for _, o := range []opv{
		{rd.SI8x16Add, "I8x16Add", VAdd}, {rd.SI16x8Add, "I16x8Add", VAdd}, {rd.SI32x4Add, "I32x4Add", VAdd}, {rd.SI64x2Add, "I64x2Add", VAdd},
		{rd.SI8x16Sub, "I8x16Sub", VSub}, {rd.SI16x8Sub, "I16x8Sub", VSub}, {rd.SI32x4Sub, "I32x4Sub", VSub}, {rd.SI64x2Sub, "I64x2Sub", VSub},
		{rd.SI8x16AddSatS, "I8x16AddSatS", VSqadd}, {rd.SI16x8AddSatS, "I16x8AddSatS", VSqadd},
		{rd.SI8x16AddSatU, "I8x16AddSatU", VUqadd}, {rd.SI16x8AddSatU, "I16x8AddSatU", VUqadd},
		{rd.SI8x16SubSatS, "I8x16SubSatS", VSqsub}, {rd.SI16x8SubSatS, "I16x8SubSatS", VSqsub},
		{rd.SI8x16SubSatU, "I8x16SubSatU", VUqsub}, {rd.SI16x8SubSatU, "I16x8SubSatU", VUqsub},
		{rd.SI16x8Mul, "I16x8Mul", VMul}, {rd.SI32x4Mul, "I32x4Mul", VMul},
		{rd.SI8x16MinS, "I8x16MinS", VSmin}, {rd.SI16x8MinS, "I16x8MinS", VSmin}, {rd.SI32x4MinS, "I32x4MinS", VSmin},
		{rd.SI8x16MinU, "I8x16MinU", VUmin}, {rd.SI16x8MinU, "I16x8MinU", VUmin}, {rd.SI32x4MinU, "I32x4MinU", VUmin},
		{rd.SI8x16MaxS, "I8x16MaxS", VSmax}, {rd.SI16x8MaxS, "I16x8MaxS", VSmax}, {rd.SI32x4MaxS, "I32x4MaxS", VSmax},
		{rd.SI8x16MaxU, "I8x16MaxU", VUmax}, {rd.SI16x8MaxU, "I16x8MaxU", VUmax}, {rd.SI32x4MaxU, "I32x4MaxU", VUmax},
		{rd.SI8x16RoundingAverageU, "I8x16RoundingAverageU", VUrhadd}, {rd.SI16x8RoundingAverageU, "I16x8RoundingAverageU", VUrhadd},
		{rd.SI16x8Q15MulRSatS, "I16x8Q15MulRSatS", VSqrdmulh}, {rd.SI16x8RelaxedQ15MulRS, "I16x8RelaxedQ15MulRS", VSqrdmulh},
		{rd.SF32x4Add, "F32x4Add", VFadd}, {rd.SF64x2Add, "F64x2Add", VFadd},
		{rd.SF32x4Sub, "F32x4Sub", VFsub}, {rd.SF64x2Sub, "F64x2Sub", VFsub},
		{rd.SF32x4Mul, "F32x4Mul", VFmul}, {rd.SF64x2Mul, "F64x2Mul", VFmul},
		{rd.SF32x4Div, "F32x4Div", VFdiv}, {rd.SF64x2Div, "F64x2Div", VFdiv},
		{rd.SF32x4Min, "F32x4Min", VFmin}, {rd.SF64x2Min, "F64x2Min", VFmin},
		{rd.SF32x4Max, "F32x4Max", VFmax}, {rd.SF64x2Max, "F64x2Max", VFmax},
		{rd.SF32x4RelaxedMin, "F32x4RelaxedMin", VFmin}, {rd.SF64x2RelaxedMin, "F64x2RelaxedMin", VFmin},
		{rd.SF32x4RelaxedMax, "F32x4RelaxedMax", VFmax}, {rd.SF64x2RelaxedMax, "F64x2RelaxedMax", VFmax},
		{rd.SI8x16Eq, "I8x16Eq", VCmeq}, {rd.SI16x8Eq, "I16x8Eq", VCmeq}, {rd.SI32x4Eq, "I32x4Eq", VCmeq}, {rd.SI64x2Eq, "I64x2Eq", VCmeq},
		{rd.SI8x16GtS, "I8x16GtS", VCmgt}, {rd.SI16x8GtS, "I16x8GtS", VCmgt}, {rd.SI32x4GtS, "I32x4GtS", VCmgt}, {rd.SI64x2GtS, "I64x2GtS", VCmgt},
		{rd.SI8x16GeS, "I8x16GeS", VCmge}, {rd.SI16x8GeS, "I16x8GeS", VCmge}, {rd.SI32x4GeS, "I32x4GeS", VCmge}, {rd.SI64x2GeS, "I64x2GeS", VCmge},
		{rd.SI8x16GtU, "I8x16GtU", VCmhi}, {rd.SI16x8GtU, "I16x8GtU", VCmhi}, {rd.SI32x4GtU, "I32x4GtU", VCmhi},
		{rd.SI8x16GeU, "I8x16GeU", VCmhs}, {rd.SI16x8GeU, "I16x8GeU", VCmhs}, {rd.SI32x4GeU, "I32x4GeU", VCmhs},
		{rd.SF32x4Eq, "F32x4Eq", VFcmeq}, {rd.SF64x2Eq, "F64x2Eq", VFcmeq},
		{rd.SF32x4Gt, "F32x4Gt", VFcmgt}, {rd.SF64x2Gt, "F64x2Gt", VFcmgt},
		{rd.SF32x4Ge, "F32x4Ge", VFcmge}, {rd.SF64x2Ge, "F64x2Ge", VFcmge},
	} {
		op3(o.x, o.name, o.op, lanes(o.name), false, false)
	}
	// The comparisons the other way: lt and le as gt and ge of C and B,
	// ne as eq negated.
	for _, o := range []opv{
		{rd.SI8x16LtS, "I8x16LtS", VCmgt}, {rd.SI16x8LtS, "I16x8LtS", VCmgt}, {rd.SI32x4LtS, "I32x4LtS", VCmgt}, {rd.SI64x2LtS, "I64x2LtS", VCmgt},
		{rd.SI8x16LeS, "I8x16LeS", VCmge}, {rd.SI16x8LeS, "I16x8LeS", VCmge}, {rd.SI32x4LeS, "I32x4LeS", VCmge}, {rd.SI64x2LeS, "I64x2LeS", VCmge},
		{rd.SI8x16LtU, "I8x16LtU", VCmhi}, {rd.SI16x8LtU, "I16x8LtU", VCmhi}, {rd.SI32x4LtU, "I32x4LtU", VCmhi},
		{rd.SI8x16LeU, "I8x16LeU", VCmhs}, {rd.SI16x8LeU, "I16x8LeU", VCmhs}, {rd.SI32x4LeU, "I32x4LeU", VCmhs},
		{rd.SF32x4Lt, "F32x4Lt", VFcmgt}, {rd.SF64x2Lt, "F64x2Lt", VFcmgt},
		{rd.SF32x4Le, "F32x4Le", VFcmge}, {rd.SF64x2Le, "F64x2Le", VFcmge},
	} {
		op3(o.x, o.name, o.op, lanes(o.name), true, false)
	}
	for _, o := range []opv{
		{rd.SI8x16Ne, "I8x16Ne", VCmeq}, {rd.SI16x8Ne, "I16x8Ne", VCmeq}, {rd.SI32x4Ne, "I32x4Ne", VCmeq}, {rd.SI64x2Ne, "I64x2Ne", VCmeq},
		{rd.SF32x4Ne, "F32x4Ne", VFcmeq}, {rd.SF64x2Ne, "F64x2Ne", VFcmeq},
	} {
		op3(o.x, o.name, o.op, lanes(o.name), false, true)
	}
	// pmin is C < B ? C : B and pmax B < C ? C : B: the comparison's mask
	// selects C's lanes.
	for _, p := range []struct {
		x     int
		name  string
		isMax bool
	}{{rd.SF32x4Pmin, "F32x4Pmin", false}, {rd.SF32x4Pmax, "F32x4Pmax", true}, {rd.SF64x2Pmin, "F64x2Pmin", false}, {rd.SF64x2Pmax, "F64x2Pmax", true}} {
		p := p
		bin(p.x, p.name, func() FReg {
			s := lanes(p.name)
			if p.isMax {
				a.V3(VFcmgt, s, V2, V1, V0)
			} else {
				a.V3(VFcmgt, s, V2, V0, V1)
			}
			a.V3(VBsl, V8, V2, V1, V0)
			return V2
		})
	}
	bin(rd.SI64x2Mul, "I64x2Mul", func() FReg {
		for lane := uint8(0); lane < 2; lane++ {
			a.Umov(V64, aD, V0, lane)
			a.Umov(V64, aS, V1, lane)
			a.Mul(aD, aD, aS)
			a.InsG(V64, V0, lane, aD)
		}
		return V0
	})
	// The narrowings: B's lanes into the lower half, C's into the upper.
	for _, o := range []opv{
		{rd.SI8x16SConvertI16x8, "I8x16SConvertI16x8", VSqxtn}, {rd.SI8x16UConvertI16x8, "I8x16UConvertI16x8", VSqxtun},
		{rd.SI16x8SConvertI32x4, "I16x8SConvertI32x4", VSqxtn}, {rd.SI16x8UConvertI32x4, "I16x8UConvertI32x4", VSqxtun},
	} {
		o := o
		bin(o.x, o.name, func() FReg {
			s := lanes(o.name)
			a.V2(o.op, s, V0, V0)
			a.V2(o.op|VQ, s, V0, V1)
			return V0
		})
	}
	// The products and sums widened.
	for _, o := range []opv{
		{rd.SI16x8ExtMulLowI8x16S, "I16x8ExtMulLowI8x16S", VSmull}, {rd.SI32x4ExtMulLowI16x8S, "I32x4ExtMulLowI16x8S", VSmull},
		{rd.SI64x2ExtMulLowI32x4S, "I64x2ExtMulLowI32x4S", VSmull},
		{rd.SI16x8ExtMulHighI8x16S, "I16x8ExtMulHighI8x16S", VSmull | VQ}, {rd.SI32x4ExtMulHighI16x8S, "I32x4ExtMulHighI16x8S", VSmull | VQ},
		{rd.SI64x2ExtMulHighI32x4S, "I64x2ExtMulHighI32x4S", VSmull | VQ},
		{rd.SI16x8ExtMulLowI8x16U, "I16x8ExtMulLowI8x16U", VUmull}, {rd.SI32x4ExtMulLowI16x8U, "I32x4ExtMulLowI16x8U", VUmull},
		{rd.SI64x2ExtMulLowI32x4U, "I64x2ExtMulLowI32x4U", VUmull},
		{rd.SI16x8ExtMulHighI8x16U, "I16x8ExtMulHighI8x16U", VUmull | VQ}, {rd.SI32x4ExtMulHighI16x8U, "I32x4ExtMulHighI16x8U", VUmull | VQ},
		{rd.SI64x2ExtMulHighI32x4U, "I64x2ExtMulHighI32x4U", VUmull | VQ},
	} {
		o := o
		bin(o.x, o.name, func() FReg {
			a.V3(o.op, lanes(o.name)-1, V2, V0, V1)
			return V2
		})
	}
	bin(rd.SI32x4DotI16x8S, "I32x4DotI16x8S", func() FReg {
		a.V3(VSmull, V16, V2, V0, V1)
		a.V3(VSmull|VQ, V16, V3, V0, V1)
		a.V3(VAddp, V32, V2, V2, V3)
		return V2
	})
	bin(rd.SI16x8DotI8x16I7x16S, "I16x8DotI8x16I7x16S", func() FReg {
		a.V3(VSmull, V8, V2, V0, V1)
		a.V3(VSmull|VQ, V8, V3, V0, V1)
		a.V3(VAddp, V16, V2, V2, V3)
		return V2
	})
	g.hs(rd.SI32x4DotI8x16I7x16AddS, "I32x4DotI8x16I7x16AddS", func() {
		g.vload(V0, 1)
		g.vload(V1, 2)
		a.V3(VSmull, V8, V2, V0, V1)
		a.V3(VSmull|VQ, V8, V3, V0, V1)
		a.V3(VAddp, V16, V2, V2, V3)
		a.V2(VSaddlp, V16, V2, V2)
		g.vloadD(V3)
		a.V3(VAdd, V32, V2, V2, V3)
		g.vstore(0, V2)
		g.next(2)
	})
	// The fused multiply-adds: FMLA into the addend D, nmadd's first
	// factor negated first.
	for _, q := range []struct {
		x    int
		name string
		neg  bool
	}{{rd.SF32x4Qfma, "F32x4Qfma", false}, {rd.SF32x4Qfms, "F32x4Qfms", true}, {rd.SF64x2Qfma, "F64x2Qfma", false}, {rd.SF64x2Qfms, "F64x2Qfms", true}} {
		q := q
		g.hs(q.x, q.name, func() {
			s := lanes(q.name)
			g.vload(V0, 1)
			g.vload(V1, 2)
			g.vloadD(V2)
			if q.neg {
				a.V2(VFneg, s, V0, V0)
			}
			a.V3(VFmla, s, V2, V0, V1)
			g.vstore(0, V2)
			g.next(2)
		})
	}
	g.hs(rd.SI8x16Swizzle, "I8x16Swizzle", func() {
		g.vload(V0, 1)
		g.vload(V1, 2)
		a.Tbl(V0, V0, 1, V1)
		g.vstore(0, V0)
		g.next(1)
	})
	g.hs(rd.SI8x16RelaxedSwizzle, "I8x16RelaxedSwizzle", func() {
		g.vload(V0, 1)
		g.vload(V1, 2)
		a.Tbl(V0, V0, 1, V1)
		g.vstore(0, V0)
		g.next(1)
	})
	// shuffle: TBL of the table of B and C (V0 and V1, consecutive) by the
	// 16 lanes in the two words after, each below 32.
	g.hs(rd.SShuffle, "Shuffle", func() {
		g.vload(V0, 1)
		g.vload(V1, 2)
		a.LoadQ(V2, aIP, 8)
		a.Tbl(V2, V0, 2, V2)
		g.vstore(0, V2)
		g.next(3)
	})

	for _, o := range []opv{
		{rd.SI8x16Abs, "I8x16Abs", VAbs}, {rd.SI16x8Abs, "I16x8Abs", VAbs}, {rd.SI32x4Abs, "I32x4Abs", VAbs}, {rd.SI64x2Abs, "I64x2Abs", VAbs},
		{rd.SI8x16Neg, "I8x16Neg", VNeg}, {rd.SI16x8Neg, "I16x8Neg", VNeg}, {rd.SI32x4Neg, "I32x4Neg", VNeg}, {rd.SI64x2Neg, "I64x2Neg", VNeg},
		{rd.SF32x4Sqrt, "F32x4Sqrt", VFsqrt}, {rd.SF64x2Sqrt, "F64x2Sqrt", VFsqrt},
		{rd.SF32x4Ceil, "F32x4Ceil", VFrintp}, {rd.SF64x2Ceil, "F64x2Ceil", VFrintp},
		{rd.SF32x4Floor, "F32x4Floor", VFrintm}, {rd.SF64x2Floor, "F64x2Floor", VFrintm},
		{rd.SF32x4Trunc, "F32x4Trunc", VFrintz}, {rd.SF64x2Trunc, "F64x2Trunc", VFrintz},
		{rd.SF32x4NearestInt, "F32x4NearestInt", VFrintn}, {rd.SF64x2NearestInt, "F64x2NearestInt", VFrintn},
		{rd.SI32x4SConvertF32x4, "I32x4SConvertF32x4", VFcvtzs}, {rd.SI32x4UConvertF32x4, "I32x4UConvertF32x4", VFcvtzu},
		{rd.SI32x4RelaxedTruncF32x4S, "I32x4RelaxedTruncF32x4S", VFcvtzs}, {rd.SI32x4RelaxedTruncF32x4U, "I32x4RelaxedTruncF32x4U", VFcvtzu},
		{rd.SF32x4SConvertI32x4, "F32x4SConvertI32x4", VScvtf}, {rd.SF32x4UConvertI32x4, "F32x4UConvertI32x4", VUcvtf},
	} {
		op2(o.x, o.name, o.op, lanes(o.name))
	}
	op2(rd.SI8x16Popcnt, "I8x16Popcnt", VCnt, V8)
	op2(rd.SF32x4DemoteF64x2Zero, "F32x4DemoteF64x2Zero", VFcvtn, V64)
	op2(rd.SF64x2PromoteLowF32x4, "F64x2PromoteLowF32x4", VFcvtl, V64)
	for _, o := range []opv{
		{rd.SI16x8ExtAddPairwiseI8x16S, "I16x8ExtAddPairwiseI8x16S", VSaddlp}, {rd.SI32x4ExtAddPairwiseI16x8S, "I32x4ExtAddPairwiseI16x8S", VSaddlp},
		{rd.SI16x8ExtAddPairwiseI8x16U, "I16x8ExtAddPairwiseI8x16U", VUaddlp}, {rd.SI32x4ExtAddPairwiseI16x8U, "I32x4ExtAddPairwiseI16x8U", VUaddlp},
	} {
		op2(o.x, o.name, o.op, lanes(o.name)-1)
	}
	// The extensions of a half: SSHLL and USHLL by 0, of the upper half
	// with Q set.
	for _, o := range []opv{
		{rd.SI16x8SConvertI8x16Low, "I16x8SConvertI8x16Low", VSshll}, {rd.SI32x4SConvertI16x8Low, "I32x4SConvertI16x8Low", VSshll},
		{rd.SI64x2SConvertI32x4Low, "I64x2SConvertI32x4Low", VSshll},
		{rd.SI16x8SConvertI8x16High, "I16x8SConvertI8x16High", VSshll | VQ}, {rd.SI32x4SConvertI16x8High, "I32x4SConvertI16x8High", VSshll | VQ},
		{rd.SI64x2SConvertI32x4High, "I64x2SConvertI32x4High", VSshll | VQ},
		{rd.SI16x8UConvertI8x16Low, "I16x8UConvertI8x16Low", VUshll}, {rd.SI32x4UConvertI16x8Low, "I32x4UConvertI16x8Low", VUshll},
		{rd.SI64x2UConvertI32x4Low, "I64x2UConvertI32x4Low", VUshll},
		{rd.SI16x8UConvertI8x16High, "I16x8UConvertI8x16High", VUshll | VQ}, {rd.SI32x4UConvertI16x8High, "I32x4UConvertI16x8High", VUshll | VQ},
		{rd.SI64x2UConvertI32x4High, "I64x2UConvertI32x4High", VUshll | VQ},
	} {
		o := o
		un(o.x, o.name, func() { a.VShiftImm(o.op, lanes(o.name)-1, V0, V0, 0) })
	}
	un(rd.SF64x2ConvertLowI32x4S, "F64x2ConvertLowI32x4S", func() {
		a.VShiftImm(VSshll, V32, V0, V0, 0)
		a.V2(VScvtf, V64, V0, V0)
	})
	un(rd.SF64x2ConvertLowI32x4U, "F64x2ConvertLowI32x4U", func() {
		a.VShiftImm(VUshll, V32, V0, V0, 0)
		a.V2(VUcvtf, V64, V0, V0)
	})
	for _, t := range []struct {
		x       int
		name    string
		cvt, xt VOp
	}{
		{rd.SI32x4TruncSatF64x2SZero, "I32x4TruncSatF64x2SZero", VFcvtzs, VSqxtn},
		{rd.SI32x4TruncSatF64x2UZero, "I32x4TruncSatF64x2UZero", VFcvtzu, VUqxtn},
		{rd.SI32x4RelaxedTruncF64x2SZero, "I32x4RelaxedTruncF64x2SZero", VFcvtzs, VSqxtn},
		{rd.SI32x4RelaxedTruncF64x2UZero, "I32x4RelaxedTruncF64x2UZero", VFcvtzu, VUqxtn},
	} {
		t := t
		un(t.x, t.name, func() {
			a.V2(t.cvt, V64, V0, V0)
			a.V2(t.xt, V32, V0, V0)
		})
	}
	// The floats' abs and neg: the sign bits alone, which keeps a NaN's
	// payload.
	for _, f := range []struct {
		abs, neg int
		name     string
		s        VSize
		sign     uint64
	}{{rd.SF32x4Abs, rd.SF32x4Neg, "F32x4", V32, 1 << 31}, {rd.SF64x2Abs, rd.SF64x2Neg, "F64x2", V64, 1 << 63}} {
		f := f
		un(f.abs, f.name+"Abs", func() {
			a.MovImm(aT, f.sign)
			a.Dup(f.s, V1, aT)
			a.V3(VBic, V8, V0, V0, V1)
		})
		un(f.neg, f.name+"Neg", func() {
			a.MovImm(aT, f.sign)
			a.Dup(f.s, V1, aT)
			a.V3(VEor, V8, V0, V0, V1)
		})
	}

	// The tests: i32 A = op B.
	test := func(x int, name string, f func()) {
		g.hs(x, name, func() {
			g.vload(V0, 1)
			f()
			g.store(0, aD)
			g.next(1)
		})
	}
	test(rd.SV128AnyTrue, "V128AnyTrue", func() {
		a.V2(VUmaxv, V8, V1, V0)
		a.Umov(V8, aD, V1, 0)
		a.CmpImm(aD, 0, false)
		a.Cset(aD, NE)
	})
	for _, o := range []opv{{rd.SI8x16AllTrue, "I8x16AllTrue", 0}, {rd.SI16x8AllTrue, "I16x8AllTrue", 0},
		{rd.SI32x4AllTrue, "I32x4AllTrue", 0}, {rd.SI64x2AllTrue, "I64x2AllTrue", 0}} {
		s := lanes(o.name)
		test(o.x, o.name, func() {
			// No lane equal to 0.
			a.V2(VCmeq0, s, V1, V0)
			a.V2(VUmaxv, V8, V1, V1)
			a.Umov(V8, aD, V1, 0)
			a.CmpImm(aD, 0, false)
			a.Cset(aD, EQ)
		})
	}
	// bitmask: each negative lane made its bit, and the bits summed.
	test(rd.SI8x16BitMask, "I8x16BitMask", func() {
		a.V2(VCmlt0, V8, V1, V0)
		g.vconst(V2, 0x8040201008040201, 0x8040201008040201)
		a.V3(VAnd, V8, V1, V1, V2)
		a.Ext(V2, V1, V1, 8)
		a.V3(VZip1, V8, V1, V1, V2)
		a.V2(VAddv, V16, V1, V1)
		a.Umov(V16, aD, V1, 0)
	})
	test(rd.SI16x8BitMask, "I16x8BitMask", func() {
		a.V2(VCmlt0, V16, V1, V0)
		g.vconst(V2, 0x0008000400020001, 0x0080004000200010)
		a.V3(VAnd, V8, V1, V1, V2)
		a.V2(VAddv, V16, V1, V1)
		a.Umov(V16, aD, V1, 0)
	})
	test(rd.SI32x4BitMask, "I32x4BitMask", func() {
		a.V2(VCmlt0, V32, V1, V0)
		g.vconst(V2, 0x0000000200000001, 0x0000000800000004)
		a.V3(VAnd, V8, V1, V1, V2)
		a.V2(VAddv, V32, V1, V1)
		a.Umov(V32, aD, V1, 0)
	})
	test(rd.SI64x2BitMask, "I64x2BitMask", func() {
		a.V2(VCmlt0, V64, V1, V0)
		g.vconst(V2, 1, 2)
		a.V3(VAnd, V8, V1, V1, V2)
		a.V3(VAddp, V64, V1, V1, V1)
		a.Umov(V64, aD, V1, 0)
	})

	// The shifts: A = B shifted by the i32 C modulo the lanes' width, by
	// SSHL or USHL of the count, negated to shift right.
	for _, s := range []struct {
		x            int
		name         string
		left, signed bool
	}{
		{rd.SI8x16Shl, "I8x16Shl", true, false}, {rd.SI8x16ShrS, "I8x16ShrS", false, true}, {rd.SI8x16ShrU, "I8x16ShrU", false, false},
		{rd.SI16x8Shl, "I16x8Shl", true, false}, {rd.SI16x8ShrS, "I16x8ShrS", false, true}, {rd.SI16x8ShrU, "I16x8ShrU", false, false},
		{rd.SI32x4Shl, "I32x4Shl", true, false}, {rd.SI32x4ShrS, "I32x4ShrS", false, true}, {rd.SI32x4ShrU, "I32x4ShrU", false, false},
		{rd.SI64x2Shl, "I64x2Shl", true, false}, {rd.SI64x2ShrS, "I64x2ShrS", false, true}, {rd.SI64x2ShrU, "I64x2ShrU", false, false},
	} {
		s := s
		sz := lanes(s.name)
		g.hs(s.x, s.name, func() {
			g.load(aD, 2)
			a.AndImm(aD, aD, uint64(8)<<sz-1, false)
			if !s.left {
				a.Neg(aD, aD, false)
			}
			a.Dup(sz, V1, aD)
			g.vload(V0, 1)
			op := VUshl
			if s.signed {
				op = VSshl
			}
			a.V3(op, sz, V0, V0, V1)
			g.vstore(0, V0)
			g.next(1)
		})
	}

	// extract_lane: A = lane C of B, read from its slot's bytes, an i32
	// with its upper half clear.
	for _, e := range []struct {
		x    int
		name string
		size int
	}{
		{rd.SExtractI8S, "ExtractI8S", -1}, {rd.SExtractI8U, "ExtractI8U", 1},
		{rd.SExtractI16S, "ExtractI16S", -2}, {rd.SExtractI16U, "ExtractI16U", 2},
		{rd.SExtract32, "Extract32", 4}, {rd.SExtract64, "Extract64", 8},
	} {
		e := e
		g.hs(e.x, e.name, func() {
			size := e.size
			if size < 0 {
				size = -size
			}
			g.addr(aS, 1)
			a.ShiftImm(Lsr, aD, aW, 48, true)
			sh := map[int]uint8{1: 0, 2: 1, 4: 2, 8: 3}[size]
			if sh > 0 {
				a.ShiftImm(Lsl, aD, aD, sh, true)
			}
			switch e.size {
			case -1:
				a.LoadS8Idx(aD, aS, aD, false)
			case 1:
				a.LoadU8Idx(aD, aS, aD)
			case -2:
				a.LoadS16Idx(aD, aS, aD, false)
			case 2:
				a.LoadU16Idx(aD, aS, aD)
			case 4:
				a.LoadU32Idx(aD, aS, aD)
			default:
				a.LoadIdx(aD, aS, aD, false)
			}
			g.store(0, aD)
			g.next(1)
		})
	}

	// replace_lane: A = B with lane (the word after) set to C.
	for _, e := range []struct {
		x    int
		name string
		s    VSize
	}{{rd.SReplace8, "Replace8", V8}, {rd.SReplace16, "Replace16", V16}, {rd.SReplace32, "Replace32", V32}, {rd.SReplace64, "Replace64", V64}} {
		e := e
		g.hs(e.x, e.name, func() {
			g.load(aD, 2)
			g.vload(V0, 1)
			g.addr(aC, 0)
			a.StoreQ(aC, 0, V0)
			a.Load(aS, aIP, 8)
			a.ShiftImm(Lsl, aS, aS, uint8(e.s), true)
			switch e.s {
			case V8:
				a.Store8Idx(aC, aS, aD)
			case V16:
				a.Store16Idx(aC, aS, aD)
			case V32:
				a.Store32Idx(aC, aS, aD)
			default:
				a.StoreIdx(aC, aS, aD)
			}
			g.next(2)
		})
	}

	// splat: A = the number B in every lane.
	for _, e := range []struct {
		x    int
		name string
		s    VSize
	}{{rd.SSplat8, "Splat8", V8}, {rd.SSplat16, "Splat16", V16}, {rd.SSplat32, "Splat32", V32}, {rd.SSplat64, "Splat64", V64}} {
		e := e
		g.hs(e.x, e.name, func() {
			g.load(aD, 1)
			a.Dup(e.s, V0, aD)
			g.vstore(0, V0)
			g.next(1)
		})
	}
}

// simdMemory lays out v128.load and v128.store of the second table, as
// amd64's.
func (g *armGen) simdMemory() {
	a := g.a
	for _, m := range []struct {
		x    int
		name string
		op   opcode.Opcode
		addr int
	}{{rd.XV128Load, "V128Load", opcode.S128LoadMem, 1}, {rd.XV128Store, "V128Store", opcode.S128StoreMem, 0}} {
		m := m
		g.hx(m.x, m.name, func() {
			a.Load(aT, aIP, 8)
			a.MovImm(aC, uint64(m.op)<<32)
			a.Cmp(aT, aC, true)
			a.BCond(NE, g.exit)
			a.Load(a8, aCtx, rd.CtxMem0)
			a.Cbz(a8, g.exit, true)
			a.LoadU8(aT, a8, rd.MemoryIs64)
			a.Cbnz(aT, g.exit, false)
			g.load32(aS, m.addr)
			a.Load(aT, aIP, 16)
			a.Op(Add, aS, aS, aT, true)
			a.AddImm(aC, aS, 16, true)
			a.Load(aT, a8, rd.MemoryBuf+rd.SliceLen)
			a.Cmp(aC, aT, true)
			a.BCond(HI, g.exit)
			a.Load(a8, a8, rd.MemoryBuf)
			if m.addr == 1 {
				a.LoadQIdx(V0, a8, aS)
				g.vstore(0, V0)
			} else {
				g.vload(V0, 1)
				a.StoreQIdx(a8, aS, V0)
			}
			g.next(3)
		})
	}
}
