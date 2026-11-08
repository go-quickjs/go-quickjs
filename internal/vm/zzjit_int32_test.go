//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"math"
	"testing"
)

// The ToInt32 of exact integer arithmetic -- asm.js's (a + b) | 0 -- is
// made of int32 operations, which wrap as ToInt32 of the exact result
// does (ssa's truncInts), and a constant's ToInt32 is an int32 constant,
// shifts' counts immediates. Each answer is the interpreter's: at the
// edges where the int32 result wraps, for unsigned operands, for products
// by constants, and where the doubles' result is not exact -- a product
// or a sum past 2**53, which stays the doubles' -- or not an integer.
func TestJITTruncatedIntArithmetic(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `
		function add(a, b) { a = a | 0; b = b | 0; return (a + b) | 0 }
		function sub(a, b) { a = a | 0; b = b | 0; return (a - b) | 0 }
		function neg(a, b) { a = a | 0; return (-a) | 0 }
		function mul(a, b) { a = a | 0; return (a * 65537 + 3) | 0 }
		function mulLeft(a, b) { a = a | 0; return (-3 * a - b) | 0 }
		function mulBig(a, b) { a = a | 0; return (a * 1073741825) | 0 }
		function u32(a, b) { return ((a >>> 0) + (b >>> 0)) | 0 }
		function chain(a, b) { a = a | 0; b = b | 0; return (a + b - (a << 3) + 2147483647 - (b >>> 1)) | 0 }
		function shifts(a, b) { a = a | 0; return ((a << 24) >> 24) ^ (a >>> 28) ^ (1 << 31 >> 3) }
		function frac(a, b) { a = a | 0; return (a + 0.5) | 0 }
		function big(a, b) { a = a | 0; b = b | 0; return (a + 9007199254740991 + b) | 0 }
		function loop(a, b) {
			var s = 0, i = 0
			for (; (i | 0) < 64; i = (i + 1) | 0) s = (s + i * 3 - (a | 0) + (b >>> 3)) | 0
			return s | 0
		}
		function run(f, a, b) { var r = 0; for (var k = 0; k < 400; k++) r = f(a, b); return r }`
	fns := []string{"add", "sub", "neg", "mul", "mulLeft", "mulBig", "u32", "chain", "shifts", "frac", "big", "loop"}
	args := []string{"0", "1", "-1", "2147483647", "-2147483648", "4294967295", "2147483648", "1.5", "-0", "NaN", "Infinity", "-7", "1e300"}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for _, fn := range fns {
		for _, a := range args {
			for _, b := range []string{"3", "-2147483648", "2147483647", "0.25"} {
				call := fmt.Sprintf("run(%s, %s, %s)", fn, a, b)
				wv, err := want.Run(compileForTest(t, call))
				if err != nil {
					t.Fatal(err)
				}
				gv, err := r.Run(compileForTest(t, call))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.Number(), wv.Number(); got != want || math.Signbit(got) != math.Signbit(want) {
					t.Fatalf("%s: got %v, interpreter %v", call, got, want)
				}
			}
		}
		cl := r.global.getOwn(r.atoms.intern(fn)).value().Object().fn().closure
		if e := r.jit.cache[r.jitKeyOf(cl.fn)]; e == nil || e.ssa == nil {
			t.Fatalf("%s did not run natively: %+v", fn, e)
		}
	}
}
