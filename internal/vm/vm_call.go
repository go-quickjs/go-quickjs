package vm

import (
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
		if len(fd.extra.rare.boundArgs) > 0 {
			merged = make([]Value, 0, len(fd.extra.rare.boundArgs)+len(args))
			merged = append(merged, fd.extra.rare.boundArgs...)
			merged = append(merged, args...)
		}
		boundThis := fd.extra.rare.boundThis
		if !newTarget.IsUndefined() {
			boundThis = this
		}
		// Each bound function in a chain is a level of Go recursion with no
		// frame of its own, so it is counted, or a long enough chain would
		// take the call past the Go stack.
		if err := r.nest(); err != nil {
			return Undefined, err
		}
		v, err := r.callObject(fd.extra.rare.boundTarget, boundThis, merged, newTarget)
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
// a property cache remembers, and the callbacks built-ins call once per
// element: sort's, forEach's, map's and the other array methods', the
// iterator helpers', Map's and Set's forEach, groupBy's, Array.from's and
// %TypedArray%.from's mapping, replace's function, JSON's replacer and
// reviver. What call would find out
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
			if c := fd.treeCall.reg; c != nil && c.wide {
				return r.regDirect(o, fd, c, this, args)
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
					if pureFirst(fd, fn) {
						if v, ok := r.pureCall(fd, this, args); ok {
							if jitBuilt && r.jitPureCounted() {
								// Counted for the JIT where the caller may
								// count it, which a built-in's frame does
								// not (pureCallCounted).
								r.jitCountPureCall()
							}
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
					this, newTarget = fd.extra.lexThis, fd.extra.newTarget()
				}
				v, err := directRest[struct{}](r, o, fd, this, args, newTarget)
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
			if op := fd.elemOp; op >= elemNativeFirst {
				// A native leaf, its own function called without a frame
				// where its check passes (leafChecks).
				if r.meter == nil && leafChecks[op](r, this, args) {
					if v, err := fd.native(r, this, args); err == nil {
						return v, nil
					}
				}
			} else if op != 0 {
				// Array.prototype.push or pop on a dense array of its own,
				// which calls nothing and cannot throw: as Math's functions,
				// it is applied without a frame to show in a stack trace.
				if v, ok := r.pureElemOp(op, this, args, true); ok {
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
// have it, and target called with them -- as a call from the interpreter's
// loop is made, by callDirect, so that a function called this way is given
// its tree's call (planTreeCall) like any other.
func (r *Runtime) applyCall(target, this Value, args []Value) (Value, error) {
	// apply's own arguments are what its frame records, which is all they
	// are for: they are kept on the runtime's argument stack rather than
	// allocated.
	i := len(r.argStack)
	r.argStack = append(r.argStack, this, Undefined)
	var v Value
	err := r.pushNativeFrame(r.applyFn, target, r.argStack[i:i+2:i+2], Undefined)
	if err == nil {
		v, err = r.callDirect(target, this, args)
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
		newTarget = fd.extra.newTarget()
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
		g := genOf(gen)
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
	if fd.closure.fn.Reg != nil {
		// Register mode's code, entered as its own (regRunFD).
		if v, err, ok := r.regRunFD(fd.closure, this, args, newTarget, o, fd); ok {
			return v, err
		}
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
	if cl.fn.Reg != nil {
		if v, err, ok := r.regRunFD(cl, this, args, newTarget, callee, fd); ok {
			return v, err
		}
	}
	return r.runFD(cl, this, args, newTarget, callee, fd)
}
