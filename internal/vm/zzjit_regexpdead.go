//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Native code's calls of exec, and of a string's replace, whose result
// nothing reads but, at most, whether it is truthy (see
// zzz_regexpdead.go) tell the function so, as the tree tier's do: a leaf's
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

// jitCallMadeDead is jitCallMade's call of in, on the VM's stack below
// sp, at pc of f's code: of the realm's exec or replace whose result
// nothing reads (deadResultAt), the function told so, of exec's receiver
// or replace's RegExp, and untold after.
func jitCallMadeDead[_ any](r *Runtime, f *frame, pc int, in bytecode.Instr, sp int) (Value, error) {
	argc := int(in.A)
	if in.Op != bytecode.OpCallMethod || r.meter != nil || r.debug != nil || f == nil || f.cl == nil || sp-argc-2 < 0 || sp > len(r.stack) {
		return r.jitCallMade(in, sp)
	}
	callee := r.stack[sp-argc-1]
	if !callee.IsObject() {
		return r.jitCallMade(in, sp)
	}
	var dead **Object
	var mark Value
	switch callee.Object() {
	case r.regexpExecFn:
		dead, mark = r.regexpExecDead, r.stack[sp-argc-2]
	case r.strReplaceFn:
		if argc >= 2 {
			dead, mark = r.strReplaceDead, r.stack[sp-argc]
		}
	}
	code := f.cl.fn.Code
	if dead == nil || !mark.IsObject() || pc >= len(code) || code[pc].Op != bytecode.OpCallMethod || !deadResultAt[struct{}](code, pc) {
		return r.jitCallMade(in, sp)
	}
	*dead = mark.Object()
	v, err := r.jitCallMade(in, sp)
	*dead = nil
	return v, err
}
