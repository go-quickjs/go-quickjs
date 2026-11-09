//go:build !((linux || darwin || freebsd || netbsd || openbsd) && (amd64 || arm64 || riscv64 || ppc64le || s390x || loong64)) && !(windows && (amd64 || arm64))

package engine

import "errors"

// reservable reports whether linear memory is reserved from the OS here: on
// 32-bit platforms, and those without a reservation to make, it is a Go slice
// that moves when it grows past its capacity.
const reservable = false

func reserve(size uint64) ([]byte, error) { return nil, errors.ErrUnsupported }

func commit(r []byte, from, to uint64) error { return errors.ErrUnsupported }

func release(r []byte) {}
