//go:build !windows

package sysfs

// base returns the host path the directory had when it was opened. Paths
// are resolved by name only where the system has no *at calls, other than
// Windows: there a directory a module opened and then moved is not found.
func (d *dirFS) base() string { return d.dir }
