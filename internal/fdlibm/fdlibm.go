// Copyright (C) 1993 by Sun Microsystems, Inc. All rights reserved.
//
// Developed at SunSoft, a Sun Microsystems, Inc. business.
// Permission to use, copy, modify, and distribute this
// software is freely granted, provided that this notice
// is preserved.
//
// Modified by Google Inc.; Copyright 2016 the V8 project authors, under the
// BSD license in LICENSE in this directory.

// Package fdlibm is V8's Math functions, which are fdlibm's: a port of V8
// 14.6's src/base/ieee754.cc, whose answers a script can see to the last bit;
// and pow, which is Arm's.
// Go's math package computes most of them differently -- some from Cephes,
// some in assembly -- and answers differently in the last place, and far
// worse in a few: the logarithm of a subnormal, and sinh and cosh near where
// they overflow.
//
// The code follows the C operation for operation, which is what the answers
// depend on; and every product that feeds a sum is rounded by an explicit
// conversion, so that arm64, which would fuse the two, answers as every other
// platform does.
package fdlibm

import "math"

// The C reads and writes the two 32-bit halves of a double.

func hiWord(x float64) int32  { return int32(math.Float64bits(x) >> 32) }
func loWord(x float64) uint32 { return uint32(math.Float64bits(x)) }

func words(x float64) (int32, uint32) {
	b := math.Float64bits(x)
	return int32(b >> 32), uint32(b)
}

func fromWords(hi int32, lo uint32) float64 {
	return math.Float64frombits(uint64(uint32(hi))<<32 | uint64(lo))
}

func setHi(x float64, hi int32) float64  { return fromWords(hi, loWord(x)) }
func setLo(x float64, lo uint32) float64 { return fromWords(hiWord(x), lo) }

// nan is what the C returns as a signalling NaN, which JavaScript cannot
// tell from any other.
var nan = math.NaN()

// --- Argument reduction for sin, cos and tan ---------------------------------

var twoOverPi = [...]int32{
	0xA2F983, 0x6E4E44, 0x1529FC, 0x2757D1, 0xF534DD, 0xC0DB62, 0x95993C,
	0x439041, 0xFE5163, 0xABDEBB, 0xC561B7, 0x246E3A, 0x424DD2, 0xE00649,
	0x2EEA09, 0xD1921C, 0xFE1DEB, 0x1CB129, 0xA73EE8, 0x8235F5, 0x2EBB44,
	0x84E99C, 0x7026B4, 0x5F7E41, 0x3991D6, 0x398353, 0x39F49C, 0x845F8B,
	0xBDF928, 0x3B1FF8, 0x97FFDE, 0x05980F, 0xEF2F11, 0x8B5A0A, 0x6D1F6D,
	0x367ECF, 0x27CB09, 0xB74F46, 0x3F669E, 0x5FEA2D, 0x7527BA, 0xC7EBE5,
	0xF17B3D, 0x0739F7, 0x8A5292, 0xEA6BFB, 0x5FB11F, 0x8D5D08, 0x560330,
	0x46FC7B, 0x6BABF0, 0xCFBC20, 0x9AF436, 0x1DA9E3, 0x91615E, 0xE61B08,
	0x659985, 0x5F14A0, 0x68408D, 0xFFD880, 0x4D7327, 0x310606, 0x1556CA,
	0x73A8C9, 0x60E27B, 0xC08C6B,
}

var npio2HW = [...]int32{
	0x3FF921FB, 0x400921FB, 0x4012D97C, 0x401921FB, 0x401F6A7A, 0x4022D97C,
	0x4025FDBB, 0x402921FB, 0x402C463A, 0x402F6A7A, 0x4031475C, 0x4032D97C,
	0x40346B9C, 0x4035FDBB, 0x40378FDB, 0x403921FB, 0x403AB41B, 0x403C463A,
	0x403DD85A, 0x403F6A7A, 0x40407E4C, 0x4041475C, 0x4042106C, 0x4042D97C,
	0x4043A28C, 0x40446B9C, 0x404534AC, 0x4045FDBB, 0x4046C6CB, 0x40478FDB,
	0x404858EB, 0x404921FB,
}

// remPio2 is __ieee754_rem_pio2: x less a multiple n of pi/2, as y[0]+y[1],
// and n.
func remPio2(x float64, y *[2]float64) int32 {
	const (
		zero    = 0.0
		half    = 5.00000000000000000000e-01
		two24   = 1.67772160000000000000e+07
		invpio2 = 6.36619772367581382433e-01
		pio2_1  = 1.57079632673412561417e+00
		pio2_1t = 6.07710050650619224932e-11
		pio2_2  = 6.07710050630396597660e-11
		pio2_2t = 2.02226624879595063154e-21
		pio2_3  = 2.02226624871116645580e-21
		pio2_3t = 8.47842766036889956997e-32
	)
	var z, w, t, r, fn float64
	var tx [3]float64
	var e0, i, j, nx, n int32

	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix <= 0x3FE921FB {
		y[0], y[1] = x, 0
		return 0
	}
	if ix < 0x4002D97C {
		if hx > 0 {
			z = x - pio2_1
			if ix != 0x3FF921FB {
				y[0] = z - pio2_1t
				y[1] = (z - y[0]) - pio2_1t
			} else {
				z -= pio2_2
				y[0] = z - pio2_2t
				y[1] = (z - y[0]) - pio2_2t
			}
			return 1
		}
		z = x + pio2_1
		if ix != 0x3FF921FB {
			y[0] = z + pio2_1t
			y[1] = (z - y[0]) + pio2_1t
		} else {
			z += pio2_2
			y[0] = z + pio2_2t
			y[1] = (z - y[0]) + pio2_2t
		}
		return -1
	}
	if ix <= 0x413921FB {
		t = math.Abs(x)
		n = int32(float64(t*invpio2) + half)
		fn = float64(n)
		r = t - float64(fn*pio2_1)
		// Products that feed a sum in a later statement are rounded there
		// too, which Go would otherwise fuse.
		w = float64(fn * pio2_1t)
		if n < 32 && ix != npio2HW[n-1] {
			y[0] = r - w
		} else {
			j = ix >> 20
			y[0] = r - w
			high := uint32(hiWord(y[0]))
			i = j - int32((high>>20)&0x7FF)
			if i > 16 {
				t = r
				w = float64(fn * pio2_2)
				r = t - w
				w = float64(fn*pio2_2t) - ((t - r) - w)
				y[0] = r - w
				high = uint32(hiWord(y[0]))
				i = j - int32((high>>20)&0x7FF)
				if i > 49 {
					t = r
					w = float64(fn * pio2_3)
					r = t - w
					w = float64(fn*pio2_3t) - ((t - r) - w)
					y[0] = r - w
				}
			}
		}
		y[1] = (r - y[0]) - w
		if hx < 0 {
			y[0], y[1] = -y[0], -y[1]
			return -n
		}
		return n
	}
	if ix >= 0x7FF00000 {
		y[0] = x - x
		y[1] = y[0]
		return 0
	}
	z = setLo(z, loWord(x))
	e0 = (ix >> 20) - 1046
	z = setHi(z, ix-int32(uint32(e0)<<20))
	for i = 0; i < 2; i++ {
		tx[i] = float64(int32(z))
		z = float64((z - tx[i]) * two24)
	}
	tx[2] = z
	nx = 3
	for tx[nx-1] == zero {
		nx--
	}
	var yy [3]float64
	n = kernelRemPio2(tx[:], &yy, int(e0), int(nx), 2)
	y[0], y[1] = yy[0], yy[1]
	if hx < 0 {
		y[0], y[1] = -y[0], -y[1]
		return -n
	}
	return n
}

