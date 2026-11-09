//go:build unix

package worker

import (
	"runtime"
	"syscall"
	"time"
)

// clockStart is the moment clock counts from.
var clockStart = time.Now()

// clock is the time from some moment of the process's, read from Go's
// monotonic clock.
func clock() time.Duration { return time.Since(clockStart) }

// processCPU is the CPU time the process has used so far, every thread's,
// in user and kernel mode.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// peakRSS is the process's peak resident set, in bytes: getrusage gives it
// in bytes on macOS and in KiB elsewhere.
func peakRSS() uint64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil || ru.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return uint64(ru.Maxrss)
	}
	return uint64(ru.Maxrss) * 1024
}
