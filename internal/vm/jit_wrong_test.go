//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// Wrong answers the register JIT gave where the vm package's JIT tests,
// which had run on the stack form's JIT alone, first ran on it. Each
// case is checked by regWrongCheck: natively from the start and by
// on-stack replacement, with the collector idle and marking, against its
// exact answer and an ordinary call's.

// regWrongCase is a call of the global function name with args, made
// warm times in each runtime first, so that the caches its code is
// compiled from have met what the call meets, and its exact answer.
type regWrongCase struct {
	name string
	args []float64
	warm int
	want string
}

// regWrongCheck runs each case of src (regWrongCase) natively from its
// register code -- from its start, and by on-stack replacement at a loop
// (RegNativeCall), three times -- and as an ordinary call in a runtime
// without the JIT, with the collector idle and with native code seeing it
// mark (jitMarkingForTest): each answer must be the case's.
func regWrongCheck(t *testing.T, src string, cases []regWrongCase) {
	t.Helper()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		for _, osr := range []bool{false, true} {
			t.Run(fmt.Sprintf("marking=%v/osr=%v", marking, osr), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				rts := regNativeRuntimes(t, src)
				// Compiled at the first call, the warming calls' code
				// native too, as jitRuntimeForTest's: what their native
				// runs learn -- the targets a forwarder's calls hand
				// their arguments to -- the code compiled next inlines.
				rts[0].jitCallThreshold, rts[0].jitICThreshold = 1, 1
				for _, c := range cases {
					args := make([]Value, len(c.args))
					for i, a := range c.args {
						args[i] = Float(a)
					}
					for k, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
						for range c.warm {
							if _, err := r.Call(fn, Undefined, args); err != nil {
								t.Fatalf("%s warming: %v", c.name, err)
							}
						}
						// Three calls: what one leaves -- its pools run
						// dry, cells left open -- the next meets.
						for call := range 3 {
							var got string
							if k == 1 {
								v, err := r.Call(fn, Undefined, args)
								got = regNativeShow(r, v, err)
							} else {
								res := r.RegNativeCall(fn, args, osr)
								if res.Skipped != "" {
									t.Fatalf("%s: not native: %s", c.name, res.Skipped)
								}
								if res.Entered < 0 || osr && !res.OSR {
									t.Fatalf("%s: native code not entered (at %d, OSR %v)", c.name, res.Entered, res.OSR)
								}
								got = regNativeShow(r, res.Value, res.Err)
							}
							if got != c.want {
								t.Errorf("%s%v, call %d, JIT %v: got %q, want %q", c.name, c.args, call, k == 0, got, c.want)
							}
						}
					}
				}
			})
		}
	}
}

// A getter or a setter that reads its arguments is not inlined where a
// register read or write calls it: inlined, its arguments.length and
// arguments[k] were read at an exit whose frame had no arguments, and a
// setter's arguments.length was 0 (TestJITAccessors' args(30), 1035 for
// 3735).
func TestJITRegAccessorArguments(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `function A(){this.k=0}
	Object.defineProperty(A.prototype,"n",{get(){return arguments.length+(arguments[0]===undefined?10:0)}});
	Object.defineProperty(A.prototype,"m",{set(v){this.k=arguments.length*100+v}});
	Object.defineProperty(A.prototype,"f",{set(v){this.k=arguments[0]*2}});
	function args(n){const a=new A();let s=0;for(let i=0;i<n;i++){s+=a.n;a.m=i;s+=a.k}return s}
	function sets(n){const a=new A();let s="";for(let i=0;i<n;i++){a.m=i;s+=a.k+","}return s}
	function firsts(n){const a=new A();let s=0;for(let i=0;i<n;i++){a.f=i;s+=a.k}return s}`, []regWrongCase{
		{"args", []float64{30}, 3, "3735"},
		{"sets", []float64{5}, 3, "100,101,102,103,104,"},
		{"firsts", []float64{30}, 3, "870"},
	})
}

