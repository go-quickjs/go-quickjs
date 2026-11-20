//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"testing"
)

// The JIT's cache ages code by its clocks (epoch, flushNow): the collector's
// cycles and the program's time, which a slow run -- a checkptr build, qemu,
// a loaded machine -- stretches until code a test is about to look at, or
// compile again, has gone unused long enough to be flushed: six tests had
// failed so on CI's slow lanes, each one by one held. A test's assertions on
// what native code ran or holds are not about that, and so every runtime
// the package's tests make has its JIT's clocks held, its own
// (jitTestConfig's clocks), which parallel tests keep apart. A test about
// aging or flushing sets its runtimes' clocks itself (jitTestConfig's
// epochNow and flushNow, which come first) or lets its runtime's run
// (jitRunClocksForTest), and says why.
//
// QUICKJS_TEST_FAST_CLOCKS set in the environment runs the clocks of the
// runtimes let run fast -- a tick for every few looks -- to show a test
// whose answer depends on how long its run took.
var jitFastClocks = os.Getenv("QUICKJS_TEST_FAST_CLOCKS") != ""

// jitHeldClocks is the clocks of the package's tests' runtimes but those
// a test lets run: both at 0.
func jitHeldClocks(*jitState) (uint64, bool) { return 0, true }

// The package's tests' runtimes are made with held clocks, a setting of
// the test binary's made before any test runs (testDefaults).
func init() {
	testDefaults = append(testDefaults, func(c *testConfig) { c.jit.clocks = jitHeldClocks })
}

// jitRunClocksForTest lets the JIT's clocks of r run on the collector's
// cycles and the program's time, or fast where QUICKJS_TEST_FAST_CLOCKS
// says, from now on: for a test about aging or flushing.
func jitRunClocksForTest(t testing.TB, r *Runtime) {
	r.jitTest.clocks = nil
	if jitFastClocks {
		var tick uint64
		r.jitTest.clocks = func(*jitState) (uint64, bool) {
			tick++
			return tick / 4, true
		}
	}
}
