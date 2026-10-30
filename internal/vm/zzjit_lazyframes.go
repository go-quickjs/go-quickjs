//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitLazyLevels is a call of Go from a native callee whose levels below
// the calling one have no frames yet (jitGoCallNested): those of contexts
// root+1 to idx-1, at depth on, the calling level's frame past them. They
// are made when a frame among them is read -- frameAt, which a stack trace,
// a function's caller, a generator's search for its own run read through
// -- from the contexts, which hold where each level is while the call
// runs: what it runs natively runs in contexts past idx. made says they
// have been.
//
// The levels' code is pinned by none of it (the calling level's is): while
// any call has put frames off, no code is closed (closeSSA,
// jitCloseRetired), so that what the levels return to stays.
type jitLazyLevels struct {
	root, idx, depth int
	runF             *frame
	made             bool
}

// jitFramesDue makes the frames calls of Go put off (jitLazyLevels), if
// frame i is one of them.
func (r *Runtime) jitFramesDue(i int) {
	if s := r.jit; s != nil && i < s.lazyTop {
		r.jitMakeLazyFrames()
	}
}

// jitMakeLazyFrames makes the frames every call of Go under way put off,
// as jitGoCallNested would have made them, where it left room for them.
func (r *Runtime) jitMakeLazyFrames() {
	s := r.jit
	// Cleared, and each set marked made, before any frame is filled: a
	// frame read while they are being filled would be handed back unfilled.
	// None is: the filling pushes frames, which reads none, and reads the
	// contexts, the stack and an accessor's property (jitLevelCallee,
	// jitDerivedFrame), none of which reads a frame or meters memory. A
	// change that has it read one -- a stack trace, the memory meter's walk
	// -- must fill them first.
	s.lazyTop = 0
	s.lazyMade++
	saved := r.frameDepth
	ctxs := s.ssaCtxs
	for j := range s.lazy {
		lz := &s.lazy[j]
		if lz.made {
			continue
		}
		lz.made = true
		r.frameDepth = lz.depth
		callerFn := lz.runF.cl.fn
		prevPC, _ := jit.CallPC(ctxs[lz.root])
		for i := lz.root + 1; i < lz.idx; i++ {
			c := ctxs[i]
			lcl := (*closure)(c.Closure)
			callPC, _ := jit.CallPC(c)
			f := r.pushFrame()
			if lcl == nil {
				// None to make: the room a frame of no function's, which a
				// stack trace passes over.
				*f = frame{openUpvalues: f.openUpvalues[:0], handlers: f.handlers[:0]}
				continue
			}
			r.jitLevelFrameFill(f, c, lcl, r.jitEntryOf(lcl), nil, callPC, prevPC, callerFn)
			prevPC, callerFn = callPC, lcl.fn
		}
	}
	r.frameDepth = saved
}

// jitLazyDone ends the innermost call of Go's put-off frames, as the call
// returns.
func (r *Runtime) jitLazyDone() {
	s := r.jit
	s.lazy = s.lazy[:len(s.lazy)-1]
	s.lazyTop = 0
	if n := len(s.lazy); n != 0 && !s.lazy[n-1].made {
		lz := s.lazy[n-1]
		s.lazyTop = lz.depth + lz.idx - lz.root - 1
	}
}

// jitFrameBlocks has the blocks of frames up to the one depth is in, as
// pushes up to it would have made them: a call of Go leaves room for the
// frames it puts off, past which its calling level's frame is pushed.
func (r *Runtime) jitFrameBlocks(depth int) {
	for uint(depth)/frameBlockSize >= uint(len(r.frames)) {
		r.frames = append(r.frames, make([]frame, frameBlockSize))
	}
}

// jitLevelFrameFill fills f, the frame of the native level of context c,
// running cl's code, of entry e, at callPC, which its caller, of callerFn,
// called at prevPC; or a generator's, g, which a step resumed natively
// (jitGenFrame).
func (r *Runtime) jitLevelFrameFill(f *frame, c *abi.Context, lcl *closure, e *jitEntry, g *generator, callPC, prevPC uint64, callerFn *bytecode.Function) {
	base := int(c.Base)
	f.cl = lcl
	f.pc = uint32(callPC) + 1
	if g != nil {
		// A generator's, its locals its own, its operands at the
		// level's base (jitGenFrame).
		f.locals, f.base = g.vals[g.nargs:], base
		f.this, f.newTarget, f.callee, f.args = g.this, Undefined, g.callee, g.vals[:g.nargs:g.nargs]
	} else {
		f.locals = r.stack[base : base+lcl.fn.LocalCount : base+lcl.fn.LocalCount]
		f.base = base + lcl.fn.LocalCount
		f.this, f.newTarget, f.callee, f.args = *(*Value)(unsafe.Pointer(&c.This)), Undefined, nil, nil
	}
	// Its function object, where the call it runs for says (an accessor's):
	// the level's caller's, at its call. Asked only of a property's read or
	// write: this runs for every call of Go a native callee makes, and each
	// level's lookup had cost AWFY's Json 2%.
	if pc := int(prevPC); pc < len(callerFn.Code) {
		if op := callerFn.Code[pc].Op; op == bytecode.OpGetProp || op == bytecode.OpSetProp {
			f.callee = r.jitLevelCallee(callerFn, pc, f.this)
		}
	}
	if c.Function != 0 && lcl.fn.Kind != bytecode.KindDerivedConstructor && e != nil && e.ssaProg != nil && e.ssaProg.FrameCallee {
		// The function object its code reads, which the call wrote
		// (jitFrameCallee); a derived construction's is jitDerivedFrame's.
		f.callee = *(**Object)(unsafe.Pointer(&c.Function))
	}
	f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
	f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
	if lcl.fn.Kind == bytecode.KindDerivedConstructor {
		jitDerivedFrame(f, c)
	}
}
