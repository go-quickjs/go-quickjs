//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// callbackSetup's functions each make a loop of a built-in's call whose
// callback native code inlines in the loop (ssa's inlineCallback), where
// it is a function made once, which the loop checks it is given -- and
// arrows and functions made at the call, which it calls natively, reading
// and assigning what they capture. Then each meets what leaves native
// code inside the callback, where Go makes the callback's frame and goes
// on with the loop (jitLoopFinish): a hole, a property of a shape the code
// was not compiled for, a throw, a call never seen made, an array the
// callback shortens or lengthens, a this given, a callee inlined in the
// callback that leaves.
const callbackSetup = `
	var nums = [1, 2, 3, 4, 5, 6, 7, 8]
	var objs = []
	for (let i = 0; i < 6; i++) objs.push({ v: i })
	var odd = { w: 1, v: 100 }
	var holey = [1, , 3, , , 6, 7, , 9]
	var thisObj = { k: 10 }
	var i32 = new Int32Array([5, -3, 8, 13, 21, -1, 7, 2])
	var total = 0
	function addTotal(x) { total += x }
	function eachMade(k) { let s = 0; nums.forEach((x, i, a) => { s += x * k + i + (a === nums ? 1 : 0) }); return s }
	function reduceMade(k) { return nums.reduce((a, x, i) => a + x * k + i, 0) + ':' + nums.reduce((a, x) => a * 2 + x) }
	function mapMade(k) { return nums.map((x, i) => x * k + i).join() }
	function filterMade(k) { return nums.filter((x) => (x * k) % 3 === 0).join() }
	function someMade(n) { let calls = 0; const r = nums.some((x) => { calls++; return x === n }); return r + ':' + calls }
	function everyMade(n) { let calls = 0; const r = nums.every((x) => { calls++; return x < n }); return r + ':' + calls }
	function findMade(n) { const r = objs.find((o) => o.v === n); return r === undefined ? 'none' : r.v }
	function findIndexMade(n) { return nums.findIndex(function (x) { return x === n }) }
	function typedMade(k) { let s = 0; i32.forEach((x, i) => { s += x * k - i }); return s }
	function outer(k) {
		let base = k * 3
		return function inner(m) { let s = 0; nums.forEach((x) => { s += x * m + base; base++ }); return s + ':' + base }
	}
	var innerFn = outer(2)
	function captured(m) { return innerFn(m) }
	function perIteration(n) {
		let r = ''
		for (let j = 0; j < n; j++) { let s = 0; nums.forEach((x) => { s += x + j }); r += s + ',' }
		return r
	}
	function eachKnown() { total = 0; nums.forEach(addTotal); return total }
	function addTwice(x) { total += 2 * x }
	function eachWith(f) { total = 0; nums.forEach(f); return total }
	var atShape = -1
	function shapeOf(o, i) { total += (i === atShape ? odd : o).v }
	function knownShapes(at) { atShape = at; total = 0; objs.forEach(shapeOf); return total }
	function throwAt(x) { total += x; if (x === atShape) throw new RangeError('at ' + x + ':' + total) }
	function knownStack() { total = nums[0] - 1; nums.forEach(throwAt); return total }
	function knownThrows(at) { atShape = at; total = 0; try { nums.forEach(throwAt) } catch (e) { return e.message } return total }
	function eachThis(n) { let s = 0; const o = thisObj; nums.forEach(function (x) { 'use strict'; s += this.k * x + n }, o); return s }
	function globalThisArg(n) { let s = 0; nums.forEach(function (x) { 'use strict'; s += this.k * x + n }, thisObj); return s }
	function sloppyThis(n) { let s = 0; nums.forEach(function (x) { s += this === globalThis ? n : 0 }); return s }
	function holes(k) { let s = 0, calls = 0; holey.forEach((x, i) => { s += x * k + i; calls++ }); return s + ':' + calls }
	function mapHoles(k) { const r = holey.map((x, i) => x * k + i); return r.length + ':' + Object.keys(r).join() + ':' + r.join() }
	function shapes(at) { let s = 0; objs.forEach((o, i) => { s += (i === at ? odd : o).v }); return s }
	function reduceShapes(at) { return objs.reduce((s, o, i) => s + (i === at ? odd : o).v, 0) }
	function throwsAt(n) {
		let s = 0
		try { nums.forEach((x) => { s += x; if (x === n) throw new RangeError('at ' + x) }) } catch (e) { return s + ':' + e.message }
		return s
	}
	function throwsIn(n) { let s = nums[0] - 1; nums.forEach((x) => { s += x; if (x === n) throw new RangeError('at ' + x + ':' + s) }); return s }
	function catches(n) { try { return throwsIn(n) } catch (e) { return e.message } }
	function coldCall(n) { let s = 0; nums.forEach((x) => { s += x; if (x === n) s += String(x).length * 1000 }); return s }
	function shorten(n) { const a = [1, 2, 3, 4, 5, 6, 7, 8]; let s = 0; a.forEach((x) => { s += x; if (x === n) a.length = 3 }); return s + ':' + a.length }
	function lengthen(n) { const a = [1, 2, 3]; let s = 0; a.forEach((x) => { s += x; if (a.length < 6) a.push(x * n) }); return s + ':' + a.length }
	function vOf(o) { return o.v }
	function nested(at) { let s = 0; objs.forEach((o, i) => { s += vOf(i === at ? odd : o) }); return s }
	var last = ''
	function stackAt(n) { nums.forEach((x) => { if (x === n) last = new Error('h').stack }); return last }
	function defaults(k) { let s = 0; nums.forEach((x, i = 5, a, d = k) => { s += x * d + i }); return s }
	function destructures(k) { let s = 0; objs.forEach(({ v }, i) => { s += v * k + i }); return s }
	function outerArguments(k) { let s = 0; nums.forEach((x) => { s += x * arguments[0] + arguments.length }); return s }
	function deadZone(n) {
		let s = nums[0] - 1
		try { nums.forEach((x) => { s += x; if (x === n) s += late }) } catch (e) { return s + ':' + e.constructor.name }
		let late = 1
		return s
	}
	function constWrite(n) {
		const c = 1
		let s = nums[0] - 1
		try { nums.forEach((x) => { s += x; if (x === n) c = 2 }) } catch (e) { return s + ':' + e.constructor.name }
		return s
	}
	function nestedLoops(k) { let s = 0; nums.forEach((x) => { objs.forEach((o) => { s += x * o.v * k }) }); return s }
	function returns(n) { return nums.filter((x) => { if (x > n) return true; if (x === 1) return true; return false }).join() + ':' + nums.findIndex((x) => { if (x > n) { return x % 2 === 0 } return false }) }
	function* genEach(k) { let s = nums[0] - 1; nums.forEach((x) => { s += x * k }); yield s; nums.forEach((x) => { s += x }); yield s }
	function gen(k) { let r = 0; for (const s of genEach(k)) r += s; return r }
`

