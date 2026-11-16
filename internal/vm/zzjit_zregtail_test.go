//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regTailSrc makes calls in tail position from register code's native
// code: tasks' functions, called by a method in tail position, as AWFY's
// Richards calls its tasks', the method compiled before it met the
// second; and recursions through calls in tail position past
// tailCallDepth, of one function and of two by turns, whose frames are
// given up.
const regTailSrc = `
"use strict";
function next(x) { return x + 1 }
class Task {
	constructor(fn) { this.fn = fn; this.h = 1 }
	run(i) { return this.fn(i, this.h) }
}
function f1(w, d) { return next(w + d) }
function f2(w, d) { return next(w - d) }
var t1 = new Task(f1), t2 = new Task(f2), runs = 0;
function tasks(n) {
	// f2 first called at the second run, once run is compiled for f1.
	var b = runs++ ? t2 : t1;
	var acc = 0;
	for (var i = 0; i < n; i++) acc += (i & 1 ? t1 : b).run(i);
	return acc;
}
function mk(k) { var o = { n: 0 }; return function (w, d) { o.n += k; return next(w + d + o.n) } }
function fresh(n) {
	// Two functions anew at each run, which the code learns up to
	// jitCallTargets of, and calls the rest of through their links.
	var a = new Task(mk(1)), b = new Task(mk(2)), acc = 0;
	for (var i = 0; i < n; i++) acc += (i & 1 ? a : b).run(i);
	return acc;
}
// Seven functions, past the four a call calls natively: the rest called
// through their links (ssa.CallSite's Generic), an arrow reading its this
// among them, and one called with more arguments than it has parameters.
function g0(x) { return x + 1 }
function g1(x, y) { return x * 2 + (y === undefined) }
function g2(x) { return x - (this === undefined) }
function g3(x) { return -x }
function g4(x) { return x * x }
var g5 = { v: 7, mk() { return (x) => x + this.v } }.mk();
function g6(x, y, z) { return x + (z === undefined ? 100 : z) }
var shapes = [g0, g1, g2, g3, g4, g5, g6];
function poly(n) { var s = 0; for (var i = 0; i < n; i++) s += shapes[i % 7](i); return s }
function even(n, acc) { if (n === 0) return acc; return odd(n - 1, acc + 1) }
function odd(n, acc) { if (n === 0) return -acc; return even(n - 1, acc + 2) }
function self(n, acc) { if (n === 0) return acc; return self(n - 1, acc + n) }
function deep(n) { var s = 0; for (var i = 0; i < n; i++) s += even(40 + (i & 7), 0) + self(40 + (i & 7), 0); return s }
`

// TestJITRegTailCalls runs regTailSrc's callers in a runtime whose register
// code runs natively and in one with no JIT, and compares their answers. A
// call in tail position that met a function its code did not call natively
// leaves native code, and Go makes it as the interpreter would -- the
// ordinary call while the stack is shallow, whose callee the code learns --
// so that tasks leaves native code at none of its calls once its code
// learned their functions -- where the interpreter had made the rest of
// the invocation, which learned nothing, and the call left native code at
// every call after; and a recursion through calls in tail position, past
// tailCallDepth, runs each frame given to a callee in its native code
// (regRestartRun).
func TestJITRegTailCalls(t *testing.T) {
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
			rts := regNativeRuntimes(t, regTailSrc)
			rts[0].SetJITWarmUp(1, 1)
			run := func(name string, n float64) {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(n)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
			}
			for range 4 {
				run("tasks", 2000)
			}
			r := rts[0]
			before := r.RegNativeStats().HostOps
			for range 4 {
				run("tasks", 2000)
			}
			after := r.RegNativeStats().HostOps
			for _, op := range []string{"tail_call_w", "tail_call_method_w"} {
				if n := after[op] - before[op]; n != 0 {
					t.Errorf("%d exits at %s once the code learned its callees", n, op)
				}
			}
			for _, name := range []string{"fresh", "poly"} {
				for range 4 {
					run(name, 2000)
				}
				before := r.RegNativeStats()
				for range 4 {
					run(name, 2000)
				}
				// Past the functions it calls natively, the others called
				// through their links: none of Go's, nor an exit.
				after := r.RegNativeStats()
				if n := after.Hosts - before.Hosts; n != 0 {
					t.Errorf("%s: %d exits once its calls were learned: %v", name, n, after.HostOps)
				}
				if n := after.GoCalls - before.GoCalls; n != 0 {
					t.Errorf("%s: %d calls of Go once its calls were learned", name, n)
				}
			}
			for range 2 {
				run("deep", 2000)
			}
			before = r.RegNativeStats().HostOps
			run("deep", 2000)
			// The frames given to a callee in place past tailCallDepth --
			// self's own, even's and odd's each other's -- natively: a jump
			// to the callee's code in the frame (mir's tailSelf and
			// windowTailJump), none of them left to the interpreter.
			after = r.RegNativeStats().HostOps
			if n := after["tail_call_w"] - before["tail_call_w"]; n != 0 {
				t.Errorf("%d exits at tail_call_w in deep: the frames given to callees ran in the interpreter", n)
			}
			// arm64's backend compiles the same code, its generic calls of
			// register code among it, on any host.
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
		})
	}
}

