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
	saved, savedFFT, savedBZ := toomThreshold, fftThreshold, burnikelThreshold
	defer func() { toomThreshold, fftThreshold, burnikelThreshold = saved, savedFFT, savedBZ }()
	toomThreshold = karatsubaThreshold
	fftThreshold = 64
	burnikelThreshold = 4
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

// divSizes are the pairs of dividend and divisor sizes the division tests
// take: quotients as long as the divisor, longer, and shorter -- shorter
// than half of it, and the divisor longer than half the dividend -- at
// and around each threshold.
func divSizes() [][2]int {
	var ps [][2]int
	add := func(nx, ny int) {
		if ny > 0 && nx >= ny {
			ps = append(ps, [2]int{nx, ny})
		}
	}
	for _, n := range around(burnikelThreshold, 2*burnikelThreshold, 4*burnikelThreshold+3, 300) {
		for _, m := range around(1, burnikelThreshold, n/2, n, 2*n) {
			add(n+m, n)
		}
		add(3*n+1, n)
		add(7*n+5, n)
	}
	for _, n := range []int{1000, 3000, 10000} {
		add(2*n, n)
		add(2*n+1, n)
		add(n+n/2, n)
		add(n+n/3, n)
		add(n+100, n)
		add(3*n, n)
		add(5*n, n/2)
		add(n, 70)
		add(n, 7)
	}
	return ps
}

// checkDiv checks x/y and x%y, of nx and ny words of each kind, and of x
// made y*q+r of a remainder of zero, of one, and of y-1.
func checkDiv(t *testing.T, r *rand.Rand, nx, ny int) {
	t.Helper()
	for kind := 0; kind < 4; kind++ {
		y := kindNat(r, (kind+1)%4, ny)
		xs := [][]uint{kindNat(r, kind, nx)}
		if nx > ny {
			qq := kindNat(r, kind, nx-ny)
			by := toBig(y)
			base := new(big.Int).Mul(toBig(qq), by)
			xs = append(xs,
				fromBig(base),
				fromBig(new(big.Int).Add(base, big.NewInt(1))),
				fromBig(new(big.Int).Add(base, new(big.Int).Sub(by, big.NewInt(1)))))
		}
		for i, x := range xs {
			bq, br := new(big.Int).QuoRem(toBig(x), toBig(y), new(big.Int))
			what := fmt.Sprintf("%d/%d kind %d x%d", len(x), ny, kind, i)
			q, rem := DivMod(make([]uint, QuoRoom(len(x), ny)), make([]uint, ny), x, y)
			check(t, "quo "+what, q, bq)
			check(t, "rem "+what, rem, br)
			q, _ = DivMod(make([]uint, QuoRoom(len(x), ny)), nil, x, y)
			check(t, "quo alone "+what, q, bq)
			_, rem = DivMod(nil, make([]uint, ny), x, y)
			check(t, "rem alone "+what, rem, br)
			q, rem, err := DivModChecked(make([]uint, QuoRoom(len(x), ny)), make([]uint, ny), x, y, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			check(t, "quo checked "+what, q, bq)
			check(t, "rem checked "+what, rem, br)
		}
	}
}

func TestDivLarge(t *testing.T) {
	r := rand.New(rand.NewPCG(25, 26))
	for _, s := range divSizes() {
		checkDiv(t, r, s[0], s[1])
	}
}

func TestDivLowThresholds(t *testing.T) {
	r := rand.New(rand.NewPCG(27, 28))
	withThresholds(func() {
		for ny := 1; ny < 200; ny += 1 + ny/6 {
			for _, nx := range []int{ny, ny + 1, ny + 3, ny + ny/2, 2 * ny, 2*ny + 1, 3*ny + 2, 9 * ny} {
				checkDiv(t, r, nx, ny)
			}
		}
		for _, s := range [][2]int{{4000, 2000}, {4001, 1999}, {6000, 1000}, {3000, 2500}} {
			checkDiv(t, r, s[0], s[1])
		}
	})
}
