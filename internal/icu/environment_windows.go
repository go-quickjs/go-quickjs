//go:build windows

package icu

import (
	"syscall"
	"unsafe"
)

const localeNameMaxLength = 85

var getUserDefaultLocaleName = syscall.NewLazyDLL("kernel32.dll").
	NewProc("GetUserDefaultLocaleName")

func systemLocale() string {
	var name [localeNameMaxLength]uint16
	written, _, _ := getUserDefaultLocaleName.Call(
		uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if written == 0 {
		return ""
	}
	return tagFromWindows(syscall.UTF16ToString(name[:]))
}
