package vm

import "math"

// Iterator helpers.
//
// map, filter, take, drop and flatMap are lazy: each returns a new iterator
// that pulls one value from the one beneath it when asked, so a pipeline over
// an infinite sequence terminates as long as something downstream stops asking.
// That is the whole point of them -- the array equivalents would have to
// materialize every intermediate stage.
//
// reduce, toArray, forEach, some, every and find are the opposite: they drain
// the iterator and return a value, which is where a pipeline ends.
//
// A helper owns the iterator beneath it. If user code throws anywhere in the
// chain, the underlying iterator is closed before the error propagates, so a
// generator upstream still runs its finally blocks.

// helperKind selects a lazy helper's behaviour.
type helperKind uint8

const (
	helperMap helperKind = iota
	helperFilter
	helperTake
	helperDrop
	helperFlatMap
)

// iterHelperData is a lazy helper's state.
type iterHelperData struct {
	kind helperKind
	// iter and next are the iterator record this helper draws from.
	iter Value
	next Value
	// fn is the mapper or predicate, unused by take and drop.
	fn Value
	// limit is take's and drop's count, which may be infinite.
	limit float64
	// counter is the zero-based index passed to the callback.
	counter float64
	// dropped records that drop has already skipped its prefix.
	dropped bool

	// inner is flatMap's current sub-iterator.
	inner     Value
	innerNext Value
	hasInner  bool

	done bool
	// running guards against a helper's next being re-entered from the
	// callback it is in the middle of calling, which would corrupt counter and
	// the inner-iterator state.
	running bool
}

// wrapData is Iterator.from's wrapper around a foreign iterator.
type wrapData struct {
	iter Value
	next Value
}

// getIteratorDirect builds an iterator record from an object that already is
// an iterator, without consulting Symbol.iterator.
func (r *Runtime) getIteratorDirect(v Value) (Value, Value, error) {
	next, err := r.getValueProp(v, atomNext)
	if err != nil {
		return Undefined, Undefined, err
	}
	return v, next, nil
}

// stepIterator pulls one value, reporting exhaustion separately from an error.
func (r *Runtime) stepIterator(iter, next Value) (Value, bool, error) {
	res, err := r.call(next, iter, nil)
	if err != nil {
		return Undefined, false, err
	}
	if !res.IsObject() {
		return Undefined, false, r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return Undefined, false, err
	}
	if done.Truthy() {
		return Undefined, false, nil
	}
	v, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, false, err
	}
	return v, true, nil
}

