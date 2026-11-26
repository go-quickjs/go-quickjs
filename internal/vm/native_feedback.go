//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

// nativeFeedback is the dispatcher of a runtime that runs the JIT
// (nativeDispatch): its handlers record the JIT's feedback -- the calls'
// and runs' counts, the operand lattice (jit_lattice.go) -- every other
// handler plain's. Its table and its own handlers are
// generated (native_handlers_feedback_<arch>.s), in builds with the JIT
// alone, and not in the build that reads the package's layout for the
// generator (native_layout.go), which must not need them.
var nativeFeedback = newNativeTable(nativeFeedbackTableAddr())

// nativeFeedbackTableAddr is the feedback dispatcher's table's address.
func nativeFeedbackTableAddr() uintptr
