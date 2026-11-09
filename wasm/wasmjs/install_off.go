//go:build !quickjs_wasm

package wasmjs

import quickjs "github.com/go-quickjs/go-quickjs"

// Install would define the global WebAssembly on rt; this build has no
// WebAssembly, and it returns ErrNotBuilt.
func Install(rt *quickjs.Runtime, opts ...Option) error { return ErrNotBuilt }
