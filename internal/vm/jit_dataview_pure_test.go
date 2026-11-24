//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// DataView's setters in pure bodies (dataViewPureSrc) with the JIT, its
// code compiled for the loops and the callers, with the collector idle and
// while it marks: each store made once, every answer the interpreter's and
// Node's.
func TestJITDataViewSetterInPureBody(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		release := holdCollectorNow(t)
		if marking {
			jitMarkingForTest(t)
		}
		rounds := []string{"run()", "rx()", "run()", "rx()", "run()", "rx()"}
		r := jitAgreesForTest(t, dataViewPureSrc, rounds...)
		for src, want := range map[string]string{"run()": dataViewPureWant, "rx()": "5:c:0"} {
			v, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, src, got, want)
			}
		}
		if jitSSACodeForTest(r, "kUint8") == nil {
			t.Errorf("marking %v: kUint8 was not compiled", marking)
		}
		release()
	}
}
