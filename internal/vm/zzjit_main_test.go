//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"os"
	"testing"
)

// TestMain stops the JIT cache's flush clock (jitFlushNow) for this
// package's tests. The flush drops code that has gone unused for a second
// of wall time, so a test that runs a function, then others, and then
// looks at the first one's code would find it gone on a machine slow
// enough: TestJITBigLeaf did, under checkptr on Windows. The flush's own
// tests set its clock themselves (jitFlushForTest); the conformance and
// stress runs keep the real one.
func TestMain(m *testing.M) {
	jitFlushNow = func() uint64 { return 0 }
	os.Exit(m.Run())
}
