package sysfs

import (
	"strings"

	"golang.org/x/sys/windows"
)

// base returns the host path the directory has now: for a directory a
// module opened, where its handle finds it, wherever it was moved since.
func (d *dirFS) base() string {
	if d.handle == nil {
		return d.dir
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(d.handle.Fd()), &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return d.dir
		}
		if int(n) < len(buf) {
			p := windows.UTF16ToString(buf[:n])
			if rest, ok := strings.CutPrefix(p, `\?\UNC\`); ok {
				return `\` + rest
			}
			return strings.TrimPrefix(p, `\?\`)
		}
		buf = make([]uint16, n)
	}
}
