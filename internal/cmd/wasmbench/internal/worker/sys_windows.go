//go:build windows

package worker

import (
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	queryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
	queryProcessCycleTime     = kernel32.NewProc("QueryProcessCycleTime")
	queryThreadCycleTime      = kernel32.NewProc("QueryThreadCycleTime")
	getCurrentThread          = kernel32.NewProc("GetCurrentThread")
	getProcessMemoryInfo      = kernel32.NewProc("K32GetProcessMemoryInfo")
	performanceFrequency      = performanceCount(queryPerformanceFrequency)
)

// performanceCount is what one of the performance counter's functions gives.
func performanceCount(p *syscall.LazyProc) int64 {
	var c int64
	if r, _, _ := syscall.SyscallN(p.Addr(), uintptr(unsafe.Pointer(&c))); r == 0 {
		panic("worker: no performance counter")
	}
	return c
}

// clock is the time from some moment of the process's, read from the
// performance counter: Go's own clock on Windows moves only at the system
// timer's tick, every 0.5 or 15.6 ms.
func clock() time.Duration {
	c, f := performanceCount(queryPerformanceCounter), performanceFrequency
	return time.Duration(c/f*1e9 + c%f*1e9/f)
}

// processCPU is the CPU time the process has used so far, every thread's,
// in user and kernel mode: its cycles at the rate they are counted, which
// is finer than GetProcessTimes' ticks of 15.6 ms, or GetProcessTimes'
// where the cycles are not to be had.
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
	units := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((units(kernel) + units(user)) * 100)
}

// cycleRate is how many cycles a second the cycle times count, or 0 where
// they cannot be read: a thread's cycles over the performance counter's
// time while it spins, the most of three tries.
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

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// peakRSS is the process's peak working set, in bytes.
func peakRSS() uint64 {
	h, err := syscall.GetCurrentProcess()
	if err != nil || getProcessMemoryInfo.Find() != nil {
		return 0
	}
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	if r, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); r == 0 {
		return 0
	}
	return uint64(c.peakWorkingSetSize)
}
