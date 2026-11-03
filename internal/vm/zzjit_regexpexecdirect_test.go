//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// Where a runtime runs the JIT, exec's calls whose arrays may be made again
// are made as before (reuseCallRest), natively and in the tree tier: each
// answer is the interpreter's and Node's, with the collector idle and while
// it marks.
func TestJITRegExpExecDirect(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSpace(regexpExecDirectTestSrc)
	setup := src[:strings.LastIndex(src, "\n")]
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 4 {
			rounds = append(rounds, "run()")
		}
		r := jitAgreesForTest(t, setup, rounds...)
		jitNativeForTest(t, r, "E")
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpExecDirectTestWant {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, regexpExecDirectTestWant)
		}
		debug.SetGCPercent(gc)
	}
}
