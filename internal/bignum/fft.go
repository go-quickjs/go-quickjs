package bignum

import "math/bits"

// Schönhage–Strassen multiplication, for the largest operands, as V8's
// mul-fft.cc multiplies from 1500 digits (Schönhage and Strassen,
// "Schnelle Multiplikation großer Zahlen", 1971; the form here is Crandall
// and Pomerance's, "Prime Numbers", 9.5.6). The product's words are cut in
// 2^k pieces of sw words, x's in the low half of them and y's -- zeros
// above -- and the product is their cyclic convolution, each coefficient a
// sum of at most 2^k products of pieces, less than 2^(2*sw*W + k): so it is
// made modulo F = 2^K+1 for a K at least that, where 2 is a root of unity
// of order 2K, and a transform of length 2^k is additions, subtractions and
// shifts. Of the three transforms -- x's, y's and the product's -- each is
// k passes over the pieces; the 2^k products of the pieces' transforms,
// each modulo F, are made by mul, by this method again where they are
// large. An element of the ring is L+1 words, K = L*W, its value from 0 to
// 2^K, its top word 0 or 1.

// fftThreshold is the words of the shorter operand from which mul
// multiplies by fftMul: measured, as V8's kFftThreshold (1500) is. In
// cycles on a Zen 2, products of n words by n took 0.95 of Toom-Cook's at
// 2500, 0.81 at 4000, 0.70 at 6000, 0.50 at 20000 and 0.36 at 50000,
// and 1.07 to 1.2 at 2000 and 1500. A variable, which the tests lower.
var fftThreshold = 2500

// fftParams are a product's transforms' shape: 2^k pieces of sw words,
// modulo 2^K+1, K = L*W.
type fftParams struct {
	k, sw, L int
}

// fftChoose is the shape of a product of operands of n1 and n2 words that
// costs least, as its operations are counted: for each k, the fewest words
// sw a piece can have, the least L that holds a coefficient and that the
// root of unity of order 2^k, 2^(2K/2^k), is a whole shift for -- K a
// multiple of 2^(k-1) and of W -- three transforms of k passes over 2^k
// elements of L words, and 2^k products of L words.
func fftChoose(n1, n2 int) fftParams {
	t := n1 + n2
	best, bestCost := fftParams{}, int64(0)
	for k := 1; k < 30 && 1<<k <= t; k++ {
		n := 1 << k
		sw := (t + n - 1) / n
		for (n1+sw-1)/sw+(n2+sw-1)/sw-1 > n {
			sw++
		}
		align := max(W, n/2)
		bits := 2*sw*W + k
		kk := (bits + align - 1) / align * align
		l := kk / W
		cost := int64(n) * (9*int64(k)*int64(l) + mulCost(l+1))
		if best.L == 0 || cost < bestCost {
			best, bestCost = fftParams{k, sw, l}, cost
		}
	}
	return best
}

// mulCost is about how many word operations mul takes for a product of
// n words by n, by its methods' recurrences: what fftChoose weighs.
func mulCost(n int) int64 {
	f := int64(n)
	switch {
	case n < karatsubaThreshold:
		return f * f
	case n < toomThreshold:
		return 3*mulCost((n+1)/2) + 8*f
	}
	return 5*mulCost((n+2)/3) + 25*f
}

