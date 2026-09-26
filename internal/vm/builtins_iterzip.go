package vm

// Iterator.zip and Iterator.zipKeyed.
//
// Both draw one value from each of several iterators per step and hand them
// out together, as an array for zip and as a null-prototype object for
// zipKeyed. They differ only in how the iterators are gathered and in what a
// step's results become; the stepping itself is shared.
//
// The mode says what happens when one iterator runs out before the others:
// "shortest" stops there, "longest" goes on with a padding value in its place
// until all are done, and "strict" requires that all of them run out together
// and throws if they do not.

// zipMode is Iterator.zip's options.mode.
type zipMode uint8

const (
	zipShortest zipMode = iota
	zipLongest
	zipStrict
)

// zipIter is one of the iterators being zipped. open is false once it has
// run out or been closed, which is what the specification's list of open
// iterators records.
type zipIter struct {
	iter, next Value
	open       bool
}

// zipState is a zip helper's state.
type zipState struct {
	iters   []zipIter
	mode    zipMode
	padding []Value
	// keys are zipKeyed's property keys, one per iterator; zip has none.
	keys  []Atom
	keyed bool
}

func (r *Runtime) initIteratorZip(ctor *Object) {
	r.defMethod(ctor, "zip", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		iterables := arg(args, 0)
		if !iterables.IsObject() {
			return Undefined, rt.throwTypeError("Iterator.zip requires an iterable object")
		}
		mode, paddingOption, err := rt.zipOptions(arg(args, 1), "Iterator.zip")
		if err != nil {
			return Undefined, err
		}
		z := &zipState{mode: mode}
		if err := rt.zipGather(z, iterables); err != nil {
			return Undefined, err
		}
		if err := rt.zipPadding(z, paddingOption); err != nil {
			return Undefined, err
		}
		return rt.newZipHelper(z), nil
	})

	r.defMethod(ctor, "zipKeyed", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		iterables := arg(args, 0)
		if !iterables.IsObject() {
			return Undefined, rt.throwTypeError("Iterator.zipKeyed requires an object")
		}
		mode, paddingOption, err := rt.zipOptions(arg(args, 1), "Iterator.zipKeyed")
		if err != nil {
			return Undefined, err
		}
		z := &zipState{mode: mode, keyed: true}
		if err := rt.zipGatherKeyed(z, iterables.Object()); err != nil {
			return Undefined, err
		}
		if mode == zipLongest {
			// The padding for a keyed zip is read by key, from an object
			// rather than an iterator.
			z.padding = undefinedList(len(z.iters))
			if paddingOption.IsObject() {
				for i, key := range z.keys {
					v, err := rt.getProp(paddingOption.Object(), key, paddingOption)
					if err != nil {
						return Undefined, rt.closeZip(z, err)
					}
					z.padding[i] = v
				}
			}
		}
		return rt.newZipHelper(z), nil
	})
}

// zipOptions reads the options both functions take: mode, and padding when the
// mode is "longest". Neither is converted, and options itself is either
// undefined or an object.
func (r *Runtime) zipOptions(options Value, name string) (zipMode, Value, error) {
	if options.IsUndefined() {
		return zipShortest, Undefined, nil
	}
	if !options.IsObject() {
		return 0, Undefined, r.throwTypeError("%s requires options to be an object", name)
	}
	o := options.Object()
	m, err := r.getProp(o, r.atoms.intern("mode"), options)
	if err != nil {
		return 0, Undefined, err
	}
	mode := zipShortest
	switch {
	case m.IsUndefined():
	case m.IsString() && m.String().Go() == "shortest":
	case m.IsString() && m.String().Go() == "longest":
		mode = zipLongest
	case m.IsString() && m.String().Go() == "strict":
		mode = zipStrict
	default:
		return 0, Undefined, r.throwTypeError(`%s requires mode to be "shortest", "longest" or "strict"`, name)
	}
	if mode != zipLongest {
		return mode, Undefined, nil
	}
	padding, err := r.getProp(o, r.atoms.intern("padding"), options)
	if err != nil {
		return 0, Undefined, err
	}
	if !padding.IsUndefined() && !padding.IsObject() {
		return 0, Undefined, r.throwTypeError("%s requires padding to be an object", name)
	}
	return mode, padding, nil
}

