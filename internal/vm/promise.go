package vm

// Promises, and the async functions built on them.
//
// A promise is a value plus a state that changes at most once. Reactions are
// never run synchronously: settling a promise queues its reactions as jobs, and
// the queue drains between turns. That ordering guarantee is the whole point of
// the design -- a `then` callback must not run before the code that registered
// it has finished -- so it is enforced here rather than left to the caller.
//
// An async function is a generator whose suspensions are awaits. Each await
// hands its operand to the promise machinery, which resumes the generator when
// the operand settles. That is why generators had to come first.

type promiseState uint8

const (
	promisePending promiseState = iota
	promiseFulfilled
	promiseRejected
)

// promiseData is a promise's internal state.
type promiseData struct {
	state promiseState
	value Value

	// reactions are the callbacks waiting for the promise to settle. They are
	// cleared once it does, since a promise settles only once.
	reactions []reaction

	// handled records whether a rejection has been observed, which an unhandled
	// rejection reporter consults.
	handled bool
}

// reaction is one registered pair of callbacks plus the promise they resolve.
type reaction struct {
	onFulfilled Value
	onRejected  Value
	result      *Object
}

func (r *Runtime) promiseOf(this Value, name string) (*promiseData, error) {
	if !this.IsObject() || this.Object().class != ClassPromise {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	p, ok := this.Object().data.(*promiseData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized promise", name)
	}
	return p, nil
}

// newPromise returns a pending promise.
func (r *Runtime) newPromise() *Object {
	o := newObject(r.proto.promise, ClassPromise)
	o.data = &promiseData{}
	return o
}

// resolvePromise settles a promise with a value.
//
// A thenable value is adopted rather than stored: resolving with a promise
// makes this promise follow it, which is what lets `return somePromise` inside
// a then callback flatten.
func (r *Runtime) resolvePromise(o *Object, v Value) {
	p, ok := o.data.(*promiseData)
	if !ok || p.state != promisePending {
		return
	}

	// Resolving a promise with itself is a TypeError rather than a hang.
	if v.IsObject() && v.Object() == o {
		r.rejectPromise(o, Obj(r.newError(errType, "a promise cannot resolve to itself")))
		return
	}

	if v.IsObject() {
		then, err := r.getProp(v.Object(), atomThen, v)
		if err != nil {
			r.rejectPromise(o, thrownValue(err))
			return
		}
		if isCallable(then) {
			// Adopting a thenable happens in a job, not synchronously, so that
			// the ordering guarantee holds for it too.
			r.enqueueJob(func() {
				resolveFn := r.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
					rt.resolvePromise(o, arg(args, 0))
					return Undefined, nil
				})
				rejectFn := r.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
					rt.rejectPromise(o, arg(args, 0))
					return Undefined, nil
				})
				if _, err := r.call(then, v, []Value{Obj(resolveFn), Obj(rejectFn)}); err != nil {
					r.rejectPromise(o, thrownValue(err))
				}
			})
			return
		}
	}

	p.state = promiseFulfilled
	p.value = v
	r.scheduleReactions(p)
}

// rejectPromise settles a promise with a reason.
func (r *Runtime) rejectPromise(o *Object, reason Value) {
	p, ok := o.data.(*promiseData)
	if !ok || p.state != promisePending {
		return
	}
	p.state = promiseRejected
	p.value = reason
	r.scheduleReactions(p)
}

// scheduleReactions queues the callbacks of a promise that has just settled.
func (r *Runtime) scheduleReactions(p *promiseData) {
	reactions := p.reactions
	p.reactions = nil
	for _, rc := range reactions {
		r.enqueueReaction(p, rc)
	}
}

