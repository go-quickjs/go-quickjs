package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

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

	// keys is the snapshot for-in walks, with the atoms it was built from:
	// a key is looked up again before it is visited, since the loop body may
	// have deleted it in the meantime.
	keys     []Value
	keyAtoms []Atom
	obj      *Object
	idx      int
	// shape is obj's shape when the keys are its own and came from its
	// shape's cache: while obj keeps it, nothing has been deleted from it.
	shape *shape
	// indexKeys is, for an array whose keys were its elements 0 to
	// indexKeys-1 and nothing else, how many there were: they are walked
	// by index rather than snapshot (see startForIn).
	indexKeys int

	// iter and next drive the for-of protocol.
	iter Value
	next Value

	// arr is set for a plain dense array iterated with the intrinsic array
	// iterator, where walking the elements directly is indistinguishable from
	// the protocol and costs no result object per step.
	arr *Object

	// asyncIter marks an iterator reached through Symbol.asyncIterator, whose
	// results are awaited whole. A synchronous iterator driven asynchronously
	// is the other case: its result is a plain object, and only the value
	// inside it is awaited.
	asyncIter bool

	// delegation marks the cursor of a `yield*`, which an exception or a
	// return unwinding past it never closes: whatever goes wrong inside a
	// delegation is the delegate's own doing -- its next, throw or return
	// failing, or a result that is not an object -- and the delegate is not
	// asked to return because of it. The one case that closes it, a throw
	// the delegate has no method for, closes it in iterResume.
	delegation bool

	done bool
}

// newIterObject wraps a cursor so that it can live on the operand stack. The
// object and the cursor are one allocation: every for-of, for-in, spread
// and array destructuring makes one.
func (r *Runtime) newIterObject(st iterState) Value {
	io := &iterObject{Object: Object{class: ClassIterator, flags: objExtensible}, st: st}
	io.data = &io.st
	return Obj(&io.Object)
}

// iterObject is a cursor's object and the cursor, made together.
type iterObject struct {
	Object
	st iterState
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
		return r.newIterObject(*st), nil
	}
	o, err := r.toObject(v)
	if err != nil {
		return Undefined, err
	}

	st.obj = o
	// The walk ends at the last object up the chain that could add a key.
	// Beyond it are objects with nothing enumerable -- Array.prototype and
	// Object.prototype are -- whose keys could only shadow keys that nothing
	// further up adds. When that is the object itself, its own keys are
	// distinct and none needs remembering.
	last := o
	for cur := o.proto; cur != nil; cur = cur.proto {
		if !noEnumerableKeys(cur) {
			last = cur
		}
	}
	// A plain object whose keys are all its own, of a layout objects share,
	// has the keys its layout has, made once for every loop over an object
	// of that layout.
	if last == o {
		if c := r.shapeKeys(o); c != nil {
			st.keys, st.keyAtoms, st.shape = c.keys, c.atoms, o.shape
			return r.newIterObject(*st), nil
		}
		// An array whose keys are its elements and nothing else -- dense,
		// with no holes and no enumerable property of its own besides --
		// has the keys 0 to its length less one, which need no snapshot:
		// each is made when it is visited, and visited if it is still
		// there, as a snapshot's would be.
		if n, ok := elemKeysOnly(o); ok {
			st.indexKeys = n
			return r.newIterObject(*st), nil
		}
	}
	var seen map[Atom]bool
	if last != o {
		seen = make(map[Atom]bool)
	}
	for cur := o; ; cur = cur.proto {
		keys, err := r.ownKeysOf(cur, false)
		if err != nil {
			return Undefined, err
		}
		for _, k := range keys {
			if seen != nil {
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			// A non-enumerable property still shadows an enumerable one of the
			// same name further up the chain, which is why it is recorded in
			// seen before being skipped.
			enumerable, err := r.isEnumerable(cur, k)
			if err != nil {
				return Undefined, err
			}
			if !enumerable {
				continue
			}
			st.keys = append(st.keys, Str(r.keyString(k)))
			st.keyAtoms = append(st.keyAtoms, k)
		}
		if cur == last {
			break
		}
	}
	return r.newIterObject(*st), nil
}