// callbackNames are callbackSetup's functions, each driven from a loop of
// a driver's, natively, as a benchmark's loop calls its function.
var callbackNames = []string{"eachMade", "reduceMade", "mapMade", "filterMade", "someMade", "everyMade", "findMade", "findIndexMade",
	"typedMade", "captured", "perIteration", "eachKnown", "eachWith", "knownShapes", "knownThrows", "eachThis", "globalThisArg", "sloppyThis", "holes", "mapHoles", "shapes", "reduceShapes",
	"throwsAt", "catches", "coldCall", "defaults", "destructures", "outerArguments", "deadZone", "constWrite", "nestedLoops", "returns", "shorten", "lengthen", "nested", "gen"}

func callbackDrivers() string {
	drivers := ""
	for _, name := range callbackNames {
		drivers += "function drive_" + name + "(n, a) { let r; for (let i = 0; i < n; i++) r = " + name + "(a); return r };"
	}
	return drivers
}

// A loop's callback is inlined in the loop native code makes of its call,
// as V8's JSCallReducer inlines it, where the callee may be inlined: each
// answer is the interpreter's, whatever leaves native code inside it --
// at every point, the loop's turns before it done natively and those
// after it by Go, the callback's frame made by Go.
func TestJITCallbackInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for i := range 8 {
		late := []string{"99", "3"}[i/6]
		rounds = append(rounds, `drive_eachMade(30, 2)`, `drive_reduceMade(30, 3)`, `drive_mapMade(30, 2)`, `drive_filterMade(30, 1)`,
			`drive_someMade(30, 5)`, `drive_someMade(30, 99)`, `drive_everyMade(30, 99)`, `drive_everyMade(30, 4)`,
			`drive_findMade(30, 3)`, `drive_findMade(30, 99)`, `drive_findIndexMade(30, 6)`, `drive_findIndexMade(30, 99)`,
			`drive_typedMade(30, 2)`, `drive_captured(30, 2)`, `drive_perIteration(30, 3)`, `drive_eachKnown(30)`, `drive_eachWith(30, `+[]string{"addTotal", "addTwice"}[i/6]+`)`,
			`drive_knownShapes(30, `+late+`)`, `drive_knownThrows(30, `+late+`)`,
			`drive_eachThis(30, 2)`, `drive_globalThisArg(30, 2)`, `drive_sloppyThis(30, 3)`, `drive_holes(30, 2)`, `drive_mapHoles(30, 3)`,
			`drive_shapes(30, `+late+`)`, `drive_reduceShapes(30, `+late+`)`, `drive_throwsAt(30, `+late+`)`, `drive_defaults(30, 2)`, `drive_destructures(30, 3)`, `drive_outerArguments(30, 2)`,
			`drive_deadZone(30, `+late+`)`, `drive_constWrite(30, `+late+`)`, `drive_nestedLoops(30, 2)`, `drive_returns(30, `+late+`)`, `drive_catches(30, `+late+`)`,
			`drive_coldCall(30, `+late+`)`, `drive_shorten(30, `+late+`)`, `drive_lengthen(30, 2)`, `drive_nested(30, `+late+`)`,
			`drive_gen(30, 2)`)
	}
	for range 3 {
		rounds = append(rounds, `(() => { try { for (let i = 0; i < 40; i++) { atShape = i === 39 ? 3 : 99; knownStack() } } catch (e) { return e.stack } })()`,
			`(() => { let s; for (let i = 0; i < 40; i++) s = stackAt(3); return s })()`,
			`(() => { try { for (let i = 0; i < 40; i++) throwsIn(i === 39 ? 3 : 99) } catch (e) { return e.stack } })()`,
			`(() => { try { for (let i = 0; i < 40; i++) nums.forEach((x) => { if (x === 4 && i > 30) null.f }) } catch (e) { return e.stack } })()`)
	}
	r := jitAgreesForTest(t, callbackSetup+callbackDrivers(), rounds...)
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
	// The callbacks inlined: the functions made once.
	for _, name := range []string{"eachKnown", "eachWith", "knownShapes"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaCallbacks != 1 {
			t.Errorf("%s inlines no callback: %+v", name, e)
		}
	}
}

