package bignum

import "math/bits"

// Toom-Cook 3: x and y of n words each split in three pieces of k words,
// x = x2*B^2 + x1*B + x0, B = 2^(W*k), as polynomials in B of degree two;
// their product, of degree four, made of its values at 0, 1, -1, -2 and
// infinity -- five products of pieces of a third of the operands, where
// Karatsuba's method takes three of halves -- and interpolated by
// Bodrato's sequence, as V8's mul-toom.cc and GMP's do (Marco Bodrato,
// "Towards Optimal Toom-Cook Multiplication for Univariate and
// Multivariate Polynomials in Characteristic 2 and 0", 2007).

// toomThreshold is the words of the shorter operand from which mul
// multiplies by Toom-Cook 3, where its five products of thirds and its
// interpolation's linear work cost less than Karatsuba's three products of
// halves: measured, as V8's kToomThreshold (193) is. Products of 200 to
// 350 words took the same either way, in cycles on a Zen 2; of 1000, 10%
// less by Toom-Cook, and of 4000, 23% less. A variable, which the tests
// lower.
var toomThreshold = 200

// snat is a signed magnitude: Toom-Cook's values at -1 and -2, and the
// interpolation's steps, may be below zero.
type snat struct {
	neg bool
	mag []uint
}

// sadd writes a+b to z, which has room for max(len(a.mag), len(b.mag))+1
// words and may be either's magnitude.
func sadd(z []uint, a, b snat) snat {
	if a.neg == b.neg {
		return snat{a.neg, Add(z, a.mag, b.mag)}
	}
	if Cmp(a.mag, b.mag) < 0 {
		a, b = b, a
	}
	d := Sub(z, a.mag, b.mag)
	return snat{a.neg && len(d) != 0, d}
}

// ssub writes a-b to z, as sadd.
func ssub(z []uint, a, b snat) snat {
	b.neg = !b.neg && len(b.mag) != 0
	return sadd(z, a, b)
}

// smul writes a*b to z, which has room for len(a.mag)+len(b.mag) words
// and overlaps neither, p's account kept.
func smul(z []uint, a, b snat, p *proc) snat {
	x, y := a.mag, b.mag
	if len(x) < len(y) {
		x, y = y, x
	}
	if len(y) == 0 {
		return snat{}
	}
	return snat{a.neg != b.neg, Norm(mul(z[:len(x)+len(y)], x, y, p))}
}

// ssqr writes a*a to z, which has room for 2*len(a.mag) words and overlaps
// it not, p's account kept.
func ssqr(z []uint, a snat, p *proc) snat {
	if len(a.mag) == 0 {
		return snat{}
	}
	return snat{false, sqrAny(z, a.mag, p)}
}

// toomMul writes x*y to z, len(x)+len(y) words, for len(x) >= len(y) >=
// toomThreshold: by toom3 where y has a third piece, which a shorter y's
// is not; a longer x in pieces of y's length, each piece's product by
// toom3, added in.
func toomMul(z, x, y []uint, p *proc) []uint {
	m := len(y)
	if 3*m > 2*len(x) {
		return toom3(z, x, y, false, p)
	}
	clear(z)
	defer p.release(p.mark())
	t := p.alloc(2 * m)
	for i := 0; i < len(x); i += m {
		px := x[i:min(i+m, len(x))]
		var pr []uint
		if len(px) == m {
			pr = toom3(t, px, y, false, p)
		} else {
			pr = mul(t[:len(px)+m], y, px, p)
		}
		if p.stopped() {
			return z
		}
		addAt(z, Norm(pr), i)
	}
	return z
}