// regClassConstructSrc constructs classes' objects, of base classes with
// no field to initialize, from register code's native code: natively, as
// the stack form's code constructs them (jitConstructs), where Go had
// constructed each; and a construction that throws, and a class's
// constructor called, which throws.
const regClassConstructSrc = `
class Vec { constructor(x, y) { this.x = x; this.y = y } add(o) { return new Vec(this.x + o.x, this.y + o.y) } }
class Cell { constructor(v) { if (v < 0) throw new RangeError("neg " + v); this.v = v; this.next = null } }
function classes(n) {
	var a = new Vec(0, 0), s = 0, head = null;
	for (var i = 0; i < n; i++) {
		a = a.add(new Vec(i, 1));
		var c = new Cell(i);
		c.next = head;
		head = (i & 15) ? c : null;
		s += c.v;
	}
	return a.x + a.y + s;
}
function checks(n) {
	var m = "", head = null;
	for (var i = 0; i < n; i++) {
		try { head = new Cell(i % 100 - 1) } catch (e) { m = e.message }
		try { Vec(1, 2) } catch (e) { m += e instanceof TypeError }
	}
	return m + (head instanceof Cell) + Object.keys(new Vec(1, 2));
}
`

// TestJITRegClassConstructions runs regClassConstructSrc's functions
// natively and in a runtime with no JIT and compares them: once compiled,
// classes' constructions are native, none Go's.
func TestJITRegClassConstructions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regClassConstructSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			r.jitState().reporting = true
			news := func() (n uint64) {
				for k, v := range r.RegNativeStats().GoCallOps {
					if strings.HasPrefix(k, "new_w") {
						n += v
					}
				}
				return n
			}
			var made uint64
			for k := range 8 {
				if k == 4 {
					made = news()
				}
				for _, name := range []string{"classes", "checks"} {
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						v, err := r.Call(fn, Undefined, []Value{Float(3000)})
						got[i] = regNativeShow(r, v, err)
					}
					if got[0] != got[1] {
						t.Fatalf("%s: with the JIT %q, without %q", name, got[0], got[1])
					}
				}
			}
			// None of Vec's and Cell's, but the RangeErrors' at most.
			if n := news() - made; n > 4*30 {
				t.Errorf("%d constructions Go made in 4 runs", n)
			}
		})
	}
}

// regInlineExitSrc's callees throw on a path seldom taken, as AWFY's
// Towers' pushDisk does: inlined all the same, the throw an exit of the
// inlined frame (jitRegFeedback's InlineExit), which leaves it to Go once
// taken.
const regInlineExitSrc = `
function check(d) { if (d < 0) throw new RangeError("bad " + d); return d * 2 }
function guard(o) { if (o === null) throw o; return o.v + 1 }
function towers(n) {
	var s = 0, o = { v: 3 }, m = "";
	for (var i = 0; i < n; i++) {
		try { s += check(i % 1000 == 999 ? -i : i) + guard(o) } catch (e) { m = e.message }
	}
	return s + m;
}
`

// TestJITRegInlineExits runs regInlineExitSrc's towers natively and in a
// runtime with no JIT and compares them: its callees are inlined, their
// throws taken now and then.
func TestJITRegInlineExits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regInlineExitSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			for range 6 {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("towers"))
					v, err := r.Call(fn, Undefined, []Value{Float(5000)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("towers: with the JIT %q, without %q", got[0], got[1])
				}
			}
			if st := r.JITStats(); st.Inlined == 0 {
				t.Errorf("no callee inlined: %+v", st)
			}
		})
	}
}

