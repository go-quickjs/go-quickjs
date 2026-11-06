//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
)

const osrSetup = `
	function P(v) { this.v = v }
	P.prototype.add = function (o) { return new P(this.v + o.v) }
	var obj = {a: 1}
	// A loop a call leaves native code in at turn k, at a write of a
	// number's property with no entry after it: the way a frame comes to a
	// loop whose entry the function's code does not have.
	function sum(n, k) {
		const one = new P(1)
		let s = new P(0)
		for (let i = 0; i < n; i++) {
			const o = i === k ? 1 : obj
			o.a = 1
			s = s.add(one)
		}
		return s.v
	}
	function twoLoops(n, m, k) {
		const one = new P(1)
		let s = new P(0)
		for (let i = 0; i < n; i++) {
			const o = i === k ? 1 : obj
			o.a = 1
			s = s.add(one)
		}
		let t = new P(0)
		for (let i = 0; i < m; i++) t = t.add(one)
		return s.v + t.v * 3
	}
	function once(n) {
		const one = new P(1)
		let s = new P(0)
		for (let i = 0; i < n; i++) s = s.add(one)
		return s.v
	}
	// Past turn at the operands are no longer what the code was compiled
	// for: its guard fails there, in whichever code runs the loop.
	function shifts(n, at) {
		const one = new P(1)
		let s = new P(0)
		for (let i = 0; i < n; i++) {
			if (i === at) s = { v: s.v + 1, add: P.prototype.add, extra: 1 }
			s = s.add(one)
		}
		return s.v
	}
	function strings(n, at) {
		const one = new P(1)
		let s = new P(0)
		for (let i = 0; i < n; i++) {
			s = s.add(i === at ? new P('x') : one)
		}
		return s.v
	}
`

// osrRounds is the rounds that bring name's frame to code kept for its loop:
// calls enough for its code to give up the entries kept for a frame
// (jitKeptStarts), each leaving at the turn at, and a long one that leaves
// early, the frame then staying in the loop. args are what comes between
// the turns and that.
func osrRounds(name, args string) []string {
	var rounds []string
	for range 2*jitKeptStarts + 2 {
		rounds = append(rounds, name+"(20, "+args+"3)")
	}
	return append(rounds, name+"(200000, "+args+"5)")
}

// osrHeader is the header of the only loop e's code has no entry at.
func osrHeader(t *testing.T, e *jitEntry) int {
	t.Helper()
	if e == nil || e.ssa == nil || len(e.ssaUnentered) != 1 {
		t.Fatalf("no one loop without an entry: %+v", e)
	}
	return int(e.ssaUnentered[0])
}

// A frame that stays in a loop the function's code has no entry at goes on
// in code compiled for it, apart from the function's, which goes on keeping
// the loop's objects unmade for every later invocation: a long first run
// costs the calls after it nothing, as V8's code for on-stack replacement
// does not.
func TestJITOSRCodeApart(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	pc := osrHeader(t, e)
	if e.ssa.HasEntry(pc) {
		t.Fatalf("the function's code took the loop's entry: %+v", e)
	}
	if e.ssaVirtuals == 0 {
		t.Fatalf("the function's code makes the loop's objects")
	}
	oe := e.osr
	if oe == nil || oe.ssa == nil || !oe.ssa.HasEntry(pc) || oe.ssaStats.entries == 0 {
		t.Fatalf("the frame did not go on in code of its own: %+v", oe)
	}
	if oe.osrOf != e || oe.jitLearned != e.jitLearned || oe.hint != 0 {
		t.Fatalf("not the function's: %+v", oe)
	}
	if slices.Contains(e.resumed, uint32(pc)) {
		t.Fatalf("the loop's entry is among what later compiles of the function's code have: %v", e.resumed)
	}
	// The objects unmade again at the calls after it, in the function's code.
	entries, starts := oe.ssaStats.entries, e.starts
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	jitCallbackMallocs(t, r, `sum(1000, -1)`)
	if n := jitCallbackMallocs(t, r, `sum(20000, -1)`); n > 200 {
		t.Errorf("sum(20000), after a long run: %d allocations", n)
	}
	if oe.ssaStats.entries != entries || e.starts == starts {
		t.Errorf("later calls ran %d times in the code kept for the loop, %d in the function's", oe.ssaStats.entries-entries, e.starts-starts)
	}
}

