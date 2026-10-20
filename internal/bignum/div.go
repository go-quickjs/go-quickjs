package bignum

import "math/bits"

// reciprocal is floor((2^(2W)-1)/d) - 2^W of a normalized d, its top bit
// set: what divRec divides a double word by d with, by multiplying.
func reciprocal(d uint) uint {
	rec, _ := bits.Div(^d, ^uint(0), d)
	return rec
}

// divRec is u1:u0 divided by d, normalized, of reciprocal rec, for u1 under
// d: two products and corrections for the hardware's division, which takes
// several times as long (Möller and Granlund, "Improved division by
// invariant integers", algorithm 4).
func divRec(u1, u0, d, rec uint) (uint, uint) {
	qh, ql := bits.Mul(rec, u1)
	ql, c := bits.Add(ql, u0, 0)
	qh, _ = bits.Add(qh, u1, c)
	qh++
	r := u0 - qh*d
	if r > ql {
		qh--
		r += d
	}
	if r >= d {
		qh++
		r -= d
	}
	return qh, r
}

// DivWord writes x/d to q, which has room for len(x) words and may be x, and
// returns it and the remainder, for d not zero: x and d shifted so that
// d's top bit is set, a word at a time, by d's reciprocal (divRec).
func DivWord(q, x []uint, d uint) ([]uint, uint) {
	s := uint(bits.LeadingZeros(d))
	d <<= s
	rec := reciprocal(d)
	var r uint
	if s == 0 {
		for i := len(x) - 1; i >= 0; i-- {
			q[i], r = divRec(r, x[i], d, rec)
		}
		return Norm(q[:len(x)]), r
	}
	if len(x) > 0 {
		r = x[len(x)-1] >> (W - s)
	}
	for i := len(x) - 1; i >= 0; i-- {
		w := x[i] << s
		if i > 0 {
			w |= x[i-1] >> (W - s)
		}
		q[i], r = divRec(r, w, d, rec)
	}
	return Norm(q[:len(x)]), r >> s
}

// ModWord is x's remainder by d, for d not zero.
func ModWord(x []uint, d uint) uint {
	s := uint(bits.LeadingZeros(d))
	d <<= s
	rec := reciprocal(d)
	var r uint
	if s != 0 && len(x) > 0 {
		r = x[len(x)-1] >> (W - s)
	}
	for i := len(x) - 1; i >= 0; i-- {
		w := x[i] << s
		if s != 0 && i > 0 {
			w |= x[i-1] >> (W - s)
		}
		_, r = divRec(r, w, d, rec)
	}
	return r >> s
}

// QuoRoom is how many words DivMod may write of a quotient of x by y, of n
// and m words.
func QuoRoom(n, m int) int { return max(n-m+1, 1) }

// DivMod writes x/y, truncated, to q, which has room for QuoRoom words, and
// x's remainder by y to r, which has room for len(y) words, for y not zero;
// neither overlaps x or y, nor the other. Either may be nil, for a result
// not wanted. A divisor of one word divides a word at a time; a longer one
// by Knuth's algorithm D (The Art of Computer Programming, 4.3.1), each
// quotient word guessed from the dividend's top two and the divisor's top
// word, corrected by the divisor's next.
func DivMod(q, r, x, y []uint) ([]uint, []uint) {
	if Cmp(x, y) < 0 {
		if r != nil {
			r = r[:len(x)]
			copy(r, x)
		}
		if q != nil {
			q = q[:0]
		}
		return q, r
	}
	if len(y) == 1 {
		var qq []uint
		if q == nil {
			rem := ModWord(x, y[0])
			if r == nil {
				return nil, nil
			}
			r = r[:0]
			if rem != 0 {
				r = append(r, rem)
			}
			return nil, r
		}
		qq, rem := DivWord(q, x, y[0])
		if r != nil {
			r = r[:0]
			if rem != 0 {
				r = append(r, rem)
			}
		}
		return qq, r
	}
	n, m := len(y), len(x)-len(y)
	// Scratch for the shifted operands: on the stack while they are small.
	var buf [96]uint
	need := (m + n + 1) + n
	var s []uint
	if need <= len(buf) {
		s = buf[:need]
	} else {
		s = make([]uint, need)
	}
	u, v := s[:m+n+1], s[m+n+1:m+2*n+1]
	// Normalized: the divisor's top bit set, the dividend shifted alike, a
	// word more for its top.
	shift := uint(bits.LeadingZeros(y[n-1]))
	shlInto(v, y, shift)
	u[m+n] = shlInto(u[:m+n], x, shift)
	vn1, vn2 := v[n-1], v[n-2]
	rec := reciprocal(vn1)
	if q != nil {
		q = q[:m+1]
	}
	for j := m; j >= 0; j-- {
		qhat := ^uint(0)
		if ujn := u[j+n]; ujn != vn1 {
			var rhat uint
			qhat, rhat = divRec(ujn, u[j+n-1], vn1, rec)
			// The 3-by-2 guess: q̂·v[n-2] against r̂:u[j+n-2].
			hi, lo := bits.Mul(qhat, vn2)
			for hi > rhat || hi == rhat && lo > u[j+n-2] {
				qhat--
				prev := rhat
				rhat += vn1
				if rhat < prev {
					// r̂ overflowed: the guess is now small enough.
					break
				}
				hi, lo = bits.Mul(qhat, vn2)
			}
		}
		// u[j:j+n+1] -= q̂·v, added back once if that went below zero.
		c := mulSubRow(u[j:j+n], v, qhat)
		top := u[j+n]
		u[j+n] = top - c
		if top < c {
			u[j+n] += addVV(u[j:j+n], u[j:j+n], v)
			qhat--
		}
		if q != nil {
			q[j] = qhat
		}
	}
	if q != nil {
		q = Norm(q)
	}
	if r != nil {
		r = r[:n]
		shrInto(r, u[:n], shift)
		r = Norm(r)
	}
	return q, r
}

// shlInto writes x<<s, for s under a word, to z, of len(x) words, and
// returns the bits shifted out of its top.
func shlInto(z, x []uint, s uint) uint {
	if s == 0 {
		copy(z, x)
		return 0
	}
	n := len(x)
	if n == 0 {
		return 0
	}
	z = z[:n]
	out := x[n-1] >> (W - s)
	for i := n - 1; i > 0; i-- {
		z[i] = x[i]<<s | x[i-1]>>(W-s)
	}
	z[0] = x[0] << s
	return out
}

// shrInto writes x>>s, for s under a word, to z, of len(x) words.
func shrInto(z, x []uint, s uint) {
	if s == 0 {
		copy(z, x)
		return
	}
	n := len(x)
	if n == 0 {
		return
	}
	z = z[:n]
	for i := 0; i < n-1; i++ {
		z[i] = x[i]>>s | x[i+1]<<(W-s)
	}
	z[n-1] = x[n-1] >> s
}
