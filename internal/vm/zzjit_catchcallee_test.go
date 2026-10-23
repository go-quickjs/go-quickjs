//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// catchCalleeSetup's callees have try statements, called natively by a
// loop: parse's catch takes a throw from a call of Go (JSON.parse), safe's
// one from a native callee, guarded's one from its own throw statement (a
// jump to its handler); fin's finally clause runs on a return and on a
// normal completion; nested's finally runs on a throw its outer catch
// takes; rethrow's catch throws again, to its caller's catch; traced
// returns the stack of the error its callee threw, whose frames Go made.
const catchCalleeSetup = `
	function thrower(i) { if (i % 7 === 3) throw new Error('e' + i); return i }
	function parse(s) { try { return JSON.parse(s).v } catch (e) { return e.name.length } }
	function safe(i) { try { return thrower(i) * 2 } catch (e) { return e.message.length } }
	function guarded(i) { try { if (i % 5 === 0) throw i; return i } catch (e) { return -e } }
	function fin(i, log) { try { if (i & 1) return i; log.n++ } finally { log.f++ } return -i }
	function nested(i) { try { try { thrower(i) } finally { i += 100 } } catch (e) { i += 1000 } return i }
	function rethrow(i) { try { thrower(i) } catch (e) { if (i % 2) throw e; return 0 } return 1 }
	function traced(i) { try { thrower(i) } catch (e) { return e.stack } return '' }
	function run(n) {
		const log = { n: 0, f: 0 }
		let s = 0, errs = 0, stacks = ''
		for (let i = 0; i < n; i++) {
			s += parse(i % 3 ? '{"v":' + i + '}' : '{') + safe(i) + guarded(i) + fin(i, log) + nested(i)
			try { s += rethrow(i) } catch (e) { errs++ }
			if (i === 3 || i === n - 4) stacks += traced(i)
		}
		return [s, errs, log.n, log.f, stacks].join()
	}`

// A callee with exception handlers is called natively: a frame Go makes of
// it where it leaves has the handlers in force there (jitHandlers), which
// the interpreter's unwinding of a throw -- from a call of Go, a native
// callee, Go's own operation -- finds, and finally clauses run on every
// completion. It is not inlined, an inlined frame having no handlers of its
// own (ssa's InlineCalls). Such callees -- 116 of the functions of npm's
// own code native callers could take, 43 of Octane's -- had been Go's to
// call. Each answer, the stacks included, is the interpreter's.
func TestJITCatchCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, catchCalleeSetup)
	for range 3 {
		both.agree(t, `run(200)`)
	}
	r := both.jit
	jitNativeForTest(t, r, "run")
	for _, name := range []string{"parse", "safe", "guarded", "fin", "nested", "rethrow"} {
		e := jitNativeOrCalleeForTest(t, r, name)
		in := e.link.in
		both.agree(t, `run(200)`)
		if e := jitNativeOrCalleeForTest(t, r, name); e.link.in-in < 100 {
			t.Errorf("%s was called natively %d times of 200", name, e.link.in-in)
		}
	}
}

// Compiled at their first calls, the callees leave at every point their
// code was not compiled for -- under the stress settings, at every poll
// and deoptimization too -- and each answer is the interpreter's.
func TestJITCatchCalleesAgree(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitAgreesForTest(t, catchCalleeSetup, `run(30)`, `run(60)`, `run(30)`)
}

// A return through a finally clause returns natively, the clause run
// first, where no other finally clause is in force around it, which the
// return would run next, and no loop's cursor is beneath the completion
// record, which the return would close (ir's Rethrow, When): find returns
// from inside its loop, its clause counting; twice's inner return goes
// through two clauses, and inLoop's from a for...of, whose iterator a
// return closes -- both the interpreter's still. Each had deoptimized at
// every such return, and find, at its every call, was compiled no more.
func TestJITReturnThroughFinally(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, `
		var count = 0, log = []
		function find(a, k) { for (let i = 0; i < a.length; i++) { try { if (a[i] === k) return i } finally { count++ } } return -1 }
		function twice(x) { try { try { if (x & 1) return x } finally { log.push('in' + x) } } finally { log.push('out' + x) } return -x }
		function* gen() { try { yield 1; yield 2; yield 3 } finally { log.push('closed') } }
		function inLoop(x) { for (const v of gen()) { try { if (v === x) return v * 10 } finally { log.push('v' + v) } } return 0 }
		function run(n) { const a = [5, 6, 7, 8, 9]; let s = 0; for (let i = 0; i < n; i++) s += find(a, 5 + (i % 6)); return s + ':' + count }
		function runOther(n) { let s = 0; for (let i = 0; i < n; i++) s += twice(i) + inLoop(i % 4); return s + ':' + log.length + ':' + log.slice(-9).join() }`)
	for range 3 {
		both.agree(t, `run(300)`, `runOther(30)`)
	}
	e := jitEntryForTest(t, both.jit, "find")
	if e.deopts > 2 || e.invalid || e.link.in < 500 {
		t.Errorf("find deoptimized %d times, invalid %v, called natively %d times", e.deopts, e.invalid, e.link.in)
	}
}

// jitEntryForTest is the entry of the global name's function, with code,
// found by its function: one only native callers call has no hint.
func jitEntryForTest(t *testing.T, r *Runtime, name string) *jitEntry {
	t.Helper()
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil || e.link == nil {
		t.Fatalf("%s has no code: %+v", name, e)
	}
	return e
}