// enqueueReaction queues one reaction against a settled promise.
func (r *Runtime) enqueueReaction(p *promiseData, rc reaction) {
	state, value := p.state, p.value
	if state == promiseRejected {
		p.handled = true
	}
	r.enqueueJob(func() {
		handler := rc.onFulfilled
		if state == promiseRejected {
			handler = rc.onRejected
		}
		if !isCallable(handler) {
			// Without a handler the outcome passes straight through, which is
			// what makes a then chain propagate a rejection.
			if state == promiseRejected {
				r.rejectPromise(rc.result, value)
			} else {
				r.resolvePromise(rc.result, value)
			}
			return
		}
		res, err := r.call(handler, Undefined, []Value{value})
		if err != nil {
			// A handler that throws rejects the promise it resolves, rather
			// than propagating out of the job queue.
			r.rejectPromise(rc.result, thrownValue(err))
			return
		}
		r.resolvePromise(rc.result, res)
	})
}

// thrownValue extracts the JavaScript value from an engine error, so that a Go
// error crossing into promise machinery still carries something catchable.
func thrownValue(err error) Value {
	if t, ok := err.(*Thrown); ok {
		return t.Value
	}
	return Str(NewString(err.Error()))
}

// enqueueJob adds a microtask.
func (r *Runtime) enqueueJob(fn func()) {
	r.microtasks = append(r.microtasks, fn)
}

// DrainJobs runs queued microtasks until none remain.
//
// A job may queue more jobs, which is how a chain of thens progresses; the loop
// is bounded so that a promise chain that queues itself forever cannot wedge
// the host.
func (r *Runtime) DrainJobs() error {
	const maxJobs = 1_000_000
	for n := 0; len(r.microtasks) > 0; n++ {
		if n > maxJobs {
			return r.throwRangeError("the microtask queue did not drain")
		}
		job := r.microtasks[0]
		r.microtasks = r.microtasks[1:]
		job()
		if err := r.checkInterrupt(); err != nil {
			return err
		}
	}
	return nil
}

// HasPendingJobs reports whether any microtask is queued.
func (r *Runtime) HasPendingJobs() bool { return len(r.microtasks) > 0 }

// promiseThen registers a reaction and returns the promise it resolves.
func (r *Runtime) promiseThen(o *Object, onFulfilled, onRejected Value) *Object {
	p, ok := o.data.(*promiseData)
	if !ok {
		return r.newPromise()
	}
	result := r.newPromise()
	rc := reaction{onFulfilled: onFulfilled, onRejected: onRejected, result: result}

	if p.state == promisePending {
		p.reactions = append(p.reactions, rc)
		return result
	}
	r.enqueueReaction(p, rc)
	return result
}

// promiseCapability is a promise together with the two functions that settle
// it, which is how the specification hands one around.
//
// It exists so that a subclass can take part: Promise.resolve called on a
// subclass has to produce an instance of that subclass, which means going
// through its constructor rather than building an intrinsic promise.
type promiseCapability struct {
	promise *Object
	resolve Value
	reject  Value
}

// newPromiseCapability builds a promise using the given constructor.
func (r *Runtime) newPromiseCapability(ctor Value) (*promiseCapability, error) {
	if !ctor.IsObject() {
		return nil, r.throwTypeError("a promise constructor is required")
	}
	if ctor.Object() == r.promiseCtor {
		// The intrinsic constructor is known not to do anything observable, so
		// the whole executor dance is skipped.
		o := r.newPromise()
		return &promiseCapability{
			promise: o,
			resolve: Obj(r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
				rt.resolvePromise(o, arg(a, 0))
				return Undefined, nil
			})),
			reject: Obj(r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
				rt.rejectPromise(o, arg(a, 0))
				return Undefined, nil
			})),
		}, nil
	}

	// The two slots start as undefined rather than as the zero Value, so that
	// the guard below can tell "not yet supplied" from "supplied".
	cap := &promiseCapability{resolve: Undefined, reject: Undefined}
	executor := r.newNativeFunc("", 2, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		if !cap.resolve.IsUndefined() || !cap.reject.IsUndefined() {
			return Undefined, rt.throwTypeError("the promise resolvers were supplied twice")
		}
		cap.resolve, cap.reject = arg(a, 0), arg(a, 1)
		return Undefined, nil
	})
	res, err := r.construct(ctor, []Value{Obj(executor)})
	if err != nil {
		return nil, err
	}
	if !isCallable(cap.resolve) || !isCallable(cap.reject) {
		return nil, r.throwTypeError("the promise constructor did not supply resolve and reject")
	}
	if !res.IsObject() {
		return nil, r.throwTypeError("the promise constructor did not return an object")
	}
	cap.promise = res.Object()
	return cap, nil
}