// fftMul writes x*y to z, len(x)+len(y) words, x*x for sq: the operands'
// pieces transformed, their transforms multiplied, the products'
// transformed back and added in at their places. A product p stops leaves
// z undefined.
func fftMul(z, x, y []uint, sq bool, p *proc) []uint {
	pr := fftChoose(len(x), len(y))
	k, sw, L := pr.k, pr.sw, pr.L
	if L+1 >= len(y) {
		// Products of the pieces no shorter than y: what thresholds lowered
		// for testing can ask for, which would never end.
		if sq {
			return toom3(z, x, x, true, p)
		}
		return toomMul(z, x, y, p)
	}
	n := 1 << k
	e := L + 1
	defer p.release(p.mark())
	a := p.alloc(n * e)
	fftSplit(a, x, sw, e)
	f := fftRing{L: L, K: L * W, t: p.alloc(e), s: p.alloc(L + 2), p: p}
	if f.forward(a, n) {
		return z
	}
	b := a
	if !sq {
		b = p.alloc(n * e)
		fftSplit(b, y, sw, e)
		if f.forward(b, n) {
			return z
		}
	}
	// The products of the transforms, element by element, modulo F.
	prod := p.alloc(2 * e)
	for i := 0; i < n; i++ {
		ai := a[i*e : (i+1)*e]
		u := Norm(ai)
		var v []uint
		if sq {
			v = u
		} else {
			v = Norm(b[i*e : (i+1)*e])
		}
		if len(u) < len(v) {
			u, v = v, u
		}
		if len(v) == 0 {
			clear(ai)
			continue
		}
		var t []uint
		if sq {
			t = sqrAny(prod, u, p)
		} else {
			t = Norm(mul(prod[:len(u)+len(v)], u, v, p))
		}
		if p.stopped() {
			return z
		}
		f.reduce(ai, t)
	}
	if f.inverse(a, n) {
		return z
	}
	// Each coefficient, 2^k times it, divided by 2^k -- times 2^(2K-k),
	// -2^(K-k) -- and added in at its place.
	clear(z)
	zero := p.alloc(e)
	clear(zero)
	for i := 0; i < n && i*sw < len(z); i++ {
		ai := a[i*e : (i+1)*e]
		f.shl(f.t, ai, f.K-k)
		f.sub(ai, zero, f.t)
		if c := Norm(ai[:L]); len(c) > 0 {
			addAt(z, c, i*sw)
		}
	}
	return z
}

// fftSplit writes x's pieces of sw words to a, element i's at a[i*e:], the
// rest of each element zeros.
func fftSplit(a, x []uint, sw, e int) {
	clear(a)
	for i := 0; i*sw < len(x); i++ {
		copy(a[i*e:i*e+sw], x[i*sw:min((i+1)*sw, len(x))])
	}
}

// fftRing is the ring of integers modulo F = 2^K+1, K = L*W, its elements
// L+1 words from 0 to F-1; scratch for its shifts, t of L+1 words and s of
// L+2; and the operation's proc.
type fftRing struct {
	L, K int
	t, s []uint
	p    *proc
}

// add writes a+b to z, which may be a or b.
func (f *fftRing) add(z, a, b []uint) {
	L := f.L
	addVV(z[:L+1], a[:L+1], b[:L+1])
	// Below 2^(K+1)+1: the top word's multiple of 2^K, which is -1,
	// taken off.
	if v := z[L]; v > 1 || v == 1 && !isZero(z[:L]) {
		z[L] = 0
		if subWordAt(z[:L], v) {
			// Below zero: F added, the wrapped value one short.
			z[L] = addWordAt(z[:L], 1)
		}
	}
}

// sub writes a-b to z, which may be a or b.
func (f *fftRing) sub(z, a, b []uint) {
	L := f.L
	if subVV(z[:L+1], a[:L+1], b[:L+1]) != 0 {
		// Below zero: F added, modulo the words' 2^(W(L+1)).
		z[L] += addWordAt(z[:L], 1) + 1
	}
}

// shl writes a*2^e to z, for e under K, z not a: a's bits from K-e up
// moved to the bottom, negated, which 2^K is -1 for.
func (f *fftRing) shl(z, a []uint, e int) {
	L := f.L
	q, r := e/W, uint(e%W)
	// The low L-q words, shifted to z's word q, the bits shifted past K
	// to be taken off.
	clear(z[:q])
	z[L] = 0
	var out uint
	if r == 0 {
		copy(z[q:L], a[:L-q])
	} else {
		out = shlInto(z[q:L], a[:L-q], r)
	}
	// The high q+1 words, shifted r bits, and out, taken off at the
	// bottom.
	s := f.s[:q+2]
	s[q+1] = shlInto(s[:q+1], a[L-q:L+1], r)
	if out != 0 {
		var c uint
		s[0], c = bits.Add(s[0], out, 0)
		for i := 1; c != 0; i++ {
			s[i], c = bits.Add(s[i], 0, c)
		}
	}
	s = Norm(s)
	if len(s) == 0 {
		return
	}
	b := subVV(z[:len(s)], z[:len(s)], s)
	if b != 0 {
		b = subWordAtBorrow(z[len(s):L+1], 1)
	}
	if b != 0 {
		z[L] += addWordAt(z[:L], 1) + 1
	}
}