// zipOpen gets an iterator from one of the values being zipped. It must be an
// object -- a string is not taken to mean its characters -- which is either
// iterable or an iterator already.
func (r *Runtime) zipOpen(v Value) (zipIter, error) {
	if !v.IsObject() {
		return zipIter{}, r.throwTypeError("%s is not an iterable object", r.describe(v))
	}
	iter, next, err := r.flattenable(v)
	if err != nil {
		return zipIter{}, err
	}
	return zipIter{iter: iter, next: next, open: true}, nil
}

// zipGather opens each value the iterables argument of zip produces.
//
// An error closes what has been opened so far, most recent first. It closes
// the outer iterator too when the error was about one of its values, but not
// when the outer iterator is what failed.
func (r *Runtime) zipGather(z *zipState, iterables Value) error {
	method, err := r.getValueProp(iterables, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return err
	}
	if !isCallable(method) {
		return r.throwTypeError("%s is not iterable", r.describe(iterables))
	}
	it, err := r.call(method, iterables, nil)
	if err != nil {
		return err
	}
	if !it.IsObject() {
		return r.throwTypeError("Symbol.iterator must return an object")
	}
	outer, outerNext, err := r.getIteratorDirect(it)
	if err != nil {
		return err
	}
	for {
		v, ok, err := r.stepIterator(outer, outerNext)
		if err != nil {
			return r.closeZip(z, err)
		}
		if !ok {
			return nil
		}
		zi, err := r.zipOpen(v)
		if err != nil {
			err = r.closeZip(z, err)
			r.closeIterator(outer)
			return err
		}
		z.iters = append(z.iters, zi)
	}
}

// zipGatherKeyed opens the value of each own enumerable property of zipKeyed's
// argument, skipping those that are undefined.
func (r *Runtime) zipGatherKeyed(z *zipState, o *Object) error {
	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return err
	}
	for _, key := range keys {
		enumerable, err := r.isEnumerable(o, key)
		if err != nil {
			return r.closeZip(z, err)
		}
		if !enumerable {
			continue
		}
		v, err := r.getProp(o, key, Obj(o))
		if err != nil {
			return r.closeZip(z, err)
		}
		if v.IsUndefined() {
			continue
		}
		zi, err := r.zipOpen(v)
		if err != nil {
			return r.closeZip(z, err)
		}
		z.keys = append(z.keys, key)
		z.iters = append(z.iters, zi)
	}
	return nil
}

// zipPadding reads zip's padding, one value per iterator, from an iterable.
// Padding shorter than the iterators leaves the rest undefined, and padding
// longer than them is closed rather than drained.
func (r *Runtime) zipPadding(z *zipState, padding Value) error {
	if z.mode != zipLongest {
		return nil
	}
	z.padding = undefinedList(len(z.iters))
	if padding.IsUndefined() {
		return nil
	}
	method, err := r.getValueProp(padding, r.atoms.internSymbol(r.wellKnown.iterator))
	if err == nil && !isCallable(method) {
		err = r.throwTypeError("%s is not iterable", r.describe(padding))
	}
	var it Value
	if err == nil {
		it, err = r.call(method, padding, nil)
	}
	if err == nil && !it.IsObject() {
		err = r.throwTypeError("Symbol.iterator must return an object")
	}
	var iter, next Value
	if err == nil {
		iter, next, err = r.getIteratorDirect(it)
	}
	if err != nil {
		return r.closeZip(z, err)
	}
	for i := range z.padding {
		v, ok, err := r.stepIterator(iter, next)
		if err != nil {
			return r.closeZip(z, err)
		}
		if !ok {
			return nil
		}
		z.padding[i] = v
	}
	if err := r.closeIteratorErr(iter); err != nil {
		return r.closeZip(z, err)
	}
	return nil
}

