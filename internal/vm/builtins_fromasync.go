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
	out    []Value

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

		src := arg(args, 0)
		if err := rt.openAsyncSource(st, src); err != nil {
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
func (r *Runtime) openAsyncSource(st *fromAsyncState, src Value) error {
	if src.IsNullish() {
		return r.throwTypeError("Array.fromAsync requires an iterable or array-like")
	}

	method, err := r.getValueProp(src, r.atoms.internSymbol(r.wellKnown.asyncIterator))
	if err != nil {
		return err
	}
	if !isCallable(method) {
		method, err = r.getValueProp(src, r.atoms.internSymbol(r.wellKnown.iterator))
		if err != nil {
			return err
		}
		st.sync = true
	}

	if !isCallable(method) {
		// Not iterable at all: read it by index, awaiting each element.
		st.sync = false
		a, err := r.viewArrayLike(src)
		if err != nil {
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
	return nil
}

// stepFromAsync advances the call by one element.
func (r *Runtime) stepFromAsync(st *fromAsyncState) {
	if st.a != nil {
		r.stepFromAsyncIndexed(st)
		return
	}

	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
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
			rt.rejectPromise(st.result, thrownValue(err))
			return
		}
		if done.Truthy() {
			rt.resolvePromise(st.result, Obj(rt.newArrayFrom(st.out)))
			return
		}
		value, err := rt.getValueProp(settled, atomValue)
		if err != nil {
			rt.rejectPromise(st.result, thrownValue(err))
			return
		}
		// A synchronous iterator's values are awaited individually, which is
		// what makes fromAsync over an array of promises produce the values.
		rt.acceptFromAsync(st, value)
	}, func(rt *Runtime, reason Value) {
		rt.rejectPromise(st.result, reason)
	})
}

// stepFromAsyncIndexed advances the array-like form.
func (r *Runtime) stepFromAsyncIndexed(st *fromAsyncState) {
	if st.index >= st.a.n {
		r.resolvePromise(st.result, Obj(r.newArrayFrom(st.out)))
		return
	}
	v, err := st.a.get(r, st.index)
	if err != nil {
		r.rejectPromise(st.result, thrownValue(err))
		return
	}
	st.index++
	r.acceptFromAsync(st, v)
}

// acceptFromAsync awaits one value, maps it, and asks for the next.
func (r *Runtime) acceptFromAsync(st *fromAsyncState, value Value) {
	r.awaitThen(value, func(rt *Runtime, settled Value) {
		i := st.count
		st.count++
		if isCallable(st.mapper) {
			mapped, err := rt.call(st.mapper, st.thisArg, []Value{settled, Float(float64(i))})
			if err != nil {
				rt.rejectPromise(st.result, thrownValue(err))
				return
			}
			// The mapper may itself be async, so its result is awaited too.
			rt.awaitThen(mapped, func(rt *Runtime, m Value) {
				st.out = append(st.out, m)
				rt.stepFromAsync(st)
			}, func(rt *Runtime, reason Value) {
				rt.rejectPromise(st.result, reason)
			})
			return
		}
		st.out = append(st.out, settled)
		rt.stepFromAsync(st)
	}, func(rt *Runtime, reason Value) {
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