// shapeKeys is the enumerable own string keys of a plain object of a layout
// objects share, as strings and as atoms, or nil for any other object. They
// are its layout's -- its keys, their order and which are enumerable -- so
// they are made the first time they are asked for and kept with the layout.
// An object with elements has keys of those as well, and is not one.
func (r *Runtime) shapeKeys(o *Object) *forInKeys {
	s := o.shape
	if o.class != ClassObject || len(o.elems) != 0 || s == nil || s.unique {
		return nil
	}
	if s.forIn == nil {
		c := &forInKeys{copyable: true}
		for _, k := range o.ownKeys(false, r.atoms) {
			if i := o.findOwn(k); i >= 0 && o.props[i].flags&(propEnumerable|propPrivate|propDeleted) == propEnumerable {
				c.keys = append(c.keys, Str(r.keyString(k)))
				c.atoms = append(c.atoms, k)
				c.slots = append(c.slots, i)
				if o.props[i].flags&propAccessor != 0 {
					c.copyable = false
				}
			}
		}
		for i := range o.props {
			if p := &o.props[i]; p.flags&(propEnumerable|propPrivate|propDeleted) == propEnumerable && r.atoms.IsSymbol(p.key) {
				c.copyable = false
			}
		}
		s.forIn = c
	}
	return s.forIn
}

// elemKeysOnly reports whether an array's keys are its dense elements and
// nothing else, and how many there are: it has no holes and no index stored
// elsewhere, and no enumerable property of its own besides.
func elemKeysOnly(o *Object) (int, bool) {
	if o.class != ClassArray || o.flags&objHasSparseElements != 0 {
		return 0, false
	}
	for i := range o.props {
		if o.props[i].flags&(propEnumerable|propDeleted) == propEnumerable {
			return 0, false
		}
	}
	for i := range o.elems {
		if holeAt(&o.elems[i]) {
			return 0, false
		}
	}
	return len(o.elems), true
}

// noEnumerableKeys reports whether an object has no enumerable own string key,
// as an ordinary object or array that only stores its properties answers
// without being asked: one whose keys are synthesized, or a proxy, which would
// see itself asked, are not looked at.
func noEnumerableKeys(o *Object) bool {
	if o.class != ClassObject && o.class != ClassArray || len(o.elems) != 0 || proxyOf(o) != nil {
		return false
	}
	for i := range o.props {
		if o.props[i].flags&(propEnumerable|propDeleted) == propEnumerable {
			return false
		}
	}
	return true
}

// startForOf opens an iterator over a value.
func (r *Runtime) startForOf(v Value) (Value, error) {
	// A plain dense array walked with the intrinsic iterator is stepped
	// directly: the protocol would allocate an iterator and a result object per
	// element, and nothing could tell the difference. The length is read again
	// at each step, so an array that grows or shrinks while it is iterated is
	// seen to, as the real iterator would see it.
	if v.IsObject() {
		o := v.Object()
		if o.class == ClassArray && o.flags&objHasSparseElements == 0 &&
			r.usesIntrinsicArrayIterator(o) {
			return r.newIterObject(iterState{arr: o}), nil
		}
	}
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
	// The method is read now and called later, and only the call requires it
	// to be callable: an iterator closed before it is ever stepped -- which a
	// destructuring pattern abandoned part-way through is -- never has to have
	// had a next at all.
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return Undefined, err
	}
	return r.newIterObject(iterState{iter: iter, next: next}), nil
}

