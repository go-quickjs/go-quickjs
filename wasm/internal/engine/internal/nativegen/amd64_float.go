package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The float operators: SSE's scalar instructions, whose NaNs are what the
// Go handlers give on amd64 (nan.go, nan_x86.go): of NaN operands the first
// operand's quieted, else the second's, the operands in the operation's
// order; of none, the default NaN.

// floatType is a float type's operations.
type floatType struct {
	name                         string
	wide                         bool
	add, sub, mul, div, min, max int
	abs, neg, sqrt               int
	eq, ne, lt, gt, le, ge       int
	brEq, brNe, brLt, brGt, brLe int
	brGe, notLt, notGt, notLe    int
	notGe                        int
	quiet                        uint64
}

var f32 = floatType{name: "F32", add: rd.F32Add, sub: rd.F32Sub, mul: rd.F32Mul, div: rd.F32Div, min: rd.F32Min, max: rd.F32Max,
	abs: rd.F32Abs, neg: rd.F32Neg, sqrt: rd.F32Sqrt,
	eq: rd.F32Eq, ne: rd.F32Ne, lt: rd.F32Lt, gt: rd.F32Gt, le: rd.F32Le, ge: rd.F32Ge,
	brEq: rd.BrF32Eq, brNe: rd.BrF32Ne, brLt: rd.BrF32Lt, brGt: rd.BrF32Gt, brLe: rd.BrF32Le, brGe: rd.BrF32Ge,
	notLt: rd.BrNotF32Lt, notGt: rd.BrNotF32Gt, notLe: rd.BrNotF32Le, notGe: rd.BrNotF32Ge, quiet: 1 << 22}

var f64 = floatType{name: "F64", wide: true, add: rd.F64Add, sub: rd.F64Sub, mul: rd.F64Mul, div: rd.F64Div, min: rd.F64Min, max: rd.F64Max,
	abs: rd.F64Abs, neg: rd.F64Neg, sqrt: rd.F64Sqrt,
	eq: rd.F64Eq, ne: rd.F64Ne, lt: rd.F64Lt, gt: rd.F64Gt, le: rd.F64Le, ge: rd.F64Ge,
	brEq: rd.BrF64Eq, brNe: rd.BrF64Ne, brLt: rd.BrF64Lt, brGt: rd.BrF64Gt, brLe: rd.BrF64Le, brGe: rd.BrF64Ge,
	notLt: rd.BrNotF64Lt, notGt: rd.BrNotF64Gt, notLe: rd.BrNotF64Le, notGe: rd.BrNotF64Ge, quiet: 1 << 51}

// loadF puts the float in the slot field i names into x, its bits into r.
func (g *gen) loadF(x XReg, r Reg, i int, wide bool) {
	a := g.a
	if wide {
		g.load(r, i)
		a.MovQToX(x, r)
		return
	}
	g.off(R9, i)
	a.LoadU32Idx(r, R12, R9, 0)
	a.MovDToX(x, r)
}

// storeF stores the float in x to the slot field i names, an f32's upper
// half clear.
func (g *gen) storeF(i int, x XReg, wide bool) {
	if wide {
		g.a.MovQFromX(RDX, x)
	} else {
		g.a.MovDFromX(RDX, x)
	}
	g.store(i, RDX)
}

// ucomi compares x with y, setting the flags as UCOMISD and UCOMISS do.
func (g *gen) ucomi(x, y XReg, wide bool) {
	if wide {
		g.a.SSEOp(UcomiSD, x, y)
	} else {
		g.a.SSEOpS(UcomiSD, x, y)
	}
}

func (g *gen) sse(op SSE, x, y XReg, wide bool) {
	if wide {
		g.a.SSEOp(op, x, y)
	} else {
		g.a.SSEOpS(op, x, y)
	}
}

func (g *gen) floats() {
	for _, t := range []floatType{f32, f64} {
		g.floatType(t)
	}
	g.conversions()
}

