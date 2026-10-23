//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// calledSetup's functions each make a closure that nothing but their own
// calls of it uses, which their code inlines and does not make (ssa's
// called.go): an arrow over a parameter, called in a loop; a function
// expression made in a loop's turn over the turn's own binding; a closure
// that assigns the bindings it captured; one whose body calls another
// closure, which is made. Then each meets what leaves native code inside
// the inlined closure -- a number that is no number, a throw, a stack trace
// taken -- where Go makes the closure, on the frame's cells, for the frame
// of the call it was in and for every slot naming it, and the interpreter
// goes on with it: the bindings it assigns then the frame's.
const calledSetup = `
	var odd = { v: 'x' }
	function squares(n, at) {
		let s = 0
		const sq = (x) => (x === at ? odd.v : x) * x + n
		for (let i = 0; i < n; i++) s += sq(i)
		return s
	}
	function turns(n, at) {
		var j, s = 0
		for (j = 0; j < n; j++) {
			let k = j
			var f = function () { return k === at ? odd.v + k : k * 2 }
			s = s + f()
		}
		return s
	}
	function bumps(n, at) {
		let s = 0, c = 0
		const bump = (d) => { s += d === at ? odd.v : d; c++ }
		for (let i = 0; i < n; i++) { bump(i); bump(1) }
		return s + ':' + c
	}
	function nests(n, at) {
		let s = 0
		const add = (a, b) => a + b
		const step = (x) => add(x === at ? odd.v : x, 1)
		for (let i = 0; i < n; i++) s = step(i) + (typeof s === 'string' ? '' : s)
		return s
	}
	function T(x) { if (x === 13) throw new RangeError('at ' + x); return x }
	function throws(n, at) {
		let s = 0
		const check = (x) => T(x === at ? 13 : x) + 1
		for (let i = 0; i < n; i++) { try { s += check(i) } catch (e) { s += e.message.length } }
		return s
	}
	function traces(n, at) {
		let s = ''
		const where = function where(x) { return x === at ? new Error('w').stack.split('\n').length + ':' + (typeof where) : x }
		for (let i = 0; i < n; i++) s = where(i) + (s.length > 40 ? '' : s)
		return s
	}
	function drive(n) { let r = 0; for (let i = 0; i < n; i++) r += squares(8, -1); return r }
`

// calledRounds drives calledSetup's functions past what native code makes
// of them in the warm rounds, and at what leaves in the last ones.
func calledRounds() []string {
	var rounds []string
	for i := range 8 {
		at := []string{"-1", "17"}[i/5]
		for _, name := range []string{"squares", "turns", "bumps", "nests", "throws", "traces"} {
			rounds = append(rounds, name+`(40, `+at+`)`)
		}
	}
	return rounds
}

// calledUnmade are calledSetup's functions whose code makes none of the
// closures it calls, in the warm rounds.
var calledUnmade = []string{"squares", "turns", "bumps", "throws"}

// Each answer is the interpreter's, under every stress setting, and Go
// makes the closures native code left unmade where it leaves inside one.
func TestJITCalledClosures(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := calledRounds()
	r := jitAgreesForTestWith(t, jitStressConfig{}, calledSetup, rounds[:5*6]...)
	for _, name := range calledUnmade {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.ssaCalled == 0 {
			t.Errorf("%s makes the closure it calls: %+v", name, e)
		}
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		r := jitAgreesForTestWith(t, c, calledSetup, rounds...)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
		if r.jit.materialized == 0 {
			t.Errorf("%+v: Go made no closure the code did not make", c)
		}
	}
}

// A closure that is only called is no allocation, nor are the cells it
// captures, which go back to their pool at the call's return (ssa.Func's
// CellsBack); and the collector, run at every call of Go, finds what the
// frames hold.
func TestJITCalledClosuresAllocate(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTestWith(t, jitStressConfig{}, calledSetup, `drive(50)`, `drive(50)`, `drive(50)`)
	jitCallbackMallocs(t, r, `drive(5000)`)
	n := jitCallbackMallocs(t, r, `drive(5000)`)
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
	jitAgreesForTestWith(t, jitStressConfig{threshold: true, budget: 2, deopt: 3}, calledSetup, calledRounds()...)
	if calls == 0 {
		t.Error("no call of Go")
	}
}
