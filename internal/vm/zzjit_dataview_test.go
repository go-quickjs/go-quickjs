//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A DataView's accessors, native leaves (zzz_dataview.go), are leaves'
// calls of native code (ssa.GoLeaf), made by the VM's routine for leaves
// rather than by calls of Go with frames: mix's loop, its indices and its
// values varied by the turn, makes its eleven accessors' calls a turn
// through the routine, with the collector idle and while it marks, each
// answer the interpreter's and Node's (dataViewTestWant), and the read past
// the view's end after the loop still throws its RangeError.
func TestJITDataViewLeaves(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 6 {
			rounds = append(rounds, "mix(mv, 200)")
		}
		r := jitAgreesForTest(t, dataViewTestSrc, rounds...)
		e := jitNativeForTest(t, r, "mix")
		leaves := 0
		for _, st := range e.ssa.GoCallStates() {
			if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf != 0 {
				leaves++
			}
		}
		if leaves < 11 {
			t.Errorf("marking %v: mix's code has %d leaves' calls, want the eleven accessors'", marking, leaves)
		}
		fast, refused, calls := r.jit.leafFast, r.jit.leafRefused, e.ssaStats.goCalls
		v, err := r.Run(compileForTest(t, "''+mix(mv, 200)"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), "83413RangeError"; got != want {
			t.Errorf("marking %v: mix(mv, 200) = %s, want Node's %s", marking, got, want)
		}
		if got := r.jit.leafFast - fast; got < 11*200 || e.ssaStats.goCalls-calls > 4 {
			t.Errorf("marking %v: mix(mv, 200) made %d leaves' calls through the routine, refused %d, %d calls of Go with frames; want at least 2200, at most 4",
				marking, got, r.jit.leafRefused-refused, e.ssaStats.goCalls-calls)
		}
		debug.SetGCPercent(gc)
	}
}
