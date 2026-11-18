//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const osrSetup = `
	function P(v) { this.v = v }
	P.prototype.add = function (o) { return new P(this.v + o.v) }
	var obj = {a: 1}
	// A loop a call leaves native code in at turn k, at a write of a
	// number's property with no entry after it (jitStressConfig's
	// primLeave): the way a frame comes to a loop whose entry the
	// function's code does not have.
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
// (jitKeptStarts): a long one its code is compiled in, short ones, one in
// two leaving, which teaches the code that path and no more leave than may
// (jitRegKeptCallsLeave), then a long one that leaves early, the frame
// staying in the loop. args are what comes between the turns and that.
func osrRounds(name, args string) []string {
	rounds := []string{name + "(200000, " + args + "-1)"}
	for i := range 2*jitKeptStarts + 2 {
		rounds = append(rounds, name+"(20, "+args+map[bool]string{true: "3", false: "-1"}[i%2 == 0]+")")
	}
	return append(rounds, name+"(200000, "+args+"5)")
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
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	life, re := jitNamedForTest(r, "sum")
	pc := osrRegHeader(t, re)
	// Its objects unmade there: the loop's entry dropped (unentered),
	// register code's escape analysis counting none (ssaVirtuals).
	if re.isLoop(pc) {
		t.Fatalf("the function's code took the loop's entry: %+v", re)
	}
	// The code kept for the loop, which the frame went on in -- the
	// only code jitRegLoopCode compiles for a frame waiting at a loop
	// (osrLearned) -- of the function's state: what it does is the
	// function's (its life), which counts no entry of its own apart.
	oe := re.osr
	if oe == nil || oe.code == nil || !oe.isLoop(pc) || r.jit.osrLearned == 0 {
		t.Fatalf("the frame did not go on in code of its own: %+v", oe)
	}
	if oe.osrOf != re || oe.life != life {
		t.Fatalf("not the function's: %+v", oe)
	}
	if slices.Contains(life.resumed, uint32(pc)) {
		t.Fatalf("the loop's entry is among what later compiles of the function's code have: %v", life.resumed)
	}
	// The objects unmade again at the calls after it, in the function's
	// code, which Go enters.
	starts, learned := life.starts, r.jit.osrLearned
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	jitCallbackMallocs(t, r, `sum(1000, -1)`)
	if n := jitCallbackMallocs(t, r, `sum(20000, -1)`); n > 200 {
		t.Errorf("sum(20000), after a long run: %d allocations", n)
	}
	if life.starts == starts || r.jit.osrLearned != learned || re.osr != oe {
		t.Errorf("later calls entered the function's code %d times, compiled %d codes for the loop", life.starts-starts, r.jit.osrLearned-learned)
	}
}

// Native callers call the function's code, however long a frame of it Go
// entered ran in the code kept for its loop: that has a link of its own,
// no native code's.
func TestJITOSRNotCalledNatively(t *testing.T) {
	t.Parallel()
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
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, append(osrRounds("sum", ""), `caller(20000)`, `caller(20000)`)...)
	_, e := jitNamedForTest(r, "sum")
	if e == nil || e.osr == nil {
		t.Skipf("the code was compiled again meanwhile: %+v", e)
	}
	// The function's link, by its key (regLinkFor), calls the function's
	// code; the code kept for the loop has no link, and no link calls it.
	l := r.jit.regLinkFor(e)
	if l == nil || r.jit.regLinks[e.key] != l {
		t.Fatalf("the runtime's link is not the function's")
	}
	if l.entry != 0 && !e.code.Contains(l.entry) {
		t.Fatalf("native callers call code not the function's")
	}
	for k, x := range r.jit.regLinks {
		if x.entry != 0 && e.osr.code.Contains(x.entry) {
			t.Fatalf("%v's link calls the code kept for the loop", k.Value().Name)
		}
	}
}

