//go:build windows

package spanclock

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	queryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
	frequency                 = sync.OnceValue(func() int64 { return count(queryPerformanceFrequency) })
)

// Now is the time from some moment of the process's, read from the
// performance counter: a call into the system, some tens of nanoseconds
// against time.Now's few, so read it for a span, not on a path every call
// takes.
func Now() time.Duration {
	c, f := count(queryPerformanceCounter), frequency()
	return time.Duration(c/f*1e9 + c%f*1e9/f)
}

// count is what one of the performance counter's functions gives, which
// cannot fail on Windows XP or later.
func count(p *syscall.LazyProc) int64 {
	var c int64
	if r, _, _ := syscall.SyscallN(p.Addr(), uintptr(unsafe.Pointer(&c))); r == 0 {
		panic("spanclock: no performance counter")
	}
	return c
}
