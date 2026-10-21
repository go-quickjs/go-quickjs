//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// deepTailScript's recursions through calls in tail position, from code
// the JIT compiles round after round, answer as the interpreter's do.
func TestJITDeepTailCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitAgreesForTest(t, "function run() { return "+deepTailScript+"; }", "run()", "run()", "run()", "run()")
}
