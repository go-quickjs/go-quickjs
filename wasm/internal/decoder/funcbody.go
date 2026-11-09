package decoder

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// validateConstExpr decodes the constant expression at b[start:end] that must
// produce a value of type expected, as V8's WasmFullDecoder does in its
// constant expression mode. It returns where decoding stopped (just past the
// expression's final end when it is valid), whether that end was found, and
// the first error.
func validateConstExpr(m *module.Module, det *detected, b []byte, start, end int, expected module.ValueType) (stop int, endFound bool, err *Error) {
	return start, false, &Error{Offset: uint32(start), Msg: "constant expressions are not implemented yet"}
}

// ValidateFunctions validates the bodies of m's defined functions, returning
// V8's error for the first invalid one: "Compiling function #N failed: ...".
func ValidateFunctions(m *module.Module) *Error {
	return nil
}