func (r *Runtime) initIteratorHelpers() {
	p := r.proto.iterator

	// The Iterator constructor is abstract: it exists to be subclassed and to
	// hold the helpers, never to be instantiated on its own.
	ctor := r.newCtor("Iterator", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("Iterator requires new")
		}
		if nt := rt.newTarget(); nt.IsObject() && nt.Object() == rt.iteratorCtor {
			return Undefined, rt.throwTypeError("Iterator is abstract and cannot be constructed directly")
		}
		return Undefined, nil
	})
	r.iteratorCtor = ctor

	r.helperProto = newObject(p, ClassObject)
	r.initHelperPrototype()
	r.wrapProto = newObject(p, ClassObject)
	r.initWrapPrototype()

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.iteratorFrom(arg(args, 0))
	})

	lazy := func(name string, length int, kind helperKind) {
		r.defMethod(p, name, length, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !this.IsObject() {
				return Undefined, rt.throwTypeError("Iterator.prototype.%s requires an object", name)
			}
			h := &iterHelperData{kind: kind, limit: math.Inf(1)}

			// The argument is validated before the iterator is touched, so a
			// bad argument does not consume anything.
			switch kind {
			case helperTake, helperDrop:
				n, err := rt.toNumber(arg(args, 0))
				if err != nil {
					// The conversion may run a valueOf that throws, and the
					// underlying iterator is abandoned either way.
					rt.closeIterator(this)
					return Undefined, err
				}
				// NaN would otherwise compare false against every bound and
				// silently behave as zero, and a count past the integer range
				// cannot be counted down to.
				if math.IsNaN(n) || (!math.IsInf(n, 0) && n > maxArrayLength) {
					rt.closeIterator(this)
					return Undefined, rt.throwRangeError("%s requires a count in range", name)
				}
				n = math.Trunc(n)
				if n < 0 {
					rt.closeIterator(this)
					return Undefined, rt.throwRangeError("%s requires a non-negative count", name)
				}
				h.limit = n
			default:
				fn := arg(args, 0)
				if !isCallable(fn) {
					rt.closeIterator(this)
					return Undefined, rt.throwTypeError("Iterator.prototype.%s requires a function", name)
				}
				h.fn = fn
			}

			iter, next, err := rt.getIteratorDirect(this)
			if err != nil {
				return Undefined, err
			}
			h.iter, h.next = iter, next

			o := newObject(rt.helperProto, ClassIteratorHelper)
			o.data = h
			return Obj(o), nil
		})
	}
	lazy("map", 1, helperMap)
	lazy("filter", 1, helperFilter)
	lazy("take", 1, helperTake)
	lazy("drop", 1, helperDrop)
	lazy("flatMap", 1, helperFlatMap)

	r.initIteratorTerminals(p)

	// toStringTag and constructor are accessors rather than data properties, so
	// that assigning to them through an instance defines an own property
	// instead of failing silently or writing through to the shared prototype.
	r.defProtoAccessor(p, r.atoms.intern("constructor"), Obj(ctor))
	r.defProtoAccessor(p, r.atoms.internSymbol(r.wellKnown.toStringTag), Str(NewString("Iterator")))

	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) { return this, nil })
}

// defProtoAccessor defines one of Iterator.prototype's self-defending
// accessors.
//
// The getter returns a fixed value. The setter refuses when the receiver is the
// prototype itself -- otherwise one script could change what every iterator in
// the realm reports -- and otherwise creates an own property on the receiver,
// which is what an ordinary assignment through an instance should do.
func (r *Runtime) defProtoAccessor(proto *Object, key Atom, val Value) {
	get := r.newNativeFunc("get", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return val, nil
	})
	set := r.newNativeFunc("set", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("cannot set a property of a non-object")
		}
		if this.Object() == proto {
			return Undefined, rt.throwTypeError("cannot assign to a property of Iterator.prototype")
		}
		this.Object().setOwnRaw(key, arg(args, 0), propDefault)
		return Undefined, nil
	})
	r.defineAccessor(proto, key, get, set, propConfigurable)
}

// initHelperPrototype defines next and return on %IteratorHelperPrototype%.
func (r *Runtime) initHelperPrototype() {
	p := r.helperProto

	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		h, err := rt.helperOf(this, "Iterator Helper.prototype.next")
		if err != nil {
			return Undefined, err
		}
		v, ok, err := rt.advanceHelper(h)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.iterResult(v, !ok)), nil
	})

	r.defMethod(p, "return", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		h, err := rt.helperOf(this, "Iterator Helper.prototype.return")
		if err != nil {
			return Undefined, err
		}
		if !h.done {
			h.done = true
			// Abandoning a helper abandons everything beneath it, so a
			// generator upstream gets to run its finally blocks. An error from
			// doing so reaches the caller, because nothing else is in flight
			// to take precedence over it.
			if h.hasInner {
				if err := rt.closeIteratorErr(h.inner); err != nil {
					return Undefined, err
				}
			}
			if err := rt.closeIteratorErr(h.iter); err != nil {
				return Undefined, err
			}
		}
		return Obj(rt.iterResult(Undefined, true)), nil
	})

	r.defProtoAccessor(p, r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString("Iterator Helper")))
}