// regArgsSrc's callees keep their arguments -- read arguments, take the
// rest -- and are called natively, given them in their contexts
// (ssa.CallSite's FrameArgs): their reads of them answered from there,
// and a frame Go makes of their level given them (jitRegPushLevel).
const regArgsSrc = `
function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s }
function rest(a, ...more) { return a + more.length + (more[1] | 0) }
function list() { "use strict"; var o = null; for (var i = arguments.length - 1; i >= 0; i--) o = { car: arguments[i], cdr: o }; return o }
function len(o) { var n = 0; while (o) { n += o.car; o = o.cdr } return n }
function stack() { return new Error("x").stack.split("\n").length + arguments.length }
function args(n) {
	var s = 0;
	for (var i = 0; i < n; i++) s += sum(i, 1, 2) + sum() + rest(i, 2, 3, 4) + len(list(i, 2, 3)) + (i % 500 == 0 ? stack(1, 2) : 0);
	return s;
}
`

// TestJITRegArgumentsCallees runs regArgsSrc's args natively and in a
// runtime with no JIT and compares them: its callees that keep their
// arguments are called natively.
func TestJITRegArgumentsCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			regNoInline(t)
			RecordJITCompiles(true)
			defer RecordJITCompiles(false)
			rts := regNativeRuntimes(t, regArgsSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			for range 6 {
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("args"))
					v, err := r.Call(fn, Undefined, []Value{Float(3000)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("args: with the JIT %q, without %q", got[0], got[1])
				}
			}
			native := map[string]uint64{}
			for key, l := range r.jit.regLinks {
				if fn := key.Value(); fn != nil {
					native[fn.Name] += l.in
				}
			}
			for _, name := range []string{"sum", "rest", "list"} {
				if native[name] == 0 {
					t.Errorf("%s: never called natively: %v", name, native)
				}
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
		})
	}
}

// regForwardSrc's constructors are prototype.js's Class.create's,
// this.initialize.apply(this, arguments), as Octane's RayTrace makes its
// objects: constructed natively, their forwarding call native too
// (ssa.CallSite's Forward); and a forwarding call through another apply,
// the interpreter's.
const regForwardSrc = `
var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
var Vec = Class.create();
Vec.prototype = {
	initialize: function (x, y, z) { this.x = x; this.y = y; this.z = z === undefined ? -1 : z },
	add: function (o) { return new Vec(this.x + o.x, this.y + o.y, this.z + o.z) },
};
var Odd = Class.create();
Odd.prototype = { initialize: function () { this.n = arguments.length + (arguments[3] | 0) } };
function fwd(n) {
	var a = new Vec(0, 0, 0), k = 0;
	for (var i = 0; i < n; i++) { a = a.add(new Vec(i, 1, 2)); k += new Odd(1, 2, 3, i).n + new Vec(i, 2).z }
	var f = function () { return [].slice.call(arguments).join() + this.q };
	f.apply = function (t, args) { return "own " + args.length };
	var g = function () { return f.apply({ q: 1 }, arguments) };
	return a.x + a.y + a.z + k + g(1, 2);
}
`

// TestJITRegForwardingConstructors runs regForwardSrc's fwd natively and in
// a runtime with no JIT and compares them: once compiled, no
// apply_arguments leaves native code.
func TestJITRegForwardingConstructors(t *testing.T) {
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
			rts := regNativeRuntimes(t, regForwardSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			var before uint64
			for k := range 8 {
				if k == 4 {
					before = r.RegNativeStats().HostOps["apply_args"]
				}
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("fwd"))
					v, err := r.Call(fn, Undefined, []Value{Float(3000)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("fwd: with the JIT %q, without %q", got[0], got[1])
				}
			}
			// g's own apply, once a run, at most.
			if n := r.RegNativeStats().HostOps["apply_args"] - before; n > 4 {
				t.Errorf("%d apply_arguments left native code in 4 runs", n)
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
		})
	}
}

