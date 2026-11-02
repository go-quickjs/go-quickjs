//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// Native code's replaces of trims' patterns, leaves' and calls of Go,
// find their matches as the tree tier does (regexp's trimTail): each
// answer is the interpreter's and Node's, with the collector idle and while
// it marks, the functions run natively.
func TestJITRegExpTrimReplace(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSpace(regexpTrimTestSrc)
	setup := src[:strings.LastIndex(src, "\n")]
	want := strings.Split(regexpTrimTestWant, " # ")[0]
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, setup, "table()", "table()", "table()", "table()")
		jitNativeForTest(t, r, "table")
		v, err := r.Run(compileForTest(t, "table()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, want)
		}
		debug.SetGCPercent(gc)
	}
}
