//go:build !amd64 && !arm64

package vm

// nativeHandlerTablesForTest is nil: the native handlers are amd64's and
// arm64's.
func nativeHandlerTablesForTest() []string { return nil }
