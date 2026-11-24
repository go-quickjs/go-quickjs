//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
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
	return jitTierRuntimeWithForTest(t, threshold, setup, nil)
}

// jitTierRuntimeWithForTest is jitTierRuntimeForTest, each runtime given
// install's globals, where it is not nil, before setup runs.
func jitTierRuntimeWithForTest(t *testing.T, threshold uint16, setup string, install func(*Runtime)) (*Runtime, func(src string)) {
	t.Helper()
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold, r.jitICThreshold = threshold, threshold
	for _, rt := range []*Runtime{want, r} {
		if install != nil {
			install(rt)
		}
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
// it, its link cleared; the interpreter runs the function, and it is
// compiled again once it has spent a fresh budget -- as many invocations
// as it took to compile it first (jitCallThreshold), each its length
// (jitRegBudget) -- for what its caches learned meanwhile: f, compiled
// for A, meets B, runs in the interpreter for three calls, and is
// compiled again at the fourth, for both, never deoptimizing again. Each
// answer is the interpreter's.
func TestJITDeoptInvalidates(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, jitTierSetup)
	for range 4 {
		run(`f(a,10)`)
	}
	e := jitNativeForTest(t, r, "f")
	fcl := jitGlobalClosure(r, "f")
	if e.invalid || e.ssaSerial != 1 || jitLinkOfForTest(r, fcl).entry == 0 {
		t.Fatalf("f is not compiled at its fourth call: %+v", e)
	}
	// Invocations of B's, whose back edges each spend their loop's length
	// of the budget (jitRegEdges), so that what the invocations spend is
	// their own length -- but the one that deoptimizes, which meets B at
	// its first turn.
	deopt, fb := `f(b,1)`, `f(b,0)`
	run(deopt)
	if link := jitLinkOfForTest(r, fcl); !e.invalid || e.deopts != 1 || link.entry != 0 || e.ssaSerial != 1 {
		t.Fatalf("f's deoptimization did not invalidate its code: invalid %v, deopts %d, link %#x, compiles %d", e.invalid, e.deopts, link.entry, e.ssaSerial)
	}
	entries := e.ssaStats.entries
	for k := 1; k <= 3; k++ {
		run(fb)
		if e.ssaSerial != 1 || !e.invalid || e.ssaStats.entries != entries {
			t.Fatalf("call %d after the deoptimization: compiled %d times, invalid %v, entered %d times", k, e.ssaSerial, e.invalid, e.ssaStats.entries-entries)
		}
	}
	guards := r.jit.guards
	run(fb)
	if e.ssaSerial != 2 || e.invalid || jitLinkOfForTest(r, fcl).entry == 0 || e.ssaStats.entries == 0 {
		t.Fatalf("f was not compiled again at the budget's end: compiled %d times, invalid %v", e.ssaSerial, e.invalid)
	}
	run(`[f(a,10),f(b,10)].join()`)
	if r.jit.guards != guards || e.invalid {
		t.Fatalf("f compiled again for both deoptimized %d times", r.jit.guards-guards)
	}
}

// A change of a function's caches' feedback while its invalidated code waits
// for the existing tiers' budget delays the compile by a fresh budget, as
// V8's NotifyICChanged does: f, invalidated by B, meets C in the
// interpreter at its second call, and is compiled at the eighth, not the fourth, for
// all three shapes. The caches filled again with what they held -- the
// same entries, in another order -- delay nothing (jitFeedbackMark).
func TestJITFeedbackChangeDelaysTierUp(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, jitTierSetup)
	for range 4 {
		run(`f(a,10)`)
	}
	e := jitNativeForTest(t, r, "f")
	// Invocations of no turns but where they meet a shape, as
	// TestJITDeoptInvalidates has them: a turn, whose back edges' work no
	// check spends before the compile (jitRegEdges).
	deopt, fb, fc := `f(b,1)`, `f(b,0)`, `f(c,1)`
	run(deopt)
	if !e.invalid {
		t.Fatal("f's deoptimization did not invalidate its code")
	}
	for k, src := range []string{fb, fc, fb, fb, fb, fb, fb} {
		run(src)
		if e.ssaSerial != 1 || !e.invalid {
			t.Fatalf("call %d after the deoptimization: compiled again, its feedback changed at the second", k+1)
		}
	}
	guards := r.jit.guards
	run(fb)
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
// deoptimizes once more, and the interpreter runs it from then on, however
// many budgets it spends.
func TestJITDeoptLimit(t *testing.T) {
	t.Parallel()
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
	for k := 0; k < 40; k++ {
		run(fmt.Sprintf(`f(%c,10)`, "ab"[k&1]))
	}
	if e.ssaSerial != 1 || !e.invalid {
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
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 1, jitTierSetup+`
		function g(n){let s=0;for(let i=0;i<n;i++){const o=i<1000?a:b;s=(s+o.x)|0}return s}`)
	run(`g(10)`)
	e := jitNativeForTest(t, r, "g")
	osrs, serial := r.jit.osrs, e.ssaSerial
	run(`g(20000)`)
	// Register code's compiles after an invalidation are counted in its
	// function's state (ssaSerial), as any compile again of it is.
	tieredUp := uint64(e.ssaSerial - serial)
	if e.deopts == 0 || e.invalid || tieredUp == 0 || r.jit.osrs == osrs || e.ssaStats.entries == 0 {
		t.Fatalf("g did not go on natively by on-stack replacement: deopts %d, invalid %v, tiered up %d, entered at loops %d", e.deopts, e.invalid, tieredUp, r.jit.osrs-osrs)
	}
	guards := r.jit.guards
	run(`g(20000)`)
	if r.jit.guards != guards || e.invalid {
		t.Fatalf("g compiled again by on-stack replacement deoptimized %d times", r.jit.guards-guards)
	}
}

// Every invocation the existing tiers run of a function whose code is
// invalid spends its budget, a frameless one too: plus, a pure body the
// call path answers without a frame (pureCall), called natively, not
// inlined (jitNoInlineForTest), compiled for its
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
	defer holdCollectorNow(t)()
	jitNoInlineForTest(t, "plus")
	r, run := jitTierRuntimeForTest(t, 4, `var m=(function(){
			function V(x){this.x=x}
			function plus(o){return new V(this.x+o.x)}
			V.prototype.plus=plus;
			return {V,plus};
		})(), V=m.V, plus=m.plus, one=new V(1);
		function loop(n){let v=new V(0);for(let i=0;i<n;i++)v=v.plus(one);return v.x}`)
	pcl := r.global.getOwn(r.atoms.intern("plus")).value().Object().fn().closure
	if k := pcl.fn.RegCode().Leaf; k != bytecode.LeafPure {
		t.Fatalf("plus is no pure body: %v", k)
	}
	var pe *jitEntry
	for range 16 {
		run(`loop(100)`)
		if pe = jitEntryOfForTest(r, pcl); pe != nil && jitHasCodeOfForTest(r, pcl) && !pe.invalid {
			break
		}
	}
	jitNativeForTest(t, r, "loop")
	if pe == nil || !jitHasCodeOfForTest(r, pcl) || pe.invalid {
		t.Fatalf("plus has no valid code for its native caller: compiled %v", pe != nil && jitHasCodeOfForTest(r, pcl))
	}
	serial := pe.ssaSerial
	r.jitRegInvalidate(pcl, jitRegOfForTest(r, pcl))
	run(`loop(3)`)
	if !pe.invalid || pe.ssaSerial != serial {
		t.Fatalf("plus compiled again after three calls: invalid %v, compiles %d", pe.invalid, pe.ssaSerial-serial)
	}
	run(`loop(1)`)
	if pe.invalid || pe.ssaSerial != serial+1 || jitLinkOfForTest(r, pcl).entry == 0 {
		t.Fatalf("plus not compiled again at its budget's end: invalid %v, compiles %d", pe.invalid, pe.ssaSerial-serial)
	}
	link := jitLinkOfForTest(r, pcl)
	in := link.in
	run(`loop(100)`)
	if link.in-in < 100 {
		t.Fatalf("loop called plus natively %d times of 100", link.in-in)
	}
}

