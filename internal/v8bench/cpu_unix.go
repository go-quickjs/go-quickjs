//go:build unix

package v8bench

import (
	"syscall"
	"time"
)

// processCPU is the CPU time the process has used so far: every thread's,
// in user and kernel mode, the collector's on other threads included.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
