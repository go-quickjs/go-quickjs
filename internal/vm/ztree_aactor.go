package vm

import (
	"math"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Constructions on the tree tier's path. A construction ran its
// constructor on the tree planTreeCall gave it, callTreeWith's, only where
// a call could run it too, so that a class's constructor, which a call
// refuses (DirectCall), was run by runFD at every new -- its frame set up
// for anything, a with's scopes and an eval's bindings asked after -- and
// a pure body's, whose plan waits for pureCall to give up on calls that a
// construction never makes, was never planned at all. A derived class's
// was worse off: runFD gave its frame a this binding of the heap, its
// super() reached the parent through callObject and runFD again, and
// copied its arguments for it, three allocations and two frames set up for
// anything for each new of a subclass -- each of Are We Fast Yet's
// DeltaBlue constraints and Json values, and a class hierarchy's every
// object in real code.
//
// planTreeNew decides once what a construction may run on, as planTreeCall
// decides for calls, and constructTree makes the constructions it allows:
// a base class's on callTreeWith; a derived class's on a frame of its own
// making, whose this binding is the runtime's cell for the frame's depth
// (thisCells) where the body makes no closure and no direct eval, the two
// things that can keep a binding past its frame. super() calls the parent
// the same way. What each construction then does is what runFD would have
// done for it, in the same order.
//
// The code is closures assigned in this file's init, the package's first
// init with closures, which are laid out after every other closure (see
// ztree_aaeq.go): the tree tier's own closures do not move.

var (
	// planTreeNew decides fd.treeNew, the constructor's constructions made
	// by constructTree, once its function has run, and records that it has
	// (newPlanned).
	planTreeNew func(fd *funcData)

	// constructTree constructs o, whose fd.treeNew is set, with this the
	// object made for it and newTarget the construction's, once its caller
	// has made the interrupt check: what runFD would do, on the function's
	// tree.
	constructTree func(r *Runtime, o *Object, fd *funcData, this Value, args []Value, newTarget Value) (Value, error)

	// constructRun is constructWithTarget's construction of a compiled
	// constructor planTreeCall gave no tree's call: constructTree's where
	// planTreeNew allows it, runFD's otherwise, which it plans after.
	// constructWithTarget calls it where it had called runFD, and asks
	// what it asked after, so that it is as long as it was: the code of
	// the package's functions laid out after it, the hot ones among them,
	// stays where it was (AGENTS.md).
	constructRun func(r *Runtime, this Value, args []Value, newTarget Value, o *Object, fd *funcData) (Value, error)

	// superConstruct is super()'s construction of its parent in frame f,
	// with f's object as this and new.target, after the parent's instance
	// initializer: constructTree's where planTreeNew allows it,
	// callObject's otherwise, which it plans after. superCall calls it
	// where it had called callObject, for the same reason; the frame
	// rather than its this, which keeps superCall's code as long as it was.
	superConstruct func(r *Runtime, parent *Object, f *frame, args []Value, newTarget Value) (Value, error)

	// constructNativeAt is constructWithTarget's construction by a
	// built-in: a native leaf whose construction is its call
	// (elemNewFirst...) applied without a frame where new.target is the
	// built-in itself, and constructNative otherwise, where it had called
	// constructNative.
	constructNativeAt func(r *Runtime, o *Object, args []Value, newTarget Value, derived bool) (Value, error)

	// setArrayCtor records the realm's Array constructor, made by
	// initArrayBuiltins, and declares its native leaf (elemArrayNew), in
	// place of the store initArrayBuiltins had made.
	setArrayCtor func(r *Runtime, ctor *Object)
)

func init() {
	// Array(...) and new Array(...) make an array and run nothing: of its
	// arguments, or of a length, which only throws where it is not one.
	leafChecks[elemArrayNew] = func(_ *Runtime, _ Value, args []Value) bool {
		if len(args) == 1 && args[0].IsNumber() {
			n := args[0].num
			return n >= 0 && n <= math.MaxUint32 && n == math.Trunc(n)
		}
		return true
	}

	setArrayCtor = func(r *Runtime, ctor *Object) {
		r.proto.arrayCtor = ctor
		setElemOp(r, ctor, elemArrayNew)
	}

	constructNativeAt = func(r *Runtime, o *Object, args []Value, newTarget Value, derived bool) (Value, error) {
		fd := o.fn()
		if op := fd.elemOp; op >= elemNewFirst && !derived && r.meter == nil &&
			(fd.realm == nil || fd.realm == r.Realm) && leafChecks[op](r, Undefined, args) {
			// A leaf whose construction is its call, by new.target itself:
			// applied without a frame, as its call is.
			if v, err := fd.native(r, Undefined, args); err == nil {
				return v, nil
			}
		}
		return r.constructNative(o, args, newTarget, derived)
	}

	constructRun = func(r *Runtime, this Value, args []Value, newTarget Value, o *Object, fd *funcData) (Value, error) {
		if fd.treeNew {
			return constructTree(r, o, fd, this, args, newTarget)
		}
		v, err := r.runFD(fd.closure, this, args, newTarget, o, fd)
		if !fd.newPlanned {
			planTreeNew(fd)
		}
		return v, err
	}

	superConstruct = func(r *Runtime, parent *Object, f *frame, args []Value, newTarget Value) (Value, error) {
		pfd, this := parent.fn(), f.this
		if pfd != nil && pfd.treeNew && pfd.closure.realm == r.Realm {
			// A compiled constructor of this realm is constructed on its
			// tree, as a new of it is: nothing else callObject asks applies.
			if err := r.tick(); err != nil {
				return Undefined, err
			}
			return constructTree(r, parent, pfd, this, args, newTarget)
		}
		v, err := r.callObject(parent, this, args, newTarget)
		if pfd != nil && !pfd.newPlanned && err == nil {
			planTreeNew(pfd)
		}
		return v, err
	}

	planTreeNew = func(fd *funcData) {
		cl := fd.closure
		if cl == nil || fd.native != nil || fd.bound {
			fd.newPlanned = true
			return
		}
		fn := cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil {
			// Not run yet; the construction that runs it decides.
			return
		}
		fd.newPlanned = true
		// A function made inside a with body or beside a direct eval has
		// scopes its frame is given (fd.extra), as one with an eval of its
		// own makes some; runFD sets both up.
		if t == noTree || fd.ctorKind == ctorNone || fd.extra != nil || fn.HasDirectEval {
			return
		}
		if fd.ctorKind == ctorDerived {
			// Its this binding is a cell of the runtime's, reused by the
			// next frame at its depth: a closure made in the body (an
			// arrow's lexThisRef) or a direct eval's code would keep it past
			// the frame.
			for _, in := range fn.Code {
				switch in.Op {
				case bytecode.OpClosure, bytecode.OpDirectEval, bytecode.OpTailDirectEval:
					return
				}
			}
		}
		fd.treeNew = true
	}

	constructTree = func(r *Runtime, o *Object, fd *funcData, this Value, args []Value, newTarget Value) (Value, error) {
		cl := fd.closure
		fn := cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if fd.ctorKind != ctorDerived {
			return r.callTreeWith(o, fd, t, this, args, newTarget, false)
		}
		// callTreeWith, but that the frame's this is a binding, unbound
		// until super() binds it, as runFD gives a derived constructor's.
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
		// The cell for this depth. A frame below that holds one of an
		// array this replaces keeps it, as nothing reads a cell but through
		// its frame's thisRef.
		d := r.frameDepth
		if d >= len(r.thisCells) {
			r.thisCells = make([]thisBinding, d+d/2+16)
		}
		cell := &r.thisCells[d]
		*cell = thisBinding{value: this}
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
		f.thisRef = cell
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
		if !native {
			v, err = r.runTree(f, t)
		}
		r.popFrameOf(f, base)
		// The object is the construction's now, not the cell's to keep.
		cell.value = Undefined
		if err == errNoSuper {
			err = r.throwError(errReference, "%s", errNoSuper.Error())
		}
		return v, err
	}
}
