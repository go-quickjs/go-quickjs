//go:build !amd64 && !arm64

package vm

// nativeCounters are a test's counts of the native code's work
// (native_run.go's): here there is none to count, and no runtime counts.
type nativeCounters struct{}

// The native handlers are amd64's and arm64's (native_run.go):
// every other architecture runs register frames by the Go table's loop
// alone, whatever a test asks (testConfig).
const (
	regNativeDefault = false
	regNativeBuilt   = false
)

func (r *Runtime) nativeRun(f *frame, c *regCode, start int) (Value, error) {
	panic("vm: no native handlers on this architecture")
}

// A callback has no native code entry here (regCallback).
func (r *Runtime) nativeCallback(c *regCode) (*regState, *nativeCtx) { return nil, nil }

func (r *Runtime) nativeLoop(st *regState, ctx *nativeCtx, f *frame, c *regCode, start int) (Value, error) {
	panic("vm: no native handlers on this architecture")
}

func nativeReset(ctx *nativeCtx) {}

func (r *Runtime) nativeCallbackRun(cb *regCallback, f *frame, regs *[1 << 16]Value) (Value, error) {
	panic("vm: no native handlers on this architecture")
}

func (r *Runtime) nativeRunTyped(f *frame, c *regCode, start int) (Value, error) {
	panic("vm: no native handlers on this architecture")
}
