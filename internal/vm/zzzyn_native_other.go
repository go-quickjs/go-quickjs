//go:build !amd64

package vm

// The native handlers are amd64's (zzzyn_native_amd64.go): every other
// architecture runs register frames by the Go table's loop alone.
const regNativeOn = false

// SetRegNative is amd64's switch of the native handlers: here there are
// none, and it reports they were off.
func SetRegNative(on bool) bool { return false }

func (r *Runtime) nativeRun(f *frame, c *regCode, start int) (Value, error) {
	panic("vm: no native handlers on this architecture")
}
