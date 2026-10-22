//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

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
// its frame anyway, and a call whose result it throws, unless fn's own
// code has been seen to make it; or a call fn's own code has never been
// seen to make:
// code compiled to leave it to Go (ssa.Func's HostsLeft), which counts
// each call it leaves at (jitCallSeen), and has never left there in runs
// enough to tell (jitCodeRan) -- as V8's code deoptimizes at a call that
// has no feedback. One taken after all counts toward the call's
// inlining's exits (jitInlineLeft), which stop it, the callee then called
// natively.
func (r *Runtime) jitInlineExit(fn *bytecode.Function, pc int) bool {
	if pc < 0 || pc >= len(fn.Code) {
		return false
	}
	switch in := jitCallInstr(fn.Code[pc]); in.Op {
	case bytecode.OpThrow, bytecode.OpThrowTypeError, bytecode.OpThrowReferenceError:
		return true
	case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		e := r.jit.entryFor(fn)
		if pc+1 < len(fn.Code) && fn.Code[pc+1].Op == bytecode.OpThrow {
			// What it makes is thrown: as seldom as the throw, an error's
			// construction, throw new Error(...) -- unless fn's own code
			// has been seen to make it (jitCallSeen), a throw taken often.
			return e == nil || e.callSites == nil || e.callSites[pc] == 0
		}
		// Its own code leaves it to Go, and has never left there: one its
		// code makes natively leaves no count -- a built-in's its code
		// makes itself (a substring, a push), one it was compiled to call.
		return e != nil && e.ssa != nil && slices.Contains(e.ssaHostsLeft, int32(pc)) && (e.callSites == nil || e.callSites[pc] == 0) &&
			jitCodeRan(e)
	}
	return false
}

// jitRunsSeen is how many runs of its code a function has made, at least,
// for a call none of them made to be one seldom made.
const jitRunsSeen = 16

// jitCodeRan reports whether e's code has run often enough -- entered by
// Go or called natively, jitRunsSeen times -- for a call it has not made
// to be one seldom made. Code that has hardly run, or never -- compiled
// for native callers that inline the function wherever they call it, as
// AWFY DeltaBlue's Variable.value is -- has seen none of its calls: its
// callers inlining it with an exit at each took the exit at every call,
// one a loop's turn.
func jitCodeRan(e *jitEntry) bool {
	return e.link != nil && e.ssaStats.entries+e.link.in >= jitRunsSeen
}