// iterNext advances a cursor, reporting whether a value was produced.
func (r *Runtime) iterNext(cursor Value) (Value, bool, error) {
	st := iterStateOf(cursor)
	if st == nil || st.done {
		return Undefined, false, nil
	}

	if st.arr != nil {
		// The walk goes to the array's length, as the iterator's does, not
		// to the end of its dense storage: freezing an array, or writing far
		// past its end, moves elements out of it.
		if st.idx >= int(st.arr.arrayLength()) {
			st.done = true
			return Undefined, false, nil
		}
		i := st.idx
		st.idx++
		if i < len(st.arr.elems) {
			if v := st.arr.elems[i]; !isHole(v) {
				return v, true, nil
			}
		}
		// Anything else is read as the iterator's Get reads it: a hole through
		// the prototype chain, an element with attributes from the table.
		got, err := r.getProp(st.arr, r.atoms.indexAtom(uint32(i)), Obj(st.arr))
		if err != nil {
			st.done = true
		}
		return got, err == nil, err
	}

	if st.forIn {
		// An array's elements, walked by index: one still in its dense
		// storage is still there, and one that is not is looked for.
		for st.idx < st.indexKeys {
			i := st.idx
			st.idx++
			if o := st.obj; i < len(o.elems) && !holeAt(&o.elems[i]) {
				return Str(r.keyString(r.atoms.indexAtom(uint32(i)))), true, nil
			}
			key := r.atoms.indexAtom(uint32(i))
			has, err := r.hasPropErr(st.obj, key)
			if err != nil {
				st.done = true
				return Undefined, false, err
			}
			if has {
				return Str(r.keyString(key)), true, nil
			}
		}
		// A key deleted since the snapshot was taken is skipped: the loop body
		// may have removed it, and what is gone is not visited.
		for st.idx < len(st.keys) {
			k, key := st.keys[st.idx], st.keyAtoms[st.idx]
			st.idx++
			if st.obj != nil && (st.shape == nil || st.obj.shape != st.shape) {
				has, err := r.hasPropErr(st.obj, key)
				if err != nil {
					st.done = true
					return Undefined, false, err
				}
				if !has {
					continue
				}
			}
			return k, true, nil
		}
		st.done = true
		return Undefined, false, nil
	}

	// A result the iterator has handed over is the iterator's last word: if
	// reading done or value from it throws, the iteration is over and the
	// iterator is not asked to return. It was not the loop that gave up.
	val, ok, err := r.stepIter(st.iter, st.next)
	if !ok {
		st.done = true
	}
	return val, ok, err
}

// The built-in next methods a loop steps itself; see funcData.iterNext.
const (
	iterNextMap = 1 + iota
	iterNextGenerator
	iterNextArray
)

