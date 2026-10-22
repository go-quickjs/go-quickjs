//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// InlineExit is ssa.InlineExitFeedback's: of an inlined callee's
// feedback, whether native code may leave the inlined frame at its
// operation at pc (jitInlineExit).
func (fb *jitFeedback) InlineExit(pc int) bool {
	return fb.root != nil && fb.r.jitInlineExit(fb.fn, pc)
}

// jitInlineExit reports whether the operation at pc of fn, which native
// code leaves to Go, may be left so inside a callee inlined with its
// caller, rather than keep fn from being inlined: one seldom run, at which
// native code leaves the inlined frame, Go making its frame and the
// interpreter going on in it, as V8's code inlines a function whose cold
// paths deoptimize, or call the runtime to throw. A throw, which leaves
// its frame anyway; or a call fn's own code has never been seen to make --
// code that counts each call it leaves to Go (jitCallSeen), and finds the
// targets of those its callers' code had seen (jitSeedCalls) -- as V8's
// code deoptimizes at a call that has no feedback. One taken after all
// counts toward the call's inlining's exits (jitInlineLeft), which stop
// it, the callee then called natively.
func (r *Runtime) jitInlineExit(fn *bytecode.Function, pc int) bool {
	if pc < 0 || pc >= len(fn.Code) {
		return false
	}
	switch in := jitCallInstr(fn.Code[pc]); in.Op {
	case bytecode.OpThrow, bytecode.OpThrowTypeError, bytecode.OpThrowReferenceError:
		return true
	case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		e := r.jit.entryFor(fn)
		return e != nil && e.ssa != nil && (e.callSites == nil || e.callSites[pc] == 0)
	}
	return false
}
