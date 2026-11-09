package engine

import "math"

// The relaxed SIMD operators may give one of several results. Each host
// architecture's file, relaxed_x86.go and relaxed_other.go, picks one, the
// one the architecture's vector instructions compute; these are the pieces
// they share.

// fma32 returns a*b+c rounded once to a float32. The product of two float32s
// is exact in a float64; the sum is rounded to odd in a float64, which has
// enough bits more than a float32 that rounding it again to a float32
// rounds as rounding the exact sum would.
func fma32(a, b, c float32) float32 {
	p := float64(float64(a) * float64(b))
	z := float64(c)
	s := p + z
	if s-s != 0 {
		// An infinity or a NaN, which no second rounding changes.
		return float32(s)
	}
	// e is the sum's rounding error, exactly.
	bb := s - p
	e := (p - (s - bb)) + (z - bb)
	if e != 0 && math.Float64bits(s)&1 == 0 {
		// Inexact and even: the odd neighbour on the side of the exact sum.
		u := math.Float64bits(s)
		if (e > 0) == (s > 0) {
			u++
		} else {
			u--
		}
		s = math.Float64frombits(u)
	}
	return float32(s)
}

func fusedMaddF32x4(x, y, z V128) V128 {
	a, b, c := x.f32(), y.f32(), z.f32()
	var r [4]float32
	for i := range r {
		r[i] = fma32(a[i], b[i], c[i])
	}
	return fromF32(r)
}

func fusedNmaddF32x4(x, y, z V128) V128 {
	a, b, c := x.f32(), y.f32(), z.f32()
	var r [4]float32
	for i := range r {
		r[i] = fma32(-a[i], b[i], c[i])
	}
	return fromF32(r)
}

func fusedMaddF64x2(x, y, z V128) V128 {
	a, b, c := x.f64(), y.f64(), z.f64()
	return fromF64([2]float64{math.FMA(a[0], b[0], c[0]), math.FMA(a[1], b[1], c[1])})
}

func fusedNmaddF64x2(x, y, z V128) V128 {
	a, b, c := x.f64(), y.f64(), z.f64()
	return fromF64([2]float64{math.FMA(-a[0], b[0], c[0]), math.FMA(-a[1], b[1], c[1])})
}

// dotSigned16 returns the sums of the products of each pair of x's and y's
// bytes, both signed, wrapped to 16 bits.
func dotSigned16(x, y V128) [8]uint16 {
	a, b := x.u8(), y.u8()
	var r [8]uint16
	for i := range r {
		p0 := int(int8(a[2*i])) * int(int8(b[2*i]))
		p1 := int(int8(a[2*i+1])) * int(int8(b[2*i+1]))
		r[i] = uint16(p0 + p1)
	}
	return r
}

// dotAdd32 returns acc plus, for each 32-bit lane, the sum of the products
// of its four bytes of x and y, x's signed and y's signed if signed, else
// unsigned, wrapped to 32 bits.
func dotAdd32(x, y, acc V128, signed bool) V128 {
	a, b, s := x.u8(), y.u8(), acc.u32()
	var r [4]uint32
	for i := range r {
		sum := int(int32(s[i]))
		for j := 4 * i; j < 4*i+4; j++ {
			bj := int(b[j])
			if signed {
				bj = int(int8(b[j]))
			}
			sum += int(int8(a[j])) * bj
		}
		r[i] = uint32(sum)
	}
	return fromU32(r)
}
