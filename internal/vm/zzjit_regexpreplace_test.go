//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// A string's replace by a template without a $, both ASCII, is made in one
// pass (zzz_regexpreplace.go) for native code's calls of replace too: run,
// compiled, replaces in its loop, with the collector idle and while it
// marks, and each answer is the interpreter's and Node's
// (replaceFlatTestWant).
func TestJITRegExpReplaceFlat(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := replaceFlatTestSrc[:strings.LastIndex(replaceFlatTestSrc, "table() +")]
	want := strings.Fields(replaceFlatTestWant)
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, setup, "run(500)", "others()", "run(2000)", "run(2000)", "table()")
		jitNativeForTest(t, r, "run")
		for _, c := range []struct{ src, want string }{
			{"run(500)", want[len(want)-2]}, {"run(2000)", want[len(want)-1]}, {"table()", want[0] + " " + want[1]},
			{"others()", strings.Join(want[2:len(want)-2], " ")},
		} {
			v, err := r.Run(compileForTest(t, "''+"+c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.src, got, c.want)
			}
		}
		debug.SetGCPercent(gc)
	}
}
