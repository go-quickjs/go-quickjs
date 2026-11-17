//go:build (amd64 || arm64) && quickjs_nativelayout

package vm

// nativeTyped is plain in the build that reads the package's layout for
// the generator, which has no typed dispatcher: it runs no code.
var nativeTyped = nativePlain