// Native callers call the function's code, however long a frame of it Go
// entered ran in the code kept for its loop: that has a link of its own,
// no native code's.
func TestJITOSRNotCalledNatively(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		function caller(n) {
			let x = 0
			for (let i = 0; i < n; i++) x += sum(i & 7, -1)
			return x
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, append(osrRounds("sum", ""), `caller(20000)`, `caller(20000)`)...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil {
		t.Skipf("the code was compiled again meanwhile: %+v", e)
	}
	if e.osr.link == e.link || r.jit.links[e.link.key] != e.link {
		t.Fatalf("the runtime's link is not the function's")
	}
	if e.link.entry == e.osr.link.entry {
		t.Fatalf("native callers call the code kept for the loop")
	}
	if e.osr.link.in != 0 {
		t.Fatalf("%d native calls of the code kept for the loop", e.osr.link.in)
	}
}

// A compile asked for at a loop's check has a frame waiting at the loop: its
// entry stays, whatever objects that makes, so that the frame goes on in
// the code at once, and a function called once is compiled once. Dropped,
// the frame waited out two more budgets for a second compile, the first
// thrown away: microbench's own loops, each a function called once, paid
// that, and carried_fresh 1.29x what it took with no escape analysis.
func TestJITFrameKeepsItsLoop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, `once(200000)`)
	e := jitCallbackEntry(r, "once")
	if e == nil || e.ssa == nil || r.jit.ssaEntries == 0 {
		t.Fatalf("once never ran natively: %+v", e)
	}
	if r.jit.osrLearned != 0 || e.osr != nil {
		t.Fatalf("a function called once learned %d loops", r.jit.osrLearned)
	}
	if len(e.ssaKept) != 1 || !e.ssa.HasEntry(int(e.ssaKept[0])) || len(e.ssaUnentered) != 0 {
		t.Fatalf("the frame's entry is not one kept: kept %v, unentered %v", e.ssaKept, e.ssaUnentered)
	}
}

// Called again and again, a function gives up the entry kept for the frame
// its first compile was asked for at, its objects unmade for every call
// from then on; and it does at once where a native caller links it, whose
// calls Go does not see.
func TestJITKeptEntryGoes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`once(200000)`}
	for range jitKeptStarts + 1 {
		rounds = append(rounds, `once(50)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, rounds...)
	e := jitCallbackEntry(r, "once")
	if e == nil || len(e.ssaKept) != 0 || len(e.ssaUnentered) != 1 || e.ssaVirtuals == 0 {
		t.Fatalf("called again, once kept its entry: %+v", e)
	}
	setup := osrSetup + `
		function caller(n) {
			let x = 0
			for (let i = 0; i < n; i++) x += once(i & 63)
			return x
		}
	`
	r = jitAgreesForTestWith(t, jitStressConfig{}, setup, `once(200000)`, `caller(200000)`)
	e = jitCallbackEntry(r, "once")
	if e == nil || len(e.ssaKept) != 0 || e.ssaVirtuals == 0 {
		t.Fatalf("called natively, once kept its entry: %+v", e)
	}
}

// Code compiled again only to give up an entry does as it did, and is
// judged by all it did: with its record gone, a cache's calls of Go alone
// were what it was judged by, and code a moment from running natively, a
// tree ran instead.
func TestJITKeptGoneKeepsItsRecord(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, `once(200000)`)
	e := jitCallbackEntry(r, "once")
	if e == nil || len(e.ssaKept) == 0 {
		t.Fatalf("no entry kept: %+v", e)
	}
	work := e.ssaStats.work
	if work == 0 {
		t.Fatal("no work counted")
	}
	cl := r.global.getOwn(r.atoms.intern("once")).value().Object().fn().closure
	r.jitKeptGone(cl, e)
	if len(e.ssaKept) != 0 || e.ssaStats.work != work {
		t.Fatalf("kept %v, work %d of %d", e.ssaKept, e.ssaStats.work, work)
	}
}

// A frame goes on from the loop it entered the code kept for it at to the
// function's next, in that code, which needs no entry there, and keeps that
// loop's objects unmade: only the loop a frame stayed in costs them.
func TestJITOSRNextLoopUnmade(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 2*jitKeptStarts + 2 {
		rounds = append(rounds, `twoLoops(20, 20, 3)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, append(rounds, `twoLoops(200000, 200000, 5)`)...)
	e := jitCallbackEntry(r, "twoLoops")
	if e == nil || e.ssa == nil || len(e.ssaUnentered) != 2 {
		t.Fatalf("not two loops without an entry: %+v", e)
	}
	oe := e.osr
	if oe == nil || oe.ssa == nil || len(oe.osrLoops) != 1 {
		t.Fatalf("the code kept for the loops: %+v", oe)
	}
	if pc := oe.osrLoops[0]; e.ssa.HasEntry(int(pc)) || !oe.ssa.HasEntry(int(pc)) {
		t.Errorf("loop at %d: an entry in the function's code %v, in the code kept for it %v", pc, e.ssa.HasEntry(int(pc)), oe.ssa.HasEntry(int(pc)))
	}
	if len(oe.ssaUnentered) != 1 || oe.ssaVirtuals == 0 {
		t.Errorf("the next loop's objects are made in the code kept for the first: %d unmade, no entry at %v", oe.ssaVirtuals, oe.ssaUnentered)
	}
}

