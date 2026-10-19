//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitTierSetup has f read x off its receivers' prototypes, which a site that
// knew nothing searches no receiver's own table for: A's, B's, C's and D's
// objects are of four shapes.
const jitTierSetup = `function A(){} A.prototype.x=1; function B(){this.y=0} B.prototype.x=2;
	function C(){this.z=0;this.w=0} C.prototype.x=3; function D(){this.q=0} D.prototype.x=4;
	var a=new A(), b=new B(), c=new C(), d=new D();
	function f(o,n){let s=0;for(let i=0;i<n;i++)s=(s+o.x)|0;return s}`

// jitFewCallsForTest has r compile a function after a few calls, the
// warm-up of tests of what native code does once compiled, rather than
// V8's budgets (jitInvocationsForMaglev), unless it compiles at once.
func jitFewCallsForTest(r *Runtime) {
	if r.jitCallThreshold > jitInvocationsForFeedback {
		r.jitCallThreshold, r.jitICThreshold = jitInvocationsForFeedback, jitInvocationsForFeedback
	}
}

// jitTierRuntimeForTest runs setup in a runtime that compiles a function at
// its threshold's call, and in an interpreter, and returns a run that has
// both run src, failing on an answer that differs.
func jitTierRuntimeForTest(t *testing.T, threshold uint16, setup string) (*Runtime, func(src string)) {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold, r.jitICThreshold = threshold, threshold
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	return r, func(src string) {
		t.Helper()
		wv, err := want.Run(compileForTest(t, "String("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, "String("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		if got, w := gv.String().Go(), wv.String().Go(); got != w {
			t.Fatalf("%s: got %s, interpreter %s", src, got, w)
		}
	}
}

// An eager deoptimization of a function's current code invalidates it, as
// V8's deoptimizer does: Go enters it no more, nor do native callers call
// it, its link cleared; the existing tiers run the function, and it is
// compiled again once they have spent a fresh budget -- as many
// invocations as it took to compile it first (jitCallThreshold), each its
// length (jitTierBudget) -- for what its caches learned meanwhile: f,
// compiled for A, meets B, runs in the tree tier for three calls, and is
// compiled again at the fourth, for both, never deoptimizing again. Each
// answer is the interpreter's.
func TestJITDeoptInvalidates(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, jitTierSetup)
	for range 4 {
		run(`f(a,10)`)
	}
	e := jitNativeForTest(t, r, "f")
	if e.invalid || e.ssaSerial != 1 || e.link.entry == 0 {
		t.Fatalf("f is not compiled at its fourth call: %+v", e)
	}
	run(`f(b,10)`)
	if !e.invalid || e.deopts != 1 || e.link.entry != 0 || e.ssaSerial != 1 {
		t.Fatalf("f's deoptimization did not invalidate its code: invalid %v, deopts %d, link %#x, compiles %d", e.invalid, e.deopts, e.link.entry, e.ssaSerial)
	}
	entries := e.ssaStats.entries
	for k := 1; k <= 3; k++ {
		run(`f(b,10)`)
		if e.ssaSerial != 1 || !e.invalid || e.ssaStats.entries != entries {
			t.Fatalf("call %d after the deoptimization: compiled %d times, invalid %v, entered %d times", k, e.ssaSerial, e.invalid, e.ssaStats.entries-entries)
		}
	}
	guards := r.jit.guards
	run(`f(b,10)`)
	if e.ssaSerial != 2 || e.invalid || e.link.entry == 0 || e.ssaStats.entries == 0 {
		t.Fatalf("f was not compiled again at the budget's end: compiled %d times, invalid %v", e.ssaSerial, e.invalid)
	}
	run(`[f(a,10),f(b,10)].join()`)
	if r.jit.guards != guards || e.invalid {
		t.Fatalf("f compiled again for both deoptimized %d times", r.jit.guards-guards)
	}
}

// A change of a function's caches' feedback while its invalidated code waits
// for the existing tiers' budget delays the compile by a fresh budget, as
// V8's NotifyICChanged does: f, invalidated by B, meets C in the tree tier
// at its second call, and is compiled at the eighth, not the fourth, for
// all three shapes. The caches filled again with what they held -- the
// same entries, in another order -- delay nothing (jitFeedbackMark).
func TestJITFeedbackChangeDelaysTierUp(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, jitTierSetup)
	for range 4 {
		run(`f(a,10)`)
	}
	e := jitNativeForTest(t, r, "f")
	run(`f(b,10)`)
	if !e.invalid {
		t.Fatal("f's deoptimization did not invalidate its code")
	}
	for k, src := range []string{`f(b,10)`, `f(c,10)`, `f(b,10)`, `f(b,10)`, `f(b,10)`, `f(b,10)`, `f(b,10)`} {
		run(src)
		if e.ssaSerial != 1 || !e.invalid {
			t.Fatalf("call %d after the deoptimization: compiled again, its feedback changed at the second", k+1)
		}
	}
	guards := r.jit.guards
	run(`f(b,10)`)
	if e.ssaSerial != 2 || e.invalid {
		t.Fatalf("f was not compiled again a budget after its feedback changed: compiled %d times, invalid %v", e.ssaSerial, e.invalid)
	}
	run(`[f(a,10),f(b,10),f(c,10)].join()`)
	if r.jit.guards != guards {
		t.Fatalf("f compiled again for its three shapes deoptimized %d times", r.jit.guards-guards)
	}

	// The mark is what the caches hold: filled again with the same, the
	// same mark; a shape they did not hold, another.
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	m := jitFeedbackMark(cl)
	run(`[f(a,10),f(b,10),f(c,10),f(a,10)].join()`)
	if got := jitFeedbackMark(cl); got != m {
		t.Fatalf("mark %d, after meeting what the caches held, %d", got, m)
	}
	run(`f(d,10)`)
	if got := jitFeedbackMark(cl); got == m {
		t.Fatal("mark unchanged by a shape the caches did not hold")
	}
}

