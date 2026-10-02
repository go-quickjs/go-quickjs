package vm

import (
	"runtime"
	"sync/atomic"
)

// Stale stack slots and the collector.
//
// A value popped off the operand stack, or left in the window of a frame that
// has returned, stays in its slot until something writes over it: clearing
// every slot as it is vacated costs a write barrier each, which was the
// interpreter's largest cost. endTurn clears what is left once a turn is
// over. But a turn can last as long as the program -- a benchmark suite, a
// server's loop -- and a stale slot keeps what it holds reachable, and what
// that reaches: one node of a linked structure that was removed from it can
// keep every node removed after it, a collection's worth at a time.
//
// A frame that has returned is left as it was too, for the next call at its
// depth to reuse, and holds its call's receiver and closure the same way.
//
// So the interpreter clears the stale slots it can see once each time the
// collector has run: the slots above the running frame's operands, the
// windows of the frames that have returned, and what those frames hold. What a stale slot keeps alive is
// then collectable one cycle later, rather than at the end of the turn, and
// the clearing costs once a cycle rather than once a pop.

// gcEpoch counts the collector's cycles, as far as a finalizer can see them.
var gcEpoch atomic.Uint32

// gcSentinel is an object that is garbage as soon as it is made: its
// finalizer runs after the next collection, counts it, and makes another.
type gcSentinel struct{ _ *int }

func init() { newGCSentinel() }

func newGCSentinel() {
	runtime.SetFinalizer(&gcSentinel{}, func(*gcSentinel) {
		gcEpoch.Add(1)
		newGCSentinel()
	})
}

// sweepStaleSlots clears the stack's stale slots, when the collector has run
// since it last did: those above sp, the running frame's operand stack
// pointer, whose window ends at stackTop, and the windows of the frames that
// have returned, up to the highest the stack has reached.
func (r *Runtime) sweepStaleSlots(sp int) {
	e := gcEpoch.Load()
	if e == r.sweptEpoch {
		return
	}
	r.sweptEpoch = e
	if sp < r.stackTop {
		clear(r.stack[sp:r.stackTop])
	}
	if r.stackHigh > r.stackTop {
		clear(r.stack[r.stackTop:r.stackHigh])
		r.stackHigh = r.stackTop
	}
	r.clearReturnedFrames()
}

// clearReturnedFrames lets go of what the frames of the calls that have
// returned still hold. A frame is left as it was when its call returns, so
// that the next call at its depth reuses its slices, and so it still holds
// the call's receiver, callee, closure and captured bindings -- a method's
// receiver, a whole data structure, for as long as nothing calls that deep
// again. The slices keep their room; only what they point to goes.
func (r *Runtime) clearReturnedFrames() {
	for i := r.frameDepth; i < r.frameHigh; i++ {
		f := r.frameAt(i)
		f.cl, f.locals, f.args, f.callee, f.thisRef = nil, nil, nil, nil, nil
		f.this, f.newTarget = Value{}, Value{}
		f.evalVars, f.withScopes = nil, nil
		// A tree's context is left set when it returns, for the next call at
		// this depth, and holds the closure and the value it returned.
		f.tc = tctx{}
		clear(f.openUpvalues[:cap(f.openUpvalues)])
		f.openUpvalues = f.openUpvalues[:0]
	}
	r.frameHigh = r.frameDepth
}
