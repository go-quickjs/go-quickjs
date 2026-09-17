package vm

// Array.fromAsync.
//
// Array.from over an async iterable produces an array of promises rather than a
// promise of an array, which is almost never what the caller wanted. fromAsync
// awaits each value and resolves once, so `await Array.fromAsync(stream)` reads
// the way it looks.
//
// It is driven by chaining promises rather than by a loop, because each step
// has to wait for the previous one. The step function re-enters itself from the
// fulfilment handler, which is what a loop would be if the language had one
// that could suspend here.

// fromAsyncState is one Array.fromAsync call in progress.
type fromAsyncState struct {
	result *Object
	// target is the object being filled, which is what the constructor the
	// method was called on produced -- an ordinary array when it was called on
	// something that is not a constructor.
	target *Object

	// iter and next drive the iterator form.
	iter Value
	next Value
	// sync marks a source reached through Symbol.iterator rather than
	// Symbol.asyncIterator, whose values still have to be awaited one by one.
	sync bool

	// a and index drive the array-like form, which has no iterator at all.
	a     *arrayLike
	index int64

	// mapper and thisArg are applied to each value, after it is awaited.
	mapper  Value
	thisArg Value
	// count is the index passed to the mapper.
	count int64
}

func (r *Runtime) initArrayFromAsync(ctor *Object) {
	r.defMethod(ctor, "fromAsync", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		st := &fromAsyncState{
			result:  rt.newPromise(),
			mapper:  arg(args, 1),
			thisArg: arg(args, 2),
		}
		if !st.mapper.IsUndefined() && !isCallable(st.mapper) {
			rt.rejectPromise(st.result, thrownValue(
				rt.throwTypeError("Array.fromAsync requires a function as its second argument")))
			return Obj(st.result), nil
		}

		// Everything from here on is reported as a rejection rather than
		// thrown, which is what makes fromAsync always hand back a promise.
		if err := rt.openAsyncSource(st, this, arg(args, 0)); err != nil {
			rt.rejectPromise(st.result, thrownValue(err))
			return Obj(st.result), nil
		}
		rt.stepFromAsync(st)
		return Obj(st.result), nil
	})
}

// openAsyncSource decides how the source will be read.
//
// Symbol.asyncIterator is preferred, then Symbol.iterator, then the array-like
// protocol -- the same order Array.from uses, with the async step in front.
func (r *Runtime) openAsyncSource(st *fromAsyncState, ctor Value, src Value) error {
	if src.IsNullish() {
		return r.throwTypeError("Array.fromAsync requires an iterable or array-like")
	}

	method, err := r.iterMethod(src, r.wellKnown.asyncIterator)
	if err != nil {
		return err
	}
	if method.IsUndefined() {
		if method, err = r.iterMethod(src, r.wellKnown.iterator); err != nil {
			return err
		}
		st.sync = true
	}

	if method.IsUndefined() {
		// Not iterable at all: read it by index, awaiting each element.
		st.sync = false
		a, err := r.viewArrayLike(src)
		if err != nil {
			return err
		}
		// The length is known here, so the constructor is told it.
		if st.target, err = r.fromAsyncTarget(ctor, a.n); err != nil {
			return err
		}
		st.a = a
		return nil
	}

	iter, err := r.call(method, src, nil)
	if err != nil {
		return err
	}
	if !iter.IsObject() {
		return r.throwTypeError("an iterator method must return an object")
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return err
	}
	if !isCallable(next) {
		return r.throwTypeError("the iterator has no next method")
	}
	st.iter, st.next = iter, next
	if st.target, err = r.fromAsyncTarget(ctor, -1); err != nil {
		return err
	}
	return nil
}

// iterMethod looks up one of the iterator symbols, which is a GetMethod: a
// value that is neither absent nor callable is an error rather than a reason to
// try the next protocol.
func (r *Runtime) iterMethod(src Value, sym *Symbol) (Value, error) {
	m, err := r.getValueProp(src, r.atoms.internSymbol(sym))
	if err != nil {
		return Undefined, err
	}
	switch {
	case m.IsUndefined() || m.IsNull():
		return Undefined, nil
	case !isCallable(m):
		return Undefined, r.throwTypeError("%s is not a function", r.describe(m))
	}
	return m, nil
}

// fromAsyncTarget builds the object the values will be put into.
//
// Called on a constructor -- which it is on Array itself, and may be on a
// subclass -- the result is what that constructor makes, told the length when
// the source is an array-like and there is one to tell.
func (r *Runtime) fromAsyncTarget(ctor Value, n int64) (*Object, error) {
	if isConstructor(ctor) {
		var args []Value
		if n >= 0 {
			args = []Value{Float(float64(n))}
		}
		v, err := r.construct(ctor, args)
		if err != nil {
			return nil, err
		}
		if !v.IsObject() {
			return nil, r.throwTypeError("the constructor did not return an object")
		}
		return v.Object(), nil
	}
	if n < 0 {
		n = 0
	}
	if n > 1<<32-1 {
		return nil, r.throwRangeError("invalid array length")
	}
	return r.newArrayOfLength(n), nil
}