// A function that keeps deoptimizing is compiled no more past jitMaxDeopts
// eager deoptimizations, as V8 gives up optimizing one: f, at the limit,
// deoptimizes once more, and the tree tier runs it from then on, however
// many budgets it spends.
func TestJITDeoptLimit(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, jitTierSetup)
	for range 4 {
		run(`f(a,10)`)
	}
	e := jitNativeForTest(t, r, "f")
	e.deopts = jitMaxDeopts
	run(`f(b,10)`)
	if !e.invalid || e.deopts != jitMaxDeopts+1 {
		t.Fatalf("f's deoptimization past the limit: invalid %v, deopts %d", e.invalid, e.deopts)
	}
	tiered := r.jit.tieredUp
	for k := 0; k < 40; k++ {
		run(fmt.Sprintf(`f(%c,10)`, "ab"[k&1]))
	}
	if e.ssaSerial != 1 || !e.invalid || r.jit.tieredUp != tiered {
		t.Fatalf("f compiled again past the deoptimization limit: compiled %d times, invalid %v", e.ssaSerial, e.invalid)
	}
}

// A long invocation whose loop deoptimizes goes on in the interpreter, its
// code invalidated, and is compiled again once its back edges have spent
// the budget with its caches' feedback settled, entered at the loop's back
// edge by on-stack replacement, as V8's code optimized again is: g's
// receivers change at its thousandth iteration, and the rest of its
// twenty thousand run natively again, in the same invocation.
func TestJITDeoptTiersUpByOSR(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, jitTierSetup+`
		function g(n){let s=0;for(let i=0;i<n;i++){const o=i<1000?a:b;s=(s+o.x)|0}return s}`)
	run(`g(10)`)
	e := jitNativeForTest(t, r, "g")
	tiered, osrs := r.jit.tieredUp, r.jit.osrs
	run(`g(20000)`)
	if e.deopts == 0 || e.invalid || r.jit.tieredUp == tiered || r.jit.osrs == osrs || e.ssaStats.entries == 0 {
		t.Fatalf("g did not go on natively by on-stack replacement: deopts %d, invalid %v, tiered up %d, entered at loops %d", e.deopts, e.invalid, r.jit.tieredUp-tiered, r.jit.osrs-osrs)
	}
	guards := r.jit.guards
	run(`g(20000)`)
	if r.jit.guards != guards || e.invalid {
		t.Fatalf("g compiled again by on-stack replacement deoptimized %d times", r.jit.guards-guards)
	}
}

