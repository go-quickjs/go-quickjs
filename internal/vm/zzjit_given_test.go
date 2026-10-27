//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// givenSetup's functions each give a closure they make to a method with a
// loop that calls it, SOM's Vector.forEach and hasSome (AWFY's Havlak and
// DeltaBlue): the method is inlined with its loop, the closure inlined at
// the loop's call of it and never made (ssa's calledClosures, givenSite).
// Then each meets what leaves native code inside the inlined loop or the
// closure -- a hole, a shape the code was not compiled for, a throw, a
// stack trace taken -- where Go makes the method's frame and the closure
// the code did not make (jitMaterializeIn), the same closure in every
// frame naming it. swaps' method assigns its parameter after the call,
// which the first build, leaving the call to an exit, did not see:
// inlining the closure there would call it at every turn, so the second
// build is refused (unmadeAlone). scaled's loop calls nothing it is
// given, and passes' first forEach is given a function the caller does not
// make: neither is inlined, the call of the loop made once for its turns.
const givenSetup = `
	class Vec {
		constructor(n) { this.storage = []; this.firstIdx = 0; this.lastIdx = 0; for (let i = 0; i < n; i++) this.append(i) }
		append(e) { this.storage[this.lastIdx] = e; this.lastIdx += 1 }
		forEach(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { fn(this.storage[i]) } }
		hasSome(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { if (fn(this.storage[i])) { return true } } return false }
		swapping(fn, other) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { fn(this.storage[i]); fn = other } }
		scaled(k) { let s = 0; for (let i = this.firstIdx; i < this.lastIdx; i += 1) { s += this.storage[i] * k } return s }
	}
	var vec = new Vec(12), holey = new Vec(12), objs = new Vec(0)
	delete holey.storage[7]
	for (let i = 0; i < 9; i++) objs.append({ v: i })
	var odd = { w: 0, v: -50 }
	function twice(x) { return 2 * x }
	function sum(v, k) { let s = 0; v.forEach((x) => { s += x * k }); return s }
	function has(v, k) { const r = v.hasSome((x) => x === k); return r ? 'yes' : 'no' }
	function count(v, k) { let n = 0; v.forEach((x) => { if (x > k) n++ }); return n }
	function shapes(v, at) { let s = 0; v.forEach((o) => { s += (o.v === at ? odd : o).v }); return s }
	function throwsAt(v, at) {
		let s = 0
		try { v.forEach((x) => { s += x; if (x === at) throw new RangeError('at ' + x + ':' + s) }) } catch (e) { return e.message }
		return s
	}
	function stackAt(v, at) { let st = 'none'; v.forEach((x) => { if (x === at) st = new Error('h').stack }); return st }
	var total = 0
	function minus(x) { total -= x }
	function swaps(v, k) { let s = 0; total = k; v.swapping((x) => { s += x * k }, minus); return s + ':' + total }
	function scaledBy(v, k) { const f = (x) => x + k; return f(v.scaled(k)) }
	function passes(v, k) { let s = 0; v.forEach(twice); v.forEach((x) => { s += x * k }); return s }
	function each(a, f) { var i, s = 0; for (i = 0; i < a.storage.length; i++) s = (s + f(a.storage[i])) | 0; return s }
	function sloppy(v, k) { var j, s = 0; for (j = 0; j < 3; j++) s = (s + each(v, function (x) { return x * j + k })) | 0; return s }
	function drive(n, v, k) { let r = 0; for (let i = 0; i < n; i++) r += sum(v, i % k) + count(v, i % k); return r }
`

// givenNames are givenSetup's functions a test drives.
var givenNames = []string{"sum", "has", "count", "shapes", "throwsAt", "stackAt", "swaps", "scaledBy", "passes", "sloppy"}

