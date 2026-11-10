//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// Accesses whose addresses loops' induction variables, masks and constants
// bound, near the memory's least size and past it, by steps that land on
// its end and that do not, with unsigned and signed comparisons, from
// constants and from parameters, compute what the interpreter computes,
// leave the memory as it does, and trap where it traps, at the same
// offset: an access the values keep within the memory is not checked, and
// any other still is.
func TestOptBoundsRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 14))
	sizes := []struct {
		load, store, ty string
		size            int
	}{
		{"i32.load8_u", "i32.store8", "i32", 1}, {"i32.load16_u", "i32.store16", "i32", 2},
		{"i32.load", "i32.store", "i32", 4}, {"i64.load", "i64.store", "i64", 8},
	}
	for iter := range 300 {
		pages := 1 + rng.IntN(2)
		min := pages << 16
		var sb strings.Builder
		fmt.Fprintf(&sb, "(module (memory %d 4)\n", pages)
		sb.WriteString("  (func (export \"run\") (param $n i32) (param $p i32) (result i64) (local $i i32) (local $acc i64)\n")
		for range 1 + rng.IntN(3) {
			acc := sizes[rng.IntN(len(sizes))]
			off := []int{0, 0, 1, 3, 8, 100}[rng.IntN(6)]
			step := []int{1, 1, 2, 3, 4, 8, 7}[rng.IntN(7)]
			// The bound is near the last address an access of this size
			// and offset has room for.
			bound := min - off - acc.size + 1 + rng.IntN(5) - 2
			init := fmt.Sprintf("(i32.const %d)", []int{0, 0, 1, bound - 3*step, bound + 2}[rng.IntN(5)])
			if rng.IntN(5) == 0 {
				init = "(local.get $p)"
			}
			limit := fmt.Sprintf("(i32.const %d)", bound)
			if rng.IntN(6) == 0 {
				limit = "(local.get $n)"
			}
			cmp := []string{"i32.lt_u", "i32.le_u", "i32.lt_s", "i32.le_s"}[rng.IntN(4)]
			val := "(local.get $i)"
			if acc.ty == "i64" {
				val = "(i64.extend_i32_u (local.get $i))"
			}
			fmt.Fprintf(&sb, "    (local.set $i %s)\n    (loop $l\n", init)
			if rng.IntN(2) == 0 {
				fmt.Fprintf(&sb, "      (%s offset=%d (local.get $i) (%s.add %s (%s.const 1)))\n", acc.store, off, acc.ty, val, acc.ty)
			}
			fmt.Fprintf(&sb, "      (local.set $acc (i64.add (local.get $acc) %s))\n", toI64(acc.ty, fmt.Sprintf("(%s offset=%d (local.get $i))", acc.load, off)))
			fmt.Fprintf(&sb, "      (local.set $i (i32.add (local.get $i) (i32.const %d)))\n", step)
			fmt.Fprintf(&sb, "      (br_if $l (%s (local.get $i) %s)))\n", cmp, limit)
		}
		mask := []int{0xff, 0xfffc, 0xffff, 0x1fff8, 0x7fff}[rng.IntN(5)]
		acc := sizes[rng.IntN(len(sizes))]
		fmt.Fprintf(&sb, "    (local.set $acc (i64.add (local.get $acc) %s))\n", toI64(acc.ty, fmt.Sprintf("(%s (i32.and (local.get $p) (i32.const %d)))", acc.load, mask)))
		sb.WriteString("    (local.get $acc)))\n")
		src := sb.String()
		var calls []jitCall
		for _, n := range []uint64{0, 10, uint64(min - 9), uint64(min - 1), uint64(min), uint64(min + 3)} {
			for _, p := range []uint64{0, 5, uint64(min - 4), uint64(min + 8), 0xfffffff0} {
				calls = append(calls, jitCall{name: "run", args: []uint64{n, p}})
			}
		}
		t.Run(fmt.Sprint(iter), func(t *testing.T) { sameAsInterpreterOffsets(t, src, calls) })
	}
}

// toI64 widens expression e of type ty to an i64.
func toI64(ty, e string) string {
	if ty == "i64" {
		return e
	}
	return "(i64.extend_i32_u " + e + ")"
}

// sameAsInterpreterOffsets is sameAsInterpreterTraps holding traps' offsets
// to the interpreter's too.
func sameAsInterpreterOffsets(t *testing.T, src string, calls []jitCall) {
	t.Helper()
	nat := tierInstance(t, src, 0)
	in := interpretedInstance(t, src)
	for _, cl := range calls {
		rn, en := call(t, nat, cl.name, cl.args...)
		ri, ei := call(t, in, cl.name, cl.args...)
		if !sameTraps(rn, en, ri, ei) {
			t.Fatalf("%s%#x: optimized %v %v, interpreted %v %v\n%s", cl.name, cl.args, rn, en, ri, ei, src)
		}
		if !optimized(nat, cl.name) {
			t.Fatalf("%s not optimized (%v)", cl.name, ReadJITStats().OptRefused)
		}
		for i := range in.Memories {
			if string(nat.Memories[i].Bytes()) != string(in.Memories[i].Bytes()) {
				t.Fatalf("%s%#x: memory %d differs\n%s", cl.name, cl.args, i, src)
			}
		}
	}
}
