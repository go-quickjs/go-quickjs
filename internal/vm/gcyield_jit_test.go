//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// With the JIT, Config.GCYield waits at the JIT's points as well -- where
// native code returns to Go or calls it -- and the answer is the same.
func TestGCYieldWithJIT(t *testing.T) {
	r := gcYieldRuntime(t, Config{GCYield: true, JIT: true})
	jitDefaultsForTest(r)
	r.jitStress.background = false
	r.SetJITWarmUp(1, 1)
	total, cycles := 0, 0
	for range 3 {
		got, pauses, n := gcYieldRun(t, r, 1)
		if got != gcYieldWant {
			t.Fatalf("churn = %q, want %q", got, gcYieldWant)
		}
		total, cycles = total+pauses, cycles+n
	}
	if jitSSACodeForTest(r, "churn") == nil {
		t.Fatal("churn not compiled")
	}
	if cycles == 0 || total < 1 || total > cycles {
		t.Fatalf("%d waits over %d cycles, want 1 to %d", total, cycles, cycles)
	}
}