// speciesConstructor finds the constructor a derived object should be built
// with, which is what lets a subclass decide what its methods return.
func (r *Runtime) speciesConstructor(o *Object, fallback *Object) (Value, error) {
	c, err := r.getProp(o, atomConstructor, Obj(o))
	if err != nil {
		return Undefined, err
	}
	if c.IsUndefined() {
		return Obj(fallback), nil
	}
	if !c.IsObject() {
		return Undefined, r.throwTypeError("the constructor property must be an object")
	}
	sp, err := r.getProp(c.Object(), r.atoms.internSymbol(r.wellKnown.species), c)
	if err != nil {
		return Undefined, err
	}
	if sp.IsNullish() {
		return Obj(fallback), nil
	}
	if !sp.IsObject() || sp.Object().fn() == nil || sp.Object().fn().ctorKind == ctorNone {
		return Undefined, r.throwTypeError("the species is not a constructor")
	}
	return sp, nil
}

// defSpecies gives a constructor the Symbol.species accessor, which by default
// simply hands back the constructor a method was reached through.
func (r *Runtime) defSpecies(ctor *Object) {
	get := r.newNativeFunc("get [Symbol.species]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) { return this, nil })
	r.defineAccessor(ctor, r.atoms.internSymbol(r.wellKnown.species), get, nil, propConfigurable)
}

// promiseThenSpecies is then with the result built by the species constructor,
// so that a subclass's then returns an instance of the subclass.
func (r *Runtime) promiseThenSpecies(o *Object, onFulfilled, onRejected Value) (Value, error) {
	ctor, err := r.speciesConstructor(o, r.promiseCtor)
	if err != nil {
		return Undefined, err
	}
	if ctor.IsObject() && ctor.Object() == r.promiseCtor {
		return Obj(r.promiseThen(o, onFulfilled, onRejected)), nil
	}
	cap, err := r.newPromiseCapability(ctor)
	if err != nil {
		return Undefined, err
	}
	// The intrinsic machinery settles an ordinary promise, whose outcome is
	// then forwarded through the subclass's own resolvers.
	inner := r.promiseThen(o, onFulfilled, onRejected)
	r.promiseThen(inner,
		Obj(r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			return rt.call(cap.resolve, Undefined, []Value{arg(a, 0)})
		})),
		Obj(r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			return rt.call(cap.reject, Undefined, []Value{arg(a, 0)})
		})))
	return Obj(cap.promise), nil
}