// reduce writes t, below (2^K+1)^2, modulo F to z: its low K bits less the
// rest, which 2^K is -1 for.
func (f *fftRing) reduce(z, t []uint) {
	L := f.L
	clear(z)
	if len(t) <= L {
		copy(z, t)
		return
	}
	copy(z, t[:L])
	hi := t[L:]
	b := subVV(z[:len(hi)], z[:len(hi)], hi)
	if b != 0 {
		b = subWordAtBorrow(z[len(hi):L+1], 1)
	}
	if b != 0 {
		z[L] += addWordAt(z[:L], 1) + 1
	}
}

// forward transforms a's n elements in place, decimation in frequency,
// its result in bit-reversed order: at each pass, of pairs half apart,
// (u, v) becomes (u+v, (u-v)*w^j), w = 2^(K/half). It reports whether the
// operation stopped.
func (f *fftRing) forward(a []uint, n int) bool {
	e := f.L + 1
	for half := n / 2; half >= 1; half /= 2 {
		step := f.K / half
		for start := 0; start < n; start += 2 * half {
			for j := 0; j < half; j++ {
				u := a[(start+j)*e : (start+j+1)*e]
				v := a[(start+j+half)*e : (start+j+half+1)*e]
				f.sub(f.t, u, v)
				f.add(u, u, v)
				if j == 0 {
					copy(v, f.t)
				} else {
					f.shl(v, f.t, j*step)
				}
				if f.p.spend(3 * e) {
					return true
				}
			}
		}
	}
	return false
}

// inverse transforms a's n elements, in bit-reversed order, back in place,
// decimation in time, its result in order and 2^k times the inverse: at
// each pass (u, v) becomes (u+v*w^-j, u-v*w^-j), w^-j = 2^(2K-j*step),
// which is -2^(K-j*step). It reports whether the operation stopped.
func (f *fftRing) inverse(a []uint, n int) bool {
	e := f.L + 1
	for half := 1; half < n; half *= 2 {
		step := f.K / half
		for start := 0; start < n; start += 2 * half {
			for j := 0; j < half; j++ {
				u := a[(start+j)*e : (start+j+1)*e]
				v := a[(start+j+half)*e : (start+j+half+1)*e]
				if j == 0 {
					// w^0 = 1: u+v and u-v.
					copy(f.t, v)
					f.sub(v, u, f.t)
					f.add(u, u, f.t)
				} else {
					// v*w^-j = -s, s = v*2^(K-j*step): u-s and u+s.
					f.shl(f.t, v, f.K-j*step)
					f.add(v, u, f.t)
					f.sub(u, u, f.t)
				}
				if f.p.spend(3 * e) {
					return true
				}
			}
		}
	}
	return false
}

// isZero reports whether x's words are all zero.
func isZero(x []uint) bool {
	for _, w := range x {
		if w != 0 {
			return false
		}
	}
	return true
}

// subWordAt subtracts w from z in place and reports whether it borrowed
// past the top.
func subWordAt(z []uint, w uint) bool {
	var b uint
	z[0], b = bits.Sub(z[0], w, 0)
	for i := 1; b != 0 && i < len(z); i++ {
		z[i], b = bits.Sub(z[i], 0, b)
	}
	return b != 0
}

// subWordAtBorrow subtracts a borrow, b, from z in place and returns the
// borrow past the top.
func subWordAtBorrow(z []uint, b uint) uint {
	for i := 0; b != 0 && i < len(z); i++ {
		z[i], b = bits.Sub(z[i], 0, b)
	}
	return b
}

// addWordAt adds w to z in place and returns the carry past the top.
func addWordAt(z []uint, w uint) uint {
	c := w
	for i := 0; c != 0 && i < len(z); i++ {
		z[i], c = bits.Add(z[i], c, 0)
	}
	return c
}
