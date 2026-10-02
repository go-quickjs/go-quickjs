package vm

import (
	"math"
	"math/big"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A result of one-word operands keeps its digits beside it. Each must be its
// own -- not the operands', nor a previous result's -- and grow out of the
// room when it has to.
func TestBigArithKeepsSmallResultsApart(t *testing.T) {
	r := New(Config{})
	vals := []int64{0, 1, -1, 7, -7, 1 << 31, -(1 << 31), math.MaxInt64, math.MinInt64}
	ops := []bytecode.Op{bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod}
	for _, x := range vals {
		for _, y := range vals {
			for _, op := range ops {
				a, b := NewBigInt(x), NewBigInt(y)
				var want big.Int
				switch op {
				case bytecode.OpAdd:
					want.Add(big.NewInt(x), big.NewInt(y))
				case bytecode.OpSub:
					want.Sub(big.NewInt(x), big.NewInt(y))
				case bytecode.OpMul:
					want.Mul(big.NewInt(x), big.NewInt(y))
				case bytecode.OpDiv, bytecode.OpMod:
					if y == 0 {
						continue
					}
					if op == bytecode.OpDiv {
						want.Quo(big.NewInt(x), big.NewInt(y))
					} else {
						want.Rem(big.NewInt(x), big.NewInt(y))
					}
				}
				v, err := r.bigArith(op, a, b)
				if err != nil {
					t.Fatal(err)
				}
				// A second result from the same operands must not disturb the
				// first.
				if _, err := r.bigArith(bytecode.OpMul, a, a); err != nil {
					t.Fatal(err)
				}
				if got := &v.BigInt().V; got.Cmp(&want) != 0 || a.V.Int64() != x || b.V.Int64() != y {
					t.Fatalf("%v %v %v = %v, want %v (operands now %v, %v)", x, op, y, got, &want, &a.V, &b.V)
				}
			}
		}
	}
	// A product that outgrows its two words.
	x := NewBigInt(math.MaxInt64)
	v, _ := r.bigArith(bytecode.OpMul, x, x)
	for i := 0; i < 3; i++ {
		v, _ = r.bigArith(bytecode.OpMul, v.BigInt(), v.BigInt())
	}
	want := new(big.Int).Exp(big.NewInt(math.MaxInt64), big.NewInt(16), nil)
	if v.BigInt().V.Cmp(want) != 0 {
		t.Fatalf("(2**63-1)**16 = %v, want %v", &v.BigInt().V, want)
	}
}