// undefinedList is n undefineds: the zero Value is the number zero, not
// undefined.
func undefinedList(n int) []Value {
	l := make([]Value, n)
	for i := range l {
		l[i] = Undefined
	}
	return l
}

func (r *Runtime) newZipHelper(z *zipState) Value {
	o := newObject(r.helperProto, ClassIteratorHelper)
	o.data = &iterHelperData{kind: helperZip, zip: z}
	return Obj(o)
}

// closeZip closes every iterator still open, most recent first, and returns
// the error that caused it if there was one. Without one, the first failure to
// close is the result; either way every iterator is still asked to close.
func (r *Runtime) closeZip(z *zipState, err error) error {
	for i := len(z.iters) - 1; i >= 0; i-- {
		it := &z.iters[i]
		if !it.open {
			continue
		}
		it.open = false
		if err != nil {
			r.closeIterator(it.iter)
		} else {
			err = r.closeIteratorErr(it.iter)
		}
	}
	return err
}

// advanceZip produces a zip helper's next set of results.
func (r *Runtime) advanceZip(h *iterHelperData) (Value, bool, error) {
	z := h.zip
	if len(z.iters) == 0 {
		h.done = true
		return Undefined, false, nil
	}
	results := make([]Value, len(z.iters))
	for i := range z.iters {
		it := &z.iters[i]
		if !it.open {
			// Only the longest mode goes on past an iterator that has run out.
			results[i] = z.padding[i]
			continue
		}
		v, ok, err := r.stepIterator(it.iter, it.next)
		if err != nil {
			it.open = false
			h.done = true
			return Undefined, false, r.closeZip(z, err)
		}
		if ok {
			results[i] = v
			continue
		}
		it.open = false
		switch z.mode {
		case zipShortest:
			h.done = true
			return Undefined, false, r.closeZip(z, nil)
		case zipStrict:
			h.done = true
			if i != 0 {
				return Undefined, false, r.closeZip(z,
					r.throwTypeError("Iterator.zip requires all iterators to have the same length"))
			}
			// The first ran out, and every other must now run out too. Only
			// whether each is done is read, not its value.
			for k := 1; k < len(z.iters); k++ {
				other := &z.iters[k]
				done, err := r.stepDone(other.iter, other.next)
				if err != nil {
					other.open = false
					return Undefined, false, r.closeZip(z, err)
				}
				if !done {
					return Undefined, false, r.closeZip(z,
						r.throwTypeError("Iterator.zip requires all iterators to have the same length"))
				}
				other.open = false
			}
			return Undefined, false, nil
		default:
			if !z.anyOpen() {
				h.done = true
				return Undefined, false, nil
			}
			results[i] = z.padding[i]
		}
	}
	return r.zipResults(z, results)
}

func (z *zipState) anyOpen() bool {
	for _, it := range z.iters {
		if it.open {
			return true
		}
	}
	return false
}

// zipResults makes one step's results into what the helper yields.
func (r *Runtime) zipResults(z *zipState, results []Value) (Value, bool, error) {
	if !z.keyed {
		return Obj(r.newArrayFrom(results)), true, nil
	}
	o := newObject(nil, ClassObject)
	for i, key := range z.keys {
		if err := r.createDataProperty(o, key, results[i], propDefault); err != nil {
			return Undefined, false, err
		}
	}
	return Obj(o), true, nil
}

// stepDone is IteratorStep for a caller that only wants to know whether the
// iterator is done, and so never reads the result's value.
func (r *Runtime) stepDone(iter, next Value) (bool, error) {
	res, err := r.call(next, iter, nil)
	if err != nil {
		return false, err
	}
	if !res.IsObject() {
		return false, r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return false, err
	}
	return done.Truthy(), nil
}
