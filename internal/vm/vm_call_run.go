package vm

import (
	"errors"
	"sync/atomic"
)

// runFD and the frames' helpers after it, apart from vm_call.go so that the
// layout lock has a pad between callDirect and runFD; see
// internal/cmd/layoutpad. Kept with the rest of the hot path.

// runFD is run for a caller that has the callee's function data already,
// which run would otherwise look up again: fd is callee's, or nil with no
// callee.
func (r *Runtime) runFD(cl *closure, this Value, args []Value, newTarget Value, callee *Object, fd *funcData) (Value, error) {
start:
	fn := cl.fn

	// A sloppy-mode function's `this` is coerced: undefined and null become the
	// global object, and a primitive becomes its wrapper. Strict mode leaves it
	// exactly as passed, which is the difference that makes strict mode able to
	// detect a missing receiver at all.
	// The object case is tested first because it is the common one and costs a
	// single mask: a method call already has an object receiver and needs no
	// coercion at all.
	if fn.CoerceThis && !this.IsObject() && newTarget.IsUndefined() {
		if this.IsNullish() {
			this = r.globalThis
		} else {
			o, err := r.toObject(this)
			if err != nil {
				return Undefined, err
			}
			this = Obj(o)
		}
	}

	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}

	// Carve a window out of the shared stack: locals first, then operands.
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	if base+need > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + need

	locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	// The arguments go to the parameters' slots, positionally; anything
	// beyond the simple positional case -- defaults, destructuring, a rest
	// parameter -- is compiled into the function's prologue. Every other
	// local starts as undefined, since the window was last used by an
	// unrelated frame and a stale value could be read by a binding whose
	// declaration was never reached: a parameter no argument was given for,
	// and the slots past the parameters, in one loop. They are stored one at
	// a time: clear would call memclr, with a bulk write barrier, which for a
	// frame's few locals cost more than the call.
	m := min(fn.ParamCount, len(locals), len(args))
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
	}

	// The high-water mark is what endTurn clears back to, so that a value left
	// behind by a returning frame does not stay reachable until its slot is
	// reused.
	if r.stackTop > r.stackHigh {
		r.stackHigh = r.stackTop
	}

	f := r.pushFrame()
	// The fields are assigned rather than the struct replaced, so that the
	// handler and upvalue slices keep their backing arrays across calls. A
	// frame is large enough that copying a fresh one, and reallocating those
	// slices, showed up in the profile.
	f.cl = cl
	f.locals = locals
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this = this
	// A derived constructor does not receive `this`; super() binds it. Until
	// then the object the caller made is held but unreachable, so that a
	// subclass cannot touch what the base class has not finished building.
	// The kind alone does not say which constructors are derived: a derived
	// class with no explicit constructor gets a synthesized one, and what makes
	// it derived is the heritage clause the class object records.
	f.newTarget = newTarget
	f.callee = callee
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	// What almost no function has is reset only where the frame's last call
	// left it set: reading a field is cheaper than writing a pointer to it.
	if f.thisRef != nil {
		f.thisRef = nil
	}
	// The chain is inherited whole, capped so that a push inside this call
	// copies rather than writing into the creating frame's array.
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
	}
	// What follows is for a derived constructor, an arrow written inside
	// one, and a function inside a with or after an eval, all of which have
	// an extra or are derived; most functions are neither.
	if fd != nil && (fd.extra != nil || fd.ctorKind == ctorDerived) {
		switch {
		case !newTarget.IsUndefined() && fd.ctorKind == ctorDerived:
			f.thisRef = &thisBinding{value: this}
		case fd.arrow && fd.extra.lexThisRef != nil:
			// An arrow written inside a derived constructor shares its
			// binding, so calling one before super() is the same error.
			f.thisRef = fd.extra.lexThisRef
		}
		if w := fd.extra.with(); len(w) > 0 {
			f.withScopes = w[:len(w):len(w)]
		}
		// What an eval declared in an enclosing function is still in scope
		// here, whether or not this one has anything of its own.
		if ev := fd.extra.evalVars(); ev != nil {
			f.evalVars = ev
		}
	}
	if fn.HasDirectEval {
		// The body contains a direct eval, so it needs somewhere for the vars
		// that eval may declare. It goes on the scope chain, inside whatever
		// the enclosing functions put there: a name the evaluated code
		// declares is found the way a `with` object's properties are.
		// An ordinary object of no prototype, newObject's, made here: a call
		// of newObject, which no longer inlines, had grown runFD's code
		// and moved what follows it.
		f.evalVars = &Object{class: ClassObject, flags: objExtensible | objEvalVars}
		f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], f.evalVars)
	}
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if f.native != "" {
		f.native = ""
	}
	if f.savedSP != 0 {
		f.savedSP = 0
	}

	// The tree the function runs as, if the tier built it one. Every call
	// asks, so once the answer is known it is a load and two comparisons
	// here; firstTree settles it the first time.
	var v Value
	var err error
	var native bool
	if r.jitOn() {
		v, err, native = r.tryJITFrame(f)
	}
	if !native {
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil {
			t = firstTree(fn)
		}
		if t != noTree {
			v, err = r.runTree(f, t)
		} else {
			v, err = r.execute(f)
		}
	}
	r.popFrameOf(f, base)
	if err != errTailCall {
		return v, err
	}
	// The frame ended in a tail call, and is gone: the callee's is made in
	// its place, in this same call of run.
	var ok bool
	if cl, this, args, callee, ok, err = r.takeTailCall(); !ok {
		return this, err
	}
	newTarget = Undefined
	fd = nil
	if callee != nil {
		fd = callee.fn()
	}
	goto start
}

// takeTailCall resolves the call a frame ended in. It reports true with the
// frame to make in its place, or false with the call's result, having made it
// the ordinary way -- which a callee that is not a compiled function needs.
func (r *Runtime) takeTailCall() (*closure, Value, []Value, *Object, bool, error) {
	tc := r.pendingTail
	r.pendingTail = tailCall{}
	cl, this, args, callee, ok, err := r.tailTarget(tc)
	if err != nil {
		return nil, Undefined, nil, nil, false, err
	}
	if !ok {
		v, err := r.callObject(tc.callee.Object(), tc.this, tc.args, Undefined)
		return nil, v, nil, nil, false, err
	}
	return cl, this, args, callee, true, nil
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

// pushAt stores v at sp and returns the next free slot. It takes and returns
// sp rather than holding it, so that the interpreter's sp stays a variable the
// compiler can keep in a register.
func pushAt(stack []Value, sp int, v Value) int {
	stack[sp] = v
	return sp + 1
}
