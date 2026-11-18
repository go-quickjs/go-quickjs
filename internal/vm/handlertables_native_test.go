//go:build amd64 || arm64

package vm

// nativeHandlerTablesForTest lists the native handlers' exit tables for
// TestHandlerTables.
func nativeHandlerTablesForTest() []string {
	var out []string
	out = append(out, handlerTableLines("nativeExitTable", &nativeExitTable)...)
	out = append(out, handlerTableLines("nativeTypedExitTable", &nativeTypedExitTable)...)
	out = append(out, handlerTableLines("nativeTypedSlowTable", &nativeTypedSlowTable)...)
	return out
}
