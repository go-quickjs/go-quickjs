//go:build !linux && !openbsd && !dragonfly && !solaris && !illumos && !darwin && !freebsd && !netbsd && !windows

package sysfs

func fillSys(*Stat, any) {}
