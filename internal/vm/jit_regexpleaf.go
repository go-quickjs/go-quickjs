//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

// Native code's call of RegExp.prototype's exec or test (zzz_regexpleaf.go)
// is compiled a leaf's call of Go of its own (ssa.GoLeaf), made through the
// VM's routine for leaves without a frame, where its site's cache holds the
// built-in at the compile (jitRegFeedback's Leaf). A literal's call is one
// too, its RegExp taken from the literal's pool, to which the leaf gives it
// back (jitRegExpLeafGive).
//
// The functions are generic, instantiated with struct{} alone: Go lays a
// generic function's instantiations out after every function and closure of
// the package, so that every function and closure stays at its address (go
// tool nm).

// jitRegExpNativeCheck is the check native code's leaf's call of exec or
// test, op, asks (regexpNativeChecks), which passes on a global or a sticky
// RegExp too: one leafChecks refuses -- a site of exec that met both kinds
// was a leaf's call, refused at every global RegExp's, each an exit to Go,
// the rest of the invocation the interpreter's (Octane RegExp's Exec, 28K
// times a run). nil for any other op.
func jitRegExpNativeCheck[_ any](op uint8) func(r *Runtime, this Value, args []Value) bool {
	switch op {
	case elemRegExpExec:
		return regexpNativeChecks[0]
	case elemRegExpTest:
		return regexpNativeChecks[1]
	}
	return nil
}
