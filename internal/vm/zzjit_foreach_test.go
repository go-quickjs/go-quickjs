//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// forEachSetup's functions each make a forEach that native code makes a
// loop of, its callback an arrow made anew at each call, as AWFY NBody's
// are -- and then make it meet what the loop leaves for Go to go on with:
// a hole, an array the callback shortens or lengthens, a callback that
// throws, or leaves native code at a check of its own, a this given as
// forEach's second argument, a sloppy callback's this, a callback that
// is no function, an empty array, forEach replaced.
const forEachSetup = `
	var bodies = []
	for (let i = 0; i < 6; i++) bodies.push({ x: i, v: i * 2 })
	function sums(dt) {
		let s = 0
		bodies.forEach((b) => { b.x += dt * b.v; s += b.x })
		return s
	}
	var holey = [1, 2, , 4, , 6]
	function holes(k) {
		let s = 0
		holey.forEach((x, i, a) => { s += x * k + i + (a === holey ? 1 : 0) })
		return s
	}
	var sparse = [1, , 3, , , 6, 7, , 9]
	function holeCalls(k) {
		let calls = 0
		sparse.forEach(() => { calls++ })
		return calls * k
	}
	function shorten(n) {
		const a = [1, 2, 3, 4, 5, 6, 7, 8]
		let s = 0
		a.forEach((x) => { s += x; if (x === n) a.length = 3 })
		return s + ':' + a.length
	}
	function lengthen(n) {
		const a = [1, 2, 3]
		let s = 0
		a.forEach((x) => { s += x; if (a.length < 6) a.push(x * n) })
		return s + ':' + a.length
	}
	function throws(n) {
		let s = 0
		try {
			bodies.forEach((b, i) => { if (i === n) throw new RangeError('at ' + i); s += i })
		} catch (e) {
			return s + ':' + e.message
		}
		return s
	}
	function leaves(n) {
		let s = 0
		bodies.forEach((b, i) => { s += i === n ? 'x'.length : i })
		return s
	}
	var thisObj = { k: 10 }
	function withThis(n) {
		let s = 0
		bodies.forEach(function (b, i) { s += this.k * i + n }, thisObj)
		return s
	}
	function sloppyThis() {
		let s = 0
		bodies.forEach(function (b, i) { s += this === globalThis ? 1 : 0 })
		return s
	}
	function strictThis() {
		'use strict'
		let s = 0
		bodies.forEach(function (b, i) { s += this === undefined ? 1 : 0 })
		return s
	}
	function notFunction(f, a) {
		try {
			a.forEach(f)
		} catch (e) {
			return e.constructor.name + ': ' + e.message
		}
		return 'none'
	}
	function arrowOver(a) { let s = 0; a.forEach((x) => { s += x }); return s }
`

// A forEach on an array is a loop of the caller's native code (ssa's
// forEachCall), its callback called natively, where Go had made every
// forEach and run its callbacks in the tree tier; what the loop's checks
// find -- a hole, an element past the array's end now, a callback that
// leaves -- Go goes on with from that index (jitLoopFinish), as V8's
// builtin continuation frames do. Each answer is the interpreter's, and
// sums, whose loop meets none of it, neither leaves its code nor calls Go.
func TestJITForEachLoop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each called from a loop of a driver's, natively, as NBody's advance
	// is from its benchmark's loop.
	drivers := ""
	for _, name := range []string{"sums", "holes", "holeCalls", "shorten", "lengthen", "throws", "leaves", "withThis", "sloppyThis", "strictThis", "arrowOver"} {
		drivers += "function drive_" + name + "(n, a) { let r; for (let i = 0; i < n; i++) r = " + name + "(a); return r };"
	}
	rounds := []string{}
	for i := range 8 {
		rounds = append(rounds,
			`drive_sums(30, 0.01)`, `drive_holes(30, 3)`, `drive_holeCalls(30, 2)`, `drive_shorten(30, 5)`, `drive_lengthen(30, 2)`, `drive_throws(30, 4)`, `drive_throws(30, 9)`,
			`drive_leaves(30, `+[]string{"9", "3"}[i/6]+`)`, `drive_withThis(30, 2)`, `drive_sloppyThis(30)`, `drive_strictThis(30)`,
			`drive_arrowOver(30, [1, 2, 3, 4])`)
	}
	rounds = append(rounds, `notFunction(x => x, [1, 2])`, `notFunction(5, [1, 2])`, `notFunction(5, [])`, `notFunction(null, bodies)`,
		`drive_arrowOver(30, [])`, `(Array.prototype.forEach = function (f) { f(100) }, drive_arrowOver(30, [1, 2]))`)
	r := jitAgreesForTest(t, forEachSetup+drivers, rounds...)
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == "sums" {
			e = x
		}
	}
	if e == nil || e.ssa == nil || !e.loops {
		t.Fatalf("sums makes no loop of its forEach: %+v", e)
	}
	if e.ssaStats.goCalls != 0 || e.ssaStats.hosts != 0 {
		t.Errorf("sums called Go %d times, left its code %d times", e.ssaStats.goCalls, e.ssaStats.hosts)
	}
	// The holes, the lengths changed, the callbacks that left: Go went on
	// with those loops.
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
}

