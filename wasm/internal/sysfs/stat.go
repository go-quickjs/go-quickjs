package sysfs

import "io/fs"

// StatOf describes the file fi does: what fi.Sys holds of the system's
// description where the platform's is known, its mode, size and
// modification time otherwise.
func StatOf(fi fs.FileInfo) Stat {
	mt := fi.ModTime().UnixNano()
	st := Stat{Mode: fi.Mode(), Size: fi.Size(), Nlink: 1, Atim: mt, Mtim: mt, Ctim: mt}
	if s, ok := fi.Sys().(*Stat); ok {
		return *s
	}
	fillSys(&st, fi.Sys())
	return st
}
