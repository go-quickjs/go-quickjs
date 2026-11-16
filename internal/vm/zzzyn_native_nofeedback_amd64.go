//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64)) || quickjs_nativelayout

package vm

// nativeFeedback is plain in a build without the JIT, whose binary holds
// no feedback dispatcher: no runtime runs the JIT there (nativeDispatch).
// So it is in the build that reads the layout for the generator.
var nativeFeedback = nativePlain
