//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// RegExp.prototype's exec and test, native leaves (zzz_regexpleaf.go), are
// leaves' calls of native code (ssa.GoLeaf) where their receiver is no
// literal's: loop's exec of a RegExp it holds and its test of one neither
// global nor sticky, and esc's exec, from a native callee of escs's loop,
// are made through the VM's routine for leaves, with the collector idle
// and while it marks, each answer the interpreter's and Node's
// (regexpLeafTestWant). A global pattern's test, which writes lastIndex,
// and a literal's, whose RegExp its call of Go gives back to the literal's
// pool (jitRegExpLitPool), stay calls of Go.
func TestJITRegExpLeaves(t *testing.T) {
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
			rounds = append(rounds, "loop(200)", "escs(200)")
		}
		r := jitAgreesForTest(t, regexpLeafTestSrc, rounds...)
		exec, test := uintptr(unsafe.Pointer(r.regexpExecFn)), uintptr(unsafe.Pointer(r.regexpTestFn))
		for _, c := range []struct {
			name        string
			execs, test int
		}{{"loop", 1, 1}, {"esc", 1, 0}} {
			e := jitNativeForTest(t, r, c.name)
			execs, tests := 0, 0
			for _, st := range e.ssa.GoCallStates() {
				if ssa.GoOp(st.Op&0xff) != ssa.GoLeaf {
					continue
				}
				switch st.Leaf {
				case exec:
					execs++
				case test:
					tests++
				}
			}
			if execs != c.execs || tests != c.test {
				t.Errorf("marking %v: %s's code has %d leaves' calls of exec and %d of test, want %d and %d", marking, c.name, execs, tests, c.execs, c.test)
			}
		}
		for _, c := range []struct {
			src, want string
			leaves    uint64
		}{{"loop(200)", "70347", 400}, {"escs(200)", "q&#x27;s11", 150}} {
			fast := r.jit.leafFast
			v, err := r.Run(compileForTest(t, "''+"+c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.src, got, c.want)
			}
			if got := r.jit.leafFast - fast; got < c.leaves {
				t.Errorf("marking %v: %s made %d leaves' calls through the routine, want at least %d", marking, c.src, got, c.leaves)
			}
		}
		debug.SetGCPercent(gc)
	}
}
