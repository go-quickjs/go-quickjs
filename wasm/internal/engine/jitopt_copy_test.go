//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"strings"
	"testing"
)

// memory.copy of every constant count up to past the small copies' most,
// between addresses that overlap either way, are apart, and reach the
// memory's end and past it, with floats live across the copy in more
// registers than the copy leaves, copies what the interpreter copies and
// traps where it traps.
func TestOptCopyConstant(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17, 31, 32, 33, 47, 48, 49, 63, 64, 65, 100} {
		var sb strings.Builder
		sb.WriteString("(module (memory 1)\n  (func (export \"run\") (param $d i32) (param $s i32) (param $x f64) (result f64)\n   (local $f0 f64) (local $f1 f64) (local $f2 f64) (local $f3 f64) (local $f4 f64) (local $f5 f64) (local $f6 f64) (local $f7 f64)\n   (local $f8 f64) (local $f9 f64) (local $f10 f64) (local $f11 f64) (local $f12 f64) (local $f13 f64) (local $f14 f64) (local $f15 f64)\n")
		for k := range 16 {
			fmt.Fprintf(&sb, "   (local.set $f%d (f64.add (local.get $x) (f64.const %d)))\n", k, k)
		}
		fmt.Fprintf(&sb, "   (memory.copy (local.get $d) (local.get $s) (i32.const %d))\n", n)
		sb.WriteString("   (f64.const 0)\n")
		for k := range 16 {
			fmt.Fprintf(&sb, "   (local.get $f%d) f64.mul (f64.const %d) f64.add\n", k, k+2)
		}
		sb.WriteString("   (f64.add (f64.convert_i64_u (i64.load (i32.const 100)))))\n")
		sb.WriteString("  (func (export \"fill\") (local $i i32) (loop $l (i32.store8 (local.get $i) (i32.mul (local.get $i) (i32.const 13))) (local.set $i (i32.add (local.get $i) (i32.const 1))) (br_if $l (i32.lt_u (local.get $i) (i32.const 65536))))))\n")
		src := sb.String()
		var calls []jitCall
		calls = append(calls, jitCall{name: "fill"})
		for _, ds := range [][2]uint64{{96, 100}, {100, 96}, {100, 100}, {100, 101}, {101, 100}, {100, 100 + uint64(n)}, {0, 4000}, {4000, 0},
			{65536 - uint64(n), 0}, {0, 65536 - uint64(n)}, {65537 - uint64(n), 0}, {0, 65537 - uint64(n)}, {65536, 65536}, {65537, 0}, {0xffffffff, 0}} {
			calls = append(calls, jitCall{name: "run", args: []uint64{ds[0], ds[1], 0x3ff8000000000000}}, jitCall{name: "fill"})
		}
		t.Run(fmt.Sprint(n), func(t *testing.T) { sameAsInterpreterOffsets(t, src, calls) })
	}
}
