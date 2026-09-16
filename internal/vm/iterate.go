package vm

// Loop iteration.
//
// for-in and for-of are driven by the same three instructions -- a start, a
// step and an implicit end -- but their semantics differ enough that the state
// object carries both shapes. for-of speaks the iterator protocol, calling a
// user-visible next method that may run arbitrary code; for-in enumerates
// property keys, and takes a snapshot up front because the specification
// permits an implementation to ignore properties added during the loop.

// iterState is the loop cursor that OpForInStart and OpForOfStart push.
type iterState struct {
	// forIn selects between the snapshot and the protocol.
	forIn bool

	// keys is the snapshot for-in walks.
	keys []Value
	idx  int

	// iter and next drive the for-of protocol.
	iter Value
	next Value

	done bool
}

// newIterObject wraps a cursor so that it can live on the operand stack.
func (r *Runtime) newIterObject(st *iterState) Value {
	o := newObject(nil, ClassIterator)
	o.data = st
	return Obj(o)
}

// iterStateOf recovers the cursor from a stack slot.
func iterStateOf(v Value) *iterState {
	if !v.IsObject() {
		return nil
	}
	st, _ := v.Object().data.(*iterState)
	return st
}

// startForIn snapshots the enumerable string keys of a value.
//
// The prototype chain is included, and a key shadowed by a nearer object is
// reported once, at the position where it is first found.
func (r *Runtime) startForIn(v Value) (Value, error) {
	st := &iterState{forIn: true}
	// null and undefined are not an error here: the loop simply runs zero
	// times, which is one of the few places JavaScript is forgiving.
	if v.IsNullish() {
		st.done = true
		return r.newIterObject(st), nil
	}
	o, err := r.toObject(v)
	if err != nil {
		return Undefined, err
	}

	seen := make(map[Atom]bool)
	for cur := o; cur != nil; cur = cur.proto {
		for _, k := range cur.ownKeys(false, r.atoms) {
			if seen[k] {
				continue
			}
			seen[k] = true
			// A non-enumerable property still shadows an enumerable one of the
			// same name further up the chain, which is why it is recorded in
			// seen before being skipped.
			if !r.isEnumerable(cur, k) {
				continue
			}
			st.keys = append(st.keys, Str(NewString(r.atoms.name(k))))
		}
	}
	return r.newIterObject(st), nil
}

// startForOf opens an iterator over a value.
func (r *Runtime) startForOf(v Value) (Value, error) {
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(method) {
		return Undefined, r.throwTypeError("%s is not iterable", r.describe(v))
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return Undefined, err
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return Undefined, err
	}
	if !isCallable(next) {
		return Undefined, r.throwTypeError("the iterator has no next method")
	}
	return r.newIterObject(&iterState{iter: iter, next: next}), nil
}

// iterNext advances a cursor, reporting whether a value was produced.
func (r *Runtime) iterNext(cursor Value) (Value, bool, error) {
	st := iterStateOf(cursor)
	if st == nil || st.done {
		return Undefined, false, nil
	}

	if st.forIn {
		// A key deleted since the snapshot was taken must be skipped, because
		// the loop body may have removed it.
		for st.idx < len(st.keys) {
			k := st.keys[st.idx]
			st.idx++
			return k, true, nil
		}
		st.done = true
		return Undefined, false, nil
	}

	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	if !res.IsObject() {
		st.done = true
		return Undefined, false, r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return Undefined, false, err
	}
	if done.Truthy() {
		st.done = true
		return Undefined, false, nil
	}
	val, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, false, err
	}
	return val, true, nil
}

// closeIter runs an iterator's return method when a loop exits early, which is
// how a generator learns that nothing more will be requested of it.
func (r *Runtime) closeIter(cursor Value) {
	st := iterStateOf(cursor)
	if st == nil || st.forIn || st.done {
		return
	}
	st.done = true
	r.closeIterator(st.iter)
}

// spreadInto appends the elements of an iterable to an array.
func (r *Runtime) spreadInto(arr *Object, src Value) error {
	// A dense array is copied directly, skipping the protocol entirely. This
	// is by far the common case and avoids allocating an iterator and a result
	// object per element.
	if src.IsObject() && src.Object().IsArray() {
		o := src.Object()
		if !r.hasOwnIterator(o) {
			for _, el := range o.elems {
				if isHole(el) {
					el = Undefined
				}
				arr.elems = append(arr.elems, el)
			}
			return nil
		}
	}
	return r.iterate(src, func(v Value) error {
		arr.elems = append(arr.elems, v)
		return nil
	})
}

