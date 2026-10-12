//go:build quickjs_jit && !android && !ios && amd64

package jit

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var flushInstructionCache = windows.NewLazySystemDLL("kernel32.dll").NewProc("FlushInstructionCache")

func executablePolicy() error { return nil }

// mapCode reserves and commits size bytes, zeroed, readable and writable,
// for an Arena.
func mapCode(size int) ([]byte, error) {
	address, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return nil, err
	}
	return foreignBytes(address, size), nil
}

// protectCode makes whole pages executable and not writable, or writable
// and not executable.
func protectCode(pages []byte, executable bool) error {
	protect := uint32(windows.PAGE_READWRITE)
	if executable {
		protect = windows.PAGE_EXECUTE_READ
	}
	var old uint32
	return windows.VirtualProtect(uintptr(unsafe.Pointer(&pages[0])), uintptr(len(pages)), protect, &old)
}

func flushCode(code []byte) error {
	ok, _, err := flushInstructionCache.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&code[0])), uintptr(len(code)))
	if ok == 0 {
		return err
	}
	return nil
}

func unmapCode(mem []byte) error {
	return windows.VirtualFree(uintptr(unsafe.Pointer(&mem[0])), 0, windows.MEM_RELEASE)
}

// foreignBytes represents a VirtualAlloc allocation as a slice. Its address
// refers to OS-owned memory, not a Go object recovered from a uintptr.
//
//go:noescape
func foreignBytes(address uintptr, size int) []byte
