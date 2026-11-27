//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// jitAnswersForTest is a JIT runtime of V8's budgets for setup, and a
// check of src's answer, as a string, against want and the interpreter's.
func jitAnswersForTest(t *testing.T, setup string) (*Runtime, func(src, want string)) {
	t.Helper()
	return jitAnswersWithForTest(t, setup, nil)
}

// jitAnswersWithForTest is jitAnswersForTest, each runtime given install's
// globals first (jitTierRuntimeWithForTest).
func jitAnswersWithForTest(t *testing.T, setup string, install func(*Runtime)) (*Runtime, func(src, want string)) {
	t.Helper()
	r, run := jitTierRuntimeWithForTest(t, jitInvocationsForMaglev, setup, install)
	r.jitState()
	return r, func(src, want string) {
		t.Helper()
		v, err := r.Run(compileForTest(t, "String("+src+")"))
		if err != nil || v.String().Go() != want {
			t.Fatalf("%s = %v, %v; want %s", src, v, err, want)
		}
		run(src)
	}
}

// A loop whose native turns are little more than a call of Go with a frame
// -- here a host function's (framedCall), at microbench.js's date_parse
// Date's methods; Number.isInteger and Date.now, which this had called, are
// leaves now, called without one -- costs more natively than in the
// interpreter, whose own calls of built-ins are cheaper than calls of Go
// from native code. Its calls of Go are judged as they are made
// (jitRegRunGoProfit), not only at the end of the stretch, which for a loop
// entered at its header, by on-stack replacement, was the end of the loop:
// the code leaves after the call that finds them not to pay, the frame goes
// back to the interpreter at that loop, and Go does not enter the code
// again at its start nor at the loop (loopSlow). Before, every invocation's
// loop ran natively, a third slower than without the JIT. Each answer is
// Node's.
func TestJITCallsOfGoAtLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	r, want := jitAnswersWithForTest(t, `function integers(n) { let s = 0; for (let i = 0; i < n; i++) s += framedCall('xy', i & 1) === 120 ? 1 : 2; return s }`, jitFramedCallForTest)
	want(`integers(200000)`, "300000")
	e := jitNativeForTest(t, r, "integers")
	// Register code's frame goes back to the register interpreter, which
	// runs every frame code leaves.
	if !e.loopSlow {
		t.Fatalf("integers' calls of Go judged to pay (loopSlow %v)", e.loopSlow)
	}
	if e.ssaStats.runGoCalls > 2*jitSSAProbe {
		t.Fatalf("integers made %d calls of Go natively before it left", e.ssaStats.runGoCalls)
	}
	calls, entries := e.ssaStats.goCalls, e.ssaStats.entries
	want(`integers(300000)`, "450000")
	if e.ssaStats.goCalls != calls || e.ssaStats.entries != entries {
		t.Fatalf("integers, judged not to pay, was entered again: %d calls of Go, %d entries", e.ssaStats.goCalls-calls, e.ssaStats.entries-entries)
	}
}

// A loop whose stretches end in an exit at every turn -- a using
// declaration's resources, which Go adds and disposes of -- was entered at
// its header by on-stack replacement at every budget, its code judged slow
// only at its entries at the start, ran a turn, and left the frame to the
// interpreter until the next budget. Such stretches are judged too
// (jitRegStretchWork's osr): Go no longer enters the code at the loop
// (loopSlow), and each frame left goes back to the interpreter at the loop;
// so too a loop over a generator's values, where its steps are calls of Go
// -- a generator whose locals a closure captures, which no step resumes
// natively (jitRegGenCallable). Each answer is Node's.
func TestJITLoopExitsAtEachTurn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	r, want := jitAnswersForTest(t, `
		function viaUsing(n) {
			const res = { n: 0, [Symbol.dispose]() { this.n++ } }
			let s = 0
			for (let i = 0; i < n; i++) { using u = res; s = (s + i) | 0 }
			return s + res.n * 3
		}
		function* count(n) { for (let i = 0; i < n; i++) yield i; return () => n }
		function viaGen(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of count(10)) s = (s + v * j) | 0; return s }`)
	interpreted := r.jit.interpreted
	want(`viaUsing(200000)`, "-1474336480")
	e := jitNativeForTest(t, r, "viaUsing")
	if !e.loopSlow {
		t.Fatalf("viaUsing's loop, leaving at each turn, judged to pay: %d stretches entered at the loop, %d work", e.ssaStats.osrStretches, e.ssaStats.osrWork)
	}
	if n := r.jit.interpreted - interpreted; n > 2*jitSSAProbe {
		t.Fatalf("viaUsing's frame given to the interpreter %d times", n)
	}
	entries := e.ssaStats.entries
	want(`viaUsing(300000)`, "2051077040")
	if e.ssaStats.entries != entries {
		t.Fatalf("viaUsing, judged not to pay at its loop, entered %d times", e.ssaStats.entries-entries)
	}
	want(`viaGen(30000)`, "-1225511480")
	if e := jitNativeForTest(t, r, "viaGen"); !e.loopSlow {
		t.Fatalf("viaGen's steps judged to pay (loopSlow %v)", e.loopSlow)
	}
	want(`viaGen(20000)`, "409615408")
}

