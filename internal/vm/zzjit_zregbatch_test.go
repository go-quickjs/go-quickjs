//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regBatchSrc runs what register code makes natively that it had left to
// Go or to the interpreter: a call inlined whose result goes to a register
// a closure captures; private fields read and written, a private method
// read and called, a private getter that throws now and then, an object of
// another class's brand; literals whose fields Go defines from native code
// (a spread, a computed key); calls in tail position, shallow and deep, of the
// function itself and of another; BigInt.asUintN of wrapped operators and
// BigInt(x); loops of built-ins' calls over arrays and typed arrays --
// forEach, reduce, map, filter, some, every, find, findIndex -- with a
// hole, a callback that throws, one that changes, one that is no function,
// and an array that grows under the loop; and a construction learned in a
// loop that runs a whole invocation natively.
const regBatchSrc = `
function Box(v) { this.v = v }
Box.prototype.get = function () { return this.v };
function capturedResult(n) {
	var b = new Box(3), v = 0, f = function () { return v * 2 }, s = 0;
	for (var i = 0; i < n; i++) { b.v = i; v = b.get(); s += f() }
	return s;
}
class Counter {
	#n = 0; #k = 1;
	#get() { return this.#n }
	get #odd() { if (this.#n % 97 == 96) throw new RangeError("odd " + this.#n); return this.#n & 1 }
	add(i) { this.#n = (this.#n + i + this.#k) | 0; return this.#n }
	sum() { return this.#get() + this.#n }
	odd() { return this.#odd }
	static has(o) { return #n in o }
}
class Other { #n = 5; read(o) { return o.#n } }
function privates(n) {
	var c = new Counter(), s = 0, e = 0, o = new Other();
	for (var i = 0; i < n; i++) {
		s = (s + c.add(i) + c.sum()) | 0;
		try { s += c.odd() } catch (err) { e++ }
		if (i % 50 == 49) { try { o.read(c) } catch (err) { e += 10 } }
	}
	return s + ":" + e + ":" + Counter.has(c);
}
function tailSelf(n, acc) {
	"use strict";
	if (n === 0) return acc;
	return tailSelf(n - 1, (acc + n) | 0);
}
function tailOther(n, acc) {
	"use strict";
	if (n === 0) return acc;
	return tailSelf(n - 1, (acc ^ n) | 0);
}
function tails(n) {
	var s = 0;
	for (var i = 0; i < n; i++) s = (s + tailSelf(i % 50, i) + tailOther(i % 7, i) + tailSelf(200, i)) | 0;
	return s;
}
function bigints(n) {
	var x = 88172645463325252n, h = 0xcbf29ce484222325n, t = "fox", y = 0n;
	for (var i = 0; i < n; i++) {
		x ^= BigInt.asUintN(64, x << 13n);
		x ^= x >> 7n;
		x ^= BigInt.asUintN(64, x << 17n);
		h = BigInt.asUintN(64, (h ^ BigInt(t.charCodeAt(i % 3))) * 0x100000001b3n);
		y = BigInt.asIntN(32, y + BigInt(i));
		if (i == n - 3) y = BigInt.asIntN(32, y * 123456789123n);
	}
	return x + ":" + h + ":" + y;
}
function loops(n) {
	var a = [], h = new Array(64), t = new Int32Array(64), s = 0, acc = { v: 0, j: 0 }, f = function (x, i) { return x + i };
	for (var i = 0; i < 64; i++) { a.push(i); t[i] = i * 3; if (i != 10) h[i] = i }
	var each = function (x, i, arr) {
		acc.v += x & i;
		if (acc.j == n - 4 && x == 3) arr.push(99);
		if (acc.j == n - 6 && i == 29) arr[30] = "30";
		if (acc.j == n - 5 && i == 29) arr[30] = 30;
		if (acc.j == n - 3 && x == 20) throw new Error("e");
	};
	for (var j = 0; j < n; j++) {
		acc.j = j;
		s += a.reduce(function (acc, x) { return acc + x }, j);
		try { (j % 100 == 7 ? h : a).forEach(each) } catch (e) { s += 1000 }
		s += a.map(f).length + a.filter(function (x) { return x & 1 }).length;
		s += a.some(function (x) { return x === 40 }) + a.every(function (x) { return x < 64 });
		s += a.find(function (x) { return x > j % 60 }) + a.findIndex(function (x) { return x == 33 });
		s += t.reduce(function (acc, x) { return acc + x }, 0) + t.map(function (x) { return x + 1 })[5];
		s += t.some(function (x) { return x == 27 }) + t.filter(function (x) { return x & 2 }).length;
		if (j == n - 2) { try { a.forEach(7) } catch (e) { s += 2000 } }
	}
	return s + acc.v + a.length;
}
function spreads(n) {
	var o = { a: 1, b: 2 }, a = [1, 2, 3], s = 0;
	function f(x, y, z) { return x + y + z }
	for (var i = 0; i < n; i++) {
		var c = { ...o, d: i }, b = [...a, i];
		s += c.a + c.d + b[3] + f(...a) + { ["k" + (i % 3)]: i }["k" + (i % 3)];
	}
	return s;
}
function Pt(x, y) { this.x = x; this.y = y }
function constructs(n) {
	var s = 0, p;
	for (var j = 0; j < n; j++) {
		if (j & 1) p = new Pt(j, 1); else p = new Pt(1, j & 15);
		s += p.x * p.y;
	}
	return s;
}
`

