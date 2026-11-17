//go:build (amd64 || arm64) && !quickjs_nativelayout

package vm

// nativeTyped is the dispatcher of typed code (zzzyn_native_typed.go): its
// table is generated (zzzyn_native_typed_<arch>.s) in every build but the
// one that reads the package's layout for the generator, which must not
// need it.
var nativeTyped = newNativeTable(nativeTypedTableAddr())

// nativeTypedTableAddr is the typed dispatcher's table's address.
func nativeTypedTableAddr() uintptr
