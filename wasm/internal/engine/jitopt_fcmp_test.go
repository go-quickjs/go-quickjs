//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Branches on float comparisons, which the optimizing tier makes of the
// comparison's flags, go where the interpreter's go for every pair of
// operands -- NaNs either side, zeros of both signs, infinities, equal and
// adjacent values -- each comparison of both widths, branching either way
// and in a loop's test, against a constant too.
func TestOptFloatBranches(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("(module\n")
	var names []string
	for _, ty := range []string{"f32", "f64"} {
		for _, op := range []string{"eq", "ne", "lt", "gt", "le", "ge"} {
			name := ty + "_" + op
			names = append(names, name+"_if", name+"_br", name+"_loop", name+"_const")
			fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (param $y %s) (result i32) (if (result i32) (%s.%s (local.get $x) (local.get $y)) (then (i32.const 7)) (else (i32.const 9))))\n", name+"_if", ty, ty, ty, op)
			fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (param $y %s) (result i32) (block $b (br_if $b (%s.%s (local.get $x) (local.get $y))) (return (i32.const 3))) (i32.const 5))\n", name+"_br", ty, ty, ty, op)
			fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (param $y %s) (result i32) (local $n i32) (loop $l (local.set $n (i32.add (local.get $n) (i32.const 1))) (local.set $x (%s.add (local.get $x) (%s.const 1))) (br_if $l (i32.and (%s.%s (local.get $x) (local.get $y)) (i32.lt_u (local.get $n) (i32.const 10))))) (local.get $n))\n",
				name+"_loop", ty, ty, ty, ty, ty, op)
			fmt.Fprintf(&sb, "  (func (export %q) (param $x %s) (param $y %s) (result i32) (if (result i32) (%s.%s (local.get $x) (%s.const 1.5)) (then (i32.const 1)) (else (i32.const 2))))\n", name+"_const", ty, ty, ty, op, ty)
		}
	}
	sb.WriteString(")\n")
	src := sb.String()
	f64s := []float64{0, math.Copysign(0, -1), 1, 1.5, -1.5, 2, math.Inf(1), math.Inf(-1), math.NaN(), math.Nextafter(1.5, 2), 5}
	var calls []jitCall
	for _, name := range names {
		for _, x := range f64s {
			for _, y := range f64s {
				var a, b uint64
				if strings.HasPrefix(name, "f32") {
					a, b = uint64(math.Float32bits(float32(x))), uint64(math.Float32bits(float32(y)))
				} else {
					a, b = math.Float64bits(x), math.Float64bits(y)
				}
				calls = append(calls, jitCall{name: name, args: []uint64{a, b}})
			}
		}
	}
	sameAsInterpreterOffsets(t, src, calls)
}
