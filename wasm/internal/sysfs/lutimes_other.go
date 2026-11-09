//go:build !windows

package sysfs

// lutimes sets the times of the symbolic link at host path h itself, which
// the platform does not let the package do.
func lutimes(string, Times) Errno { return ENOTSUP }
