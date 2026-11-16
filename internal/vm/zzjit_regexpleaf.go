//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

// Native code's call of RegExp.prototype's exec or test (zzz_regexpleaf.go)
// on a RegExp that is no literal's is learned to be a leaf's at the call of
// Go that makes it, once a site, and compiled next a leaf's call of Go of its
// own (ssa.GoLeaf), made through the VM's routine for leaves without a
// frame, as jitLeafSeen learns the leaves jitGoCallPlain makes. A literal's
// call stays a call of Go, which gives the literal's pool its RegExp back
// (jitRegExpLitPool).
//
// The functions are generic, instantiated with struct{} alone, and called
// where jitRegExpLitPool's call of jitRegExpLitPC was: its code grows by
// the dictionary and the runtime, within the padding that rounds it to 32
// bytes, and every function and closure of the package stays at its
// address (go tool nm).

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