var pio2Chunks = [...]float64{
	1.57079625129699707031e+00,
	7.54978941586159635335e-08,
	5.39030252995776476554e-15,
	3.28200341580791294123e-22,
	1.27065575308067607349e-29,
	1.22933308981111328932e-36,
	2.73370053816464559624e-44,
	2.16741683877804819444e-51,
}

// kernelRemPio2 is __kernel_rem_pio2, for the large arguments.
func kernelRemPio2(x []float64, y *[3]float64, e0, nx, prec int) int32 {
	const (
		zero   = 0.0
		one    = 1.0
		two24  = 1.67772160000000000000e+07
		twon24 = 5.96046447753906250000e-08
	)
	initJK := [...]int{2, 3, 4, 6}
	var iq [20]int32
	var f, fq, q [20]float64
	var z, fw float64
	var jz, jx, jv, jp, jk, carry, i, j, k, m, q0, ih int
	var n int32

	jk = initJK[prec]
	jp = jk

	jx = nx - 1
	jv = (e0 - 3) / 24
	if jv < 0 {
		jv = 0
	}
	q0 = e0 - 24*(jv+1)

	j = jv - jx
	m = jx + jk
	for i = 0; i <= m; i, j = i+1, j+1 {
		if j < 0 {
			f[i] = zero
		} else {
			f[i] = float64(twoOverPi[j])
		}
	}

	for i = 0; i <= jk; i++ {
		fw = 0.0
		for j = 0; j <= jx; j++ {
			fw += float64(x[j] * f[jx+i-j])
		}
		q[i] = fw
	}

	jz = jk
recompute:
	i, j, z = 0, jz, q[jz]
	for ; j > 0; i, j = i+1, j-1 {
		fw = float64(int32(twon24 * z))
		iq[i] = int32(z - float64(two24*fw))
		z = q[j-1] + fw
	}

	z = math.Ldexp(z, q0)
	z -= float64(8.0 * math.Floor(z*0.125))
	n = int32(z)
	z -= float64(n)
	ih = 0
	if q0 > 0 {
		i = int(iq[jz-1] >> (24 - q0))
		n += int32(i)
		iq[jz-1] -= int32(i << (24 - q0))
		ih = int(iq[jz-1] >> (23 - q0))
	} else if q0 == 0 {
		ih = int(iq[jz-1] >> 23)
	} else if z >= 0.5 {
		ih = 2
	}

	if ih > 0 {
		n++
		carry = 0
		for i = 0; i < jz; i++ {
			jj := iq[i]
			if carry == 0 {
				if jj != 0 {
					carry = 1
					iq[i] = 0x1000000 - jj
				}
			} else {
				iq[i] = 0xFFFFFF - jj
			}
		}
		if q0 > 0 {
			switch q0 {
			case 1:
				iq[jz-1] &= 0x7FFFFF
			case 2:
				iq[jz-1] &= 0x3FFFFF
			}
		}
		if ih == 2 {
			z = one - z
			if carry != 0 {
				z -= math.Ldexp(one, q0)
			}
		}
	}

	if z == zero {
		jj := int32(0)
		for i = jz - 1; i >= jk; i-- {
			jj |= iq[i]
		}
		if jj == 0 {
			for k = 1; jk >= k && iq[jk-k] == 0; k++ {
			}
			for i = jz + 1; i <= jz+k; i++ {
				f[jx+i] = float64(twoOverPi[jv+i])
				fw = 0.0
				for j = 0; j <= jx; j++ {
					fw += float64(x[j] * f[jx+i-j])
				}
				q[i] = fw
			}
			jz += k
			goto recompute
		}
	}

	if z == 0.0 {
		jz--
		q0 -= 24
		for iq[jz] == 0 {
			jz--
			q0 -= 24
		}
	} else {
		z = math.Ldexp(z, -q0)
		if z >= two24 {
			fw = float64(int32(twon24 * z))
			iq[jz] = int32(z - float64(two24*fw))
			jz++
			q0 += 24
			iq[jz] = int32(fw)
		} else {
			iq[jz] = int32(z)
		}
	}

	fw = math.Ldexp(one, q0)
	for i = jz; i >= 0; i-- {
		q[i] = float64(fw * float64(iq[i]))
		fw = float64(fw * twon24)
	}

	for i = jz; i >= 0; i-- {
		fw = 0.0
		for k = 0; k <= jp && k <= jz-i; k++ {
			fw += float64(pio2Chunks[k] * q[i+k])
		}
		fq[jz-i] = fw
	}

	switch prec {
	case 0:
		fw = 0.0
		for i = jz; i >= 0; i-- {
			fw += fq[i]
		}
		if ih == 0 {
			y[0] = fw
		} else {
			y[0] = -fw
		}
	case 1, 2:
		fw = 0.0
		for i = jz; i >= 0; i-- {
			fw += fq[i]
		}
		if ih == 0 {
			y[0] = fw
		} else {
			y[0] = -fw
		}
		fw = fq[0] - fw
		for i = 1; i <= jz; i++ {
			fw += fq[i]
		}
		if ih == 0 {
			y[1] = fw
		} else {
			y[1] = -fw
		}
	}
	return n & 7
}

