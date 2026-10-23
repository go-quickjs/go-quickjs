//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// handlerSetup's functions read, in their catch and finally clauses, locals
// their try statements assign before and after the call that throws, from
// an inlined callee, a native call or a call Go makes, in nested try
// statements and loops: what a handler reads is live wherever it is in
// force, and only that (compile's liveLocals) -- every exit inside a try
// writes those locals, whose values the handler, run by the interpreter or
// natively, reads; a local the handler does not read, dead past its
// iteration's try, is no frame state's past it.
const handlerSetup = `
	function thrower(i) { if (i % 5 === 3) throw new RangeError('t' + i); return i }
	var holder = { f: thrower }
	function caught(n) {
		let r = 0
		for (let i = 0; i < n; i++) {
			let a = 1, b = 2, c = 3
			try { a = i; b = thrower(i) * 2; c = a + b } catch (e) { r += a * 100 + b * 10 + c + e.message.length }
		}
		return r
	}
	function finals(n) {
		let r = ''
		for (let i = 0; i < n; i++) {
			let a = 0, s = 'x'
			try { try { a = i; s = 's' + holder.f(i) } finally { r += a + ':' + s.length + ',' } } catch (e) { r += e.message }
		}
		return r
	}
	function nested(n) {
		let r = 0
		for (let i = 0; i < n; i++) {
			let a = i, o = { v: i }
			try { try { a += thrower(i) } catch (e) { a += 1000; thrower(i + 1 === 3 ? 3 : 0) } } catch (e2) { r += a + o.v }
			r += a
		}
		return r
	}
	function deadAfter(n) {
		let r = 0
		for (let i = 0; i < n; i++) { try { const t = { x: i, y: i * 2 }; r += t.x + t.y + thrower(i) } catch (e) { r += 7 } }
		return r
	}
	function partial(n) {
		let r = 0
		for (let i = 0; i < n; i++) { try { const t = { x: i, y: thrower(i) }; r += t.x + t.y } catch (e) { r += 7 } }
		return r
	}
`

// Every answer is the interpreter's, under every stress setting: the
// handlers' locals as the code left them at each exit.
func TestJITHandlerLiveness(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 8 {
		rounds = append(rounds, `caught(20)`, `finals(20)`, `nested(20)`, `deadAfter(20)`, `partial(20)`)
	}
	for _, c := range []jitStressConfig{{}, {threshold: true, budget: 3}, {threshold: true, budget: 2, deopt: 3}} {
		r := jitAgreesForTestWith(t, c, handlerSetup, rounds...)
		if c == (jitStressConfig{}) {
			// The object deadAfter makes in its try, which no handler reads,
			// is not made: dead past the iteration's try, it is no loop
			// header's.
			if e := jitCallbackEntry(r, "deadAfter"); e == nil || e.ssa == nil || e.ssaVirtuals == 0 {
				t.Errorf("deadAfter makes its object: %+v", e)
			}
		}
	}
}
