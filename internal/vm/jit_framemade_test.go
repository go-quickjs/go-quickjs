//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// frameMadeSrc's callees make a rest parameter's array -- after a
// parameter, of none, one and nine arguments, the room grown for them --
// and unmapped arguments objects -- a strict function's, of none, three
// and six, one of a default parameter's function -- each called natively,
// the objects kept, grown, read and passed on. shapes looks at what they
// are: their class, prototype, keys, iterator, callee's throw and JSON,
// and objects past what native code makes (more arguments than an
// arguments object's allocation holds, or a context). drive calls hot, so
// that hot runs as a native callee from its start once warm: a frame Go
// enters the code with has its objects made by Go on the frame.
const frameMadeSrc = `
function rest(a, ...r) { var s = a; for (var i = 0; i < r.length; i++) s += r[i]; return s + r.length * 100 }
function restKeep(...r) { return r }
function strictArgs() { 'use strict'; return arguments }
function dfltArgs(a = 1) { var o = arguments; return o.length + (o[0] | 0) + (o[1] | 0) }
function hot(n) {
	var s = 0, keep = []
	for (var i = 0; i < n; i++) {
		s += rest(i, 1, 2, 3) + rest(i) + rest(i, 1, 2, 3, 4, 5, 6, 7, 8, 9)
		var k = restKeep(i, 'x')
		k.push(i)
		s += k.length + (Array.isArray(k) ? 1 : 0)
		var o = strictArgs(i, 2, 3)
		s += o.length + o[0] + o[2] + strictArgs().length + strictArgs(1, 2, 3, 4, 5, 6).length
		s += dfltArgs(i, 5) + dfltArgs()
		if ((i & 63) === 0) keep.push(k, o)
	}
	return s + ':' + keep.length
}
function shapes() {
	var o = strictArgs(1, 'b', null)
	var e = ''
	try { o.callee } catch (x) { e = x.constructor.name }
	var r = restKeep(4, 5)
	return [Object.prototype.toString.call(o), o.length, [...o].join('|'), e, Object.keys(o).join(), JSON.stringify(o),
		Object.getOwnPropertyNames(o).join(), Object.getPrototypeOf(o) === Object.prototype, r.length, r.join(), Object.getPrototypeOf(r) === Array.prototype,
		strictArgs(1, 2, 3, 4, 5, 6, 7, 8, 9).length, restKeep(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17).length].join(';')
}
function drive(n) { return hot(n) }
function cold(n) { var s = ''; for (var i = 0; i < n; i++) s = shapes(); return s }
`

// TestJITFrameMade runs frameMadeSrc natively and in a runtime with no
// JIT and compares them, and with Node's answers, plainly and with native
// code seeing the collector mark: once warm, its native callees' rest
// parameters' arrays and arguments objects are native code's (mir's
// frameMade), taken from their sites' pools, which calls of Go that find
// no frame fill again, and no operation on a frame makes one.
func TestJITFrameMade(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			recordJITCompilesForTest(t)
			rts := regNativeRuntimes(t, frameMadeSrc)
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
			// Node's answers.
			const wantHot = "1260750:16"
			const wantCold = `[object Arguments];3;1|b|;TypeError;0,1,2;{"0":1,"1":"b","2":null};0,1,2,length,callee;true;2;4,5;true;9;17`
			for range 4 {
				if got := call("hot", 500); got != wantHot {
					t.Fatalf("hot: %q, want %q", got, wantHot)
				}
				if got := call("cold", 30); got != wantCold {
					t.Fatalf("cold: %q, want %q", got, wantCold)
				}
			}
			r.jitStress = jitStressConfig{}
			for range 3 {
				call("drive", 500)
			}
			calls, ops := 0, r.jit.calleeFrameOps
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = func() { calls++ } })
			if got := call("drive", 500); got != wantHot {
				t.Fatalf("drive: %q, want %q", got, wantHot)
			}
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.goCallWork = nil })
			if n := r.jit.calleeFrameOps - ops; n != 0 {
				t.Errorf("%d operations on a native callee's frame in a warm run of drive(500)", n)
			}
			t.Logf("%d calls of Go in a warm run of drive(500)", calls)
			for _, name := range []string{"rest", "restKeep", "strictArgs", "dfltArgs"} {
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
