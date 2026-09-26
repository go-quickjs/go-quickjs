package vm

import "math"

// Iterator helpers.
//
// map, filter, take, drop, flatMap, chunks and windows are lazy: each returns
// a new iterator that pulls from the one beneath it only when asked, so a
// pipeline over an infinite sequence terminates as long as something
// downstream stops asking. That is the whole point of them -- the array
// equivalents would have to materialize every intermediate stage.
//
// reduce, toArray, forEach, some, every, find, includes and join are the
// opposite: they drain the iterator and return a value, which is where a
// pipeline ends.
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
	helperChunks
	helperWindows
	helperConcat
	helperZip
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

	// size is chunks' and windows' length, buf the values gathered towards
	// the next array, and partial windows' allow-partial.
	size    int64
	buf     []Value
	partial bool

	// concat is Iterator.concat's list of iterables not yet opened. The one
	// being drawn from is inner.
	concat []concatItem
	// zip is Iterator.zip's and Iterator.zipKeyed's state.
	zip *zipState

	// started records that next has run, which decides whether return finds
	// the helper suspended before its first step or in the middle of one.
	started bool
	done    bool
	// running guards against a helper's next being re-entered from the
	// callback it is in the middle of calling, which would corrupt counter and
	// the inner-iterator state.
	running bool
	// argv is the list the callback's arguments go in, made once rather than
	// once per element. A callee may not keep it, any more than it may keep
	// the interpreter's own stack, and running says nothing else is using it.
	argv [2]Value
}

// concatItem is one of Iterator.concat's arguments with the iterator method it
// had when concat was called, which is the one called when its turn comes.
type concatItem struct {
	method, iterable Value
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

	r.initIteratorZip(ctor)

	// Iterator.concat checks every argument and reads its iterator method
	// up front, and opens each only when the one before it is exhausted.
	r.defMethod(ctor, "concat", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		items := make([]concatItem, 0, len(args))
		for _, item := range args {
			if !item.IsObject() {
				return Undefined, rt.throwTypeError("Iterator.concat requires iterable objects")
			}
			method, err := rt.getValueProp(item, rt.atoms.internSymbol(rt.wellKnown.iterator))
			if err != nil {
				return Undefined, err
			}
			if !isCallable(method) {
				return Undefined, rt.throwTypeError("%s is not iterable", rt.describe(item))
			}
			items = append(items, concatItem{method: method, iterable: item})
		}
		o := newObject(rt.helperProto, ClassIteratorHelper)
		o.data = &iterHelperData{kind: helperConcat, concat: items}
		return Obj(o), nil
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

	r.defMethod(p, "chunks", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.sizedHelper(this, arg(args, 0), "chunks", helperChunks,
			func() (bool, error) { return false, nil })
	})
	r.defMethod(p, "windows", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// undersized is checked after the size, and is one of the two strings
		// exactly: nothing is converted to one.
		return rt.sizedHelper(this, arg(args, 0), "windows", helperWindows, func() (bool, error) {
			u := arg(args, 1)
			switch {
			case u.IsUndefined():
				return false, nil
			case u.IsString() && u.String().Go() == "only-full":
				return false, nil
			case u.IsString() && u.String().Go() == "allow-partial":
				return true, nil
			}
			rt.closeIterator(this)
			return false, rt.throwTypeError(`Iterator.prototype.windows requires "only-full" or "allow-partial"`)
		})
	})

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
			h.buf = nil
			// A helper that has started is a generator suspended in a yield,
			// and closing what is beneath it resumes that generator: while it
			// does, the helper is running, and a next or return from inside a
			// return method is refused. One that has not started is simply
			// finished, and answers such a call as done.
			if h.started {
				h.running = true
				defer func() { h.running = false }()
			}
			if err := rt.closeHelper(h); err != nil {
				return Undefined, err
			}
		}
		return Obj(rt.iterResult(Undefined, true)), nil
	})

	r.defProtoAccessor(p, r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString("Iterator Helper")))
}

