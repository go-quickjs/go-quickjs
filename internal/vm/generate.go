package vm

// go generate runs a package's directives in the order of its files'
// names: the JIT's snapshot of its feedback here first, then the native
// handlers (native_run.go, on amd64 and arm64), then the layout lock's
// pads (z_layoutpad.go, which sorts last), which measure what the others
// write.
//
//go:generate go run ./internal/snapgen/cmd