// Go goes on in a run of the code kept for a loop as in any run: past an
// exit with no entry after it, the interpreter runs to the loop's header,
// where the run goes on natively, in that code. The function's code has no
// entry there: asked, the frame would begin a run within the interpreter
// at each exit, one inside the other.
func TestJITOSRGoesOnInItsRun(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		var obj = {a: 1}
		function exits(n, every) {
			const one = new P(1)
			let s = new P(0)
			for (let i = 0; i < n; i++) {
				const o = i % every === 0 ? 1 : obj
				o.a = 1
				s = s.add(one)
			}
			return s.v
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, `exits(20, 3)`, `exits(20, 3)`, `exits(20, 3)`, `exits(20, 3)`, `exits(300000, 100)`)
	e := jitCallbackEntry(r, "exits")
	if e == nil || e.osr == nil || e.osr.ssa == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	// The site known by now, the run leaves at each of them, and goes on.
	if oe := e.osr; r.jit.wentOn < 1000 || oe.ssaStats.entries < 1000 {
		t.Errorf("went on natively %d times, entered the code kept for the loop %d times", r.jit.wentOn, oe.ssaStats.entries)
	}
}

// The code kept for a loop deoptimizes as any code does, and every answer
// is the interpreter's; what it learned reaches the function's code, whose
// next compile is for it.
func TestJITOSRDeoptimizes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}, {threshold: true, budget: 1, deopt: 2}} {
		r := jitAgreesForTestWith(t, c, osrSetup,
			`shifts(20, -1)`, `shifts(20, -1)`, `shifts(150000, 140000)`, `shifts(20, -1)`, `shifts(20, 7)`, `shifts(150000, 140000)`,
			`strings(20, -1)`, `strings(20, -1)`, `strings(150000, 140000)`, `strings(20, 7)`, `strings(150000, -1)`)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
		for _, name := range []string{"shifts", "strings"} {
			if e := jitCallbackEntry(r, name); e == nil {
				t.Errorf("%s has no entry", name)
			}
		}
	}
}

// A loop every call leaves native code in early, with no entry after the
// exit, too short for a frame to spend a budget in: the frames learn the
// loop all the same, each one going on in the code kept for it, not in the
// interpreter to the loop's end -- and learn it again once the function's
// code, compiled again, has let that go. A budget was never spent there,
// each exit having the next check due at once: every call interpreted the
// rest of its loop, three times as slow as with an entry.
func TestJITOSRShortLoopsLearn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		var obj = {a: 1}
		function early(n, k) {
			const one = new P(1)
			let s = new P(0)
			for (let i = 0; i < n; i++) {
				const o = i === k ? 1 : obj
				o.a = 1
				s = s.add(one)
			}
			return s.v
		}
		function calls(c) {
			let t = 0
			for (let j = 0; j < c; j++) t += early(100, j % 2 === 0 ? 5 : -1)
			return t
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, `calls(6000)`, `calls(6000)`)
	e := jitCallbackEntry(r, "early")
	if e == nil || e.ssa == nil {
		t.Fatalf("early was not compiled: %+v", e)
	}
	if e.ssaVirtuals == 0 || len(e.ssaUnentered) == 0 {
		t.Fatalf("the function's code does not keep the loop's objects unmade: %+v", e)
	}
	oe := e.osr
	if oe == nil || oe.ssa == nil {
		t.Fatalf("no code kept for the loop, after %d frames left native code: %+v", e.goOns, e)
	}
	// Whatever went before the code settled, the frames go on natively.
	if oe.ssaStats.entries < 2000 {
		t.Errorf("%d of the frames that left the function's code went on in the code kept for the loop, of %d", oe.ssaStats.entries, e.goOns)
	}
}