// closeHelper closes everything beneath a helper that is being returned from,
// so that a generator upstream gets to run its finally blocks. An error from
// doing so reaches the caller, because nothing else is in flight to take
// precedence over it.
func (r *Runtime) closeHelper(h *iterHelperData) error {
	if h.kind == helperZip {
		return r.closeZip(h.zip, nil)
	}
	if h.kind == helperConcat {
		// concat holds one iterable open at a time, and none before it starts
		// or between two of them.
		h.concat = nil
		if !h.hasInner {
			return nil
		}
		h.hasInner = false
		return r.closeIteratorErr(h.inner)
	}
	if h.hasInner {
		if err := r.closeIteratorErr(h.inner); err != nil {
			return err
		}
	}
	return r.closeIteratorErr(h.iter)
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
	h.started = true
	defer func() { h.running = false }()

	switch h.kind {
	case helperMap:
		v, ok, err := r.pull(h)
		if !ok || err != nil {
			return Undefined, false, err
		}
		i := h.counter
		h.counter++
		h.argv[0], h.argv[1] = v, Float(i)
		out, err := r.call(h.fn, Undefined, h.argv[:])
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
			h.argv[0], h.argv[1] = v, Float(i)
			keep, err := r.call(h.fn, Undefined, h.argv[:])
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
			h.argv[0], h.argv[1] = v, Float(i)
			mapped, err := r.call(h.fn, Undefined, h.argv[:])
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

	case helperZip:
		return r.advanceZip(h)

	case helperConcat:
		for {
			if !h.hasInner {
				if len(h.concat) == 0 {
					h.done = true
					return Undefined, false, nil
				}
				item := h.concat[0]
				h.concat = h.concat[1:]
				iter, err := r.call(item.method, item.iterable, nil)
				if err == nil && !iter.IsObject() {
					err = r.throwTypeError("Symbol.iterator must return an object")
				}
				var next Value
				if err == nil {
					iter, next, err = r.getIteratorDirect(iter)
				}
				if err != nil {
					h.done, h.concat = true, nil
					return Undefined, false, err
				}
				h.inner, h.innerNext, h.hasInner = iter, next, true
			}
			// The value is taken out of the result rather than the result
			// passed along, so what next returns is always a fresh object.
			v, ok, err := r.stepIterator(h.inner, h.innerNext)
			if err != nil {
				h.done, h.concat, h.hasInner = true, nil, false
				return Undefined, false, err
			}
			if ok {
				return v, true, nil
			}
			h.hasInner = false
		}

	case helperChunks:
		for {
			v, ok, err := r.pull(h)
			if err != nil {
				return Undefined, false, err
			}
			if !ok {
				// The last chunk may be short, and is still a chunk. The
				// source is exhausted by then, so the helper is done whatever
				// happens to it.
				if len(h.buf) == 0 {
					return Undefined, false, nil
				}
				out := h.buf
				h.buf = nil
				return Obj(r.newArrayFrom(out)), true, nil
			}
			h.buf = append(h.buf, v)
			if int64(len(h.buf)) == h.size {
				out := h.buf
				h.buf = nil
				return Obj(r.newArrayFrom(out)), true, nil
			}
		}

	case helperWindows:
		for {
			v, ok, err := r.pull(h)
			if err != nil {
				return Undefined, false, err
			}
			if !ok {
				// A source shorter than one window yields nothing unless
				// partial windows were asked for, and then yields itself.
				out := h.buf
				h.buf = nil
				if !h.partial || len(out) == 0 || int64(len(out)) == h.size {
					return Undefined, false, nil
				}
				return Obj(r.newArrayFrom(out)), true, nil
			}
			if int64(len(h.buf)) == h.size {
				h.buf = h.buf[1:]
			}
			h.buf = append(h.buf, v)
			if int64(len(h.buf)) == h.size {
				// The windows overlap, so each is a copy: the arrays handed
				// out are the caller's to keep and change.
				return Obj(r.newArrayFrom(append([]Value(nil), h.buf...))), true, nil
			}
		}
	}
	return Undefined, false, nil
}