// kernelCos is __kernel_cos, on [-pi/4, pi/4], y the tail of x.
func kernelCos(x, y float64) float64 {
	const (
		one = 1.00000000000000000000e+00
		C1  = 4.16666666666666019037e-02
		C2  = -1.38888888888741095749e-03
		C3  = 2.48015872894767294178e-05
		C4  = -2.75573143513906633035e-07
		C5  = 2.08757232129817482790e-09
		C6  = -1.13596475577881948265e-11
	)
	ix := hiWord(x) & 0x7FFFFFFF
	if ix < 0x3E400000 {
		if int32(x) == 0 {
			return one
		}
	}
	z := float64(x * x)
	r := float64(z * (C1 + float64(z*(C2+float64(z*(C3+float64(z*(C4+float64(z*(C5+float64(z*C6)))))))))))
	if ix < 0x3FD33333 {
		return one - (float64(0.5*z) - (float64(z*r) - float64(x*y)))
	}
	var qx float64
	if ix > 0x3FE90000 {
		qx = 0.28125
	} else {
		qx = fromWords(ix-0x00200000, 0)
	}
	iz := float64(0.5*z) - qx
	a := one - qx
	return a - (iz - (float64(z*r) - float64(x*y)))
}

// kernelSin is __kernel_sin, on [-pi/4, pi/4]; iy says whether y is used.
func kernelSin(x, y float64, iy int) float64 {
	const (
		half = 5.00000000000000000000e-01
		S1   = -1.66666666666666324348e-01
		S2   = 8.33333333332248946124e-03
		S3   = -1.98412698298579493134e-04
		S4   = 2.75573137070700676789e-06
		S5   = -2.50507602534068634195e-08
		S6   = 1.58969099521155010221e-10
	)
	ix := hiWord(x) & 0x7FFFFFFF
	if ix < 0x3E400000 {
		if int32(x) == 0 {
			return x
		}
	}
	z := float64(x * x)
	v := float64(z * x)
	r := S2 + float64(z*(S3+float64(z*(S4+float64(z*(S5+float64(z*S6)))))))
	if iy == 0 {
		return x + float64(v*(S1+float64(z*r)))
	}
	return x - ((float64(z*(float64(half*y)-float64(v*r))) - y) - float64(v*S1))
}

var tanT = [...]float64{
	3.33333333333334091986e-01,
	1.33333333333201242699e-01,
	5.39682539762260521377e-02,
	2.18694882948595424599e-02,
	8.86323982359930005737e-03,
	3.59207910759131235356e-03,
	1.45620945432529025516e-03,
	5.88041240820264096874e-04,
	2.46463134818469906812e-04,
	7.81794442939557092300e-05,
	7.14072491382608190305e-05,
	-1.85586374855275456654e-05,
	2.59073051863633712884e-05,
}

// kernelTan is __kernel_tan: tan(x+y) for iy 1, -1/tan(x+y) for iy -1.
func kernelTan(x, y float64, iy int) float64 {
	const (
		one    = 1.00000000000000000000e+00
		pio4   = 7.85398163397448278999e-01
		pio4lo = 3.06161699786838301793e-17
	)
	T := &tanT
	var z, r, v, w, s float64
	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix < 0x3E300000 {
		if int32(x) == 0 {
			low := loWord(x)
			if (uint32(ix)|low)|uint32(iy+1) == 0 {
				return one / math.Abs(x)
			}
			if iy == 1 {
				return x
			}
			z = x + y
			w = z
			z = setLo(z, 0)
			v = y - (z - x)
			a := -one / w
			t := setLo(a, 0)
			s = one + float64(t*z)
			return t + float64(a*(s+float64(t*v)))
		}
	}
	if ix >= 0x3FE59428 {
		if hx < 0 {
			x, y = -x, -y
		}
		z = pio4 - x
		w = pio4lo - y
		x = z + w
		y = 0.0
	}
	z = float64(x * x)
	w = float64(z * z)
	r = T[1] + float64(w*(T[3]+float64(w*(T[5]+float64(w*(T[7]+float64(w*(T[9]+float64(w*T[11])))))))))
	v = float64(z * (T[2] + float64(w*(T[4]+float64(w*(T[6]+float64(w*(T[8]+float64(w*(T[10]+float64(w*T[12])))))))))))
	s = float64(z * x)
	r = y + float64(z*(float64(s*(r+v))+y))
	r += float64(T[0] * s)
	w = x + r
	if ix >= 0x3FE59428 {
		v = float64(iy)
		return float64(float64(1-((hx>>30)&2)) * (v - float64(2.0*(x-(float64(w*w)/(w+v)-r)))))
	}
	if iy == 1 {
		return w
	}
	z = setLo(w, 0)
	v = r - (z - x)
	a := -1.0 / w
	t := setLo(a, 0)
	s = 1.0 + float64(t*z)
	return t + float64(a*(s+float64(t*v)))
}

// Sin is fdlibm's sin.
func Sin(x float64) float64 {
	var y [2]float64
	ix := hiWord(x) & 0x7FFFFFFF
	switch {
	case ix <= 0x3FE921FB:
		return kernelSin(x, 0, 0)
	case ix >= 0x7FF00000:
		return x - x
	}
	switch remPio2(x, &y) & 3 {
	case 0:
		return kernelSin(y[0], y[1], 1)
	case 1:
		return kernelCos(y[0], y[1])
	case 2:
		return -kernelSin(y[0], y[1], 1)
	default:
		return -kernelCos(y[0], y[1])
	}
}

// Cos is fdlibm's cos.
func Cos(x float64) float64 {
	var y [2]float64
	ix := hiWord(x) & 0x7FFFFFFF
	switch {
	case ix <= 0x3FE921FB:
		return kernelCos(x, 0)
	case ix >= 0x7FF00000:
		return x - x
	}
	switch remPio2(x, &y) & 3 {
	case 0:
		return kernelCos(y[0], y[1])
	case 1:
		return -kernelSin(y[0], y[1], 1)
	case 2:
		return -kernelCos(y[0], y[1])
	default:
		return kernelSin(y[0], y[1], 1)
	}
}

