//go:build windows

package v8bench

import (
	"syscall"
	"time"
)

// processCPU is the CPU time the process has used so far: every thread's,
// in user and kernel mode, the collector's on other threads included.
func processCPU() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	// A duration in a Filetime is in units of 100 ns.
	units := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((units(kernel) + units(user)) * 100)
}
