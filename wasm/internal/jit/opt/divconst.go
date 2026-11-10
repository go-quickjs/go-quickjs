package opt

import (
	"math/bits"
)

// A division or remainder by a constant is made of multiplications, shifts
// and adds, which compute the same quotient: a division instruction takes
// tens of cycles where these take a few. fold calls divConst for each.
//
// An unsigned division of a w-bit x by d, 2^(p-1) < d < 2^p, is the high
// half of x times a magic number m, shifted right by s, where
// 2^(w+s) <= m*d <= 2^(w+s) + 2^s and m < 2^w (Granlund and Montgomery,
// "Division by Invariant Integers using Multiplication", 1994, theorem 4.2).
// Where no s below p has such an m, m is ceil(2^(w+p)/d) less 2^w, and the
// quotient (t + (x-t)/2) >> (p-1), t the high half of x*m. A signed
// division takes the magic number of Warren's "Hacker's Delight", 10-1,
// and adds 1 to a negative quotient. A divisor that is a power of 2 is a
// shift, an unsigned one of 2^(w-1) or more a comparison, and a remainder
// is the dividend less the quotient times the divisor.
//
// An i32's product is made in 64 bits, its high half the product shifted
// right by 32; an i64's high half is OpMulHU's or OpMulHS's.

// magicU is the multiplier m and shift s of the unsigned division of w-bit
// integers by d, which is neither 0 nor a power of 2 and below 2^(w-1);
// add says the quotient is (t + (x-t)>>1) >> (s-1), t the product's high
// half, else it is t >> s.
func magicU(d uint64, w uint) (m uint64, s uint, add bool) {
	p := uint(bits.Len64(d - 1))
	// 2^(w+s) / d for s below p: 2^s < d, so the quotient has w bits.
	for s := uint(0); s < p; s++ {
		var q, r uint64
		if w == 64 {
			q, r = bits.Div64(1<<s, 0, d)
		} else {
			q, r = (uint64(1)<<(w+s))/d, (uint64(1)<<(w+s))%d
		}
		m := q + 1
		// m*d - 2^(w+s) is d - r: it must be at most 2^s, and m below
		// 2^w.
		if (w == 64 && q == ^uint64(0)) || (w < 64 && m>>w != 0) {
			continue
		}
		if d-r <= uint64(1)<<s {
			return m, s, false
		}
	}
	// ceil(2^(w+p) / d) - 2^w = ceil((2^p - d) * 2^w / d), which has w
	// bits as 2^p - d < d.
	var q uint64
	if w == 64 {
		q, _ = bits.Div64((uint64(1)<<p)-d, 0, d)
	} else {
		q = (((uint64(1) << p) - d) << w) / d
	}
	return q + 1, p, true
}

// magicS is the multiplier m, a w-bit two's complement integer, and shift s
// of the signed division of w-bit integers by d, whose magnitude is at
// least 2 and not a power of 2 (Hacker's Delight, figure 10-1). The
// quotient is t >> s plus 1 if that is negative, t the signed product's
// high half, to which x is added where d > 0 and m < 0, and from which it is
// subtracted where d < 0 and m > 0.
func magicS(d int64, w uint) (m uint64, s uint) {
	mask := ^uint64(0) >> (64 - w)
	top := uint64(1) << (w - 1)
	ad := uint64(d)
	if d < 0 {
		ad = -ad
	}
	ad &= mask
	t := top + (uint64(d)&mask)>>(w-1)
	anc := t - 1 - t%ad
	p := w - 1
	q1, r1 := top/anc, top%anc
	q2, r2 := top/ad, top%ad
	for {
		p++
		q1, r1 = (2*q1)&mask, (2*r1)&mask
		if r1 >= anc {
			q1, r1 = (q1+1)&mask, (r1-anc)&mask
		}
		q2, r2 = (2*q2)&mask, (2*r2)&mask
		if r2 >= ad {
			q2, r2 = (q2+1)&mask, (r2-ad)&mask
		}
		delta := ad - r2
		if !(q1 < delta || (q1 == delta && r1 == 0)) {
			break
		}
	}
	m = (q2 + 1) & mask
	if d < 0 {
		m = -m & mask
	}
	return m, p - w
}

