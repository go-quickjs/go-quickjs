package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A call of a compiled function asks the same questions every time --
// whether it is a closure of this realm, bound, an arrow, a derived
// constructor, made inside a with or beside an eval, which tier runs it --
// and for a given function the answers never change. planTreeCall asks
// them once, after a call, and keeps the tree in funcData.treeCall when
// they all say the function is called as runFD would call it with nothing
// to establish but the frame; callTree then makes the call on that. It is
// in the package's last files with the other calls kept out of the hot
// files' way: see zcall_tail.go.

// planTreeCall decides whether calls of fd's function may go to callTree,
// after a call that ran it, which has built its tree if the tier builds
// one. A pure body is left undecided while pureCall may still answer its
// calls, which it tries first -- unless pureByTree says they go to its
// tree.
//
// One that only ends in a tail call, whose frame callTree's caller could
// not then give up, or is an arrow, whose this is its own, is run on its
// tree by callRest (treeRest), which makes the tail call as runFD's loop
// does and gives the arrow what it captured. Every function a strict
// body's return f() is in -- a class's method, as a rule -- had been run
// through runFD and runTree's recover at every call: 11 million of Are We
// Fast Yet's calls, and 2 million an arrow's, since calls were first made
// on trees.
func planTreeCall(fd *funcData) {
	cl := fd.closure
	fn := cl.fn
	t := (*tree)(atomic.LoadPointer(&fn.VMCode))
	switch {
	case t == nil:
		return
	case fn.Leaf == bytecode.LeafPure && cl.pureMiss < pureMissLimit && !pureByTree(fn):
		return
	case t == noTree || fd.native != nil || fd.bound || fd.ctorKind == ctorDerived || !fn.DirectCall ||
		fn.HasDirectEval || fn.Leaf != bytecode.LeafNone && fn.Leaf != bytecode.LeafPure:
		// A leaf's other kinds are answered by leafCall.
	case fd.arrow:
		// An arrow with nothing of its surroundings but its this and its
		// new.target: not one inside a with, beside an eval, or sharing a
		// derived constructor's this.
		if x := fd.extra; x != nil && len(x.with()) == 0 && x.evalVars() == nil && x.lexThisRef == nil {
			fd.treeRest = true
		}
	case fd.extra != nil:
	case fn.HasTailCall:
		fd.treeRest = true
	default:
		fd.treeCall = t
	}
	fd.treePlanned = true
}

// makeClosure builds a closure from a nested function template
// (newClosure), and plans its calls (planTreeCall) where its function's
// tree is built: what planTreeCall asks of it is settled when it is made.
// A closure had been planned after its first call, which runFD made: a
// function made anew at every call of its maker -- a callback, an arrow
// for a forEach, as Havlak's loops make them -- had every first call made
// so, half a million of Are We Fast Yet's.
func (r *Runtime) makeClosure(f *frame, c Value) *Object {
	o := r.newClosure(f, c)
	if fd := o.fn(); fd != nil && atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
		planTreeCall(fd)
	}
	return o
}

// callTree calls o, whose funcData planTreeCall has given a tree, with this
// and args, once callDirect has found it a compiled function of this
// realm and made its interrupt check: runFD's call for a function with no
// extra, run as its tree. A sloppy body's this that is a primitive is left
// to runFD, which wraps it.
//
// nested says the caller is a tree's call node, which throws whatever error
// the call returns: the tree is then run by runTreeNested, without a recover
// of its own. An unhandled exception goes past this call, frame and all, to
// the nearest handler wrapper or runTree that catches it.
func (r *Runtime) callTree(o *Object, fd *funcData, this Value, args []Value, nested bool) (Value, error) {
	cl := fd.closure
	fn := cl.fn
	if fn.CoerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.runFD(cl, this, args, Undefined, o, fd)
		}
		this = r.globalThis
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	if base+need > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + need
	locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	m := min(fn.ParamCount, len(locals), len(args))
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
	}
	if r.stackTop > r.stackHigh {
		r.stackHigh = r.stackTop
	}
	// pushFrame, written out: called, it costs the call and the registers
	// saved around it, which are most of what it does.
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = cl
	f.locals = locals
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this = this
	f.newTarget = Undefined
	f.callee = o
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil {
		f.thisRef = nil
	}
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
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
	// The JIT, when it runs the frame, comes first. A nested tree whose loop
	// may promote keeps runTree's recover (see jitTreeRecovery).
	var v Value
	var err error
	var native bool
	if r.jitOn() {
		v, err, native = r.tryJITFrame(f)
	}
	if native {
	} else if nested && !(r.jitOn() && r.jitTreeRecovery(f)) {
		v, err = r.runTreeNested(f, fd.treeCall)
	} else {
		v, err = r.runTree(f, fd.treeCall)
	}
	r.popFrameOf(f, base)
	return v, err
}

