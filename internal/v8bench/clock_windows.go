//go:build windows

package v8bench

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	queryPerformanceCounter   = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	performanceFrequency      = performanceCount(queryPerformanceFrequency)
)

// performanceCount is what one of the performance counter's functions gives.
func performanceCount(p *syscall.LazyProc) int64 {
	var c int64
	if r, _, _ := syscall.SyscallN(p.Addr(), uintptr(unsafe.Pointer(&c))); r == 0 {
		panic("v8bench: no performance counter")
	}
	return c
}

// clock is the time from some moment of the process's, read from the
// performance counter, as QuickJS's os.now and Node's performance.now read
// it. Go's own clock on Windows is the interrupt time, which moves only at
// the system timer's tick, every 0.5 ms or, while no program asks for
// more, every 15.6 ms: a run of a few milliseconds could be timed as none,
// and the next as two.
func clock() time.Duration {
	c, f := performanceCount(queryPerformanceCounter), performanceFrequency
	return time.Duration(c/f*1e9 + c%f*1e9/f)
}
