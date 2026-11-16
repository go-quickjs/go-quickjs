package vm

import (
	"math"
)

// Constructions: constructWithTarget's of a compiled constructor
// (constructRun) and of a built-in whose construction is a native leaf,
// applied without a frame (constructNativeAt); super()'s of its parent
// (superConstruct), after the parent's instance initializer (superInit);
// and the realm's Array constructor's leaf (setArrayCtor).
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

var (
	// constructRun is constructWithTarget's construction of a compiled
	// constructor, with this the object made for it: runFD's.
	constructRun func(r *Runtime, this Value, args []Value, newTarget Value, o *Object, fd *funcData) (Value, error)

	// superConstruct is super()'s construction of its parent in frame f,
	// with f's object as this and new.target, after the parent's instance
	// initializer: callObject's.
	superConstruct func(r *Runtime, parent *Object, f *frame, args []Value, newTarget Value) (Value, error)

	// constructNativeAt is constructWithTarget's construction by a
	// built-in: a native leaf whose construction is its call
	// (elemNewFirst...) applied without a frame where new.target is the
	// built-in itself, and constructNative otherwise, where it had called
	// constructNative.
	constructNativeAt func(r *Runtime, o *Object, args []Value, newTarget Value, derived bool) (Value, error)

	// superInit is super()'s instance initializer of a base parent, in
	// frame f, run on f's object before the parent's body as runFD's
	// construction of it would -- where super() has not bound this yet: a
	// second super()'s parent makes an object of its own, which
	// superConstruct's construction initializes. superCall calls it where
	// it had called initInstanceElements.
	superInit func(r *Runtime, parent *Object, f *frame) error

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

	superInit = func(r *Runtime, parent *Object, f *frame) error {
		if f.thisRef.init {
			return nil
		}
		// initInstanceElements, written out: a class without fields or
		// private methods has no initializer, the common case.
		fd := parent.fn()
		if fd == nil || fd.fieldInit() == nil {
			return nil
		}
		_, err := r.callObject(fd.fieldInit(), f.this, nil, Undefined)
		return err
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
		return r.runFD(fd.closure, this, args, newTarget, o, fd)
	}

	superConstruct = func(r *Runtime, parent *Object, f *frame, args []Value, newTarget Value) (Value, error) {
		if f.thisRef.init {
			// A second super(): the parent's [[Construct]] with new.target
			// makes an object of its own and runs the parent on it, its
			// fields included (superInit left them to it), and only then is
			// binding this an error (BindThisValue); the object the first
			// super() bound is left as it is.
			if _, err := r.constructWithTarget(Obj(parent), args, newTarget); err != nil {
				return Undefined, err
			}
			return Undefined, r.throwError(errReference, "super() has already been called")
		}
		return r.callObject(parent, f.this, args, newTarget)
	}
}