func (r *Runtime) helperOf(this Value, name string) (*iterHelperData, error) {
	if !this.IsObject() || this.Object().class != ClassIteratorHelper {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	h, ok := this.Object().data.(*iterHelperData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized iterator helper", name)
	}
	if h.running {
		return nil, r.throwTypeError("%s called while the helper is already running", name)
	}
	return h, nil
}

// advanceHelper produces the helper's next value, or reports exhaustion.
func (r *Runtime) advanceHelper(h *iterHelperData) (Value, bool, error) {
	if h.done {
		return Undefined, false, nil
	}
	h.running = true
	defer func() { h.running = false }()

	switch h.kind {
	case helperMap:
		v, ok, err := r.pull(h)
		if !ok || err != nil {
			return Undefined, false, err
		}
		i := h.counter
		h.counter++
		out, err := r.call(h.fn, Undefined, []Value{v, Float(i)})
		if err != nil {
			return Undefined, false, r.abandon(h, err)
		}
		return out, true, nil

	case helperFilter:
		for {
			v, ok, err := r.pull(h)
			if !ok || err != nil {
				return Undefined, false, err
			}
			i := h.counter
			h.counter++
			keep, err := r.call(h.fn, Undefined, []Value{v, Float(i)})
			if err != nil {
				return Undefined, false, r.abandon(h, err)
			}
			if keep.Truthy() {
				return v, true, nil
			}
		}

	case helperTake:
		if h.counter >= h.limit {
			// Reaching the limit closes the source rather than leaving it
			// suspended, which is what makes take safe over a generator. This
			// is an ordinary completion, so a failure to close is reported
			// rather than swallowed.
			h.done = true
			return Undefined, false, r.closeIteratorErr(h.iter)
		}
		h.counter++
		return r.pull(h)

	case helperDrop:
		if !h.dropped {
			h.dropped = true
			for i := float64(0); i < h.limit; i++ {
				if _, ok, err := r.pull(h); !ok || err != nil {
					return Undefined, false, err
				}
			}
		}
		return r.pull(h)

	case helperFlatMap:
		for {
			if h.hasInner {
				v, ok, err := r.stepIterator(h.inner, h.innerNext)
				if err != nil {
					return Undefined, false, r.abandon(h, err)
				}
				if ok {
					return v, true, nil
				}
				h.hasInner = false
			}
			v, ok, err := r.pull(h)
			if !ok || err != nil {
				return Undefined, false, err
			}
			i := h.counter
			h.counter++
			mapped, err := r.call(h.fn, Undefined, []Value{v, Float(i)})
			if err != nil {
				return Undefined, false, r.abandon(h, err)
			}
			// flatMap flattens one level, and only over iterables: a string is
			// deliberately not one here, because flattening it into characters
			// is almost never what the caller meant.
			inner, innerNext, err := r.flattenable(mapped)
			if err != nil {
				return Undefined, false, r.abandon(h, err)
			}
			h.inner, h.innerNext, h.hasInner = inner, innerNext, true
		}
	}
	return Undefined, false, nil
}

// pull takes one value from the underlying iterator, marking the helper done
// when it is exhausted.
func (r *Runtime) pull(h *iterHelperData) (Value, bool, error) {
	v, ok, err := r.stepIterator(h.iter, h.next)
	if err != nil {
		h.done = true
		return Undefined, false, err
	}
	if !ok {
		h.done = true
		return Undefined, false, nil
	}
	return v, true, nil
}

// abandon closes everything the helper owns and returns the error that caused
// it, so that a throw from user code still unwinds the sources.
func (r *Runtime) abandon(h *iterHelperData, err error) error {
	h.done = true
	if h.hasInner {
		r.closeIterator(h.inner)
		h.hasInner = false
	}
	r.closeIterator(h.iter)
	return err
}

// flattenable returns an iterator record for a value flatMap produced.
func (r *Runtime) flattenable(v Value) (Value, Value, error) {
	if v.IsString() {
		return Undefined, Undefined, r.throwTypeError("flatMap does not flatten strings")
	}
	if !v.IsObject() {
		return Undefined, Undefined, r.throwTypeError("flatMap requires an iterable")
	}
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return Undefined, Undefined, err
	}
	if method.IsNullish() {
		// An object that is already an iterator, with a next method but no
		// Symbol.iterator, is accepted directly. A Symbol.iterator that is
		// present but not callable is a mistake, not an absence.
		return r.getIteratorDirect(v)
	}
	if !isCallable(method) {
		return Undefined, Undefined, r.throwTypeError("Symbol.iterator is not a function")
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return Undefined, Undefined, err
	}
	if !iter.IsObject() {
		return Undefined, Undefined, r.throwTypeError("Symbol.iterator must return an object")
	}
	return r.getIteratorDirect(iter)
}

