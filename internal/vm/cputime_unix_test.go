//go:build unix

package vm

import (
	"syscall"
	"time"
)

// processCPUForTest is the CPU time the process has spent, user and
// system: what a test that compares the cost of two runs measures, which,
// unlike the clock, other processes on the machine do not move.
func processCPUForTest() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
