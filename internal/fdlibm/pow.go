// Copyright (c) 2018-2026, Arm Limited.
// SPDX-License-Identifier: MIT
//
// Ported from Arm's Optimized Routines, math/pow.c, under its MIT license;
// see LICENSE in this directory.

package fdlibm

import "math"

// Arm's pow, from Arm's Optimized Routines, as it is built without fused
// multiply-adds. V8 calls the C library's pow for Math.pow and **. glibc's
// pow is this same code, contributed to it by Arm, which is why Node on
// Linux answers as this does; its error is below 0.52 ulp, and in practice it
// rounds correctly, as the other platforms' C libraries do. fdlibm's pow,
// which V8 falls back to without one, is further out.

const (
	powLogTableBits = 7
	powLogN         = 1 << powLogTableBits
	powOff          = 0x3fe6955500000000
	ln2hi           = 0x1.62e42fefa3800p-1
	ln2lo           = 0x1.ef35793c76730p-45

	expTableBits = 7
	expN         = 1 << expTableBits
	invLn2N      = float64(0x1.71547652b82fep0 * expN)
	negLn2hiN    = -0x1.62e42fefa0000p-8
	negLn2loN    = -0x1.cf79abc9e3b3ap-47
	expShift     = 0x1.8p52
	signBias     = 0x800 << expTableBits
)

func asuint64(x float64) uint64 { return math.Float64bits(x) }
func asdouble(i uint64) float64 { return math.Float64frombits(i) }
func top12(x float64) uint32    { return uint32(asuint64(x) >> 52) }

// logInline is log(x) as y + tail, tail about 15 bits more; ix is x's bits,
// a subnormal normalized with the sign bit as part of the exponent.
func logInline(ix uint64) (float64, float64) {
	A := &powLogPoly
	tmp := ix - powOff
	i := (tmp >> (52 - powLogTableBits)) % powLogN
	k := int64(tmp) >> 52
	iz := ix - (tmp & (0xfff << 52))
	z := asdouble(iz)
	kd := float64(k)

	invc := powLogTab[i].invc
	logc := powLogTab[i].logc
	logctail := powLogTab[i].logctail

	zhi := asdouble((iz + (1 << 31)) & 0xFFFFFFFF00000000)
	zlo := z - zhi
	rhi := float64(zhi*invc) - 1.0
	rlo := float64(zlo * invc)
	r := rhi + rlo

	t1 := float64(kd*ln2hi) + logc
	t2 := t1 + r
	lo1 := float64(kd*ln2lo) + logctail
	lo2 := t1 - t2 + r

	// Each product that feeds a sum is rounded on its own, as C rounds it
	// without an fma, even across statements, where Go would fuse it.
	ar := float64(A[0] * r)
	ar2 := float64(r * ar)
	ar3 := float64(r * ar2)
	arhi := float64(A[0] * rhi)
	arhi2 := float64(rhi * arhi)
	hi := t2 + arhi2
	lo3 := float64(rlo * (ar + arhi))
	lo4 := t2 - hi + arhi2
	p := float64(ar3 * (A[1] + float64(r*A[2]) + float64(ar2*(A[3]+float64(r*A[4])+float64(ar2*(A[5]+float64(r*A[6])))))))
	lo := lo1 + lo2 + lo3 + lo4 + p
	y := hi + lo
	return y, hi - y + lo
}

// expSpecial is specialcase: scale*(1+tmp) where it may overflow or
// underflow.
func expSpecial(tmp float64, sbits, ki uint64) float64 {
	if ki&0x80000000 == 0 {
		sbits -= 1009 << 52
		scale := asdouble(sbits)
		y := scale + float64(scale*tmp)
		return float64(y * 0x1p1009)
	}
	sbits += 1022 << 52
	scale := asdouble(sbits)
	y := scale + float64(scale*tmp)
	if math.Abs(y) < 1.0 {
		// Rounded to the precision it has before it is scaled into the
		// subnormal range, where it would be rounded again.
		one := 1.0
		if y < 0.0 {
			one = -1.0
		}
		lo := scale - y + float64(scale*tmp)
		hi := one + y
		lo = one - hi + y + lo
		y = (hi + lo) - one
		if y == 0.0 {
			y = asdouble(sbits & 0x8000000000000000)
		}
	}
	return 0x1p-1022 * y
}

