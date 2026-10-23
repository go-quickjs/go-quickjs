//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// cursorSetup's callees, called natively by loops, step cursors: sum's
// for...of over an array (native steps), over a Map's values and a
// generator (Go's); first's breaks out, its iterator closed; pair's
// parameter is an array pattern; picky's throws inside its loop, the
// iterator closed by the throw's unwinding; early's returns from inside
// its loop, which closes it; keys' is a for...in.
const cursorSetup = `
	var closed = 0
	function* gen(n) { try { for (let i = 0; i < n; i++) yield i } finally { closed++ } }
	function sum(it) { let s = 0; for (const v of it) s += v; return s }
	function first(it) { for (const v of it) if (v > 1) return v; return -1 }
	function pair([a, b = 5]) { return a * 10 + b }
	function picky(it, bad) { let s = 0; for (const v of it) { if (v === bad) throw new RangeError('bad ' + v); s += v } return s }
	function keys(o) { let s = ''; for (const k in o) s += k; return s }
	function run(n) {
		const a = [1, 2, 3, 4], m = new Map([[1, 1], [2, 2], [3, 3]])
		let s = 0, errs = 0, ks = ''
		for (let i = 0; i < n; i++) {
			s += sum(a) + sum(m.values()) + sum(gen(3)) + first(a) + first(gen(5)) + pair([i]) + pair([i, 1])
			try { s += picky(gen(4), i % 7 === 3 ? 2 : 9) } catch (e) { errs++ }
			ks = keys({ x: 1, y: i })
		}
		return [s, errs, closed, ks].join()
	}`

// A native callee's cursors -- for...of's, for...in's, an array pattern's
// -- are its frame's operands, which Go steps from native code on the
// frame of its level (jitGoCallNested), or native code steps itself over
// an array; where the callee leaves, Go makes its frame with the cursor in
// it, which a throw's unwinding or a return closes as the interpreter's.
// Such callees -- 75 of npm's own code -- had been refused for native
// callers. An inlined callee has none (ssa's InlineCalls). Each answer,
// the iterators' closings counted, is the interpreter's. (first's return
// from inside its loop, which closes the cursor, and keys' and pair's
// cursors, which Go makes at each call, leave or call Go at every call,
// and their native callers stop calling them natively, as for any callee
// that does not pay: sum's loop over an array is the one that stays.)
func TestJITCursorCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, cursorSetup)
	for range 6 {
		both.agree(t, `run(100)`)
	}
	r := both.jit
	// pair has no loop: its code is for native callers alone, from the
	// lowering for them, which had refused its cursor.
	if e := jitEntryForTest(t, r, "pair"); !e.ssaCallee {
		t.Errorf("pair's code is not for native callers: %+v", e)
	}
	for _, name := range []string{"sum"} {
		e := jitEntryForTest(t, r, name)
		in := e.link.in
		both.agree(t, `run(100)`)
		if e.link.in-in < 50 {
			t.Errorf("%s was called natively %d times", name, e.link.in-in)
		}
	}
	jitAgreesForTest(t, cursorSetup, `run(10)`, `run(30)`, `closed`)
}
