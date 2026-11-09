package wasi

import "github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"

// The clock ids.
const (
	clockRealtime  = 0
	clockMonotonic = 1
)

func clockResGet(c *call, p []uint64) sysfs.Errno {
	var res uint64
	switch uint32(p[0]) {
	case clockRealtime:
		res = uint64(c.sc.WalltimeResolution)
	case clockMonotonic:
		res = uint64(c.sc.NanotimeResolution)
	default:
		return sysfs.EINVAL
	}
	if !c.putU64(uint32(p[1]), res) {
		return sysfs.EFAULT
	}
	return 0
}

// now reads clock id, in nanoseconds.
func (c *call) now(id uint32) (int64, sysfs.Errno) {
	switch id {
	case clockRealtime:
		return c.walltime(), 0
	case clockMonotonic:
		return c.sc.Nanotime(), 0
	}
	return 0, sysfs.EINVAL
}

// walltime reads the wall clock in nanoseconds since the Unix epoch.
func (c *call) walltime() int64 {
	sec, nsec := c.sc.Walltime()
	return sec*1e9 + int64(nsec)
}

func clockTimeGet(c *call, p []uint64) sysfs.Errno {
	t, errno := c.now(uint32(p[0]))
	if errno != 0 {
		return errno
	}
	if !c.putU64(uint32(p[2]), uint64(t)) {
		return sysfs.EFAULT
	}
	return 0
}
