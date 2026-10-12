//go:build quickjs_jit

package jit

import "unsafe"

// writeBarrier is the runtime's write-barrier flag, which compiled Go tests
// before every pointer store: set while the collector marks. The runtime
// keeps it for use outside, by this linkname, and promises not to change
// its type (runtime/mgc.go, go.dev/issue/67401); TestWriteBarrierFlag checks
// it still behaves as native code assumes.
//
//go:linkname writeBarrier runtime.writeBarrier
var writeBarrier struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// WriteBarrier is the address of the runtime's write-barrier flag, a byte
// that is not zero while the collector marks (abi.Encoding.WriteBarrier).
func WriteBarrier() uint64 { return uint64(uintptr(unsafe.Pointer(&writeBarrier.enabled))) }

// Marking reports whether the collector marks, which it starts and stops
// only while no native code runs.
func Marking() bool { return writeBarrier.enabled }
