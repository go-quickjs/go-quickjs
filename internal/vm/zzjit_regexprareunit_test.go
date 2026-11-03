//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Native code's search for a pattern's required literal over a subject's
// code units (regexp's indexUnits, which looks for its rarest unit) finds
// the interpreter's and Node's matches, with the collector idle and while
// it marks.
func TestJITRegExpRareUnit(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, regexpRareUnitTestSrc, "run()", "run()", "run()", "run()")
		jitNativeForTest(t, r, "run")
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpRareUnitTestWant {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, regexpRareUnitTestWant)
		}
		debug.SetGCPercent(gc)
	}
}