// A generator whose code is judged slow -- each resumption a stretch of a
// few instructions to the next yield -- is resumed in the interpreter, and
// the resumptions look at the code once for each jitSSAProbe more
// (jitResumeSkip), counting them all toward Go's retrying it
// (jitRegRetrySlow): looked at at each, a loop over the generator's values
// had spent a twentieth of its time there. The generator's locals a
// closure captures, so that no step resumes it natively
// (jitRegGenCallable):
// Go resumes it at each. Each answer is Node's.
func TestJITSlowResumptionsLookSeldom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	r, want := jitAnswersForTest(t, `
		function* count(n) { for (let i = 0; i < n; i++) yield i; return () => n }
		function total(n) { let s = 0; for (const v of count(n)) s = (s + v * 3) | 0; return s }`)
	for range 50 {
		want(`total(1000)`, "1498500")
	}
	e := jitNativeForTest(t, r, "count")
	if !e.entrySlow {
		t.Fatalf("count's code, a few instructions a resumption, judged to pay: %+v", e.ssaStats)
	}
	looks, calls, backoff := r.jit.slowResumes, e.slowCalls, e.slowBackoff
	// Each answer is found twice: the call runs once to check it, and once
	// more as the interpreter's is compared (jitTierRuntimeForTest).
	const n = 2 * 100001
	want(`total(100000)`, "2114948112")
	if l := r.jit.slowResumes - looks; l == 0 || l > n/jitSSAProbe+4 {
		t.Fatalf("%d resumptions looked at count's code %d times", n, l)
	}
	if e.slowBackoff != backoff {
		return // retried meanwhile: the count began again
	}
	if c := e.slowCalls - calls; c < n-2*jitSSAProbe || c > n+2*jitSSAProbe {
		t.Fatalf("%d resumptions counted %d toward a retry", n, c)
	}
}

// Code compiled again is judged afresh at its loops too (jitRegCached): a
// loop whose code was found slow there (loopSlow) -- its stretches each a
// turn, left at an inlined callee's exit, the inlining given up after 16 of
// them while the 64 stretches found the loop slow -- had Go enter the code
// compiled without the inlining at no loop until it was retried, after
// 1,024 looks: microbench's args_read ran 996,348 of its 1,048,576 turns
// in the interpreter, its callee's native code called through Go at each.
// Code compiled again only to give up the entries kept for a frame does
// as it did, and keeps its judgement.
func TestJITLoopJudgedAfreshCompiledAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	// compileAgain compiles sum anew, as a compile for what its code
	// learned would, while its frame runs in the interpreter.
	compileAgain := func(rt *Runtime) {
		rt.global.setOwnRaw(rt.atoms.intern("compileAgain"), rt.NewFunction("compileAgain", 0,
			func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				if rt.jit != nil {
					cl := rt.global.getOwn(rt.atoms.intern("sum")).value().Object().fn().closure
					if e := rt.jitRegCompileFor(cl, cl.reg, rt.jitRegKeyOf(cl), false); e == nil || e.code == nil {
						return Undefined, rt.throwTypeError("sum not compiled again")
					}
				}
				return Undefined, nil
			}), propDefault)
	}
	r, want := jitAnswersWithForTest(t, `
		function sum(n, again) {
			var s = 0;
			for (var j = 0; j < n; j++) {
				if (j === again)
					compileAgain();
				s = (s + j * 3) | 0;
			}
			return s;
		}`, compileAgain)
	want(`sum(100000, -1)`, "2114948112")
	life := jitNativeForTest(t, r, "sum")
	// Judged slow at its start and at its loop, as its stretches judge
	// code that does not pay for them (jitRegStretchWork).
	life.entrySlow, life.loopSlow = true, true
	entries := life.ssaStats.entries
	want(`sum(100000, -1)`, "2114948112")
	if life.ssaStats.entries != entries {
		t.Fatalf("sum, judged slow, entered %d times", life.ssaStats.entries-entries)
	}
	// Compiled again half way through its loop: entered at the loop for
	// the rest of it, in each of the two calls.
	life.entrySlow, life.loopSlow = true, true
	want(`sum(100000, 50000)`, "2114948112")
	if life.ssaStats.entries == entries {
		t.Fatal("sum, compiled again, not entered at its loop")
	}
	if life.loopSlow {
		t.Fatal("sum compiled again: still judged slow at its loop")
	}
}