// Every invocation the existing tiers run of a function whose code is
// invalid spends its budget, a frameless one too: plus, a pure body the
// call path answers without a frame (pureCall), which reads what it
// captured and so is called natively, not inlined, compiled for its
// native caller, loop, and invalidated, is called by Go at each of loop's
// calls, and compiled again once four of those have spent its budget, as
// a frame of it would have been; loop then calls it natively. Uncounted,
// it was never compiled again, each call made by Go -- as AWFY's CD's
// Vector2D.plus was, invalidated to inline its construction.
func TestJITFramelessCallsSpendTheBudget(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// No collection meanwhile, which the budget is not about.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, run := jitTierRuntimeForTest(t, 4, `var m=(function(){
			function V(x){this.x=x}
			function plus(o){return new V(this.x+o.x)}
			V.prototype.plus=plus;
			return {V,plus};
		})(), V=m.V, plus=m.plus, one=new V(1);
		function loop(n){let v=new V(0);for(let i=0;i<n;i++)v=v.plus(one);return v.x}`)
	pcl := r.global.getOwn(r.atoms.intern("plus")).value().Object().fn().closure
	if pcl.fn.Leaf != bytecode.LeafPure {
		t.Fatalf("plus is no pure body: %v", pcl.fn.Leaf)
	}
	var pe *jitEntry
	for range 16 {
		run(`loop(100)`)
		if pe = r.jit.entryFor(pcl.fn); pe != nil && pe.ssa != nil && !pe.invalid {
			break
		}
	}
	jitNativeForTest(t, r, "loop")
	if pe == nil || pe.ssa == nil || pe.invalid {
		t.Fatalf("plus has no valid code for its native caller: compiled %v", pe != nil && pe.ssa != nil)
	}
	serial := pe.ssaSerial
	r.jitInvalidate(pcl, pe)
	run(`loop(3)`)
	if !pe.invalid || pe.ssaSerial != serial {
		t.Fatalf("plus compiled again after three calls: invalid %v, compiles %d", pe.invalid, pe.ssaSerial-serial)
	}
	run(`loop(1)`)
	if pe.invalid || pe.ssaSerial != serial+1 || pe.link.entry == 0 {
		t.Fatalf("plus not compiled again at its budget's end: invalid %v, compiles %d", pe.invalid, pe.ssaSerial-serial)
	}
	in := pe.link.in
	run(`loop(100)`)
	if pe.link.in-in < 100 {
		t.Fatalf("loop called plus natively %d times of 100", pe.link.in-in)
	}
}

// Code that learns at its exits what to be compiled again for is not
// invalidated, as V8 never invalidates code for feedback: it is the tier
// that learns it -- the existing tiers' caches note no call's target --
// and runs on, compiled again once it has spent a budget since it began
// to learn (jitLearnedDue): apply, compiled at its fourth call, learns at
// its calls of Go that its callback is inc, keeps its code, Go entering
// it, and is compiled again, inc inlined, only once a budget of its
// invocations has passed, Go making no call of inc after.
func TestJITLearnedCodeRunsOn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, `function inc(x){return x+1}
		function apply(fn,n){let s=0;for(let i=0;i<n;i++)s=fn(s);return s}`)
	for range 4 {
		run(`apply(inc,3)`)
	}
	e := jitNativeForTest(t, r, "apply")
	k := 0
	for ; k < 16 && !e.reoptPending(); k++ {
		run(`apply(inc,3)`)
	}
	if !e.reoptPending() || e.ssaSerial != 1 {
		t.Fatalf("apply learned nothing to compile again for in %d calls: compiled %d times", k, e.ssaSerial)
	}
	starts := e.starts
	for k = 0; k < 64 && e.ssaSerial == 1; k++ {
		if e.invalid || e.link.entry == 0 {
			t.Fatalf("call %d after apply learned: its code invalidated", k)
		}
		run(`apply(inc,3)`)
	}
	if e.ssaSerial != 2 || e.invalid {
		t.Fatalf("apply not compiled again for what it learned: compiled %d times, invalid %v", e.ssaSerial, e.invalid)
	}
	if k < 2 || e.starts-starts < 2 {
		t.Fatalf("apply compiled again %d calls after it learned, entered %d times meanwhile: no budget spent", k, e.starts-starts)
	}
	calls := r.jit.goCalls
	run(`apply(inc,100)`)
	if r.jit.goCalls != calls {
		t.Fatalf("apply compiled again made %d calls of Go", r.jit.goCalls-calls)
	}

	// What it learns counts in the mark that delays the compile: a call
	// decided, as a cache's new shape does.
	cl := r.global.getOwn(r.atoms.intern("apply")).value().Object().fn().closure
	m := jitLearnedMark(cl, e)
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: 0})
	if jitLearnedMark(cl, e) == m {
		t.Fatal("mark unchanged by a call learned")
	}
	e.nativeCalls = e.nativeCalls[:len(e.nativeCalls)-1]
}

// v8TierRuntimeForTest is a runtime with V8's budgets (jitInvocationsForMaglev,
// jitInvocationsAfterICUpdate), and an interpreter beside it, as
// jitTierRuntimeForTest's.
func v8TierRuntimeForTest(t *testing.T, setup string) (*Runtime, func(src string)) {
	t.Helper()
	r, run := jitTierRuntimeForTest(t, jitInvocationsForMaglev, setup)
	r.jitICThreshold = jitInvocationsAfterICUpdate
	if r.jitStress.threshold {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	return r, run
}

// A function is compiled once the existing tiers have spent
// invocation_count_for_maglev invocations' worth of its budget, each its
// bytecode's length, as V8 tiers up to Maglev: k, whose caches meet one
// shape from its first call on, is compiled at its 400th call, not at its
// 399th. A change of its caches' feedback raises what is left to
// minimum_invocations_after_ic_update invocations' worth, as
// NotifyICChanged does: g, the same, meeting a second shape at its 100th
// call, is compiled at its 900th, the change found when its budget was
// spent at its 400th, not before. Neither has a loop, whose back edges
// would spend their budget too (jitTierWork).
func TestJITV8Budgets(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, jitTierSetup+`
		function k(o){const p=[o];return p[0].x}
		function g(o){const p=[o];return p[0].x}`)
	fe := func(name string) *jitEntry {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		return r.jit.entryFor(cl.fn)
	}
	for n := 1; n < jitInvocationsForMaglev; n++ {
		run(`k(a)`)
		if e := fe("k"); e != nil && e.ssa != nil {
			t.Fatalf("k compiled at call %d", n)
		}
	}
	run(`k(a)`)
	if e := fe("k"); e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
		t.Fatalf("k not compiled at call %d", jitInvocationsForMaglev)
	}

	want := jitInvocationsForMaglev + jitInvocationsAfterICUpdate
	for k := 1; k < want; k++ {
		o := "a"
		if k >= 100 && k&1 == 0 {
			o = "b"
		}
		run(`g(` + o + `)`)
		if e := fe("g"); e != nil && e.ssa != nil {
			t.Fatalf("g compiled at call %d, its feedback changed at the 100th", k)
		}
	}
	run(`g(a)`)
	if e := fe("g"); e == nil || e.ssa == nil {
		t.Fatalf("g not compiled at call %d", want)
	}
	guards := r.jit.guards
	run(`[g(a),g(b)].join()`)
	if r.jit.guards != guards {
		t.Fatal("g, compiled for both shapes, deoptimized")
	}
}

// A long-running loop is entered by on-stack replacement once its
// function's budget is spent while the loop still runs, as V8's
// MaybeOptimizeFrame has it, not after a fixed number of back-edge
// budgets: each full budget of back edges spends its count of them, each
// the loop's length (jitTierWork). h, long, with a short loop, has spent
// part of its budget after a run of a quarter of it, and is compiled and
// entered at its loop in its second run, a long one, its first call
// spending next to none of it.
func TestJITV8LoopTierUp(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var b strings.Builder
	b.WriteString(`function h(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;`)
	for k := 0; k < 40; k++ {
		fmt.Fprintf(&b, "if(n===%d)s=(s+%d)|0;", -k-1, k)
	}
	b.WriteString(`return s}`)
	r, run := v8TierRuntimeForTest(t, b.String())
	cl := r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure
	e0 := jitTierWorkForTest(r, cl)
	budget := r.jitTierBudget(cl.fn, nil)
	quarter := int(budget / 4 / e0)
	run(fmt.Sprintf(`h(%d)`, quarter*backEdgeCheckInterval))
	e := r.jit.entryFor(cl.fn)
	if e != nil && e.ssa != nil {
		t.Fatalf("h compiled within a quarter of its budget")
	}
	if e == nil || !e.warming || e.tierBudget >= budget {
		t.Fatalf("h's loop spent none of its budget: %+v", e)
	}
	osrs := r.jit.osrs
	run(fmt.Sprintf(`h(%d)`, 8*quarter*backEdgeCheckInterval))
	e = r.jit.entryFor(cl.fn)
	if e == nil || e.ssa == nil || r.jit.osrs == osrs || e.starts != 0 {
		t.Fatalf("h's long loop was not entered by on-stack replacement: osrs %d, entry %+v", r.jit.osrs-osrs, e != nil && e.ssa != nil)
	}
}

// jitTierWorkForTest is the work a full budget of back edges in cl's loops
// spends at the least: half the interval, as backEdgeBudget varies it.
func jitTierWorkForTest(r *Runtime, cl *closure) int64 {
	return int64(backEdgeCheckInterval/2) * int64(jitLoopLength(cl.fn))
}

// A function warming up (jitWarmFor) that a native caller's call asks for
// is compiled at once, as one with no entry is (jitFor): g, a loop whose
// exception handler its lowering for native callers refuses, has been called a few
// times, its budget far from spent, when its caller learns the call, and
// is compiled for Go to enter and its callers to call -- not refused for
// native callers for good, its warming entry taken for a compile that had
// been tried. So AWFY's Richards' runTask, compiled first, had made every
// call of its tasks' functions by Go, at three and a half times the time.
func TestJITWarmingCalleeCompiledOnDemand(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, `function g(x){let s=0;for(let i=0;i<2;i++){try{s+=x}catch(e){s--}}return s}`)
	for range 2 * jitInvocationsForFeedback {
		run(`g(1)`)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	if _, err := r.jitLower(cl.fn, jitLowerInline); err == nil {
		t.Fatal("g is lowered for native callers: no test of a refusal")
	}
	e := r.jit.entryFor(cl.fn)
	if e == nil || !e.warming || e.ssa != nil {
		t.Fatalf("g is not warming up: %+v", e)
	}
	if !r.jitNativeOrLinked(cl) {
		t.Fatal("a native caller may not call g")
	}
	if e = r.jit.entryFor(cl.fn); e == nil || e.ssa == nil || e.warming || e.calleeRefused {
		t.Fatalf("g not compiled at its native caller's call: %+v", e != nil && e.ssa != nil)
	}
}

// Code Go does not enter at its start for its short stretches (entrySlow)
// is still entered by on-stack replacement at a loop that has run a full
// budget of back edges in the existing tiers, as V8's code is: the
// judgement is of the calls Go enters it at, short ones, and a long
// invocation, whose loop proves it long, goes on natively. sum, judged
// slow, runs a call of a million iterations natively from its loop.
// DeltaBlue's forEach, judged slow on its many short calls, had run its
// plan's, thousands of constraints long, in the tree tier.
func TestJITSlowCodeEnteredByOSR(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, `function sum(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}`)
	run(`sum(10)`)
	e := jitNativeForTest(t, r, "sum")
	e.entrySlow = true
	starts, osrs, entries := e.starts, r.jit.osrs, e.ssaStats.entries
	run(`sum(3)`)
	if e.starts != starts || r.jit.osrs != osrs {
		t.Fatal("Go entered sum, judged slow, at a short call")
	}
	run(`sum(1000000)`)
	if r.jit.osrs == osrs || e.ssaStats.entries == entries {
		t.Fatalf("sum's long loop was not entered by on-stack replacement: %d tries at loops, %d native entries", r.jit.osrs-osrs, e.ssaStats.entries-entries)
	}
}

// test262's buildString, under V8's budgets: codePoints.length = 0, at a
// megamorphic site, between the chunks it appends to result. A write
// there that Go made on the store's miss path, which native code's
// analyses took for no call, gave a wrong string, and crashed. Each answer
// is the interpreter's.
func TestJITMegamorphicWriteKeeps(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function buildString(args) {
			const loneCodePoints = args.loneCodePoints
			const ranges = args.ranges
			const CHUNK_SIZE = 10000
			let result = String.fromCodePoint.apply(null, loneCodePoints)
			for (let i = 0; i < ranges.length; i++) {
				let range = ranges[i]
				let start = range[0]
				let end = range[1]
				let codePoints = []
				for (let length = 0, codePoint = start; codePoint <= end; codePoint++) {
					codePoints[length++] = codePoint
					if (length === CHUNK_SIZE) {
						result += String.fromCodePoint.apply(null, codePoints)
						codePoints.length = length = 0
					}
				}
				result += String.fromCodePoint.apply(null, codePoints)
			}
			return result
		}
		const s = buildString({ loneCodePoints: [], ranges: [[0xDC00, 0xDFFF], [0x80, 0xDBFF], [0xE000, 0x10FFFF]] })
		s.length + ':' + s.charCodeAt(s.length >> 1) + ':' + s.charCodeAt(s.length - 1)`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	gv, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, w := gv.String().Go(), wv.String().Go(); got != w {
		t.Fatalf("got %s, interpreter %s", got, w)
	}
}

