//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

// Native code's calls of exec, and of a string's replace, whose result
// nothing reads but, at most, whether it is truthy (see
// zzz_regexpdead.go) tell the function so, as the interpreter's do: a leaf's
// call of exec (ssa.GoLeaf) by its description, which the SSA builder
// marks from the instruction after the call (ssa.CallSite's Unused,
// abi.GoCallState's), its form's function telling the leaf just before it
// calls it (jitRegExpDeadNative); a call of Go -- replace's, a RegExp
// literal's exec, one not yet learned a leaf's -- by the instruction after
// it in the code's bytecode (jitCallMadeDead). The call is made as it was,
// by the same path, errors and all: only the array exec makes, or the
// string replace makes, is not made.
//
// The functions are generic, each instantiated with struct{} alone.

// jitRegExpDeadNative is the function a leaf's call's form calls for
// native, the leaf o's function, where nothing reads the call's result
// (abi.GoCallState's Unused): native itself but for the realm's exec,
// whose leaf it tells, of the call's receiver, just before it calls it.
func jitRegExpDeadNative[_ any](r *Runtime, o *Object, native NativeFunc) NativeFunc {
	dead := r.regexpExecDead
	if o != r.regexpExecFn || dead == nil || native == nil {
		return native
	}
	return func(r *Runtime, this Value, args []Value) (Value, error) {
		if this.IsObject() {
			*dead = this.Object()
		}
		return native(r, this, args)
	}
}
