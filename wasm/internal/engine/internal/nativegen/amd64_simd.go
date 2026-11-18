package main

import (
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD operators: SSE's packed instructions over a v128's two slots,
// for the operators whose instruction gives exactly what the operator's
// Go function does. rSIMD's handler dispatches through the context's SIMD
// table, which the engine gives these handlers only where the CPU has
// SSSE3, SSE4.1 and SSE4.2, and the exit stub's elsewhere.

// The SSE registers the SIMD handlers use beyond X0 and X1, their scratch.
const (
	X2 XReg = 2
	X3 XReg = 3
	X4 XReg = 4
	X5 XReg = 5
)

// hs lays out the handler of operation x of rSIMD's table, named name.
func (g *gen) hs(x int, name string, body func()) {
	if _, dup := g.simdLabels[x]; dup {
		panic("nativegen: two handlers of " + name)
	}
	g.a.Align(16)
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.simdLabels[x], g.simdNames[x] = l, name
	body()
}

// loadV is x = the v128 in the slots field i names; R9 its scratch.
func (g *gen) loadV(x XReg, i int) {
	g.off(R9, i)
	g.a.LoadVecIdx(x, R12, R9, 0)
}

// storeV is the slots field i names = the v128 in x; R9 its scratch.
func (g *gen) storeV(i int, x XReg) {
	g.off(R9, i)
	g.a.StoreVecIdx(R12, R9, 0, x)
}

// ones is x = every bit set.
func (g *gen) ones(x XReg) { g.a.Vec(Pcmpeqd, x, x) }

// splatByte is x = b in every byte; RCX its scratch.
func (g *gen) splatByte(x XReg, b uint8) {
	g.a.MovImm(RCX, uint64(b)*0x0101010101010101)
	g.a.MovQToX(x, RCX)
	g.a.Vec(Punpcklqdq, x, x)
}

func (g *gen) simd() {
	a := g.a
	// A = B op C, or C op B where swap; the result negated where not.
	bin := func(x int, name string, op VOp, swap, not bool) {
		g.hs(x, name, func() {
			g.loadV(X0, 1)
			g.loadV(X1, 2)
			r := X0
			if swap {
				a.Vec(op, X1, X0)
				r = X1
			} else {
				a.Vec(op, X0, X1)
			}
			if not {
				g.ones(X2)
				a.Vec(Pxor, r, X2)
			}
			g.storeV(0, r)
			g.next(1)
		})
	}
	// A = B op C with an immediate (CMPPS, CMPPD), or C op B where swap.
	binImm := func(x int, name string, op VOp, imm uint8, swap bool) {
		g.hs(x, name, func() {
			g.loadV(X0, 1)
			g.loadV(X1, 2)
			r := X0
			if swap {
				a.VecImm(op, X1, X0, imm)
				r = X1
			} else {
				a.VecImm(op, X0, X1, imm)
			}
			g.storeV(0, r)
			g.next(1)
		})
	}
	for _, b := range []struct {
		x         int
		name      string
		op        VOp
		swap, not bool
	}{
		{rd.SS128And, "S128And", Pand, false, false},
		{rd.SS128Or, "S128Or", Por, false, false},
		{rd.SS128Xor, "S128Xor", Pxor, false, false},
		// B & ^C: PANDN negates its destination.
		{rd.SS128AndNot, "S128AndNot", Pandn, true, false},

		{rd.SI8x16Add, "I8x16Add", Paddb, false, false},
		{rd.SI8x16Sub, "I8x16Sub", Psubb, false, false},
		{rd.SI8x16AddSatS, "I8x16AddSatS", Paddsb, false, false},
		{rd.SI8x16AddSatU, "I8x16AddSatU", Paddusb, false, false},
		{rd.SI8x16SubSatS, "I8x16SubSatS", Psubsb, false, false},
		{rd.SI8x16SubSatU, "I8x16SubSatU", Psubusb, false, false},
		{rd.SI8x16MinS, "I8x16MinS", Pminsb, false, false},
		{rd.SI8x16MinU, "I8x16MinU", Pminub, false, false},
		{rd.SI8x16MaxS, "I8x16MaxS", Pmaxsb, false, false},
		{rd.SI8x16MaxU, "I8x16MaxU", Pmaxub, false, false},
		{rd.SI8x16RoundingAverageU, "I8x16RoundingAverageU", Pavgb, false, false},
		{rd.SI8x16SConvertI16x8, "I8x16SConvertI16x8", Packsswb, false, false},
		{rd.SI8x16UConvertI16x8, "I8x16UConvertI16x8", Packuswb, false, false},

		{rd.SI16x8Add, "I16x8Add", Paddw, false, false},
		{rd.SI16x8Sub, "I16x8Sub", Psubw, false, false},
		{rd.SI16x8AddSatS, "I16x8AddSatS", Paddsw, false, false},
		{rd.SI16x8AddSatU, "I16x8AddSatU", Paddusw, false, false},
		{rd.SI16x8SubSatS, "I16x8SubSatS", Psubsw, false, false},
		{rd.SI16x8SubSatU, "I16x8SubSatU", Psubusw, false, false},
		{rd.SI16x8Mul, "I16x8Mul", Pmullw, false, false},
		{rd.SI16x8MinS, "I16x8MinS", Pminsw, false, false},
		{rd.SI16x8MinU, "I16x8MinU", Pminuw, false, false},
		{rd.SI16x8MaxS, "I16x8MaxS", Pmaxsw, false, false},
		{rd.SI16x8MaxU, "I16x8MaxU", Pmaxuw, false, false},
		{rd.SI16x8RoundingAverageU, "I16x8RoundingAverageU", Pavgw, false, false},
		{rd.SI16x8SConvertI32x4, "I16x8SConvertI32x4", Packssdw, false, false},
		{rd.SI16x8UConvertI32x4, "I16x8UConvertI32x4", Packusdw, false, false},

		{rd.SI32x4Add, "I32x4Add", Paddd, false, false},
		{rd.SI32x4Sub, "I32x4Sub", Psubd, false, false},
		{rd.SI32x4Mul, "I32x4Mul", Pmulld, false, false},
		{rd.SI32x4MinS, "I32x4MinS", Pminsd, false, false},
		{rd.SI32x4MinU, "I32x4MinU", Pminud, false, false},
		{rd.SI32x4MaxS, "I32x4MaxS", Pmaxsd, false, false},
		{rd.SI32x4MaxU, "I32x4MaxU", Pmaxud, false, false},
		{rd.SI32x4DotI16x8S, "I32x4DotI16x8S", Pmaddwd, false, false},

		{rd.SI64x2Add, "I64x2Add", Paddq, false, false},
		{rd.SI64x2Sub, "I64x2Sub", Psubq, false, false},

		// Comparisons: eq and gt_s, swapped for lt_s, negated for ne,
		// le_s and ge_s.
		{rd.SI8x16Eq, "I8x16Eq", Pcmpeqb, false, false},
		{rd.SI8x16Ne, "I8x16Ne", Pcmpeqb, false, true},
		{rd.SI8x16GtS, "I8x16GtS", Pcmpgtb, false, false},
		{rd.SI8x16LtS, "I8x16LtS", Pcmpgtb, true, false},
		{rd.SI8x16LeS, "I8x16LeS", Pcmpgtb, false, true},
		{rd.SI8x16GeS, "I8x16GeS", Pcmpgtb, true, true},
		{rd.SI16x8Eq, "I16x8Eq", Pcmpeqw, false, false},
		{rd.SI16x8Ne, "I16x8Ne", Pcmpeqw, false, true},
		{rd.SI16x8GtS, "I16x8GtS", Pcmpgtw, false, false},
		{rd.SI16x8LtS, "I16x8LtS", Pcmpgtw, true, false},
		{rd.SI16x8LeS, "I16x8LeS", Pcmpgtw, false, true},
		{rd.SI16x8GeS, "I16x8GeS", Pcmpgtw, true, true},
		{rd.SI32x4Eq, "I32x4Eq", Pcmpeqd, false, false},
		{rd.SI32x4Ne, "I32x4Ne", Pcmpeqd, false, true},
		{rd.SI32x4GtS, "I32x4GtS", Pcmpgtd, false, false},
		{rd.SI32x4LtS, "I32x4LtS", Pcmpgtd, true, false},
		{rd.SI32x4LeS, "I32x4LeS", Pcmpgtd, false, true},
		{rd.SI32x4GeS, "I32x4GeS", Pcmpgtd, true, true},
		{rd.SI64x2Eq, "I64x2Eq", Pcmpeqq, false, false},
		{rd.SI64x2Ne, "I64x2Ne", Pcmpeqq, false, true},
		{rd.SI64x2GtS, "I64x2GtS", Pcmpgtq, false, false},
		{rd.SI64x2LtS, "I64x2LtS", Pcmpgtq, true, false},
		{rd.SI64x2LeS, "I64x2LeS", Pcmpgtq, false, true},
		{rd.SI64x2GeS, "I64x2GeS", Pcmpgtq, true, true},

		// The float arithmetic: of NaN operands the first's quieted, as
		// the scalar operators.
		{rd.SF32x4Add, "F32x4Add", Addps, false, false},
		{rd.SF32x4Sub, "F32x4Sub", Subps, false, false},
		{rd.SF32x4Mul, "F32x4Mul", Mulps, false, false},
		{rd.SF32x4Div, "F32x4Div", Divps, false, false},
		{rd.SF64x2Add, "F64x2Add", Addpd, false, false},
		{rd.SF64x2Sub, "F64x2Sub", Subpd, false, false},
		{rd.SF64x2Mul, "F64x2Mul", Mulpd, false, false},
		{rd.SF64x2Div, "F64x2Div", Divpd, false, false},
		// pmin is C < B ? C : B, and pmax C > B ? C : B: MINPS and MAXPS
		// with C first, which give their second operand where either is
		// NaN or both are zeros.
		{rd.SF32x4Pmin, "F32x4Pmin", Minps, true, false},
		{rd.SF32x4Pmax, "F32x4Pmax", Maxps, true, false},
		{rd.SF64x2Pmin, "F64x2Pmin", Minpd, true, false},
		{rd.SF64x2Pmax, "F64x2Pmax", Maxpd, true, false},
	} {
		bin(b.x, b.name, b.op, b.swap, b.not)
	}
	// The float comparisons: CMPPS's predicates eq (0), lt (1), le (2) and
	// neq (4, true of NaN), swapped for gt and ge.
	for _, f := range []struct {
		eq, ne, lt, gt, le, ge int
		name                   string
		op                     VOp
	}{
		{rd.SF32x4Eq, rd.SF32x4Ne, rd.SF32x4Lt, rd.SF32x4Gt, rd.SF32x4Le, rd.SF32x4Ge, "F32x4", Cmpps},
		{rd.SF64x2Eq, rd.SF64x2Ne, rd.SF64x2Lt, rd.SF64x2Gt, rd.SF64x2Le, rd.SF64x2Ge, "F64x2", Cmppd},
	} {
		binImm(f.eq, f.name+"Eq", f.op, 0, false)
		binImm(f.ne, f.name+"Ne", f.op, 4, false)
		binImm(f.lt, f.name+"Lt", f.op, 1, false)
		binImm(f.gt, f.name+"Gt", f.op, 1, true)
		binImm(f.le, f.name+"Le", f.op, 2, false)
		binImm(f.ge, f.name+"Ge", f.op, 2, true)
	}
	// The unsigned comparisons, of the lesser or greater: B <= C where
	// min(B, C) is B, B >= C where max(B, C) is B, negated for gt and lt.
	for _, u := range []struct {
		lt, gt, le, ge int
		name           string
		min, max, eq   VOp
	}{
		{rd.SI8x16LtU, rd.SI8x16GtU, rd.SI8x16LeU, rd.SI8x16GeU, "I8x16", Pminub, Pmaxub, Pcmpeqb},
		{rd.SI16x8LtU, rd.SI16x8GtU, rd.SI16x8LeU, rd.SI16x8GeU, "I16x8", Pminuw, Pmaxuw, Pcmpeqw},
		{rd.SI32x4LtU, rd.SI32x4GtU, rd.SI32x4LeU, rd.SI32x4GeU, "I32x4", Pminud, Pmaxud, Pcmpeqd},
	} {
		for _, c := range []struct {
			x        int
			name     string
			op       VOp
			negation bool
		}{{u.le, "LeU", u.min, false}, {u.gt, "GtU", u.min, true}, {u.ge, "GeU", u.max, false}, {u.lt, "LtU", u.max, true}} {
			g.hs(c.x, u.name+c.name, func() {
				g.loadV(X0, 1)
				g.loadV(X1, 2)
				a.Vec(c.op, X1, X0)
				a.Vec(u.eq, X0, X1)
				if c.negation {
					g.ones(X2)
					a.Vec(Pxor, X0, X2)
				}
				g.storeV(0, X0)
				g.next(1)
			})
		}
	}

	// A = op B.
	un := func(x int, name string, body func()) {
		g.hs(x, name, func() {
			g.loadV(X0, 1)
			body()
			g.storeV(0, X0)
			g.next(1)
		})
	}
	un(rd.SS128Not, "S128Not", func() {
		g.ones(X1)
		a.Vec(Pxor, X0, X1)
	})
	for _, b := range []struct {
		x    int
		name string
		op   VOp
	}{
		{rd.SI8x16Abs, "I8x16Abs", Pabsb},
		{rd.SI16x8Abs, "I16x8Abs", Pabsw},
		{rd.SI32x4Abs, "I32x4Abs", Pabsd},
		{rd.SF32x4Sqrt, "F32x4Sqrt", Sqrtps},
		{rd.SF64x2Sqrt, "F64x2Sqrt", Sqrtpd},
		{rd.SI16x8SConvertI8x16Low, "I16x8SConvertI8x16Low", Pmovsxbw},
		{rd.SI16x8UConvertI8x16Low, "I16x8UConvertI8x16Low", Pmovzxbw},
		{rd.SI32x4SConvertI16x8Low, "I32x4SConvertI16x8Low", Pmovsxwd},
		{rd.SI32x4UConvertI16x8Low, "I32x4UConvertI16x8Low", Pmovzxwd},
		{rd.SI64x2SConvertI32x4Low, "I64x2SConvertI32x4Low", Pmovsxdq},
		{rd.SI64x2UConvertI32x4Low, "I64x2UConvertI32x4Low", Pmovzxdq},
	} {
		op := b.op
		un(b.x, b.name, func() { a.Vec(op, X0, X0) })
	}
	// The high halves' extensions: the high quadword moved down first.
	for _, b := range []struct {
		x    int
		name string
		op   VOp
	}{
		{rd.SI16x8SConvertI8x16High, "I16x8SConvertI8x16High", Pmovsxbw},
		{rd.SI16x8UConvertI8x16High, "I16x8UConvertI8x16High", Pmovzxbw},
		{rd.SI32x4SConvertI16x8High, "I32x4SConvertI16x8High", Pmovsxwd},
		{rd.SI32x4UConvertI16x8High, "I32x4UConvertI16x8High", Pmovzxwd},
		{rd.SI64x2SConvertI32x4High, "I64x2SConvertI32x4High", Pmovsxdq},
		{rd.SI64x2UConvertI32x4High, "I64x2UConvertI32x4High", Pmovzxdq},
	} {
		op := b.op
		un(b.x, b.name, func() {
			a.VecImm(Pshufd, X0, X0, 0xEE)
			a.Vec(op, X0, X0)
		})
	}
	// neg is 0 - B.
	for _, b := range []struct {
		x    int
		name string
		op   VOp
	}{{rd.SI8x16Neg, "I8x16Neg", Psubb}, {rd.SI16x8Neg, "I16x8Neg", Psubw}, {rd.SI32x4Neg, "I32x4Neg", Psubd}, {rd.SI64x2Neg, "I64x2Neg", Psubq}} {
		op := b.op
		g.hs(b.x, b.name, func() {
			g.loadV(X1, 1)
			a.Vec(Pxor, X0, X0)
			a.Vec(op, X0, X1)
			g.storeV(0, X0)
			g.next(1)
		})
	}
	// The floats' abs and neg: the sign bits alone, which keeps a NaN's
	// payload.
	for _, b := range []struct {
		abs, neg int
		name     string
		right    VShift
		left     VShift
		bits     uint8
	}{{rd.SF32x4Abs, rd.SF32x4Neg, "F32x4", PsrldImm, PslldImm, 32}, {rd.SF64x2Abs, rd.SF64x2Neg, "F64x2", PsrlqImm, PsllqImm, 64}} {
		b := b
		un(b.abs, b.name+"Abs", func() {
			g.ones(X1)
			a.VecShift(b.right, X1, 1)
			a.Vec(Pand, X0, X1)
		})
		un(b.neg, b.name+"Neg", func() {
			g.ones(X1)
			a.VecShift(b.left, X1, b.bits-1)
			a.Vec(Pxor, X0, X1)
		})
	}
	// The roundings: ROUNDPS's modes, the precision exception masked.
	for _, r := range []struct {
		x    int
		name string
		op   VOp
		mode uint8
	}{
		{rd.SF32x4NearestInt, "F32x4NearestInt", Roundps, 0}, {rd.SF32x4Floor, "F32x4Floor", Roundps, 1},
		{rd.SF32x4Ceil, "F32x4Ceil", Roundps, 2}, {rd.SF32x4Trunc, "F32x4Trunc", Roundps, 3},
		{rd.SF64x2NearestInt, "F64x2NearestInt", Roundpd, 0}, {rd.SF64x2Floor, "F64x2Floor", Roundpd, 1},
		{rd.SF64x2Ceil, "F64x2Ceil", Roundpd, 2}, {rd.SF64x2Trunc, "F64x2Trunc", Roundpd, 3},
	} {
		r := r
		un(r.x, r.name, func() { a.VecImm(r.op, X0, X0, 8|r.mode) })
	}

	// The tests: i32 A = op B.
	test := func(x int, name string, body func()) {
		g.hs(x, name, func() {
			g.loadV(X0, 1)
			body()
			g.store(0, RDX)
			g.next(1)
		})
	}
	test(rd.SV128AnyTrue, "V128AnyTrue", func() {
		a.Op(Xor, RDX, RDX, false)
		a.Vec(Ptest, X0, X0)
		a.Setcc(CondNE, RDX)
	})
	for _, b := range []struct {
		x    int
		name string
		eq   VOp
	}{{rd.SI8x16AllTrue, "I8x16AllTrue", Pcmpeqb}, {rd.SI16x8AllTrue, "I16x8AllTrue", Pcmpeqw}, {rd.SI32x4AllTrue, "I32x4AllTrue", Pcmpeqd}, {rd.SI64x2AllTrue, "I64x2AllTrue", Pcmpeqq}} {
		eq := b.eq
		test(b.x, b.name, func() {
			// No lane equal to zero.
			a.Vec(Pxor, X1, X1)
			a.Vec(eq, X1, X0)
			a.Op(Xor, RDX, RDX, false)
			a.Vec(Ptest, X1, X1)
			a.Setcc(CondE, RDX)
		})
	}
	test(rd.SI8x16BitMask, "I8x16BitMask", func() { a.VecMask(Pmovmskb, RDX, X0) })
	test(rd.SI16x8BitMask, "I16x8BitMask", func() {
		a.Vec(Packsswb, X0, X0)
		a.VecMask(Pmovmskb, RDX, X0)
		a.MovZX8(RDX, RDX)
	})
	test(rd.SI32x4BitMask, "I32x4BitMask", func() { a.VecMask(Movmskps, RDX, X0) })
	test(rd.SI64x2BitMask, "I64x2BitMask", func() { a.VecMask(Movmskpd, RDX, X0) })

	// The shifts: A = B shifted by the i32 C modulo the lane's width.
	for _, s := range []struct {
		x    int
		name string
		op   VOp
		mask int32
	}{
		{rd.SI16x8Shl, "I16x8Shl", Psllw, 15}, {rd.SI16x8ShrS, "I16x8ShrS", Psraw, 15}, {rd.SI16x8ShrU, "I16x8ShrU", Psrlw, 15},
		{rd.SI32x4Shl, "I32x4Shl", Pslld, 31}, {rd.SI32x4ShrS, "I32x4ShrS", Psrad, 31}, {rd.SI32x4ShrU, "I32x4ShrU", Psrld, 31},
		{rd.SI64x2Shl, "I64x2Shl", Psllq, 63}, {rd.SI64x2ShrU, "I64x2ShrU", Psrlq, 63},
	} {
		s := s
		g.hs(s.x, s.name, func() {
			g.load(RDX, 2)
			a.OpImm(And, RDX, s.mask, false)
			a.MovQToX(X1, RDX)
			g.loadV(X0, 1)
			a.Vec(s.op, X0, X1)
			g.storeV(0, X0)
			g.next(1)
		})
	}

	// bitselect: A = B & D | C & ^D, D's slot field in the word after.
	g.hs(rd.SS128Select, "S128Select", func() {
		g.loadV(X0, 1)
		g.loadV(X1, 2)
		a.LoadU16(R9, RBX, 8)
		a.ShiftImm(Shl, R9, 3, true)
		a.LoadVecIdx(X2, R12, R9, 0)
		a.Vec(Pand, X0, X2)
		a.Vec(Pandn, X2, X1)
		a.Vec(Por, X0, X2)
		g.storeV(0, X0)
		g.next(2)
	})

	// swizzle: B's byte C[i], or 0 where C[i] is past 15: PSHUFB's mask
	// with the top bit set past 15 by a saturating add of 0x70.
	g.hs(rd.SI8x16Swizzle, "I8x16Swizzle", func() {
		g.loadV(X0, 1)
		g.loadV(X1, 2)
		g.splatByte(X2, 0x70)
		a.Vec(Paddusb, X1, X2)
		a.Vec(Pshufb, X0, X1)
		g.storeV(0, X0)
		g.next(1)
	})

	// shuffle: A[i] = (B ++ C)[k] for the 16 lanes k in the two words
	// after, below 32: B's bytes by a mask whose lanes past 15 have their
	// top bit set, C's by one whose lanes below 16 have.
	g.hs(rd.SShuffle, "Shuffle", func() {
		g.loadV(X0, 1)
		g.loadV(X1, 2)
		a.LoadVec(X2, RBX, 8)
		g.splatByte(X3, 0x70)
		g.splatByte(X4, 0x10)
		a.Vec(Pxor, X4, X2)
		a.Vec(Paddusb, X2, X3)
		a.Vec(Paddusb, X4, X3)
		a.Vec(Pshufb, X0, X2)
		a.Vec(Pshufb, X1, X4)
		a.Vec(Por, X0, X1)
		g.storeV(0, X0)
		g.next(3)
	})

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
			g.off(RCX, 1)
			a.Op(Add, RCX, R12, true)
			a.MovRR(RDX, RAX)
			a.ShiftImm(Shr, RDX, 48, true)
			a.LeaIdx(RCX, RCX, RDX, uint8(size), 0)
			switch e.size {
			case -1:
				a.LoadS8(RDX, RCX, 0)
				a.MovRR32(RDX, RDX)
			case 1:
				a.LoadU8(RDX, RCX, 0)
			case -2:
				a.LoadS16(RDX, RCX, 0)
				a.MovRR32(RDX, RDX)
			case 2:
				a.LoadU16(RDX, RCX, 0)
			case 4:
				a.LoadU32(RDX, RCX, 0)
			default:
				a.Load(RDX, RCX, 0)
			}
			g.store(0, RDX)
			g.next(1)
		})
	}

	// replace_lane: A = B with lane (the word after) set to C.
	for _, e := range []struct {
		x    int
		name string
		size int
	}{{rd.SReplace8, "Replace8", 1}, {rd.SReplace16, "Replace16", 2}, {rd.SReplace32, "Replace32", 4}, {rd.SReplace64, "Replace64", 8}} {
		e := e
		g.hs(e.x, e.name, func() {
			g.load(RDX, 2)
			g.loadV(X0, 1)
			g.storeV(0, X0)
			g.off(RCX, 0)
			a.Op(Add, RCX, R12, true)
			a.Load(RSI, RBX, 8)
			a.LeaIdx(RCX, RCX, RSI, uint8(e.size), 0)
			switch e.size {
			case 1:
				a.Store8(RCX, 0, RDX)
			case 2:
				a.Store16(RCX, 0, RDX)
			case 4:
				a.Store32(RCX, 0, RDX)
			default:
				a.Store(RCX, 0, RDX)
			}
			g.next(2)
		})
	}

	// splat: A = the number B in every lane.
	g.hs(rd.SSplat8, "Splat8", func() {
		g.load(RDX, 1)
		a.MovDToX(X0, RDX)
		a.Vec(Pxor, X1, X1)
		a.Vec(Pshufb, X0, X1)
		g.storeV(0, X0)
		g.next(1)
	})
	g.hs(rd.SSplat16, "Splat16", func() {
		g.load(RDX, 1)
		a.MovDToX(X0, RDX)
		a.VecImm(Pshuflw, X0, X0, 0)
		a.Vec(Punpcklqdq, X0, X0)
		g.storeV(0, X0)
		g.next(1)
	})
	g.hs(rd.SSplat32, "Splat32", func() {
		g.load(RDX, 1)
		a.MovDToX(X0, RDX)
		a.VecImm(Pshufd, X0, X0, 0)
		g.storeV(0, X0)
		g.next(1)
	})
	g.hs(rd.SSplat64, "Splat64", func() {
		g.load(RDX, 1)
		a.MovQToX(X0, RDX)
		a.Vec(Punpcklqdq, X0, X0)
		g.storeV(0, X0)
		g.next(1)
	})
}

