//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// TestMain stops the JIT cache's flush clock (jitFlushNow) for this
// package's tests. The flush drops code that has gone unused for a second
// of the program's time, so a test that runs a function, then others, and then
// looks at the first one's code would find it gone on a machine slow
// enough: TestJITBigLeaf did, under checkptr on Windows. The flush's own
// tests set its clock themselves (jitFlushForTest); the conformance and
// stress runs keep the real one.
//
// Every compile checks its register allocation (mir.CheckAllocations),
// but where a test or a benchmark times the compiles.
//
// A panic of the compiler's that a worker made a refusal of fails the run
// (jitWorkerPanics), as it would a compile on the spot: the runtimes that
// compile in the background are many more than the tests that look at
// their backendPanics.
//
// With QJS_JIT_SNAPCHECK set, every build is checked against its snapshot
// (jitSnapOracle), and the run fails if any built otherwise.
func TestMain(m *testing.M) {
	jitFlushNow = func() uint64 { return 0 }
	mir.CheckAllocations = true
	if os.Getenv("QJS_JIT_SNAPCHECK") != "" {
		jitFeedbackHook, jitBuiltHook = jitSnapOracle, jitSnapBuilt
	}
	// With QJS_JIT_SNAPOPS set to a file, every build asks a noting proxy
	// (jitAsked), and the methods asked at each op are written there.
	var opsLog *jitAskLog
	if f := os.Getenv("QJS_JIT_SNAPOPS"); f != "" {
		opsLog = newJITAskLog()
		jitFeedbackHook = func(fb *jitFeedback, p *ir.Program) ssa.Feedback {
			opsLog.builds++
			return &jitAsked{fb: fb, p: p, log: opsLog}
		}
	}
	code := m.Run()
	if n := jitWorkerPanics.Load(); n != 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "the JIT's workers made refusals of %d panics of the compiler's\n", n)
		code = 1
	}
	if f := os.Getenv("QJS_JIT_SNAPOPS"); f != "" {
		if err := jitWriteOps(f, opsLog); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	if os.Getenv("QJS_JIT_SNAPCHECK") != "" && jitSnapReport(os.Stderr) && code == 0 {
		code = 1
	}
	os.Exit(code)
}