// stepFromAsync advances the call by one element.
func (r *Runtime) stepFromAsync(st *fromAsyncState) {
	if st.a != nil {
		r.stepFromAsyncIndexed(st)
		return
	}

	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
		// The iterator failed to produce a step, so there is nothing left to
		// close: it is already done as far as the protocol is concerned.
		r.rejectPromise(st.result, thrownValue(err))
		return
	}
	// A synchronous iterator hands back a plain result; an async one hands back
	// a promise for the whole result. Awaiting either settles to the result.
	r.awaitThen(res, func(rt *Runtime, settled Value) {
		if !settled.IsObject() {
			rt.rejectPromise(st.result, thrownValue(
				rt.throwTypeError("an iterator result must be an object")))
			return
		}
		done, err := rt.getValueProp(settled, atomDone)
		if err != nil {
			rt.failFromAsync(st, thrownValue(err))
			return
		}
		if done.Truthy() {
			rt.finishFromAsync(st)
			return
		}
		value, err := rt.getValueProp(settled, atomValue)
		if err != nil {
			rt.failFromAsync(st, thrownValue(err))
			return
		}
		// A synchronous iterator's values are awaited individually, which is
		// what makes fromAsync over an array of promises produce the values.
		// An asynchronous one has already awaited them, and awaiting again
		// would unwrap a promise the iterator meant to yield.
		if st.sync {
			rt.awaitFromAsync(st, value)
			return
		}
		rt.mapFromAsync(st, value)
	}, func(rt *Runtime, reason Value) {
		rt.rejectPromise(st.result, reason)
	})
}

// stepFromAsyncIndexed advances the array-like form.
func (r *Runtime) stepFromAsyncIndexed(st *fromAsyncState) {
	if st.index >= st.a.n {
		r.finishFromAsync(st)
		return
	}
	v, err := st.a.get(r, st.index)
	if err != nil {
		r.rejectPromise(st.result, thrownValue(err))
		return
	}
	st.index++
	r.awaitFromAsync(st, v)
}

// awaitFromAsync settles one value before it is mapped, which is what a
// synchronous source's values need and an asynchronous one's have already had.
func (r *Runtime) awaitFromAsync(st *fromAsyncState, value Value) {
	r.awaitThen(value, func(rt *Runtime, settled Value) {
		rt.mapFromAsync(st, settled)
	}, func(rt *Runtime, reason Value) {
		rt.failFromAsync(st, reason)
	})
}

// mapFromAsync applies the map function to one value, stores the result and
// asks for the next.
func (r *Runtime) mapFromAsync(st *fromAsyncState, value Value) {
	i := st.count
	st.count++
	if !isCallable(st.mapper) {
		r.storeFromAsync(st, i, value)
		return
	}
	mapped, err := r.call(st.mapper, st.thisArg, []Value{value, Float(float64(i))})
	if err != nil {
		r.failFromAsync(st, thrownValue(err))
		return
	}
	// The mapper may itself be async, so its result is awaited too.
	r.awaitThen(mapped, func(rt *Runtime, m Value) {
		rt.storeFromAsync(st, i, m)
	}, func(rt *Runtime, reason Value) {
		rt.failFromAsync(st, reason)
	})
}

// storeFromAsync puts one value in place and asks for the next.
func (r *Runtime) storeFromAsync(st *fromAsyncState, i int64, v Value) {
	if err := r.createIndexed(st.target, i, v); err != nil {
		r.failFromAsync(st, thrownValue(err))
		return
	}
	r.stepFromAsync(st)
}

// finishFromAsync records how many values arrived and resolves.
func (r *Runtime) finishFromAsync(st *fromAsyncState) {
	a := arrayLike{o: st.target}
	if err := a.setLength(r, st.count); err != nil {
		r.rejectPromise(st.result, thrownValue(err))
		return
	}
	r.resolvePromise(st.result, Obj(st.target))
}

// failFromAsync ends the call, closing the iterator first.
//
// Everything after the iterator is opened is inside the loop as far as the
// specification is concerned, so an abrupt completion there -- a map function
// that throws, a value that rejects, a property that refuses to be created --
// tells the iterator it is done before the promise rejects.
func (r *Runtime) failFromAsync(st *fromAsyncState, reason Value) {
	if !st.iter.IsObject() {
		r.rejectPromise(st.result, reason)
		return
	}
	iter := st.iter
	st.iter = Undefined
	ret, err := r.getValueProp(iter, atomReturn)
	if err != nil || !isCallable(ret) {
		// The original reason is the one that matters, so a failure to even
		// find the return method is discarded.
		r.rejectPromise(st.result, reason)
		return
	}
	res, err := r.call(ret, iter, nil)
	if err != nil {
		r.rejectPromise(st.result, reason)
		return
	}
	// An async iterator's return hands back a promise, which has to settle
	// before the rejection is delivered -- but whatever it settles to is
	// discarded.
	r.awaitThen(res, func(rt *Runtime, _ Value) {
		rt.rejectPromise(st.result, reason)
	}, func(rt *Runtime, _ Value) {
		rt.rejectPromise(st.result, reason)
	})
}

// awaitThen runs one of two Go callbacks once a value settles, which is what
// `await` would do if this were script.
func (r *Runtime) awaitThen(v Value, onOK func(*Runtime, Value), onErr func(*Runtime, Value)) {
	p := r.toPromise(v)
	fulfil := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		onOK(rt, arg(a, 0))
		return Undefined, nil
	})
	reject := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		onErr(rt, arg(a, 0))
		return Undefined, nil
	})
	r.promiseThen(p, Obj(fulfil), Obj(reject))
}