// A megamorphic site's write that neither the receiver's own table nor the
// megamorphic cache has -- a property it adds, a shape it never met -- Go
// makes, called from native code (ssa.GoCall) on a branch of its own, which
// goes on after it, as V8's megamorphic stores call the runtime: mark, of objects of eight
// shapes, adding hit to some and writing it in others, never leaves its
// code, entered from Go and called natively alike; each answer, and each
// object, is the interpreter's. Each such write had left native code, the
// rest of the invocation in the interpreter: RayTrace's testIntersection,
// of best.hitCount, at a call in eleven.
func TestJITMegamorphicWritesByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, `var shapes=[];
		for(let k=0;k<8;k++){const o={};for(let j=0;j<=k;j++)o['p'+j]=j;if(k&1)o.hit=0;shapes.push(o)}
		function mark(o,v){let s=0;for(let i=0;i<2;i++)s+=i;o.hit=v+s;return o}
		function each(n){let t=0;for(let i=0;i<n;i++){const o=Object.assign({},shapes[i&7]);t+=mark(o,i).hit}return t}`)
	for range 4 {
		run(`each(64)`)
	}
	e := jitNativeForTest(t, r, "mark")
	if p := r.global.getOwn(r.atoms.intern("mark")).value().Object().fn().closure; !r.jitSiteMegamorphic(p, e, jitSetPropPC(t, p.fn)) {
		t.Fatal("mark's write is not megamorphic: no test of its misses")
	}
	ee := jitNativeForTest(t, r, "each")
	hosts, ehosts, interpreted := e.ssaStats.hosts, ee.ssaStats.hosts, r.jit.interpreted
	run(`each(400)`)
	run(`JSON.stringify(shapes.map(o=>mark(Object.assign({},o),1)))`)
	if n := e.ssaStats.hosts - hosts + ee.ssaStats.hosts - ehosts; n != 0 {
		t.Errorf("mark, or each calling it, left native code %d times", n)
	}
	if n := r.jit.interpreted - interpreted; n != 0 {
		t.Errorf("the interpreter finished %d invocations", n)
	}
}