// hasOwnIterator reports whether an array has had its Symbol.iterator replaced,
// in which case the fast path would observably skip user code.
func (r *Runtime) hasOwnIterator(o *Object) bool {
	key := r.atoms.internSymbol(r.wellKnown.iterator)
	return o.getOwn(key) != nil
}

// spreadToStack collects an iterable's elements for a call's argument list.
func (r *Runtime) spreadToStack(src Value) ([]Value, error) {
	var out []Value
	if src.IsObject() && src.Object().IsArray() && !r.hasOwnIterator(src.Object()) {
		for _, el := range src.Object().elems {
			if isHole(el) {
				el = Undefined
			}
			out = append(out, el)
		}
		return out, nil
	}
	err := r.iterate(src, func(v Value) error {
		out = append(out, v)
		return nil
	})
	return out, err
}

// copyDataProps implements object spread, copying own enumerable properties.
//
// It reads through getters rather than copying descriptors, which is what makes
// {...obj} produce plain data properties even when the source has accessors.
func (r *Runtime) copyDataProps(target *Object, src Value) error {
	// A string spreads as its indexed characters.
	if src.IsString() {
		s := src.String()
		for i := 0; i < s.Len(); i++ {
			if err := r.defineOwnProp(target, internIndex(uint32(i)),
				Str(s.Substring(i, i+1)), propDefault); err != nil {
				return err
			}
		}
		return nil
	}
	if !src.IsObject() {
		// A number or boolean has no own enumerable properties, so spreading
		// one contributes nothing rather than failing.
		return nil
	}
	o := src.Object()
	for _, k := range o.ownKeys(true, r.atoms) {
		if !r.isEnumerable(o, k) {
			continue
		}
		v, err := r.getProp(o, k, src)
		if err != nil {
			return err
		}
		if err := r.defineOwnProp(target, k, v, propDefault); err != nil {
			return err
		}
	}
	return nil
}

// objectRest builds the rest object of an object destructuring pattern: every
// own enumerable property of the source except those already bound.
func (r *Runtime) objectRest(src Value, excluded []Value) (Value, error) {
	out := newObject(r.proto.object, ClassObject)
	if src.IsNullish() {
		return Obj(out), nil
	}
	o, err := r.toObject(src)
	if err != nil {
		return Undefined, err
	}

	skip := make(map[Atom]bool, len(excluded))
	for _, e := range excluded {
		k, err := r.toPropertyKey(e)
		if err != nil {
			return Undefined, err
		}
		skip[k] = true
	}

	for _, k := range o.ownKeys(true, r.atoms) {
		if skip[k] || !r.isEnumerable(o, k) {
			continue
		}
		v, err := r.getProp(o, k, src)
		if err != nil {
			return Undefined, err
		}
		if err := r.defineOwnProp(out, k, v, propDefault); err != nil {
			return Undefined, err
		}
	}
	return Obj(out), nil
}

// newArgumentsObject materializes the arguments object for a call.
//
// It is the unmapped form, which is what strict mode requires and what every
// modern function gets: the elements are a snapshot, not aliases of the
// parameter slots.
func (r *Runtime) newArgumentsObject(f *frame) *Object {
	o := newObject(r.proto.object, ClassArguments)
	o.elems = append(o.elems, f.args...)
	o.setOwnRaw(atomLength, Int(len(f.args)), propWritable|propConfigurable)
	if f.callee != nil {
		o.setOwnRaw(atomCallee, Obj(f.callee), propWritable|propConfigurable)
	}
	// An arguments object is iterable, using the same iterator as an array.
	o.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator),
		Obj(r.newNativeFunc("[Symbol.iterator]", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				return rt.newArrayIterator(this)
			})), propWritable|propConfigurable)
	return o
}

// completionKind says how a protected block finished, which a finally clause
// must reproduce after it runs.
type completionKind uint8

const (
	completionNormal completionKind = iota
	completionThrow
	completionReturn
)
