//go:build windows

package v8bench

import (
	"testing"
	"time"
)

// processCPU resolves less than the system timer's tick, 15.625 ms, which
// GetProcessTimes charged CPU time by: a spin of about 2 ms counts as more
// than nothing and less than a tick. A try the spinning thread was taken
// off its processor in may count less; one of a few must pass.
func TestProcessCPUFinerThanATick(t *testing.T) {
	if cycleRate() <= 0 {
		t.Skip("no cycle times")
	}
	var got []time.Duration
	for range 5 {
		c0, t0 := processCPU(), clock()
		for clock()-t0 < 2*time.Millisecond {
		}
		d := processCPU() - c0
		if d > time.Millisecond && d < 15*time.Millisecond {
			return
		}
		got = append(got, d)
	}
	t.Errorf("2 ms of spinning counted as %v of CPU time", got)
}