// Tan is fdlibm's tan.
func Tan(x float64) float64 {
	var y [2]float64
	ix := hiWord(x) & 0x7FFFFFFF
	switch {
	case ix <= 0x3FE921FB:
		return kernelTan(x, 0, 1)
	case ix >= 0x7FF00000:
		return x - x
	}
	n := remPio2(x, &y)
	return kernelTan(y[0], y[1], int(1-((n&1)<<1)))
}

// --- Inverse trigonometric functions -----------------------------------------

const (
	pS0 = 1.66666666666666657415e-01
	pS1 = -3.25565818622400915405e-01
	pS2 = 2.01212532134862925881e-01
	pS3 = -4.00555345006794114027e-02
	pS4 = 7.91534994289814532176e-04
	pS5 = 3.47933107596021167570e-05
	qS1 = -2.40339491173441421878e+00
	qS2 = 2.02094576023350569471e+00
	qS3 = -6.88283971605453293030e-01
	qS4 = 7.70381505559019352791e-02
)

func asinP(z float64) float64 {
	return float64(z * (pS0 + float64(z*(pS1+float64(z*(pS2+float64(z*(pS3+float64(z*(pS4+float64(z*pS5)))))))))))
}

func asinQ(z float64) float64 {
	return 1.0 + float64(z*(qS1+float64(z*(qS2+float64(z*(qS3+float64(z*qS4)))))))
}

// Acos is fdlibm's acos.
func Acos(x float64) float64 {
	const (
		one    = 1.00000000000000000000e+00
		pi     = 3.14159265358979311600e+00
		pio2Hi = 1.57079632679489655800e+00
		pio2Lo = 6.12323399573676603587e-17
	)
	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix >= 0x3FF00000 {
		if uint32(ix-0x3FF00000)|loWord(x) == 0 {
			if hx > 0 {
				return 0.0
			}
			return pi + float64(2.0*pio2Lo)
		}
		return nan
	}
	if ix < 0x3FE00000 {
		if ix <= 0x3C600000 {
			return pio2Hi + pio2Lo
		}
		z := float64(x * x)
		r := asinP(z) / asinQ(z)
		return pio2Hi - (x - (pio2Lo - float64(x*r)))
	} else if hx < 0 {
		z := float64((one + x) * 0.5)
		p, q := asinP(z), asinQ(z)
		s := math.Sqrt(z)
		r := p / q
		w := float64(r*s) - pio2Lo
		return pi - float64(2.0*(s+w))
	}
	z := float64((one - x) * 0.5)
	s := math.Sqrt(z)
	df := setLo(s, 0)
	c := (z - float64(df*df)) / (s + df)
	p, q := asinP(z), asinQ(z)
	r := p / q
	w := float64(r*s) + c
	return float64(2.0 * (df + w))
}

// Acosh is fdlibm's acosh.
func Acosh(x float64) float64 {
	const (
		one = 1.0
		ln2 = 6.93147180559945286227e-01
	)
	hx, lx := words(x)
	switch {
	case hx < 0x3FF00000:
		return nan
	case hx >= 0x41B00000:
		if hx >= 0x7FF00000 {
			return x + x
		}
		return Log(x) + ln2
	case uint32(hx-0x3FF00000)|lx == 0:
		return 0.0
	case hx > 0x40000000:
		t := float64(x * x)
		return Log(float64(2.0*x) - one/(x+math.Sqrt(t-one)))
	}
	t := x - one
	return Log1p(t + math.Sqrt(float64(2.0*t)+float64(t*t)))
}

// Asin is fdlibm's asin.
func Asin(x float64) float64 {
	const (
		one    = 1.00000000000000000000e+00
		huge   = 1.000e+300
		pio2Hi = 1.57079632679489655800e+00
		pio2Lo = 6.12323399573676603587e-17
		pio4Hi = 7.85398163397448278999e-01
	)
	var t float64
	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix >= 0x3FF00000 {
		if uint32(ix-0x3FF00000)|loWord(x) == 0 {
			return float64(x*pio2Hi) + float64(x*pio2Lo)
		}
		return nan
	} else if ix < 0x3FE00000 {
		if ix < 0x3E400000 {
			if huge+x > one {
				return x
			}
		} else {
			t = float64(x * x)
		}
		w := asinP(t) / asinQ(t)
		return x + float64(x*w)
	}
	w := one - math.Abs(x)
	t = float64(w * 0.5)
	p, q := asinP(t), asinQ(t)
	s := math.Sqrt(t)
	if ix >= 0x3FEF3333 {
		w = p / q
		t = pio2Hi - (float64(2.0*(s+float64(s*w))) - pio2Lo)
	} else {
		w = setLo(s, 0)
		c := (t - float64(w*w)) / (s + w)
		r := p / q
		p = float64(float64(2.0*s)*r) - (pio2Lo - float64(2.0*c))
		q = pio4Hi - float64(2.0*w)
		t = pio4Hi - (p - q)
	}
	if hx > 0 {
		return t
	}
	return -t
}

// Asinh is fdlibm's asinh.
func Asinh(x float64) float64 {
	const (
		one  = 1.00000000000000000000e+00
		ln2  = 6.93147180559945286227e-01
		huge = 1.00000000000000000000e+300
	)
	var w float64
	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix >= 0x7FF00000 {
		return x + x
	}
	if ix < 0x3E300000 {
		if huge+x > one {
			return x
		}
	}
	switch {
	case ix > 0x41B00000:
		w = Log(math.Abs(x)) + ln2
	case ix > 0x40000000:
		t := math.Abs(x)
		w = Log(float64(2.0*t) + one/(math.Sqrt(float64(x*x)+one)+t))
	default:
		t := float64(x * x)
		w = Log1p(math.Abs(x) + t/(one+math.Sqrt(one+t)))
	}
	if hx > 0 {
		return w
	}
	return -w
}

