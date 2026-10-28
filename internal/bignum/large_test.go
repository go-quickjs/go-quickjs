package bignum

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"testing"
)

// The algorithms for large numbers against math/big: at sizes at every
// threshold and around it, balanced and not, of operands that carry and
// borrow through -- random, all ones, a lone top bit, the nasty words
// randNat makes -- and with the thresholds lowered, so that every method
// recurses into every other many times over numbers small enough to test
// by the hundred.

// kindNat is a magnitude of n words of a kind: 0 random, 1 all ones, 2 a
// power of two, 3 randNat's.
func kindNat(r *rand.Rand, kind, n int) []uint {
	x := make([]uint, n)
	if n == 0 {
		return x
	}
	switch kind {
	case 0:
		for i := range x {
			x[i] = uint(r.Uint64())
		}
		if x[n-1] == 0 {
			x[n-1] = 1
		}
	case 1:
		for i := range x {
			x[i] = ^uint(0)
		}
	case 2:
		x[n-1] = 1 << uint(r.IntN(W))
	default:
		return randNat(r, n)
	}
	return x
}

// around is each of ts, with its neighbours.
func around(ts ...int) []int {
	var s []int
	for _, t := range ts {
		s = append(s, t-1, t, t+1)
	}
	return s
}

// mulSizes are the pairs of operand sizes the product tests take.
func mulSizes() [][2]int {
	var ps [][2]int
	add := func(n, m int) {
		if n > 0 && m > 0 {
			ps = append(ps, [2]int{n, m})
		}
	}
	for _, n := range around(karatsubaThreshold, toomThreshold, 2*toomThreshold, 3*toomThreshold, fftThreshold, 2*fftThreshold) {
		add(n, n)
		add(n, n-7)
		add(2*n+1, n)
		add(3*n+5, n)
		add(n, karatsubaThreshold+3)
		add(n, 5)
	}
	for _, n := range []int{600, 1000, 2500, 7000, 20000} {
		add(n, n)
		add(n, n/2+1)
		add(n, n/3)
		add(n, fftThreshold)
	}
	return ps
}

func checkMul(t *testing.T, r *rand.Rand, n, m int) {
	t.Helper()
	for kind := 0; kind < 4; kind++ {
		x, y := kindNat(r, kind, n), kindNat(r, (kind+1)%4, m)
		want := new(big.Int).Mul(toBig(x), toBig(y))
		check(t, fmt.Sprintf("mul %d×%d kind %d", n, m, kind), Mul(make([]uint, n+m), x, y), want)
		check(t, fmt.Sprintf("mul %d×%d kind %d swapped", n, m, kind), Mul(make([]uint, n+m), y, x), want)
		if n == m {
			check(t, fmt.Sprintf("sqr %d kind %d", n, kind), Sqr(make([]uint, 2*n), x), new(big.Int).Mul(toBig(x), toBig(x)))
		}
	}
}

func TestMulLarge(t *testing.T) {
	r := rand.New(rand.NewPCG(21, 22))
	for _, s := range mulSizes() {
		checkMul(t, r, s[0], s[1])
	}
}

// withThresholds runs f with the thresholds lowered as low as each method
// allows, restored after.
func withThresholds(f func()) {
	saved, savedFFT := toomThreshold, fftThreshold
	defer func() { toomThreshold, fftThreshold = saved, savedFFT }()
	toomThreshold = karatsubaThreshold
	fftThreshold = 64
	f()
}

func TestMulLowThresholds(t *testing.T) {
	r := rand.New(rand.NewPCG(23, 24))
	withThresholds(func() {
		for n := 1; n < 400; n += 1 + n/8 {
			for _, m := range []int{n, n/2 + 1, n/3 + 1, 1, 2} {
				if m <= n {
					checkMul(t, r, n, m)
				}
			}
		}
		for _, n := range []int{1000, 3001, 20000} {
			checkMul(t, r, n, n)
			checkMul(t, r, n, n/2)
		}
	})
}

func FuzzMul(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{4, 5}, uint16(0))
	f.Fuzz(func(t *testing.T, a, b []byte, words uint16) {
		// The operands, and a third of words words of a's bytes over and
		// over, for products past the thresholds of what fuzzing makes.
		bx, by := new(big.Int).SetBytes(a), new(big.Int).SetBytes(b)
		if n := int(words % 3000); n > 0 && len(a) > 0 {
			rep := make([]byte, n*W/8)
			for i := range rep {
				rep[i] = a[i%len(a)] ^ byte(i>>8)
			}
			bx.SetBytes(rep)
		}
		x, y := fromBig(bx), fromBig(by)
		want := new(big.Int).Mul(bx, by)
		check(t, "mul", Mul(make([]uint, len(x)+len(y)), x, y), want)
		check(t, "sqr", Sqr(make([]uint, 2*len(x)), x), new(big.Int).Mul(bx, bx))
		withThresholds(func() {
			check(t, "mul low", Mul(make([]uint, len(x)+len(y)), x, y), want)
		})
	})
}
