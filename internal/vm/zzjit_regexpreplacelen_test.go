//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// Replaces whose results are only measured (zzz_regexpreplacelen.go) answer
// the same natively, where native code makes them as before, and in the
// tree tier of a runtime running the JIT, before the functions are
// compiled: each answer is the interpreter's and Node's, with the collector
// idle and while it marks.
func TestJITRegExpReplaceLen(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSpace(regexpReplaceLenTestSrc)
	setup := src[:strings.LastIndex(src, "\n")]
	want := strings.Split(regexpReplaceLenTestWant, " # ")
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 4 {
			rounds = append(rounds, "table()")
		}
		r := jitAgreesForTest(t, setup, rounds...)
		jitNativeForTest(t, r, "table")
		v, err := r.Run(compileForTest(t, "table() + ' # ' + odd() + ' # ' + table()"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), want[0]+" # "+want[1]+" # "+want[2]; got != want {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, want)
		}
		debug.SetGCPercent(gc)
	}
}