// runTreeNested is runTree for a tree a tree's call node calls, through
// callTree: the same, but an unhandled exception is not caught here. It goes
// through the calling node to the nearest handler wrapper or runTree, whose
// recover pops the frames it passed, as unwindTreeFrames does. Not
// setting up a recover is most of what entering a tree costs beyond its
// frame.
func (r *Runtime) runTreeNested(f *frame, t *tree) (Value, error) {
	c := &f.tc
	if r.stopped != nil {
		c.throw(r.stopped)
	}
	c.r, c.f, c.cl, c.locals = r, f, f.cl, f.locals
	c.closureCode = f.cl.closureCode
	c.stack = r.stack[f.base : f.base+f.cl.fn.MaxStack]
	blocks := t.blocks
	b := 0
	for {
		blk := &blocks[b]
		for _, s := range blk.body {
			s(c)
		}
		if b = blk.next(c); b < 0 {
			if r.treeTail {
				r.treeTail = false
				return Undefined, errTailCall
			}
			return c.ret, nil
		}
	}
}

// unwindTreeFrames pops the frames above depth that an exception left,
// trees runTreeNested ran, as callTree would have popped them on return:
// their open upvalues are closed. depth is frameDepth with runTree's own
// frame on the stack, so that frame, the depth-th, stays: it is its caller's
// to pop, which also resets the stack's top.
func (r *Runtime) unwindTreeFrames(depth int) {
	for r.frameDepth > depth {
		f := r.frameAt(r.frameDepth - 1)
		for _, u := range f.openUpvalues {
			u.close()
		}
		r.frameDepth--
	}
}

// callRest is callDirect for a callee that is the object o, whose funcData
// is fd or nil, once a tree's call node has found it is not one
// planTreeCall has given a tree: callDirect's other cases. A compiled
// function's comes first, which is callDirect's third: the two before it,
// a Math function and Function.prototype.call, are built-ins, so no callee
// is of both, and a pure body's call, the commonest here, asks nothing
// first that cannot be its answer.
func (r *Runtime) callRest(o *Object, fd *funcData, this Value, args []Value) (Value, error) {
	if fd != nil && fd.native == nil && !fd.bound &&
		fd.closure != nil && fd.closure.realm == r.Realm {
		if fn := fd.closure.fn; fn.DirectCall {
			if err := r.tick(); err != nil {
				return Undefined, err
			}
			if leaf := fn.Leaf; leaf == bytecode.LeafPure {
				if pureFirst(fd, fn) {
					if v, ok := r.pureCall(fd, this, args); ok {
						return v, nil
					}
				}
			} else if leaf != bytecode.LeafNone && this.IsObject() {
				if v, ok := r.leafCall(fd.closure, this.Object(), args); ok {
					return v, nil
				}
			}
			newTarget := Undefined
			if fd.arrow {
				this, newTarget = fd.extra.lexThis, fd.extra.lexNewTarget
			}
			if fd.treeRest && !(fn.CoerceThis && !this.IsObject()) {
				// The tree planTreeCall has found it may be run on, as a
				// nested call's, the call its frame ended in made after it.
				v, err := r.callTreeWith(o, fd, (*tree)(atomic.LoadPointer(&fn.VMCode)), this, args, newTarget, true)
				if err == errTailCall {
					return r.tailFromTree()
				}
				return v, err
			}
			v, err := r.runFD(fd.closure, this, args, newTarget, o, fd)
			if err == errNoSuper {
				err = r.throwError(errReference, "%s", errNoSuper.Error())
			}
			if !fd.treePlanned {
				planTreeCall(fd)
			}
			return v, err
		}
	}
	if fd != nil && fd.mathOp != 0 {
		if v, ok := mathCall(fd.mathOp, args); ok {
			return v, nil
		}
	}
	if o == r.callFn {
		return r.callThrough(o, this, args)
	}
	if fd != nil && fd.native != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) {
		if fd.elemOp != 0 {
			if v, ok := r.pureElemOp(fd.elemOp, this, args, true); ok {
				return v, nil
			}
		}
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
			return Undefined, err
		}
		v, err := fd.native(r, this, args)
		r.frameDepth--
		if r.meter != nil && err == nil {
			err = r.chargeResult(v)
		}
		return v, err
	}
	return r.call(Obj(o), this, args)
}