var atanHi = [...]float64{
	4.63647609000806093515e-01,
	7.85398163397448278999e-01,
	9.82793723247329054082e-01,
	1.57079632679489655800e+00,
}

var atanLo = [...]float64{
	2.26987774529616870924e-17,
	3.06161699786838301793e-17,
	1.39033110312309984516e-17,
	6.12323399573676603587e-17,
}

var aT = [...]float64{
	3.33333333333329318027e-01,
	-1.99999999998764832476e-01,
	1.42857142725034663711e-01,
	-1.11111104054623557880e-01,
	9.09088713343650656196e-02,
	-7.69187620504482999495e-02,
	6.66107313738753120669e-02,
	-5.83357013379057348645e-02,
	4.97687799461593236017e-02,
	-3.65315727442169155270e-02,
	1.62858201153657823623e-02,
}

// Atan is fdlibm's atan.
func Atan(x float64) float64 {
	const one, huge = 1.0, 1.0e300
	var id int
	hx := hiWord(x)
	ix := hx & 0x7FFFFFFF
	if ix >= 0x44100000 {
		if ix > 0x7FF00000 || (ix == 0x7FF00000 && loWord(x) != 0) {
			return x + x
		}
		if hx > 0 {
			return atanHi[3] + atanLo[3]
		}
		return -atanHi[3] - atanLo[3]
	}
	if ix < 0x3FDC0000 {
		if ix < 0x3E400000 {
			if huge+x > one {
				return x
			}
		}
		id = -1
	} else {
		x = math.Abs(x)
		if ix < 0x3FF30000 {
			if ix < 0x3FE60000 {
				id = 0
				x = (float64(2.0*x) - one) / (2.0 + x)
			} else {
				id = 1
				x = (x - one) / (x + one)
			}
		} else {
			if ix < 0x40038000 {
				id = 2
				x = (x - 1.5) / (one + float64(1.5*x))
			} else {
				id = 3
				x = -1.0 / x
			}
		}
	}
	z := float64(x * x)
	w := float64(z * z)
	s1 := float64(z * (aT[0] + float64(w*(aT[2]+float64(w*(aT[4]+float64(w*(aT[6]+float64(w*(aT[8]+float64(w*aT[10])))))))))))
	s2 := float64(w * (aT[1] + float64(w*(aT[3]+float64(w*(aT[5]+float64(w*(aT[7]+float64(w*aT[9])))))))))
	if id < 0 {
		return x - float64(x*(s1+s2))
	}
	z = atanHi[id] - ((float64(x*(s1+s2)) - atanLo[id]) - x)
	if hx < 0 {
		return -z
	}
	return z
}

// Atan2 is fdlibm's atan2.
func Atan2(y, x float64) float64 {
	const (
		tiny  = 1.0e-300
		zero  = 0.0
		piO4  = 7.8539816339744827900e-01
		piO2  = 1.5707963267948965580e+00
		pi    = 3.1415926535897931160e+00
		piLo  = 1.2246467991473531772e-16
		three = 3.0
	)
	var z float64
	hx, lx := words(x)
	ix := hx & 0x7FFFFFFF
	hy, ly := words(y)
	iy := hy & 0x7FFFFFFF
	if uint32(ix)|((lx|-lx)>>31) > 0x7FF00000 || uint32(iy)|((ly|-ly)>>31) > 0x7FF00000 {
		return x + y
	}
	if uint32(hx-0x3FF00000)|lx == 0 {
		return Atan(y)
	}
	m := ((hy >> 31) & 1) | ((hx >> 30) & 2)

	if uint32(iy)|ly == 0 {
		switch m {
		case 0, 1:
			return y
		case 2:
			return pi + tiny
		case 3:
			return -pi - tiny
		}
	}
	if uint32(ix)|lx == 0 {
		if hy < 0 {
			return -piO2 - tiny
		}
		return piO2 + tiny
	}
	if ix == 0x7FF00000 {
		if iy == 0x7FF00000 {
			switch m {
			case 0:
				return piO4 + tiny
			case 1:
				return -piO4 - tiny
			case 2:
				return float64(three*piO4) + tiny
			case 3:
				return float64(-three*piO4) - tiny
			}
		} else {
			switch m {
			case 0:
				return zero
			case 1:
				return math.Copysign(0, -1)
			case 2:
				return pi + tiny
			case 3:
				return -pi - tiny
			}
		}
	}
	if iy == 0x7FF00000 {
		if hy < 0 {
			return -piO2 - tiny
		}
		return piO2 + tiny
	}
	k := (iy - ix) >> 20
	switch {
	case k > 60:
		z = piO2 + float64(0.5*piLo)
		m &= 1
	case hx < 0 && k < -60:
		z = 0.0
	default:
		z = Atan(math.Abs(y / x))
	}
	switch m {
	case 0:
		return z
	case 1:
		return -z
	case 2:
		return pi - (z - piLo)
	default:
		return (z - piLo) - pi
	}
}

// Atanh is fdlibm's atanh.
func Atanh(x float64) float64 {
	const one, huge, zero = 1.0, 1e300, 0.0
	var t float64
	hx, lx := words(x)
	ix := hx & 0x7FFFFFFF
	if uint32(ix)|((lx|-lx)>>31) > 0x3FF00000 {
		return nan
	}
	if ix == 0x3FF00000 {
		if x > 0 {
			return math.Inf(1)
		}
		return math.Inf(-1)
	}
	if ix < 0x3E300000 && (huge+x) > zero {
		return x
	}
	x = setHi(x, ix)
	if ix < 0x3FE00000 {
		t = x + x
		t = float64(0.5 * Log1p(t+float64(t*x)/(one-x)))
	} else {
		t = float64(0.5 * Log1p((x+x)/(one-x)))
	}
	if hx >= 0 {
		return t
	}
	return -t
}

// --- Exponentials and logarithms ---------------------------------------------

