package bignum

import (
	"math"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

// math/big is the oracle: each operation's every result, at every size
// from none to past Karatsuba's threshold and the conversions' split, of
// words that carry and borrow through -- all ones, a lone top bit, zero --
// is math/big's.

func toBig(x []uint) *big.Int {
	w := make([]big.Word, len(x))
	for i, v := range x {
		w[i] = big.Word(v)
	}
	return new(big.Int).SetBits(w)
}

func fromBig(b *big.Int) []uint {
	w := b.Bits()
	x := make([]uint, len(w))
	for i, v := range w {
		x[i] = uint(v)
	}
	return Norm(x)
}

func signed(neg bool, x []uint) *big.Int {
	b := toBig(x)
	if neg {
		b.Neg(b)
	}
	return b
}

// randNat is a magnitude of n words, nasty ones often.
func randNat(r *rand.Rand, n int) []uint {
	x := make([]uint, n)
	for i := range x {
		switch r.IntN(6) {
		case 0:
			x[i] = ^uint(0)
		case 1:
			x[i] = 0
		case 2:
			x[i] = 1 << (W - 1)
		case 3:
			x[i] = uint(r.IntN(3))
		default:
			x[i] = uint(r.Uint64())
		}
	}
	if n > 0 && x[n-1] == 0 {
		x[n-1] = 1 + uint(r.IntN(1000))
	}
	return x
}

// sizes are the word counts the tests take operands of.
var sizes = []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 39, 40, 41, 63, 80, 81, 120}

func check(t *testing.T, what string, got []uint, want *big.Int) {
	t.Helper()
	if len(got) > 0 && got[len(got)-1] == 0 {
		t.Fatalf("%s: not normalized: %v", what, got)
	}
	if toBig(got).Cmp(want) != 0 {
		t.Fatalf("%s = %v, want %v", what, toBig(got), want)
	}
}

func TestArithmetic(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range sizes {
		for _, m := range sizes {
			for range 4 {
				x, y := randNat(r, n), randNat(r, m)
				bx, by := toBig(x), toBig(y)
				check(t, "add", Add(make([]uint, max(n, m)+1), x, y), new(big.Int).Add(bx, by))
				if Cmp(x, y) != bx.Cmp(by) {
					t.Fatalf("cmp %v %v", bx, by)
				}
				if bx.Cmp(by) >= 0 {
					check(t, "sub", Sub(make([]uint, n), x, y), new(big.Int).Sub(bx, by))
				}
				check(t, "mul", Mul(make([]uint, MulRoom(n, m)), x, y), new(big.Int).Mul(bx, by))
				if m > 0 {
					q, rem := DivMod(make([]uint, QuoRoom(n, m)), make([]uint, m), x, y)
					check(t, "quo", q, new(big.Int).Quo(bx, by))
					check(t, "rem", rem, new(big.Int).Rem(bx, by))
				}
				// In place.
				z := make([]uint, max(n, m)+1)
				copy(z, x)
				check(t, "add in place", Add(z, z[:n], y), new(big.Int).Add(bx, by))
			}
			x := randNat(r, n)
			check(t, "sqr", Sqr(make([]uint, 2*n), x), new(big.Int).Mul(toBig(x), toBig(x)))
		}
	}
}

func TestWords(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for _, n := range sizes {
		x := randNat(r, n)
		bx := toBig(x)
		for _, w := range []uint{0, 1, 2, 7, 10, 1 << (W - 1), ^uint(0), uint(r.Uint64())} {
			bw := new(big.Int).SetUint64(uint64(w))
			check(t, "addword", AddWord(make([]uint, n+1), x, w), new(big.Int).Add(bx, bw))
			check(t, "mulword", MulWord(make([]uint, n+1), x, w), new(big.Int).Mul(bx, bw))
			check(t, "muladdword", MulAddWord(make([]uint, n+1), x, w, 3), new(big.Int).Add(new(big.Int).Mul(bx, bw), big.NewInt(3)))
			if bx.Cmp(bw) >= 0 {
				check(t, "subword", SubWord(make([]uint, n), x, w), new(big.Int).Sub(bx, bw))
			}
			if w != 0 {
				q, rem := DivWord(make([]uint, n), x, w)
				check(t, "divword", q, new(big.Int).Quo(bx, bw))
				if rem != uint(new(big.Int).Rem(bx, bw).Uint64()) || ModWord(x, w) != rem {
					t.Fatalf("remword %v %v", bx, w)
				}
			}
		}
		for _, s := range []uint{0, 1, 5, W - 1, W, W + 1, 3*W + 7, 200} {
			check(t, "shl", Shl(make([]uint, ShlRoom(n, s)), x, s), new(big.Int).Lsh(bx, s))
			check(t, "shr", Shr(make([]uint, n), x, s), new(big.Int).Rsh(bx, s))
			for _, neg := range []bool{false, true} {
				sn, sz := Sar(make([]uint, SarRoom(n)), neg, x, s)
				check(t, "sar", sz, new(big.Int).Abs(new(big.Int).Rsh(signed(neg, x), s)))
				if want := new(big.Int).Rsh(signed(neg, x), s); (want.Sign() < 0) != sn {
					t.Fatalf("sar sign %v %v", signed(neg, x), s)
				}
			}
		}
		if BitLen(x) != bx.BitLen() {
			t.Fatalf("bitlen %v", bx)
		}
	}
}

