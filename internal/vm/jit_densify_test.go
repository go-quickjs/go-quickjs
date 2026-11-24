//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"testing"
)

// Sparse arrays made dense again (densify.go) under the JIT, which
// reads and writes them natively once they are: Node's answers, with the
// collector idle and while it marks, at each of four runs, the first
// compiling as it goes and the later ones in the code it left. Each run
// is held to Node's answer, which TestDensify holds the interpreter to:
// a runtime without the JIT beside it, to compare with, had run the
// program three more times in each mode and checked nothing more.
func TestJITDensify(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		release := holdCollectorNow(t)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitRuntimeForTest(t, Config{JIT: true})
		if _, err := r.Run(compileForTest(t, densifySrc)); err != nil {
			t.Fatal(err)
		}
		for i := range 4 {
			v, err := r.Run(compileForTest(t, "main()"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != densifyWant {
				t.Errorf("marking %v, run %d:\ngot  %s\nwant %s", marking, i, got, densifyWant)
			}
		}
		release()
	}
}