// regTailSelfSrc's functions call themselves in tail position past
// tailCallDepth: strict code, whose frame the interpreter gives the call in
// place (regTailTo), which native code makes a jump to its start
// (ssa.OpTailSelf) -- a plain function's, with fewer and more arguments
// than it has parameters, a method's, given its receiver, an arrow's,
// which keeps the this it captured, a construction's frame, which no call
// gives up, and one that calls another now and then, its frame given back
// to it by the interpreter, Go entering its code again -- beside two
// functions calling each other, a self call in a try, which is no call in
// tail position, and a sloppy function's, which makes none. Each driver
// runs its recursion k times, natively once warm.
const regTailSelfSrc = `
function self(n, acc) { "use strict"; if (n === 0) return acc; return self(n - 1, acc + 1) }
function deepSelf(n, k) { var s; for (var i = 0; i < k; i++) s = self(n, 0); return s }
function mutA(n, acc) { "use strict"; if (n === 0) return acc; return mutB(n - 1, acc + 2) }
function mutB(n, acc) { "use strict"; if (n === 0) return -acc; return mutA(n - 1, acc + 1) }
function deepMut(n, k) { var s; for (var i = 0; i < k; i++) s = mutA(n, 0) + "," + mutA(n + 1, 0); return s }
function counts(n, a, b, c) {
	"use strict";
	if (n === 0) return a + ":" + b + ":" + c;
	if (n % 3 === 0) return counts(n - 1, a + 1);
	if (n % 3 === 1) return counts(n - 1, a, (b | 0) + 1, (c | 0) + 2, 99, 100);
	return counts(n - 1, a, b);
}
function deepCounts(n, k) { var s; for (var i = 0; i < k; i++) s = counts(n, 0, 0, 0); return s }
function tried(n, acc) { "use strict"; try { if (n === 0) return acc; return tried(n - 1, acc + 1) } catch (e) { return "caught " + (e instanceof RangeError) } }
function deepTried(n, k) { var s; for (var i = 0; i < k; i++) s = tried(n, 0); return s }
var obj = { k: 3, m(n, acc) { "use strict"; if (n === 0) return acc + this.k; return this.m(n - 1, acc + this.k) } };
var other = { k: 5, m: obj.m };
function deepMethod(n, k) { var s; for (var i = 0; i < k; i++) s = obj.m(n, 0) + other.m(n, 1); return s }
var maker = { k: 2, f(n) { "use strict"; const g = (n, acc) => n === 0 ? acc + this.k : g(n - 1, acc + this.k); return g(n, 0) } };
function deepArrow(n, k) { var s; for (var i = 0; i < k; i++) s = maker.f(n); return s }
function P(n, acc) { "use strict"; if (n <= 0) return { acc: acc, made: this !== undefined }; if (this) this.seen = n; return P(n - 1, acc + 1) }
function nest(d, n) { if (d > 0) { var v = nest(d - 1, n); return v } var o = new P(n, 0); return JSON.stringify(o) + P.length + new P(0, 1).made }
function deepNew(n, k) { var s; for (var i = 0; i < k; i++) s = nest(40, n); return s }
function mix(n, acc) { "use strict"; if (n === 0) return acc; if (n % 100 === 0) return mix2(n - 1, acc); return mix(n - 1, acc + 1) }
function mix2(n, acc) { "use strict"; return mix(n, acc + 2) }
function deepMix(n, k) { var s; for (var i = 0; i < k; i++) s = mix(n, 0); return s }
function sloppy(n, acc) { if (n === 0) return acc + ':' + (this === undefined); return sloppy(n - 1, acc + 1) }
function deepSloppy(n, k) { var s; for (var i = 0; i < k; i++) s = sloppy(n, 0); return s }
`

