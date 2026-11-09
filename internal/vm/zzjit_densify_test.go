//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Sparse arrays made dense again (zzz_densify.go) under the JIT, which
// reads and writes them natively once they are: Node's answers, with the
// collector idle and while it marks.
func TestJITDensify(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, densifySrc, "main()", "main()", "main()")
		v, err := r.Run(compileForTest(t, "main()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != densifyWant {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, densifyWant)
		}
		debug.SetGCPercent(gc)
	}
}
