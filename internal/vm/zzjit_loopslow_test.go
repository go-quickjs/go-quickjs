//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// jitAnswersForTest is a JIT runtime of V8's budgets for setup, and a
// check of src's answer, as a string, against want and the interpreter's.
func jitAnswersForTest(t *testing.T, setup string) (*Runtime, func(src, want string)) {
	t.Helper()
	r, run := jitTierRuntimeForTest(t, jitInvocationsForMaglev, setup)
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

// A loop whose native turns are little more than a call of Go -- here
// Number.isInteger, at microbench.js's date_now and date_parse Date.now
// and Date's methods -- costs more natively than in the tree tier, whose
// own calls of built-ins are cheaper than calls of Go from native code.
// Its calls of Go are judged as they are made (jitRunGoProfit), not only
// at the end of the stretch, which for a loop entered at its header, by
// on-stack replacement, was the end of the loop: the code leaves after the
// call that finds them not to pay, the frame goes back to the tree that
// entered it, at that loop (tryJITTreeLoopEnabled), and Go does not enter
// the code again at its start nor at the loop (loopSlow). Before, every
// invocation's loop ran natively, a third slower than without the JIT.
// Each answer is Node's.
func TestJITCallsOfGoAtLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, want := jitAnswersForTest(t, `function integers(n) { let s = 0; for (let i = 0; i < n; i++) s += Number.isInteger(i / 2) ? 1 : 2; return s }`)
	back := r.jit.backToTree
	want(`integers(200000)`, "300000")
	e := jitNativeForTest(t, r, "integers")
	if !e.loopSlow || r.jit.backToTree == back {
		t.Fatalf("integers' calls of Go judged to pay (loopSlow %v), or its frame not given back to its tree (%d)", e.loopSlow, r.jit.backToTree-back)
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
// only at its entries at the start (tryJITAt), ran a turn, and left the
// frame to the bytecode interpreter until the next budget, its tree, one
// of handlers, not taking it back. Such stretches are judged too
// (jitSSAProfit's osr): Go no longer enters the code at the loop
// (loopSlow), and each frame left goes back to its tree at the loop; so
// too a loop over a generator's values, whose cursor makes its tree one of
// handlers too. Each answer is Node's.
func TestJITLoopExitsAtEachTurn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, want := jitAnswersForTest(t, `
		function viaUsing(n) {
			const res = { n: 0, [Symbol.dispose]() { this.n++ } }
			let s = 0
			for (let i = 0; i < n; i++) { using u = res; s = (s + i) | 0 }
			return s + res.n * 3
		}
		function* count(n) { for (let i = 0; i < n; i++) yield i }
		function viaGen(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of count(10)) s = (s + v * j) | 0; return s }`)
	back, interpreted := r.jit.backToTree, r.jit.interpreted
	want(`viaUsing(200000)`, "-1474336480")
	e := jitNativeForTest(t, r, "viaUsing")
	if !e.loopSlow {
		t.Fatalf("viaUsing's loop, leaving at each turn, judged to pay: %d stretches entered at the loop, %d work", e.ssaStats.osrStretches, e.ssaStats.osrWork)
	}
	if r.jit.backToTree == back {
		t.Fatal("viaUsing's frame did not go back to its tree")
	}
	if n := r.jit.interpreted - interpreted; n > 2*jitSSAProbe {
		t.Fatalf("viaUsing's frame given to the interpreter %d times", n)
	}
	entries := e.ssaStats.entries
	want(`viaUsing(300000)`, "2051077040")
	if e.ssaStats.entries != entries {
		t.Fatalf("viaUsing, judged not to pay at its loop, entered %d times", e.ssaStats.entries-entries)
	}
	back = r.jit.backToTree
	want(`viaGen(30000)`, "-1225511480")
	if e := jitNativeForTest(t, r, "viaGen"); !e.loopSlow || r.jit.backToTree == back {
		t.Fatalf("viaGen's steps judged to pay (loopSlow %v), or its frame not given back to its tree (%d)", e.loopSlow, r.jit.backToTree-back)
	}
	want(`viaGen(20000)`, "409615408")
}

// A frame that native code entered at its start, and left to the
// interpreter, goes on in its tree from the next loop header
// (runTreeFrom), a tree of handlers or for...of loops too (handledTree),
// as the tree would have run it from the start: an exception thrown in it
// is caught by the handler in force, the iterators of the loops it leaves
// are closed, as a break closes one -- each return method called -- and a
// using declaration's resource is disposed of at each turn. Each function
// is compiled on calls whose loops do arithmetic alone, natively, and
// then called to do the rest, which leaves native code: at a call of Go
// that finds them not to pay, or at an exit for the resource. Each answer
// is Node's.
func TestJITHandlerTreeGoesOn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, want := jitAnswersForTest(t, `
		function mixed(n, k) {
			const log = { closed: 0 }
			const five = { [Symbol.iterator]() { let i = 0; return { next() { return { value: i, done: i++ >= 5 } }, return() { log.closed++; return {} } } } }
			let s = 0, errs = 0
			for (let j = 0; j < n; j++) {
				if (j < k) {
					s = (s * 31 + j) | 0
					s = (s ^ (s >>> 7)) | 0
					continue
				}
				try {
					for (const v of five) {
						if (v == 3) break
						if (j % 1000 == 999 && v == 2) throw new Error('x' + j)
						s = (s + v + (Number.isInteger(j / 3) ? 1 : 0)) | 0
					}
				} catch (e) { errs++; s = (s + e.message.length) | 0 }
			}
			return s + ',' + errs + ',' + log.closed
		}
		function viaUsing(n, k) {
			const res = { n: 0, [Symbol.dispose]() { this.n++ } }
			let s = 0
			for (let i = 0; i < n; i++) {
				if (i >= k) { using u = res; s = (s + i) | 0 }
				else s = (s * 7 + i) | 0
			}
			return s + res.n * 3
		}`)
	for range 3 {
		want(`mixed(3000, 3000)`, "-1388502024,0,0")
		want(`viaUsing(3000, 3000)`, "-2027996068")
	}
	jitNativeForTest(t, r, "mixed")
	jitNativeForTest(t, r, "viaUsing")
	goneOn := r.jit.treeGoneOn
	want(`mixed(30000, 0)`, "120099,30,30000")
	if r.jit.treeGoneOn == goneOn {
		t.Fatal("mixed's invocation did not go on in its tree")
	}
	goneOn = r.jit.treeGoneOn
	want(`viaUsing(200000, 0)`, "-1474336480")
	if r.jit.treeGoneOn == goneOn {
		t.Fatal("viaUsing's invocation did not go on in its tree")
	}
}

// A generator whose code is judged slow -- each resumption a stretch of a
// few instructions to the next yield -- is resumed in its tree, and the
// resumptions look at the code once for each jitSSAProbe more (jitResume's
// jitResumeSkip), counting them all toward Go's retrying it
// (jitRetrySlow): looked at at each, a loop over the generator's values
// had spent a twentieth of its time there. Each answer is Node's.
func TestJITSlowResumptionsLookSeldom(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r, want := jitAnswersForTest(t, `
		function* count(n) { for (let i = 0; i < n; i++) yield i }
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
