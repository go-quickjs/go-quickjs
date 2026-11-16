//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regWholeSrc runs register instructions Go runs whole for native code
// (jitRegWhole): private members' reads and writes, a private getter that
// assigns a captured binding and one that throws now and then, caught in
// the same frame or not; a for...in's and a for...of's cursor; object and
// array spreads, a spread call and a computed key's definition; a private
// read into a binding a closure captures; a native callee's private read;
// and a getter that changes a global the code was compiled to read as a
// constant, which finds the code invalid after the call.
const regWholeSrc = `
var seen = 0;
var K = 3;
class P {
	#x = 1; #y = 2;
	get #g() { seen++; return this.#x + 1 }
	get #t() { if (this.#x % 37 == 36) throw new RangeError("t " + this.#x); return this.#x }
	get #k() { if (this.#x == 400) K = 4; return this.#x }
	step(i) { this.#x = this.#x + i % 5; this.#y = this.#x ^ i; return this.#y }
	read() { return this.#x }
	g() { return this.#g }
	t() { return this.#t }
	k() { return this.#k }
}
function privates(n) {
	var p = new P(), s = 0;
	for (var i = 0; i < n; i++) s = (s + p.step(i) + p.read() + p.g()) % 1000003;
	return s + ":" + seen;
}
function caught(n) {
	var p = new P(), s = 0, e = "";
	for (var i = 0; i < n; i++) {
		p.step(1);
		try { s += p.t() } catch (err) { e = err.message }
	}
	return s + e;
}
function uncaught(n) {
	var p = new P(), s = 0;
	for (var i = 0; i < n; i++) { p.step(1); s += p.t() }
	return s;
}
function captured(n) {
	var p = new P(), v = 0, f = function () { return v * 2 }, s = 0;
	for (var i = 0; i < n; i++) { p.step(i); v = p.read(); s += f() }
	return s;
}
function invalid(n) {
	var p = new P(), s = 0;
	for (var i = 0; i < n; i++) { p.step(1); s += p.k() + K }
	return s;
}
function forin(n) {
	var o = { a: 1, b: 2, c: 3 }, s = 0, k;
	for (var i = 0; i < n; i++) for (k in o) s += o[k];
	return s;
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
`

// TestJITRegWholeGoCalls runs regWholeSrc's functions in a runtime whose
// register code calls Go for the instructions it runs whole (jitRegWhole)
// and in one with no JIT, the collector idle and marking, calls of Go
// clobbering what they may: the answers equal, and the instructions left
// to Go at exits only where they did not go on natively.
func TestJITRegWholeGoCalls(t *testing.T) {
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
				rts := regNativeRuntimes(t, regWholeSrc)
				rts[0].SetJITWarmUp(1, 1)
				for _, name := range []string{"privates", "caught", "uncaught", "captured", "invalid", "forin", "spreads"} {
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						for range 6 {
							v, err := r.Call(fn, Undefined, []Value{Float(500)})
							got[i] = regNativeShow(r, v, err)
						}
					}
					if got[0] != got[1] {
						t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
					}
				}
				st := rts[0].RegNativeStats()
				t.Logf("calls of Go %d, hosts %v, finished at exits %d", st.GoCalls, st.HostOps, st.WholeLeft)
				if st.WholeLeft == 0 {
					t.Errorf("no instruction Go ran whole was finished at an exit")
				}
				for _, op := range []string{"get_private", "set_private", "for_in_start", "copy_data_props", "array_spread", "call_spread"} {
					// Each left only where it did not go on natively: a throw,
					// the code found invalid -- some tens over the runs.
					if n := st.HostOps[op]; n > 100 {
						t.Errorf("%s left to Go %d times", op, n)
					}
				}
			})
		}
	}
}

