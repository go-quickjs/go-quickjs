//go:build windows

package v8bench

import (
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	queryProcessCycleTime = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryProcessCycleTime")
	queryThreadCycleTime  = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryThreadCycleTime")
	getCurrentThread      = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentThread")
)

// processCPU is the CPU time the process has used so far: every thread's,
// in user and kernel mode, the collector's on other threads included. It
// is the process's cycles (QueryProcessCycleTime) at the rate they are
// counted (cycleRate): GetProcessTimes, what it was, is charged a whole
// tick of the system timer, 15.625 ms, at a time, so that a benchmark of a
// few milliseconds was timed as none or as one tick. Where the cycles are
// not to be had, it is GetProcessTimes' all the same.
func processCPU() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	if hz := cycleRate(); hz > 0 {
		var cycles uint64
		if r, _, _ := queryProcessCycleTime.Call(uintptr(h), uintptr(unsafe.Pointer(&cycles))); r != 0 {
			return time.Duration(float64(cycles) / hz * 1e9)
		}
	}
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0
	}
	// A duration in a Filetime is in units of 100 ns.
	units := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((units(kernel) + units(user)) * 100)
}

// cycleRate is how many cycles the cycle times count a second, or 0 where
// they cannot be read: measured once, as a thread's cycles over the
// performance counter's time while it spins, the most of a few tries, a
// try a thread was taken off its processor in counting fewer. Windows
// counts them by the processor's time-stamp counter, whose rate is
// constant, not by the clock the core runs at.
var cycleRate = sync.OnceValue(func() float64 {
	if queryProcessCycleTime.Find() != nil || queryThreadCycleTime.Find() != nil || getCurrentThread.Find() != nil {
		return 0
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, _, _ := getCurrentThread.Call()
	threadCycles := func() (uint64, bool) {
		var c uint64
		r, _, _ := queryThreadCycleTime.Call(thread, uintptr(unsafe.Pointer(&c)))
		return c, r != 0
	}
	best := 0.0
	for range 3 {
		c0, ok0 := threadCycles()
		t0 := clock()
		for clock()-t0 < 5*time.Millisecond {
		}
		c1, ok1 := threadCycles()
		t1 := clock()
		if !ok0 || !ok1 || c1 <= c0 {
			return 0
		}
		best = max(best, float64(c1-c0)/(t1-t0).Seconds())
	}
	return best
})