// Code that learns at its exits what to be compiled again for is not
// invalidated, as V8 never invalidates code for feedback: it is the tier
// that learns it -- the interpreter's caches note no call's target -- and
// runs on, compiled again once it has spent a budget since it began to
// learn (jitRegLearnedDue): apply, compiled at its fourth call, learns at
// its calls of Go that its callback is inc, keeps its code, Go entering
// it, and is compiled again, inc inlined, only once a budget of its
// invocations has passed, Go making no call of inc after.
func TestJITLearnedCodeRunsOn(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := jitTierRuntimeForTest(t, 4, `function inc(x){return x+1}
		function apply(fn,n){let s=0;for(let i=0;i<n;i++)s=fn(s);return s}`)
	// Compiled with no call of inc seen: register code's call caches note
	// their targets, which its first compile would inline (regCallSites),
	// so it is compiled at calls that make none, and learns inc at its
	// calls of Go. Counted at their returns (regTickRet): compiled at the
	// call after the fourth.
	warm, warmCalls := `apply(inc,0)`, 5
	for range warmCalls {
		run(warm)
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
	// Calls of no turns: register code's native stretches spend its budget
	// by their turns too (jitRegStretchWork), as its back edges do in the
	// interpreter: what they spend is their length.
	after := `apply(inc,0)`
	for k = 0; k < 64 && e.ssaSerial == 1; k++ {
		if e.invalid || jitLinkOfForTest(r, jitGlobalClosure(r, "apply")).entry == 0 {
			t.Fatalf("call %d after apply learned: its code invalidated", k)
		}
		run(after)
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
// would spend their budget too (jitRegEdgeWork).
func TestJITV8Budgets(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, jitTierSetup+`
		function k(o){const p=[o];return p[0].x}
		function g(o){const p=[o];return p[0].x}`)
	fe := func(name string) *jitEntry {
		return jitEntryOfForTest(r, jitGlobalClosure(r, name))
	}
	code := func(name string) bool { return jitHasCodeOfForTest(r, jitGlobalClosure(r, name)) }
	for n := 1; n < jitInvocationsForMaglev; n++ {
		run(`k(a)`)
		if code("k") {
			t.Fatalf("k compiled at call %d", n)
		}
	}
	// Register code counts an invocation at its return (regTickRet): a
	// budget spent by the last invocation's run compiles the function at
	// the call after it.
	late := 1
	run(`k(a)`)
	if code("k") {
		t.Fatalf("k compiled at call %d", jitInvocationsForMaglev)
	}
	run(`k(a)`)
	if e := fe("k"); e == nil || !code("k") || e.ssaStats.entries == 0 {
		t.Fatalf("k not compiled at call %d", jitInvocationsForMaglev+late)
	}

	want := jitInvocationsForMaglev + jitInvocationsAfterICUpdate + late
	for k := 1; k < want; k++ {
		o := "a"
		if k >= 100 && k&1 == 0 {
			o = "b"
		}
		run(`g(` + o + `)`)
		if code("g") {
			t.Fatalf("g compiled at call %d, its feedback changed at the 100th", k)
		}
	}
	run(`g(a)`)
	if !code("g") {
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
// the loop's length (jitRegEdgeWork). h, long, with a short loop, has spent
// part of its budget after a run of a quarter of it, and is compiled and
// entered at its loop in its second run, a long one, its first call
// spending next to none of it.
func TestJITV8LoopTierUp(t *testing.T) {
	t.Parallel()
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
	var budget int64
	var turns int
	// Register code's back edges spend its budget each at its loop's
	// length, counted, not sampled (jitRegEdges): a quarter of it is
	// as many turns as take a quarter of it.
	c := r.regOf(cl)
	budget = jitRegBudget(r.jitCallThreshold, c)
	turns = int(budget / 4 / int64(jitRegLoopsForTest(c)[0]))
	run(fmt.Sprintf(`h(%d)`, turns))
	e := jitEntryOfForTest(r, cl)
	if jitHasCodeOfForTest(r, cl) {
		t.Fatalf("h compiled within a quarter of its budget")
	}
	if e == nil || !e.warming || e.tierBudget >= budget {
		t.Fatalf("h's loop spent none of its budget: %+v", e)
	}
	osrs := r.jit.osrs
	run(fmt.Sprintf(`h(%d)`, 8*turns))
	e = jitEntryOfForTest(r, cl)
	if e == nil || !jitHasCodeOfForTest(r, cl) || r.jit.osrs == osrs || e.starts != 0 {
		t.Fatalf("h's long loop was not entered by on-stack replacement: osrs %d, entry %+v", r.jit.osrs-osrs, jitHasCodeOfForTest(r, cl))
	}
}

// A loop's back edges spend its function's budget at the loop's own
// length (jitRegEdgeWork), not at the mean of its function's loops': k's short
// loop, run for half its budget at its length, is not compiled, where at
// the mean, which k's long loop makes many times as long, the budget would
// have been spent several times over; run for twice it, it is.
func TestJITLoopSpendsItsOwnLength(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, `function k(n,m){let t=0;for(let j=0;j<m;j++){`+strings.Repeat(`t=(t*3+j)|0;`, 30)+`}for(let i=0;i<n;i++)t=(t+i)|0;return t}`)
	cl := r.global.getOwn(r.atoms.intern("k")).value().Object().fn().closure
	var short, mean int
	var budget int64
	// Register code's back edges count each at its own loop's length
	// (jitRegEdges): the long loop's against the short one's, where a
	// mean would have spent the budget at the long one's.
	c := r.regOf(cl)
	loops := jitRegLoopsForTest(c)
	short, mean = slices.Min(loops), slices.Max(loops)
	budget = jitRegBudget(r.jitCallThreshold, c)
	if mean < 8*short {
		t.Fatalf("k's loops: mean %d, short %d", mean, short)
	}
	edges := int(budget) / short / 2
	run(fmt.Sprintf(`k(%d,0)`, edges))
	if e := jitEntryOfForTest(r, cl); e == nil || jitHasCodeOfForTest(r, cl) || !e.warming {
		t.Fatalf("k compiled within half its budget at its short loop's length: %+v", jitHasCodeOfForTest(r, cl))
	}
	run(fmt.Sprintf(`k(%d,0)`, 4*edges))
	if !jitHasCodeOfForTest(r, cl) {
		t.Fatal("k not compiled at its short loop once its budget is spent")
	}
}

// A function past the 4,096 instructions the JIT had compiled is compiled
// as any other, where what its compile holds fits the JIT's budget
// (jitCompiles): zlib's bk, 5,039 instructions and called hundreds of
// times a run, had run in the interpreter. At the runtime's default budget
// too, which the estimate of a word a slot an instruction had kept long
// from.
func TestJITCompilesLongFunctions(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `function long(n){let t=0;for(let i=0;i<n;i++){` + strings.Repeat(`t=(t*3+i)|0;`, 2100) + `}return t}`
	for _, budget := range []int64{32 << 20, 0} {
		r, run := jitTierRuntimeForTest(t, 2, src)
		r.jitMemoryLimit = budget
		cl := r.global.getOwn(r.atoms.intern("long")).value().Object().fn().closure
		if n := len(firstReg(cl.fn).rc.Code); n <= 4096 {
			t.Fatalf("long is %d instructions", n)
		}
		for range 4 {
			run(`long(3)`)
		}
		e := jitEntryOfForTest(r, cl)
		if compiles := jitCompiles(cl.fn, r.jitBudget()); !compiles || e == nil || !jitHasCodeOfForTest(r, cl) || e.ssaStats.entries == 0 {
			t.Fatalf("long (%d instructions) not run natively within %d bytes: %+v", len(firstReg(cl.fn).rc.Code), r.jitBudget(), jitHasCodeOfForTest(r, cl))
		}
	}
}

// A function warming up (jitRegWarmUp) that a native caller's call asks for
// is compiled at once, as one with no entry is (jitRegNativeCallee): g, a
// loop that defines an accessor of a computed key (define_getter_index),
// which its lowering for native callers refuses, has been called a few
// times, its budget far from spent, when its caller learns the call, and is
// compiled for Go to enter and its callers to call -- not refused for native
// callers for good, its warming entry taken for a compile that had been
// tried. So AWFY's Richards' runTask, compiled first, had made every call of
// its tasks' functions by Go, at three and a half times the time.
func TestJITWarmingCalleeCompiledOnDemand(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, `function id(v){return v} function g(x){'use strict';let s=0;for(let i=0;i<2;i++)s+=x;const o={get ['k'+x](){return 1}};return id(s)}`)
	for range 2 * jitInvocationsForFeedback {
		run(`g(1)`)
	}
	cl := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	// Register code: its lowering for native callers (LowerRegInline)
	// refused, and its native caller's call of Go asks for its code
	// (jitRegNativeCallee).
	c := r.regOf(cl)
	if _, err := r.jitRegLower(cl, c.rc, jitLowerInline); err == nil {
		t.Fatal("g is lowered for native callers: no test of a refusal")
	}
	e := jitEntryOfForTest(r, cl)
	if e == nil || !e.warming || jitHasCodeOfForTest(r, cl) {
		t.Fatalf("g is not warming up: %+v", e)
	}
	r.jitRegNativeCallee(cl, c, nil)
	if e = jitEntryOfForTest(r, cl); e == nil || !jitHasCodeOfForTest(r, cl) || e.warming || cl.closureCode.jitRegRefused || cl.closureCode.jitReg == nil {
		t.Fatalf("g not compiled at its native caller's call: %+v", jitHasCodeOfForTest(r, cl))
	}
}

// jitRareCallForTest has h make call, of i, once in twenty-five of its
// calls: less often than jitInliningFrequency.
func jitRareCallForTest(call string) string {
	return `var q=0;function h(n,k){let t=0;for(let i=0;i<n;i++){t=(t+i)|0;if(k&&(++q%25===0))t=(t+` + call + `)|0}return t}`
}

// A callee too long to inline (ssa.MaxInline) that its native caller calls
// seldom warms up as any function does, and is compiled once it has spent
// its own budget -- not at its native caller's first call of it, nor when
// that caller is compiled: it would not be inlined in V8 either
// (jitRegCalledOften), and Mandreel's per-frame functions, __udivmoddi4's
// 1,233 instructions among them, had been compiled so. g, called by h's code
// once in twenty-five of h's calls, is compiled at about its own 400th call,
// and is called natively by h from then on. Meanwhile h's call waits for it,
// decided no more at each of g's calls (jitCallWaiting).
func TestJITLongCalleeWarmsUp(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, `function g(x){`+strings.Repeat(`x=(x*3+1)|0;`, jitLongRepeatsForTest(16))+`return x}`+jitRareCallForTest(`g(i)`))
	g := r.global.getOwn(r.atoms.intern("g")).value().Object().fn().closure
	h := r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure
	if n := jitInlineSizeForTest(r, g); n <= ssa.MaxInline {
		t.Fatalf("g is %d instructions, short enough to inline", n)
	}
	for range jitInvocationsForMaglev {
		run(`h(4,0)`)
	}
	if !jitHasCodeOfForTest(r, h) {
		t.Fatal("h has no code")
	}
	for i := range 25 * (jitInvocationsForMaglev - 8) {
		run(`h(1,1)`)
		if jitHasCodeOfForTest(r, g) {
			t.Fatalf("g compiled at its %d. call", (i+1)/25)
		}
	}
	for range 25 * jitInvocationsForMaglev {
		run(`h(1,1)`)
	}
	e := jitEntryOfForTest(r, g)
	if e == nil || !jitHasCodeOfForTest(r, g) {
		t.Fatalf("g not compiled once warm: %+v", e)
	}
	link := jitLinkOfForTest(r, g)
	in := link.in
	run(`h(250,1)`)
	if link.in-in < 10 {
		t.Fatalf("h's code called g natively %d times of 10", link.in-in)
	}
}

// A callee too long to inline that its native caller calls often is
// compiled for it once it has been invoked jitInliningFrequency hundredths
// as often as the caller, as V8 would inline it, well before its own
// budget is spent: g, called at each of h's calls once h is compiled, is
// compiled by its 100th, and h calls it natively. One past what a
// function inlines in all (ssa.MaxInlineTotal), which V8 would not inline,
// waits for its budget however often it is called: big. The calls are
// counted as invocations of their caller, not by the work of its loop's
// back edges, which a long loop's would make look seldom: w's loop, run
// once, calls g at every tenth of its iterations, which is compiled when w
// is, before its own budget is spent. Octane's Richards had run
// HandlerTask.run, called 0.37 times a call of its caller, in the
// interpreter for 750 calls.
func TestJITFrequentLongCalleeCompiledForCaller(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	body := func(n int) string { return strings.Repeat(`x=(x*3+1)|0;`, n) }
	r, run := v8TierRuntimeForTest(t, `function g(x){`+body(jitLongRepeatsForTest(16))+`return x}
		function big(x){`+body(jitLongRepeatsForTest(60))+`return x}
		function h(n,k){let t=0;for(let i=0;i<n;i++){t=(t+i)|0;if(k===1)t=(t+g(i))|0;if(k===2)t=(t+big(i))|0}return t}`)
	closure := func(name string) *closure {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	}
	g, big, h := closure("g"), closure("big"), closure("h")
	if n := jitInlineSizeForTest(r, g); n <= ssa.MaxInline || n > ssa.MaxInlineTotal || jitInlineSizeForTest(r, big) <= ssa.MaxInlineTotal {
		t.Fatalf("g is %d instructions, big %d", n, jitInlineSizeForTest(r, big))
	}
	for range jitInvocationsForMaglev {
		run(`h(4,0)`)
	}
	if !jitHasCodeOfForTest(r, h) {
		t.Fatal("h has no code")
	}
	calls := 0
	for ; calls < jitInvocationsForMaglev/4; calls++ {
		if jitHasCodeOfForTest(r, g) {
			break
		}
		run(`h(1,1)`)
	}
	if calls < jitInvocationsForFeedback || calls >= jitInvocationsForMaglev/4 {
		t.Fatalf("g compiled at its %d. call", calls)
	}
	// h's code is compiled again for the call once it has spent a budget
	// (jitRegLearnedDue).
	for range 2 * jitInvocationsForMaglev {
		run(`h(1,1)`)
	}
	gl := jitLinkOfForTest(r, g)
	in := gl.in
	run(`h(100,1)`)
	if he := jitEntryOfForTest(r, h); !slices.ContainsFunc(he.nativeCalls, func(x jitInline) bool { return x.cl == g }) || gl.in-in < 100 {
		t.Fatalf("h's code called g natively %d times of 100", gl.in-in)
	}
	rb, runb := v8TierRuntimeForTest(t, `function big(x){`+body(jitLongRepeatsForTest(60))+`return x}
		function h(n,k){let t=0;for(let i=0;i<n;i++){t=(t+i)|0;if(k)t=(t+big(i))|0}return t}`)
	big = rb.global.getOwn(rb.atoms.intern("big")).value().Object().fn().closure
	for range jitInvocationsForMaglev {
		runb(`h(4,0)`)
	}
	for i := range jitInvocationsForMaglev / 2 {
		runb(`h(1,1)`)
		if jitHasCodeOfForTest(rb, big) {
			t.Fatalf("big compiled at its %d. call", i+1)
		}
	}
	r2, run2 := v8TierRuntimeForTest(t, `function g(x){`+body(jitLongRepeatsForTest(16))+`return x}
		function w(n){let t=0;for(let i=0;i<n;i++){t=(t*3+i)|0;if(i%10===0)t=(t+g(i))|0}return t}`)
	g2 := r2.global.getOwn(r2.atoms.intern("g")).value().Object().fn().closure
	run2(fmt.Sprintf(`w(%d)`, 64*backEdgeCheckInterval))
	e := jitEntryOfForTest(r2, g2)
	invoked := int64(0)
	if e != nil {
		// Register code's runs while it had no code (jitRegInvoked).
		invoked = jitRegInvoked(g2, e)
	}
	if e == nil || !jitHasCodeOfForTest(r2, g2) || invoked >= jitInvocationsForMaglev || jitLinkOfForTest(r2, g2).in == 0 {
		t.Fatalf("g, called at every tenth iteration of w's loop, not compiled for w: %+v", e)
	}
}

// A long callee native callers may not call through its link -- an arrow
// that reads this, its link's frame none of its own -- whose calls Go makes
// for its native caller meanwhile, warms up so too, called seldom: its
// calls Go makes spend its budget, those the interpreter answers without
// a frame as well, and the call is decided once it
// is compiled, not settled as Go's while it warms up.
func TestJITLongArrowCalleeWarmsUp(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r, run := v8TierRuntimeForTest(t, `function C(){this.v=1;this.f=(x)=>{`+strings.Repeat(`x=(x*3+1)|0;`, jitLongRepeatsForTest(16))+`return (x+this.v)|0}}
		var c=new C();`+jitRareCallForTest(`c.f(i)`))
	f := r.global.getOwn(r.atoms.intern("c")).value().Object().getOwn(r.atoms.intern("f")).value().Object().fn().closure
	h := r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure
	// Register code: an arrow that reads this is no function native
	// callers call through its link with no code (jitRegTarget).
	if n := jitInlineSizeForTest(r, f); n <= ssa.MaxInline {
		t.Fatalf("f is %d instructions of register code", n)
	}
	for range jitInvocationsForMaglev {
		run(`h(4,0)`)
	}
	if !jitHasCodeOfForTest(r, h) {
		t.Fatal("h has no code")
	}
	for i := range 25 * (jitInvocationsForMaglev - 8) {
		run(`h(1,1)`)
		if jitHasCodeOfForTest(r, f) {
			t.Fatalf("f compiled at its %d. call", (i+1)/25)
		}
	}
	// Its caches' feedback changes in its first calls, which raises what
	// is left of its budget (jitRegWarmUp).
	for range 50 * jitInvocationsForMaglev {
		run(`h(1,1)`)
	}
	e := jitEntryOfForTest(r, f)
	if e == nil || !jitHasCodeOfForTest(r, f) {
		t.Fatalf("f not compiled once warm: %+v", e)
	}
	if he := jitEntryOfForTest(r, h); !slices.ContainsFunc(he.nativeCalls, func(x jitInline) bool { return x.cl == f }) {
		t.Fatalf("h does not call f natively: %+v", he.nativeCalls)
	}
}

// Code Go does not enter at its start for its short stretches (entrySlow)
// is still entered by on-stack replacement at a loop that has run a full
// budget of back edges in the existing tiers, as V8's code is: the
// judgement is of the calls Go enters it at, short ones, and a long
// invocation, whose loop proves it long, goes on natively. sum, judged
// slow, runs a call of a million iterations natively from its loop.
// DeltaBlue's forEach, judged slow on its many short calls, had run its
// plan's, thousands of constraints long, in the interpreter.
func TestJITSlowCodeEnteredByOSR(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	wv, err := want.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRuntime(t, Config{JIT: true})
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
	t.Parallel()
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
	// The register form inlines mark in each from the first compile, its
	// write's misses calls of Go there too (jitRegPlainSet's inlined
	// callee): the site, which no exit teaches, keeps its cases and its
	// misses Go's, which is what the test is of.
	if p := r.global.getOwn(r.atoms.intern("mark")).value().Object().fn().closure; !true && !jitSiteMegamorphicForTest(t, r, p, e, bytecode.OpSetProp, bytecode.RSetProp, bytecode.RAddProp, bytecode.RAddPropI) {
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
	t.Parallel()
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
	if p := r.global.getOwn(r.atoms.intern("look")).value().Object().fn().closure; !jitSiteMegamorphicForTest(t, r, p, e, bytecode.OpGetProp, bytecode.RGetProp) {
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

// jitLastInlinesForTest has the last compile of a function named name
// record the closures it inlines (ssa.Func's Inlined) in *into: a
// process-wide hook, which also keeps compiles out of the background, so
// that a test using it is never parallel.
func jitLastInlinesForTest(t *testing.T, name string, into *[]uintptr) {
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.ssaOptimized = func(fn *bytecode.Function, f *ssa.Func) {
			if fn.Name == name {
				*into = slices.Clone(f.Inlined)
			}
		}
	})
}

// megaGetterSrc has drive call look at each of twelve receivers, three of
// whose prototypes hold a getter of hit, look's read megamorphic; fail
// reads hit of a receiver whose getter throws.
const megaGetterSrc = `var data={hit:7}, getter={get hit(){if(this.p0===-1)throw new Error('hit of '+this.p1);return this.p0+this.p1+100}}, shapes=[];
					for(let k=0;k<12;k++){const o=Object.create(k%3==1?data:k%3==2?getter:Object.prototype);
						for(let j=0;j<=k;j++)o['p'+j]=j*k;if(k%4==0)o.hit=k;shapes.push(o)}
					var bad=Object.create(getter);bad.p0=-1;bad.p1=9;for(let j=2;j<12;j++)bad['p'+j]=j
					function look(o){const h=o.hit;return h===undefined?-1:h}
					function drive(n){let t=0;for(let i=0;i<n;i++)t+=look(shapes[i%12]);return t}
					function fail(){try{return look(bad)}catch(e){return e.message+'|'+e.stack.split(String.fromCharCode(10)).slice(0,3).join('|')}}`

// A callee whose read at a megamorphic site has met a getter on its
// receivers' prototypes is not inlined (jitMegaGetter): inlined, the read
// would have no frame for Go to call the getter on, and would leave native
// code at every receiver of the getter. drive does not inline look, by
// the Go table and the native handlers, marking and not, and the answers
// -- the getter's this, and the exception one receiver's getter throws,
// with its stack -- are the interpreter's.
func TestJITMegamorphicGetterNotInlined(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/marking=%v", native, marking), func(t *testing.T) {
				nativeForTest(t, native)
				if marking {
					jitMarkingForTest(t)
				}
				var inlined []uintptr
				jitLastInlinesForTest(t, "drive", &inlined)
				r, run := jitTierRuntimeForTest(t, 1, megaGetterSrc)
				for range 4 {
					run(`drive(96)`)
				}
				look := jitGlobalClosureForTest(r, "look")
				if !jitSiteMegamorphicForTest(t, r, look, r.jit.regLearnedOf(r.jitRegKeyOf(look)), bytecode.OpGetProp, bytecode.RGetProp) {
					t.Fatal("look's read is not megamorphic: no test of its rule")
				}
				jitNativeForTest(t, r, "drive")
				if slices.Contains(inlined, uintptr(unsafe.Pointer(look))) {
					t.Fatal("drive inlines look, whose megamorphic read meets a getter")
				}
				run(`drive(120)`)
				run(`fail()`)
			})
		}
	}
}

