package vm

// Explicit resource management: DisposableStack, AsyncDisposableStack, and
// the disposal they share with using declarations.
//
// A dispose capability is a stack of resources, each a value with the method
// that disposes of it. Disposing runs them last-first. An error from one does
// not stop the rest: it becomes the completion, and an error already in flight
// is kept as what it suppressed, so that nothing thrown is ever lost.
//
// The asynchronous kind awaits what each async disposer returns before going
// on to the next, which native code does by chaining promises: the rest of the
// walk runs from the handler that the awaited promise settles.

// disposableResource is one resource on a capability's stack.
type disposableResource struct {
	value Value
	// method disposes of it. It is undefined only for an `await using` of
	// null or undefined, which disposes of nothing but still awaits once.
	method Value
	async  bool
}

// disposeCapability is a stack of resources waiting to be disposed of.
type disposeCapability struct {
	stack []disposableResource
}

// disposableStackData is a DisposableStack's or an AsyncDisposableStack's
// state.
type disposableStackData struct {
	async    bool
	disposed bool
	cap      *disposeCapability
}

// addDisposableResource puts a value on the stack, finding its dispose method.
//
// A sync null or undefined is nothing to dispose of and is left off; an async
// one is kept, with no method, for the await it still owes.
func (r *Runtime) addDisposableResource(c *disposeCapability, v Value, async bool) error {
	if v.IsNullish() {
		if !async {
			return nil
		}
		c.stack = append(c.stack, disposableResource{value: Undefined, method: Undefined, async: true})
		return nil
	}
	if !v.IsObject() {
		return r.throwTypeError("%s is not an object and cannot be disposed of", r.describe(v))
	}
	method, err := r.getDisposeMethod(v, async)
	if err != nil {
		return err
	}
	if method.IsUndefined() {
		if async {
			return r.throwTypeError("the value has neither a Symbol.asyncDispose nor a Symbol.dispose method")
		}
		return r.throwTypeError("the value has no Symbol.dispose method")
	}
	c.stack = append(c.stack, disposableResource{value: v, method: method, async: async})
	return nil
}

// addDisposeCallback puts a callback on the stack, to be called with no
// receiver and no arguments.
func (r *Runtime) addDisposeCallback(c *disposeCapability, fn Value, async bool) {
	c.stack = append(c.stack, disposableResource{value: Undefined, method: fn, async: async})
}

// getDisposeMethod is GetDisposeMethod: Symbol.asyncDispose for an async
// resource, falling back to Symbol.dispose -- wrapped so that it returns a
// promise, and so that what it throws rejects that promise rather than
// escaping -- and Symbol.dispose for a sync one.
func (r *Runtime) getDisposeMethod(v Value, async bool) (Value, error) {
	if async {
		m, err := r.getMethod(v, r.wellKnown.asyncDispose)
		if err != nil || !m.IsUndefined() {
			return m, err
		}
		m, err = r.getMethod(v, r.wellKnown.dispose)
		if err != nil || m.IsUndefined() {
			return m, err
		}
		sync := m
		return Obj(r.newNativeFunc("", 0, func(rt *Runtime, this Value, _ []Value) (Value, error) {
			p := rt.newPromise()
			if _, err := rt.call(sync, this, nil); err != nil {
				if _, ok := err.(*Thrown); !ok {
					return Undefined, err
				}
				rt.rejectPromise(p, thrownValue(err))
			} else {
				rt.resolvePromise(p, Undefined)
			}
			return Obj(p), nil
		})), nil
	}
	return r.getMethod(v, r.wellKnown.dispose)
}

// getMethod is GetMethod: a property that must be callable if it is there.
func (r *Runtime) getMethod(v Value, sym *Symbol) (Value, error) {
	m, err := r.getValueProp(v, r.atoms.internSymbol(sym))
	if err != nil {
		return Undefined, err
	}
	if m.IsNullish() {
		return Undefined, nil
	}
	if !isCallable(m) {
		return Undefined, r.throwTypeError("%s is not a function", sym.Description)
	}
	return m, nil
}

// suppress folds a new error into the completion so far: the first error is
// the completion, and each later one becomes a SuppressedError holding it.
// An error that is not a JavaScript exception -- the host cancelling -- is
// never folded; it ends the disposal.
func (r *Runtime) suppress(completion, err error) error {
	if _, ok := err.(*Thrown); !ok {
		return err
	}
	if completion == nil {
		return err
	}
	if _, ok := completion.(*Thrown); !ok {
		return completion
	}
	e := r.newError(errSuppressed, "An error was suppressed during disposal")
	e.setOwnRaw(r.atoms.intern("error"), thrownValue(err), propWritable|propConfigurable)
	e.setOwnRaw(r.atoms.intern("suppressed"), thrownValue(completion), propWritable|propConfigurable)
	return r.throw(Obj(e))
}

