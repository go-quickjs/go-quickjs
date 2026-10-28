package bignum

import "math/bits"

// Burnikel–Ziegler division, for long divisors and quotients, as V8's
// div-burnikel.cc divides from 57 digits (Christoph Burnikel and Joachim
// Ziegler, "Fast Recursive Division", MPI-I-98-1-022, 1998): the dividend
// in blocks of the divisor's length n, each step's two blocks divided by
// the divisor (D2n1n), which halves it -- two divisions of three halves by
// two (D3n2n), each a division of two halves by one, recursively, and a
// product of halves -- so that a division costs some products of its
// size, not the schoolbook's n*m.

// burnikelThreshold is the words of divisor and quotient from which
// DivMod divides by divLarge: measured, as V8's kBurnikelThreshold (57) is.
// A variable, which the tests lower.
var burnikelThreshold = 57

// divLarge is DivMod of x by y, of n words at least burnikelThreshold, for
// x at least y and a quotient of at least burnikelThreshold words, p's
// account kept. A quotient shorter than the divisor is the quotient of
// their top words, at most two off; a longer one is Burnikel and
// Ziegler's.
func divLarge(q, r, x, y []uint, p *proc) ([]uint, []uint) {
	n, m := len(y), len(x)-len(y)
	if 2*(m+1) < n {
		return divShort(q, r, x, y, p)
	}
	defer p.release(p.mark())
	// The block size: nb = j*2^e, at least n, 2^e the least power of two
	// that halves it below the threshold.
	e := 1 << bits.Len(uint(n/burnikelThreshold))
	nb := (n + e - 1) / e * e
	// The divisor and the dividend shifted so that the divisor is nb
	// words, its top bit set.
	sigma := uint(bits.LeadingZeros(y[n-1]))
	ds := nb - n
	b := p.alloc(nb)
	clear(b[:ds])
	shlInto(b[ds:], y, sigma)
	la := len(x) + ds + 1
	// t blocks of the dividend, a word more than it has, so that its top
	// bit is zero, and its top block below the divisor's, which is set.
	t := max((la+nb-1)/nb, 2)
	a := p.alloc(t * nb)
	clear(a[:ds])
	a[la-1] = shlInto(a[ds:la-1], x, sigma)
	clear(a[la:])
	// Each step: Z = [R, A_i] divided by the divisor, its quotient the
	// quotient's block i.
	qs := p.alloc((t - 1) * nb)
	z := p.alloc(2 * nb)
	ri := p.alloc(nb)
	copy(z, a[(t-2)*nb:t*nb])
	for i := t - 2; i >= 0; i-- {
		if zn := Norm(z); i == t-2 && 2*(len(zn)-nb+1) < nb {
			// The top block, a word or a few of the shifted dividend's,
			// whose quotient is as short: a short quotient's division,
			// not a whole block's.
			clear(qs[i*nb : (i+1)*nb])
			clear(ri)
			divMod(qs[i*nb:(i+1)*nb], ri, zn, b, p)
		} else {
			d2n1n(qs[i*nb:(i+1)*nb], ri, z, b, p)
		}
		if p.stopped() {
			return nil, nil
		}
		if i > 0 {
			copy(z[nb:], ri)
			copy(z, a[(i-1)*nb:i*nb])
		}
	}
	if q != nil {
		q = q[:QuoRoom(len(x), n)]
		copy(q, qs)
		q = Norm(q)
	}
	if r != nil {
		r = Norm(Shr(r[:n], ri, uint(ds*W)+sigma))
	}
	return q, r
}

// divShort is divLarge of a quotient of q words, shorter than half the
// divisor's n: the quotient of x's top 2q+1 words by y's top q+1, the low
// n-q-1 of each dropped, is the quotient or one off either way -- the
// dividend's dropped words take away at most one, and the divisor's add
// at most one more, the divisor above 2^(W(n-1)) -- corrected by the
// remainder, x less the quotient times y.
func divShort(q, r, x, y []uint, p *proc) ([]uint, []uint) {
	n, m := len(y), len(x)-len(y)
	drop := n - m - 2
	defer p.release(p.mark())
	qq := p.alloc(QuoRoom(len(x)-drop, n-drop) + 1)
	qt, _ := divMod(qq[:QuoRoom(len(x)-drop, n-drop)], nil, x[drop:], y[drop:], p)
	if p.stopped() {
		return nil, nil
	}
	// The remainder, x - qt*y, from below zero to under 3y: qt one more
	// than the quotient at most once less, and one less at most once more.
	prod := p.alloc(len(x) + 2)
	pr := prod[:0]
	if len(qt) > 0 {
		pr = Norm(mul(prod[:len(y)+len(qt)], y, qt, p))
		if p.stopped() {
			return nil, nil
		}
	}
	for Cmp(pr, x) > 0 {
		qt = SubWord(qt, qt, 1)
		pr = Sub(prod, pr, y)
	}
	rem := Sub(p.alloc(len(x)), x, pr)
	for Cmp(rem, y) >= 0 {
		qt = qq[:len(qt)+1]
		qt = AddWord(qt, qt[:len(qt)-1], 1)
		rem = Sub(rem, rem, y)
	}
	if q != nil {
		q = q[:len(qt)]
		copy(q, qt)
	}
	if r != nil {
		r = r[:len(rem)]
		copy(r, rem)
	}
	return q, r
}

