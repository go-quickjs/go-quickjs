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
	// A finalization callback is a job of its own, and one queued by the
	// collector between turns has no other moment to run.
	r.runCleanups()
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
		if len(r.microtasks) == 0 {
			// Between jobs is where a finalization callback belongs: it is a
			// job of its own, and it may queue more.
			r.runCleanups()
		}
	}
	r.endTurn()
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
		if err := rt.requireNew("Promise"); err != nil {
			return Undefined, err
		}
		executor := arg(args, 0)
		if !isCallable(executor) {
			return Undefined, rt.throwTypeError("the Promise executor must be a function")
		}
		// The prototype is read here, before the executor runs: a getter that
		// throws stops the construction rather than the executor.
		proto, err := rt.protoFromNewTargetErr(rt.proto.promise)
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassPromise)
		o.data = &promiseData{}
		// The pair the executor is handed are anonymous, which a script can
		// check.
		resolveFn := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			rt.resolvePromise(o, arg(args, 0))
			return Undefined, nil
		})
		rejectFn := rt.newNativeFunc("", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
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
		// Generic: it is `this.then(undefined, onRejected)` and nothing else,
		// so anything with a then will do.
		return rt.invokeThen(this, Undefined, arg(args, 0))
	})
	r.defMethod(p, "finally", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError(
				"Promise.prototype.finally requires an object")
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			// Not callable: the same value is handed to then for both halves,
			// which then will ignore.
			return rt.invokeThen(this, cb, cb)
		}
		// The callback's result is awaited before the outcome is passed on, so
		// `finally(() => sleep())` delays what follows it. Which constructor
		// that promise comes from is the receiver's species.
		ctor, err := rt.speciesConstructor(this.Object(), rt.promiseCtor)
		if err != nil {
			return Undefined, err
		}
		after := func(carry func(Value) (Value, error)) *Object {
			return rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
				arg0 := arg(a, 0)
				res, err := rt.call(cb, Undefined, nil)
				if err != nil {
					return Undefined, err
				}
				p, err := rt.promiseResolveWith(ctor, res)
				if err != nil {
					return Undefined, err
				}
				thunk := rt.newNativeFunc("", 0,
					func(rt *Runtime, _ Value, _ []Value) (Value, error) {
						return carry(arg0)
					})
				// One argument, which is what a then that counts them sees.
				return rt.invokeThen(p, Obj(thunk))
			})
		}
		onFulfilled := after(func(v Value) (Value, error) { return v, nil })
		onRejected := after(func(v Value) (Value, error) { return Undefined, rt.throw(v) })
		return rt.invokeThen(this, Obj(onFulfilled), Obj(onRejected))
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

	// The constructor's own resolve is looked up once, before anything is
	// iterated, and used for every element. That is what lets a subclass see
	// each value go past -- and looking it up once means a resolve that
	// replaces itself mid-iteration does not take effect.
	resolveFn, err := r.getProp(ctor.Object(), r.atoms.intern("resolve"), ctor)
	if err != nil {
		fail(thrownValue(err))
		return Obj(result), nil
	}
	if !isCallable(resolveFn) {
		fail(thrownValue(r.throwTypeError("the promise constructor has no resolve method")))
		return Obj(result), nil
	}

	// The elements are resolved as they arrive rather than collected first,
	// because the resolve is allowed to throw and that has to stop the
	// iteration: an iterator that never finishes on its own would otherwise be
	// drained forever. The count starts at one for the iteration itself, so
	// that a run of already-settled promises cannot conclude the whole thing
	// before the last element has been seen.
	var values []Value
	remaining := 1
	finish := func() {
		remaining--
		if remaining != 0 {
			return
		}
		if kind == combinatorAny {
			fail(Obj(r.aggregateRejections(values)))
			return
		}
		settle(Obj(r.newArrayFrom(values)))
	}

	iterErr := r.iterate(iterable, func(item Value) error {
		idx := len(values)
		values = append(values, Undefined)
		remaining++
		// Each element's own settle function may be called once. A thenable
		// that calls it twice -- or calls both halves -- must not make the
		// combinator count the element twice.
		called := false

		// Every element goes through the constructor's resolve, so a plain
		// value works too and a subclass gets to see it.
		pv, err := r.call(resolveFn, ctor, []Value{item})
		if err != nil {
			return err
		}
		thenFn, err := r.getValueProp(pv, r.atoms.intern("then"))
		if err != nil {
			return err
		}
		if !isCallable(thenFn) {
			return r.throwTypeError("the resolved value is not thenable")
		}

		onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			if called {
				return Undefined, nil
			}
			called = true
			v := arg(a, 0)
			switch kind {
			case combinatorAllSettled:
				o := newObject(rt.proto.object, ClassObject)
				o.setOwnRaw(rt.atoms.intern("status"), Str(NewString("fulfilled")), propDefault)
				o.setOwnRaw(atomValue, v, propDefault)
				values[idx] = Obj(o)
			default:
				values[idx] = v
			}
			finish()
			return Undefined, nil
		})

		onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
			if called {
				return Undefined, nil
			}
			called = true
			reason := arg(a, 0)
			switch kind {
			case combinatorAllSettled:
				o := newObject(rt.proto.object, ClassObject)
				o.setOwnRaw(rt.atoms.intern("status"), Str(NewString("rejected")), propDefault)
				o.setOwnRaw(rt.atoms.intern("reason"), reason, propDefault)
				values[idx] = Obj(o)
			default: // any
				values[idx] = reason
			}
			finish()
			return Undefined, nil
		})

		// Where the whole thing settles on one element, the capability's own
		// function is what each element is given -- the same object every
		// time, which a script can check.
		fulfil, reject := Obj(onFulfilled), Obj(onRejected)
		switch kind {
		case combinatorRace:
			fulfil, reject = cap.resolve, cap.reject
		case combinatorAll:
			reject = cap.reject
		case combinatorAny:
			fulfil = cap.resolve
		}
		_, err = r.call(thenFn, pv, []Value{fulfil, reject})
		return err
	})
	if iterErr != nil {
		fail(thrownValue(iterErr))
		return Obj(result), nil
	}

	if len(values) == 0 {
		switch kind {
		case combinatorAll, combinatorAllSettled:
			settle(Obj(r.newArrayFrom(nil)))
		case combinatorAny:
			fail(Obj(r.aggregateRejections(nil)))
		}
		// Promise.race over nothing stays pending forever, which is what the
		// specification says.
		return Obj(result), nil
	}
	finish()
	return Obj(result), nil
}

