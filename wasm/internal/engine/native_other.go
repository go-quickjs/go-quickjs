//go:build !amd64 && !arm64

package engine

import "unsafe"

// nativeBuilt says the native handlers are built for this architecture.
const nativeBuilt = false

var nativeStoreRefAddr, nativeAllocAddr uintptr

func nativeRun(f *frame, rc *regCode, pc int32) { panic("wasm: no native handlers") }

// wasmNativeTablePtr is the native table: none here.
func wasmNativeTablePtr() unsafe.Pointer { return nil }

// The SIMD table the contexts are given and the exit stub's alone: none
// here.
var (
	nativeSIMDTable uintptr
	nativeSIMDExits [256]uintptr
)

// The exit stub's table of Go handlers and its run of them: none here.
var nativeExitTable [256]regHandler

func nativeExitRun(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	panic("wasm: no native handlers")
}