// Where three calls in four leave a loop, what its objects' staying unmade
// gains the rest does not pay what each of those costs: the function's code
// takes the loop's entry, its objects made, the code kept goes with what it
// took, and an exit no longer looks for objects none of its code leaves
// unmade. Where one call in two leaves (TestJITOSRShortLoopsLearn), it does
// not, whatever the calls' turns or loops: a call that leaves again goes on
// in that run.
func TestJITFramesLeave(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		function leaves(n, k) {
			const one = new P(1)
			let s = new P(0)
			for (let i = 0; i < n; i++) {
				const o = i === k || i % 2 === k ? 1 : obj
				o.a = 1
				s = s.add(one)
			}
			return s.v
		}
		function calls(c, k) {
			let t = 0
			for (let j = 0; j < c; j++) t += leaves(100, k)
			return t
		}
		function several(n, k) {
			const one = new P(1)
			let s = new P(0), t = new P(0), u = new P(0), v = new P(0)
			for (let i = 0; i < n; i++) { const o = i === k ? 1 : obj; o.a = 1; s = s.add(one) }
			for (let i = 0; i < n; i++) { const o = i === k ? 1 : obj; o.a = 1; t = t.add(one) }
			for (let i = 0; i < n; i++) { const o = i === k ? 1 : obj; o.a = 1; u = u.add(one) }
			for (let i = 0; i < n; i++) { const o = i === k ? 1 : obj; o.a = 1; v = v.add(one) }
			return s.v + t.v + u.v + v.v
		}
		function some(c) {
			let t = 0
			for (let j = 0; j < c; j++) t += several(100, j % 2 === 0 ? 5 : -1)
			return t
		}
	`
	for _, c := range []struct{ name, args string }{{"at one turn", "5"}, {"at every other turn", "1"}} {
		t.Run(c.name, func(t *testing.T) {
			run := "calls(3000, " + c.args + ")"
			r := jitAgreesForTestWith(t, jitStressConfig{}, setup, run, run)
			e := jitCallbackEntry(r, "leaves")
			if e == nil || e.ssa == nil {
				t.Fatalf("leaves was not compiled: %+v", e)
			}
			if len(e.ssaUnentered) != 0 || e.osr != nil || e.ssaVirtuals != 0 {
				t.Fatalf("every call left and the code goes on leaving its objects unmade: unentered %v, osr %v, virtuals %d", e.ssaUnentered, e.osr != nil, e.ssaVirtuals)
			}
			if e.ssaHomes != 0 {
				t.Fatalf("code with no object unmade has exits look for them: %d homes", e.ssaHomes)
			}
		})
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, `some(6000)`, `some(6000)`)
	e := jitCallbackEntry(r, "several")
	if e == nil || e.ssaVirtuals == 0 || len(e.ssaUnentered) != 4 || e.osr == nil {
		t.Fatalf("calls that run their loops through gave up what they gain: %+v", e)
	}
}

// An exit looks for objects not made in the homes of the code that may still
// exit: the new code's once no run of the code it replaced is left, however
// that ends.
func TestJITHomesComeDown(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, `once(200000)`)
	e, s := jitCallbackEntry(r, "once"), r.jit
	if e == nil {
		t.Fatal("once was not compiled")
	}
	e.setHomes(s, 40)
	if e.ssaHomes != 40 {
		t.Fatalf("no run of the code: its homes %d, not 40", e.ssaHomes)
	}
	e.ssaRuns++
	s.runEntries = append(s.runEntries, e)
	e.setHomes(s, 0)
	if e.ssaHomes != 40 {
		t.Fatalf("a run of the code it replaced lost its homes: %d", e.ssaHomes)
	}
	r.jitRunDone(e, 0, s.ctxTop)
	if e.ssaHomes != 0 {
		t.Fatalf("its last run ended, and it holds %d homes", e.ssaHomes)
	}
}

// A compile asked for in a run, for what the code learned at its exits, has
// a frame in a function hardly called yet: the frame goes on in the code it
// asks for, at the loop's entry the compile keeps, with no code of its own.
func TestJITCompileInARunKeepsItsLoop(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function run(n) {
			var i, j, s, sum = 0
			var sink = { push: function (x) { return x !== null && (s += x), true } }
			for (j = 0; j < n; j += 1024) {
				s = ""
				for (i = 0; i < 1024; i++) sink.push("<x>")
				sum += s.length + s.lastIndexOf("<x>")
			}
			return j + sum
		}
	`
	run := `run(40000000)`
	if !allocsOrdinary() {
		// Under checkptr, where it took 40 s of CI's native boundary.
		run = `run(4000000)`
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, run)
	e := jitCallbackEntry(r, "run")
	if e == nil || e.ssa == nil || r.jit.ssaEntries == 0 {
		t.Fatalf("run never ran natively: %+v", e)
	}
	if r.jit.osrLearned != 0 || e.osr != nil {
		t.Fatalf("a frame in a function called once asked for %d loops' code of its own", r.jit.osrLearned)
	}
}