// sizedHelper implements chunks and windows up to the point where they differ:
// the size is an integral number from 1 to 2^32-1, taken as it is rather than
// converted, and the iterator is closed for a bad one before its next method is
// read.
func (r *Runtime) sizedHelper(this, size Value, name string, kind helperKind,
	check func() (bool, error)) (Value, error) {
	if !this.IsObject() {
		return Undefined, r.throwTypeError("Iterator.prototype.%s requires an object", name)
	}
	n := size.Number()
	if !size.IsNumber() || math.IsInf(n, 0) || n != math.Trunc(n) {
		r.closeIterator(this)
		return Undefined, r.throwTypeError("Iterator.prototype.%s requires an integral size", name)
	}
	if n < 1 || n > math.MaxUint32 {
		r.closeIterator(this)
		return Undefined, r.throwRangeError("Iterator.prototype.%s requires a size from 1 to 2^32-1", name)
	}
	partial, err := check()
	if err != nil {
		return Undefined, err
	}
	iter, next, err := r.getIteratorDirect(this)
	if err != nil {
		return Undefined, err
	}
	o := newObject(r.helperProto, ClassIteratorHelper)
	o.data = &iterHelperData{kind: kind, iter: iter, next: next, size: int64(n), partial: partial}
	return Obj(o), nil
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
		// One list for the whole walk rather than one per element.
		var argv [2]Value
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			argv[0], argv[1] = v, Float(i)
			_, err := rt.call(fn, Undefined, argv[:])
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
		var argv [3]Value
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			empty = false
			if !seeded {
				seeded, acc = true, v
				return true, nil
			}
			argv[0], argv[1], argv[2] = acc, v, Float(i)
			out, err := rt.call(fn, Undefined, argv[:])
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
			var argv [2]Value
			err := each(rt, this, func(v Value, i float64) (bool, error) {
				argv[0], argv[1] = v, Float(i)
				out, err := rt.call(fn, Undefined, argv[:])
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
	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := requireIterator(rt, this, "includes"); err != nil {
			return Undefined, err
		}
		// The count of elements to skip is not converted: anything but an
		// integral number or an infinity is refused, and the iterator is
		// closed for it before its next method is so much as read.
		toSkip := 0.0
		if s := arg(args, 1); !s.IsUndefined() {
			n := s.Number()
			if !s.IsNumber() || (!math.IsInf(n, 0) && n != math.Trunc(n)) {
				rt.closeIterator(this)
				return Undefined, rt.throwTypeError("Iterator.prototype.includes requires an integral skip count")
			}
			if n < 0 || (!math.IsInf(n, 0) && n > maxSafeInteger) {
				rt.closeIterator(this)
				return Undefined, rt.throwRangeError("Iterator.prototype.includes requires a skip count in range")
			}
			toSkip = n
		}
		target := arg(args, 0)
		found := false
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			if i < toSkip {
				return true, nil
			}
			found = v.SameValueZero(target)
			return !found, nil
		})
		if err != nil {
			return Undefined, err
		}
		return Bool(found), nil
	})

	r.defMethod(p, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := requireIterator(rt, this, "join"); err != nil {
			return Undefined, err
		}
		// The separator is converted before the next method is read, and a
		// conversion that throws closes the iterator.
		sep := ","
		if s := arg(args, 0); !s.IsUndefined() {
			ss, err := rt.toString(s)
			if err != nil {
				rt.closeIterator(this)
				return Undefined, err
			}
			sep = ss.Go()
		}
		// As with Array.prototype.join, the pieces are joined rather than
		// appended, and null and undefined contribute nothing.
		var sb partsBuilder
		err := each(rt, this, func(v Value, i float64) (bool, error) {
			if i > 0 {
				sb.WriteString(sep)
			}
			if v.IsNullish() {
				return true, nil
			}
			s, err := rt.toString(v)
			if err != nil {
				return false, err
			}
			sb.WriteString(s.Go())
			return true, nil
		})
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(sb.String())), nil
	})

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