// stepIter calls an iterator's next method and reads its result: the value,
// or false once the iterator is done or has failed.
//
// A built-in next of this realm -- a Map or Set iterator's, an array
// iterator's, a generator's -- is applied directly, without the result object
// it would make: a loop reads only the object's own done and value, which
// nothing could have changed, so the object is never seen. An array
// iterator's and a generator's next still have their frames, which a getter
// it runs, or a stack trace taken in the generator's body, sees. A result a
// generator hands over from a yield* is the delegate's own object, and is
// read as any other.
func (r *Runtime) stepIter(iter, next Value) (Value, bool, error) {
	var res Value
	have := false
	if next.IsObject() && iter.IsObject() {
		n, it := next.Object(), iter.Object()
		if fd := n.fn(); fd != nil && fd.iterNext != 0 && (fd.realm == nil || fd.realm == r.Realm) {
			switch fd.iterNext {
			case iterNextMap:
				if d, ok := it.data.(*mapIterData); ok {
					if err := r.tick(); err != nil {
						return Undefined, false, err
					}
					v, ok := r.mapIterStep(d)
					return v, ok, nil
				}
			case iterNextArray:
				// An array iterator over what is not an array -- arguments,
				// a typed array, a string's wrapper, entries() -- reads the
				// length and the element as next does, which may run a
				// getter, so next's frame is there for it to see.
				if d, ok := it.data.(*arrayIterData); ok {
					if err := r.pushNativeFrame(n, iter, nil, Undefined); err != nil {
						return Undefined, false, err
					}
					v, ok, err := r.arrayIterStep(d)
					r.frameDepth--
					return v, ok, err
				}
			case iterNextGenerator:
				if g, ok := it.data.(*generator); ok && it.class == ClassGenerator {
					if err := r.pushNativeFrame(n, iter, nil, Undefined); err != nil {
						return Undefined, false, err
					}
					got, err := r.resumeFull(g, Undefined, resumeNext)
					r.frameDepth--
					if err != nil {
						return Undefined, false, err
					}
					if !got.raw || !got.value.IsObject() {
						return got.value, !got.done, nil
					}
					res, have = got.value, true
				}
			}
		}
	}
	if !have {
		var err error
		if res, err = r.call(next, iter, nil); err != nil {
			return Undefined, false, err
		}
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
	if st == nil || st.forIn || st.done || st.arr != nil {
		// A directly-walked array has no return method to call.
		return
	}
	st.done = true
	r.closeIterator(st.iter)
}

// closeIteratorsIn closes the for-of cursors sitting in a region of the operand
// stack that is about to be discarded.
//
// A cursor lives on the operand stack for as long as its loop is running, so
// the cursors in a discarded region are exactly the for-of loops being left
// abruptly -- by a throw unwinding to a handler, or by a return leaving the
// frame. Closing them there rather than at each exit point means every abrupt
// exit is covered by one rule, including ones the compiler cannot see, and a
// generator being iterated still gets to run its finally blocks.
//
// Closing runs user code, which may itself throw; that error is discarded,
// because the completion that caused the unwind is the one that matters.
func (r *Runtime) closeIteratorsIn(from, to int) {
	for i := to - 1; i >= from; i-- {
		st := iterStateOf(r.stack[i])
		if st == nil || st.forIn || st.done || st.arr != nil || st.delegation {
			continue
		}
		st.done = true
		r.closeIterator(st.iter)
	}
}

// closeIteratorsReturning closes the for-of cursors in a region that a return
// is leaving.
//
// Unlike a throw, a return carries no completion that outranks a failure in the
// iterator's own return method, so the first such failure becomes the result.
func (r *Runtime) closeIteratorsReturning(from, to int) error {
	var first error
	for i := to - 1; i >= from; i-- {
		if st := iterStateOf(r.stack[i]); st != nil && st.delegation {
			continue
		}
		if err := r.iterCloseNormal(r.stack[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// iterToArray drains an array-destructuring source into a dense array.
//
// Only as many values as the pattern names are pulled, so destructuring an
// infinite generator terminates; the iterator is then closed, because the
// pattern is done with it. A pattern with a rest element asks for all of them,
// and the iterator ends exhausted.
func (r *Runtime) iterToArray(src Value, want uint32) (Value, error) {
	// A plain dense array with the standard iterator is copied directly. This
	// is overwhelmingly the common case, and going through the protocol would
	// allocate an iterator and a result object per element for no observable
	// difference.
	if src.IsObject() && src.Object().IsArray() && r.usesIntrinsicArrayIterator(src.Object()) {
		return src, nil
	}

	cursor, err := r.startForOf(src)
	if err != nil {
		return Undefined, err
	}
	out := newObject(r.proto.array, ClassArray)
	for want == bytecode.IterAll || uint32(len(out.elems)) < want {
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		if !ok {
			break
		}
		out.elems = append(out.elems, v)
	}
	// The pattern has what it needs, so anything still suspended upstream is
	// told to stop.
	r.closeIter(cursor)
	return Obj(out), nil
}

// spreadInto appends the elements of an iterable to an array.
func (r *Runtime) spreadInto(arr *Object, src Value) error {
	// What the spread adds is charged to the memory limit: spreading an
	// array into itself, twice, doubles it in one step.
	before := cap(arr.elems)
	// A plain dense array is copied directly, skipping the protocol entirely.
	// This is by far the common case and avoids allocating an iterator and a
	// result object per element.
	if els, ok := r.plainDenseElems(src); ok {
		arr.elems = append(arr.elems, els...)
	} else if err := r.iterate(src, func(v Value) error {
		arr.elems = append(arr.elems, v)
		return nil
	}); err != nil {
		return err
	}
	if r.meter != nil {
		return r.chargeGrowth(arr, int64(cap(arr.elems)-before)*valueSize)
	}
	return nil
}

// plainDenseElems returns an array's elements when reading them directly is
// indistinguishable from walking it with the iteration protocol.
//
// Three things rule that out: an own Symbol.iterator, which would mean skipping
// user code; elements that have moved into the property table, which freezing
// and defineProperty both do; and a hole, which the protocol would look up the
// prototype chain for rather than report as undefined.
func (r *Runtime) plainDenseElems(src Value) ([]Value, bool) {
	if !src.IsObject() {
		return nil, false
	}
	o := src.Object()
	if !o.IsArray() || o.flags&objHasSparseElements != 0 || !r.usesIntrinsicArrayIterator(o) {
		return nil, false
	}
	for _, el := range o.elems {
		if isHole(el) {
			return nil, false
		}
	}
	return o.elems, true
}

// usesIntrinsicArrayIterator reports whether an array still iterates the way
// Array.prototype does.
//
// Anything else -- a replaced %ArrayIteratorPrototype%.next, an own
// Symbol.iterator, a replaced one on the prototype, or none at all, which `delete Array.prototype[Symbol.iterator]` leaves -- means
// the fast path would answer differently from the protocol. The last case is
// the one that matters most: without an iterator, destructuring an array is a
// TypeError, and a fast path that skipped the lookup would quietly succeed.
func (r *Runtime) usesIntrinsicArrayIterator(o *Object) bool {
	// The iterator's next is called for each element, so a replaced one is
	// as much a different way of iterating as a replaced Symbol.iterator.
	if p := r.proto.arrayIter.getOwn(atomNext); p == nil || p.isAccessor() ||
		!p.value.IsObject() || p.value.Object() != r.arrayIterNextFn {
		return false
	}
	key := r.atoms.internSymbol(r.wellKnown.iterator)
	for cur := o; cur != nil; cur = cur.proto {
		if p := cur.getOwn(key); p != nil {
			return !p.isAccessor() && p.value.IsObject() &&
				p.value.Object() == r.arrayValuesFn
		}
	}
	return false
}

// spreadToStack collects an iterable's elements for a call's argument list.
func (r *Runtime) spreadToStack(src Value) ([]Value, error) {
	var out []Value
	if els, ok := r.plainDenseElems(src); ok {
		out = append(out, els...)
	} else if err := r.iterate(src, func(v Value) error {
		out = append(out, v)
		return nil
	}); err != nil {
		return out, err
	}
	// The values are charged to the memory limit as a builder's output is:
	// a call spreading an array into a push onto it doubles it.
	if r.meter != nil {
		h := heldMeter{r: r}
		if err := h.charge(cap(out) * int(valueSize)); err != nil {
			return nil, err
		}
	}
	return out, nil
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
			if err := r.defineOwnProp(target, r.atoms.indexAtom(uint32(i)),
				Str(r.unitString(s, i)), propDefault); err != nil {
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
	// A plain object of a layout objects share, whose enumerable properties
	// all hold data under string keys, has its keys, in order, and their
	// places with the layout: its values are read from there, with nothing
	// a getter could see.
	if c := r.shapeKeys(o); c != nil && c.copyable {
		for i, k := range c.atoms {
			if err := r.defineOwnProp(target, k, o.props[c.slots[i]].value, propDefault); err != nil {
				return err
			}
		}
		return nil
	}
	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return err
	}
	for _, k := range keys {
		enumerable, err := r.isEnumerable(o, k)
		if err != nil {
			return err
		}
		if !enumerable {
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

	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return Undefined, err
	}
	for _, k := range keys {
		if skip[k] {
			continue
		}
		enumerable, err := r.isEnumerable(o, k)
		if err != nil {
			return Undefined, err
		}
		if !enumerable {
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

// argumentsData holds what a mapped arguments object aliases.
//
// mapped[i] is the parameter the index i was passed to, or nil where there is
// none: an argument beyond the declared parameters, one whose parameter is
// shadowed by a later one of the same name, or an index the program has since
// deleted or redefined.
type argumentsData struct {
	mapped []*upvalue
}

// newArgumentsObject materializes the arguments object for a call.
//
// In sloppy mode with a simple parameter list the object is mapped: its indices
// alias the parameters they were passed to, so that writing one is visible
// through the other. Strict mode and anything more elaborate than plain
// parameters get the unmapped form, where the elements are a snapshot.
//
// The aliases are the ordinary captured-binding cells, so they keep working
// after the call returns -- which they must, since the object can outlive it.
func (r *Runtime) newArgumentsObject(f *frame) *Object {
	// Its length, callee and Symbol.iterator are carried in the object's own
	// allocation, rather than in a table grown as each is added.
	o := newLiteralObject(r.proto.object, ClassArguments, 3)
	o.elems = append(o.elems, f.args...)
	mapped := f.cl != nil && f.cl.fn.MappedArguments
	kind := 0
	if mapped {
		kind = 1
	}
	if t := r.argsLayouts[kind]; t != nil && (!mapped || f.callee != nil) {
		// Every arguments object of the kind has the same three properties
		// in the same layout as the first, so they are copied from it, with
		// the values that are this one's own.
		o.props = append(o.props, t.props[:]...)
		o.shape = t.shape
		o.props[0].value = Int(len(f.args))
		if mapped {
			o.props[1].value = Obj(f.callee)
			r.mapArguments(o, f)
		} else {
			o.props[1].value = accessorValue(&accessor{getter: r.throwTypeErrorFn, setter: r.throwTypeErrorFn})
		}
		return o
	}
	o.setOwnRaw(atomLength, Int(len(f.args)), propWritable|propConfigurable)
	if mapped {
		r.mapArguments(o, f)
	}
	defer r.keepArgsLayout(kind, o)
	switch {
	case !mapped:
		// An unmapped arguments object refuses to say what called it: the
		// property is there, and reading or writing it throws.
		r.defineAccessor(o, atomCallee, r.throwTypeErrorFn, r.throwTypeErrorFn, 0)
	case f.callee != nil:
		o.setOwnRaw(atomCallee, Obj(f.callee), propWritable|propConfigurable)
	}
	// An arguments object is iterable, using the same iterator as an array --
	// not merely an equivalent one but the very function Array.prototype.values
	// is, which a script can check.
	o.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator),
		Obj(r.arrayValuesFn), propWritable|propConfigurable)
	return o
}

// argsLayout is the layout of the three properties every object of a kind
// is made with -- an arguments object's length, callee and Symbol.iterator, a
// match result's index, input and groups -- and their shape if they have one,
// kept from the first one made, for the rest to copy.
type argsLayout struct {
	shape *shape
	props [3]Property
}

// layoutOf is the layout of an object just made, if it is one the rest of its
// kind can share: three properties, with no shape -- a table small enough to
// scan -- or one of the tree rather than one of the object's own.
func layoutOf(o *Object) *argsLayout {
	if len(o.props) != 3 || o.shape != nil && (o.shape.unique || o.shape.n != 3) {
		return nil
	}
	t := &argsLayout{shape: o.shape}
	copy(t.props[:], o.props)
	return t
}

// keepArgsLayout keeps the layout of a first arguments object of a kind.
func (r *Runtime) keepArgsLayout(kind int, o *Object) {
	r.argsLayouts[kind] = layoutOf(o)
}

// mapArguments aliases each index to the parameter it was passed to.
//
// Two parameters may share a name in a sloppy simple list, and only the last of
// them is the one the name resolves to -- so only that one is mapped, and the
// earlier index is left alone.
func (r *Runtime) mapArguments(o *Object, f *frame) {
	fn := f.cl.fn
	n := fn.ParamCount
	if n > len(f.args) {
		n = len(f.args)
	}
	if n <= 0 {
		return
	}
	mapped := make([]*upvalue, n)
	seen := make(map[string]bool, n)
	for i := n - 1; i >= 0; i-- {
		name := ""
		if i < len(fn.Locals) {
			name = fn.Locals[i].Name
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		mapped[i] = r.captureLocal(f, i)
	}
	o.data = &argumentsData{mapped: mapped}
	o.flags |= objMappedArguments
}

// completionKind says how a protected block finished, which a finally clause
// must reproduce after it runs.
type completionKind uint8

const (
	completionNormal completionKind = iota
	completionThrow
	completionReturn
)

// startForAwaitOf opens an async iterator over a value.
//
// Symbol.asyncIterator is preferred, but a plain iterable is accepted too: its
// values are awaited individually, which is what makes `for await` work over an
// array of promises.
func (r *Runtime) startForAwaitOf(v Value) (Value, error) {
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.asyncIterator))
	if err != nil {
		return Undefined, err
	}
	// Only an absent Symbol.asyncIterator falls back to the synchronous
	// protocol. One that is present but not callable is a mistake rather than
	// an absence, and saying so here is what stops the sync iterator from
	// being called for an object that declared itself async.
	if method.IsNullish() {
		return r.startForOf(v)
	}
	if !isCallable(method) {
		return Undefined, r.throwTypeError("Symbol.asyncIterator is not a function")
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return Undefined, err
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return Undefined, err
	}
	return r.newIterObject(iterState{iter: iter, next: next, asyncIter: true}), nil
}

// iterSend calls a cursor's next method with a value, which is what `yield*`
// needs: the value its own caller sent in has to reach the delegate, or a
// generator delegating to another cannot be driven at all.
//
// The raw iterator result is returned rather than an unpacked value, because
// the caller needs the done flag and the returned value separately.
func (r *Runtime) iterSend(cursor Value, sent Value, async bool) (Value, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, r.throwTypeError("not an iterator")
	}
	if st.forIn {
		return Undefined, r.throwTypeError("cannot delegate to a property enumeration")
	}
	if st.arr != nil {
		// A directly-walked array has no next to send to, and nothing to do
		// with the value that would have been sent.
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		res := Obj(r.iterResult(v, !ok))
		if !async {
			return res, nil
		}
		return r.awaitIterResult(st, res)
	}
	res, err := r.call(st.next, st.iter, []Value{sent})
	if err != nil {
		return Undefined, err
	}
	if !async {
		return res, nil
	}
	return r.asyncResult(st, res), nil
}

// asyncResult turns what an iterator method returned into the promise the
// awaiting instruction that follows expects.
//
// An async iterator's result is awaited whole by the instruction that follows,
// whatever it is: `{value: promise}` from one of those yields the promise
// itself rather than what it settles to. A synchronous iterator's result is a
// plain object that needs no awaiting, but the value inside it does, so that
// one is rebuilt around a promise for the value.
func (r *Runtime) asyncResult(st *iterState, res Value) Value {
	if st.asyncIter {
		// Handed on as it is: the await that follows resolves it, and doing
		// that here as well would look up the promise's constructor twice.
		return res
	}
	out, err := r.awaitIterResult(st, res)
	if err != nil {
		p := r.newPromise()
		r.rejectPromise(p, thrownValue(err))
		return Obj(p)
	}
	return out
}

// asyncIterNext calls an async iterator's next method, returning the promise it
// produces. A synchronous iterator is wrapped so that both protocols can be
// driven by the same instruction sequence.
func (r *Runtime) asyncIterNext(cursor Value) (Value, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, r.throwTypeError("not an iterator")
	}
	if st.forIn {
		return Undefined, r.throwTypeError("cannot iterate properties asynchronously")
	}
	if st.arr != nil {
		// A directly-walked array has no next to call, so the plain result
		// object the rest of this needs is built here.
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		return r.awaitIterResult(st, Obj(r.iterResult(v, !ok)))
	}
	res, err := r.call(st.next, st.iter, nil)
	if err != nil {
		return Undefined, err
	}
	return r.asyncResult(st, res), nil
}

// awaitIterResult turns a synchronous iterator's result into the promise a
// for-await loop expects.
//
// A synchronous iterator returns a plain result object, whose value must be
// awaited individually: `for await (const v of [1, promise])` yields the
// promise's value, not the promise. So the result is rebuilt around the settled
// value rather than merely wrapped.
func (r *Runtime) awaitIterResult(st *iterState, res Value) (Value, error) {
	// A synchronous iterator driven asynchronously still has to hand back a
	// result object. Anything else is a TypeError, which reaches the caller as
	// a rejection rather than a throw.
	if !res.IsObject() {
		return Undefined, r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return Undefined, err
	}
	value, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, err
	}
	isDone := done.Truthy()

	out := r.newPromise()
	onFulfilled := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		rt.resolvePromise(out, Obj(rt.iterResult(arg(a, 0), isDone)))
		return Undefined, nil
	})
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		// A synchronous iterator that produced a rejected promise is done
		// with: the loop will never ask it for another value, so it is closed
		// here rather than left for the unwinding to reach. That is what makes
		// a generator's finally run before the rejection is delivered. The
		// cursor is marked done so that the unwinding does not close it again.
		if !isDone {
			st.done = true
			rt.closeIterator(st.iter)
		}
		rt.rejectPromise(out, arg(a, 0))
		return Undefined, nil
	})
	wrapped, err := r.toPromiseErr(value)
	if err != nil {
		// The value could not even be wrapped -- reading its constructor
		// threw -- so the result is rejected here rather than through a
		// promise of its own: there is nothing left to wait for.
		if !isDone {
			st.done = true
			r.closeIterator(st.iter)
		}
		r.rejectPromise(out, thrownValue(err))
		return Obj(out), nil
	}
	r.promiseThen(wrapped, Obj(onFulfilled), Obj(onRejected))
	return Obj(out), nil
}

