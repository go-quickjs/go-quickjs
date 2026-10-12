//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (darwin && arm64))

package jit

import "golang.org/x/sys/unix"

// mapCode maps size bytes, zeroed, readable and writable, for an Arena.
func mapCode(size int) ([]byte, error) {
	return unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
}

// protectCode makes whole pages executable and not writable, or writable
// and not executable.
func protectCode(pages []byte, executable bool) error {
	if executable {
		return unix.Mprotect(pages, unix.PROT_READ|unix.PROT_EXEC)
	}
	return unix.Mprotect(pages, unix.PROT_READ|unix.PROT_WRITE)
}

func unmapCode(mem []byte) error { return unix.Munmap(mem) }