// jitSetPropPC is the PC of fn's one set_prop.
func jitSetPropPC(t *testing.T, fn *bytecode.Function) int {
	t.Helper()
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpSetProp {
			return pc
		}
	}
	t.Fatal("no set_prop")
	return -1
}

// A megamorphic site's read that neither the receiver's own table nor the
// megamorphic cache has -- a shape it never met, a property up a chain, a
// getter -- Go makes, called from native code (ssa.GoCall) on a branch of
// its own, and the code goes on with its value, as V8's megamorphic loads
// call the runtime: look, of objects of a dozen shapes, some too large to
// search, some with the property their own, some their prototype's, data
// or a getter, and some with none, never leaves its code, entered from Go
// and called natively alike, and each answer is the interpreter's. Each
// such read had left native code, the rest of the invocation in the
// interpreter: Typescript's at typescript-compiler.js:1607, nearly four
// thousand times in five runs.
func TestJITMegamorphicReadsByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, `var data={hit:7}, getter={get hit(){return this.p0+100}}, shapes=[];
		for(let k=0;k<12;k++){const o=Object.create(k%3==1?data:k%3==2?getter:Object.prototype);
			for(let j=0;j<=k;j++)o['p'+j]=j;if(k%4==0)o.hit=k;shapes.push(o)}
		function look(o){let s=0;for(let i=0;i<2;i++)s+=i;const h=o.hit;return (h===undefined?-1:h)+s}
		function each(n){let t=0;for(let i=0;i<n;i++){const s=shapes[i%12],o=Object.create(Object.getPrototypeOf(s));Object.assign(o,s);t+=look(o)}return t}`)
	for range 4 {
		run(`each(96)`)
	}
	e := jitNativeForTest(t, r, "look")
	if p := r.global.getOwn(r.atoms.intern("look")).value().Object().fn().closure; !r.jitSiteMegamorphic(p, e, jitGetPropPC(t, p.fn)) {
		t.Fatal("look's read is not megamorphic: no test of its misses")
	}
	ee := jitNativeForTest(t, r, "each")
	hosts, ehosts, interpreted := e.ssaStats.hosts, ee.ssaStats.hosts, r.jit.interpreted
	run(`each(600)`)
	run(`JSON.stringify(shapes.map(o=>look(o)))`)
	if n := e.ssaStats.hosts - hosts + ee.ssaStats.hosts - ehosts; n != 0 {
		t.Errorf("look, or each calling it, left native code %d times", n)
	}
	if n := r.jit.interpreted - interpreted; n != 0 {
		t.Errorf("the interpreter finished %d invocations", n)
	}
}

