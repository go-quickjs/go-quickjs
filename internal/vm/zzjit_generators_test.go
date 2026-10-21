//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// TestJITGeneratorProtocol runs generatorCases with the JIT compiling at
// the first call, so that a loop over a generator, a generator's
// resumptions and the helpers' callbacks run natively from the second
// round on, where the JIT takes them: each round's answer is Node's.
func TestJITGeneratorProtocol(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runGeneratorCases(t, func() *Runtime {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		return r
	}, 4)
}