// regAddPushSrc updates properties in place, add_prop, of numbers that
// turn strings, and through a getter and setter, which count their calls;
// and pushes and pops arrays, one frozen; and a push now and then of an
// object's own push method.
const regAddPushSrc = `
var gets = 0, sets = 0;
function update(n) {
	var o = { a: 1, b: 2 }, s = "";
	for (var j = 0; j < n; j++) {
		o.a += j;
		o.b += 1;
		if (j == n - 7) o.a = "x";
	}
	return o.a + ":" + o.b;
}
function accessors(n) {
	var o = { _v: 0, get v() { gets++; return this._v }, set v(x) { sets++; this._v = x } };
	for (var j = 0; j < n; j++) o.v += j;
	return o.v + ":" + gets + ":" + sets;
}
function pushes(n) {
	var a = [], s = 0, f = Object.freeze([1]);
	for (var j = 0; j < n; j++) {
		a.push(j);
		if (j % 3 == 0) s += a.pop();
		a.push(1);
		try { if (j % 97 == 0) f.push(j) } catch (e) { s += 1 }
	}
	return a.length + ":" + s;
}
function poly(n) {
	var a = [], q = { n: 0, push(x) { this.n += x; return 0 } };
	for (var j = 0; j < n; j++) (j % 50 == 49 ? q : a).push(1);
	return a.length + ":" + q.n;
}
`

// TestJITRegAddPropPush runs regAddPushSrc in a runtime whose register code
// speculates on add_prop's addition after a read built to call nothing,
// and pushes and pops natively, and in one with no JIT: the answers equal,
// the getter and the setter called as often, and add_prop and the pushes
// left to Go at exits only where their speculation failed.
func TestJITRegAddPropPush(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regAddPushSrc)
			rts[0].SetJITWarmUp(1, 1)
			for _, name := range []string{"update", "accessors", "pushes", "poly"} {
				if name == "poly" {
					st := rts[0].RegNativeStats()
					t.Logf("hosts %v, guards %d", st.HostOps, st.Guards)
					if n := st.HostOps["add_prop"]; n > 100 {
						t.Errorf("add_prop left to Go %d times", n)
					}
					// The frozen array's push, every 97 turns, and each
					// array's growth.
					if n := st.HostOps["call_method_w"]; n > 200 {
						t.Errorf("call_method_w left to Go %d times", n)
					}
				}
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 6 {
						v, err := r.Call(fn, Undefined, []Value{Float(1000)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
			}
		})
	}
}

// regFallbackSrc runs what the builder leaves to Go whole, called from
// native code: a literal's field past a spread, a destructuring's steps
// and rest, a BigInt stored in a BigInt64Array's and a BigUint64Array's
// elements, and closures made in a loop of a function whose captured
// registers are its frame's own, each turn's binding closed at its end.
const regFallbackSrc = `
function fields(n) {
	var s = 0, base = { a: 1 };
	for (var j = 0; j < n; j++) { var o = { ...base, b: j, c: 2 }; s += o.b + o.c }
	return s;
}
function destruct(n) {
	var s = 0, a = [1, 2, 3, 4];
	for (var j = 0; j < n; j++) { var [x, y, ...r] = a; s += x + y + r.length }
	return s;
}
function bigs(n) {
	var t = new BigInt64Array(4), u = new BigUint64Array(4), v = 0n;
	for (var j = 0; j < n; j++) { v += 3n; t[j & 3] = v; u[j & 3] = -v; t[1] = 1n << 70n }
	return t.join() + "/" + u.join();
}
function closures(n) {
	var s = 0, fs = [];
	for (var j = 0; j < n; j++) { let k = j; var f = function () { return k }; s = (s + f()) | 0; if (j % 100 == 0) fs.push(f) }
	return s + ":" + fs.map(function (g) { return g() }).join();
}
`

// TestJITRegWholeFallbacks runs regFallbackSrc in a runtime whose register
// code calls Go for what its builder leaves to Go whole, and in one with no
// JIT, the collector idle and marking: the answers equal, and none of
// those instructions left to Go at exits but a few times.
func TestJITRegWholeFallbacks(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			jitClobberCallsForTest(t)
			rts := regNativeRuntimes(t, regFallbackSrc)
			rts[0].SetJITWarmUp(1, 1)
			for _, name := range []string{"fields", "destruct", "bigs", "closures"} {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 6 {
						v, err := r.Call(fn, Undefined, []Value{Float(1000)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
			}
			st := rts[0].RegNativeStats()
			t.Logf("hosts %v", st.HostOps)
			// A scope's end where the code's captured registers are in cells
			// of their own still leaves (jitRegWhole), as it does under
			// QJS_JIT_STRESS: not counted.
			for _, op := range []string{"define_field", "iter_step", "iter_rest", "set_index", "closure_w"} {
				if n := st.HostOps[op]; n > 50 {
					t.Errorf("%s left to Go %d times", op, n)
				}
			}
		})
	}
}
