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
// immediates, and an element access whose key is an int32 -- a read or a
// write of numbers, a read of any value, a store of any value, a BigInt
// typed array's -- takes it as it is: negative, no index. A loop's phi of integers entered with a number past the
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
		// Elements read as references, stored whatever they are, and a
		// BigInt typed array's, by int32 keys: a binary search's
		// (lo + hi) >> 1, negative, past the length, holes.
		var objs = [], bigs = new BigInt64Array(16), words = [], sparse = ["a", , "c"]
		for (var q = 0; q < 16; q++) { objs[q] = { v: q * 7 }; bigs[q] = BigInt(q) * 7919n; words[q] = "w" + q }
		function keyObject(a, b) {
			a = a | 0; b = b | 0
			var o = objs[(a & 15) >> 1], p = objs[(a - b) | 0], s = sparse[(b & 3) | 0], w = words[(a >> 28) | 0]
			return o.v + (p === undefined ? -1 : p.v) * 100 + (s === undefined ? 5 : s.length) * 10000 + (w === undefined ? 3 : w.length) * 1000000
		}
		function keySearch(a, b) {
			var k = BigInt((a | 0) & 15) * 7919n, lo = 0, hi = 16
			while (lo < hi) { var mid = (lo + hi) >> 1; if (bigs[mid] < k) lo = mid + 1; else hi = mid }
			return lo + Number(bigs[((b | 0) & 15) >> 1]) + (bigs[(a | 0) >> 2] === undefined ? 7 : 0)
		}
		function keyStore(a, b) {
			a = a | 0; b = b | 0
			var t = [{}, "x", 1n, null]
			t[(a & 3) | 0] = "s" + a
			t[(b - a) | 0] = t
			words[(b & 15) >> 1] = { w: a }
			bigs[(a & 15) >> 1] = BigInt(b)
			bigs[(b - 2147483647) | 0] = 5n
			sparse[(a & 1) | 0] = b
			return t.length * 100 + (typeof t[a & 3]).length + Number(bigs[(a & 15) >> 1]) + (typeof words[(b & 15) >> 1]).length * 1000 + (sparse[1] === undefined ? 7 : 1)
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
	fns := []string{"add", "sub", "neg", "mul", "mulLeft", "mulBig", "u32", "chain", "shifts", "frac", "big", "loop", "cmp", "cmpu", "cmpk", "mixed", "bounded", "keyArray", "keyHeap", "keyWrite", "keyAppend", "keyGrow", "keyHole", "keyObject", "keySearch", "keyStore", "div", "mod", "modk", "negz", "mulz", "shv", "shk", "ushr"}
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

// A loop's phi of doubles that only ever merges int32s is an int32 phi
// (ssa's intPhis): its entries' numbers guarded to be int32s, not -0
// (OpCheckI32), its number boxed of the int32 where a state names it
// (mir's exitValue, ExitI32Reg), as many in general registers as a loop's
// budget allows (intPhiBudgets). Each answer is the interpreter's: loops
// entered with an int32, a fraction, -0, NaN, a number past 2**31 and a
// string; a loop of three variables rotating; eight sums read after their
// loop, past the budget; with no budget, and with the code's budget of
// back edges at 3, so that its loops leave at their headers' polls, each
// writing the frame's numbers of its int32 phis.
func TestJITInt32Phis(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `
		function sum(a, n) {
			var s = a, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) s = (s + i) | 0
			return 1 / s + s
		}
		function rot(a, n) {
			var x = a | 0, y = 1, z = 2, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) { var t = x; x = y; y = z; z = (t + y) | 0 }
			return x * 3 + y * 5 + z
		}
		function many(a, n) {
			var s0 = 0, s1 = 1, s2 = 2, s3 = 3, s4 = 4, s5 = 5, s6 = 6, s7 = 7, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) {
				s0 = (s0 + i) | 0; s1 = (s1 ^ s0) | 0; s2 = (s2 + s1) | 0; s3 = (s3 - s2) | 0
				s4 = (s4 + (s3 >> 2)) | 0; s5 = (s5 ^ s4) | 0; s6 = (s6 + s5) | 0; s7 = (s7 + (s6 & a)) | 0
			}
			return s0 + s1 + s2 + s3 + s4 + s5 + s6 + s7
		}
		function shifted(a, n) {
			var h = a, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) h = ((h << 5) - h + i) | 0
			return h
		}
		// A phi entered with the constant -0 is no int32 phi: 1 / s is
		// -Infinity where the loop does not run.
		function negzero(a, n) {
			var s = -0, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) s = (s + i) | 0
			return 1 / s
		}
		// A uint32's, past 2**31, is no int32 phi either.
		function uphi(a, n) {
			var h = a >>> 0, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) h = (h * 31 + i) >>> 0
			return h + (h > 2147483647 ? 0.5 : 0)
		}
		// Compared with a constant at each turn, NaN at the first where
		// it enters so, which the entry's guard leaves to the doubles.
		function cmpnan(a, n) {
			var x = a, c = 0, i = 0
			for (; (i | 0) < (n | 0); i = (i + 1) | 0) { if (x < 5) c = (c + 1) | 0; if (x >= -3) c = (c + 7) | 0; x = (x + 1) | 0 }
			return c * 1000 + x
		}
		function run(f, a, n) { var r = 0; for (var k = 0; k < 60; k++) r = f(a, n); return r }`
	for _, budget := range []uint16{0, 3} {
		want := New(Config{})
		r := jitRuntimeForTest(t, Config{JIT: true})
		jitDefaultsForTest(r)
		if budget != 0 {
			r.jitStress = jitStressConfig{threshold: true, budget: budget}
		}
		for _, rt := range []*Runtime{want, r} {
			if _, err := rt.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		for _, fn := range []string{"sum", "rot", "many", "shifted", "negzero", "uphi", "cmpnan"} {
			// -0 before the others an int32 phi's guard fails for: a guard
			// that failed has the entry compiled again with none.
			for _, a := range []string{"0", "5", "-3", "-0", "0.5", "NaN", "2147483648", "4294967295", "-2147483649", "9007199254740993", "Infinity", "'7'"} {
				for _, n := range []string{"0", "1", "40", "300"} {
					call := fmt.Sprintf("run(%s, %s, %s)", fn, a, n)
					wv, err := want.Run(compileForTest(t, call))
					if err != nil {
						t.Fatal(err)
					}
					gv, err := r.Run(compileForTest(t, call))
					if err != nil {
						t.Fatal(err)
					}
					if got, want := gv.Number(), wv.Number(); !(got == want || got != got && want != want) || math.Signbit(got) != math.Signbit(want) {
						t.Fatalf("budget %d, %s: got %v, interpreter %v", budget, call, got, want)
					}
				}
			}
			cl := r.global.getOwn(r.atoms.intern(fn)).value().Object().fn().closure
			if e := r.jit.cache[r.jitKeyOf(cl.fn)]; e == nil || e.ssa == nil {
				t.Fatalf("budget %d: %s did not run natively: %+v", budget, fn, e)
			}
		}
		want.Close()
		want.ReleaseClosed()
	}
}