// iterStep advances a cursor for a destructuring pattern, reporting undefined
// once the iterator is exhausted.
//
// A pattern asks for as many values as it names, and a source with fewer simply
// leaves the rest undefined -- which is why this reports a value rather than
// whether there was one.
func (r *Runtime) iterStep(cursor Value) (Value, error) {
	v, ok, err := r.iterNext(cursor)
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return Undefined, nil
	}
	return v, nil
}

// iterRest drains what is left of a cursor into a dense array, which is what a
// pattern's rest element binds.
func (r *Runtime) iterRest(cursor Value) (Value, error) {
	out := newObject(r.proto.array, ClassArray)
	for {
		v, ok, err := r.iterNext(cursor)
		if err != nil {
			return Undefined, err
		}
		if !ok {
			return Obj(out), nil
		}
		out.elems = append(out.elems, v)
	}
}

// iterCloseNormal closes a destructuring pattern's cursor once the pattern is
// done with it.
//
// A pattern that stopped short of the end tells the iterator so, and unlike a
// close during an abrupt completion this one has nothing else in flight: a
// failure in the return method is the result. So is a return method that hands
// back something other than an object, which is the one place the protocol
// checks that.
func (r *Runtime) iterCloseNormal(cursor Value) error {
	st := iterStateOf(cursor)
	if st == nil || st.forIn || st.done || st.arr != nil {
		return nil
	}
	st.done = true
	ret, err := r.getValueProp(st.iter, atomReturn)
	if err != nil {
		return err
	}
	// An iterator without a return method simply ends; one whose return is
	// neither absent nor callable is a TypeError, which is what looking a
	// method up means.
	if ret.IsUndefined() || ret.IsNull() {
		return nil
	}
	if !isCallable(ret) {
		return r.throwTypeError("the iterator's return method is not callable")
	}
	res, err := r.call(ret, st.iter, nil)
	if err != nil {
		return err
	}
	if !res.IsObject() {
		return r.throwTypeError("the iterator's return method must return an object")
	}
	return nil
}