// disposeResources disposes of a stack whose resources are all sync, and
// returns the completion as it stands at the end: the one it was given, or
// what the disposers threw.
func (r *Runtime) disposeResources(c *disposeCapability, completion error) error {
	stack := c.stack
	c.stack = nil
	for i := len(stack) - 1; i >= 0; i-- {
		res := stack[i]
		if _, err := r.call(res.method, res.value, nil); err != nil {
			completion = r.suppress(completion, err)
			if _, ok := err.(*Thrown); !ok {
				return completion
			}
		}
	}
	return completion
}

// disposeResourcesAsync disposes of a stack that may hold async resources,
// awaiting each as the specification does, and calls finish with the
// completion once it is done.
//
// The awaits are real ones, so that everything else waiting its turn gets it
// in the order the specification gives: an async disposer's result is
// awaited, a sync one that follows an `await using` of nothing is preceded by
// an await, and a stack whose only async entries disposed of nothing still
// awaits once at the end.
func (r *Runtime) disposeResourcesAsync(c *disposeCapability, completion error, finish func(error)) {
	stack := c.stack
	c.stack = nil
	needsAwait, hasAwaited := false, false
	var step func(i int, completion error)
	step = func(i int, completion error) {
		for ; i >= 0; i-- {
			res := stack[i]
			if !res.async && needsAwait && !hasAwaited {
				needsAwait = false
				at := i
				r.awaitThen(Undefined, func(rt *Runtime, _ Value) { step(at, completion) },
					func(rt *Runtime, _ Value) { step(at, completion) })
				return
			}
			if res.method.IsUndefined() {
				needsAwait = true
				continue
			}
			v, err := r.call(res.method, res.value, nil)
			if err != nil {
				completion = r.suppress(completion, err)
				if _, ok := err.(*Thrown); !ok {
					finish(completion)
					return
				}
				continue
			}
			if res.async {
				hasAwaited = true
				next := i - 1
				r.awaitThen(v, func(rt *Runtime, _ Value) { step(next, completion) },
					func(rt *Runtime, reason Value) { step(next, rt.suppress(completion, rt.throw(reason))) })
				return
			}
		}
		if needsAwait && !hasAwaited {
			r.awaitThen(Undefined, func(rt *Runtime, _ Value) { finish(completion) },
				func(rt *Runtime, _ Value) { finish(completion) })
			return
		}
		finish(completion)
	}
	step(len(stack)-1, completion)
}

func (r *Runtime) initDisposeBuiltins() {
	r.initDisposableStack(false)
	r.initDisposableStack(true)

	// Iterator.prototype[Symbol.dispose] closes the iterator, so that
	// `using it = ...` does what a loop left early does.
	r.defSymbolMethod(r.proto.iterator, r.wellKnown.dispose, "[Symbol.dispose]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			ret, err := rt.getValueProp(this, atomReturn)
			if err != nil {
				return Undefined, err
			}
			if ret.IsNullish() {
				return Undefined, nil
			}
			if !isCallable(ret) {
				return Undefined, rt.throwTypeError("the iterator's return is not a function")
			}
			_, err = rt.call(ret, this, nil)
			return Undefined, err
		})

	// %AsyncIteratorPrototype%[Symbol.asyncDispose] does the same for an async
	// iterator, and answers with a promise that settles once return has.
	r.defSymbolMethod(r.proto.asyncIterator, r.wellKnown.asyncDispose, "[Symbol.asyncDispose]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			p := rt.newPromise()
			fail := func(err error) (Value, error) {
				if _, ok := err.(*Thrown); !ok {
					return Undefined, err
				}
				rt.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			ret, err := rt.getValueProp(this, atomReturn)
			if err != nil {
				return fail(err)
			}
			if ret.IsNullish() {
				rt.resolvePromise(p, Undefined)
				return Obj(p), nil
			}
			if !isCallable(ret) {
				return fail(rt.throwTypeError("the iterator's return is not a function"))
			}
			res, err := rt.call(ret, this, nil)
			if err != nil {
				return fail(err)
			}
			wrapped, err := rt.promiseResolveWith(Obj(rt.promiseCtor), res)
			if err != nil {
				return fail(err)
			}
			// What return resolves to is not the answer: the promise settles
			// with undefined.
			unwrap := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				return Undefined, nil
			})
			rt.promiseThenInto(wrapped.Object(), Obj(unwrap), Undefined, p)
			return Obj(p), nil
		})
}