// divConst makes division or remainder v, the i-th value of its block, by
// a constant divisor of its second argument, the operations above, v the
// last of them; it returns the number of values put before v. A divisor
// that may trap -- 0, and -1 for a signed division -- and one fold made a
// copy of are left.
func divConst(f *Func, v *Value, i int) int {
	if v.Op != OpDivS && v.Op != OpDivU && v.Op != OpRemS && v.Op != OpRemU || v.Args[1].Op != OpConst {
		return 0
	}
	t := v.Type
	w := uint(32)
	if t == TI64 {
		w = 64
	}
	mask := ^uint64(0) >> (64 - w)
	d := v.Args[1].Aux & mask
	signed := v.Op == OpDivS || v.Op == OpRemS
	rem := v.Op == OpRemS || v.Op == OpRemU
	sd := int64(d<<(64-w)) >> (64 - w)
	switch {
	case d == 0:
		return 0
	case signed && sd == -1:
		if !rem {
			return 0
		}
		v.setConst(0)
		return 0
	case d == 1:
		if rem {
			v.setConst(0)
		} else {
			v.setCopy(v.Args[0])
		}
		return 0
	}
	e := expander{f: f, b: v.Block, t: t, w: w}
	x := v.Args[0]
	var q *Value
	switch {
	case !signed && d&(d-1) == 0:
		k := uint64(bits.TrailingZeros64(d))
		if rem {
			v.Op, v.Args = OpAnd, e.args(x, e.c(d-1))
		} else {
			v.Op, v.Args = OpShrU, e.args(x, e.c(k))
		}
		return e.put(v, i)
	case !signed && d >= uint64(1)<<(w-1):
		// The quotient is 0 or 1.
		ge := e.op(OpGeU, TI32, x, e.c(d))
		if rem {
			v.Op, v.Args = OpSelect, e.args(e.op(OpSub, t, x, e.c(d)), x, ge)
			return e.put(v, i)
		}
		if t == TI64 {
			v.Op, v.Args = OpExtendU, e.args(ge)
		} else {
			v.setCopy(ge)
		}
		return e.put(v, i)
	case !signed:
		q = e.divU(x, d)
	default:
		ad := uint64(sd)
		if sd < 0 {
			ad = -ad
		}
		ad &= mask
		if ad&(ad-1) == 0 {
			// x + (x < 0 ? |d|-1 : 0), shifted right by k: rounded
			// toward zero.
			k := uint64(bits.TrailingZeros64(ad))
			sign := x
			if k > 1 {
				sign = e.op(OpShrS, t, x, e.c(uint64(w-1)))
			}
			bias := e.op(OpShrU, t, sign, e.c(uint64(w)-k))
			sum := e.op(OpAdd, t, x, bias)
			if rem {
				// The bits of x+bias below 2^k, less the bias.
				v.Op, v.Args = OpSub, e.args(x, e.op(OpAnd, t, sum, e.c(-ad)))
				return e.put(v, i)
			}
			q = e.op(OpShrS, t, sum, e.c(k))
			if sd < 0 {
				q = e.op(OpSub, t, e.c(0), q)
			}
		} else {
			q = e.divS(x, sd)
		}
	}
	if rem {
		v.Op, v.Args = OpSub, e.args(x, e.op(OpMul, t, q, e.c(d)))
	} else {
		v.setCopy(q)
	}
	return e.put(v, i)
}

// expander makes the values a division by a constant becomes, in block b,
// of type t, w bits wide, to be put before the division.
type expander struct {
	f    *Func
	b    *Block
	t    Type
	w    uint
	made []*Value
}

