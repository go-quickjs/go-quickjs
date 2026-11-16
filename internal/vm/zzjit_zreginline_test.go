//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regInlineSrc's callers call small register functions that register code
// inlines (jitRegFeedback's Inline): methods, plain calls of strict and
// sloppy functions reading this, a closure's captured binding, missing
// and extra arguments; and callees that leave the inlined code -- a shape
// the callee was not compiled for, a receiver that is no object, a throw
// the caller catches, a call it makes that Go runs -- whose frames Go makes
// from the exit's, finishing them in the interpreter; and callees inlined
// in inlined callees, left two levels deep, throwing through both; and
// constructions inlined, their receivers from pools, left where a callee
// meets a new shape or throws; and getters and setters inlined at the
// reads and writes that call them, a class's and a prototype's, left
// where a getter meets a new shape or throws.
const regInlineSrc = `
function P(x) { this.x = x }
P.prototype.get = function () { return this.x };
P.prototype.add = function (k) { return this.x + k };
function sq(a) { return a * a }
function pair(a, b) { return b === undefined ? a : a - b }
function sloppy() { return this === globalThis ? 1 : 0 }
function strict() { "use strict"; return this === undefined ? 2 : 0 }
var withK = (function () { var k = 5; return function (a) { return a + k } })();
function inlines(n) {
	var p = new P(3), s = 0;
	for (var i = 0; i < n; i++) s += p.get() + p.add(i) + sq(i) + pair(i) + pair(i, 1, 2) + sloppy() + strict() + withK(i);
	return s;
}
function rd(o) { return o.x }
function shapes(n) {
	var a = { x: 1 }, b = { y: 0, x: 2.5 }, s = 0;
	for (var i = 0; i < n; i++) s += rd(i % 97 == 96 ? b : a);
	return s;
}
function throws(n) {
	var a = { x: 1 }, s = 0, m = "";
	for (var i = 0; i < n; i++) { try { s += rd(i % 50 == 49 ? undefined : a) } catch (e) { m = e.constructor.name } }
	return s + m;
}
function str(o) { return o.x + "!" }
function callsGo(n) { var s = 0; for (var i = 0; i < n; i++) s += str(i % 3 ? { x: i } : { x: "k" + i }).length; return s }
function mid(o) { return rd(o) + 1 }
function nested(n) { var a = { x: 3 }, b = { z: 1, x: 4 }, s = 0; for (var i = 0; i < n; i++) s += mid(i % 61 == 60 ? b : a); return s }
function V(x, y) { this.x = x; this.y = y }
function Vr(x) { this.x = x; return this }
function Vs(o) { this.v = o.x }
function constructs(n) {
	var s = 0, a = { x: 1 }, b = { y: 0, x: 2 }, m = "";
	for (var i = 0; i < n; i++) {
		var v = new V(i, 2), w = new Vr(i);
		s += v.x + v.y + w.x + new Vs(i % 61 == 60 ? b : a).v;
		try { s += new Vs(i % 50 == 49 ? null : a).v } catch (e) { m = e.constructor.name }
	}
	return s + m + (new V(1, 2) instanceof V) + Object.keys(new Vr(3)) + Object.getPrototypeOf(new Vs(a)).constructor.name;
}
class Pt { constructor(x) { this._x = x; this.o = { v: 1 } } get x() { return this._x } set x(v) { this._x = v } get nn() { return this.o.v } }
var proto = { get g() { return this.k * 2 } };
function accessors(n) {
	var p = new Pt(1), q = Object.create(proto), s = 0, m = "";
	q.k = 3;
	for (var i = 0; i < n; i++) {
		p.x = i;
		s += p.x + q.g;
		p.o = i % 50 == 49 ? null : i % 61 == 60 ? { w: 0, v: 2 } : { v: 1 };
		try { s += p.nn } catch (e) { m = e.constructor.name }
	}
	return s + m;
}
function guarded(o) { try { return mid(o) } catch (e) { return -1 } }
function throws2(n) {
	var a = { x: 1 }, s = 0, m = "";
	for (var i = 0; i < n; i++) {
		try { s += mid(i % 50 == 49 ? undefined : a) } catch (e) { m = e.constructor.name }
		s += guarded(i % 70 == 69 ? null : a);
	}
	return s + m;
}
`

