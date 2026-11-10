package engine

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
)

// exactFMA32 returns a*b+c rounded once to a float32, for finite operands.
func exactFMA32(a, b, c float32) float32 {
	x := new(big.Float).SetPrec(0).SetFloat64(float64(a))
	y := new(big.Float).SetPrec(0).SetFloat64(float64(b))
	z := new(big.Float).SetPrec(0).SetFloat64(float64(c))
	p := new(big.Float).SetPrec(2000).Mul(x, y)
	s := new(big.Float).SetPrec(2000).Add(p, z)
	f, _ := s.Float32()
	if f == 0 && s.Signbit() {
		return float32(math.Copysign(0, -1))
	}
	return f
}

// TestFMA32 compares fma32 with the exact sum rounded once, on operands
// whose product and addend nearly cancel or lie far apart, where rounding
// twice would differ.
func TestFMA32(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	randF := func() float32 {
		for {
			f := math.Float32frombits(r.Uint32())
			if !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) {
				return f
			}
		}
	}
	for n := 0; n < 200000; n++ {
		a, b := randF(), randF()
		var c float32
		switch n % 5 {
		case 4:
			// Products of 13-bit significands, which fall on the
			// midpoints between float32s, and a small addend that
			// decides which way they round, below a float64's
			// rounding point.
			e := r.Intn(20) - 10
			a = float32(math.Ldexp(float64(1<<12|r.Intn(1<<12)), e-12))
			b = float32(math.Ldexp(float64(1<<12|r.Intn(1<<12)), -e-12))
			c = float32(math.Ldexp(float64(1+r.Intn(1<<20)), -90-r.Intn(20)))
			if r.Intn(2) == 0 {
				c = -c
			}
		case 0:
			c = randF()
		case 1:
			// Cancel all but the product's low bits.
			c = -(a * b)
		case 2:
			// An addend far below the product, its bits around the
			// float32's rounding point.
			c = a * b * float32(math.Ldexp(1, -24-r.Intn(30)))
		default:
			a = float32(math.Ldexp(1+float64(r.Intn(1<<23))/(1<<23), r.Intn(40)-20))
			b = float32(math.Ldexp(1+float64(r.Intn(1<<23))/(1<<23), r.Intn(40)-20))
			c = float32(math.Ldexp(float64(r.Intn(1<<24)), r.Intn(40)-60))
		}
		if math.IsInf(float64(c), 0) || math.IsNaN(float64(c)) {
			continue
		}
		got, want := fma32(a, b, c, false), exactFMA32(a, b, c)
		if math.Float32bits(got) != math.Float32bits(want) {
			t.Fatalf("fma32(%g, %g, %g) = %g, want %g", a, b, c, got, want)
		}
	}
}
