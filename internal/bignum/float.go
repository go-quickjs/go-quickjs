package bignum

import "math"

// Float64 is x's magnitude as the nearest float64, a tie to the even one,
// +Inf past the largest: as math/big's Float64 of an Int, which the
// specification's conversions of a BigInt to a Number are.
func Float64(x []uint) float64 {
	n := BitLen(x)
	if n == 0 {
		return 0
	}
	if n <= 64 {
		u := low64(x)
		if n <= 53 {
			return float64(u)
		}
		// Rounded below, with what is shifted out.
		return round64(u<<(64-n), false, n-64)
	}
	// The top 64 bits, and whether any bit below them is set.
	top, sticky := top64(x, n)
	return round64(top, sticky, n-64)
}

// round64 is t*2^e rounded to 53 bits, t's top bit set, sticky if bits below
// t's are set.
func round64(t uint64, sticky bool, e int) float64 {
	const drop = 64 - 53
	m := t >> drop
	rem := t & (1<<drop - 1)
	const half = 1 << (drop - 1)
	if rem > half || rem == half && (sticky || m&1 != 0) {
		m++
		if m == 1<<53 {
			m >>= 1
			e++
		}
	}
	return math.Ldexp(float64(m), e+drop)
}

// low64 is x's low 64 bits.
func low64(x []uint) uint64 {
	var u uint64
	for i := 0; i < len(x) && i*W < 64; i++ {
		u |= uint64(x[i]) << (i * W)
	}
	return u
}

// top64 is x's top 64 bits, of n, and whether a bit below them is set.
func top64(x []uint, n int) (uint64, bool) {
	s := uint(n - 64)
	var t uint64
	// The bits from s on.
	q, r := int(s/W), s%W
	for i := 0; i*W < 64+W && q+i < len(x); i++ {
		w := uint64(x[q+i])
		if i == 0 {
			t |= w >> r
		} else if sh := uint(i*W) - r; sh < 64 {
			t |= w << sh
		}
	}
	return t, !LowBitsZero(x, s)
}

// FromFloat64 is an integral float64's magnitude and sign, exactly, false
// for one that is not an integer, or not finite.
func FromFloat64(f float64) ([]uint, bool, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return nil, false, false
	}
	neg := f < 0
	if neg {
		f = -f
	}
	if f == 0 {
		return nil, false, true
	}
	b := math.Float64bits(f)
	e := int(b>>52&0x7FF) - 1075
	m := b&(1<<52-1) | 1<<52
	// m*2^e, m of 53 bits, e at least -52 for an integer.
	if e < 0 {
		m >>= uint(-e)
		e = 0
	}
	var w []uint
	if W == 64 {
		w = []uint{uint(m)}
	} else {
		w = Norm([]uint{uint(m), uint(m >> 32)})
	}
	z := make([]uint, ShlRoom(len(w), uint(e)))
	return Shl(z, w, uint(e)), neg, true
}