// Exp is fdlibm's exp.
func Exp(x float64) float64 {
	const (
		one        = 1.0
		oThreshold = 7.09782712893383973096e+02
		uThreshold = -7.45133219101941108420e+02
		invln2     = 1.44269504088896338700e+00
		P1         = 1.66666666666666019037e-01
		P2         = -2.77777777770155933842e-03
		P3         = 6.61375632143793436117e-05
		P4         = -1.65339022054652515390e-06
		P5         = 4.13813679705723846039e-08
		E          = 2.718281828459045
		huge       = 1.0e+300
		twom1000   = 9.33263618503218878990e-302
		two1023    = 8.988465674311579539e307
	)
	halF := [2]float64{0.5, -0.5}
	ln2HI := [2]float64{6.93147180369123816490e-01, -6.93147180369123816490e-01}
	ln2LO := [2]float64{1.90821492927058770002e-10, -1.90821492927058770002e-10}

	var y, hi, lo, c, t, twopk float64
	var k int32
	hx := uint32(hiWord(x))
	xsb := int32((hx >> 31) & 1)
	hx &= 0x7FFFFFFF

	if hx >= 0x40862E42 {
		if hx >= 0x7FF00000 {
			if (hx&0xFFFFF)|loWord(x) != 0 {
				return x + x
			}
			if xsb == 0 {
				return x
			}
			return 0.0
		}
		if x > oThreshold {
			return math.Inf(1)
		}
		if x < uThreshold {
			return 0
		}
	}

	if hx > 0x3FD62E42 {
		if hx < 0x3FF0A2B2 {
			if x == 1.0 {
				return E
			}
			hi = x - ln2HI[xsb]
			lo = ln2LO[xsb]
			k = 1 - xsb - xsb
		} else {
			k = int32(float64(invln2*x) + halF[xsb])
			t = float64(k)
			hi = x - float64(t*ln2HI[0])
			lo = float64(t * ln2LO[0])
		}
		x = hi - lo
	} else if hx < 0x3E300000 {
		if huge+x > one {
			return one + x
		}
	} else {
		k = 0
	}

	t = float64(x * x)
	if k >= -1021 {
		twopk = fromWords(0x3FF00000+int32(uint32(k)<<20), 0)
	} else {
		twopk = fromWords(0x3FF00000+int32(uint32(k+1000)<<20), 0)
	}
	c = x - float64(t*(P1+float64(t*(P2+float64(t*(P3+float64(t*(P4+float64(t*P5)))))))))
	if k == 0 {
		return one - (float64(x*c)/(c-2.0) - x)
	}
	y = one - ((lo - float64(x*c)/(2.0-c)) - hi)
	if k >= -1021 {
		if k == 1024 {
			return float64(y * 2.0 * two1023)
		}
		return float64(y * twopk)
	}
	return float64(y * twopk * twom1000)
}

// Expm1 is fdlibm's expm1.
func Expm1(x float64) float64 {
	const (
		one        = 1.0
		tiny       = 1.0e-300
		oThreshold = 7.09782712893383973096e+02
		ln2Hi      = 6.93147180369123816490e-01
		ln2Lo      = 1.90821492927058770002e-10
		invln2     = 1.44269504088896338700e+00
		Q1         = -3.33333333333331316428e-02
		Q2         = 1.58730158725481460165e-03
		Q3         = -7.93650757867487942473e-05
		Q4         = 4.00821782732936239552e-06
		Q5         = -2.01099218183624371326e-07
		huge       = 1.0e+300
	)
	var y, hi, lo, c, t, e, hxs, hfx, r1, twopk float64
	var k int32
	hx := uint32(hiWord(x))
	xsb := hx & 0x80000000
	hx &= 0x7FFFFFFF

	if hx >= 0x4043687A {
		if hx >= 0x40862E42 {
			if hx >= 0x7FF00000 {
				if (hx&0xFFFFF)|loWord(x) != 0 {
					return x + x
				}
				if xsb == 0 {
					return x
				}
				return -1.0
			}
			if x > oThreshold {
				return math.Inf(1)
			}
		}
		if xsb != 0 {
			if x+tiny < 0.0 {
				return tiny - one
			}
		}
	}

	if hx > 0x3FD62E42 {
		if hx < 0x3FF0A2B2 {
			if xsb == 0 {
				hi = x - ln2Hi
				lo = ln2Lo
				k = 1
			} else {
				hi = x + ln2Hi
				lo = -ln2Lo
				k = -1
			}
		} else {
			if xsb == 0 {
				k = int32(float64(invln2*x) + 0.5)
			} else {
				k = int32(float64(invln2*x) - 0.5)
			}
			t = float64(k)
			hi = x - float64(t*ln2Hi)
			lo = float64(t * ln2Lo)
		}
		x = hi - lo
		c = (hi - x) - lo
	} else if hx < 0x3C900000 {
		t = huge + x
		return x - (t - (huge + x))
	} else {
		k = 0
	}

	hfx = float64(0.5 * x)
	hxs = float64(x * hfx)
	r1 = one + float64(hxs*(Q1+float64(hxs*(Q2+float64(hxs*(Q3+float64(hxs*(Q4+float64(hxs*Q5)))))))))
	t = 3.0 - float64(r1*hfx)
	e = float64(hxs * ((r1 - t) / (6.0 - float64(x*t))))
	if k == 0 {
		return x - (float64(x*e) - hxs)
	}
	twopk = fromWords(0x3FF00000+int32(uint32(k)<<20), 0)
	e = float64(x*(e-c)) - c
	e -= hxs
	if k == -1 {
		return float64(0.5*(x-e)) - 0.5
	}
	if k == 1 {
		if x < -0.25 {
			return float64(-2.0 * (e - (x + 0.5)))
		}
		return one + float64(2.0*(x-e))
	}
	if k <= -2 || k > 56 {
		y = one - (e - x)
		if k == 1024 {
			y = y * 2.0 * 8.98846567431158e+307
		} else {
			y = float64(y * twopk)
		}
		return y - one
	}
	t = one
	if k < 20 {
		t = setHi(t, 0x3FF00000-(0x200000>>uint(k)))
		y = t - (e - x)
		y = float64(y * twopk)
	} else {
		t = setHi(t, (0x3FF-k)<<20)
		y = x - (e + t)
		y += one
		y = float64(y * twopk)
	}
	return y
}

const (
	lg1 = 6.666666666666735130e-01
	lg2 = 3.999999999940941908e-01
	lg3 = 2.857142874366239149e-01
	lg4 = 2.222219843214978396e-01
	lg5 = 1.818357216161805012e-01
	lg6 = 1.531383769920937332e-01
	lg7 = 1.479819860511658591e-01
)