func (g *gen) floatType(t floatType) {
	a := g.a
	w := t.wide
	for _, b := range []struct {
		op   int
		name string
		sse  SSE
	}{{t.add, "Add", AddSD}, {t.sub, "Sub", SubSD}, {t.mul, "Mul", MulSD}, {t.div, "Div", DivSD}} {
		g.h(b.op, t.name+b.name, func() {
			g.loadF(XM0, RDX, 1, w)
			g.loadF(XM1, RSI, 2, w)
			g.sse(b.sse, XM0, XM1, w)
			g.storeF(0, XM0, w)
			g.next(1)
		})
	}
	g.h(t.sqrt, t.name+"Sqrt", func() {
		g.loadF(XM0, RDX, 1, w)
		g.sse(SqrtSD, XM0, XM0, w)
		g.storeF(0, XM0, w)
		g.next(1)
	})
	// abs and neg: the sign bit alone, which keeps a NaN's payload.
	sign := func(op int, name string, alu ALU) {
		g.h(op, t.name+name, func() {
			g.load(RDX, 1)
			if w {
				m := uint64(1) << 63
				if alu == And {
					m = ^m
				}
				a.MovImm(RCX, m)
				a.Op(alu, RDX, RCX, true)
			} else {
				m := int32(-0x80000000)
				if alu == And {
					m = 0x7fffffff
				}
				a.OpImm(alu, RDX, m, false)
			}
			g.store(0, RDX)
			g.next(1)
		})
	}
	sign(t.abs, "Abs", And)
	sign(t.neg, "Neg", Xor)
	// min and max: of a NaN operand, pick's NaN (the first operand's
	// quieted, else the second's); of equal operands, their bits' OR (min)
	// or AND (max), so that -0 is below +0; else the lesser or greater.
	minmax := func(op int, name string, isMin bool) {
		g.h(op, t.name+name, func() {
			nan, eq, takeA, aNaN := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
			g.loadF(XM0, RDX, 1, w)
			g.loadF(XM1, RSI, 2, w)
			g.ucomi(XM0, XM1, w)
			a.Jcc(CondP, nan)
			a.Jcc(CondE, eq)
			if isMin {
				a.Jcc(CondB, takeA)
			} else {
				a.Jcc(CondA, takeA)
			}
			g.store(0, RSI)
			g.next(1)
			a.Bind(takeA)
			g.store(0, RDX)
			g.next(1)
			a.Bind(eq)
			if isMin {
				a.Op(Or, RDX, RSI, w)
			} else {
				a.Op(And, RDX, RSI, w)
			}
			g.store(0, RDX)
			g.next(1)
			a.Bind(nan)
			g.ucomi(XM0, XM0, w)
			a.Jcc(CondP, aNaN)
			a.MovRR(RDX, RSI)
			a.Bind(aNaN)
			if w {
				a.MovImm(RCX, t.quiet)
				a.Op(Or, RDX, RCX, true)
			} else {
				a.OpImm(Or, RDX, int32(t.quiet), false)
			}
			g.store(0, RDX)
			g.next(1)
		})
	}
	minmax(t.min, "Min", true)
	minmax(t.max, "Max", false)
	// The comparisons: lt and le as gt and ge of the operands swapped, so
	// that an unordered pair, which sets CF, never satisfies them.
	type cmp struct {
		op, br int
		name   string
		swap   bool
		cond   Cond
	}
	for _, c := range []cmp{
		{t.gt, t.brGt, "Gt", false, CondA}, {t.ge, t.brGe, "Ge", false, CondAE},
		{t.lt, t.brLt, "Lt", true, CondA}, {t.le, t.brLe, "Le", true, CondAE},
	} {
		g.h(c.op, t.name+c.name, func() {
			g.loadF(XM0, RDX, 1, w)
			g.loadF(XM1, RSI, 2, w)
			if c.swap {
				g.ucomi(XM1, XM0, w)
			} else {
				g.ucomi(XM0, XM1, w)
			}
			a.Setcc(c.cond, RDX)
			a.MovZX8(RDX, RDX)
			g.store(0, RDX)
			g.next(1)
		})
		g.h(c.br, "Br"+t.name+c.name, func() {
			g.floatBranch(w, c.swap, func(tk Label) { a.Jcc(c.cond, tk) })
		})
	}
	for _, c := range []cmp{
		{0, t.notGt, "Gt", false, CondBE}, {0, t.notGe, "Ge", false, CondB},
		{0, t.notLt, "Lt", true, CondBE}, {0, t.notLe, "Le", true, CondB},
	} {
		g.h(c.br, "BrNot"+t.name+c.name, func() {
			g.floatBranch(w, c.swap, func(tk Label) { a.Jcc(c.cond, tk) })
		})
	}
	g.h(t.eq, t.name+"Eq", func() {
		g.loadF(XM0, RDX, 1, w)
		g.loadF(XM1, RSI, 2, w)
		g.ucomi(XM0, XM1, w)
		a.Setcc(CondE, RDX)
		a.Setcc(CondNP, RCX)
		a.MovZX8(RDX, RDX)
		a.MovZX8(RCX, RCX)
		a.Op(And, RDX, RCX, false)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(t.ne, t.name+"Ne", func() {
		g.loadF(XM0, RDX, 1, w)
		g.loadF(XM1, RSI, 2, w)
		g.ucomi(XM0, XM1, w)
		a.Setcc(CondNE, RDX)
		a.Setcc(CondP, RCX)
		a.MovZX8(RDX, RDX)
		a.MovZX8(RCX, RCX)
		a.Op(Or, RDX, RCX, false)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(t.brEq, "Br"+t.name+"Eq", func() {
		g.floatBranch(w, false, func(tk Label) {
			no := a.NewLabel()
			a.Jcc(CondP, no)
			a.Jcc(CondE, tk)
			a.Bind(no)
		})
	})
	g.h(t.brNe, "Br"+t.name+"Ne", func() {
		g.floatBranch(w, false, func(tk Label) {
			a.Jcc(CondP, tk)
			a.Jcc(CondNE, tk)
		})
	})
}

// floatBranch is a compare-and-branch of the floats A and B (swapped if
// swap), taken where jump jumps to its label.
func (g *gen) floatBranch(w, swap bool, jump func(tk Label)) {
	a := g.a
	tk := a.NewLabel()
	g.loadF(XM0, RDX, 0, w)
	g.loadF(XM1, RSI, 1, w)
	if swap {
		g.ucomi(XM1, XM0, w)
	} else {
		g.ucomi(XM0, XM1, w)
	}
	jump(tk)
	g.next(1)
	a.Bind(tk)
	g.immC(RCX)
	g.take(RCX)
}

// conversions lays out the conversions between floats and integers. A
// truncation whose result is out of the instruction's range -- NaN,
// infinity, past the type's -- leaves to Go, which traps or, at the range's
// edge, gives it.
func (g *gen) conversions() {
	a := g.a
	unary := func(op int, name string, body func()) {
		g.h(op, name, func() {
			g.load(RSI, 1)
			body()
			g.next(1)
		})
	}
	toFloat := func(op int, name string, wide, src64, f64 bool, unsigned bool) {
		unary(op, name, func() {
			if unsigned && src64 {
				a.Op(Test, RSI, RSI, true)
				a.Jcc(CondS, g.exit)
			}
			a.SSEOp(XorPD, XM0, XM0)
			if f64 {
				a.Cvtsi2sd(XM0, RSI, src64 || unsigned)
			} else {
				a.Cvtsi2ss(XM0, RSI, src64 || unsigned)
			}
			g.storeF(0, XM0, f64)
		})
	}
	toFloat(rd.F64SConvertI32, "F64SConvertI32", true, false, true, false)
	toFloat(rd.F64UConvertI32, "F64UConvertI32", true, false, true, true)
	toFloat(rd.F64SConvertI64, "F64SConvertI64", true, true, true, false)
	toFloat(rd.F64UConvertI64, "F64UConvertI64", true, true, true, true)
	toFloat(rd.F32SConvertI32, "F32SConvertI32", false, false, false, false)
	toFloat(rd.F32UConvertI32, "F32UConvertI32", false, false, false, true)
	toFloat(rd.F32SConvertI64, "F32SConvertI64", false, true, false, false)
	toFloat(rd.F32UConvertI64, "F32UConvertI64", false, true, false, true)
	unary(rd.F32ConvertF64, "F32ConvertF64", func() {
		a.MovQToX(XM0, RSI)
		a.Cvtsd2ss(XM1, XM0)
		g.storeF(0, XM1, false)
	})
	unary(rd.F64ConvertF32, "F64ConvertF32", func() {
		a.MovDToX(XM0, RSI)
		a.Cvtss2sd(XM1, XM0)
		g.storeF(0, XM1, true)
	})
	trunc := func(op int, name string, from64, to64, unsigned bool) {
		unary(op, name, func() {
			if from64 {
				a.MovQToX(XM0, RSI)
			} else {
				a.MovDToX(XM0, RSI)
			}
			switch {
			case !to64 && !unsigned:
				// The 32-bit indefinite integer, 0x80000000.
				if from64 {
					a.Cvttsd2si32(RDX, XM0)
				} else {
					a.Cvttss2si(RDX, XM0, false)
				}
				a.OpImm(Cmp, RDX, -0x80000000, false)
				a.Jcc(CondE, g.exit)
			case to64 && !unsigned:
				// The 64-bit indefinite integer, 1<<63.
				g.cvtt64(from64)
				a.MovImm(RCX, 1<<63)
				a.Op(Cmp, RDX, RCX, true)
				a.Jcc(CondE, g.exit)
			case !to64:
				// Unsigned to 32 bits: the truncation to 64, which must be
				// below 2^32.
				g.cvtt64(from64)
				a.MovRR(RCX, RDX)
				a.ShiftImm(Shr, RCX, 32, true)
				a.Jcc(CondNE, g.exit)
			default:
				// Unsigned to 64 bits: below 2^63 here, Go's above.
				g.cvtt64(from64)
				a.Op(Test, RDX, RDX, true)
				a.Jcc(CondS, g.exit)
			}
			g.store(0, RDX)
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

// cvtt64 truncates XM0, a double if from64 else a float, to a 64-bit
// integer in RDX.
func (g *gen) cvtt64(from64 bool) {
	if from64 {
		g.a.Cvttsd2si(RDX, XM0)
	} else {
		g.a.Cvttss2si(RDX, XM0, true)
	}
}

// The SSE registers the float handlers use, their scratch.
const (
	XM0 XReg = 0
	XM1 XReg = 1
)
