package bignum

import (
	"math/big"
	"math/rand/v2"
	"strconv"
	"testing"
)

// BenchmarkKernels is bignum's products, squares, sums and divisions
// beside math/big's, of operands of 4, 16 and 64 words.
func BenchmarkKernels(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))
	for _, n := range []int{4, 16, 64} {
		x, y := randNat(r, n), randNat(r, n)
		x[n-1], y[n-1] = ^uint(0), ^uint(0)
		bx, by := toBig(x), toBig(y)
		z := make([]uint, 2*n+1)
		var bz big.Int
		w := strconv.Itoa(n)
		b.Run("mul/ours/"+w, func(b *testing.B) {
			for range b.N {
				Mul(z, x, y)
			}
		})
		b.Run("mul/big/"+w, func(b *testing.B) {
			for range b.N {
				bz.Mul(bx, by)
			}
		})
		b.Run("sqr/ours/"+w, func(b *testing.B) {
			for range b.N {
				Sqr(z, x)
			}
		})
		b.Run("add/ours/"+w, func(b *testing.B) {
			for range b.N {
				Add(z, x, y)
			}
		})
		b.Run("add/big/"+w, func(b *testing.B) {
			for range b.N {
				bz.Add(bx, by)
			}
		})
		xx := Mul(make([]uint, 2*n), x, y)
		bxx := toBig(xx)
		q, rr := make([]uint, n+2), make([]uint, n)
		var bq, br big.Int
		b.Run("div/ours/"+w, func(b *testing.B) {
			for range b.N {
				DivMod(q, rr, xx, y)
			}
		})
		b.Run("div/big/"+w, func(b *testing.B) {
			for range b.N {
				bq.QuoRem(bxx, by, &br)
			}
		})
	}
}