// aggregateRejections builds the error Promise.any rejects with, which carries
// every reason it collected in the order the promises were given.
func (r *Runtime) aggregateRejections(reasons []Value) *Object {
	e := r.newError(errAggregate, "all promises were rejected")
	e.setOwnRaw(r.atoms.intern("errors"), Obj(r.newArrayFrom(reasons)),
		propWritable|propConfigurable)
	return e
}

// promiseResolveWith is PromiseResolve: a promise already built by the given
// constructor is handed back unchanged, and anything else is wrapped in one the
// constructor makes.
func (r *Runtime) promiseResolveWith(ctor Value, v Value) (Value, error) {
	if !ctor.IsObject() {
		return Undefined, r.throwTypeError("a promise constructor is required")
	}
	if v.IsObject() && v.Object().class == ClassPromise {
		c, err := r.getProp(v.Object(), atomConstructor, v)
		if err != nil {
			return Undefined, err
		}
		if c.SameValue(ctor) {
			return v, nil
		}
	}
	cap, err := r.newPromiseCapability(ctor)
	if err != nil {
		return Undefined, err
	}
	if _, err := r.call(cap.resolve, Undefined, []Value{v}); err != nil {
		return Undefined, err
	}
	return Obj(cap.promise), nil
}

// invokeThen calls a value's own then method, which is how the generic halves of
// the prototype are defined: catch and finally add handlers to whatever they
// were called on rather than to a promise they know about.
func (r *Runtime) invokeThen(this Value, args ...Value) (Value, error) {
	// A primitive receiver is not refused here: the method is read through the
	// value, which works on anything that is not nullish.
	then, err := r.getValueProp(this, r.atoms.intern("then"))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(then) {
		return Undefined, r.throwTypeError("then is not a function")
	}
	return r.call(then, this, args)
}

// toPromise coerces a value to a promise, wrapping a plain value and adopting a
// thenable.
func (r *Runtime) toPromise(v Value) *Object {
	p, err := r.toPromiseErr(v)
	if err != nil {
		// Reaching here means a promise's own constructor property threw,
		// which the caller had no way to report. Treating it as a rejection
		// keeps the value on the promise track rather than losing it.
		p = r.newPromise()
		r.rejectPromise(p, thrownValue(err))
	}
	return p
}

// toPromiseErr is toPromise with the error the lookup may produce.
//
// A promise is handed back as itself only when its constructor is the
// intrinsic, because a subclass's instance has to be adopted rather than
// reused -- and reading that property runs whatever getter is there.
func (r *Runtime) toPromiseErr(v Value) (*Object, error) {
	if v.IsObject() && v.Object().class == ClassPromise {
		ctor, err := r.getProp(v.Object(), atomConstructor, v)
		if err != nil {
			return nil, err
		}
		if ctor.IsObject() && ctor.Object() == r.promiseCtor {
			return v.Object(), nil
		}
	}
	o := r.newPromise()
	r.resolvePromise(o, v)
	return o, nil
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
