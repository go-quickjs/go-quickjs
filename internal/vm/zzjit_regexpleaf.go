//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

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
// (jitRegExpLeafSeen), and so is a string's replace given a RegExp that is
// no literal's (jitReplaceLeafSeen).
func jitRegExpLitSite[_ any](r *Runtime, e *jitEntry, fn *bytecode.Function, pc, argc int, arg bool) int32 {
	lpc := int32(jitRegExpLitPC(e, fn, pc, argc, arg))
	switch {
	case lpc >= 0:
	case !arg:
		jitRegExpLeafSeen[struct{}](r, e, pc, argc)
	default:
		// A string's replace (zzjit_regexpreplaceleaf.go).
		jitReplaceLeafSeen[struct{}](r, e, pc, argc)
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
	if fd == nil || fd.elemOp != elemRegExpExec && fd.elemOp != elemRegExpTest || !jitRegExpNativeCheck[struct{}](fd.elemOp)(r, this, r.stack[sp-argc:sp]) {
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

// jitRegExpLeafGuess is the function a compile's call at pc of cl's code,
// whose site has no leaf learned yet, is guessed to call as a leaf's
// (jitFeedback.Leaf): RegExp.prototype's exec or test, where the call's
// method read -- the get_prop_this its callee is, from the instructions
// before the call -- is of a RegExp no literal's that a global or a
// captured binding holds now (jitChainReceiver), and finds there the
// built-in, its check passing on it given a string. A loop's re.exec(s)
// was learned to be a leaf's at its call of Go, the function compiled
// again for it: one compile more for each loop that first ran such a call.
// So is a string's replace whose RegExp a global or a captured binding holds
// (jitReplaceLeafGuess). The guess is a guess: the code checks the function
// each call calls, and the leaf's routine its check, refusing what fails
// them.
func jitRegExpLeafGuess[_ any](r *Runtime, cl *closure, p *ir.Program, pc int) *Object {
	fn := cl.fn
	if p == nil || len(p.Maps) != len(fn.Code) || pc >= len(fn.Code) || fn.Code[pc].Op != bytecode.OpCallMethod || p.Maps[pc].Depth < 0 {
		return nil
	}
	slot := p.Maps[pc].Depth - int(fn.Code[pc].A) - 1
	q := pc - 1
	for ; q >= 0 && q >= pc-64; q-- {
		if d := p.Maps[q].Depth; d < slot {
			return nil
		} else if d == slot {
			break
		}
	}
	if q < 0 || q < pc-64 || fn.Code[q].Op != bytecode.OpGetPropThis || int(fn.Code[q].A) >= len(cl.names) {
		return nil
	}
	if cl.names[fn.Code[q].A] == r.atoms.intern("replace") {
		// A string's replace (zzjit_regexpreplaceleaf.go).
		return jitReplaceLeafGuess[struct{}](r, cl, p, pc, q)
	}
	recv, ok := r.jitChainReceiver(cl, p, q, jitChainReads)
	if !ok || recv.class != ClassRegExp || recv.proto != r.proto.regexp || len(recv.vals) != 1 {
		return nil
	}
	var f *Object
	switch cl.names[fn.Code[q].A] {
	case r.atoms.intern("exec"):
		f = r.regexpExecFn
	case r.atoms.intern("test"):
		f = r.regexpTestFn
	default:
		return nil
	}
	fd := f.fn()
	if fd == nil || fd.elemOp != elemRegExpExec && fd.elemOp != elemRegExpTest || !propsIntact(recv.proto, r.regexpExecProps) ||
		f == r.regexpTestFn && !propsIntact(recv.proto, r.regexpTestProps) ||
		!jitRegExpNativeCheck[struct{}](fd.elemOp)(r, Obj(recv), []Value{Str(emptyString)}) {
		return nil
	}
	return f
}

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
