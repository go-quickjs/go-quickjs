//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// plainThisSetup's callers call plainly functions that read their this:
// sloppy ones, whose this is the global object, one writing a property of
// it and one leaving its code for a throw at the end; strict ones, whose
// this is undefined; a method taken off its object.
const plainThisSetup = `
	var hits = 0
	function sloppy() { return this === globalThis }
	function sloppyWrites() { this.hits++; return typeof this }
	function sloppyThrows(i) { if (i === 99) throw new TypeError(String(this === globalThis)); return this === globalThis }
	function strict() { 'use strict'; return this === undefined }
	var o = { m() { 'use strict'; return this === undefined ? 1 : 2 } }
	var m = o.m
	function sloppies(n) { let s = 0; for (let i = 0; i < n; i++) s += sloppy() ? 1 : 0; return s }
	function writes(n) { let s = ''; for (let i = 0; i < n; i++) s = sloppyWrites(); return s + hits }
	function throws(n) { let s = 0; try { for (let i = 0; i < n; i++) s += sloppyThrows(i) ? 1 : 0 } catch (e) { return s + ':' + e.message } return s }
	function stricts(n) { let s = 0; for (let i = 0; i < n; i++) s += strict() ? 1 : 0; return s }
	function methods(n) { let s = 0; for (let i = 0; i < n; i++) s += m(); return s }
`

// A plain call of a function that reads its this is made natively (ssa's
// PlainThis), its this undefined or, the callee sloppy, the global object,
// where Go had made every one: each answer is the interpreter's, and no
// caller leaves its code at each of its calls.
func TestJITPlainCallsOfThisReaders(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	callers := []string{"sloppies", "writes", "throws", "stricts", "methods"}
	rounds := []string{}
	for range 8 {
		for _, c := range callers {
			rounds = append(rounds, c+"(100)")
		}
	}
	r := jitAgreesForTest(t, plainThisSetup, rounds...)
	for _, name := range callers {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil {
			t.Errorf("%s has no code", name)
			continue
		}
		// Had Go made its calls, the caller would leave its code at the
		// first call of each entry, and be judged slow for it; throws
		// leaves once a round, for its throw.
		if e.ssaStats.hosts > 8 || e.entrySlow {
			t.Errorf("%s left its code %d times, judged slow %v", name, e.ssaStats.hosts, e.entrySlow)
		}
	}
}