// initIteratorTerminals defines the helpers that drain an iterator.
func (r *Runtime) initIteratorTerminals(p *Object) {
	// each walks an iterator, handing every value to visit until it says stop
	// or the iterator is exhausted. An error or an early stop closes the
	// iterator, which is the part every one of these has to get right.
	each := func(rt *Runtime, this Value, visit func(v Value, i float64) (bool, error)) error {
		iter, next, err := rt.getIteratorDirect(this)
		if err != nil {
			return err
		}
		for i := float64(0); ; i++ {
			v, ok, err := rt.stepIterator(iter, next)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			cont, err := visit(v, i)
			if err != nil {
				// The visitor's error is the one in flight, so a failure to
				// close is swallowed behind it.
				rt.closeIterator(iter)
				return err
			}
			if !cont {
				// Stopping early is an ordinary completion, so a failure to
				// close is the result.
				return rt.closeIteratorErr(iter)
			}
		}
	}

	// requireIterator checks the receiver, and callback the argument, in that
	// order -- the specification makes the order observable.
	requireIterator := func(rt *Runtime, this Value, name string) error {
		if !this.IsObject() {
			return rt.throwTypeError("Iterator.prototype.%s requires an object", name)
		}
		return nil
	}
	requireCallback := func(rt *Runtime, this Value, fn Value, name string) error {
		if !isCallable(fn) {
			rt.closeIterator(this)
			return rt.throwTypeError("Iterator.prototype.%s requires a function", name)
		}
		return nil
	}

	r.defMethod(p, "toArray", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := requireIterator(rt, this, "toArray"); err != nil {
			return Undefined, err
		}
		var out []Value
		err := each(rt, this, func(v Value, _ float64) (bool, error) {
			out = append(out, v)
			return true, nil
		})
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(p, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := requireIterator(rt, this, "forEach"); err != nil {
			return Undefined, err
		}
		fn := arg(args, 0)
		if err := requireCallback(rt, this, fn, "forEach"); err != nil {
			return Undefined, err
		}
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			_, err := rt.call(fn, Undefined, []Value{v, Float(i)})
			return true, err
		})
		return Undefined, err
	})

	r.defMethod(p, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := requireIterator(rt, this, "reduce"); err != nil {
			return Undefined, err
		}
		fn := arg(args, 0)
		if err := requireCallback(rt, this, fn, "reduce"); err != nil {
			return Undefined, err
		}
		acc := arg(args, 1)
		// Without an initial value the first element becomes the accumulator,
		// and an empty iterator is then an error rather than undefined.
		seeded := len(args) > 1
		empty := true
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			empty = false
			if !seeded {
				seeded, acc = true, v
				return true, nil
			}
			out, err := rt.call(fn, Undefined, []Value{acc, v, Float(i)})
			if err != nil {
				return false, err
			}
			acc = out
			return true, nil
		})
		if err != nil {
			return Undefined, err
		}
		if empty && len(args) <= 1 {
			return Undefined, rt.throwTypeError("reduce of an empty iterator with no initial value")
		}
		return acc, nil
	})

	// some, every and find share a shape: run the predicate until it decides,
	// then close the iterator and answer.
	short := func(name string, stopOn bool, answer func(v Value, stopped bool) Value) {
		r.defMethod(p, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if err := requireIterator(rt, this, name); err != nil {
				return Undefined, err
			}
			fn := arg(args, 0)
			if err := requireCallback(rt, this, fn, name); err != nil {
				return Undefined, err
			}
			var found Value = Undefined
			stopped := false
			err := each(rt, this, func(v Value, i float64) (bool, error) {
				out, err := rt.call(fn, Undefined, []Value{v, Float(i)})
				if err != nil {
					return false, err
				}
				if out.Truthy() == stopOn {
					found, stopped = v, true
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				return Undefined, err
			}
			return answer(found, stopped), nil
		})
	}
	short("some", true, func(_ Value, stopped bool) Value { return Bool(stopped) })
	short("every", false, func(_ Value, stopped bool) Value { return Bool(!stopped) })
	short("find", true, func(v Value, stopped bool) Value {
		if stopped {
			return v
		}
		return Undefined
	})
}

