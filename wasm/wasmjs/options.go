// Package wasmjs gives a JavaScript runtime the WebAssembly namespace: the
// WebAssembly JavaScript interface over the module's own wasm engine.
//
// It is opt-in twice over. It is built only with the quickjs_wasm build tag,
// as the JIT is with quickjs_jit: without it, Install returns ErrNotBuilt
// and nothing of the wasm engine is linked. And the JavaScript engine does
// not import the wasm engine; a host that wants WebAssembly calls Install,
// which defines the global, built the first time a script reads it:
//
//	// go build -tags quickjs_wasm
//	rt := quickjs.New()
//	if err := wasmjs.Install(rt); err != nil { ... }
//	rt.Eval(`new WebAssembly.Module(bytes)`)
//
// The namespace follows the WebAssembly JavaScript Interface, its JS Promise
// Integration and its JS String Builtins, with the error types and messages
// Node gives. A runtime made WithNodeQuirks validates modules against V8's
// limits rather than the interface's: a memory64 may declare at most 262,144
// pages, where the interface allows 2^37 - 1, and a module may declare up to
// 100,000 memories, where the interface allows 100.
package wasmjs

import "errors"

// ErrNotBuilt is what Install returns in a build without the quickjs_wasm
// tag, which has no WebAssembly.
var ErrNotBuilt = errors.New("wasmjs: this build has no WebAssembly (build with -tags quickjs_wasm)")

// Option configures Install.
type Option func(*config)

type config struct {
	noJSPI bool
}

// WithoutJSPI leaves out the JS Promise Integration: WebAssembly.Suspending,
// WebAssembly.promising and WebAssembly.SuspendError.
func WithoutJSPI() Option { return func(c *config) { c.noJSPI = true } }