// The functions of callbackSetup entered by Go, from code the interpreter
// runs, never called natively: an exit inside the callback inlined in a
// loop goes back to the run Go entered, which makes the callback's frame,
// finishes it, and goes on with the loop after the callback's index
// (runSSA). Each answer is the interpreter's.
func TestJITCallbackEnteredByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each reads an element, which has Go enter it (LowerSSA's hostOnly).
	setup := callbackSetup + `
		function goShapes(at) { let s = nums[0] - 1; objs.forEach((o, i) => { s += (i === at ? odd : o).v }); return s }
		function goReduce(at) { const z = nums[0] - 1; return objs.reduce((s, o, i) => s + (i === at ? odd : o).v, z) }
		function goNested(at) { let s = nums[0] - 1; objs.forEach((o, i) => { s += vOf(i === at ? odd : o) }); return s }
		function goMap(at) { const k = nums[0]; return objs.map((o, i) => (i === at ? odd : o).v + k).join() }
		function goSome(at) { let calls = nums[0] - 1; const r = objs.some((o, i) => { calls++; return (i === at ? odd : o).v > 50 }); return r + ':' + calls }
		function goHoles(k) { let s = nums[0] - 1; holey.forEach((x, i) => { s += x * k + i }); return s }
		function goWith(f) { total = nums[0] - 1; nums.forEach(f); return total }
		function goStack() { total = nums[0] - 1; nums.forEach(throwAt); return total }
		function goKnown(at) { atShape = at; total = nums[0] - 1; objs.forEach(shapeOf); return total }`
	calls := func(f string, n int) string {
		r := "[" + f
		for range n - 1 {
			r += "," + f
		}
		return r + "].join()"
	}
	rounds := []string{}
	for _, at := range []string{"99", "99", "3", "99", "2", "4"} {
		for _, f := range []string{"goShapes(" + at + ")", "goReduce(" + at + ")", "goNested(" + at + ")", "goMap(" + at + ")",
			"goSome(" + at + ")", "goStack()", "goHoles(2)", "catches(" + at + ")", "goKnown(" + at + ")", "goWith(" + []string{"addTotal", "addTwice"}[len(at)%2] + ")"} {
			rounds = append(rounds, calls(f, 12))
		}
	}
	rounds = append(rounds, `(() => { try { throwsIn(3) } catch (e) { return e.stack } })()`,
		`(() => { try { atShape = 3; goStack() } catch (e) { return e.stack } })()`)
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"goWith", "goKnown"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaCallbacks == 0 || e.ssaStats.entries == 0 {
			t.Errorf("%s inlines no callback, or Go entered it not: %+v", name, e)
		}
	}
	if r.jit.loopsFinished == 0 {
		t.Error("Go went on with no loop")
	}
}

