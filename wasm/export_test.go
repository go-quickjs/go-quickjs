package wasm

import "github.com/go-quickjs/go-quickjs/wasm/internal/engine"

// JITMemoryOf returns what the compiler holds for a compiled module, for
// tests.
func JITMemoryOf(cm CompiledModule) engine.JITMemory {
	return cm.(*compiledModule).code.code.JITMemory()
}

// SameCode reports whether two compiled modules are one module's code, as
// a compilation cache shares it, for tests.
func SameCode(a, b CompiledModule) bool { return a.(*compiledModule).code == b.(*compiledModule).code }
