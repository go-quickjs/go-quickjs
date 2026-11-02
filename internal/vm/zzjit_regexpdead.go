//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Native code's calls of exec whose result nothing reads but, at most,
// whether it is truthy (see zzz_regexpdead.go) tell exec's leaf so, as the
// tree tier's do: a leaf's call (ssa.GoLeaf) by its description, which the
// SSA builder marks from the instruction after the call
// (ssa.CallSite's Unused, abi.GoCallState's), its form's function telling
// the leaf just before it calls it (jitRegExpDeadNative); a call of Go -- a
// RegExp literal's, one not yet learned a leaf's -- by the instruction
// after it in the code's bytecode (jitCallMadeDead). The call is made as
// it was, by the same path, errors and all: only the array exec makes is
// not made.
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

// jitCallMadeDead is jitCallMade's call of in, on the VM's stack below
// sp, at pc of f's code: of the realm's exec whose result nothing reads
// (deadResultAt), its leaf told so, of the receiver, and untold after.
func jitCallMadeDead[_ any](r *Runtime, f *frame, pc int, in bytecode.Instr, sp int) (Value, error) {
	dead := r.regexpExecDead
	if dead == nil || in.Op != bytecode.OpCallMethod || r.meter != nil || r.debug != nil || f == nil || f.cl == nil {
		return r.jitCallMade(in, sp)
	}
	argc := int(in.A)
	if sp-argc-2 < 0 || sp > len(r.stack) {
		return r.jitCallMade(in, sp)
	}
	callee, recv := r.stack[sp-argc-1], r.stack[sp-argc-2]
	code := f.cl.fn.Code
	if !callee.IsObject() || callee.Object() != r.regexpExecFn || !recv.IsObject() ||
		pc >= len(code) || code[pc].Op != bytecode.OpCallMethod || !deadResultAt[struct{}](code, pc) {
		return r.jitCallMade(in, sp)
	}
	*dead = recv.Object()
	v, err := r.jitCallMade(in, sp)
	*dead = nil
	return v, err
}