func (r *Runtime) initPromiseBuiltins() {
	r.proto.promise = newObject(r.proto.object, ClassObject)
	p := r.proto.promise

	ctor := r.newCtor("Promise", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		executor := arg(args, 0)
		if !isCallable(executor) {
			return Undefined, rt.throwTypeError("the Promise executor must be a function")
		}
		o := rt.newPromise()
		resolveFn := rt.newNativeFunc("resolve", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			rt.resolvePromise(o, arg(args, 0))
			return Undefined, nil
		})
		rejectFn := rt.newNativeFunc("reject", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			rt.rejectPromise(o, arg(args, 0))
			return Undefined, nil
		})
		// The executor runs synchronously, unlike everything else here.
		if _, err := rt.call(executor, Undefined, []Value{Obj(resolveFn), Obj(rejectFn)}); err != nil {
			rt.rejectPromise(o, thrownValue(err))
		}
		return Obj(o), nil
	})

	r.promiseCtor = ctor
	r.defSpecies(ctor)

	r.defMethod(ctor, "resolve", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("Promise.resolve requires a constructor receiver")
		}
		// A promise already built by this very constructor is handed back
		// unchanged rather than wrapped in another.
		if v.IsObject() && v.Object().class == ClassPromise {
			c, err := rt.getProp(v.Object(), atomConstructor, v)
			if err != nil {
				return Undefined, err
			}
			if c.SameValue(this) {
				return v, nil
			}
		}
		cap, err := rt.newPromiseCapability(this)
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.call(cap.resolve, Undefined, []Value{v}); err != nil {
			return Undefined, err
		}
		return Obj(cap.promise), nil
	})
	r.defMethod(ctor, "reject", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("Promise.reject requires a constructor receiver")
		}
		cap, err := rt.newPromiseCapability(this)
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.call(cap.reject, Undefined, []Value{arg(args, 0)}); err != nil {
			return Undefined, err
		}
		return Obj(cap.promise), nil
	})

	r.defMethod(ctor, "all", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.promiseCombinator(this, arg(args, 0), combinatorAll)
	})
	r.defMethod(ctor, "allSettled", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.promiseCombinator(this, arg(args, 0), combinatorAllSettled)
	})
	r.defMethod(ctor, "race", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.promiseCombinator(this, arg(args, 0), combinatorRace)
	})
	r.defMethod(ctor, "any", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.promiseCombinator(this, arg(args, 0), combinatorAny)
	})

	r.defMethod(p, "then", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.promiseOf(this, "Promise.prototype.then"); err != nil {
			return Undefined, err
		}
		return rt.promiseThenSpecies(this.Object(), arg(args, 0), arg(args, 1))
	})
	r.defMethod(p, "catch", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.promiseOf(this, "Promise.prototype.catch"); err != nil {
			return Undefined, err
		}
		return rt.promiseThenSpecies(this.Object(), Undefined, arg(args, 0))
	})
	r.defMethod(p, "finally", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.promiseOf(this, "Promise.prototype.finally"); err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		// A finally callback receives no value and does not change the
		// outcome, so each side re-raises what it was given.
		onFulfilled := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			if isCallable(cb) {
				if _, err := rt.call(cb, Undefined, nil); err != nil {
					return Undefined, err
				}
			}
			return arg(a, 0), nil
		})
		onRejected := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			if isCallable(cb) {
				if _, err := rt.call(cb, Undefined, nil); err != nil {
					return Undefined, err
				}
			}
			return Undefined, rt.throw(arg(a, 0))
		})
		return Obj(rt.promiseThen(this.Object(), Obj(onFulfilled), Obj(onRejected))), nil
	})

	r.defToStringTag(p, "Promise")
}

// combinatorKind selects which of the Promise combinators is being built.
type combinatorKind uint8

const (
	combinatorAll combinatorKind = iota
	combinatorAllSettled
	combinatorRace
	combinatorAny
)

