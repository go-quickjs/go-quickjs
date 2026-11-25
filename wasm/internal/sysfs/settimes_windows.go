package sysfs

import "golang.org/x/sys/windows"

var procReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// setTimes sets the file's times by its handle, reopened for writing its
// attributes, which a handle opened for reading cannot.
func (o *osFile) setTimes(t Times, _ *Stat) Errno {
	if err := procReOpenFile.Find(); err != nil {
		return ENOTSUP
	}
	var flags uintptr
	if o.isDir {
		flags = windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	r, _, err := procReOpenFile.Call(o.f.Fd(), windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, flags)
	h := windows.Handle(r)
	if h == windows.InvalidHandle {
		return ToErrno(err)
	}
	defer windows.CloseHandle(h)
	a, m := windows.NsecToFiletime(t.Atim), windows.NsecToFiletime(t.Mtim)
	return ToErrno(windows.SetFileTime(h, nil, &a, &m))
}