// A megamorphic site's read whose receivers' prototypes hold a getter is
// compiled as the site's other reads are -- the receiver's own table, the
// megamorphic cache, and Go's read from native code on a branch of its own
// (ssa's megamorphicByGo) -- and Go calls the getter there, with the
// receiver its this; the function is not inlined (jitMegaGetter), so that
// its code, called natively, has the frame Go calls the getter on.
// Compiled for the getter and the shapes it was found for, every other
// receiver had left look's own code, two entries in three; inlined, every
// receiver of the getter left. drive calls look at each of twelve
// receivers, three of whose prototypes hold the getter: by the Go table
// and the native handlers, marking and not, neither function leaves native
// code, look's code runs as drive's native callee and calls Go for the
// getter, and the answers -- the getter's this, and the exception one
// receiver's getter throws, with its stack -- are the interpreter's.
func TestJITMegamorphicAccessorReadsByGo(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/marking=%v", native, marking), func(t *testing.T) {
				nativeForTest(t, native)
				if marking {
					jitMarkingForTest(t)
				}
				var inlined []uintptr
				jitLastInlinesForTest(t, "drive", &inlined)
				r, run := jitTierRuntimeForTest(t, 1, megaGetterSrc)
				for range 4 {
					run(`drive(96)`)
				}
				look := jitGlobalClosureForTest(r, "look")
				if !jitSiteMegamorphicForTest(t, r, look, r.jit.regLearnedOf(r.jitRegKeyOf(look)), bytecode.OpGetProp, bytecode.RGetProp) {
					t.Fatal("look's read is not megamorphic: no test of its misses")
				}
				if slices.Contains(inlined, uintptr(unsafe.Pointer(look))) {
					t.Fatal("drive inlines look, whose megamorphic read meets a getter")
				}
				// look runs only as drive's native callee: its code is entered
				// by no call from Go (jitNativeForTest's entries).
				el, ed := jitRegLifeForTest(r, "look"), jitNativeForTest(t, r, "drive")
				if el == nil {
					t.Fatal("look was never compiled")
				}
				lh, lg, lc, lw := el.ssaStats.hosts, el.ssaStats.guards, el.ssaStats.calleeGoCalls, el.ssaStats.calleeWork
				dh, dg, interpreted := ed.ssaStats.hosts, ed.ssaStats.guards, r.jit.interpreted
				run(`drive(120)`)
				if n := el.ssaStats.hosts - lh + el.ssaStats.guards - lg + ed.ssaStats.hosts - dh + ed.ssaStats.guards - dg; n != 0 {
					t.Errorf("look or drive left native code %d times", n)
				}
				if n := r.jit.interpreted - interpreted; n != 0 {
					t.Errorf("the interpreter finished %d invocations", n)
				}
				// A getter that throws: the exception leaves at the call, Go
				// finishing it (goPending), its message and stack the
				// interpreter's.
				run(`fail()`)
				if el.ssaStats.calleeWork == lw {
					t.Error("look's code never ran as drive's native callee")
				}
				if el.ssaStats.calleeGoCalls == lc {
					t.Error("look's code called Go for no getter")
				}
			})
		}
	}
}

