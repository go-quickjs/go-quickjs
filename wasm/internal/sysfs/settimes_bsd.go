//go:build darwin || freebsd || netbsd || openbsd

package sysfs

import "golang.org/x/sys/unix"

// setTimes sets a directory's times by its descriptor, as "." in it. A
// regular file's are set by its name, while that still names it: the system
// calls that set them by a descriptor alone are not reachable here at a
// nanosecond's precision.
func (o *osFile) setTimes(t Times, st *Stat) Errno {
	if !o.isDir {
		return o.setTimesByName(t, st)
	}
	ts := []unix.Timespec{unix.NsecToTimespec(t.Atim), unix.NsecToTimespec(t.Mtim)}
	rc, err := o.f.SyscallConn()
	if err != nil {
		return ToErrno(err)
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = unix.UtimesNanoAt(int(fd), ".", ts, 0) }); err != nil {
		return ToErrno(err)
	}
	return ToErrno(opErr)
}
