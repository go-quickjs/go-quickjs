//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
)

// A callee Go does not enter itself (no loop), whose code, compiled before
// it ran, left at each native call -- so that native callers stopped
// calling it (notNative) -- and learned there what to be compiled for,
// has each call Go makes of it count as an invocation of its budget
// (jitLearnedCall), and is compiled again for it, native callers calling
// it again. Before, nothing counted Go's calls of it, and every call was
// Go's for good: microbench_extra's private_method's calls of #get, its
// loop a call of Go at each turn. Each answer is the interpreter's.
func TestJITLearnedCalleeCompiledAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	setup := `class Counter { #n = 3; #get() { return this.#n } sum(n) { var j, s = 0; for (j = 0; j < n; j++) s = (s + this.#get()) | 0; return s } }
		function method(n) { return new Counter().sum(n) }`
	var srcs []string
	n := 1.0
	for i := 0; i < 15; i++ {
		srcs = append(srcs, fmt.Sprintf("method(%v) + method(%v) + method(%v)", n, n, n))
		n *= [3]float64{2, 2.5, 2}[i%3]
	}
	r := jitDefaultRuns(t, setup, srcs...)
	calls := r.jit.goCalls
	if _, err := r.Run(compileForTest(t, `method(200000)`)); err != nil {
		t.Fatal(err)
	}
	if c := r.jit.goCalls - calls; c > 2000 {
		t.Errorf("Go made %d of an invocation's 200,000 calls of #get", c)
	}
}