// A megamorphic site's read in an inlined callee is Go's at its miss too
// (ssa's megamorphic read), where it needs no frame (jitPlainRead): a data
// property its receiver or a prototype has, or none -- which had left
// native code at every such miss, the interpreter finishing the caller's
// invocation. look, inlined in drive, reads hit of 3,000 objects of as
// many shapes, over three prototypes, some with hit of their own, more
// than the megamorphic cache holds, so that its misses go on: by the Go
// table and the native handlers, marking and not, drive never leaves,
// each answer the interpreter's.
func TestJITMegamorphicInlinedReadsByGo(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/marking=%v", native, marking), func(t *testing.T) {
				nativeForTest(t, native)
				if marking {
					jitMarkingForTest(t)
				}
				var inlined []uintptr
				jitLastInlinesForTest(t, "drive", &inlined)
				// The interpreter runs look first, its cache giving up.
				r, run := jitTierRuntimeForTest(t, 200, `var a={hit:7}, b={hit:'b'}, objs=[];
					for(let k=0;k<3000;k++){const o=Object.create(k%3==1?a:k%3==2?b:Object.prototype);
						o['q'+k]=k;for(let j=0;j<10;j++)o['p'+j]=j;if(k%4==0)o.hit=k;objs.push(o)}
					function look(o){return o.hit}
					function drive(n){let t=0,s='';for(let i=0;i<n;i++){const h=look(objs[i%3000]);if(typeof h=='number')t+=h;else s+=h}return t+s.length}`)
				for range 4 {
					run(`drive(3000)`)
				}
				look := jitGlobalClosureForTest(r, "look")
				if !jitSiteMegamorphicForTest(t, r, look, r.jit.regLearnedOf(r.jitRegKeyOf(look)), bytecode.OpGetProp, bytecode.RGetProp) {
					t.Fatal("look's read is not megamorphic: no test of its misses")
				}
				if !slices.Contains(inlined, uintptr(unsafe.Pointer(look))) {
					t.Fatal("drive does not inline look")
				}
				ed := jitNativeForTest(t, r, "drive")
				dh, dg, interpreted := ed.ssaStats.hosts, ed.ssaStats.guards, r.jit.interpreted
				run(`drive(6000)`)
				if n := ed.ssaStats.hosts - dh + ed.ssaStats.guards - dg; n != 0 {
					t.Errorf("drive left native code %d times", n)
				}
				if n := r.jit.interpreted - interpreted; n != 0 {
					t.Errorf("the interpreter finished %d invocations", n)
				}
			})
		}
	}
}

