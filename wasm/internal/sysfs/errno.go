package sysfs

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"syscall"
)

// Errno is a WASI preview 1 error number. Zero is success.
type Errno uint16

// The error numbers of WASI preview 1, in the order of its errno enum.
const (
	ESUCCESS Errno = iota
	E2BIG
	EACCES
	EADDRINUSE
	EADDRNOTAVAIL
	EAFNOSUPPORT
	EAGAIN
	EALREADY
	EBADF
	EBADMSG
	EBUSY
	ECANCELED
	ECHILD
	ECONNABORTED
	ECONNREFUSED
	ECONNRESET
	EDEADLK
	EDESTADDRREQ
	EDOM
	EDQUOT
	EEXIST
	EFAULT
	EFBIG
	EHOSTUNREACH
	EIDRM
	EILSEQ
	EINPROGRESS
	EINTR
	EINVAL
	EIO
	EISCONN
	EISDIR
	ELOOP
	EMFILE
	EMLINK
	EMSGSIZE
	EMULTIHOP
	ENAMETOOLONG
	ENETDOWN
	ENETRESET
	ENETUNREACH
	ENFILE
	ENOBUFS
	ENODEV
	ENOENT
	ENOEXEC
	ENOLCK
	ENOLINK
	ENOMEM
	ENOMSG
	ENOPROTOOPT
	ENOSPC
	ENOSYS
	ENOTCONN
	ENOTDIR
	ENOTEMPTY
	ENOTRECOVERABLE
	ENOTSOCK
	ENOTSUP
	ENOTTY
	ENXIO
	EOVERFLOW
	EOWNERDEAD
	EPERM
	EPIPE
	EPROTO
	EPROTONOSUPPORT
	EPROTOTYPE
	ERANGE
	EROFS
	ESPIPE
	ESRCH
	ESTALE
	ETIMEDOUT
	ETXTBSY
	EXDEV
	ENOTCAPABLE
)

var errnoNames = [...]string{
	"ESUCCESS", "E2BIG", "EACCES", "EADDRINUSE", "EADDRNOTAVAIL", "EAFNOSUPPORT",
	"EAGAIN", "EALREADY", "EBADF", "EBADMSG", "EBUSY", "ECANCELED", "ECHILD",
	"ECONNABORTED", "ECONNREFUSED", "ECONNRESET", "EDEADLK", "EDESTADDRREQ",
	"EDOM", "EDQUOT", "EEXIST", "EFAULT", "EFBIG", "EHOSTUNREACH", "EIDRM",
	"EILSEQ", "EINPROGRESS", "EINTR", "EINVAL", "EIO", "EISCONN", "EISDIR",
	"ELOOP", "EMFILE", "EMLINK", "EMSGSIZE", "EMULTIHOP", "ENAMETOOLONG",
	"ENETDOWN", "ENETRESET", "ENETUNREACH", "ENFILE", "ENOBUFS", "ENODEV",
	"ENOENT", "ENOEXEC", "ENOLCK", "ENOLINK", "ENOMEM", "ENOMSG", "ENOPROTOOPT",
	"ENOSPC", "ENOSYS", "ENOTCONN", "ENOTDIR", "ENOTEMPTY", "ENOTRECOVERABLE",
	"ENOTSOCK", "ENOTSUP", "ENOTTY", "ENXIO", "EOVERFLOW", "EOWNERDEAD", "EPERM",
	"EPIPE", "EPROTO", "EPROTONOSUPPORT", "EPROTOTYPE", "ERANGE", "EROFS",
	"ESPIPE", "ESRCH", "ESTALE", "ETIMEDOUT", "ETXTBSY", "EXDEV", "ENOTCAPABLE",
}

// Name returns the errno's name, such as "ENOENT", or "errno(N)" for a
// number WASI does not define.
func (e Errno) Name() string {
	if int(e) < len(errnoNames) {
		return errnoNames[e]
	}
	return "errno(" + itoa(uint64(e)) + ")"
}

func (e Errno) Error() string { return e.Name() }

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

// ToErrno returns the errno of err, an error of the os, io/fs or syscall
// packages: zero for nil and io.EOF, EIO for an error it does not know.
func ToErrno(err error) Errno {
	if err == nil || err == io.EOF {
		return 0
	}
	var e Errno
	if errors.As(err, &e) {
		return e
	}
	var se syscall.Errno
	if errors.As(err, &se) {
		if e, ok := platformErrno(se); ok {
			return e
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ENOENT
	case errors.Is(err, fs.ErrExist):
		return EEXIST
	case errors.Is(err, fs.ErrPermission):
		return EPERM
	case errors.Is(err, fs.ErrInvalid):
		return EINVAL
	case errors.Is(err, fs.ErrClosed):
		return EBADF
	case strings.Contains(err.Error(), "path escapes from parent"):
		// os.Root's error for a path or a symbolic link that leaves it.
		return EPERM
	}
	return EIO
}