// op makes a value of op and type t on args.
func (e *expander) op(op Op, t Type, args ...*Value) *Value {
	v := e.f.value(e.b, op, t, args)
	e.made = append(e.made, v)
	return v
}

// c makes the constant c of e's type, cK of type t.
func (e *expander) c(c uint64) *Value { return e.cT(e.t, c) }

func (e *expander) cT(t Type, c uint64) *Value {
	v := e.op(OpConst, t)
	if t == TI32 {
		c = uint64(uint32(c))
	}
	v.Aux = c
	return v
}

// args is a value's arguments, in the workspace.
func (e *expander) args(a ...*Value) []*Value { return append(e.f.ws.vals.take(len(a)), a...) }

// divU is x / d, unsigned, d neither 0, a power of 2 nor 2^(w-1) or more.
func (e *expander) divU(x *Value, d uint64) *Value {
	m, s, add := magicU(d, e.w)
	if e.t == TI32 {
		// The product of x and m, below 2^64; with add, t + x is below
		// 2^33, and the quotient (t + x) >> s.
		x64 := e.op(OpExtendU, TI64, x)
		p := e.op(OpMul, TI64, x64, e.cT(TI64, m))
		var q *Value
		if add {
			q = e.op(OpShrU, TI64, e.op(OpAdd, TI64, e.op(OpShrU, TI64, p, e.cT(TI64, 32)), x64), e.cT(TI64, uint64(s)))
		} else {
			q = e.op(OpShrU, TI64, p, e.cT(TI64, uint64(32+s)))
		}
		return e.op(OpWrap, TI32, q)
	}
	t := e.op(OpMulHU, TI64, x, e.c(m))
	if !add {
		if s == 0 {
			return t
		}
		return e.op(OpShrU, TI64, t, e.c(uint64(s)))
	}
	half := e.op(OpShrU, TI64, e.op(OpSub, TI64, x, t), e.c(1))
	return e.op(OpShrU, TI64, e.op(OpAdd, TI64, half, t), e.c(uint64(s-1)))
}

// divS is x / d, signed, |d| at least 2 and not a power of 2.
func (e *expander) divS(x *Value, d int64) *Value {
	m, s := magicS(d, e.w)
	if e.t == TI32 {
		// The magic number with the 2^32 the corrections add or take
		// away, times x, is within 2^63: shifted right by 32 + s it is
		// the quotient rounded down, plus 1 where it is negative.
		mm := int64(int32(uint32(m)))
		switch {
		case d > 0 && mm < 0:
			mm += 1 << 32
		case d < 0 && mm > 0:
			mm -= 1 << 32
		}
		p := e.op(OpMul, TI64, e.op(OpExtendS, TI64, x), e.cT(TI64, uint64(mm)))
		q := e.op(OpShrS, TI64, p, e.cT(TI64, uint64(32+s)))
		q = e.op(OpAdd, TI64, q, e.op(OpShrU, TI64, q, e.cT(TI64, 63)))
		return e.op(OpWrap, TI32, q)
	}
	q := e.op(OpMulHS, TI64, x, e.c(m))
	switch {
	case d > 0 && int64(m) < 0:
		q = e.op(OpAdd, TI64, q, x)
	case d < 0 && int64(m) > 0:
		q = e.op(OpSub, TI64, q, x)
	}
	if s > 0 {
		q = e.op(OpShrS, TI64, q, e.c(uint64(s)))
	}
	return e.op(OpAdd, TI64, q, e.op(OpShrU, TI64, q, e.c(63)))
}

// put puts the values made before v, the i-th value of its block, and
// returns their number.
func (e *expander) put(v *Value, i int) int {
	n := len(e.made)
	if n == 0 {
		return 0
	}
	b := e.b
	vs := appendA(&e.f.ws.vals, b.Values, e.made...)
	copy(vs[i+n:], vs[i:len(vs)-n])
	copy(vs[i:], e.made)
	b.Values = vs
	return n
}