// Compiled in the background, the code kept for a loop is built from a
// snapshot of what the function learned, which is asked for the loops'
// entries like every other: not asked, it has none, and the frame stays in
// the existing tiers.
func TestJITOSRInTheBackground(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		function carried(n, k) {
			let c = new P(0)
			for (let i = 0; i < n; i++) {
				const o = i === k ? 1 : obj
				o.a = 1
				c = new P(c.v + 1)
			}
			return c.v
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2}, setup, append(osrRounds("carried", ""), osrRounds("carried", "")...)...)
	e := jitCallbackEntry(r, "carried")
	pc := osrHeader(t, e)
	if e.ssa.HasEntry(pc) {
		t.Fatalf("the function's code took the loop's entry")
	}
	oe := e.osr
	if oe == nil || oe.ssa == nil || !oe.ssa.HasEntry(pc) || oe.ssaStats.entries == 0 {
		t.Fatalf("the frame did not go on in code of its own: %+v", oe)
	}
}

// A shape one code of a function missed, the other, compiled no later,
// missing it too is not a code compiled again for it that cannot use it
// (jitDeoptSeen): the site, of two shapes, stays polymorphic. Each code's
// serial its own, its second miss made it megamorphic.
func TestJITOSRSiteNotMegamorphic(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := osrSetup + `
		function A() { this.k = 1 }
		A.prototype.get = function () { return this.k }
		function B() { this.pad = 0; this.k = 2 }
		B.prototype.get = function () { return this.k }
		var a = new A(), b = new B()
		function run(o, n) {
			let s = new P(0)
			for (let i = 0; i < n; i++) s = new P(s.v + o.get())
			return s.v
		}
	`
	r := jitAgreesForTestWith(t, jitStressConfig{}, setup, `run(a, 200000)`, `run(b, 200000)`)
	e := jitCallbackEntry(r, "run")
	if e == nil || e.ssa == nil {
		t.Fatalf("run was not compiled: %+v", e)
	}
	for i := range e.poly {
		if e.poly[i].mega {
			t.Errorf("a site of two shapes is megamorphic: %+v", e.poly[i])
		}
	}
}

// Code kept for a loop compiled again -- for a call Go went on after, at
// once, or for what it learned, in a run -- tells the function's code first,
// and its learning the loop's entry does not: that would have the code it
// was compiled apart from compiled again, which lets it go.
func TestJITOSRTellsBeforeItsCompile(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil || e.osr.ssa == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	if e.reoptPending() {
		t.Fatalf("learning the loop's entry has the function's code compiled again: %+v", e)
	}
	oe := e.osr
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	oe.resumeReopt = true
	r.jitReoptimize(cl, oe)
	if oe.resumeReopt || !e.resumeReopt {
		t.Fatalf("not told before its compile: told %v, still set %v", e.resumeReopt, oe.resumeReopt)
	}
}

// A long frame in the code kept for its loop uses the function: its
// entries there count, and the cache does not flush it as unused.
func TestJITOSRUsesTheFunction(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	u := e.uses()
	e.osr.ssaStats.entries++
	if e.uses() != u+1 {
		t.Fatalf("an entry of the code kept for the loop is no use of the function: %d, %d", u, e.uses())
	}
}