// iteratorFrom implements Iterator.from.
//
// An object that already inherits from Iterator.prototype is returned as is,
// since wrapping it would add a layer for no benefit. Anything else is wrapped
// so that the helpers become available on it.
func (r *Runtime) iteratorFrom(v Value) (Value, error) {
	var iter, next Value

	switch {
	case v.IsString():
		method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
		if err != nil {
			return Undefined, err
		}
		it, err := r.call(method, v, nil)
		if err != nil {
			return Undefined, err
		}
		iter, next, err = r.getIteratorDirect(it)
		if err != nil {
			return Undefined, err
		}
	case v.IsObject():
		method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
		if err != nil {
			return Undefined, err
		}
		switch {
		case isCallable(method):
			it, err := r.call(method, v, nil)
			if err != nil {
				return Undefined, err
			}
			if !it.IsObject() {
				return Undefined, r.throwTypeError("Symbol.iterator must return an object")
			}
			iter, next, err = r.getIteratorDirect(it)
			if err != nil {
				return Undefined, err
			}
		case method.IsNullish():
			// No Symbol.iterator at all: the object is taken to be an iterator
			// already. One that is present but not callable is a mistake.
			iter, next, err = r.getIteratorDirect(v)
			if err != nil {
				return Undefined, err
			}
		default:
			return Undefined, r.throwTypeError("Symbol.iterator is not a function")
		}
	default:
		return Undefined, r.throwTypeError("Iterator.from requires an object or a string")
	}

	if r.inheritsFrom(iter, r.proto.iterator) {
		return iter, nil
	}
	o := newObject(r.wrapProto, ClassIteratorWrap)
	o.data = &wrapData{iter: iter, next: next}
	return Obj(o), nil
}

// inheritsFrom reports whether a value's prototype chain reaches an object.
func (r *Runtime) inheritsFrom(v Value, target *Object) bool {
	if !v.IsObject() {
		return false
	}
	for p := v.Object().proto; p != nil; p = p.proto {
		if p == target {
			return true
		}
	}
	return false
}

// initWrapPrototype defines %WrapForValidIteratorPrototype%, which forwards to
// the wrapped iterator.
func (r *Runtime) initWrapPrototype() {
	p := r.wrapProto

	wrapOf := func(rt *Runtime, this Value, name string) (*wrapData, error) {
		if !this.IsObject() || this.Object().class != ClassIteratorWrap {
			return nil, rt.throwTypeError("%s called on an incompatible receiver", name)
		}
		w, ok := this.Object().data.(*wrapData)
		if !ok {
			return nil, rt.throwTypeError("%s called on an uninitialized wrapper", name)
		}
		return w, nil
	}

	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		w, err := wrapOf(rt, this, "next")
		if err != nil {
			return Undefined, err
		}
		return rt.call(w.next, w.iter, nil)
	})

	r.defMethod(p, "return", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		w, err := wrapOf(rt, this, "return")
		if err != nil {
			return Undefined, err
		}
		ret, err := rt.getValueProp(w.iter, r.atoms.intern("return"))
		if err != nil {
			return Undefined, err
		}
		if !isCallable(ret) {
			// An iterator with no return method is simply abandoned.
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		return rt.call(ret, w.iter, nil)
	})
}