// A megamorphic site's method read whose miss Go makes (ssa.GoCall) leaves
// the site's cache as the interpreter reads a method (get_prop_this, by
// cachedProp, whose entries are data): a getter's read of the method there
// had filled the cache as a property read's (cachedGet), its entry the
// getter's, which the interpreter then took for the method itself -- the
// getter's pair, an object no call could make: Intl's format, a getter of
// the bound function, read and called by test262's NumberFormat tests.
func TestJITMegamorphicMethodReadsByGo(t *testing.T) {
	t.Parallel()
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
	if !jitSiteMegamorphicForTest(t, r, cl, e, bytecode.OpGetPropThis, bytecode.RGetMethod, bytecode.RGetMethodW) {
		t.Fatal("call's method read is not megamorphic: no test of its misses")
	}
	run(`each(600)`)
	// What the site's cache answers for each receiver is what a read finds.
	site, name := jitMethodSiteForTest(t, r, cl)
	shapes := r.global.getOwn(r.atoms.intern("shapes")).value().Object()
	for i, s := range shapes.elemsOf() {
		got, ok := r.cachedProp(&cl.ic[site], s.Object(), name)
		want, err := r.getValueProp(s, name)
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
	defer holdCollectorNow(t)()
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
	reset := r.global.getOwn(r.atoms.intern("s")).value().Object().proto().getOwn(r.atoms.intern("reset")).value().Object().fn().closure
	// The closures register code inlines are its keep's (jitRegEntry's
	// keep, jitRegFeedback's Inline).
	inlined := jitRegOfForTest(r, jitGlobalClosure(r, "each")).keep.inlined
	if !slices.Contains(inlined, reset) {
		t.Fatal("each does not inline reset: no test of an inlined callee's literals")
	}
	hosts := e.ssaStats.hosts
	run(`each(600)`)
	if n := e.ssaStats.hosts - hosts; n > 2*600/abi.PoolCapacity+4 {
		t.Errorf("each left native code %d times", n)
	}
}
