//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// A loop that runs a whole invocation natively, entered by on-stack
// replacement, as each of microbench.js's tests is: compiled before the
// function it calls had been called -- by Go, from native code
// (ssa.GoCall), which learns it -- its polls (ssa.GoPoll) leave for that
// work to be counted toward the code's budget, and the code is compiled
// again within the invocation, as V8's back edges spend its interrupt
// budget. Its loop never leaving and its stretch's work counted only at
// its return, Go had made every call to the invocation's end. Each answer
// is the interpreter's.
func TestJITLongLoopCompiledAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `var global_res
		function long(n) { function f(a) { return 1 } var j, sum = 0; for (j = 0; j < n; j++) { sum += f(j); sum += f(j); sum += f(j); sum += f(j) } global_res = sum; return n * 4 }`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range []string{`long(1000000) + global_res`, `long(1000000) + global_res`} {
		var calls uint64
		if r.jit != nil {
			calls = r.jit.goCalls
		}
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if gv.Number() != wv.Number() {
			t.Fatalf("%s = %v, interpreter %v", src, gv.Number(), wv.Number())
		}
		if r.jit == nil || r.jit.entries == 0 {
			t.Fatal("no native code ran")
		}
		if n := r.jit.goCalls - calls; n > 400000 {
			t.Errorf("%s: Go made %d of its 4,000,000 calls", src, n)
		}
	}
}