// TestJITRegTailSelf runs regTailSelfSrc's functions natively and in a
// runtime with no JIT and compares them: a recursion through calls of the
// function itself in tail position runs as a loop in its frame, which
// leaves native code at none of them, and a million calls deep uses the
// stack no deeper than a thousand did.
func TestJITRegTailSelf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := map[string]string{
		"deepSelf":   "1000000",
		"deepMut":    "1500000,-1500002",
		"deepCounts": "333333:1:2",
		"deepTried":  "caught true",
		"deepMethod": "8000009",
		"deepArrow":  "2000002",
		"deepNew":    `{"acc":1000000,"made":false}2true`,
		"deepMix":    "1010000",
		"deepSloppy": "1000:false",
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			RecordJITCompiles(true)
			defer RecordJITCompiles(false)
			rts := regNativeRuntimes(t, regTailSelfSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			run := func(name string, n, k float64) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(n), Float(k)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s(%v): with the JIT %q, without %q", name, n, got[0], got[1])
				}
				return got[0]
			}
			for _, name := range []string{"deepSelf", "deepMut", "deepCounts", "deepMethod", "deepArrow", "deepNew", "deepMix", "deepSloppy"} {
				// Until its code has learned what the guards met.
				for range 8 {
					run(name, 40, 50)
				}
				n := 1e6
				if name == "deepSloppy" {
					// No call in tail position in sloppy code: the stack
					// grows.
					n = 1000
				}
				st := r.RegNativeStats()
				stack, frames := r.stackHigh, r.frameHigh
				got := run(name, n, 1)
				if got != want[name] {
					t.Errorf("%s(%v) = %q, want %q", name, n, got, want[name])
				}
				st2 := r.RegNativeStats()
				// The frames Go makes of the native levels while it polls,
				// tailCallDepth of them at most.
				if name != "deepSloppy" && (r.stackHigh > stack+64 || r.frameHigh > max(frames, tailCallDepth+8)) {
					t.Errorf("%s: the stack grew from %d to %d, the frames from %d to %d", name, stack, r.stackHigh, frames, r.frameHigh)
				}
				exits := st2.HostOps["tail_call_w"] + st2.HostOps["tail_call_method_w"] - st.HostOps["tail_call_w"] - st.HostOps["tail_call_method_w"]
				switch name {
				case "deepSelf", "deepCounts", "deepMethod", "deepArrow", "deepNew":
					// A loop in the frame, entered once: but for the
					// construction's own call, whose frame no call gives up
					// (abi.Context's TailFrame), the ordinary call Go makes.
					if name == "deepNew" && exits == 1 {
						exits--
					}
					if exits != 0 || st2.Entries-st.Entries > 100 {
						t.Errorf("%s: %d calls in tail position left native code, %d entries", name, exits, st2.Entries-st.Entries)
					}
				case "deepMix":
					// mix2's calls of mix, and mix's of mix2, given the frame
					// in place by Go: none of mix's own.
					if exits > 2*1e6/100+100 {
						t.Errorf("%s: %d calls in tail position left native code", name, exits)
					}
				}
				if st2.Entries == st.Entries {
					t.Errorf("%s: no native code ran", name)
				}
			}
			// A self call in a try is an ordinary call: the stack runs out.
			if got := run("deepTried", 1e6, 1); got != want["deepTried"] {
				t.Errorf("deepTried = %q, want %q", got, want["deepTried"])
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
		})
	}
}