// givenRounds drives givenSetup's functions, the all-number rounds first,
// then those with exits inside the inlined loops: holes, shapes, throws,
// stack traces.
func givenRounds(exits bool) []string {
	drivers := []string{}
	for _, name := range givenNames {
		drivers = append(drivers, "(() => { let r; for (let i = 0; i < 30; i++) r = "+name+"(%s, %s); return r })()")
	}
	var rounds []string
	for i := range 6 {
		v, k, at := "vec", "3", "99"
		if exits && i >= 4 {
			v, k, at = []string{"holey", "objs"}[i-4], "5", "6"
		}
		for j, d := range drivers {
			arg, key := v, k
			switch givenNames[j] {
			case "shapes":
				arg, key = "objs", at
			case "throwsAt", "stackAt":
				key = at
			}
			rounds = append(rounds, sprintf2(d, arg, key))
		}
	}
	return rounds
}

// sprintf2 is format with its two %s replaced by a and b.
func sprintf2(format, a, b string) string {
	for _, s := range []string{a, b} {
		for i := 0; i+1 < len(format); i++ {
			if format[i] == '%' && format[i+1] == 's' {
				format = format[:i] + s + format[i+2:]
				break
			}
		}
	}
	return format
}

// A closure a function makes and gives to a method calling it in a loop
// is inlined there with the method, and not made; and every answer is the
// interpreter's, whatever leaves native code inside the loop or the
// closure, under budgets of polls and of finished invocations or not.
func TestJITGivenClosures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, givenSetup, givenRounds(false)...)
	// How many of each function's calls its code inlines (ssaInlinedAt),
	// and how many of its closures it does not make.
	for _, c := range []struct {
		name            string
		inlined, unmade int
	}{
		{"sum", 1, 1}, {"has", 1, 1}, {"count", 1, 1}, {"shapes", 1, 1}, {"throwsAt", 1, 1},
		// What the method calls is assigned after the call: neither the
		// method nor the closures.
		{"swaps", 0, 0},
		// The loop calls nothing it is given: f's call alone, which the
		// function makes and calls itself.
		{"scaledBy", 0, 1},
		// The first forEach is given a function the caller does not make.
		{"passes", 1, 1},
		// A function's, its parameter read with another local
		// (get_local2), its closure made in the caller's loop.
		{"sloppy", 1, 1},
	} {
		e := jitCallbackEntry(r, c.name)
		if e == nil || e.ssa == nil {
			t.Errorf("%s was not compiled", c.name)
			continue
		}
		if len(e.ssaInlinedAt) != c.inlined || e.ssaCalled != c.unmade {
			t.Errorf("%s inlines %d calls, %d closures not made; want %d, %d", c.name, len(e.ssaInlinedAt), e.ssaCalled, c.inlined, c.unmade)
		}
	}
	r = jitAgreesForTestWith(t, jitStressConfig{}, givenSetup, givenRounds(true)...)
	if r.jit.materialized == 0 {
		t.Error("Go made no closure the code did not make")
	}
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		r := jitAgreesForTestWith(t, c, givenSetup, givenRounds(true)...)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
	}
}

// A closure given to a method's loop and not made is no allocation, nor
// are the cells it captures, which go back to their pool at the call's
// return (ssa.Func's CellsBack); and the collector, run at every call of
// Go, finds what the frames hold.
func TestJITGivenClosuresAllocate(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTestWith(t, jitStressConfig{}, givenSetup, `drive(50, vec, 4)`, `drive(50, vec, 4)`, `drive(50, vec, 4)`)
	jitCallbackMallocs(t, r, `drive(5000, vec, 4)`)
	n := jitCallbackMallocs(t, r, `drive(5000, vec, 4)`)
	t.Logf("drive(5000): %d allocations", n)
	if n > 200 {
		t.Errorf("drive(5000): %d allocations", n)
	}
	calls := 0
	jitGoCallWork = func() {
		if calls++; calls%3 == 0 {
			runtime.GC()
		}
	}
	defer func() { jitGoCallWork = nil }()
	jitAgreesForTestWith(t, jitStressConfig{threshold: true, budget: 2, deopt: 3}, givenSetup, givenRounds(true)...)
	if calls == 0 {
		t.Error("no call of Go")
	}
}
