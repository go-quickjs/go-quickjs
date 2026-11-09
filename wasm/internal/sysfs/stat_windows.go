package sysfs

import "syscall"

func fillSys(st *Stat, sys any) {
	s, ok := sys.(*syscall.Win32FileAttributeData)
	if !ok {
		return
	}
	st.Atim = s.LastAccessTime.Nanoseconds()
	st.Ctim = s.LastWriteTime.Nanoseconds()
}