// regDerivedSrc constructs derived classes from register code: super() of
// arguments, through two levels and past a class without a constructor of
// its own, fields in a base and a derived class, a derived constructor
// returning an object and returning undefined, new.target, an arrow made
// after super() and one before it, a parent whose code leaves native code
// -- a write its property sites never met, a throw -- and the
// ReferenceErrors and the TypeError a derived construction throws: this
// read before super(), a second super(), a number returned. megas' parent
// writes its fields at sites the subclasses' layouts make megamorphic,
// which Go writes from a native callee's code with no frame
// (jitGoCallPlain): read at a register pc as the stack form's, the write
// had gone to another property -- Bin's direction to v1.
const regDerivedSrc = `
"use strict";
class Base {
	constructor(x, y) { this.x = x; this.y = y }
	sum() { return this.x + this.y }
}
class Mid extends Base { constructor(x, y, z) { super(x, y); this.z = z } }
class Leaf extends Mid {
	constructor(x) { super(x, x + 1, x + 2); this.w = new.target === Leaf ? 1 : 2 }
}
class Plain extends Base {}
class Deep extends Plain { constructor(a) { super(a, -a); this.d = 1 } }
class Twig extends Deep {}
class FieldBase { f = 7; constructor(v) { this.v = v } }
class FieldDerived extends FieldBase { g = 3; constructor(v) { super(v); this.h = this.f + this.g } }
class Other extends Base { constructor(x) { super(x, 0); return { other: x } } }
class Undef extends Base { constructor(x) { super(x, 1); return undefined } }
class Early extends Base { constructor(x) { if (x < 0) this.bad = 1; super(x, 2) } }
class Twice extends Base { constructor(x) { super(x, 3); if (x === 13) super(x, 4) } }
class Num extends Base { constructor(x) { super(x, 0); if (x === 5) return 1 } }
class NT extends Base { constructor(x) { super(x, 5); this.nt = new.target.name } }
class NTBase { constructor() { this.t = new.target.name } }
class NTKid extends NTBase {}
class Arrowed extends Base { constructor(x) { super(x, 6); this.get = () => this.x } }
class Shifty {
	constructor(v) {
		this.v = v;
		if (typeof v === "string") this.s = v.length;
		if (v === 999) throw new RangeError("v " + v);
	}
}
class ShiftyKid extends Shifty { constructor(v) { super(v); this.k = 1 } }
class ArrowFirst extends Base { constructor(x) { const f = () => this.x; super(x, 7); this.f = f() } }
class Abs { constructor(s) { this.strength = s } }
class Bin extends Abs { constructor(a, b, s) { super(s); this.v1 = a; this.v2 = b; this.direction = null } }
class K1 extends Bin { constructor(a, b) { super(a, b, 1); this.k1 = 1 } }
class K2 extends Bin { constructor(a, b) { super(a, b, 2); this.k2 = 1 } }
class K3 extends Bin { constructor(a, b) { super(a, b, 3); this.k3 = 1 } }
class K4 extends Bin { constructor(a, b) { super(a, b, 4); this.k4 = 1 } }
class K5 extends Bin { constructor(a, b, c, d) { super(a, b, 5); this.c = c; this.d = d } }
class K6 extends Bin { constructor(a, b) { super(a, b, 6); this.k6 = 1 } }
function shown(o) { return Object.keys(o).join() + ":" + (o.v1 === null) }
function megas(n) {
	var out = "";
	for (var i = 0; i < n; i++) {
		var a = { i }, b = { i };
		var all = [new K1(a, b), new K2(a, b), new K3(a, b), new K4(a, b), new K5(a, b, 1, 2), new K6(a, b)];
		if (i % 400 === 0) out += all.map(shown).join("|") + ";";
	}
	return out;
}
function natives(n) {
	var acc = 0;
	for (var i = 0; i < n; i++) {
		var l = new Leaf(i);
		acc += l.sum() + l.z + l.w;
		acc += new Plain(i, 1).sum();
		var d = new Deep(i);
		acc += d.sum() + d.d + new Twig(i).d;
		acc += new Undef(i).sum();
		acc += new ShiftyKid(i).k;
	}
	return acc;
}
function classes(n) {
	var acc = 0;
	for (var i = 0; i < n; i++) {
		var fd = new FieldDerived(i);
		acc += fd.h + fd.v + fd.g;
		acc += new Other(i).other;
		acc += new NT(i).nt.length + new NTKid().t.length;
		acc += new Arrowed(i).get() + new ArrowFirst(i).f;
		var s = new ShiftyKid(i % 50 === 49 ? "str" + i : i);
		acc += s.k + (s.s || 0);
	}
	return acc;
}
function checks(n) {
	var out = [];
	for (var i = 0; i < n; i++) {
		if (i % 500 === 0) {
			try { new Early(-1) } catch (e) { out.push(e.constructor.name) }
			try { new Twice(13) } catch (e) { out.push(e.constructor.name) }
			try { new Num(5) } catch (e) { out.push(e.constructor.name) }
			try { new ShiftyKid(999) } catch (e) { out.push(e.constructor.name + " " + e.message) }
		}
		new Early(i).sum();
		new Twice(i).sum();
		new Num(i + 6).sum();
	}
	return out.join();
}
`

// TestJITRegDerivedConstructions runs regDerivedSrc's functions in a
// runtime whose register code runs natively and in one with no JIT, and
// compares their answers: derived classes' constructions are native calls,
// their super()s native calls of the parent with the instance as its
// receiver, as the stack form's code makes them (ssa.CallSite's Derived and
// Super) -- where Go had made every construction of a derived class -- and
// what leaves native code inside them, a parent's exit, a throw, a second
// super(), Go finishes from the frames it makes of their levels. natives'
// constructions and super()s are native once learned, the collector idle
// and marking.
func TestJITRegDerivedConstructions(t *testing.T) {
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
			rts := regNativeRuntimes(t, regDerivedSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			r.jitState().reporting = true
			goMade := func() (n uint64) {
				st := r.RegNativeStats()
				for k, v := range st.GoCallOps {
					if strings.HasPrefix(k, "new_w") || strings.HasPrefix(k, "super_call") {
						n += v
					}
				}
				return n + st.HostOps["new_w"] + st.HostOps["super_call"] + st.HostOps["derived_ret"]
			}
			var made uint64
			for k := range 8 {
				for _, name := range []string{"natives", "classes", "checks", "megas"} {
					before := goMade()
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
						v, err := r.Call(fn, Undefined, []Value{Float(2000)})
						got[i] = regNativeShow(r, v, err)
					}
					if got[0] != got[1] {
						t.Fatalf("%s: with the JIT %q, without %q", name, got[0], got[1])
					}
					if k >= 4 && name == "natives" {
						made += goMade() - before
					}
				}
			}
			// Of 4 runs of 2000 iterations' 7 constructions each, a few
			// at most, as natives' code is compiled again.
			if made > 4*20 {
				t.Errorf("%d constructions and super()s Go made in 4 runs of natives", made)
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Error("nothing compiled for arm64")
			}
		})
	}
}
