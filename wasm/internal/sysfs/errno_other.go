//go:build (!unix || aix) && !windows

package sysfs

import "syscall"

func platformErrno(syscall.Errno) (Errno, bool) { return 0, false }
