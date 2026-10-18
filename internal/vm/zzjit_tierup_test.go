//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// jitTierSetup has f read x off its receivers' prototypes, which a site that
// knew nothing searches no receiver's own table for: A's, B's, C's and D's
// objects are of four shapes.
const jitTierSetup = `function A(){} A.prototype.x=1; function B(){this.y=0} B.prototype.x=2;
	function C(){this.z=0;this.w=0} C.prototype.x=3; function D(){this.q=0} D.prototype.x=4;
	var a=new A(), b=new B(), c=new C(), d=new D();
	function f(o,n){let s=0;for(let i=0;i<n;i++)s=(s+o.x)|0;return s}`

// jitTierRuntimeForTest runs setup in a runtime that compiles a function at
// its threshold's call, and in an interpreter, and returns a run that has
// both run src, failing on an answer that differs.
func jitTierRuntimeForTest(t *testing.T, threshold uint8, setup string) (*Runtime, func(src string)) {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitCallThreshold = threshold
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
