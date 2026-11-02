//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// Native code's leaves' calls of exec and test (jitLeafGo) find the matches
// a look at the subject says miss (regexp's MissesASCII) as the tree tier
// does: each answer is the interpreter's and Node's, with the collector
// idle and while it marks, the functions run natively and their calls of
// exec made through the leaves' routine.
func TestJITRegExpLeafMisses(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSuffix(strings.TrimSpace(regexpMissTestSrc), "misses(3) + ' | ' + execs(100) + ' | ' + execs(400)")
	want := regexpMissTestWant[strings.LastIndex(regexpMissTestWant[:strings.LastIndex(regexpMissTestWant, " | ")], " | ")+3:]
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 4 {
			rounds = append(rounds, "misses(2)", "execs(100) + ' | ' + execs(400)")
		}
		r := jitAgreesForTest(t, src, rounds...)
		jitNativeForTest(t, r, "execs")
		jitNativeForTest(t, r, "misses")
		fast := r.jit.leafFast
		v, err := r.Run(compileForTest(t, "execs(100) + ' | ' + execs(400)"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("marking %v: execs = %s, want Node's %s", marking, got, want)
		}
		if r.jit.leafFast == fast {
			t.Errorf("marking %v: execs made no leaf's call through the routine", marking)
		}
		v, err = r.Run(compileForTest(t, "misses(3)"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), regexpMissTestWant[:strings.Index(regexpMissTestWant, " | ")]; got != want {
			t.Errorf("marking %v: misses:\ngot  %s\nwant %s", marking, got, want)
		}
		debug.SetGCPercent(gc)
	}
}
