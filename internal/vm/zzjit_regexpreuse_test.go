//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// Where a runtime runs the JIT, exec's leaf is told nothing of the arrays it
// may make again (zzz_regexpreuse.go): native code holds a frame's locals
// where the slots cannot say. The calls that would have been told are made
// natively and in the tree tier as before: each answer is the
// interpreter's and Node's, with the collector idle and while it marks, and
// no array was made again.
func TestJITRegExpReuse(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSpace(regexpReuseTestSrc)
	setup := src[:strings.LastIndex(src, "\n")]
	want := strings.Split(regexpReuseTestWant, " # ")
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 4 {
			rounds = append(rounds, "all()")
		}
		r := jitAgreesForTest(t, setup, rounds...)
		jitNativeForTest(t, r, "Exec")
		v, err := r.Run(compileForTest(t, "all() + ' # ' + all() + ' # ' + leaks()"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), want[0]+" # "+want[1]+" # "+want[2]; got != want {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, want)
		}
		if execReuseLeaf[struct{}](r).reuseTable != nil {
			t.Errorf("marking %v: arrays made again in a runtime running the JIT", marking)
		}
		debug.SetGCPercent(gc)
	}
}