// expInline is sign*exp(x+xtail), the sign -1 where signBias is given.
func expInline(x, xtail float64, sbias uint32) float64 {
	C2, C3, C4, C5 := expPoly[0], expPoly[1], expPoly[2], expPoly[3]
	abstop := top12(x) & 0x7ff
	if abstop-top12(0x1p-54) >= top12(512.0)-top12(0x1p-54) {
		if abstop-top12(0x1p-54) >= 0x80000000 {
			one := 1.0 + x
			if sbias != 0 {
				return -one
			}
			return one
		}
		if abstop >= top12(1024.0) {
			neg := sbias != 0
			if asuint64(x)>>63 != 0 {
				if neg {
					return math.Copysign(0, -1)
				}
				return 0
			}
			if neg {
				return math.Inf(-1)
			}
			return math.Inf(1)
		}
		abstop = 0
	}

	z := float64(invLn2N * x)
	kd := z + expShift
	ki := asuint64(kd)
	kd -= expShift
	r := x + float64(kd*negLn2hiN) + float64(kd*negLn2loN)
	r += xtail
	idx := 2 * (ki % expN)
	top := (ki + uint64(sbias)) << (52 - expTableBits)
	tail := asdouble(expTab[idx])
	sbits := expTab[idx+1] + top
	r2 := float64(r * r)
	tmp := tail + r + float64(r2*(C2+float64(r*C3))) + float64(float64(r2*r2)*(C4+float64(r*C5)))
	if abstop == 0 {
		return expSpecial(tmp, sbits, ki)
	}
	scale := asdouble(sbits)
	return scale + float64(scale*tmp)
}

// checkint is 0 for a y that is no integer, 1 for an odd one, and 2 for an
// even one, from its bits; y is finite and not zero.
func checkint(iy uint64) int {
	e := int(iy >> 52 & 0x7ff)
	if e < 0x3ff {
		return 0
	}
	if e > 0x3ff+52 {
		return 2
	}
	if iy&((1<<uint(0x3ff+52-e))-1) != 0 {
		return 0
	}
	if iy&(1<<uint(0x3ff+52-e)) != 0 {
		return 1
	}
	return 2
}

// zeroinfnan is whether the bits are of zero, an infinity or a NaN.
func zeroinfnan(i uint64) bool {
	return 2*i-1 >= 2*asuint64(math.Inf(1))-1
}

// PowC is Arm's pow, with C's semantics: 1**y and x**0 are 1 whatever y and
// x are, which JavaScript's is not.
func PowC(x, y float64) float64 {
	var sbias uint32
	ix := asuint64(x)
	iy := asuint64(y)
	topx := top12(x)
	topy := top12(y)
	if topx-0x001 >= 0x7ff-0x001 || (topy&0x7ff)-0x3be >= 0x43e-0x3be {
		if zeroinfnan(iy) {
			if 2*iy == 0 {
				return 1.0
			}
			if ix == asuint64(1.0) {
				return 1.0
			}
			if 2*ix > 2*asuint64(math.Inf(1)) || 2*iy > 2*asuint64(math.Inf(1)) {
				return x + y
			}
			if 2*ix == 2*asuint64(1.0) {
				return 1.0
			}
			if (2*ix < 2*asuint64(1.0)) == (iy>>63 == 0) {
				return 0.0
			}
			return float64(y * y)
		}
		if zeroinfnan(ix) {
			x2 := float64(x * x)
			if ix>>63 != 0 && checkint(iy) == 1 {
				x2 = -x2
			}
			if iy>>63 != 0 {
				return 1 / x2
			}
			return x2
		}
		if ix>>63 != 0 {
			yint := checkint(iy)
			if yint == 0 {
				return math.NaN()
			}
			if yint == 1 {
				sbias = signBias
			}
			ix &= 0x7fffffffffffffff
			topx &= 0x7ff
		}
		if (topy&0x7ff)-0x3be >= 0x43e-0x3be {
			if ix == asuint64(1.0) {
				return 1.0
			}
			if topy&0x7ff < 0x3be {
				if ix > asuint64(1.0) {
					return 1.0 + y
				}
				return 1.0 - y
			}
			if (ix > asuint64(1.0)) == (topy < 0x800) {
				return math.Inf(1)
			}
			return 0
		}
		if topx == 0 {
			ix = asuint64(x * 0x1p52)
			ix &= 0x7fffffffffffffff
			ix -= 52 << 52
		}
	}

	hi, lo := logInline(ix)
	yhi := asdouble(iy & 0xFFFFFFFFF8000000)
	ylo := y - yhi
	lhi := asdouble(asuint64(hi) & 0xFFFFFFFFF8000000)
	llo := hi - lhi + lo
	ehi := float64(yhi * lhi)
	elo := float64(ylo*lhi) + float64(y*llo)
	return expInline(ehi, elo, sbias)
}
