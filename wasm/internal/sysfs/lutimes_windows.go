package sysfs

import "golang.org/x/sys/windows"

// lutimes sets the times of the symbolic link at host path h itself.
func lutimes(h string, t Times) Errno {
	name, err := windows.UTF16PtrFromString(h)
	if err != nil {
		return EINVAL
	}
	hd, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return ToErrno(err)
	}
	defer windows.CloseHandle(hd)
	var a, m *windows.Filetime
	if t.SetAtim {
		ft := windows.NsecToFiletime(t.Atim)
		a = &ft
	}
	if t.SetMtim {
		ft := windows.NsecToFiletime(t.Mtim)
		m = &ft
	}
	return ToErrno(windows.SetFileTime(hd, nil, a, m))
}
