// Package internalapi holds what keeps the public wasm interfaces closed to
// implementations outside this module.
package internalapi

// WasmOnly is embedded in the public interfaces that only this module
// implements. Its method cannot be named outside this module, so no other
// type satisfies them.
type WasmOnly interface {
	wasmOnly()
}

// WasmOnlyType is embedded in the types that implement WasmOnly.
type WasmOnlyType struct{}

func (WasmOnlyType) wasmOnly() {}
