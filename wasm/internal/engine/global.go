package engine

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// V128 is a 128-bit vector, its low lanes in Lo.
type V128 struct {
	Lo, Hi uint64
}

// Global is a global variable. A number is held in Num -- an i32 or f32 in its
// low 32 bits, an f32 or f64 as its bits -- a vector in Num and Hi, a
// reference in Ref.
type Global struct {
	Num uint64
	Hi  uint64
	Ref Ref
	// Type is the global's type, Module the module whose types it names.
	Type    module.ValueType
	Module  *module.Module
	Mutable bool
}