// jitGetPropPC is the PC of fn's first get_prop.
func jitGetPropPC(t *testing.T, fn *bytecode.Function) int {
	t.Helper()
	for pc, in := range fn.Code {
		if in.Op == bytecode.OpGetProp {
			return pc
		}
	}
	t.Fatal("no get_prop")
	return -1
}

// A megamorphic site's method read whose miss Go makes (ssa.GoCall) leaves
// the site's cache as the interpreter reads a method (get_prop_this, by
// cachedProp, whose entries are data): a getter's read of the method there
// had filled the cache as a property read's (cachedGet), its entry the
// getter's, which the interpreter then took for the method itself -- the
// getter's pair, an object no call could make: Intl's format, a getter of
// the bound function, read and called by test262's NumberFormat tests.
func TestJITMegamorphicMethodReadsByGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, `var own={}, getter={get m(){return function(k){return this.p0+k+100}}}, shapes=[];
		own.m=function(k){return this.p0+k};
		for(let k=0;k<12;k++){const o=Object.create(k%2?getter:own);for(let j=0;j<=k;j++)o['p'+j]=j;shapes.push(o)}
		function call(o){let s=0;for(let i=0;i<2;i++)s+=i;return o.m(s)}
		function each(n){let t=0;for(let i=0;i<n;i++)t+=call(shapes[i%12]);return t}`)
	for range 4 {
		run(`each(96)`)
	}
	e := jitNativeForTest(t, r, "call")
	cl := r.global.getOwn(r.atoms.intern("call")).value().Object().fn().closure
	pc := -1
	for i, in := range cl.fn.Code {
		if in.Op == bytecode.OpGetPropThis {
			pc = i
		}
	}
	if pc < 0 || !r.jitSiteMegamorphic(cl, e, pc) {
		t.Fatal("call's method read is not megamorphic: no test of its misses")
	}
	run(`each(600)`)
	// What the site's cache answers for each receiver is what a read finds.
	in := cl.fn.Code[pc]
	shapes := r.global.getOwn(r.atoms.intern("shapes")).value().Object()
	for i, s := range shapes.elemsOf() {
		got, ok := r.cachedProp(&cl.ic[in.B], s.Object(), cl.names[in.A])
		want, err := r.getValueProp(s, cl.names[in.A])
		if err != nil {
			t.Fatal(err)
		}
		if ok && !got.sameBits(want) {
			t.Errorf("shape %d: the cache of call's method read answers other than a read", i)
		}
	}
	run(`JSON.stringify(shapes.map(o=>call(o)))`)
}