// Log is fdlibm's log.
func Log(x float64) float64 {
	const (
		ln2Hi = 6.93147180369123816490e-01
		ln2Lo = 1.90821492927058770002e-10
		two54 = 1.80143985094819840000e+16
		zero  = 0.0
	)
	var hfsq, f, s, z, R, w, t1, t2, dk float64
	var k, i, j int32
	hx, lx := words(x)

	if hx < 0x00100000 {
		if uint32(hx&0x7FFFFFFF)|lx == 0 {
			return math.Inf(-1)
		}
		if hx < 0 {
			return nan
		}
		k -= 54
		x = float64(x * two54)
		hx = hiWord(x)
	}
	if hx >= 0x7FF00000 {
		return x + x
	}
	k += (hx >> 20) - 1023
	hx &= 0x000FFFFF
	i = (hx + 0x95F64) & 0x100000
	x = setHi(x, hx|(i^0x3FF00000))
	k += i >> 20
	f = x - 1.0
	if (0x000FFFFF & (2 + hx)) < 3 {
		if f == zero {
			if k == 0 {
				return zero
			}
			dk = float64(k)
			return float64(dk*ln2Hi) + float64(dk*ln2Lo)
		}
		R = float64(float64(f*f) * (0.5 - float64(0.33333333333333333*f)))
		if k == 0 {
			return f - R
		}
		dk = float64(k)
		return float64(dk*ln2Hi) - ((R - float64(dk*ln2Lo)) - f)
	}
	s = f / (2.0 + f)
	dk = float64(k)
	z = float64(s * s)
	i = hx - 0x6147A
	w = float64(z * z)
	j = 0x6B851 - hx
	t1 = float64(w * (lg2 + float64(w*(lg4+float64(w*lg6)))))
	t2 = float64(z * (lg1 + float64(w*(lg3+float64(w*(lg5+float64(w*lg7)))))))
	i |= j
	R = t2 + t1
	if i > 0 {
		hfsq = float64(0.5 * f * f)
		if k == 0 {
			return f - (hfsq - float64(s*(hfsq+R)))
		}
		return float64(dk*ln2Hi) - ((hfsq - (float64(s*(hfsq+R)) + float64(dk*ln2Lo))) - f)
	}
	if k == 0 {
		return f - float64(s*(f-R))
	}
	return float64(dk*ln2Hi) - ((float64(s*(f-R)) - float64(dk*ln2Lo)) - f)
}

// Log1p is fdlibm's log1p.
func Log1p(x float64) float64 {
	const (
		ln2Hi = 6.93147180369123816490e-01
		ln2Lo = 1.90821492927058770002e-10
		two54 = 1.80143985094819840000e+16
		zero  = 0.0
	)
	var hfsq, f, c, s, z, R, u float64
	var k, hu int32
	hx := hiWord(x)
	ax := hx & 0x7FFFFFFF

	k = 1
	if hx < 0x3FDA827A {
		if ax >= 0x3FF00000 {
			if x == -1.0 {
				return math.Inf(-1)
			}
			return nan
		}
		if ax < 0x3E200000 {
			if two54+x > zero && ax < 0x3C900000 {
				return x
			}
			return x - float64(float64(x*x)*0.5)
		}
		if hx > 0 || hx <= int32(-0x402D413C) {
			k = 0
			f = x
			hu = 1
		}
	}
	if hx >= 0x7FF00000 {
		return x + x
	}
	if k != 0 {
		if hx < 0x43400000 {
			u = 1.0 + x
			hu = hiWord(u)
			k = (hu >> 20) - 1023
			if k > 0 {
				c = 1.0 - (u - x)
			} else {
				c = x - (u - 1.0)
			}
			c /= u
		} else {
			u = x
			hu = hiWord(u)
			k = (hu >> 20) - 1023
			c = 0
		}
		hu &= 0x000FFFFF
		if hu < 0x6A09E {
			u = setHi(u, hu|0x3FF00000)
		} else {
			k++
			u = setHi(u, hu|0x3FE00000)
			hu = (0x00100000 - hu) >> 2
		}
		f = u - 1.0
	}
	hfsq = float64(0.5 * f * f)
	dk := float64(k)
	if hu == 0 {
		if f == zero {
			if k == 0 {
				return zero
			}
			c += float64(dk * ln2Lo)
			return float64(dk*ln2Hi) + c
		}
		R = float64(hfsq * (1.0 - float64(0.66666666666666666*f)))
		if k == 0 {
			return f - R
		}
		return float64(dk*ln2Hi) - ((R - (float64(dk*ln2Lo) + c)) - f)
	}
	s = f / (2.0 + f)
	z = float64(s * s)
	R = float64(z * (lg1 + float64(z*(lg2+float64(z*(lg3+float64(z*(lg4+float64(z*(lg5+float64(z*(lg6+float64(z*lg7)))))))))))))
	if k == 0 {
		return f - (hfsq - float64(s*(hfsq+R)))
	}
	return float64(dk*ln2Hi) - ((hfsq - (float64(s*(hfsq+R)) + (float64(dk*ln2Lo) + c))) - f)
}

// kLog1p is k_log1p: log(1+f) - f, for 1+f in about [sqrt(2)/2, sqrt(2)].
func kLog1p(f float64) float64 {
	s := f / (2.0 + f)
	z := float64(s * s)
	w := float64(z * z)
	t1 := float64(w * (lg2 + float64(w*(lg4+float64(w*lg6)))))
	t2 := float64(z * (lg1 + float64(w*(lg3+float64(w*(lg5+float64(w*lg7)))))))
	R := t2 + t1
	hfsq := float64(0.5 * f * f)
	return float64(s * (hfsq + R))
}