// d2n1n writes a/b to q and the remainder to r, each n words, for b of n
// words, its top bit set, and a of 2n, below b*2^(nW): Burnikel and
// Ziegler's algorithm 1. An odd or short b divides schoolbook. A division
// p stops leaves q and r undefined.
func d2n1n(q, r, a, b []uint, p *proc) {
	n := len(b)
	if n&1 != 0 || n < burnikelThreshold {
		m := p.mark()
		u := p.alloc(2 * n)
		copy(u, a)
		divSchool(q, u, b)
		copy(r, u[:n])
		p.release(m)
		p.spend(n * n)
		return
	}
	h := n / 2
	m := p.mark()
	r1 := p.alloc(n)
	// The high half of the quotient, of a's top three halves; the low, of
	// the remainder's two and a's last.
	d3n2n(q[h:], r1, a[h:], b, p)
	if p.stopped() {
		return
	}
	t := p.alloc(3 * h)
	copy(t, a[:h])
	copy(t[h:], r1)
	d3n2n(q[:h], r, t, b, p)
	p.release(m)
}

// d3n2n writes a/b to q, h words, and the remainder to r, 2h, for b of 2h
// words, its top bit set, and a of 3h, below b*2^(hW): Burnikel and
// Ziegler's algorithm 2. With a = [A1 A2 A3] and b = [B1 B2], halves from
// the top, the quotient is guessed from [A1 A2]/B1, at most two over,
// and corrected by B2.
func d3n2n(q, r, a, b []uint, p *proc) {
	h := len(b) / 2
	a3, a12, a1 := a[:h], a[h:3*h], a[2*h:3*h]
	b2, b1 := b[:h], b[h:]
	m := p.mark()
	defer p.release(m)
	// R̂ = [R1, A3] - Qhat*B2, of 2h+1 words, and the borrow past its top.
	rh := p.alloc(2*h + 1)
	copy(rh[:h], a3)
	if cmpFixed(a1, b1) < 0 {
		r1 := rh[h : 2*h]
		d2n1n(q, r1, a12, b1, p)
		if p.stopped() {
			return
		}
		rh[2*h] = 0
	} else {
		// A1 is B1, the dividend below b*2^(hW): Qhat = 2^(hW)-1, R1 =
		// [A1 A2] - Qhat*B1 = A2 + B1.
		for i := range q {
			q[i] = ^uint(0)
		}
		rh[2*h] = addVV(rh[h:2*h], a12[:h], b1)
	}
	d := p.alloc(2 * h)
	clear(d)
	if qn, bn := Norm(q), Norm(b2); len(qn) > 0 && len(bn) > 0 {
		if len(qn) < len(bn) {
			qn, bn = bn, qn
		}
		mul(d[:len(qn)+len(bn)], qn, bn, p)
		if p.stopped() {
			return
		}
	}
	// R̂ less D, b added back while below zero: twice at most.
	borrow := subVV(rh[:2*h], rh[:2*h], d)
	top, under := bits.Sub(rh[2*h], 0, borrow)
	rh[2*h] = top
	for under != 0 {
		c := addVV(rh[:2*h], rh[:2*h], b)
		rh[2*h], c = bits.Add(rh[2*h], 0, c)
		if c != 0 {
			under = 0
		}
		subWordAt(q, 1)
	}
	copy(r, rh[:2*h])
}

// cmpFixed compares x and y, of the same length, zeros on top allowed.
func cmpFixed(x, y []uint) int {
	for i := len(x) - 1; i >= 0; i-- {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// divSchool writes u/v to q, len(u)-len(v) words, and leaves the remainder
// in u[:len(v)], for v of n words, at least two, its top bit set, and u's
// top n words below v: DivMod's long division, of operands it has already
// normalized.
func divSchool(q, u, v []uint) {
	n := len(v)
	m := len(u) - n - 1
	vn1, vn2 := v[n-1], v[n-2]
	rec := reciprocal(vn1)
	for j := m; j >= 0; j-- {
		qhat := ^uint(0)
		if ujn := u[j+n]; ujn != vn1 {
			var rhat uint
			qhat, rhat = divRec(ujn, u[j+n-1], vn1, rec)
			hi, lo := bits.Mul(qhat, vn2)
			for hi > rhat || hi == rhat && lo > u[j+n-2] {
				qhat--
				prev := rhat
				rhat += vn1
				if rhat < prev {
					break
				}
				hi, lo = bits.Mul(qhat, vn2)
			}
		}
		c := mulSubRow(u[j:j+n], v, qhat)
		top := u[j+n]
		u[j+n] = top - c
		if top < c {
			u[j+n] += addVV(u[j:j+n], u[j:j+n], v)
			qhat--
		}
		q[j] = qhat
	}
}
