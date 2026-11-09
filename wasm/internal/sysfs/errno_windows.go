package sysfs

import "syscall"

// Windows error codes the syscall package does not name.
const (
	errorInvalidHandle    = syscall.Errno(6)
	errorNotSupported     = syscall.Errno(50)
	errorNotSameDevice    = syscall.Errno(17)
	errorSharingViolation = syscall.Errno(32)
	errorLockViolation    = syscall.Errno(33)
	errorDiskFull         = syscall.Errno(112)
	errorInvalidName      = syscall.Errno(123)
	errorNegativeSeek     = syscall.Errno(131)
	errorBadPathname      = syscall.Errno(161)
	errorFilenameExcedRng = syscall.Errno(206)
	errorDirectory        = syscall.Errno(267)
	errorCantResolve      = syscall.Errno(1921)
	errorNotAReparsePoint = syscall.Errno(4390)
)

// windowsErrno maps the Windows error codes file operations return.
func windowsErrno(e syscall.Errno) (Errno, bool) {
	switch e {
	case syscall.ERROR_FILE_NOT_FOUND, syscall.ERROR_PATH_NOT_FOUND:
		return ENOENT, true
	case syscall.ERROR_ACCESS_DENIED:
		return EACCES, true
	case syscall.ERROR_ALREADY_EXISTS, syscall.ERROR_FILE_EXISTS:
		return EEXIST, true
	case syscall.ERROR_DIR_NOT_EMPTY:
		return ENOTEMPTY, true
	case syscall.ERROR_PRIVILEGE_NOT_HELD:
		return EPERM, true
	case syscall.ERROR_HANDLE_EOF:
		return 0, true
	case syscall.ERROR_BROKEN_PIPE:
		return EPIPE, true
	case errorNotSupported:
		return ENOTSUP, true
	case errorInvalidHandle:
		return EBADF, true
	case errorNotSameDevice:
		return EXDEV, true
	case errorSharingViolation, errorLockViolation:
		return EBUSY, true
	case errorDiskFull:
		return ENOSPC, true
	case errorInvalidName, errorBadPathname, errorNegativeSeek, errorNotAReparsePoint:
		return EINVAL, true
	case errorFilenameExcedRng:
		return ENAMETOOLONG, true
	case errorDirectory:
		return ENOTDIR, true
	case errorCantResolve:
		return ELOOP, true
	}
	return 0, false
}
