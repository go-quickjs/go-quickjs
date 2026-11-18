package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The float operators on arm64: the scalar instructions, whose NaNs are
// what the Go handlers give on arm64 (nan.go, nan_other.go): of NaN
// operands a signaling one ahead of a quiet one, of two of a kind the
// first, quieted; of none, the default NaN. FMIN and FMAX are min and max
// whole: they order -0 below +0 and pick a NaN by the same rule. A float
// compare's conditions are those FCMP leaves: MI for less, LS for less or
// equal, GT, GE, EQ, all false for an unordered pair, and NE true for one.

// loadFA puts the float in the slot field i names into v.
func (g *armGen) loadFA(v FReg, i int, wide bool) {
	g.field(a9, i)
	g.a.AddLsl(a9, aL, a9, 3)
	if wide {
		g.a.LoadF(v, a9, 0)
	} else {
		g.a.LoadS(v, a9, 0)
	}
}

// storeFA stores the float in v to the slot field i names, an f32's upper
// half clear.
func (g *armGen) storeFA(i int, v FReg, wide bool) {
	if wide {
		g.a.FMovFromF(aD, v)
	} else {
		g.a.FMovFromS(aD, v)
	}
	g.store(i, aD)
}

func (g *armGen) fcmp(x, y FReg, wide bool) {
	if wide {
		g.a.FCmp(x, y)
	} else {
		g.a.FCmpS(x, y)
	}
}

func (g *armGen) farith(op FOp, d, x, y FReg, wide bool) {
	if wide {
		g.a.FArith(op, d, x, y)
	} else {
		g.a.FArithS(op, d, x, y)
	}
}

func (g *armGen) floats() {
	for _, t := range []floatType{f32, f64} {
		g.floatType(t)
	}
	g.conversions()
}

func (g *armGen) floatType(t floatType) {
	a := g.a
	w := t.wide
	for _, b := range []struct {
		op   int
		name string
		f    FOp
	}{{t.add, "Add", FAdd}, {t.sub, "Sub", FSub}, {t.mul, "Mul", FMul}, {t.div, "Div", FDiv},
		{t.min, "Min", FMin}, {t.max, "Max", FMax}} {
		g.h(b.op, t.name+b.name, func() {
			g.loadFA(V0, 1, w)
			g.loadFA(V1, 2, w)
			g.farith(b.f, V0, V0, V1, w)
			g.storeFA(0, V0, w)
			g.next(1)
		})
	}
	g.h(t.sqrt, t.name+"Sqrt", func() {
		g.loadFA(V0, 1, w)
		if w {
			a.FSqrt(V0, V0)
		} else {
			a.FSqrtS(V0, V0)
		}
		g.storeFA(0, V0, w)
		g.next(1)
	})
	// abs and neg: the sign bit alone, which keeps a NaN's payload.
	sign := uint64(1) << 63
	if !w {
		sign = 1 << 31
	}
	g.h(t.abs, t.name+"Abs", func() {
		g.load(aS, 1)
		a.LogicImm(And, aD, aS, sign-1, w)
		g.store(0, aD)
		g.next(1)
	})
	g.h(t.neg, t.name+"Neg", func() {
		g.load(aS, 1)
		a.LogicImm(Eor, aD, aS, sign, w)
		g.store(0, aD)
		g.next(1)
	})
	for _, c := range []struct {
		op, br, not int
		name        string
		cond        Cond
	}{
		{t.eq, t.brEq, -1, "Eq", EQ}, {t.ne, t.brNe, -1, "Ne", NE},
		{t.lt, t.brLt, t.notLt, "Lt", MI}, {t.le, t.brLe, t.notLe, "Le", LS},
		{t.gt, t.brGt, t.notGt, "Gt", GT}, {t.ge, t.brGe, t.notGe, "Ge", GE},
	} {
		g.h(c.op, t.name+c.name, func() {
			g.loadFA(V0, 1, w)
			g.loadFA(V1, 2, w)
			g.fcmp(V0, V1, w)
			a.Cset(aD, c.cond)
			g.store(0, aD)
			g.next(1)
		})
		g.h(c.br, "Br"+t.name+c.name, func() { g.floatBranch(w, c.cond) })
		if c.not >= 0 {
			// The negation, true of an unordered pair too.
			g.h(c.not, "BrNot"+t.name+c.name, func() { g.floatBranch(w, c.cond.Invert()) })
		}
	}
}