// initDisposableStack defines DisposableStack, or with async
// AsyncDisposableStack.
func (r *Runtime) initDisposableStack(async bool) {
	name, disposeName, hint := "DisposableStack", "dispose", r.wellKnown.dispose
	if async {
		name, disposeName, hint = "AsyncDisposableStack", "disposeAsync", r.wellKnown.asyncDispose
	}
	proto := newObject(r.proto.object, ClassObject)
	var ctor *Object
	ctor = r.newCtor(name, 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew(name); err != nil {
			return Undefined, err
		}
		p, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(p, ClassObject)
		o.data = &disposableStackData{async: async, cap: &disposeCapability{}}
		return Obj(o), nil
	})

	stackOf := func(rt *Runtime, this Value, method string) (*disposableStackData, error) {
		if this.IsObject() {
			if d, ok := this.Object().data.(*disposableStackData); ok && d.async == async {
				return d, nil
			}
		}
		return nil, rt.throwTypeError("%s.prototype.%s called on an incompatible receiver", name, method)
	}
	// The methods that add to a stack refuse one that has been disposed of,
	// since nothing would ever dispose of what they added.
	liveStackOf := func(rt *Runtime, this Value, method string) (*disposableStackData, error) {
		d, err := stackOf(rt, this, method)
		if err != nil {
			return nil, err
		}
		if d.disposed {
			return nil, rt.throwError(errReference, "the %s has already been disposed of", name)
		}
		return d, nil
	}

	r.defGetter(proto, "disposed", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := stackOf(rt, this, "disposed")
		if err != nil {
			return Undefined, err
		}
		return Bool(d.disposed), nil
	})

	var dispose *Object
	if async {
		dispose = r.defMethod(proto, disposeName, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			// Everything, a receiver of the wrong kind included, is answered
			// through the promise.
			p := rt.newPromise()
			d, err := stackOf(rt, this, disposeName)
			if err != nil {
				rt.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			if d.disposed {
				rt.resolvePromise(p, Undefined)
				return Obj(p), nil
			}
			d.disposed = true
			var hostErr error
			rt.disposeResourcesAsync(d.cap, nil, func(completion error) {
				switch completion.(type) {
				case nil:
					rt.resolvePromise(p, Undefined)
				case *Thrown:
					rt.rejectPromise(p, thrownValue(completion))
				default:
					hostErr = completion
				}
			})
			if hostErr != nil {
				return Undefined, hostErr
			}
			return Obj(p), nil
		})
	} else {
		dispose = r.defMethod(proto, disposeName, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := stackOf(rt, this, disposeName)
			if err != nil {
				return Undefined, err
			}
			if d.disposed {
				return Undefined, nil
			}
			d.disposed = true
			return Undefined, rt.disposeResources(d.cap, nil)
		})
	}
	// Symbol.dispose, or Symbol.asyncDispose, is the same function object.
	proto.setOwnRaw(r.atoms.internSymbol(hint), Obj(dispose), propWritable|propConfigurable)

	r.defMethod(proto, "use", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := liveStackOf(rt, this, "use")
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		if err := rt.addDisposableResource(d.cap, v, async); err != nil {
			return Undefined, err
		}
		return v, nil
	})

	r.defMethod(proto, "adopt", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := liveStackOf(rt, this, "adopt")
		if err != nil {
			return Undefined, err
		}
		v, onDispose := arg(args, 0), arg(args, 1)
		if !isCallable(onDispose) {
			return Undefined, rt.throwTypeError("%s.prototype.adopt requires a function", name)
		}
		// The callback is handed the value, and for a sync stack what it
		// returns is not kept.
		fn := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			res, err := rt.call(onDispose, Undefined, []Value{v})
			if !async {
				return Undefined, err
			}
			return res, err
		})
		rt.addDisposeCallback(d.cap, Obj(fn), async)
		return v, nil
	})

	r.defMethod(proto, "defer", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := liveStackOf(rt, this, "defer")
		if err != nil {
			return Undefined, err
		}
		onDispose := arg(args, 0)
		if !isCallable(onDispose) {
			return Undefined, rt.throwTypeError("%s.prototype.defer requires a function", name)
		}
		rt.addDisposeCallback(d.cap, onDispose, async)
		return Undefined, nil
	})

	// move hands everything on the stack to a new one, which is always of
	// the intrinsic kind, and leaves this one disposed of with nothing on it.
	r.defMethod(proto, "move", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := liveStackOf(rt, this, "move")
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassObject)
		o.data = &disposableStackData{async: async, cap: d.cap}
		d.cap = &disposeCapability{}
		d.disposed = true
		return Obj(o), nil
	})

	r.defToStringTag(proto, name)
	_ = ctor
}
