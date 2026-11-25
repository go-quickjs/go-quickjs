//go:build !(linux || windows || darwin || freebsd || netbsd || openbsd)

package sysfs

// setTimes sets the file's times by its name, while that still names it.
func (o *osFile) setTimes(t Times, st *Stat) Errno { return o.setTimesByName(t, st) }