// promiseCombinator implements Promise.all and its relatives.
func (r *Runtime) promiseCombinator(ctor Value, iterable Value, kind combinatorKind) (Value, error) {
	// The receiver is the constructor, so a subclass's Promise.all yields an
	// instance of the subclass. Anything that is not a constructor is refused
	// before the iterable is touched.
	cap, err := r.newPromiseCapability(ctor)
	if err != nil {
		return Undefined, err
	}
	result := cap.promise
	settle := func(v Value) { r.call(cap.resolve, Undefined, []Value{v}) }
	fail := func(v Value) { r.call(cap.reject, Undefined, []Value{v}) }

	var items []Value
	if err := r.iterate(iterable, func(v Value) error {
		items = append(items, v)
		return nil
	}); err != nil {
		fail(thrownValue(err))
		return Obj(result), nil
	}

	if len(items) == 0 {
		switch kind {
		case combinatorAll, combinatorAllSettled:
			settle(Obj(r.newArrayFrom(nil)))
		case combinatorAny:
			fail(Obj(r.newError(errAggregate, "all promises were rejected")))
		}
		// Promise.race over nothing stays pending forever, which is what the
		// specification says.
		return Obj(result), nil
	}

	values := make([]Value, len(items))
	for i := range values {
		values[i] = Undefined
	}
	remaining := len(items)

	for i, item := range items {
		idx := i
		// Every element is coerced to a promise, so a plain value works too.
		p := r.toPromise(item)

		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			v := arg(a, 0)
			switch kind {
			case combinatorRace, combinatorAny:
				settle(v)
				return Undefined, nil
			case combinatorAllSettled:
				o := newObject(rt.proto.object, ClassObject)
				o.setOwnRaw(rt.atoms.intern("status"), Str(NewString("fulfilled")), propDefault)
				o.setOwnRaw(atomValue, v, propDefault)
				values[idx] = Obj(o)
			default:
				values[idx] = v
			}
			remaining--
			if remaining == 0 {
				settle(Obj(rt.newArrayFrom(values)))
			}
			return Undefined, nil
		})

		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			reason := arg(a, 0)
			switch kind {
			case combinatorAll, combinatorRace:
				// One rejection settles the whole thing.
				fail(reason)
				return Undefined, nil
			case combinatorAllSettled:
				o := newObject(rt.proto.object, ClassObject)
				o.setOwnRaw(rt.atoms.intern("status"), Str(NewString("rejected")), propDefault)
				o.setOwnRaw(rt.atoms.intern("reason"), reason, propDefault)
				values[idx] = Obj(o)
			default: // any
				values[idx] = reason
			}
			remaining--
			if remaining == 0 {
				if kind == combinatorAny {
					fail(Obj(rt.newError(errAggregate, "all promises were rejected")))
					return Undefined, nil
				}
				settle(Obj(rt.newArrayFrom(values)))
			}
			return Undefined, nil
		})

		r.promiseThen(p, Obj(onFulfilled), Obj(onRejected))
	}
	return Obj(result), nil
}

// toPromise coerces a value to a promise, wrapping a plain value and adopting a
// thenable.
func (r *Runtime) toPromise(v Value) *Object {
	if v.IsObject() && v.Object().class == ClassPromise {
		return v.Object()
	}
	o := r.newPromise()
	r.resolvePromise(o, v)
	return o
}

// ---------------------------------------------------------------------------
// Async functions
// ---------------------------------------------------------------------------

// runAsync starts an async function and returns the promise for its result.
//
// The body runs as a generator whose suspensions are awaits. Each await hands
// its operand to the promise machinery and registers a continuation that
// resumes the generator, so the whole function is driven by the job queue.
func (r *Runtime) runAsync(g *generator) Value {
	result := r.newPromise()
	g.promise = result
	r.stepAsync(g, Undefined, resumeNext)
	return Obj(result)
}

// stepAsync advances an async function by one resumption.
func (r *Runtime) stepAsync(g *generator, sent Value, mode resumeMode) {
	v, done, err := r.resume(g, sent, mode)
	if err != nil {
		// An exception that escapes the body rejects the function's promise
		// rather than propagating to the caller, which has long since returned.
		r.rejectPromise(g.promise, thrownValue(err))
		return
	}
	if done {
		r.resolvePromise(g.promise, v)
		return
	}

	// The generator suspended on an await. Resume it when the awaited value
	// settles.
	awaited := r.toPromise(v)
	onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		rt.stepAsync(g, arg(a, 0), resumeNext)
		return Undefined, nil
	})
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		// A rejected await throws at the await expression, so a try inside the
		// body can catch it.
		rt.stepAsync(g, arg(a, 0), resumeThrow)
		return Undefined, nil
	})
	r.promiseThen(awaited, Obj(onFulfilled), Obj(onRejected))
}
