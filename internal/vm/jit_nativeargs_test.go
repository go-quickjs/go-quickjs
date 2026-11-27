//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// argReadSrc's list and append read their arguments as EarleyBoyer's
// sc_list and sc_append do -- arguments.length and arguments[k] in a loop,
// the object never made -- each called natively, not inlined: a loop
// that makes calls. Their calls of Go to read them (ssa.GoArgument) had
// been EarleyBoyer's Earley's most frequent, at every read of a native
// callee, each finding the callee's level. edge, mapped, made and many are
// the reads native code leaves to Go: keys that are no index of them,
// past or before them, a string, a fraction, a negative zero; a parameter
// a mapped object aliases, assigned; the object made after a read; more
// arguments than a native call writes to its callee's context. assigned's
// functions assign their parameters -- a strict one, whose arguments keep
// what the call gave, one of a default, whose object maps none, one whose
// closure assigns a parameter a mapped object aliases -- or read them
// through an arrow.
const argReadSrc = `
function Pair(a, d) { this.car = a; this.cdr = d }
function list() { var r = null; var a = arguments; for (var i = a.length - 1; i >= 0; i--) r = new Pair(a[i], r); return r }
function dual(l1, l2) { if (l1 === null) return l2; return new Pair(l1.car, dual(l1.cdr, l2)) }
function append() {
	if (arguments.length === 0) return null
	var res = arguments[arguments.length - 1]
	for (var i = arguments.length - 2; i >= 0; i--) res = dual(arguments[i], res)
	return res
}
function total(l) { var s = 0; for (; l !== null; l = l.cdr) s += typeof l.car === 'number' ? l.car : l.car.length; return s }
function hot(n) {
	var s = 0
	for (var i = 0; i < n; i++) s += total(append(list(i, 1, 2), list(), list(3, 'xy', {}.x === undefined ? 4 : 0), list(i & 3)))
	return s
}
function show(v) { return v === undefined ? 'u' : typeof v === 'object' ? (v === null ? 'n' : 'o') : String(v) }
function edge(k) {
	var s = ''
	for (var j = 0; j < 2; j++)
		s += [arguments[k], arguments[-1], arguments[1.5], arguments['1'], arguments[-0], arguments.length, arguments[20], arguments[k + 3]].map(show).join() + ';'
	return s
}
function mapped(a, b) { var s = 0; for (var j = 0; j < 2; j++) { a = a + 1; s += arguments[0] + arguments[1] + (arguments[2] | 0) + arguments.length } return s }
function made() { var s = 0; for (var j = 0; j < 2; j++) { var x = arguments[0]; var o = arguments; o[0] = x + 5; s += x + arguments[0] + o.length } return s }
function many() { var s = 0; for (var j = 0; j < 2; j++) s += arguments.length + (arguments[15] | 0) + (arguments[16] | 0); return s }
function strictLen() { 'use strict'; var s = 0; for (var j = 0; j < 2; j++) s += arguments.length * 10 + (arguments[0] | 0); return s }
function strictSet(a) { 'use strict'; var s = 0; for (var j = 0; j < 2; j++) { a = a + 10; s += arguments[0] + a + arguments[1] } return s }
function dflt(a, b = 7) { var s = 0; for (var j = 0; j < 2; j++) { a = a + 1; s += arguments[0] + (arguments[1] | 0) + b + arguments.length } return s }
function viaArrow(a) { var g = () => arguments[0] + arguments.length; var s = 0; for (var j = 0; j < 2; j++) s += g() + arguments[1]; return s }
function closureSet(a, b) { var set = function () { a = 100 }; var s = 0; for (var j = 0; j < 2; j++) { s += arguments[0] + arguments[1] + arguments[2]; set() } return s }
function assigned(n) {
	var out = []
	for (var i = 0; i < n; i++) out = [strictSet(i, 3), dflt(i), dflt(i, 2, 9), viaArrow(i, 4), closureSet(i, 5, 6)]
	return out.join()
}
function cold(n) {
	var s = ''
	for (var i = 0; i < n; i++) {
		s = edge(i & 7, 1, 'a', null) + mapped(i, 2, 3) + made(i, 4) + strictLen(i, 2) + strictLen()
		s += many(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17) + many(i)
	}
	return s
}
`

// TestJITArgumentsRead runs argReadSrc natively and in a runtime with no
// JIT and compares them, plainly and with native code seeing the
// collector mark: once warm, list's and append's reads of their arguments
// as native callees are native code's (mir's argRead), with no call of Go
// for them, and the reads native code leaves to Go are Go's answers.
func TestJITArgumentsRead(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, argReadSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			call := func(name string, n int) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(float64(n))})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Fatalf("%s: with the JIT %q, without %q", name, got[0], got[1])
				}
				return got[0]
			}
			const wantHot, wantCold = "22600", "u,u,u,1,5,4,u,u;u,u,u,1,5,4,u,u;77140980102"
			for range 4 {
				if got := call("hot", 200); got != wantHot {
					t.Fatalf("hot: %q, want %q", got, wantHot)
				}
				if got := call("cold", 30); got != wantCold {
					t.Fatalf("cold: %q, want %q", got, wantCold)
				}
				// Node's answer.
				if got, want := call("assigned", 30), "152,74,72,70,151"; got != want {
					t.Fatalf("assigned: %q, want %q", got, want)
				}
			}
			// What follows counts what native code did, which the stress
			// modes' compiles, deoptimizations and budgets change.
			r.jitStress = jitStressConfig{}
			for range 3 {
				call("hot", 200)
			}
			calls := 0
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = func() { calls++ } })
			before := r.RegNativeStats().HostOps
			call("hot", 200)
			after := r.RegNativeStats().HostOps
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = nil })
			for _, op := range []string{"args_index", "args_len", "arguments"} {
				if n := after[op] - before[op]; n != 0 {
					t.Errorf("%d exits at %s once warm: %v", n, op, after)
				}
			}
			// 200 turns of 5 calls of list and append, which read their
			// arguments some 3,000 times: the calls of Go left are the
			// pools' refills.
			if calls > 100 {
				t.Errorf("%d calls of Go in a warm run of hot(200): its arguments' reads are Go's", calls)
			}
			t.Logf("%d calls of Go in a warm run of hot(200)", calls)
			for _, name := range []string{"list", "append"} {
				v := jitCodeNamedForTest(t, r, name)
				if l := v.link(r, jitGlobalClosureForTest(r, name)); l == nil || l.in == 0 {
					t.Errorf("%s was never called natively", name)
				}
			}
			if fns, _, _ := regCompileARM64(t, r); fns == 0 {
				t.Errorf("arm64: no function compiled")
			}
		})
	}
}
