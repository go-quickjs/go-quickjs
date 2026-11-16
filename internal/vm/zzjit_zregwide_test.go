//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime/debug"
	"testing"
)

// wideCallsSetup's loops make calls of more operands than a call of Go
// takes in the context (abi.MaxGoArgs): plain calls, method calls and
// constructions, of callees that vary past what the call makes natively
// (jitCallTargets), of a built-in, and of a callee that throws -- each
// past the ones native code calls a call of Go, which reads its operands
// from the callee's window (mir's windowGoCall).
const wideCallsSetup = `
	function w0(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { return a + b * 2 + p * 3 + (this && this.k | 0) }
	function w1(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { return a - p + o }
	function w2(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { return a * 3 + n }
	function w3(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { return (a ^ p) + m }
	function w4(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { return arguments.length + a + p }
	function thrower(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { if (a % 97 === 96) throw new Error('w ' + a); return p }
	function W(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { this.v = a + p }
	var fns = [w0, w1, w2, w3, w4]
	var obj = { k: 5, m: w0 }
	function plain(n) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + fns[i % 5](i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)) | 0
		return s
	}
	function methods(n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			obj.m = fns[i % 5]
			s = (s + obj.m(i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)) | 0
		}
		return s
	}
	function builtin(n) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + Math.max(i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16)) | 0
		return s
	}
	function constructs(n) {
		let s = 0
		for (let i = 0; i < n; i++) s = (s + new W(i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15).v) | 0
		return s
	}
	function throws(n) {
		let s = 0, errs = 0
		for (let i = 0; i < n; i++) {
			try { s = (s + thrower(i,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15)) | 0 } catch (e) { errs++ }
		}
		return s + ':' + errs
	}`

// A register call of more operands than a call of Go takes in the context
// is a native call where it calls what its code learned, and a call of Go
// past them, its operands written to the callee's window, where Go makes
// the frame of the call from (mir's windowGoCall, the VM's jitRegGoCallOn)
// -- not an exit: the stack form's left native code there. Each answer is
// the interpreter's, the collector marking or not.
func TestJITRegWideCallsOfGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gc := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gc) })
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			both := jitBothForTest(t, wideCallsSetup)
			runs := []string{`plain(400)`, `methods(400)`, `builtin(400)`, `constructs(400)`, `throws(400)`}
			for range 4 {
				both.agree(t, runs...)
			}
			if !regJITForTest() {
				return
			}
			r := both.jit
			for _, name := range []string{"plain", "methods", "constructs"} {
				jitNativeForTest(t, r, name)
			}
			hosts, goCalls := r.jit.hosts, r.jit.goCalls
			both.agree(t, `plain(400)`, `methods(400)`)
			if r.jit.hosts != hosts || r.jit.goCalls == goCalls {
				t.Errorf("wide calls past the native ones: %d exits, %d calls of Go", r.jit.hosts-hosts, r.jit.goCalls-goCalls)
			}
		})
	}
}