// Log2 is fdlibm's log2.
func Log2(x float64) float64 {
	const (
		two54   = 1.80143985094819840000e+16
		ivln2hi = 1.44269504072144627571e+00
		ivln2lo = 1.67517131648865118353e-10
	)
	var k, i int32
	hx, lx := words(x)
	if hx < 0x00100000 {
		if uint32(hx&0x7FFFFFFF)|lx == 0 {
			return math.Inf(-1)
		}
		if hx < 0 {
			return nan
		}
		k -= 54
		x = float64(x * two54)
		hx = hiWord(x)
	}
	if hx >= 0x7FF00000 {
		return x + x
	}
	if hx == 0x3FF00000 && lx == 0 {
		return 0.0
	}
	k += (hx >> 20) - 1023
	hx &= 0x000FFFFF
	i = (hx + 0x95F64) & 0x100000
	x = setHi(x, hx|(i^0x3FF00000))
	k += i >> 20
	y := float64(k)
	f := x - 1.0
	hfsq := float64(0.5 * f * f)
	r := kLog1p(f)

	hi := setLo(f-hfsq, 0)
	lo := (f - hi) - hfsq + r
	valHi := float64(hi * ivln2hi)
	valLo := float64((lo+hi)*ivln2lo) + float64(lo*ivln2hi)

	w := y + valHi
	valLo += (y - w) + valHi
	valHi = w
	return valLo + valHi
}

// Log10 is fdlibm's log10.
func Log10(x float64) float64 {
	const (
		two54    = 1.80143985094819840000e+16
		ivln10   = 4.34294481903251816668e-01
		log102hi = 3.01029995663611771306e-01
		log102lo = 3.69423907715893078616e-13
	)
	var k int32
	hx, lx := words(x)
	if hx < 0x00100000 {
		if uint32(hx&0x7FFFFFFF)|lx == 0 {
			return math.Inf(-1)
		}
		if hx < 0 {
			return nan
		}
		k -= 54
		x = float64(x * two54)
		hx, lx = words(x)
	}
	if hx >= 0x7FF00000 {
		return x + x
	}
	if hx == 0x3FF00000 && lx == 0 {
		return 0.0
	}
	k += (hx >> 20) - 1023

	i := int32(uint32(k) & 0x80000000 >> 31)
	hx = (hx & 0x000FFFFF) | ((0x3FF - i) << 20)
	y := float64(k + i)
	x = fromWords(hx, lx)

	z := float64(y*log102lo) + float64(ivln10*Log(x))
	return z + float64(y*log102hi)
}

// Cbrt is fdlibm's cbrt.
func Cbrt(x float64) float64 {
	const (
		B1 = 715094163
		B2 = 696219795
		P0 = 1.87595182427177009643
		P1 = -1.88497979543377169875
		P2 = 1.621429720105354466140
		P3 = -0.758397934778766047437
		P4 = 0.145996192886612446982
	)
	var t float64
	hx, low := words(x)
	sign := uint32(hx) & 0x80000000
	hx ^= int32(sign)
	if hx >= 0x7FF00000 {
		return x + x
	}
	if hx < 0x00100000 {
		if uint32(hx)|low == 0 {
			return x
		}
		t = setHi(t, 0x43500000)
		t = float64(t * x)
		high := uint32(hiWord(t))
		t = fromWords(int32(sign|((high&0x7FFFFFFF)/3+B2)), 0)
	} else {
		t = fromWords(int32(sign|(uint32(hx)/3+B1)), 0)
	}

	r := float64(float64(t*t) * (t / x))
	t = float64(t * ((P0 + float64(r*(P1+float64(r*P2)))) + float64(float64(float64(r*r)*r)*(P3+float64(r*P4)))))

	bits := math.Float64bits(t)
	bits = (bits + 0x80000000) & 0xFFFFFFFFC0000000
	t = math.Float64frombits(bits)

	s := float64(t * t)
	r = x / s
	w := t + t
	r = (r - t) / (w + r)
	return t + float64(t*r)
}

// --- Hyperbolic functions ----------------------------------------------------

// Sinh is V8's sinh, over fdlibm's exp and expm1.
func Sinh(x float64) float64 {
	const (
		kSinhOverflow = 710.4758600739439
		twoM28        = 3.725290298461914e-9
		logMaxD       = 709.7822265625
		shuge         = 1.0e307
	)
	h := 0.5
	if x < 0 {
		h = -0.5
	}
	ax := math.Abs(x)
	if ax < 22 {
		if ax < twoM28 {
			return x
		}
		t := Expm1(ax)
		if ax < 1 {
			return float64(h * (float64(2*t) - float64(t*t)/(t+1)))
		}
		return float64(h * (t + t/(t+1)))
	}
	if ax < logMaxD {
		return float64(h * Exp(ax))
	}
	if ax <= kSinhOverflow {
		w := Exp(0.5 * ax)
		t := float64(h * w)
		return float64(t * w)
	}
	return float64(x * shuge)
}

// Cosh is V8's cosh, over fdlibm's exp and expm1.
func Cosh(x float64) float64 {
	const (
		kCoshOverflow = 710.4758600739439
		one, half     = 1.0, 0.5
	)
	ix := hiWord(x) & 0x7FFFFFFF
	if ix < 0x3FD62E43 {
		t := Expm1(math.Abs(x))
		w := one + t
		if ix < 0x3C800000 {
			return w
		}
		return one + float64(t*t)/(w+w)
	}
	if ix < 0x40360000 {
		t := Exp(math.Abs(x))
		return float64(half*t) + half/t
	}
	if ix < 0x40862E42 {
		return float64(half * Exp(math.Abs(x)))
	}
	if math.Abs(x) <= kCoshOverflow {
		w := Exp(half * math.Abs(x))
		t := float64(half * w)
		return float64(t * w)
	}
	if ix >= 0x7FF00000 {
		return float64(x * x)
	}
	return math.Inf(1)
}

// Tanh is fdlibm's tanh.
func Tanh(x float64) float64 {
	const (
		tiny = 1.0e-300
		one  = 1.0
		two  = 2.0
		huge = 1.0e300
	)
	var z float64
	jx := hiWord(x)
	ix := jx & 0x7FFFFFFF
	if ix >= 0x7FF00000 {
		if jx >= 0 {
			return one/x + one
		}
		return one/x - one
	}
	if ix < 0x40360000 {
		if ix < 0x3E300000 {
			if huge+x > one {
				return x
			}
		}
		if ix >= 0x3FF00000 {
			t := Expm1(two * math.Abs(x))
			z = one - two/(t+two)
		} else {
			t := Expm1(-two * math.Abs(x))
			z = -t / (t + two)
		}
	} else {
		z = one - tiny
	}
	if jx >= 0 {
		return z
	}
	return -z
}
