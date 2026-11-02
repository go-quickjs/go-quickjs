//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// A match's array and its strings are made together (zzz_regexpresult.go)
// for native code's leaves' calls of exec too: run's Exec, compiled, calls
// exec through the VM's routine for leaves, with the collector idle and
// while it marks, and each answer is the interpreter's and Node's
// (matchResultTestWant).
func TestJITRegExpMatchResults(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := matchResultTestSrc[:strings.LastIndex(matchResultTestSrc, "table() +")]
	want := strings.Fields(matchResultTestWant)
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, setup, "run(1000)", "table()", "run(3000)", "run(3000)", "table()")
		jitNativeForTest(t, r, "Exec")
		fast := r.jit.leafFast
		for _, c := range []struct{ src, want string }{
			{"run(1000)", want[2]}, {"run(3000)", want[3]}, {"table()", want[0] + " " + want[1]},
		} {
			v, err := r.Run(compileForTest(t, "''+"+c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.src, got, c.want)
			}
		}
		if got := r.jit.leafFast - fast; got < 4000 {
			t.Errorf("marking %v: %d leaves' calls through the routine, want at least 4,000", marking, got)
		}
		debug.SetGCPercent(gc)
	}
}
