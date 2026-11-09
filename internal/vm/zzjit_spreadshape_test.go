//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Object spread by layout (zzz_spreadshape.go) under the JIT: the spreads
// native code leaves to Go, and those it makes from what Go's taught it
// (jitCopyLearn), make what the interpreter makes -- Node's answer -- with
// the collector idle and while it marks.
func TestJITSpreadByLayout(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, spreadShapeSrc, "run(1)", "run(30)", "run(30)", "run(30)", "run(30)")
		v, err := r.Run(compileForTest(t, "run(30)"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != spreadShapeWant {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, spreadShapeWant)
		}
		debug.SetGCPercent(gc)
	}
}