// A closure Go makes on the frame Go entered the code with, for a pool of
// closures run dry, takes the cells native code keeps its captured
// registers in, its by-value captures' too, as the stack form's does
// (jitCellsClosure): made by value from the window's registers, its
// other captures had upvalues opened on them, which went on reading the
// registers native code had left, and one left open there was taken by
// a later entry for its register's cell, two iterations' closures then
// sharing a let (TestJITCellsClosedMarking's loops(200)).
func TestJITRegClosureCellsByValue(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `function lc(n) {
		const fs = []
		for (let i = 0; i < n; i++) {
			const k = i * 2
			let m = i
			fs.push(() => { m++; return m + k })
			if (i % 3 === 0) fs[fs.length - 1]()
		}
		let s = []
		for (let j = 0; j < fs.length; j++) s.push(fs[j]())
		return s.join()
	}`, []regWrongCase{
		{"lc", []float64{40}, 1, "2,4,7,11,13,16,20,22,25,29,31,34,38,40,43,47,49,52,56,58,61,65,67,70,74,76,79,83,85,88,92,94,97,101,103,106,110,112,115,119"},
		{"lc", []float64{5}, 0, "2,4,7,11,13"},
	})
}

// The frames Go makes of inlined levels at an exit inside them are named
// in a stack trace as the interpreter's are: by the function object the
// code inlined each for, and, for one f.apply(x, arguments) forwards to
// with its caller's arguments, by the receiver the interpreter's apply
// gives it -- x, the global object for a sloppy callee's undefined, a
// primitive's wrapper. They had read their windows' registers 0 and 1,
// which inlined code writes only where the callee reads them, and held
// what calls made since had left there: "Number.tThrow" for "tThrow"
// (TestJITForwardersInlined), "at" for "fw".
func TestJITRegForwardReceiverInStack(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `function tT(a) { if (a === 7) throw new RangeError('at ' + a); return a * 2 }
	function tS(a) { 'use strict'; if (a === 7) throw new RangeError('s' + a); return a }
	function fw() { return tT.apply(this, arguments) }
	function fx() { return tT.apply(5, arguments) }
	function fo() { return tT.apply(o, arguments) }
	function fs() { return tS.apply('q', arguments) }
	var o = { k: 1 }
	function at(e) { return e.stack.split(String.fromCharCode(10)).slice(1, 3).map(l => l.trim().split(' ')[1]).join() }
	function w(n) { let s = ''; for (let i = 0; i < n; i++) { try { s += fw(i) } catch (e) { s += e.message + at(e) } } return s }
	function x(n) { let s = ''; for (let i = 0; i < n; i++) { try { s += fx(i) } catch (e) { s += e.message + at(e) } } return s }
	function ob(n) { let s = ''; for (let i = 0; i < n; i++) { try { s += fo(i) } catch (e) { s += e.message + at(e) } } return s }
	function st(n) { let s = ''; for (let i = 0; i < n; i++) { try { s += fs(i) } catch (e) { s += e.message + at(e) } } return s }`, []regWrongCase{
		{"w", []float64{10}, 8, "024681012at 7tT,fw1618"},
		{"x", []float64{10}, 8, "024681012at 7Number.tT,fx1618"},
		{"ob", []float64{10}, 8, "024681012at 7Object.tT,fo1618"},
		{"st", []float64{10}, 8, "0123456s7String.tS,fs89"},
	})
}

// A native callee that left inside a callee its code inlined has its
// frame, which Go makes at the exit, past the call, as the run's own frame
// is then: it had been at the instruction before the call, and a stack
// trace through it named the line before the call's ("Object.walk
// (test:20:5)" for "(test:21:12)", TestJITInlinesCapturedBindings).
func TestJITRegInlinedExitCallerPosition(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, inlineUpvaluesSetup+`
	function wl(k, v) { mod.reset(); n2.v = v; let s = ''; for (let j = 0; j < 2; j++) { try { s += mod.walk(n1, k) + '|' + mod.walk(n2, k) } catch (e) { s += e.stack } } return s }`,
		[]regWrongCase{
			{"wl", []float64{50, 7}, 3, "4225:150:5|11775:300:719225:450:5|26775:600:7"},
			{"wl", []float64{10, 500}, 0, "RangeError: past 3 at 33\n    at Node.check (test:10:46)\n    at Object.walk (test:21:12)\n    at wl (test:49:126)" +
				"RangeError: past 3 at 66\n    at Node.check (test:10:46)\n    at Object.walk (test:21:12)\n    at wl (test:49:126)"},
		})
}

