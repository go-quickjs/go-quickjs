//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Every change regexpPristineCases makes is found by native code's RegExp
// calls too, with the collector idle and while it marks: each answer is
// Node's, work() and prep() ran natively, and the keyed and the megamorphic
// write natively made, to RegExp.prototype, are writes Go sees -- the
// keyed one by its call of Go (jitStoreProp), the megamorphic one because
// the megamorphic cache names no pristine object's property one it may
// write (megaFill).
func TestJITRegExpPristine(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		where := map[bool]string{false: "marking false", true: "marking true"}[marking]
		for _, c := range regexpPristineCases {
			runtime.GC()
			gc := debug.SetGCPercent(-1)
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			regexpPristineCheck(t, r, c, where)
			for _, f := range []string{"work", "prep", "mputs", "kputs"} {
				jitNativeForTest(t, r, f)
			}
			debug.SetGCPercent(gc)
		}
	}
}

// A pristine object has more properties than native code searches a table
// of for a write (abi.MaxScan), which no cache's entry answers and so no
// write of Go's sees.
func TestJITRegExpPristineScan(t *testing.T) {
	if pristineMinProps < abi.MaxScan {
		t.Errorf("pristineMinProps is %d, abi.MaxScan %d", pristineMinProps, abi.MaxScan)
	}
}
