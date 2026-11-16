package vm

import (
	"errors"
)

// runFD and the frames' helpers after it, apart from vm_call.go so that the
// layout lock has a pad between callDirect and runFD; see
// internal/cmd/layoutpad. Kept with the rest of the hot path.

// runFD runs a call of cl -- the callee's function data fd, or nil with no
// callee -- as register code entered as its own (regRunFD), which makes the
// chain of tail calls the frame ends in.
func (r *Runtime) runFD(cl *closure, this Value, args []Value, newTarget Value, callee *Object, fd *funcData) (Value, error) {
	v, err, ok := r.regRunFD(cl, this, args, newTarget, callee, fd)
	if !ok {
		return Undefined, errNoRegCode(cl.fn)
	}
	return v, err
}

// tailCall is a call a frame ended in, waiting for run to make it.
type tailCall struct {
	callee Value
	this   Value
	args   []Value
}

// tailCallDepth is how deep the call stack is before a tail call gives up its
// frame. Giving one up is observable only in the space it saves, so a shallow
// stack keeps its frames and makes the call the ordinary way, which is
// cheaper: a chain of tail calls still runs in constant space, just not in
// the least there is.
const tailCallDepth = 32

// errTailCall is how a frame tells run that it ended in a tail call, which
// pendingTail holds.
var errTailCall = errors.New("tail call")

// tailTarget resolves the callee of a tail call to a compiled function run can
// make a frame for, looking through bound functions. It reports false for one
// it cannot -- which is then called the ordinary way.
func (r *Runtime) tailTarget(tc tailCall) (*closure, Value, []Value, *Object, bool, error) {
	if !tc.callee.IsObject() {
		return nil, Undefined, nil, nil, false, r.throwTypeError("%s is not a function", r.describe(tc.callee))
	}
	o, this, args := tc.callee.Object(), tc.this, tc.args
	for {
		if err := r.tick(); err != nil {
			return nil, Undefined, nil, nil, false, err
		}
		if proxyOf(o) != nil {
			return nil, Undefined, nil, nil, false, nil
		}
		fd := o.fn()
		if fd == nil || fd.native != nil {
			return nil, Undefined, nil, nil, false, nil
		}
		if fd.bound {
			if len(fd.extra.rare.boundArgs) > 0 {
				merged := make([]Value, 0, len(fd.extra.rare.boundArgs)+len(args))
				merged = append(merged, fd.extra.rare.boundArgs...)
				args = append(merged, args...)
			}
			o, this = fd.extra.rare.boundTarget, fd.extra.rare.boundThis
			continue
		}
		// A function of another realm is called the ordinary way, which is
		// what switches to its realm.
		if fd.closure == nil || isGeneratorTemplate(fd.closure.fn) ||
			isClassConstructorKind(fd.closure.fn.Kind) || fd.closure.realm != r.Realm {
			return nil, Undefined, nil, nil, false, nil
		}
		if fd.arrow {
			this = fd.extra.lexThis
			if !fd.extra.newTarget().IsUndefined() {
				// An arrow inside a constructor sees its new.target, which run
				// would have to be given: the ordinary call path does that.
				return nil, Undefined, nil, nil, false, nil
			}
		}
		return fd.closure, this, args, o, true, nil
	}
}

// pushFrame extends the call stack by one and returns the new frame.
//
// The slice is resliced rather than appended to. Its capacity is fixed at
// construction, so this can never reallocate -- which matters because the
// interpreter holds a *frame across nested calls, and a reallocation would
// silently leave those pointers aimed at the abandoned array.
//
// Reslicing also means the frame left behind at this depth keeps its handler
// and upvalue slices, whose backing arrays the next call reuses.
// detachSuspended lets go of the handler and upvalue slices a generator's
// frame ran with, which the generator keeps for its next resumption. The
// frame goes back to the pool, and the next call made at its depth reuses
// the slices it holds, truncating them and appending: were they still the
// generator's, that call would write over the catch handlers and captured
// variables of the generator it was suspended with.
func (f *frame) detachSuspended() {
	f.handlers = nil
	f.openUpvalues = nil
}

func (r *Runtime) pushFrame() *frame {
	// The block the next frame goes in is kept to hand, so an ordinary push is
	// a subtraction and an index. The unsigned comparison covers both ways the
	// block can be the wrong one: the depth has grown past its end, or it has
	// fallen below its start since the block was chosen.
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	return &r.cur[off]
}

// seekFrameBlock picks the block the current depth falls in, allocating it if
// the call stack has never been this deep.
func (r *Runtime) seekFrameBlock() {
	b := uint(r.frameDepth) / frameBlockSize
	if b == uint(len(r.frames)) {
		r.frames = append(r.frames, make([]frame, frameBlockSize))
	}
	r.cur = r.frames[b]
	r.frameBase = int(b) * frameBlockSize
}

// frameAt is the frame at a depth, counting from the bottom.
func (r *Runtime) frameAt(i int) *frame {
	if jitBuilt {
		// One of the native levels whose frames a call of Go put off, made
		// first (jitLazyLevels).
		r.jitFramesDue(i)
	}
	u := uint(i)
	return &r.frames[u/frameBlockSize][u%frameBlockSize]
}

// topFrame is the frame being executed, or nil when nothing is.
func (r *Runtime) topFrame() *frame {
	if r.frameDepth == 0 {
		return nil
	}
	return r.frameAt(r.frameDepth - 1)
}

// popFrame releases a frame's stack window, closing any upvalues that pointed
// into its locals so that closures created inside it keep working.
//
// The window is deliberately left dirty. Clearing it on the way out was the
// single largest cost in the interpreter -- it doubled the zeroing work per
// call, since the next frame to use the region clears its locals on entry
// anyway. Nothing can read the stale values: locals are cleared before use, and
// the compiler guarantees every operand slot is written before it is read.
//
// The cost is that values stay reachable from the stack until their slots are
// reused. That delays collection, so the region above the live frames is
// cleared once per turn instead -- see endTurn, which is where a program could
// next observe the difference.
func (r *Runtime) popFrame(base int) {
	r.popFrameOf(r.frameAt(r.frameDepth-1), base)
}

// popFrameOf is popFrame for a caller that has the frame, which is the top
// one: finding it again by its depth is a cost every call would pay.
func (r *Runtime) popFrameOf(f *frame, base int) {
	for _, u := range f.openUpvalues {
		u.close()
	}
	r.stackTop = base
	r.frameDepth--
}
