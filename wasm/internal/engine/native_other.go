//go:build !amd64

package engine

import "unsafe"

// nativeBuilt says the native handlers are built for this architecture.
const nativeBuilt = false

func nativeRun(f *frame, rc *regCode, pc int32) { panic("wasm: no native handlers") }

// wasmNativeTablePtr is the native table: none here.
func wasmNativeTablePtr() unsafe.Pointer { return nil }
