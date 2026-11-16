//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64)) && !quickjs_nativelayout

package vm

// nativeFeedback is the dispatcher of a runtime that runs the JIT
// (nativeDispatch): its handlers that record the JIT's feedback OR the
// kinds of value they met into the function's feedback slot (ctx.fv),
// every other handler plain's. Its table and its own handlers are
// generated (zzzyn_native_feedback_amd64.s), in builds with the JIT
// alone, and not in the build that reads the package's layout for the
// generator (zzzyn_native_layout.go), which must not need them.
var nativeFeedback = newNativeTable(nativeFeedbackTableAddr())

// nativeFeedbackTableAddr is the feedback dispatcher's table's address.
func nativeFeedbackTableAddr() uintptr
