//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// With the JIT, Config.GCYield waits as well when compiled code finds the
// collector marking -- where native code returns to Go or calls it, or at
// its interrupt checks -- and the answer is the same: churn compiled, the
// collector marking for the next three questions is one wait.
func TestGCYieldWithJIT(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true, JIT: true})
	jitDefaultsForTest(r)
	r.jitStress.background = false
	r.SetJITWarmUp(1, 1)
	marking, _ := gcMarkingFor(0)
	r.SetGCMarkingForTest(marking)
	for range 3 {
		if got, pauses, _ := gcYieldRun(t, r, 1); got != gcYieldWant || pauses != 0 {
			t.Fatalf("churn = %q with %d waits, the collector not marking, want %q with none", got, pauses, gcYieldWant)
		}
	}
	if jitSSACodeForTest(r, "churn") == nil {
		t.Fatal("churn not compiled")
	}
	marking, asked := gcMarkingFor(3)
	r.SetGCMarkingForTest(marking)
	if got, pauses, _ := gcYieldRun(t, r, 1); got != gcYieldWant || pauses != 1 || *asked <= 4 {
		t.Fatalf("churn = %q with %d waits, %d questions, want %q with 1 and more than 4", got, pauses, *asked, gcYieldWant)
	}
}
