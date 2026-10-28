package bignum

import "math/bits"

// Barrett division with a Newton inverse, for the longest divisors, as
// V8's div-barrett.cc divides from 13310 digits: the divisor's reciprocal,
// to its own precision, by Newton's iteration -- each step a reciprocal of
// half the precision and two products (Brent and Zimmermann, "Modern
// Computer Arithmetic", algorithm 3.5, ApproximateReciprocal) -- and each
// block of the quotient the product of the dividend's top half and the
// reciprocal, at most a few over, corrected by the remainder (algorithm
// 2.5, BarrettDivRem): two products of the divisor's size a block, where
// Burnikel and Ziegler's division is some products a level of its
// recursion.

// barrettThreshold is the words of divisor from which divLarge divides by
// divBarrett: measured, as V8's kBarrettThreshold (13310) is. A variable,
// which the tests lower.
var barrettThreshold = 13310

// barrettPays reports whether a division by a divisor of n words, for a
// quotient of m+1, is made in fewer cycles by Barrett's method than by
// Burnikel and Ziegler's: from barrettThreshold for a quotient of the
// divisor's length, from half that for one of twice, and a quarter for
// one of three times, its reciprocal made once for more blocks. Measured
// in cycles on a Zen 2, quotients by Barrett's method: of 2n words by n,
// 1.44 the time at 4000 words, 0.96 at 13000, 0.77 at 100000; of 4n by
// n, 0.89 at 4000, 0.75 at 8000, 0.5 at 100000.
func barrettPays(n, m int) bool {
	steps := (m + 1) / n
	return n >= barrettThreshold || steps >= 2 && 2*n >= barrettThreshold || steps >= 3 && 4*n >= barrettThreshold
}

// newtonThreshold is the words of divisor below which invert divides for
// the reciprocal, as V8's kNewtonInversionThreshold (50) is.
var newtonThreshold = 50

// invert writes to z, n+1 words, the reciprocal X of v, n words, its top
// bit set: v*X < 2^(2nW) <= v*(X+2), X's top word 1. Below newtonThreshold
// it is (2^(2nW)-1)/v, exactly; above, Newton's step from the reciprocal
// X_h of v's top h words, of half the precision: T = v*X_h, made below
// 2^((n+h)W); X = X_h*2^(lW) + X_h*((2^((n+h)W) - T) >> lW) >> ((2h-l)W).
// A reciprocal p stops leaves z undefined.
func invert(z, v []uint, p *proc) {
	n := len(v)
	defer p.release(p.mark())
	if n < newtonThreshold {
		u := p.alloc(2 * n)
		for i := range u {
			u[i] = ^uint(0)
		}
		clear(z)
		divMod(z, nil, u, v, p)
		return
	}
	l := (n - 1) / 2
	h := n - l
	xh := p.alloc(h + 1)
	invert(xh, v[l:], p)
	if p.stopped() {
		return
	}
	// T = v*X_h, n+h+1 words, less v while it is 2^((n+h)W) or more.
	t := p.alloc(n + h + 1)
	mul(t, v, xh, p)
	if p.stopped() {
		return
	}
	for t[n+h] != 0 {
		subWordAt(xh, 1)
		if subVV(t[:n], t[:n], v) != 0 {
			subWordAtBorrow(t[n:], 1)
		}
	}
	// T = 2^((n+h)W) - T, below 3v: n+1 words.
	t = t[:n+h]
	for i := range t {
		t[i] = ^t[i]
	}
	addWordAt(t, 1)
	tm := Norm(t[l : n+1])
	// U = T_m * X_h, 2h+2 words; X = X_h*2^(lW) + U >> ((2h-l)W).
	clear(z)
	copy(z[l:], xh)
	if len(tm) > 0 {
		u := p.alloc(len(tm) + h + 1)
		mul(u, xh, tm, p)
		if p.stopped() {
			return
		}
		if un := Norm(u); len(un) > 2*h-l {
			addAt(z, un[2*h-l:], 0)
		}
	}
}

// divBarrett is DivMod of x by y, of n words, for a quotient of at least
// n/2 words, by Barrett's method: x and y shifted so that y's top bit is
// set, x in blocks of n words, each step's two blocks -- the remainder so
// far and the next -- divided by the reciprocal inv, n+1 words, which is
// made if it is nil. p's account kept.
func divBarrett(q, r, x, y, inv []uint, p *proc) ([]uint, []uint) {
	n := len(y)
	defer p.release(p.mark())
	sigma := uint(bits.LeadingZeros(y[n-1]))
	b := p.alloc(n)
	shlInto(b, y, sigma)
	if inv == nil {
		inv = p.alloc(n + 1)
		invert(inv, b, p)
		if p.stopped() {
			return nil, nil
		}
	}
	// t blocks of the dividend, a word more than it has, so that the top
	// block is below the divisor, whose top bit is set.
	la := len(x) + 1
	t := max((la+n-1)/n, 2)
	a := p.alloc(t * n)
	a[la-1] = shlInto(a[:la-1], x, sigma)
	clear(a[la:])
	qs := p.alloc((t - 1) * n)
	z := p.alloc(2 * n)
	ri := p.alloc(n)
	copy(z, a[(t-2)*n:t*n])
	for i := t - 2; i >= 0; i-- {
		qi := qs[i*n : (i+1)*n]
		if zn := Norm(z); i == t-2 && 2*(len(zn)-n+1) < n {
			// The top block's short quotient.
			clear(qi)
			clear(ri)
			divMod(qi, ri, zn, b, p)
		} else {
			barrettStep(qi, ri, z, b, inv, p)
		}
		if p.stopped() {
			return nil, nil
		}
		if i > 0 {
			copy(z[n:], ri)
			copy(z, a[(i-1)*n:i*n])
		}
	}
	if q != nil {
		q = q[:QuoRoom(len(x), n)]
		copy(q, qs)
		q = Norm(q)
	}
	if r != nil {
		r = Norm(Shr(r[:n], Norm(ri), sigma))
	}
	return q, r
}

// barrettStep writes a/b to q and the remainder to r, each n words, for b
// of n words, its top bit set, a of 2n below b*2^(nW), and inv b's
// reciprocal: the quotient guessed as a's top half times inv, its top n+1
// words of 2n+1, at most a few under, and corrected while the remainder
// is b or more.
func barrettStep(q, r, a, b, inv []uint, p *proc) {
	n := len(b)
	m := p.mark()
	defer p.release(m)
	a1 := Norm(a[n:])
	prod := p.alloc(2*n + 2)
	clear(q)
	if len(a1) > 0 {
		pr := mul(prod[:len(inv)+len(a1)], inv, a1, p)
		if p.stopped() {
			return
		}
		copy(q, Norm(pr[n:]))
	}
	// R = a - q*b, a few b at most.
	rem := p.alloc(2*n + 1)
	copy(rem, a)
	rem[2*n] = 0
	if qn := Norm(q); len(qn) > 0 {
		pr := Norm(mul(prod[:n+len(qn)], b, qn, p))
		if p.stopped() {
			return
		}
		if subVV(rem[:len(pr)], rem[:len(pr)], pr) != 0 && subWordAtBorrow(rem[len(pr):], 1) != 0 {
			// The guess is never over: the reciprocal is never.
			panic("bignum: a Barrett quotient over")
		}
	}
	rn := Norm(rem)
	for Cmp(rn, b) >= 0 {
		rn = Sub(rn, rn, b)
		addWordAt(q, 1)
	}
	clear(r)
	copy(r, rn)
}
