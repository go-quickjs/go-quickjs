package vm

import (
	"math"
	"math/big"
	"math/bits"
	"math/rand/v2"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestShortOps pins shortOp and shortBits, the operators of BigInts held in
// their Values past the int64 range, against math/big: of values about
// zero, the int64 range's edges and 2^64's, and at random, in either sign,
// and shifts by counts from -130 to 130 and the int64 range's ends; each
// result one held in its Value where it is below 2^64.
func TestShortOps(t *testing.T) {
	if bits.UintSize != 64 {
		t.Skip("the fast paths are a 64-bit machine's")
	}
	r := rand.New(rand.NewPCG(11, 12))
	var vals []*big.Int
	for _, u := range []uint64{0, 1, 2, 3, 1<<63 - 1, 1 << 63, 1<<63 + 1, 1<<64 - 1, 1<<64 - 2, 1 << 50, 1<<50 - 1} {
		for _, neg := range []bool{false, true} {
			z := new(big.Int).SetUint64(u)
			if neg {
				z.Neg(z)
			}
			vals = append(vals, z)
		}
	}
	for range 40 {
		z := new(big.Int).SetUint64(r.Uint64() >> uint(r.IntN(64)))
		if r.IntN(2) == 0 {
			z.Neg(z)
		}
		vals = append(vals, z)
	}
	check := func(what string, x, y *big.Int, got Value, want *big.Int) {
		t.Helper()
		if gotI := bigIntOf(got.BigInt()); gotI.Cmp(want) != 0 || got.isShortBig() != (want.BitLen() <= 64) {
			t.Fatalf("%v %s %v = %v (short %v), want %v", x, what, y, gotI, got.isShortBig(), want)
		}
	}
	for _, x := range vals {
		for _, y := range vals {
			a, b := valueOf(x), valueOf(y)
			for _, op := range []struct {
				op   bytecode.Op
				name string
				want func() *big.Int
			}{
				{bytecode.OpAdd, "+", func() *big.Int { return new(big.Int).Add(x, y) }},
				{bytecode.OpSub, "-", func() *big.Int { return new(big.Int).Sub(x, y) }},
				{bytecode.OpMul, "*", func() *big.Int { return new(big.Int).Mul(x, y) }},
				{bytecode.OpDiv, "/", func() *big.Int { return new(big.Int).Quo(x, y) }},
				{bytecode.OpMod, "%", func() *big.Int { return new(big.Int).Rem(x, y) }},
			} {
				if y.Sign() == 0 && (op.op == bytecode.OpDiv || op.op == bytecode.OpMod) {
					if _, ok := shortOp(op.op, a, b); ok {
						t.Fatalf("%v %s 0 made", x, op.name)
					}
					continue
				}
				v, ok := shortOp(op.op, a, b)
				if !ok {
					t.Fatalf("%v %s %v not made", x, op.name, y)
				}
				check(op.name, x, y, v, op.want())
			}
			for _, op := range []struct {
				op   bytecode.Op
				name string
				want *big.Int
			}{
				{bytecode.OpBitAnd, "&", new(big.Int).And(x, y)},
				{bytecode.OpBitOr, "|", new(big.Int).Or(x, y)},
				{bytecode.OpBitXor, "^", new(big.Int).Xor(x, y)},
			} {
				v, _ := shortBits(op.op, a, b)
				check(op.name, x, y, v, op.want)
			}
		}
		counts := []int64{math.MinInt64, math.MaxInt64, -(1 << 62)}
		for s := int64(-130); s <= 130; s++ {
			counts = append(counts, s)
		}
		for _, s := range counts {
			a, b := valueOf(x), shortBig(s)
			for _, left := range []bool{true, false} {
				op, name := bytecode.OpShl, "<<"
				if !left {
					op, name = bytecode.OpShr, ">>"
				}
				v, ok := shortBits(op, a, b)
				// x << s is x >> -s; a shift left of 64 or more is bigBits'.
				effLeft, mag := left, uint64(s)
				if s < 0 {
					effLeft, mag = !effLeft, -mag
				}
				if !ok {
					if !effLeft || mag < 64 {
						t.Fatalf("%v %s %d not made", x, name, s)
					}
					continue
				}
				var want *big.Int
				if effLeft {
					want = new(big.Int).Lsh(x, uint(mag))
				} else {
					want = new(big.Int).Rsh(x, uint(min(mag, 1000)))
				}
				check(name, x, big.NewInt(s), v, want)
			}
		}
	}
}
