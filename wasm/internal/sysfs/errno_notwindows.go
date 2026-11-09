//go:build !windows

package sysfs

import "syscall"

func windowsErrno(syscall.Errno) (Errno, bool) { return 0, false }
