//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"math"
	"testing"
)

// The ToInt32 of exact integer arithmetic -- asm.js's (a + b) | 0 -- is
// made of int32 operations, which wrap as ToInt32 of the exact result
// does (ssa's truncInts), a comparison of two int32s or two uint32s is
// theirs, and a constant's ToInt32 is an int32 constant, shifts' counts
// immediates, and an element read or write whose key is an int32 takes
// it as it is -- negative, no index. A loop's phi of integers entered with a number past the
// truncation's range -- 2**63 + 2**11, whose ToInt32 is 2048 -- is
// converted as ToInt32 has it. Each answer is the interpreter's: at the
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
		function cmp(a, b) {
			a = a | 0; b = b | 0
			return (a < b) | ((a <= b) << 1) | ((a > b) << 2) | ((a >= b) << 3) | ((a == b) << 4) | ((a != b) << 5)
		}
		function cmpu(a, b) {
			a = a >>> 0; b = b >>> 0
			return (a < b) | ((a <= b) << 1) | ((a > b) << 2) | ((a >= b) << 3) | ((a === b) << 4) | ((a !== b) << 5)
		}
		function cmpk(a, b) {
			a = a | 0
			if (a < -5) return 1
			if (3000000000 > (b >>> 0)) return 2
			if (7 === a) return 3
			if (-2147483648 >= a) return 5
			return 4
		}
		function mixed(a, b) { return ((a | 0) < (b >>> 0)) | (((a >>> 0) >= (b | 0)) << 1) }
		function bounded(a, b) {
			var x = a, s = 0
			for (var i = 0; i < 50; i = (i + 1) | 0) { s = (s + (x | 0) + (x >> 3)) | 0; x = (x + b) | 0 }
			return s
		}
		var arr = [10, 11, 12, 13, 14, 15, 16, 17], heap = new Int32Array(64), bytes = new Uint8Array(16)
		for (var q = 0; q < 64; q++) heap[q] = q * 1000003
		function keyArray(a, b) { a = a | 0; var x = arr[a & 7], y = arr[(a - b) | 0]; return (y === undefined ? -1 : y) + x }
		function keyHeap(a, b) { a = a | 0; b = b | 0; return heap[(a & 255) >> 2] + (heap[(a + b) | 0] | 0) }
		function keyWrite(a, b) {
			a = a | 0; b = b | 0
			heap[(a & 252) >> 2] = (a * 3) | 0
			bytes[(b & 15) | 0] = a
			heap[(b - 1) | 0] = 7
			arr[(b & 7) | 0] = a
			arr[(a - 2147483647) | 0] = 9
			return (heap[(a & 252) >> 2] + bytes[b & 15] + arr[b & 7] + heap[63]) | 0
		}
		function keyAppend(a, b) {
			a = a | 0
			var t = [1, 2, 3]
			t[((a & 3) + 3) | 0] = a
			t[(a & 1) | 0] = b
			return t.length * 100 + (t[3] === undefined ? 0 : t[3]) + t[a & 1]
		}
		var grown = []
		function keyGrow(a, b) {
			if (grown.length > 500) grown.length = 0
			grown[grown.length | 0] = a
			grown[((grown.length | 0) + 1) | 0] = b
			return grown.length * 1000 + Object.keys(grown).length
		}
		var holes = [1, 2, 3]
		function keyHole(a, b) {
			delete holes[1]
			holes[(a & 1) | 0] = (a + 1) | 0
			return (holes[1] === undefined ? -5 : holes[1]) + holes.length
		}
		// What is no ring operation stays the doubles': a quotient, a
		// remainder -- -0, NaN, -2**31 / -1 -- a negation or a product that
		// is -0, a shift's count past 31 or negative, an unsigned shift past
		// 2**31 added to its operand.
		function div(a, b) { a = a | 0; b = b | 0; return ((a / b) | 0) + ((a / 3) | 0) * 7 }
		function mod(a, b) { a = a | 0; b = b | 0; return a % b }
		function modk(a, b) { a = a | 0; return ((a % 3) | 0) * 1000 + (a % -2) }
		function negz(a, b) { a = a | 0; return -a }
		function mulz(a, b) { a = a | 0; return a * -1 + a * 0 }
		function shv(a, b) { a = a | 0; b = b | 0; return (a << b) ^ (a >> (b + 32)) ^ (a >>> (b - 1)) }
		function shk(a, b) { a = a | 0; return (a << 32) + (a >> 33) * 3 + (a >>> 63) * 5 + (a >>> 32) }
		function ushr(a, b) { a = a | 0; return (a >>> 0) + ((a >>> 1) | 0) - (a >>> 31) }
		function run(f, a, b) { var r = 0; for (var k = 0; k < 400; k++) r = f(a, b); return r }`
	fns := []string{"add", "sub", "neg", "mul", "mulLeft", "mulBig", "u32", "chain", "shifts", "frac", "big", "loop", "cmp", "cmpu", "cmpk", "mixed", "bounded", "keyArray", "keyHeap", "keyWrite", "keyAppend", "keyGrow", "keyHole", "div", "mod", "modk", "negz", "mulz", "shv", "shk", "ushr"}
	args := []string{"0", "1", "-1", "2147483647", "-2147483648", "4294967295", "2147483648", "1.5", "-0", "NaN", "Infinity", "-7", "1e300", "9223372036854777856", "-Infinity"}
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
			for _, b := range []string{"3", "-2147483648", "2147483647", "0.25", "0", "-1", "33"} {
				call := fmt.Sprintf("run(%s, %s, %s)", fn, a, b)
				wv, err := want.Run(compileForTest(t, call))
				if err != nil {
					t.Fatal(err)
				}
				gv, err := r.Run(compileForTest(t, call))
				if err != nil {
					t.Fatal(err)
				}
				if got, want := gv.Number(), wv.Number(); !(got == want || got != got && want != want) || math.Signbit(got) != math.Signbit(want) {
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
