//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// sameTraps reports whether two outcomes are the same, a trap's code and
// every frame's offset included.
func sameTraps(rn []Val, en error, ri []Val, ei error) bool {
	if !sameJITOutcome(rn, en, ri, ei) {
		return false
	}
	var tn, ti *Trap
	if !errors.As(en, &tn) || !errors.As(ei, &ti) {
		return true
	}
	if len(tn.Frames) != len(ti.Frames) {
		return false
	}
	for i := range tn.Frames {
		if tn.Frames[i].Func != ti.Frames[i].Func || tn.Frames[i].Offset != ti.Frames[i].Offset {
			return false
		}
	}
	return true
}

// The divisions and remainders by constants, which the optimizing tier
// makes of multiplications and shifts, compute what the interpreter
// computes for every kind of divisor -- powers of 2 and their negations, 2^(w-1) and
// above, the extremes, those whose magic numbers need the add -- and
// dividend, in a function of each alone, the quotient and remainder of one
// dividend together, and in a loop; a divisor of 0, and -1 for a signed
// division, still traps where it does.
func TestOptDivConst(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	divs := func(w uint) []uint64 {
		mask := ^uint64(0) >> (64 - w)
		ds := []uint64{0, 1, 2, 3, 5, 6, 7, 9, 10, 12, 25, 60, 100, 641, 1000, 1000003, mask, mask - 1, mask >> 1, mask>>1 + 1, mask>>1 + 2, mask>>1 - 1, 1<<(w/2) + 1}
		for range 12 {
			ds = append(ds, rng.Uint64()>>rng.UintN(64), 1<<rng.UintN(w))
		}
		for i, d := range ds {
			ds[i] = d & mask
			if rng.IntN(2) == 0 {
				ds = append(ds, -d&mask)
			}
		}
		return ds
	}
	for _, ty := range []string{"i32", "i64"} {
		w := uint(32)
		if ty == "i64" {
			w = 64
		}
		ds := divs(w)
		for chunk := 0; chunk < len(ds); chunk += 8 {
			var sb strings.Builder
			sb.WriteString("(module\n")
			var names []string
			for k, d := range ds[chunk:min(chunk+8, len(ds))] {
				for _, op := range []string{"div_s", "div_u", "rem_s", "rem_u"} {
					name := fmt.Sprintf("%s_%d", op, k)
					names = append(names, name)
					fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (result %s) (%s.%s (local.get $x) (%s.const %d)))\n", name, ty, ty, ty, op, ty, int64(d))
				}
				for _, sign := range []string{"s", "u"} {
					name := fmt.Sprintf("both_%s_%d", sign, k)
					names = append(names, name)
					fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (result %s) (%s.xor (%s.div_%s (local.get $x) (%s.const %d)) (%s.mul (%s.rem_%s (local.get $x) (%s.const %d)) (%s.const 31))))\n",
						name, ty, ty, ty, ty, sign, ty, int64(d), ty, ty, sign, ty, int64(d), ty)
					name = fmt.Sprintf("loop_%s_%d", sign, k)
					names = append(names, name)
					fmt.Fprintf(&sb, `  (func (export %q) (param $x %s) (result %s) (local $i i32) (local $acc %s)
    (loop $l
      (local.set $acc (%s.add (local.get $acc) (%s.div_%s (local.get $x) (%s.const %d))))
      (local.set $acc (%s.add (local.get $acc) (%s.rem_%s (local.get $x) (%s.const %d))))
      (local.set $x (%s.add (local.get $x) (%s.const 0x1234567)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 50))))
    (local.get $acc))
`, name, ty, ty, ty, ty, ty, sign, ty, int64(d), ty, ty, sign, ty, int64(d), ty, ty)
				}
			}
			sb.WriteString(")\n")
			src := sb.String()
			nat := tierInstance(t, src, 0)
			in := interpretedInstance(t, src)
			mask := ^uint64(0) >> (64 - w)
			xs := []uint64{0, 1, 2, 3, mask, mask - 1, mask >> 1, mask>>1 + 1, mask>>1 - 1, 1000003, 999999999}
			for range 40 {
				xs = append(xs, rng.Uint64()>>rng.UintN(64))
			}
			for _, name := range names {
				for _, x := range xs {
					x &= mask
					rn, en := call(t, nat, name, x)
					ri, ei := call(t, in, name, x)
					if !sameTraps(rn, en, ri, ei) {
						t.Fatalf("%s(%#x): optimized %v %v, interpreted %v %v\n%s", name, x, rn, en, ri, ei, src)
					}
				}
				if !optimized(nat, name) {
					t.Fatalf("%s not optimized", name)
				}
			}
		}
	}
}
