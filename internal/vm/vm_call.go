package vm

import (
	"errors"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Calls and frames: what a call from the interpreter's loop, from a tree or
// from a built-in goes through to run a function, and the frames it makes.
// Kept with the rest of the hot path; see the package comment.

// call invokes a callable value.
func (r *Runtime) call(fn Value, this Value, args []Value) (Value, error) {
	if !fn.IsObject() {
		return Undefined, r.throwTypeError("%s is not a function", r.describe(fn))
	}
	return r.callObject(fn.Object(), this, args, Undefined)
}

// callObject invokes a function object, dispatching to a native
// implementation, a bound function or compiled bytecode.
func (r *Runtime) callObject(o *Object, this Value, args []Value, newTarget Value) (Value, error) {
	// Calls are counted as well as instructions, because a program can run for
	// a long time without either finishing a frame's instruction budget or
	// leaving one loop: deep recursion, or a loop whose every iteration calls
	// something expensive. Both pass through here, and the count is nothing
	// beside the cost of the call it is attached to.
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if p := proxyOf(o); p != nil {
		if !newTarget.IsUndefined() {
			return r.proxyConstruct(p, args, newTarget)
		}
		return r.proxyCall(p, this, args)
	}
	fd := o.fn()
	if fd == nil {
		return Undefined, r.throwTypeError("value is not a function")
	}

	// A bound function prepends its stored arguments and replaces `this`,
	// except under `new`, where the original `this` is discarded anyway.
	if fd.bound {
		if newTarget.IsUndefined() {
			return r.callBound(fd, args)
		}
		merged := args
		if len(fd.extra.boundArgs) > 0 {
			merged = make([]Value, 0, len(fd.extra.boundArgs)+len(args))
			merged = append(merged, fd.extra.boundArgs...)
			merged = append(merged, args...)
		}
		boundThis := fd.extra.boundThis
		if !newTarget.IsUndefined() {
			boundThis = this
		}
		// Each bound function in a chain is a level of Go recursion with no
		// frame of its own, so it is counted, or a long enough chain would
		// take the call past the Go stack.
		if err := r.nest(); err != nil {
			return Undefined, err
		}
		v, err := r.callObject(fd.extra.boundTarget, boundThis, merged, newTarget)
		r.unnest()
		return v, err
	}

	if fd.native != nil {
		if err := r.nativeFrame(o, fd, this, args, newTarget); err != nil {
			return Undefined, err
		}
		// A built-in runs in its own realm, whoever calls it.
		if re := fd.realm; re != nil && re != r.Realm {
			prev := r.Realm
			r.Realm = re
			v, err := fd.native(r, this, args)
			r.Realm = prev
			r.frameDepth--
			if r.meter != nil && err == nil {
				err = r.chargeResult(v)
			}
			return v, err
		}
		v, err := fd.native(r, this, args)
		r.frameDepth--
		if r.meter != nil && err == nil {
			err = r.chargeResult(v)
		}
		return v, err
	}

	if fd.closure == nil {
		return Undefined, r.throwTypeError("function has no implementation")
	}
	// So does a compiled function.
	var v Value
	var err error
	if re := fd.closure.realm; re != r.Realm {
		prev := r.Realm
		r.Realm = re
		v, err = r.callClosure(o, fd, this, args, newTarget)
		r.Realm = prev
	} else {
		v, err = r.callClosure(o, fd, this, args, newTarget)
	}
	if err == errNoSuper {
		err = r.throwError(errReference, "%s", errNoSuper.Error())
	}
	return v, err
}

// nativeFrame pushes the frame a built-in runs in, so that stack traces
// include it; the caller pops it when the built-in returns.
func (r *Runtime) nativeFrame(o *Object, fd *funcData, this Value, args []Value, newTarget Value) error {
	if r.frameDepth >= r.maxFrames {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	f := r.pushFrame()
	// The closure and new.target are written whatever was there: the frame
	// last at this depth was usually a compiled function's, with a closure,
	// so a test would only add a load and a branch to the store. The
	// handlers and open upvalues are almost always empty already, and are
	// reset only where they are not, as runFD does: a read is cheaper than a
	// pointer written.
	f.cl = nil
	f.native = fd.name
	f.this = this
	f.newTarget = newTarget
	f.callee = o
	f.args = args
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	return nil
}

// pushNativeFrame is what callObject does for the built-in o up to running
// it: the call is counted, and its frame pushed. It is for running what a
// built-in of this realm does directly, as the built-in would.
func (r *Runtime) pushNativeFrame(o *Object, this Value, args []Value, newTarget Value) error {
	if err := r.tick(); err != nil {
		return err
	}
	return r.nativeFrame(o, o.fn(), this, args, newTarget)
}

// callDirect is call for the engine's own calls of what is most often a
// compiled function: the interpreter's and the tree tier's call
// instructions, f.call(...), a bound function's target, a getter or setter
// a property cache remembers, and the callbacks sort, forEach, map and the
// other array methods call once per element. What call would find out
// through callObject and callClosure, it asks in one place, in this order:
//
//   - a function planTreeCall has given a tree is called on it, by
//     callTree;
//   - a Math function given numbers is applied here, without a frame;
//   - Function.prototype.call is made by callThrough;
//   - a compiled function of this realm that runs when called -- not a
//     generator, whose body waits for next(), not a class constructor,
//     which a call refuses, not bound -- is run directly: a pure body by
//     pureCall, and a leaf that reads or writes its this by leafCall,
//     without a frame where they can, and otherwise by runFD, after which
//     planTreeCall decides about it;
//   - a built-in of this realm is called in a native frame.
//
// Anything else is called as call calls it.
//
// callRest, which a tree's call node asks once it has made the first test
// itself, repeats every case after it: a change to one is a change to both.
func (r *Runtime) callDirect(callee, this Value, args []Value) (Value, error) {
	if callee.IsObject() {
		o := callee.Object()
		fd := o.fn()
		// A function planTreeCall has given a tree is called on it, without
		// what the rest asks of a call.
		if fd != nil && fd.treeCall != nil && fd.closure.realm == r.Realm {
			if err := r.tick(); err != nil {
				return Undefined, err
			}
			return r.callTree(o, fd, this, args, false)
		}
		if fd != nil && fd.mathOp != 0 {
			// Math.floor(x) and its kind, given a number, and Math.max(a, b)
			// and Math.min(a, b), given two, cannot throw or call anything,
			// so no frame is made to show in a stack trace: the function is
			// applied here.
			if v, ok := mathCall(fd.mathOp, args); ok {
				return v, nil
			}
		}
		if o == r.callFn {
			return r.callThrough(o, this, args)
		}
		if fd != nil && fd.native == nil && !fd.bound &&
			fd.closure != nil && fd.closure.realm == r.Realm {
			if fn := fd.closure.fn; fn.DirectCall {
				if err := r.tick(); err != nil {
					return Undefined, err
				}
				// A body that only reads or writes a property of its this is
				// answered here where it can be, without a frame.
				if leaf := fn.Leaf; leaf == bytecode.LeafPure {
					if v, ok := r.pureCall(fd, this, args); ok {
						return v, nil
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
		if fd != nil && fd.native != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) {
			// A built-in of this realm is called as callObject calls one,
			// without going through call and callObject to find out.
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
	}
	return r.call(callee, this, args)
}

// callThrough is f.call(x, ...rest) from the interpreter's loop: what call
// does, done here -- its frame pushed, as the built-in would have it, and f
// called with x and the rest the way the loop calls a function, rather than
// through call's own and then call's call of f. A class uses it to run its
// parent's constructor on the object it is building.
func (r *Runtime) callThrough(call *Object, f Value, args []Value) (Value, error) {
	this, rest := Undefined, args
	if len(args) > 0 {
		this, rest = args[0], args[1:]
	}
	if err := r.pushNativeFrame(call, f, args, Undefined); err != nil {
		return Undefined, err
	}
	v, err := r.callDirect(f, this, rest)
	r.frameDepth--
	return v, err
}

// applyArguments is f.apply(x, arguments) in a function that makes its
// arguments object only when it has to. The built-in apply given the object
// would call f with what the object holds -- the arguments as passed, or as
// the parameters now are for an object that maps them -- so it is given them
// directly, in apply's frame as ever. Any other apply gets the object, made
// now and kept in its slot, so that every call is given the same one.
func (r *Runtime) applyArguments(f *frame, slot uint32, target, apply, this Value) (Value, error) {
	if !apply.IsObject() || apply.Object() != r.applyFn {
		o := f.locals[slot]
		if !o.IsObject() {
			o = Obj(r.newArgumentsObject(f))
			f.locals[slot] = o
		}
		return r.call(apply, target, []Value{this, o})
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show.
				args = append([]Value(nil), args...)
				copy(args, f.locals[:n])
				break
			}
		}
	}
	return r.applyCall(target, this, args)
}

// applyCall is target.apply(this, arguments) by the built-in apply, given
// the arguments themselves: apply's frame is pushed, as the built-in would
// have it, and target called with them.
func (r *Runtime) applyCall(target, this Value, args []Value) (Value, error) {
	// apply's own arguments are what its frame records, which is all they
	// are for: they are kept on the runtime's argument stack rather than
	// allocated.
	i := len(r.argStack)
	r.argStack = append(r.argStack, this, Undefined)
	var v Value
	err := r.pushNativeFrame(r.applyFn, target, r.argStack[i:i+2:i+2], Undefined)
	if err == nil {
		v, err = r.call(target, this, args)
		r.frameDepth--
	}
	clear(r.argStack[i:])
	r.argStack = r.argStack[:i]
	return v, err
}

// callClosure calls a compiled function, in the realm it belongs to.
func (r *Runtime) callClosure(o *Object, fd *funcData, this Value, args []Value, newTarget Value) (Value, error) {
	// An arrow ignores the this and new.target it was called with.
	if fd.arrow {
		this = fd.extra.lexThis
		newTarget = fd.extra.lexNewTarget
	}

	// A generator or async function does not run its body on call. A generator
	// returns an object whose next method drives it; an async function starts
	// immediately but returns a promise at its first await.
	if fn := fd.closure.fn; isGeneratorTemplate(fn) {
		// A generator builds its frames itself, so the receiver is coerced here
		// rather than by run: a sloppy-mode async function called with no
		// receiver sees the global object like any other.
		if fn.UsesThis && !this.IsObject() && !fn.Strict {
			if this.IsNullish() {
				this = r.globalThis
			} else {
				w, err := r.toObject(this)
				if err != nil {
					return Undefined, err
				}
				this = Obj(w)
			}
		}
		gen, err := r.newGenerator(fd.closure, this, args, o, fn.Async, newTarget)
		if err != nil {
			// A generator binds its parameters at the call, so a destructuring
			// error surfaces here rather than at the first next(). An async
			// function turns it into a rejection instead, which is what makes
			// an async function never throw synchronously.
			if fn.Async && !fn.Generator {
				p := r.newPromise()
				r.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			return Undefined, err
		}
		g := gen.data.(*generator)
		switch {
		case fn.Generator:
			// An async generator is still a generator: it returns an object
			// whose next method drives it, differing only in that the method
			// returns a promise.
			return Obj(gen), nil
		case fn.Async:
			return r.runAsync(g), nil
		}
		return Obj(gen), nil
	}
	// A class constructor has to be constructed. Calling one would run its
	// body with no object to initialize -- and in a derived class with no
	// `this` at all, since super() is what binds it.
	if newTarget.IsUndefined() && isClassConstructorKind(fd.closure.fn.Kind) {
		return Undefined, r.throwTypeError(
			"class constructor %s cannot be invoked without \"new\"", fd.name)
	}
	return r.runFD(fd.closure, this, args, newTarget, o, fd)
}

// isClassConstructorKind reports whether a function is a class's constructor,
// which is the one kind that may only be constructed.
func isClassConstructorKind(k bytecode.FuncKind) bool {
	return k == bytecode.KindConstructor || k == bytecode.KindDerivedConstructor
}

// describe renders a value for an error message without risking a callback
// into user code, which toString would.
func (r *Runtime) describe(v Value) string {
	switch v.Kind() {
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindString:
		return "\"" + v.String().Go() + "\""
	case KindNumber:
		return jsnum.FormatFloat(v.Number())
	case KindBool:
		if v.BoolValue() {
			return "true"
		}
		return "false"
	}
	return v.Kind().String()
}

// run executes a compiled function, and the chain of tail calls it ends in.
//
// A frame that ends in a tail call is given up before the callee's frame is
// made, in this same call of run, so that a chain of them uses one frame and
// one level of the Go stack however long it is. A callee this cannot be done
// for -- a native function, a proxy, a generator -- is called the ordinary
// way, which ends the chain.
func (r *Runtime) run(cl *closure, this Value, args []Value, newTarget Value, callee *Object) (Value, error) {
	var fd *funcData
	if callee != nil {
		fd = callee.fn()
	}
	return r.runFD(cl, this, args, newTarget, callee, fd)
}

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
		if fd.extra != nil && len(fd.extra.lexWith) > 0 {
			f.withScopes = fd.extra.lexWith[:len(fd.extra.lexWith):len(fd.extra.lexWith)]
		}
		// What an eval declared in an enclosing function is still in scope
		// here, whether or not this one has anything of its own.
		if fd.extra != nil && fd.extra.lexEvalVars != nil {
			f.evalVars = fd.extra.lexEvalVars
		}
	}
	if fn.HasDirectEval {
		// The body contains a direct eval, so it needs somewhere for the vars
		// that eval may declare. It goes on the scope chain, inside whatever
		// the enclosing functions put there: a name the evaluated code
		// declares is found the way a `with` object's properties are.
		f.evalVars = newObject(nil, ClassObject)
		f.evalVars.flags |= objEvalVars
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
			if len(fd.extra.boundArgs) > 0 {
				merged := make([]Value, 0, len(fd.extra.boundArgs)+len(args))
				merged = append(merged, fd.extra.boundArgs...)
				args = append(merged, args...)
			}
			o, this = fd.extra.boundTarget, fd.extra.boundThis
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
			if !fd.extra.lexNewTarget.IsUndefined() {
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
