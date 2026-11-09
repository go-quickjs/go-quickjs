//go:build !((linux || darwin || freebsd || netbsd || openbsd) && (amd64 || arm64 || riscv64 || ppc64le || s390x || loong64 || 386 || arm)) && !(windows && (amd64 || arm64 || 386))

package engine

import "errors"

// canReserve reports whether address space can be reserved from the OS here:
// where it cannot, every memory is a Go slice.
const canReserve = false

func reserve(size uint64) ([]byte, error) { return nil, errors.ErrUnsupported }

func commit(r []byte, from, to uint64) error { return errors.ErrUnsupported }

func release(r []byte) {}