// TestJITRegBatch runs regBatchSrc's functions in a runtime whose register
// code makes them natively and in one with no JIT, the collector idle and
// marking, calls of Go clobbering what they may, with inlining and
// without: the answers equal, and no compile refused for a panic.
func TestJITRegBatch(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, inline := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("inline=%v/marking=%v", inline, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				if !inline {
					regNoInline(t)
				}
				jitClobberCallsForTest(t)
				rts := regNativeRuntimes(t, regBatchSrc)
				for _, name := range []string{"capturedResult", "privates", "spreads", "tails", "bigints", "loops", "constructs"} {
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						for range 6 {
							v, err := r.Call(fn, Undefined, []Value{Float(400)})
							got[i] = regNativeShow(r, v, err)
						}
					}
					if got[0] != got[1] {
						t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
					}
				}
				for k, e := range rts[0].jit.regCache {
					// A compile's panic is a bug, which a refusal hides.
					if fn := k.Value(); fn != nil && strings.Contains(e.refusal, "runtime error") {
						t.Errorf("%s: %s", fn.Name, e.refusal)
					}
				}
			})
		}
	}
}

// TestJITRegBatchNative checks that what regBatchSrc makes natively stays
// native: private members read and written with no exit and no call of Go
// once compiled, but a getter's, which Go calls; built-ins' loops made by
// the code, their callbacks called natively; a construction learned in a
// loop made natively after its code is compiled again.
func TestJITRegBatchNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regBatchSrc)
	r := rts[0]
	// Without QJS_JIT_STRESS's budgets and deoptimizations, whose exits
	// these counts are not written for: TestJITRegBatch runs the same
	// program under them, its answers checked.
	r.jitStress = jitStressConfig{}
	r.jitState().reporting = true
	for _, name := range []string{"privates", "loops", "constructs", "bigints"} {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		for range 6 {
			if _, err := r.Call(fn, Undefined, []Value{Float(2000)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	st := r.RegNativeStats()
	t.Logf("calls of Go %v, hosts %v, loops finished by Go %d", st.GoCallOps, st.HostOps, r.jit.loopsFinished)
	for op, n := range st.GoCallOps {
		// Each call of Go made until the code was compiled for what it
		// learned, or that left: some hundreds over the runs, not one for
		// each of the 12,000 turns -- but the private getter's, which Go
		// calls by design.
		if n > 3000 && op != "whole get_private" {
			t.Errorf("%s made by Go %d times", op, n)
		}
	}
	for op, n := range st.HostOps {
		if n > 1000 {
			t.Errorf("%s left to Go %d times", op, n)
		}
	}
	for _, m := range []string{"forEach", "reduce", "map", "filter", "some", "every"} {
		// Made by Go until the code was compiled with its loop, and where
		// the loop's checks fail before it begins (a callback that is no
		// function): a few times, not once for each of the 12,000 turns.
		if n := st.GoCallOps["call_method_w->"+m]; n > 50 {
			t.Errorf("%s made by Go %d times", m, n)
		}
	}
}

// regUnmadeInlineSrc gives an inlined callee an object literal escape
// analysis leaves unmade, whose property the callee reads as a number
// where it is a string now and then: the exit inside the callee writes
// the word naming the object to the callee's window, which Go makes the
// object of, as the frame's own registers' (jitRegMaterializeSpans).
const regUnmadeInlineSrc = `
function str(o) { return o.x + "!" }
function callsGo(n) { var s = 0; for (var i = 0; i < n; i++) s += str(i % 3 ? { x: i } : { x: "k" + i }).length; return s }
`

// TestJITRegUnmadeInInlined runs regUnmadeInlineSrc with the JIT and
// without, the interpreter's Go handlers filling the caches (as a
// verifying build's do), the collector idle and marking: the answers
// equal. The objects in the inlined callee's window had been left unmade:
// it read a property of undefined.
func TestJITRegUnmadeInInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	was := regNativeOn
	regNativeOn = false
	t.Cleanup(func() { regNativeOn = was })
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regUnmadeInlineSrc)
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("callsGo"))
				for range 8 {
					v, err := r.Call(fn, Undefined, []Value{Float(600)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Errorf("with the JIT %q, without %q", got[0], got[1])
			}
		})
	}
}
