package sysfs

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// setTimes sets the file's times by its descriptor: utimensat with no path
// acts on the descriptor itself.
func (o *osFile) setTimes(t Times, _ *Stat) Errno {
	ts := [2]unix.Timespec{unix.NsecToTimespec(t.Atim), unix.NsecToTimespec(t.Mtim)}
	rc, err := o.f.SyscallConn()
	if err != nil {
		return ToErrno(err)
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall6(unix.SYS_UTIMENSAT, fd, 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	}); err != nil {
		return ToErrno(err)
	}
	if errno != 0 {
		return ToErrno(errno)
	}
	return 0
}
