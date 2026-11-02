//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

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

// jitRegExpLitSite is jitRegExpLitPC's answer for the call at pc of fn's
// code, of argc arguments, given a RegExp as its first argument where arg
// is set and otherwise as its receiver, which jitRegExpLitPool asks once a
// site: the pc of the literal whose RegExp the call is given, or -1. A
// receiver that is no literal's is asked whether its call is a leaf's
// (jitRegExpLeafSeen).
func jitRegExpLitSite[_ any](r *Runtime, e *jitEntry, fn *bytecode.Function, pc, argc int, arg bool) int32 {
	lpc := int32(jitRegExpLitPC(e, fn, pc, argc, arg))
	if lpc < 0 && !arg {
		jitRegExpLeafSeen[struct{}](r, e, pc, argc)
	}
	return lpc
}

// jitRegExpLeafSeen notes, for e's next compile (jitFeedback.Leaf), the call
// at pc of exec or test, of argc arguments, whose operands are below the VM
// stack's top, where it is a leaf's: its check passes on them.
func jitRegExpLeafSeen[_ any](r *Runtime, e *jitEntry, pc, argc int) {
	sp := r.stackTop
	if r.meter != nil || argc < 0 || sp-argc-2 < 0 || sp > len(r.stack) {
		return
	}
	callee, this := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() {
		return
	}
	o := callee.Object()
	if o != r.regexpExecFn && o != r.regexpTestFn {
		return
	}
	fd := o.fn()
	if fd == nil || fd.elemOp != elemRegExpExec && fd.elemOp != elemRegExpTest || !leafChecks[fd.elemOp](r, this, r.stack[sp-argc:sp]) {
		return
	}
	for _, list := range [2][]jitInline{e.nativeCalls, e.inlines} {
		for k := range list {
			if int(list[k].pc) == pc && list[k].leaf {
				return
			}
		}
	}
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, leaf: true})
	e.inlinePending = true
}
