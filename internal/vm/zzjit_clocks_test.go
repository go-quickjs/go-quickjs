//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// The JIT's cache ages code by its clocks (epoch, flushNow): the collector's
// cycles and the program's time, which a slow run -- a checkptr build, qemu,
// a loaded machine -- stretches until code a test is about to look at, or
// compile again, has gone unused long enough to be flushed: six tests had
// failed so on CI's slow lanes, each one by one held. A test's assertions on
// what native code ran or holds are not about that, and so every runtime
// the package's tests make has its JIT's clocks held, per runtime, which
// parallel tests keep apart. A test about aging or flushing sets the clocks
// itself (jitEpochNow, jitFlushNow, which stand for every runtime's and come
// first) or lets its runtime's run (jitRunClocksForTest), and says why.
//
// QUICKJS_TEST_FAST_CLOCKS set in the environment runs the clocks of the
// runtimes let run fast -- a tick for every few looks -- to show a test
// whose answer depends on how long its run took.
var (
	jitRunningClocks sync.Map // *Runtime
	jitFastTick      atomic.Uint64
)

func init() {
	fast := os.Getenv("QUICKJS_TEST_FAST_CLOCKS") != ""
	jitClocksForTest = func(s *jitState) (uint64, bool) {
		if _, running := jitRunningClocks.Load(s.rt); !running {
			return 0, true
		}
		if fast {
			return jitFastTick.Add(1) / 4, true
		}
		return 0, false
	}
}

// jitRunClocksForTest lets the JIT's clocks of r run on the collector's
// cycles and the program's time until t ends: for a test about aging or
// flushing.
func jitRunClocksForTest(t testing.TB, r *Runtime) {
	jitRunningClocks.Store(r, struct{}{})
	t.Cleanup(func() { jitRunningClocks.Delete(r) })
}
