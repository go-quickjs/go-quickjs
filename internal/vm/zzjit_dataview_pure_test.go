//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
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
		gc := debug.SetGCPercent(-1)
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
		if e := r.jit.entryFor(r.global.getOwn(r.atoms.intern("kUint8")).value().Object().fn().closure.fn); e == nil || e.ssa == nil {
			t.Errorf("marking %v: kUint8 was not compiled", marking)
		}
		debug.SetGCPercent(gc)
	}
}
