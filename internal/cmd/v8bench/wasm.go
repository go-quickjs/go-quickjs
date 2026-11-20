//go:build quickjs_wasm

package main

import (
	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// A build with the quickjs_wasm tag links the WebAssembly engine and
// installs its namespace in every runtime, as qjs does, so that the
// benchmarks can be timed in the program a host that offers WebAssembly
// builds.
func init() {
	installWasm = func(rt *quickjs.Runtime) error { return wasmjs.Install(rt) }
}