// toom3 writes x*y to z, len(x)+len(y) words, for x of n words, at least
// toomThreshold, and y of at most n, zeros on top allowed: x*x for sq, y
// then x. A product p stops leaves z undefined.
func toom3(z, x, y []uint, sq bool, p *proc) []uint {
	n := len(x)
	k := (n + 2) / 3
	piece := func(v []uint, i int) snat {
		return snat{false, Norm(v[min(i*k, len(v)):min((i+1)*k, len(v))])}
	}
	x0, x1, x2 := piece(x, 0), piece(x, 1), piece(x, 2)
	// Room: the values, of k+3 words at most, five for each operand, and
	// the five products, of 2k+6 words.
	e := k + 3
	f := 2*k + 6
	defer p.release(p.mark())
	buf := p.alloc(10*e + 6*f)
	room := func(i int) []uint { return buf[i*e : (i+1)*e] }
	prod := func(i int) []uint { return buf[10*e+i*f : 10*e+(i+1)*f] }
	// The values at 1, -1 and -2: p0 = x0+x2, p(1) = p0+x1, p(-1) = p0-x1,
	// p(-2) = 2(p(-1)+x2) - x0.
	eval := func(v0, v1, v2 snat, at int) (snat, snat, snat) {
		p0 := sadd(room(at), v0, v2)
		p1 := sadd(room(at+1), p0, v1)
		pm1 := ssub(room(at+2), p0, v1)
		t := sadd(room(at+3), pm1, v2)
		t.mag = Shl(room(at+4), t.mag, 1)
		pm2 := ssub(room(at+4), t, v0)
		return p1, pm1, pm2
	}
	p1, pm1, pm2 := eval(x0, x1, x2, 0)
	var r0, r1, rm1, rm2, rinf snat
	if sq {
		r0 = ssqr(prod(0), x0, p)
		r1 = ssqr(prod(1), p1, p)
		rm1 = ssqr(prod(2), pm1, p)
		rm2 = ssqr(prod(3), pm2, p)
		rinf = ssqr(prod(4), x2, p)
	} else {
		y0, y1, y2 := piece(y, 0), piece(y, 1), piece(y, 2)
		q1, qm1, qm2 := eval(y0, y1, y2, 5)
		r0 = smul(prod(0), x0, y0, p)
		r1 = smul(prod(1), p1, q1, p)
		rm1 = smul(prod(2), pm1, qm1, p)
		rm2 = smul(prod(3), pm2, qm2, p)
		rinf = smul(prod(4), x2, y2, p)
	}
	if p.stopped() {
		return z
	}
	// Bodrato's interpolation, each step in the room of a value it no
	// longer needs:
	//	r3 = (r(-2) - r(1)) / 3
	//	r1 = (r(1) - r(-1)) / 2
	//	r2 = r(-1) - r(0)
	//	r3 = (r2 - r3) / 2 + 2 r(inf)
	//	r2 = r2 + r1 - r(inf)
	//	r1 = r1 - r3
	r3 := ssub(prod(3), rm2, r1)
	r3.mag = divExact3(r3.mag, r3.mag)
	r1 = ssub(prod(1), r1, rm1)
	r1.mag = Shr(r1.mag, r1.mag, 1)
	r2 := ssub(prod(2), rm1, r0)
	r3 = ssub(prod(3), r2, r3)
	r3.mag = Shr(r3.mag, r3.mag, 1)
	r3 = sadd(prod(3), r3, snat{false, Shl(prod(5), rinf.mag, 1)})
	r2 = sadd(prod(2), r2, r1)
	r2 = ssub(prod(2), r2, rinf)
	r1 = ssub(prod(1), r1, r3)
	// Each coefficient of the product is a sum of products of pieces, at
	// least zero: z = r0 + r1 B + r2 B^2 + r3 B^3 + r(inf) B^4.
	z = z[:len(x)+len(y)]
	clear(z)
	copy(z, r0.mag)
	copy(z[4*k:], rinf.mag)
	addAt(z, r1.mag, k)
	addAt(z, r2.mag, 2*k)
	addAt(z, r3.mag, 3*k)
	return z
}

// divExact3 writes x/3, x a multiple of 3, to z, which may be x: a word at
// a time from the bottom, each the word less what the ones below took,
// times 3's inverse modulo 2^W (Jebelean's exact division).
func divExact3(z, x []uint) []uint {
	const inv3 = ^uint(0)/3*2 + 1
	var c uint
	for i, xi := range x {
		s, b := bits.Sub(xi, c, 0)
		q := s * inv3
		z[i] = q
		hi, _ := bits.Mul(q, 3)
		c = hi + b
	}
	return Norm(z[:len(x)])
}
