//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// Functions of calls and no loop, compiled once hot (LowerRegHot): a
// recursion, a mutual one, one constructing, one whose calls throw, each
// answer the interpreter's under every stress setting.
func TestJITHotCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const piles = [[], [], []]
		function push(p, d) { piles[p].push(d) }
		function pop(p) { return piles[p].pop() }
		function move1(from, to) { push(to, pop(from)) }
		function moves(n, from, to) { if (n === 1) { move1(from, to); return 1 } const other = 3 - from - to; return moves(n - 1, from, other) + moves(1, from, to) + moves(n - 1, other, to) }
		function isEven(n) { return n === 0 ? true : isOdd(n - 1) }
		function isOdd(n) { return n === 0 ? false : isEven(n - 1) }
		class Pt { constructor(x) { this.x = x } }
		function mk(n) { return n === 0 ? null : { p: new Pt(n), next: mk(n - 1) } }
		function bad(n) { if (n === 0) throw new RangeError('n'); return bad(n - 1) }
		const log = []
		for (let r = 0; r < 6; r++) {
			for (let i = 8; i > 0; i--) piles[0].push(i)
			log.push(moves(8, 0, 2), piles[2].length)
			piles[2].length = 0
			log.push(isEven(25 + r), mk(30).next.p.x)
			try { bad(20) } catch (e) { log.push(e.name) }
		}
		log.join()`
	jitDifferential(t, src)
}

// A recursion of no loop runs natively whichever function first made the
// call site that starts it: a harness's call, decided for one benchmark,
// taking the next. It had been refused once entered from Go -- a function
// of calls and no loop -- and AWFY's Towers, run after Sieve, ran its
// recursion in the interpreter, four times as long as run alone.
func TestJITHotCallsAnyOrder(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		var first = { benchmark() { let s = 0; for (let i = 0; i < 2000; i++) s += i; return s } }
		var moved = 0
		function moveOne() { moved++ }
		function moves(n) { if (n === 0) return; moves(n - 1); moveOne(); moves(n - 1) }
		var second = { benchmark() { moved = 0; moves(12); return moved } }
		function run(b) { let s = 0; for (let k = 0; k < 4; k++) s += b.benchmark(); return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := r.Run(compileForTest(t, `run(first)`)); err != nil {
			t.Fatal(err)
		}
	}
	// Until moves is compiled once hot: within a few runs, later where the
	// collector runs often, its code for native callers made again
	// meanwhile.
	var e *jitEntry
	for i := 0; i < 20 && e == nil; i++ {
		if _, err := r.Run(compileForTest(t, `run(second)`)); err != nil {
			t.Fatal(err)
		}
		e = jitHotForTest(r, "moves")
	}
	if e == nil {
		t.Fatalf("moves not compiled once hot in 20 runs: %+v", e)
	}
	uses := jitNativeUsesForTest(r, "moves")
	if _, err := r.Run(compileForTest(t, `run(second)`)); err != nil {
		t.Fatal(err)
	}
	// 4 * (2**13 - 1) calls of moves in all.
	if n := jitNativeUsesForTest(r, "moves") - uses; n < 4*8000 {
		t.Errorf("moves ran natively %d times of %d: slow %v notNative %v stats %+v", n, 4*(1<<13-1), e.entrySlow, e.notNative, e.ssaStats)
	}
}

// While the collector marks, native code makes its native calls, as it
// does when it does not: a recursion runs on natively, no call of Go, and
// is not judged slow. When Go made the calls then (abi.GoOpMarking), a
// recursion's had been judged slow on its leaves -- under a collector that
// marked often, in TestJITHotCallsAnyOrder on Windows -- and the interpreter
// ran it.
func TestJITHotCallsWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	marking := jitMarkingForTest(t)
	*marking = 0
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		var moved = 0
		function moveOne() { moved++ }
		function moves(n) { if (n === 0) return; moves(n - 1); moveOne(); moves(n - 1) }
		function run() { let s = 0; for (let k = 0; k < 4; k++) { moved = 0; moves(10); s += moved } return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	var e *jitEntry
	for i := 0; i < 20 && (e == nil || e.reoptPending() || len(e.nativeCalls) == 0); i++ {
		if _, err := r.Run(compileForTest(t, `run()`)); err != nil {
			t.Fatal(err)
		}
		e = jitHotForTest(r, "moves")
	}
	if e == nil || e.entrySlow || len(e.nativeCalls) == 0 {
		t.Fatalf("moves not compiled once hot to call itself natively: %+v", e)
	}
	*marking = 1
	made := r.jit.goCalls
	for range 4 {
		v, err := r.Run(compileForTest(t, `run()`))
		if err != nil || v.Number() != 4*(1<<10-1) {
			t.Fatalf("run() = %v, %v", v, err)
		}
	}
	if now := jitHotForTest(r, "moves"); r.jit.goCalls != made || e.entrySlow || now != e {
		t.Errorf("moves, its calls made by Go while the collector marked (%d), judged slow %v (current %v): %+v",
			r.jit.goCalls-made, e.entrySlow, now == e, e.ssaStats)
	}
}

// jitHotForTest is the entry of the global function name's code compiled
// once hot (LowerRegHot), nil where it has none: or, where r runs register
// code, its register code's state, the code compiled as the register
// form's compiles a function of calls and no loop (LowerRegHot).
func jitHotForTest(r *Runtime, name string) *jitEntry {
	return jitRegLifeForTest(r, name)
}