// A loop's callback, inlined in the loop of a built-in's call or called
// natively from it, runs with the receiver Go's forEach gives it, and its
// frames are those Go's make: a sloppy callback that reads its this is
// not inlined, the loop's undefined coerced to the global object by its
// call (this === globalThis had been false, TestJITCallbackInlined's
// sloppyThis gave 0 for 24); and at an exit inside a callback, inlined or
// a native callee, the built-in's frame is made below the callback's and
// the callback's receiver is the loop's thisArg -- a stack trace had left
// "at Array.forEach" out and named the callback "Number.throwAt" by what
// its window held.
func TestJITRegLoopCallbackFrames(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `var nums = [1, 2, 3, 4, 5], u8 = new Uint8Array([1, 2, 3, 4, 5]), self = { k: 1 }
	function at(e) { return e.stack.split(String.fromCharCode(10)).slice(1, 4).map(l => l.trim().split(' ')[1]).join() }
	function sloppy(n) { let s = 0; for (let i = 0; i < n; i++) nums.forEach(function (x) { s += this === globalThis ? 1 : 0 }); return s }
	function reduced(n) { let s = 0; for (let i = 0; i < n; i++) s += nums.reduce(function (a, x) { return a + (this === globalThis ? 1 : 0) }, 0); return s }
	function throwAt(x) { if (x === 3) throw new Error('at ' + x) }
	function strictAt(x) { 'use strict'; if (x === 3) throw new Error('s' + x) }
	function throws(n) { let s = ''; for (let i = 0; i < n; i++) { try { nums.forEach(throwAt) } catch (e) { s += at(e) + ';' } } return s }
	function withThis(n) { let s = ''; for (let i = 0; i < n; i++) { try { nums.forEach(throwAt, self) } catch (e) { s += at(e) + ';' } } return s }
	function strict(n) { let s = ''; for (let i = 0; i < n; i++) { try { nums.forEach(strictAt, 7) } catch (e) { s += at(e) + ';' } } return s }
	function arrows(n) { let s = ''; for (let i = 0; i < n; i++) { try { nums.forEach((x) => { if (x === 3) throw new Error('a') }) } catch (e) { s += at(e) + ';' } } return s }
	function typed(n) { let s = ''; for (let i = 0; i < n; i++) { try { u8.forEach(throwAt) } catch (e) { s += at(e) + ';' } } return s }
	var last = ''
	function stackOf(x) { if (x === 3) last = at(new Error('h')) }
	function stackThis(x) { if (x === 3) last = at(new Error('t')) + typeof this }
	function made(n) { let s = ''; for (let i = 0; i < n; i++) { nums.forEach((x) => { if (x === 3) last = at(new Error('m')) }); s += last + ';' } return s }
	function taken(n) { let s = ''; for (let i = 0; i < n; i++) { nums.forEach(stackOf); s += last + ';' } return s }
	function takenThis(n) { let s = ''; for (let i = 0; i < n; i++) { u8.forEach(stackThis, self); s += last + ';' } return s }
	function inner() { nums.forEach((x) => { if (x === 3) last = at(new Error('i')) }); return last }
	function outer(n) { let s = ''; for (let i = 0; i < n; i++) s += inner() + ';'; return s }`, []regWrongCase{
		{"sloppy", []float64{6}, 8, "30"},
		{"reduced", []float64{6}, 8, "30"},
		{"throws", []float64{3}, 8, "throwAt,Array.forEach,throws;throwAt,Array.forEach,throws;throwAt,Array.forEach,throws;"},
		{"withThis", []float64{3}, 8, "Object.throwAt,Array.forEach,withThis;Object.throwAt,Array.forEach,withThis;Object.throwAt,Array.forEach,withThis;"},
		{"strict", []float64{3}, 8, "Number.strictAt,Array.forEach,strict;Number.strictAt,Array.forEach,strict;Number.strictAt,Array.forEach,strict;"},
		{"arrows", []float64{3}, 8, "test:10:112,Array.forEach,arrows;test:10:112,Array.forEach,arrows;test:10:112,Array.forEach,arrows;"},
		{"typed", []float64{3}, 8, "throwAt,Uint8Array.forEach,typed;throwAt,Uint8Array.forEach,typed;throwAt,Uint8Array.forEach,typed;"},
		{"made", []float64{3}, 8, "test:15:108,Array.forEach,made;test:15:108,Array.forEach,made;test:15:108,Array.forEach,made;"},
		{"taken", []float64{3}, 8, "stackOf,Array.forEach,taken;stackOf,Array.forEach,taken;stackOf,Array.forEach,taken;"},
		{"outer", []float64{3}, 8, "test:18:66,Array.forEach,inner;test:18:66,Array.forEach,inner;test:18:66,Array.forEach,inner;"},
		{"takenThis", []float64{3}, 8, "Object.stackThis,Uint8Array.forEach,takenThisobject;Object.stackThis,Uint8Array.forEach,takenThisobject;Object.stackThis,Uint8Array.forEach,takenThisobject;"},
	})
}