// An inlined callee's array and object literals are made natively, from
// pools the code inlining it keeps (jitFeedback's Literal), as its own
// are: reset, inlined into each, makes an empty array and an empty object
// at every call, and each, native, leaves only for the pools to be filled
// again. Inlined, they had been Go's, at every one:
// Typescript's errorSink = [] and commentStack = [], fourteen thousand
// times in five runs. Each answer is the interpreter's.
func TestJITInlinedLiterals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, run := jitTierRuntimeForTest(t, 1, `function Sink(){this.errors=[];this.box={}}
		Sink.prototype.reset=function(){this.errors=[];this.box={};return this.errors};
		var s=new Sink;
		function warm(n){let t=0;for(let i=0;i<n;i++)t+=s.reset().length;return t}
		function each(n){let t=0;for(let i=0;i<n;i++){const a=s.reset();a.push(i);s.box.v=i;t=(t+a.length+s.errors.length+s.box.v)|0}return t}`)
	for range 4 {
		run(`warm(64)`)
		run(`each(64)`)
	}
	e := jitNativeForTest(t, r, "each")
	reset := r.global.getOwn(r.atoms.intern("s")).value().Object().proto.getOwn(r.atoms.intern("reset")).value().Object().fn().closure
	if !slices.Contains(e.ssaInlined, reset) {
		t.Fatal("each does not inline reset: no test of an inlined callee's literals")
	}
	hosts := e.ssaStats.hosts
	run(`each(600)`)
	if n := e.ssaStats.hosts - hosts; n > 2*600/abi.PoolCapacity+4 {
		t.Errorf("each left native code %d times", n)
	}
}
