// Package wat compiles the WebAssembly text format to the binary format.
//
// It covers the text format of WebAssembly 3.0 -- GC types and recursion
// groups, typed references, exceptions with try_table, tail calls, SIMD and
// relaxed SIMD, multiple memories and 64-bit addresses -- with the threads
// proposal's shared memories and atomic instructions and the legacy
// exception instructions (try, catch, catch_all, delegate, rethrow).
//
// Abbreviations are expanded to the binary the specification's reference
// interpreter writes for them. An inline signature that no type definition
// matches adds a function type at the end of the type section; such types
// follow every explicit one, field by field, and within a function its
// signature's comes first, then those of its instructions, last to first,
// a block's body before the block itself.
//
// The binary also carries a name section with the module's identifiers: the
// module's, the functions', their locals', and those of types, fields,
// tables, memories, globals, tags and segments. @name annotations name the
// module, functions and tags, and @custom annotations add custom sections
// where they say. Other annotations are ignored.
//
// Compile neither decodes nor validates the binary: a module that parses is
// encoded even if it is invalid.
package wat

import "github.com/go-quickjs/go-quickjs/wasm/internal/text"

// Error is a syntax error in a text module, at a 1-based line and column.
type Error = text.Error

// Compile compiles a text module to the binary format. src is either a
// (module ...) form or a module's fields alone. A syntax error is an *Error.
func Compile(src []byte) ([]byte, error) {
	return text.Compile(src)
}
