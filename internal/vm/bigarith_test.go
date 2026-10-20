package vm

import (
	"math"
	"math/big"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A result is made in the runtime's scratch (bigTmp), then a BigInt of its
// own: each must be its own -- not the operands', nor a previous result's,
// nor the next one's -- and the operands unchanged, held in their Values or
// as BigInts of their own (heapOf), which no result of a value in the int64
// range is.
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
				v, err := r.bigOp(op, heapOf(a), heapOf(b))
				if err != nil {
					t.Fatal(err)
				}
				// A second result from the same operands must not disturb the
				// first.
				if _, err := r.bigOp(bytecode.OpMul, heapOf(a), heapOf(a)); err != nil {
					t.Fatal(err)
				}
				if got := bigIntOf(v.BigInt()); got.Cmp(&want) != 0 || bigIntOf(a).Int64() != x || bigIntOf(b).Int64() != y {
					t.Fatalf("%v %v %v = %v, want %v (operands now %v, %v)", x, op, y, got, &want, a, b)
				}
			}
		}
	}
	// A product that outgrows its two words.
	x := NewBigInt(math.MaxInt64)
	v, _ := r.bigOp(bytecode.OpMul, heapOf(x), heapOf(x))
	for i := 0; i < 3; i++ {
		v, _ = r.bigOp(bytecode.OpMul, v, v)
	}
	want := new(big.Int).Exp(big.NewInt(math.MaxInt64), big.NewInt(16), nil)
	if bigIntOf(v.BigInt()).Cmp(want) != 0 {
		t.Fatalf("(2**63-1)**16 = %v, want %v", v.BigInt(), want)
	}
}

// heapOf is b as a Value as it is: a BigInt of its own, whatever its value.
func heapOf(b *BigInt) Value { return heapBig(b) }