// floatBranch is a compare-and-branch of the floats A and B, taken where
// cond holds after FCMP.
func (g *armGen) floatBranch(w bool, cond Cond) {
	a := g.a
	tk := a.NewLabel()
	g.loadFA(V0, 0, w)
	g.loadFA(V1, 1, w)
	g.fcmp(V0, V1, w)
	a.BCond(cond, tk)
	g.next(1)
	a.Bind(tk)
	g.immC(aC)
	g.take(aC)
}

// conversions lays out the conversions between floats and integers. SCVTF
// and UCVTF round as Go's conversions do; a truncation of NaN, or whose
// result is out of the type's range (FCVTZS saturates), leaves to Go,
// which traps, and so does one at the range's very edge.
func (g *armGen) conversions() {
	a := g.a
	unary := func(op int, name string, body func()) {
		g.h(op, name, func() {
			g.load(aS, 1)
			body()
			g.next(1)
		})
	}
	toFloat := func(op int, name string, src64, f64, unsigned bool) {
		unary(op, name, func() {
			a.Cvtf(V0, aS, !unsigned, src64, !f64)
			g.storeFA(0, V0, f64)
		})
	}
	toFloat(rd.F64SConvertI32, "F64SConvertI32", false, true, false)
	toFloat(rd.F64UConvertI32, "F64UConvertI32", false, true, true)
	toFloat(rd.F64SConvertI64, "F64SConvertI64", true, true, false)
	toFloat(rd.F64UConvertI64, "F64UConvertI64", true, true, true)
	toFloat(rd.F32SConvertI32, "F32SConvertI32", false, false, false)
	toFloat(rd.F32UConvertI32, "F32UConvertI32", false, false, true)
	toFloat(rd.F32SConvertI64, "F32SConvertI64", true, false, false)
	toFloat(rd.F32UConvertI64, "F32UConvertI64", true, false, true)
	unary(rd.F32ConvertF64, "F32ConvertF64", func() {
		a.FMovToF(V0, aS)
		a.FCvtToSingle(V1, V0)
		g.storeFA(0, V1, false)
	})
	unary(rd.F64ConvertF32, "F64ConvertF32", func() {
		a.FMovToS(V0, aS)
		a.FCvtFromSingle(V1, V0)
		g.storeFA(0, V1, true)
	})
	trunc := func(op int, name string, from64, to64, unsigned bool) {
		unary(op, name, func() {
			if from64 {
				a.FMovToF(V0, aS)
			} else {
				a.FMovToS(V0, aS)
			}
			g.fcmp(V0, V0, from64)
			a.BCond(VS, g.exit)
			a.Fcvtz(aD, V0, true, true, !from64)
			switch {
			case !to64 && !unsigned:
				a.Sxtw(aC, aD)
				a.Cmp(aC, aD, true)
				a.BCond(NE, g.exit)
				a.MovRR32(aD, aD)
			case !to64:
				a.ShiftImm(Lsr, aC, aD, 32, true)
				a.Cbnz(aC, g.exit, true)
			default:
				// The saturated results, and below 0 for unsigned.
				if unsigned {
					a.CmpImm(aD, 0, true)
					a.BCond(LT, g.exit)
				} else {
					a.MovImm(aC, 1<<63)
					a.Cmp(aD, aC, true)
					a.BCond(EQ, g.exit)
				}
				a.MovImm(aC, 1<<63-1)
				a.Cmp(aD, aC, true)
				a.BCond(EQ, g.exit)
			}
			g.store(0, aD)
		})
	}
	trunc(rd.I32SConvertF32, "I32SConvertF32", false, false, false)
	trunc(rd.I32SConvertF64, "I32SConvertF64", true, false, false)
	trunc(rd.I32UConvertF32, "I32UConvertF32", false, false, true)
	trunc(rd.I32UConvertF64, "I32UConvertF64", true, false, true)
	trunc(rd.I64SConvertF32, "I64SConvertF32", false, true, false)
	trunc(rd.I64SConvertF64, "I64SConvertF64", true, true, false)
	trunc(rd.I64UConvertF32, "I64UConvertF32", false, true, true)
	trunc(rd.I64UConvertF64, "I64UConvertF64", true, true, true)
}
