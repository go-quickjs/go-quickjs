//go:build darwin || freebsd || netbsd

package sysfs

import "syscall"

func fillSys(st *Stat, sys any) {
	s, ok := sys.(*syscall.Stat_t)
	if !ok {
		return
	}
	st.Dev, st.Ino, st.Nlink = uint64(s.Dev), uint64(s.Ino), uint64(s.Nlink)
	st.Atim, st.Ctim = s.Atimespec.Nano(), s.Ctimespec.Nano()
}