// jitCallbackEntry is the entry of name's function in r.
func jitCallbackEntry(r *Runtime, name string) *jitEntry {
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == name {
			return x
		}
	}
	return nil
}

// An inlined callback that leaves native code at every call is called
// natively after a few (jitInlineExits), not inlined, its code compiled
// again for that: each answer the interpreter's.
func TestJITCallbackLeavesOften(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var nums = [1, 2, 3, 4, 5, 6, 7, 8]
		var shapes = [{ v: 1 }, { w: 0, v: 2 }, { u: 0, w: 0, v: 3 }, { t: 0, u: 0, w: 0, v: 4 }, { s: 0, t: 0, u: 0, w: 0, v: 5 }]
		var total = 0
		function thrower(x, i) { total += x; if (i === 6) throw total }
		function leavesKnown(k) {
			total = nums[0] - 1 + k
			try { nums.forEach(thrower) } catch (e) { return e + ':' + total }
			return total
		}
		function drive(n) { let r = ''; for (let i = 0; i < n; i++) r += leavesKnown(i); return r }`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `drive(40)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	for _, name := range []string{"leavesKnown"} {
		e := jitCallbackEntry(r, name)
		if e == nil || e.ssa == nil || !e.loops {
			t.Fatalf("%s makes no loop: %+v", name, e)
		}
		left := false
		for _, x := range e.nativeCalls {
			left = left || x.forEach && x.cbLeft
		}
		if !left || e.ssaCallbacks != 0 {
			t.Errorf("%s's callback, which leaves at every call, is inlined still: %d", name, e.ssaCallbacks)
		}
	}
}
