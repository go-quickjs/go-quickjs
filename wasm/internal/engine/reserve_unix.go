//go:build (linux || darwin || freebsd || netbsd || openbsd) && (amd64 || arm64 || riscv64 || ppc64le || s390x || loong64)

package engine

import "golang.org/x/sys/unix"

// reservable reports whether linear memory is reserved from the OS here.
const reservable = true

// reserve reserves size bytes of address space, inaccessible until
// committed. The memory is the OS's, outside the Go heap.
func reserve(size uint64) ([]byte, error) {
	return unix.Mmap(-1, 0, int(size), unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_NORESERVE)
}

// commit makes r[from:to] readable and writable; it reads as zeros.
func commit(r []byte, from, to uint64) error {
	if to <= from {
		return nil
	}
	start := from &^ (osPage - 1)
	return unix.Mprotect(r[start:to], unix.PROT_READ|unix.PROT_WRITE)
}

// release returns a reservation to the OS.
func release(r []byte) { _ = unix.Munmap(r) }