// tailFromTree makes the call a frame callTreeWith ran ended in
// (errTailCall), as runFD's loop does: its callee's frame in the one it
// gave up. A callee planTreeCall has given a tree is run on it, here, and
// a call its frame ends in made by the next turn of the loop: a recursion
// through tail calls, past the depth one gives its frame up at
// (tailCallDepth), stays in the tree tier, in as many frames. Any other is
// runFD's, whose loop makes the rest.
func (r *Runtime) tailFromTree() (Value, error) {
	for {
		cl, this, args, callee, ok, err := r.takeTailCall()
		if !ok {
			// Made the ordinary way: this is its result.
			return this, err
		}
		var fd *funcData
		if callee != nil {
			fd = callee.fn()
		}
		var t *tree
		switch {
		case fd == nil || cl.fn.CoerceThis && !this.IsObject():
		case fd.treeCall != nil:
			t = fd.treeCall
		case fd.treeRest:
			t = (*tree)(atomic.LoadPointer(&cl.fn.VMCode))
		}
		if t == nil {
			return r.runFD(cl, this, args, Undefined, callee, fd)
		}
		v, err := r.callTreeWith(callee, fd, t, this, args, Undefined, true)
		if err != errTailCall {
			return v, err
		}
	}
}

// callTreeWith is callTree for the calls it does not make: a
// construction's, this the object made and newTarget the constructor's
// (constructWithTarget), and callRest's, of a function whose frame may end in a
// tail call, which comes back as errTailCall, or an arrow's, this what it
// captured (treeRest) -- t, the function's tree. It is callTree's own
// code, written again, as callTree is one of the interpreter's hot
// functions (AGENTS.md): constructors -- Are We Fast Yet's and the V8
// suite's every object -- had been run through runFD, its frame set up for
// anything, and runTree, its recover, at every new.
func (r *Runtime) callTreeWith(o *Object, fd *funcData, t *tree, this Value, args []Value, newTarget Value, nested bool) (Value, error) {
	cl := fd.closure
	fn := cl.fn
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	if base+need > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + need
	locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	m := min(fn.ParamCount, len(locals), len(args))
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
	}
	if r.stackTop > r.stackHigh {
		r.stackHigh = r.stackTop
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = cl
	f.locals = locals
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this = this
	f.newTarget = newTarget
	f.callee = o
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil {
		f.thisRef = nil
	}
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
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
	var v Value
	var err error
	var native bool
	if r.jitOn() {
		v, err, native = r.tryJITFrame(f)
	}
	if native {
	} else if nested && !(r.jitOn() && r.jitTreeRecovery(f)) {
		v, err = r.runTreeNested(f, t)
	} else {
		v, err = r.runTree(f, t)
	}
	r.popFrameOf(f, base)
	return v, err
}
