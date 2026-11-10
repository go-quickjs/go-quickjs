package opt

import (
	"math"
	"math/bits"
	"math/rand/v2"
	"testing"
)

// divisors are the divisors the magic numbers are tested with, of w bits:
// the small ones, those near the powers of 2, the extremes, and random ones.
func divisors(rng *rand.Rand, w uint) []uint64 {
	mask := ^uint64(0) >> (64 - w)
	var ds []uint64
	for d := uint64(1); d < 3000; d++ {
		ds = append(ds, d, -d&mask)
	}
	for k := uint(2); k < w; k++ {
		for _, d := range []uint64{1<<k - 1, 1<<k + 1, 1<<k + 3, 1<<k - 3} {
			ds = append(ds, d&mask, -d&mask)
		}
	}
	ds = append(ds, mask, mask>>1, mask>>1-1, mask>>1+2, 1000003, 0x9E3779B97F4A7C15&mask)
	for range 3000 {
		d := rng.Uint64() & mask >> rng.UintN(w)
		ds = append(ds, d, -d&mask)
	}
	return ds
}

// dividends are the dividends of w bits each divisor is tested with.
func dividends(rng *rand.Rand, w uint, d uint64) []uint64 {
	mask := ^uint64(0) >> (64 - w)
	xs := []uint64{0, 1, 2, mask, mask - 1, mask >> 1, mask>>1 + 1, mask>>1 - 1, d, d - 1, d + 1, 2 * d, 2*d - 1, -d, -d + 1, -d - 1}
	for k := range w {
		xs = append(xs, 1<<k, 1<<k-1, -(uint64(1) << k))
	}
	for range 200 {
		xs = append(xs, rng.Uint64(), rng.Uint64()>>rng.UintN(64), (d*rng.Uint64N(1<<20)+rng.Uint64N(3))-1)
	}
	for i := range xs {
		xs[i] &= mask
	}
	return xs
}

// The magic numbers of the unsigned divisions compute the quotient of
// every dividend tested, as the expansion computes it.
func TestMagicU(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for _, w := range []uint{32, 64} {
		for _, d := range divisors(rng, w) {
			if d == 0 || d&(d-1) == 0 || d >= 1<<(w-1) {
				continue
			}
			m, s, add := magicU(d, w)
			for _, x := range dividends(rng, w, d) {
				var q uint64
				switch {
				case w == 32 && add:
					q = ((x*m)>>32 + x) >> s
				case w == 32:
					q = (x * m) >> (32 + s)
				default:
					tt, _ := bits.Mul64(x, m)
					if add {
						q = ((x-tt)>>1 + tt) >> (s - 1)
					} else {
						q = tt >> s
					}
				}
				if q != x/d {
					t.Fatalf("w%d %d / %d = %d, want %d (m %#x s %d add %v)", w, x, d, q, x/d, m, s, add)
				}
			}
		}
	}
}

// mulhs is the high half of the signed product of x and y.
func mulhs(x, y int64) int64 {
	hi, _ := bits.Mul64(uint64(x), uint64(y))
	if x < 0 {
		hi -= uint64(y)
	}
	if y < 0 {
		hi -= uint64(x)
	}
	return int64(hi)
}

// The magic numbers of the signed divisions likewise.
func TestMagicS(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for _, w := range []uint{32, 64} {
		for _, ud := range divisors(rng, w) {
			d := int64(ud<<(64-w)) >> (64 - w)
			ad := uint64(d)
			if d < 0 {
				ad = -ad
			}
			if ad&(ad-1) == 0 || w == 32 && ad >= 1<<31 {
				continue
			}
			m, s := magicS(d, w)
			for _, ux := range dividends(rng, w, ad) {
				x := int64(ux<<(64-w)) >> (64 - w)
				var q int64
				if w == 32 {
					mm := int64(int32(uint32(m)))
					switch {
					case d > 0 && mm < 0:
						mm += 1 << 32
					case d < 0 && mm > 0:
						mm -= 1 << 32
					}
					q = (x * mm) >> (32 + s)
					q += int64(uint64(q) >> 63)
					q = int64(int32(q))
				} else {
					q = mulhs(x, int64(m))
					switch {
					case d > 0 && int64(m) < 0:
						q += x
					case d < 0 && int64(m) > 0:
						q -= x
					}
					q >>= s
					q += int64(uint64(q) >> 63)
				}
				want := x / d
				if w == 32 {
					want = int64(int32(x) / int32(d))
				}
				if x == math.MinInt64 && d == -1 {
					continue
				}
				if q != want {
					t.Fatalf("w%d %d / %d = %d, want %d (m %#x s %d)", w, x, d, q, want, m, s)
				}
			}
		}
	}
}