// simdMemory lays out v128.load and v128.store of the second table, where
// they are of memory 0, a 32-bit unshared one, and the plain 16 bytes:
// the word after holds the memory and the opcode, the next the offset.
// Any other memory, a load that extends, splats or zeroes, and an access
// out of bounds leave to Go.
func (g *gen) simdMemory() {
	a := g.a
	for _, m := range []struct {
		x    int
		name string
		op   opcode.Opcode
		addr int
	}{{rd.XV128Load, "V128Load", opcode.S128LoadMem, 1}, {rd.XV128Store, "V128Store", opcode.S128StoreMem, 0}} {
		m := m
		g.hx(m.x, m.name, func() {
			a.MovImm(RCX, uint64(m.op)<<32)
			a.CmpMem(RCX, RBX, 8, true)
			a.Jcc(CondNE, g.exit)
			a.Load(R8, RDI, rd.CtxMem0)
			a.Op(Test, R8, R8, true)
			a.Jcc(CondE, g.exit)
			a.TestMem8(R8, rd.MemoryIs64, 0xff)
			a.Jcc(CondNE, g.exit)
			g.off(R9, m.addr)
			a.LoadU32Idx(RSI, R12, R9, 0)
			a.OpMem(Add, RSI, RBX, 16, true)
			a.Lea(RCX, RSI, 16)
			a.CmpMem(RCX, R8, rd.MemoryBuf+rd.SliceLen, true)
			a.Jcc(CondA, g.exit)
			a.Load(R8, R8, rd.MemoryBuf)
			if m.addr == 1 {
				a.LoadVecIdx(X0, R8, RSI, 0)
				g.storeV(0, X0)
			} else {
				g.loadV(X0, 1)
				a.StoreVecIdx(R8, RSI, 0, X0)
			}
			g.next(3)
		})
	}
}