// iterResume drives one step of a `yield*`.
//
// How the outer generator was resumed decides which of the delegate's methods
// is called: next for an ordinary resumption, throw for an injected exception,
// return for a forced return. A delegate that has no throw is closed and the
// delegation fails, since there is no way to deliver the exception; one that
// has no return simply ends, which is what lets `yield*` work over an iterator
// that never expected to be abandoned.
//
// The bool reports that the delegation ended with a return the delegate could
// not take, which the outer generator turns into a return of its own.
func (r *Runtime) iterResume(cursor Value, sent Value, mode resumeMode,
	async bool) (Value, bool, error) {
	st := iterStateOf(cursor)
	if st == nil {
		return Undefined, false, r.throwTypeError("not an iterator")
	}
	st.delegation = true
	switch mode {
	case resumeThrow:
		if st.arr != nil {
			// A directly-walked array has no throw, so the delegation fails
			// with the exception it was given.
			st.done = true
			return Undefined, false, r.throw(sent)
		}
		method, err := r.getValueProp(st.iter, r.atoms.intern("throw"))
		if err != nil {
			return Undefined, false, err
		}
		if !isCallable(method) {
			// The delegate is told the delegation is over before the failure
			// is reported, which is what gives a generator its finally -- and
			// a failure there is what the delegation fails with, since the
			// TypeError has not been raised yet.
			st.done = true
			if err := r.closeIteratorErr(st.iter); err != nil {
				return Undefined, false, err
			}
			return Undefined, false, r.throwTypeError(
				"the delegate has no throw method")
		}
		res, err := r.call(method, st.iter, []Value{sent})
		if err != nil {
			return Undefined, false, err
		}
		return r.delegateResult(st, res, async)

	case resumeReturn:
		if st.arr != nil {
			return sent, true, nil
		}
		method, err := r.getValueProp(st.iter, atomReturn)
		if err != nil {
			return Undefined, false, err
		}
		if !isCallable(method) {
			return sent, true, nil
		}
		res, err := r.call(method, st.iter, []Value{sent})
		if err != nil {
			return Undefined, false, err
		}
		out, _, err := r.delegateResult(st, res, async)
		if err != nil {
			return Undefined, false, err
		}
		if async {
			// The result is a promise; whether it says done is only known once
			// it settles, so the instruction that reads it afterwards decides,
			// which is why it is told what kind of resumption this was.
			return out, false, nil
		}
		if !out.IsObject() {
			return Undefined, false, r.throwTypeError(
				"an iterator result must be an object")
		}
		done, err := r.getValueProp(out, atomDone)
		if err != nil {
			return Undefined, false, err
		}
		if done.Truthy() {
			v, err := r.getValueProp(out, atomValue)
			return v, true, err
		}
		return out, false, nil
	}
	res, err := r.iterSend(cursor, sent, async)
	return res, false, err
}

// delegateResult checks what a delegate's method handed back.
func (r *Runtime) delegateResult(st *iterState, res Value, async bool) (Value, bool, error) {
	if async {
		return r.asyncResult(st, res), false, nil
	}
	if !res.IsObject() {
		return Undefined, false, r.throwTypeError("an iterator result must be an object")
	}
	return res, false, nil
}