// A compile asked for at a loop's check has a frame waiting at the loop: its
// entry stays, whatever objects that makes, so that the frame goes on in
// the code at once, and a function called once is compiled once. Dropped,
// the frame waited out two more budgets for a second compile, the first
// thrown away: microbench's own loops, each a function called once, paid
// that, and carried_fresh 1.29x what it took with no escape analysis.
func TestJITFrameKeepsItsLoop(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, `once(200000)`)
	_, re := jitNamedForTest(r, "once")
	if re == nil || re.code == nil || r.jit.ssaEntries == 0 {
		t.Fatalf("once never ran natively: %+v", re)
	}
	if r.jit.osrLearned != 0 || re.osr != nil {
		t.Fatalf("a function called once learned %d loops", r.jit.osrLearned)
	}
	if len(re.kept) != 1 || !re.isLoop(re.kept[0]) || len(re.unentered) != 0 {
		t.Fatalf("the frame's entry is not one kept: kept %v, unentered %v", re.kept, re.unentered)
	}
}

// Called again and again, a function gives up the entry kept for the frame
// its first compile was asked for at, its objects unmade for every call
// from then on; and it does at once where a native caller links it, whose
// calls Go does not see.
func TestJITKeptEntryGoes(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`once(200000)`}
	for range jitKeptStarts + 1 {
		rounds = append(rounds, `once(50)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, rounds...)
	// Its objects unmade: the loop's entry dropped (unentered).
	if _, re := jitNamedForTest(r, "once"); re == nil || re.code == nil || len(re.kept) != 0 || len(re.unentered) != 1 {
		t.Fatalf("called again, once kept its entry: %+v", re)
	}
	setup := osrSetup + `
		function caller(n) {
			let x = 0
			for (let i = 0; i < n; i++) x += once(i & 63)
			return x
		}
	`
	r = jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, `once(200000)`, `caller(200000)`)
	if _, re := jitNamedForTest(r, "once"); re == nil || re.code == nil || len(re.kept) != 0 || len(re.unentered) == 0 {
		t.Fatalf("called natively, once kept its entry: %+v", re)
	}
}

// Calls that leave the loop anyway do not give up the entry kept for a
// frame: they gain nothing from its objects' staying unmade, and would ask
// for it back. carried_exit_3000 made two compiles more than main for that.
func TestJITKeptEntryStaysForCallsThatLeave(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`sum(200000, -1)`}
	for range 6 * jitKeptStarts {
		rounds = append(rounds, `sum(100, 5)`)
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, rounds...)
	_, re := jitNamedForTest(r, "sum")
	if re == nil || re.code == nil {
		t.Fatalf("sum was not compiled: %+v", re)
	}
	if len(re.kept) == 0 || re.osr != nil || r.jit.osrLearned != 0 {
		t.Fatalf("calls that left gave up the entry: kept %v, code apart %v, %d learned", re.kept, re.osr != nil, r.jit.osrLearned)
	}
	// And one in two leaving does.
	rounds = []string{`sum(200000, -1)`}
	for i := range 6 * jitKeptStarts {
		rounds = append(rounds, map[bool]string{true: `sum(100, 5)`, false: `sum(100, -1)`}[i%2 == 0])
	}
	r = jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, rounds...)
	// Its objects unmade: the loop's entry dropped (unentered).
	if _, re := jitNamedForTest(r, "sum"); re == nil || re.code == nil || len(re.kept) != 0 || len(re.unentered) == 0 {
		t.Fatalf("calls that ran their loop through kept the entry: %+v", re)
	}
}

// Code compiled again only to give up an entry does as it did, and is
// judged by all it did: with its record gone, a cache's calls of Go alone
// were what it was judged by, and code a moment from running natively, a
// tree ran instead.
func TestJITKeptGoneKeepsItsRecord(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, `once(200000)`)
	life, re := jitNamedForTest(r, "once")
	if re == nil || re.code == nil || len(re.kept) == 0 {
		t.Fatalf("no entry kept: %+v", re)
	}
	// Judged by all it did: what its runs did kept, and its judging
	// not begun afresh (jitRegCached's keptGone).
	st := &life.ssaStats
	work, base, judged := st.work, st.workBase, st.judged
	if work == 0 {
		t.Fatal("no work counted")
	}
	cl := jitGlobalClosure(r, "once")
	ne := r.jitRegKeptGone(cl, r.regOf(cl), re)
	if ne == nil || ne.code == nil || len(ne.kept) != 0 || st.work != work || st.workBase != base || st.judged != judged {
		t.Fatalf("kept %v, work %d of %d, judged from %d of %d", ne, st.work, work, st.workBase, base)
	}
}

// A frame goes on from the loop it entered the code kept for it at to the
// function's next, in that code, which needs no entry there, and keeps that
// loop's objects unmade: only the loop a frame stayed in costs them.
func TestJITOSRNextLoopUnmade(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`twoLoops(200000, 200000, -1)`}
	for i := range 2*jitKeptStarts + 2 {
		rounds = append(rounds, map[bool]string{true: `twoLoops(20, 20, 3)`, false: `twoLoops(20, 20, -1)`}[i%2 == 0])
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, append(rounds, `twoLoops(200000, 200000, 5)`)...)
	_, re := jitNamedForTest(r, "twoLoops")
	if re == nil || re.code == nil || len(re.unentered) != 2 {
		t.Fatalf("not two loops without an entry: %+v", re)
	}
	oe := re.osr
	if oe == nil || oe.code == nil {
		t.Fatalf("the code kept for the loops: %+v", oe)
	}
	// The loop it was compiled for: the one of the function's code's
	// without an entry that it has one at.
	var loops []int
	for _, pc := range re.unentered {
		if oe.isLoop(pc) {
			loops = append(loops, pc)
		}
	}
	if len(loops) != 1 {
		t.Fatalf("the code kept for the loops has entries at %v of %v", loops, re.unentered)
	}
	if pc := loops[0]; re.isLoop(pc) || !oe.isLoop(pc) {
		t.Errorf("loop at %d: an entry in the function's code %v, in the code kept for it %v", pc, re.isLoop(pc), oe.isLoop(pc))
	}
	// The next loop's objects unmade: its entry dropped there too.
	if len(oe.unentered) != 1 {
		t.Errorf("the next loop's objects are made in the code kept for the first: no entry at %v", oe.unentered)
	}
}

// Go goes on in a run of the code kept for a loop as in any run: past an
// exit with no entry after it, the interpreter runs to the loop's header,
// where the run goes on natively, in that code. The function's code has no
// entry there: asked, the frame would begin a run within the interpreter
// at each exit, one inside the other.
func TestJITOSRGoesOnInItsRun(t *testing.T) {
	t.Parallel()
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
	rounds := []string{`exits(20, 3)`, `exits(20, 3)`, `exits(20, 3)`, `exits(20, 3)`, `exits(300000, 100)`}
	// Register code counts a call at its return (regTickRet): a first
	// call with no turn has the function compiled at its second call's
	// start, as the stack form's is at its first, not at its loop for
	// the frame waiting there, whose entry the code would keep
	// (jitRegFrameAt).
	rounds = append([]string{`exits(0, 3)`}, rounds...)
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, rounds...)
	_, re := jitNamedForTest(r, "exits")
	if re == nil || re.osr == nil || re.osr.code == nil {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	// The site known by now, the run leaves at each of them, and goes
	// on, in the code kept for the loop, the function's having no
	// entry there: register code counts no entries of its own apart
	// from its function's.
	if r.jit.wentOn < 1000 || re.isLoop(re.unentered[0]) {
		t.Errorf("went on natively %d times; an entry at the loop in the function's code %v", r.jit.wentOn, re.isLoop(re.unentered[0]))
	}
}

// The code kept for a loop deoptimizes as any code does, and every answer
// is the interpreter's; what it learned reaches the function's code, whose
// next compile is for it.
func TestJITOSRDeoptimizes(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []jitStressConfig{{primLeave: true}, {threshold: true, budget: 3, primLeave: true}, {threshold: true, budget: 3, deopt: 7, primLeave: true}, {threshold: true, budget: 1, deopt: 2, primLeave: true}} {
		r := jitAgreesForTestWith(t, c, osrSetup,
			`shifts(20, -1)`, `shifts(20, -1)`, `shifts(150000, 140000)`, `shifts(20, -1)`, `shifts(20, 7)`, `shifts(150000, 140000)`,
			`strings(20, -1)`, `strings(20, -1)`, `strings(150000, 140000)`, `strings(20, 7)`, `strings(150000, -1)`)
		t.Logf("%+v: %d materialized", c, r.jit.materialized)
		for _, name := range []string{"shifts", "strings"} {
			if e, _ := jitNamedForTest(r, name); e == nil {
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
	t.Parallel()
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
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, `calls(6000)`, `calls(6000)`)
	life, re := jitNamedForTest(r, "early")
	if re == nil || re.code == nil {
		t.Fatalf("early was not compiled: %+v", re)
	}
	// Its objects unmade: the loop's entry dropped (unentered).
	if len(re.unentered) == 0 {
		t.Fatalf("the function's code does not keep the loop's objects unmade: %+v", re)
	}
	oe := re.osr
	if oe == nil || oe.code == nil {
		t.Fatalf("no code kept for the loop, after %d frames left native code: %+v", life.goOns, re)
	}
	// Whatever went before the code settled, the frames go on natively,
	// at the loop's next back edge (wentOn).
	if r.jit.wentOn < 2000 {
		t.Errorf("%d of the frames that left the function's code went on in the code kept for the loop, of %d", r.jit.wentOn, life.goOns)
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
	t.Parallel()
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
			r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, run, run)
			_, re := jitNamedForTest(r, "leaves")
			if re == nil || re.code == nil {
				t.Fatalf("leaves was not compiled: %+v", re)
			}
			// No object unmade at the loop: its entry kept (unentered),
			// no code kept for it; and no homes past its frame for them
			// (jitRegEntry's frame, its ssa.Func's HomesEnd).
			if len(re.unentered) != 0 || re.osr != nil || !re.isLoop(regLoopHeaderForTest(r, "leaves")) {
				t.Fatalf("every call left and the code goes on leaving its objects unmade: unentered %v, osr %v, loops %v", re.unentered, re.osr != nil, re.loops)
			}
			if re.frame != 0 {
				t.Fatalf("code with no object unmade has exits look for them: %d slots of homes", re.frame)
			}
		})
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, `some(6000)`, `some(6000)`)
	// Its objects unmade: the loops' entries dropped (unentered).
	if _, re := jitNamedForTest(r, "several"); re == nil || re.code == nil || len(re.unentered) != 4 || re.osr == nil {
		t.Fatalf("calls that run their loops through gave up what they gain: %+v", re)
	}
}

// A compile asked for in a run, for what the code learned at its exits, has
// a frame in a function hardly called yet: the frame goes on in the code it
// asks for, at the loop's entry the compile keeps, with no code of its own.
func TestJITCompileInARunKeepsItsLoop(t *testing.T) {
	// Asked before the test runs in parallel, which AllocsPerRun may not.
	ordinary := allocsOrdinary()
	t.Parallel()
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
	if !ordinary {
		// Under checkptr, where it took 40 s of CI's native boundary.
		run = `run(4000000)`
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, run)
	_, re := jitNamedForTest(r, "run")
	if re == nil || re.code == nil || r.jit.ssaEntries == 0 {
		t.Fatalf("run never ran natively: %+v", re)
	}
	if r.jit.osrLearned != 0 || re.osr != nil {
		t.Fatalf("a frame in a function called once asked for %d loops' code of its own", r.jit.osrLearned)
	}
}

// Compiled in the background, the code kept for a loop is built from a
// snapshot of what the function learned, which is asked for the loops'
// entries like every other: not asked, it has none, and the frame stays in
// the existing tiers.
func TestJITOSRInTheBackground(t *testing.T) {
	t.Parallel()
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
	// Register code compiles the code kept for a loop in the
	// background too (jitRegCompile's osrAt): the frame waiting at the
	// loop goes on in the interpreter until it is installed, at the
	// second check after it was handed over, and in it from then on.
	// The first long call that leaves is the frame (osrRounds' last).
	rounds := osrRounds("carried", "")
	r := jitAgreesForTestWith(t, jitStressConfig{background: true, checks: 2, primLeave: true}, setup, rounds[:len(rounds)-1]...)
	_, re := jitNamedForTest(r, "carried")
	pc := osrRegHeader(t, re)
	if re.isLoop(pc) {
		t.Fatalf("the function's code took the loop's entry")
	}
	if re.osr != nil || r.jit.osrLearned != 0 {
		t.Fatalf("code for the loop before a frame waited there: %+v", re.osr)
	}
	submitted, entries := r.jit.bgSubmitted, r.JITStats().SSAEntries
	jitCallForTest(t, r, rounds[len(rounds)-1], 200000)
	oe := re.osr
	if oe == nil || oe.code == nil || !oe.isLoop(pc) || r.jit.osrLearned != 1 || re.osrJob != nil {
		t.Fatalf("the frame did not go on in code of its own: %+v, %d learned, pending %v", oe, r.jit.osrLearned, re.osrJob != nil)
	}
	if r.jit.bgSubmitted != submitted+1 || r.jit.bgDiscarded != 0 || r.JITStats().SSAEntries == entries {
		t.Fatalf("submitted %d, discarded %d, entered %d: want the loop's code compiled in the background and entered",
			r.jit.bgSubmitted-submitted, r.jit.bgDiscarded, r.JITStats().SSAEntries-entries)
	}
	for _, src := range rounds {
		n, _, _ := strings.Cut(strings.TrimPrefix(src, "carried("), ",")
		want, _ := strconv.Atoi(n)
		jitCallForTest(t, r, src, float64(want))
	}
	if re.osr != oe || r.jit.osrLearned != 1 {
		t.Fatalf("the loop's code compiled again: %+v, %d learned", re.osr, r.jit.osrLearned)
	}

}

// A shape one code of a function missed, the other, compiled no later,
// missing it too is not a code compiled again for it that cannot use it
// (jitRegDeoptSeen): the site, of two shapes, stays polymorphic. Each code's
// serial its own, its second miss made it megamorphic.
func TestJITOSRSiteNotMegamorphic(t *testing.T) {
	t.Parallel()
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
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, setup, `run(a, 200000)`, `run(b, 200000)`)
	e, re := jitNamedForTest(r, "run")
	if re == nil || re.code == nil {
		t.Fatalf("run was not compiled: %+v", re)
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
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	life, re := jitNamedForTest(r, "sum")
	if re == nil || re.osr == nil || re.osr.code == nil {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	if life.reoptPending() {
		t.Fatalf("learning the loop's entry has the function's code compiled again: %+v", life)
	}
	// What the code kept for the loop learns is its function's state
	// (its life, the same): nothing to tell, before its compile or
	// after, as the stack form's code apart tells its function's entry.
	if re.osr.life != life {
		t.Fatalf("the code kept for the loop learns apart from its function")
	}
}

// A long frame in the code kept for its loop uses the function: its
// entries there count, and the cache does not flush it as unused.
func TestJITOSRUsesTheFunction(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	// Register code kept for a loop counts its entries in its
	// function's state (its life), which the cache's uses of the
	// function's code count (regUses).
	life, re := jitNamedForTest(r, "sum")
	if re == nil || re.osr == nil || re.osr.life != life {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	u := r.jit.regUses(re)
	re.osr.life.ssaStats.entries++
	if r.jit.regUses(re) != u+1 {
		t.Fatalf("an entry of the code kept for the loop is no use of the function: %d, %d", u, r.jit.regUses(re))
	}
}

// What the code kept for a loop learned at its exits, the function's code
// is told, which would otherwise find the site known already, and be
// compiled for it at no exit of its own.
func TestJITOSRTellsTheFunction(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	// What register code kept for a loop learns at its exits is its
	// function's state (its life, the same), whose code is compiled
	// for it.
	life, re := jitNamedForTest(r, "sum")
	if re == nil || re.osr == nil || re.osr.life != life {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	re.osr.life.polyReopt = true
	if !life.polyReopt || !life.reoptPending() {
		t.Fatalf("the function's state was not told: %+v", life)
	}
	life.polyReopt = false
}

// The function's code compiled again, the code kept for its loops goes: it
// is out of date, and holds a compile's bytes for frames that may not come
// back. A frame that stays in a loop then learns it afresh. Not while a
// run is in it, which goes on there and lets it go when it ends.
func TestJITOSRGoesWithTheCode(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	_, re := jitNamedForTest(r, "sum")
	if re == nil || re.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	oe, s := re.osr, r.jit
	cl := jitGlobalClosure(r, "sum")
	held := s.regHeld

	// A run in it.
	oe.runs++
	ne := r.jitRegReoptimize(cl, r.regOf(cl), re, -1)
	if ne == nil || ne == re || re.osr != nil || !oe.dropped || oe.code == nil {
		t.Fatalf("compiled again: %v, kept for the loop %v, dropped %v, closed under its run %v", ne != nil, re.osr != nil, oe.dropped, oe.code == nil)
	}
	if want := held - re.bytes - oe.bytes + ne.bytes; s.regHeld != want {
		t.Errorf("the cache holds %d bytes, %d of them its code's", s.regHeld, want)
	}
	oe.runs--
	s.jitRegCloseRetired()
	if oe.code != nil {
		t.Fatalf("kept once its run ended: %+v", oe)
	}

	// A long call now begins in the function's code, and needs none.
	v, err := r.Run(compileForTest(t, `sum(200000, -1)`))
	if err != nil || v.Number() != 200000 {
		t.Fatalf("got %v, %v", v, err)
	}
	if _, re = jitNamedForTest(r, "sum"); re.osr != nil {
		t.Fatalf("a call begun in the function's code had code kept for its loop: %+v", re.osr)
	}
}

// The code kept for a loop counts in its function's entry, and goes with
// it: the cache holds what it counts, no less, and nothing of it past it.
func TestJITOSRCounted(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTestWith(t, jitStressConfig{primLeave: true}, osrSetup, osrRounds("sum", "")...)
	_, re := jitNamedForTest(r, "sum")
	if re == nil || re.osr == nil {
		t.Fatalf("no code kept for the loop: %+v", re)
	}
	oe, s := re.osr, r.jit
	// Counted in what the cache holds (regHeld), no less than its code.
	if oe.bytes < oe.code.Footprint() {
		t.Fatalf("%d bytes counted for %d of code", oe.bytes, oe.code.Footprint())
	}
	// With what each function's state holds (regAccount).
	held := 0
	for _, x := range s.regCache {
		held += x.bytes
		if x.osr != nil {
			held += x.osr.bytes
		}
	}
	for _, l := range s.regLearned {
		if l.regCounted {
			held += l.bytes
		}
	}
	if s.regHeld != held {
		t.Fatalf("the cache counts %d bytes of %d", s.regHeld, held)
	}
	// In use while it is compiled again, or runs.
	if !s.regInUse(oe) {
		oe.busy++
		if !s.regInUse(oe) {
			t.Errorf("code kept for a loop being compiled is not in use")
		}
		oe.busy--
		oe.runs++
		if !s.regInUse(oe) {
			t.Errorf("code kept for a loop that runs is not in use")
		}
		oe.runs--
	}
	before := s.regHeld
	s.dropReg(re.key, re)
	if re.osr != nil || !oe.dropped || oe.code != nil {
		t.Fatalf("the code kept for the loop outlived its function's: %+v", oe)
	}
	if s.regHeld >= before {
		t.Errorf("the cache holds %d bytes, %d before", s.regHeld, before)
	}
	// Compiled afresh, and right.
	for range 3 {
		if v, err := r.Run(compileForTest(t, `sum(100000, -1)`)); err != nil || v.Number() != 100000 {
			t.Fatalf("got %v, %v", v, err)
		}
	}

}