// TestJITRegInline runs regInlineSrc's callers in a runtime whose register
// code inlines its callees and in one with no JIT, and compares their
// answers, the collector idle and marking; the callers inline what they
// call, and their code compiles for arm64 too.
func TestJITRegInline(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			RecordJITCompiles(true)
			defer RecordJITCompiles(false)
			rts := regNativeRuntimes(t, regInlineSrc)
			rts[0].SetJITWarmUp(1, 1)
			// Each case's callers' exits inside the callees they inline
			// are left, at most, unwound -- learned from, the code compiled
			// again, or the call made natively after jitInlineExits: not
			// at every call that misses (1,600 times in shapes, 10,000 in
			// nested, the code's runs going on in code compiled stale), but
			// at every throw, which leaves native code: each inlined level
			// it leaves counted: throws2's 8 runs throw 160 times through
			// mid and rd, two levels, and 112 through guarded, mid and rd,
			// three, some 650 levels -- under QJS_JIT_STRESS too.
			for _, c := range []struct {
				name    string
				n       float64
				unwound uint64
			}{{"inlines", 2000, 0}, {"shapes", 2000, 40}, {"throws", 1000, 220}, {"callsGo", 600, 60}, {"nested", 2000, 60}, {"throws2", 1000, 700}, {"constructs", 1000, 220}, {"accessors", 1000, 220}} {
				var got [2]string
				before := rts[0].RegNativeStats().Unwound
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
					for range 8 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						got[i] = regNativeShow(r, v, err)
					}
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", c.name, got[0], got[1])
				}
				if n := rts[0].RegNativeStats().Unwound - before; n > c.unwound {
					t.Errorf("%s: unwound %d, past %d: inlined callees left too often", c.name, n, c.unwound)
				}
			}
			r := rts[0]
			st := r.RegNativeStats()
			t.Logf("inlined %d; unwound %d, guards %d, hosts %d, entries %d", r.jit.inlined, st.Unwound, st.Guards, st.Hosts, st.Entries)
			if r.jit.inlined == 0 {
				t.Error("no callee inlined")
			}
			// constructs' code inlines its constructions -- V's, Vr's, Vs's
			// -- and accessors' its getters and setters, as the stack
			// form's does.
			for _, c := range []struct {
				name string
				n    int
			}{{"constructs", 3}, {"accessors", 3}} {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
				if cl := fn.Object().fn().closure; cl != nil {
					if e := r.jit.regCache[r.jitKeyOf(cl.fn)]; e == nil || e.p == nil {
						t.Errorf("%s: not compiled", c.name)
					} else if f, err := ssa.BuildWith(e.p, r.jitRegFeedbackFor(cl, e.p)); err != nil || len(f.Inlined) < c.n {
						t.Errorf("%s: %v, %d callees inlined", c.name, err, len(f.Inlined))
					}
				}
			}
			// The same code, arm64's backend's, compiled on any host.
			if fns, _, inlined := regCompileARM64(t, r); inlined == 0 {
				t.Errorf("arm64: %d functions compiled, none inlining a callee", fns)
			}
		})
	}
}

// regAccessorSrc reads and writes accessors register code does not inline:
// a class's getter and setter (inlining off), a prototype's getter, a
// bound function's getter, a built-in's (Map's size); one throws now and
// then.
const regAccessorSrc = `
class Pt { constructor(x) { this._x = x } get x() { return this._x } set x(v) { if (v === -1) throw new RangeError("neg"); this._x = v } }
var proto = { get g() { return this.k * 2 } };
var holder = { k: 5 };
var bound = Object.create(Object.defineProperty({}, "b", { get: function () { return this.k + 1 }.bind(holder) }));
function accessorGoCalls(n) {
	var p = new Pt(1), q = Object.create(proto), m = new Map([[1, 2]]), s = 0, e = "";
	q.k = 3;
	for (var i = 0; i < n; i++) {
		p.x = i;
		s += p.x + q.g + bound.b + m.size;
		try { p.x = i % 50 == 49 ? -1 : i } catch (err) { e = err.message }
	}
	return s + e;
}
`

// TestJITRegAccessorGoCalls runs regAccessorSrc's accessorGoCalls in a
// runtime whose register code calls Go for the accessors it does not
// inline (jitRegGoCallOn's GoCallGet, GoCallSet), and in one with no JIT,
// the collector idle and marking: the answers equal, and its reads and
// writes left to Go at exits only where a setter threw.
func TestJITRegAccessorGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			regNoInline(t)
			rts := regNativeRuntimes(t, regAccessorSrc)
			rts[0].SetJITWarmUp(1, 1)
			var got [2]string
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("accessorGoCalls"))
				for range 8 {
					v, err := r.Call(fn, Undefined, []Value{Float(1000)})
					got[i] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Errorf("with the JIT %q, without %q", got[0], got[1])
			}
			st := rts[0].RegNativeStats()
			t.Logf("calls of Go %d, hosts %v", st.GoCalls, st.HostOps)
			// 8 runs of 1,000 turns, a setter's throw every 50: 160 host
			// exits at set_prop at most; the reads, none but a first's.
			if n := st.HostOps["get_prop"]; n > 8 {
				t.Errorf("get_prop left to Go %d times", n)
			}
			if n := st.HostOps["set_prop"]; n > 200 {
				t.Errorf("set_prop left to Go %d times", n)
			}
		})
	}
}