// The code kept for a loop starts from what its function's compiles have
// spent: told its reasons, the function's code gets no more compiles than
// its budgets give it.
func TestJITOSRSpendsTheFunctionsBudgets(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, `sum(20, 3)`, `sum(20, 3)`)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.ssa == nil {
		t.Skipf("sum was not compiled: %+v", e)
	}
	e.inlineReopts, e.upgradeReopts, e.polyReopts = 2, 3, 4
	oe := r.jit.osrEntry(e)
	if oe.inlineReopts != 2 || oe.upgradeReopts != 3 || oe.polyReopts != 4 {
		t.Fatalf("budgets afresh: %+v", oe)
	}
	r.jit.dropOSR(e)
}

// What the code kept for a loop learned at its exits, the function's code
// is told, which would otherwise find the site known already, and be
// compiled for it at no exit of its own.
func TestJITOSRTellsTheFunction(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	oe := e.osr
	oe.polyReopt = true
	r.jit.osrRunDone(oe)
	if !e.polyReopt || !e.reoptPending() {
		t.Fatalf("the function's entry was not told: %+v", e)
	}
	oe.polyReopt = false
}

// The function's code compiled again, the code kept for its loops goes: it
// is out of date, and holds a compile's bytes for frames that may not come
// back. A frame that stays in a loop then learns it afresh. Not while a
// run is in it, which goes on there and lets it go when it ends.
func TestJITOSRGoesWithTheCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	oe := e.osr
	cl := r.global.getOwn(r.atoms.intern("sum")).value().Object().fn().closure
	before := e.measure()

	// A run in it.
	oe.ssaRuns++
	e.inlineReopt = true
	r.jitReoptimize(cl, e)
	if e.osr != oe || !oe.osrSpent {
		t.Fatalf("let go under a run: %+v", e)
	}
	oe.ssaRuns--
	r.jit.osrRunDone(oe)
	if e.osr != nil {
		t.Fatalf("kept once its run ended: %+v", e)
	}
	if oe.owned() {
		t.Fatalf("still its link's")
	}
	if e.measure() >= before {
		t.Errorf("the entry holds %d bytes for %d", e.measure(), before)
	}
	r.jit.account(e)

	// A long call now begins in the function's code, and needs none.
	v, err := r.Run(compileForTest(t, `sum(200000, -1)`))
	if err != nil || v.Number() != 200000 {
		t.Fatalf("got %v, %v", v, err)
	}
	if e.osr != nil {
		t.Fatalf("a call begun in the function's code had code kept for its loop: %+v", e.osr)
	}
}

// The code kept for a loop counts in its function's entry, and goes with
// it: the cache holds what it counts, no less, and nothing of it past it.
func TestJITOSRCounted(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{}, osrSetup, osrRounds("sum", "")...)
	e := jitCallbackEntry(r, "sum")
	if e == nil || e.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", e)
	}
	oe := e.osr
	if n := e.osrBytes(); n < oe.ssa.Footprint() {
		t.Fatalf("%d bytes counted for %d of code", n, oe.ssa.Footprint())
	}
	if e.bytes != e.measure() {
		t.Fatalf("the cache counts %d bytes of %d", e.bytes, e.measure())
	}
	if !r.jit.inUse(e) {
		oe.busy++
		if !r.jit.inUse(e) {
			t.Errorf("an entry whose code for its loop is being compiled is not in use")
		}
		oe.busy--
		oe.ssaRuns++
		if !r.jit.inUse(e) {
			t.Errorf("an entry whose code for its loop runs is not in use")
		}
		oe.ssaRuns--
	}
	held := r.jit.entryBytes
	key := r.jit.linkKeyForTest(e)
	if !r.jit.dropEntry(key, e) {
		t.Fatal("the entry was not dropped")
	}
	if e.osr != nil || oe.owned() {
		t.Fatalf("the code kept for the loop outlived its function's: %+v", oe)
	}
	if r.jit.entryBytes >= held {
		t.Errorf("the cache holds %d bytes, %d before", r.jit.entryBytes, held)
	}
	// Compiled afresh, and right.
	for range 3 {
		if v, err := r.Run(compileForTest(t, `sum(100000, -1)`)); err != nil || v.Number() != 100000 {
			t.Fatalf("got %v, %v", v, err)
		}
	}
}

// linkKeyForTest is the cache's key of e.
func (s *jitState) linkKeyForTest(e *jitEntry) (key jitKeyForTest) {
	for k, x := range s.cache {
		if x == e {
			return k
		}
	}
	return key
}

// jitKeyForTest is the cache's key.
type jitKeyForTest = jitKey