func TestBitwise(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for _, n := range sizes {
		for _, m := range sizes {
			x, y := randNat(r, n), randNat(r, m)
			for _, xn := range []bool{false, true} {
				for _, yn := range []bool{false, true} {
					if n == 0 && xn || m == 0 && yn {
						continue
					}
					bx, by := signed(xn, x), signed(yn, y)
					for _, op := range []struct {
						name string
						f    func([]uint, bool, []uint, bool, []uint) (bool, []uint)
						want *big.Int
					}{
						{"and", And, new(big.Int).And(bx, by)},
						{"or", Or, new(big.Int).Or(bx, by)},
						{"xor", Xor, new(big.Int).Xor(bx, by)},
					} {
						neg, z := op.f(make([]uint, max(n, m)+1), xn, x, yn, y)
						if got := signed(neg, z); got.Cmp(op.want) != 0 || neg && len(z) == 0 {
							t.Fatalf("%v %s %v = %v, want %v", bx, op.name, by, got, op.want)
						}
					}
					neg, z := Not(make([]uint, n+1), xn, x)
					if got := signed(neg, z); got.Cmp(new(big.Int).Not(bx)) != 0 {
						t.Fatalf("~%v = %v", bx, got)
					}
				}
			}
		}
	}
}

func TestConversions(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for _, n := range append(sizes, 200, 500) {
		x := randNat(r, n)
		bx := toBig(x)
		for base := 2; base <= 36; base++ {
			for _, neg := range []bool{false, true} {
				want := signed(neg && n > 0, x).Text(base)
				got := string(Append([]byte("pre"), neg && n > 0, x, base))
				if got != "pre"+want {
					t.Fatalf("%v in base %d: %s, want %s", bx, base, got, want)
				}
			}
			p, ok := Parse(bx.Text(base), base)
			if !ok {
				t.Fatalf("parse %s in base %d", bx.Text(base), base)
			}
			check(t, "parse", p, bx)
			if base > 10 {
				p, _ = Parse(strings.ToUpper(bx.Text(base)), base)
				check(t, "parse upper", p, bx)
			}
		}
	}
	for _, s := range []string{"", "12a", "-1", " 1", "1_0"} {
		if _, ok := Parse(s, 10); ok {
			t.Fatalf("parsed %q", s)
		}
	}
	// Leading zeros are digits too.
	p, _ := Parse("000123", 10)
	check(t, "zeros", p, big.NewInt(123))
}

func TestFloats(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	near := []uint64{1<<53 - 1, 1 << 53, 1<<53 + 1, 1<<54 + 1, 1<<54 + 2, 1<<54 + 3, 1<<63 + 1<<10, 1<<63 + 1<<10 + 1, 1<<64 - 1}
	var xs [][]uint
	for _, u := range near {
		xs = append(xs, fromBig(new(big.Int).SetUint64(u)))
		xs = append(xs, fromBig(new(big.Int).Lsh(new(big.Int).SetUint64(u), 70)))
		xs = append(xs, fromBig(new(big.Int).Add(new(big.Int).Lsh(new(big.Int).SetUint64(u), 70), big.NewInt(1))))
	}
	for _, n := range sizes {
		xs = append(xs, randNat(r, n))
	}
	xs = append(xs, fromBig(new(big.Int).Lsh(big.NewInt(1), 1024)), fromBig(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 1024), new(big.Int).Lsh(big.NewInt(1), 970))))
	for _, x := range xs {
		want, _ := new(big.Float).SetInt(toBig(x)).Float64()
		if got := Float64(x); got != want {
			t.Fatalf("float of %v: %v, want %v", toBig(x), got, want)
		}
		if !math.IsInf(want, 0) {
			back, neg, ok := FromFloat64(-want)
			wb, _ := new(big.Float).SetFloat64(want).Int(nil)
			if !ok || neg != (want != 0) || toBig(back).Cmp(wb) != 0 {
				t.Fatalf("from %v: %v %v %v", -want, toBig(back), neg, ok)
			}
		}
	}
	for _, f := range []float64{0.5, math.NaN(), math.Inf(1), -1.25} {
		if _, _, ok := FromFloat64(f); ok {
			t.Fatalf("converted %v", f)
		}
	}
}

func FuzzDivMod(f *testing.F) {
	f.Add([]byte{1, 2, 3}, []byte{4, 5})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		bx, by := new(big.Int).SetBytes(a), new(big.Int).SetBytes(b)
		if by.Sign() == 0 {
			return
		}
		x, y := fromBig(bx), fromBig(by)
		q, r := DivMod(make([]uint, QuoRoom(len(x), len(y))), make([]uint, len(y)), x, y)
		check(t, "quo", q, new(big.Int).Quo(bx, by))
		check(t, "rem", r, new(big.Int).Rem(bx, by))
		check(t, "mul", Mul(make([]uint, MulRoom(len(x), len(y))), x, y), new(big.Int).Mul(bx, by))
	})
}

// TestMulSubRow pins the division's kernel: z - x*d, its low len(x) words
// written to z and the word to subtract from the one above returned, of
// words that carry and borrow through, against math/big.
func TestMulSubRow(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 9))
	for _, n := range sizes {
		for it := 0; it < 200; it++ {
			x, z := randNat(r, n), randNat(r, n)
			d := []uint{^uint(0), 1, 1 << (W - 1), uint(r.Uint64())}[it%4]
			z0 := toBig(z)
			c := mulSubRow(z, x, d)
			// z0 - x*d == z - c*2^(W*n)
			lhs := new(big.Int).Sub(z0, new(big.Int).Mul(toBig(x), toBig([]uint{d})))
			rhs := new(big.Int).Sub(toBig(z), new(big.Int).Lsh(toBig([]uint{c}), uint(W*n)))
			if lhs.Cmp(rhs) != 0 {
				t.Fatalf("n=%d: z - x*d = %v, got z %v and %v over", n, lhs, toBig(z), c)
			}
		}
	}
}
