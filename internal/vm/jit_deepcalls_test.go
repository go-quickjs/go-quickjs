//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// A native call past the deepest context the runtime has is a call of
// Go from native code, as one of a callee with no code is: the caller's
// code goes on after it, the callee run in the existing tiers, and the
// call has more contexts made, as many as there are, for the calls after
// it to be native. It had been an exit, Go finishing the deepest native
// level in the interpreter at every such call: AWFY's Havlak, whose
// depth-first search recurses past the contexts, left native code so some
// 5,600 times a run. Grown to the recursion's depth, its runs never leave
// native code. Each answer is the interpreter's.
func TestJITCallsPastDeepestContext(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer holdCollectorNow(t)()
	setup := `function depth(n, o) { if (n === 0) return o.v; return (depth(n - 1, o) + n * o.v) | 0 }
		function drive(k, n) { const o = { v: 3 }; let s = 0; for (let i = 0; i < k; i++) s = (s + depth(n, o)) | 0; return s }`
	r := jitAgreesForTest(t, setup, `drive(50, 300)`, `drive(50, 300)`, `drive(50, 300)`, `drive(50, 300)`)
	if n := len(r.jit.ssaCtxs); n < 302 {
		t.Fatalf("%d contexts for a recursion 300 deep", n)
	}
	unwound, hosts := r.jit.unwound, r.jit.hosts
	if v, err := r.Run(compileForTest(t, `''+drive(100, 300)`)); err != nil || v.String().Go() != "13545300" {
		t.Fatalf("drive(100, 300) = %s, %v", v.String().Go(), err)
	}
	if n, h := r.jit.unwound-unwound, r.jit.hosts-hosts; n != 0 || h != 0 {
		t.Errorf("100 recursions 300 deep: Go finished %d levels, %d exits, want none", n, h)
	}
	jitDifferential(t, setup+`; [drive(20, 300), drive(5, 1000)].join()`)
}