// A callee inlined at a method's call, a read's getter or a write's setter
// is named in a stack trace by its receiver, which every state inside it
// holds (ssa's frameState forces a register callee's register 1): left
// out where the callee did not read it, the frame Go made of the level at
// an exit had been named by what the window held, "Number.leaf" for
// "Leaf.leaf", "Number.get" for "N.get [as far]" (TestJITTailCallsInlined,
// TestJITAccessors, TestJITInlinedSmallLoopFrames).
func TestJITRegInlinedReceiverInStack(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `class Leaf { constructor(x) { this.x = x } leaf() { return this.x.y } }
	function N(o) { this.o = o }
	Object.defineProperty(N.prototype, 'far', { get() { return this.o.v } })
	function Box(v) { this.v = v; this.n = 0 }
	Object.defineProperty(Box.prototype, 'add', { set(k) { this.n += k + this.v.w } })
	var leaves = [new Leaf({ y: 1 }), new Leaf({ y: 2 }), new Leaf(null)], ns = [new N({ v: 1 }), new N({ v: 2 }), new N(undefined)]
	var boxes = [new Box({ w: 1 }), new Box({ w: 2 }), new Box(undefined)]
	function at(e) { return e.stack.split(String.fromCharCode(10)).slice(1, 3).map(l => l.trim().split(' ').slice(1, -1).join(' ')).join() }
	function down(o) { return o.leaf() }
	function methods(n) { let s = 0, e = ''; for (let i = 0; i < n; i++) { try { s += down(leaves[i % 3 === 2 && i > n - 3 ? 2 : i % 2]) } catch (x) { e = at(x) } } return s + ':' + e }
	function getters(n) { let s = 0, e = ''; for (let i = 0; i < n; i++) { try { s += ns[i % 3 === 2 && i > n - 3 ? 2 : i % 2].far } catch (x) { e = at(x) } } return s + ':' + e }
	function setters(n) { let e = ''; boxes[0].n = 0; for (let i = 0; i < n; i++) { try { boxes[i % 3 === 2 && i > n - 3 ? 2 : i % 2].add = i } catch (x) { e = at(x) } } return boxes[0].n + ':' + e }`, []regWrongCase{
		{"methods", []float64{30}, 8, "43:Leaf.leaf,down"},
		{"getters", []float64{30}, 8, "43:N.get [as far],getters"},
		{"setters", []float64{30}, 8, "225:Box.set [as add],setters"},
	})
}

// A construction with a class that has no constructor of its own, made
// with its parent's code while the class's prototype is still that
// parent (ssa.CallSite's Parent), is not inlined, which would not check
// the parent: given another by Object.setPrototypeOf, the class had gone
// on constructing with the old one's code (TestJITDerivedDefaultConstruct's
// case 7, h for q).
func TestJITRegDefaultDerivedParentChanged(t *testing.T) {
	t.Parallel()
	regWrongCheck(t, `class P { constructor(h) { this.h = h } }
	class D extends P {}
	class Q { constructor(h) { this.q = h } }
	function make(n, q) { Object.setPrototypeOf(D, q ? Q : P); let o, s = 0; for (let i = 0; i < n; i++) { o = new D(i); s += o.h === i ? 1 : o.q === i ? 100 : 0 } return Object.keys(o).join() + s }`, []regWrongCase{
		{"make", []float64{50, 0}, 4, "h50"},
		{"make", []float64{5, 1}, 0, "q500"},
		{"make", []float64{5, 0}, 0, "h5"},
	})
}
