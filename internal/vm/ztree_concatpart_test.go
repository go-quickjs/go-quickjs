package vm

import (
	"runtime"
	"testing"
)

// A + chain whose operands are not string literals is compiled as a concat
// of the parts, each operand converted by an addition to the empty string
// (internal/compiler's compileStringChain). The tree tier takes a number
// part to the join as the number it is, which writes its text where it
// belongs, rather than through the string that addition made of it and the
// join copied: every kind of part answers as the additions did, in the
// order they converted them, and a number part allocates nothing.
func TestTreeConcatPartsAnswers(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function hot(f) { var v; for (var i = 0; i < 3000; i++) v = f(i); return String(v) }
		var order = []
		function val(n) { return {valueOf: function () { order.push(n); return n }} }
		;[
			hot(function (i) { return "a" + i + "b" }),
			hot(function (i) { return "a" + -0 + "b" + NaN + "c" + Infinity + "d" + -Infinity }),
			hot(function (i) { return "a" + 1e21 + "b" + 1e-7 + "c" + 0.1 + "d" + 1/3 + "e" + 5e-324 }),
			hot(function (i) { return "a" + (i|0) + "b" + 10000000000000000000000n + "c" }),
			hot(function (i) { return "a" + true + "b" + false + "c" + null + "d" + undefined }),
			hot(function (i) { return "a" + {valueOf: function () { return 7 }, toString: function () { return "S" }} + "b" }),
			hot(function (i) { return "a" + {toString: function () { return "T" }} + "b" + [1,2] + "c" + {} }),
			hot(function (i) { return "a" + {[Symbol.toPrimitive]: function (h) { return "P:" + h }} + "b" }),
			hot(function (i) { return 1 + 2 + "a" + 3 + 4 }),
			hot(function (i) { return "" + i + "" }),
			hot(function (i) { return "\ud83d" + "\ude00" + i }),
			(function () { order = []; var s = "x" + val(1) + val(2) + val(3); return s + "|" + order.join(",") })(),
			(function () { try { return "" + "a" + Symbol("s") } catch (e) { return e.constructor.name } })()
		].join(" ")`))
	if err != nil {
		t.Fatal(err)
	}
	want := `a2999b a0bNaNcInfinityd-Infinity a1e+21b1e-7c0.1d0.3333333333333333e5e-324 ` +
		`a2999b10000000000000000000000c atruebfalsecnulldundefined a7b aTb1,2c[object Object] ` +
		`aP:defaultb 3a34 2999 ` + "\U0001f600" + `2999 x123|1,2,3 TypeError`
	if got := v.String().Go(); got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}

// A number part of such a chain costs no allocation of its own: the join
// writes its text. The string the chain makes is the only allocation each
// iteration, which is what makes 10,000 of them stay near 10,000.
func TestTreeConcatPartsAllocations(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function chain(n) { var t; for (var i = 0; i < n; i++) t = "item-" + i + ";"; return t }
		chain(2000)`)); err != nil {
		t.Fatal(err)
	}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	v, err := r.Run(compileForTest(t, `chain(10000)`))
	runtime.ReadMemStats(&b)
	if err != nil || v.String().Go() != "item-9999;" {
		t.Fatalf("= %v, %v", v, err)
	}
	// One string an iteration, with room for what the test's own call
	// path allocates. Both parts converted through a string of their own
	// had made 20,000.
	if n := b.Mallocs - a.Mallocs; n > 12000 {
		t.Errorf("10,000 chains of a string, a number and a string made %d allocations, want about 10,000", n)
	}
}