// afterCallbackSetup's visit makes a forEach native code makes a loop of,
// whose callback, at the index at, has visit's code invalidated
// (invalidateVisit) and then leaves native code itself, at an object of
// another shape: Go finishes the callback, and visit's code, invalid, does
// not go on after the call.
const afterCallbackSetup = `
	var items = []
	for (let i = 0; i < 8; i++) items.push({ v: i })
	var odd = { w: 0, v: 100 }
	function visit(at) {
		let s = 0
		items.forEach((o, i) => {
			if (i === at) invalidateVisit()
			s += (i === at ? odd : o).v
		})
		return s
	}
	function drive(n, at) { let r = 0; for (let i = 0; i < n; i++) r += visit(at); return r }
`

// A callback that leaves native code where the loop's code cannot go on
// after its call -- invalidated meanwhile -- has Go go on with the loop
// from the index after the callback's (jitLoopFinish's after), the
// callback called once for each element: each answer is the
// interpreter's.
func TestJITForEachAfterCallback(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("invalidateVisit"), Obj(rt.newNativeFunc("invalidateVisit", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				if rt != r {
					return Undefined, nil
				}
				for k, x := range rt.jit.cache {
					if fn := k.Value(); fn != nil && fn.Name == "visit" && x.ssa != nil {
						fd := rt.global.getOwn(rt.atoms.intern("visit")).value().Object().fn()
						rt.jitInvalidate(fd.closure, x)
					}
				}
				return Undefined, nil
			})), propDefault)
		if _, err := rt.Run(compileForTest(t, afterCallbackSetup)); err != nil {
			t.Fatal(err)
		}
	}
	before := r.jit.loopsFinished
	for i, src := range []string{`drive(40, -1)`, `drive(40, -1)`, `drive(40, -1)`, `drive(3, 4)`, `drive(3, 2)`, `drive(40, -1)`} {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		if werr != nil || gerr != nil || gv.String().Go() != wv.String().Go() {
			t.Fatalf("round %d (%s): %v, %v; interpreter %v, %v", i, src, gv, gerr, wv, werr)
		}
	}
	if r.jit.loopsFinished == before {
		t.Error("Go went on with no loop")
	}
}

// stackSetup's throws throws from its forEach's callback, which leaves
// native code there, Go making the frames of the levels native code had
// (jitUnwindNative); undefinedCall's callback calls a global that is not
// there, a call of Go from the native callback (jitGoCallNested); both
// from a driver's loop, natively.
const stackSetup = `
	var nums = [1, 2, 3]
	function throws(n) { let s = 0; nums.forEach((x) => { if (x === n) throw new Error('at ' + x); s += x }); return s }
	function undefinedCall(n) { let s = 0; nums.forEach((x) => { if (x === n) notDefined(); s += x }); return s }
	function drive(m, n) { let r = 0; for (let i = 0; i < m; i++) r += throws(n) + undefinedCall(n); return r }
`

// A stack trace taken in a callback of a forEach native code makes a loop
// of shows the forEach's frame, "at Array.forEach", where Go's forEach
// has it -- the frame Go makes between the loop's level and its callee's
// (jitPushLoopFrame) -- as the interpreter's trace does.
func TestJITForEachStackTrace(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`drive(50, -1)`, `drive(50, -1)`}
	for range 3 {
		rounds = append(rounds,
			`(() => { try { drive(2, 2) } catch (e) { return e.stack } })()`,
			`(() => { try { for (let i = 0; i < 3; i++) undefinedCall(2) } catch (e) { return e.stack } })()`)
	}
	r := jitAgreesForTest(t, stackSetup, rounds...)
	for _, name := range []string{"throws", "undefinedCall"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil || !e.loops {
			t.Errorf("%s makes no loop of its forEach", name)
		}
	}
}
