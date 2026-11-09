//go:build windows && (amd64 || arm64)

package engine

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// reservable reports whether linear memory is reserved from the OS here.
const reservable = true

// reserve reserves size bytes of address space, inaccessible until
// committed. The memory is the OS's, outside the Go heap.
func reserve(size uint64) ([]byte, error) {
	addr, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_RESERVE, windows.PAGE_NOACCESS)
	if err != nil {
		return nil, err
	}
	return osBytes(addr, size), nil
}

// commit makes r[from:to] readable and writable; it reads as zeros.
func commit(r []byte, from, to uint64) error {
	if to <= from {
		return nil
	}
	start := from &^ (osPage - 1)
	base := uintptr(unsafe.Pointer(unsafe.SliceData(r)))
	_, err := windows.VirtualAlloc(base+uintptr(start), uintptr(to-start), windows.MEM_COMMIT, windows.PAGE_READWRITE)
	return err
}

// release returns a reservation to the OS.
func release(r []byte) {
	_ = windows.VirtualFree(uintptr(unsafe.Pointer(unsafe.SliceData(r))), 0, windows.MEM_RELEASE)
}

// osBytes returns the OS memory at addr as a slice. The address is the OS's,
// not a Go object's, so it is read as a pointer rather than converted.
func osBytes(addr uintptr, size uint64) []byte {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return unsafe.Slice((*byte)(p), size)
}
