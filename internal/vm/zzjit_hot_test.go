//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// Functions of calls and no loop, compiled once hot (LowerSSAHot): a
// recursion, a mutual one, one constructing, one whose calls throw, each
// answer the interpreter's in every tier and under every stress setting.
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
// recursion in the tree tier, four times as long as run alone.
func TestJITHotCallsAnyOrder(t *testing.T) {
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
	cl := r.global.getOwn(r.atoms.intern("moves")).value().Object().fn().closure
	var e *jitEntry
	for i := 0; i < 20 && (e == nil || !e.hot); i++ {
		if _, err := r.Run(compileForTest(t, `run(second)`)); err != nil {
			t.Fatal(err)
		}
		e = r.jit.hint(cl)
	}
	if e == nil || !e.hot || e.ssa == nil {
		t.Fatalf("moves not compiled once hot in 20 runs: %+v", e)
	}
	entries, in := e.ssaStats.entries, e.link.in
	if _, err := r.Run(compileForTest(t, `run(second)`)); err != nil {
		t.Fatal(err)
	}
	// 4 * (2**13 - 1) calls of moves in all.
	if n := e.ssaStats.entries - entries + e.link.in - in; n < 4*8000 {
		t.Errorf("moves ran natively %d times of %d: slow %v notNative %v stats %+v", n, 4*(1<<13-1), e.entrySlow, e.notNative, e.ssaStats)
	}
}

// While the collector marks, native code makes its native calls, as it
// does when it does not: a recursion runs on natively, no call of Go, and
// is not judged slow. When Go made the calls then (abi.GoOpMarking), a
// recursion's had been judged slow on its leaves -- under a collector that
// marked often, in TestJITHotCallsAnyOrder on Windows -- and the tree tier
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
	cl := r.global.getOwn(r.atoms.intern("moves")).value().Object().fn().closure
	var e *jitEntry
	for i := 0; i < 20 && (e == nil || !e.hot || e.reoptPending() || len(e.nativeCalls) == 0); i++ {
		if _, err := r.Run(compileForTest(t, `run()`)); err != nil {
			t.Fatal(err)
		}
		e = r.jit.hint(cl)
	}
	if e == nil || !e.hot || e.ssa == nil || e.entrySlow || len(e.nativeCalls) == 0 {
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
	if r.jit.goCalls != made || e.entrySlow || r.jit.hint(cl) != e {
		t.Errorf("moves, its calls made by Go while the collector marked (%d), judged slow %v (current %v): %+v",
			r.jit.goCalls-made, e.entrySlow, r.jit.hint(cl) == e, e.ssaStats)
	}
}

// A function of calls and no loop is not compiled hot where each of its
// calls is of a function native callers do not call (notNative): native
// code Go entered would make each through Go, which the tree tier makes
// itself (jitCallsNatively). AWFY's Json's readChar, all of whose calls are
// of read, which leaves for Go at each substring, had taken it 5% longer.
// A call of itself, or of one native callers call or that has no code yet,
// would be native.
func TestJITHotCallsOfGoOnly(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	jitFewCallsForTest(r)
	const setup = `
		var o = { s: 'abcdefgh', i: 0, cur: 'a' }
		function read(o) { let k = 0; for (let j = 0; j < 1; j++) k += j; o.i = (o.i + 1) & 7; o.cur = o.s.substring(o.i, o.i + 1) }
		function readChar(o, c) { if (o.cur !== c) return false; read(o); return true }
		function self(n) { return n === 0 ? 0 : self(n - 1) + 1 }
		function fresh() { return later() }
		function later() { return 1 }
		for (let i = 0; i < 30; i++) { read(o); readChar(o, o.cur); self(3); fresh() }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	closure := func(name string) *closure {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	}
	re := r.jit.entryFor(closure("read").fn)
	if re == nil || re.ssa == nil {
		t.Fatalf("read has no code: %+v", re)
	}
	re.notNative = true
	if r.jitCallsNatively(closure("readChar")) {
		t.Error("readChar's call of read, which native callers do not call, taken for a native one")
	}
	re.notNative = false
	if !r.jitCallsNatively(closure("readChar")) {
		t.Error("readChar's call of read, which native callers call, not taken for a native one")
	}
	if !r.jitCallsNatively(closure("self")) {
		t.Error("a recursion not taken for a native call")
	}
	if e := r.jit.entryFor(closure("later").fn); e != nil {
		t.Skipf("later was compiled: %+v", e)
	}
	if !r.jitCallsNatively(closure("fresh")) {
		t.Error("a call of a function with no code yet not taken for a native one")
	}
}
