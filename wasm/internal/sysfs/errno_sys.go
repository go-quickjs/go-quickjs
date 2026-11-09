//go:build (unix && !aix) || windows

package sysfs

import "syscall"

// syscallErrnos maps the system's error numbers to WASI's.
var syscallErrnos = map[syscall.Errno]Errno{
	syscall.E2BIG:        E2BIG,
	syscall.EACCES:       EACCES,
	syscall.EADDRINUSE:   EADDRINUSE,
	syscall.EAGAIN:       EAGAIN,
	syscall.EALREADY:     EALREADY,
	syscall.EBADF:        EBADF,
	syscall.EBUSY:        EBUSY,
	syscall.ECANCELED:    ECANCELED,
	syscall.ECHILD:       ECHILD,
	syscall.ECONNREFUSED: ECONNREFUSED,
	syscall.ECONNRESET:   ECONNRESET,
	syscall.EDQUOT:       EDQUOT,
	syscall.EEXIST:       EEXIST,
	syscall.EFAULT:       EFAULT,
	syscall.EFBIG:        EFBIG,
	syscall.EINTR:        EINTR,
	syscall.EINVAL:       EINVAL,
	syscall.EIO:          EIO,
	syscall.EISDIR:       EISDIR,
	syscall.ELOOP:        ELOOP,
	syscall.EMFILE:       EMFILE,
	syscall.EMLINK:       EMLINK,
	syscall.ENAMETOOLONG: ENAMETOOLONG,
	syscall.ENFILE:       ENFILE,
	syscall.ENODEV:       ENODEV,
	syscall.ENOENT:       ENOENT,
	syscall.ENOMEM:       ENOMEM,
	syscall.ENOSPC:       ENOSPC,
	syscall.ENOSYS:       ENOSYS,
	syscall.ENOTDIR:      ENOTDIR,
	syscall.ENOTEMPTY:    ENOTEMPTY,
	syscall.ENOTSOCK:     ENOTSOCK,
	syscall.ENOTSUP:      ENOTSUP,
	syscall.ENOTTY:       ENOTTY,
	syscall.ENXIO:        ENXIO,
	syscall.EPERM:        EPERM,
	syscall.EPIPE:        EPIPE,
	syscall.ERANGE:       ERANGE,
	syscall.EROFS:        EROFS,
	syscall.ESPIPE:       ESPIPE,
	syscall.ESRCH:        ESRCH,
	syscall.ETIMEDOUT:    ETIMEDOUT,
	syscall.ETXTBSY:      ETXTBSY,
	syscall.EXDEV:        EXDEV,
}

func platformErrno(e syscall.Errno) (Errno, bool) {
	if w, ok := windowsErrno(e); ok {
		return w, true
	}
	w, ok := syscallErrnos[e]
	return w, ok
}
