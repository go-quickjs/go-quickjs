package vm

import (
	"syscall"
	"time"
)

// processCPUForTest is the CPU time the process has spent, user and
// kernel: what a test that compares the cost of two runs measures, which,
// unlike the clock, other processes on the machine do not move.
func processCPUForTest() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	units := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((units(kernel) + units(user)) * 100)
}
