package vm

// This file implements the property access semantics: the [[Get]], [[Set]],
// [[HasProperty]] and [[Delete]] internal methods, including the prototype
// chain walk, accessor invocation, and the exotic behaviour of arrays, string
// wrappers and the arguments object.

// GetProp returns the value of a property, walking the prototype chain and
// invoking a getter if it finds one.
//
// receiver is the value `this` takes inside a getter. It differs from the
// object being searched when a getter is inherited, which is what lets a
// prototype accessor read the properties of the instance it was called on.
func (r *Runtime) getProp(obj *Object, key Atom, receiver Value) (Value, error) {
	for o := obj; o != nil; o = o.proto {
		// A proxy intercepts the operation before anything else happens,
		// including the rest of the prototype walk, since the trap decides
		// what the chain even is. That holds wherever in the chain it is
		// reached, not only when it is where the lookup started.
		if p := proxyOf(o); p != nil {
			return r.proxyGet(p, key, receiver)
		}
		// Dense elements come first, since an array index is the hottest key.
		if key.IsIndex() {
			if v, ok := o.getElem(key.Index()); ok {
				return v, nil
			}
			// A class with exotic index behaviour handles indices not present
			// in its dense storage. An ordinary object has none, which is the
			// common case and worth not asking about.
			if o.class != ClassObject {
				if v, ok, err := r.getExoticIndex(o, key.Index()); ok || err != nil {
					return v, err
				}
			}
		} else if o.class != ClassObject {
			if v, ok, err := r.getExoticNamed(o, key); ok || err != nil {
				return v, err
			}
		}

		// The scan of a small table is written out here rather than called: a
		// property read is the hottest thing the interpreter does, and the
		// layers it otherwise goes through -- own, visible, not an accessor --
		// cost more than the comparisons they wrap. Anything less ordinary
		// than a plain data property falls through to them.
		//
		// A mapped arguments object is left out whole: its indices name
		// parameters, and only the binding knows what they hold now.
		if o.index == nil && o.flags&objMappedArguments == 0 {
			for i := range o.props {
				p := &o.props[i]
				if p.key == key &&
					p.flags&(propDeleted|propPrivate|propAccessor) == 0 {
					return p.value, nil
				}
			}
		}
		if p := o.getOwnVisible(key); p != nil {
			if p.isAccessor() {
				a := p.getterSetter()
				if a == nil || a.getter == nil {
					// An accessor with no getter reads as undefined.
					return Undefined, nil
				}
				return r.call(Obj(a.getter), receiver, nil)
			}
			if u := mappedArgument(o, key); u != nil {
				// The index still names a parameter even though the property
				// has left dense storage, which redefining it does.
				return u.get(), nil
			}
			return p.value, nil
		}
	}
	return Undefined, nil
}

// getExoticNamed handles the named properties that some classes synthesize
// rather than store.
func (r *Runtime) getExoticNamed(o *Object, key Atom) (Value, bool, error) {
	switch o.class {
	case ClassArray:
		if key == atomLength {
			return Uint32(o.arrayLength()), true, nil
		}
	case ClassStringWrapper:
		if key == atomLength {
			if s, ok := o.data.(*String); ok {
				return Int(s.Len()), true, nil
			}
		}
	case ClassTypedArray:
		if key == atomLength && o.getOwn(atomLength) == nil {
			if t, ok := o.data.(*typedArrayData); ok {
				// A view over a buffer that has gone is empty, not eight
				// elements of nothing. The answer comes from the prototype's
				// getter, so a property a script defines under the name is
				// what is read instead.
				return Int(t.count()), true, nil
			}
		}
		// A numeric key is answered from the buffer or not at all. It is never
		// looked for up the prototype chain, which is what stops
		// Uint8Array.prototype[1.5] from being visible through an instance.
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			if !ix.valid {
				return Undefined, true, nil
			}
			return o.data.(*typedArrayData).getElem(ix.i), true, nil
		}
	case ClassFunction:
		// name and length are materialized on first read rather than created
		// with every function, since most functions are never asked.
		if fd := o.fn(); fd != nil {
			if !fd.propsMaterialized {
				switch key {
				case atomLength:
					if o.getOwn(atomLength) == nil {
						return Int(fd.length), true, nil
					}
				case atomName:
					if o.getOwn(atomName) == nil {
						return Str(NewString(fd.name)), true, nil
					}
				}
			}
			// .prototype is an object rather than a value, so it is built
			// rather than synthesized: whoever asks first gets an object the
			// next reader has to see too.
			if key == atomPrototype && fd.protoPending {
				r.materializeFunctionProto(o)
			}
		}
	}
	return Undefined, false, nil
}

// materializeFunctionProp creates a function's name or length as a real own
// property, so that anything looking at the property table rather than reading
// through getProp finds it.
//
// They are synthesized on demand rather than created with every function -- a
// program makes far more functions than it inspects -- but a descriptor query,
// a redefinition or an ownKeys walk has to see the same property a read does.
func (r *Runtime) materializeFunctionProp(o *Object, key Atom) {
	if o.class != ClassFunction {
		return
	}
	if key == atomPrototype {
		r.materializeFunctionProto(o)
		return
	}
	if key != atomName && key != atomLength {
		return
	}
	fd := o.fn()
	if fd == nil || fd.propsMaterialized {
		return
	}
	// Both are created together, because the synthesized reads stop as soon as
	// either exists -- one of them is a flag on the function, not on the
	// property. They are non-writable, non-enumerable and configurable, which
	// is what lets a script rename a function with defineProperty, or delete
	// the name outright, but not assign to it.
	//
	// They go at the front of the table rather than the end, because that is
	// where they would have been: a function is built with its length and its
	// name before anything else -- a prototype, a static member -- is added. A
	// class whose static member is called "length" depends on this, since
	// redefining a property leaves it where it was.
	fd.propsMaterialized = true
	var head [2]Property
	n := 0
	if o.getOwn(atomLength) == nil {
		head[n] = Property{key: atomLength, value: Int(fd.length), flags: propConfigurable}
		n++
	}
	if o.getOwn(atomName) == nil {
		head[n] = Property{key: atomName, value: Str(NewString(fd.name)), flags: propConfigurable}
		n++
	}
	o.prependProps(head[:n])
}

// toNumericForElement coerces a value as writing it to a typed array would,
// and discards it.
//
// A write outside the array stores nothing, but the coercion still happens:
// the specification orders it before the range check, and a valueOf on the
// value being written can see that it did.
func (r *Runtime) toNumericForElement(o *Object, v Value) (Value, error) {
	t, ok := o.data.(*typedArrayData)
	if !ok {
		return Undefined, nil
	}
	if t.info().big {
		_, err := r.toBigIntOperand(v)
		return Undefined, err
	}
	_, err := r.toNumber(v)
	return Undefined, err
}

// getExoticIndex handles index reads that are not backed by dense storage.
func (r *Runtime) getExoticIndex(o *Object, idx uint32) (Value, bool, error) {
	switch o.class {
	case ClassStringWrapper:
		if s, ok := o.data.(*String); ok && int(idx) < s.Len() {
			return Str(s.Substring(int(idx), int(idx)+1)), true, nil
		}
	case ClassTypedArray:
		// A typed array's elements live in its buffer, so an index never
		// reaches the property table.
		if t, ok := o.data.(*typedArrayData); ok {
			if t.storage().detached {
				return Undefined, true, nil
			}
			return t.getElem(int(idx)), true, nil
		}
	}
	return Undefined, false, nil
}

// getValueProp reads a property from any value, boxing a primitive so that
// "abc".length and (5).toFixed work without materializing a wrapper object.
func (r *Runtime) getValueProp(v Value, key Atom) (Value, error) {
	switch v.Kind() {
	case KindObject:
		return r.getProp(v.Object(), key, v)

	case KindString:
		s := v.String()
		// length and indices are answered directly from the string, avoiding
		// the wrapper entirely.
		if key == atomLength {
			return Int(s.Len()), nil
		}
		if key.IsIndex() {
			i := int(key.Index())
			if i < s.Len() {
				return Str(s.Substring(i, i+1)), nil
			}
			return Undefined, nil
		}
		return r.getProp(r.proto.str, key, v)

	case KindNumber:
		return r.getProp(r.proto.number, key, v)
	case KindBool:
		return r.getProp(r.proto.boolean, key, v)
	case KindSymbol:
		return r.getProp(r.proto.symbol, key, v)
	case KindBigInt:
		return r.getProp(r.proto.bigint, key, v)
	}
	// null and undefined have no properties at all.
	return Undefined, r.throwTypeError("cannot read property %q of %s",
		r.atoms.name(key), v.Kind())
}

// setProp assigns a property, honouring setters found on the prototype chain
// and the non-writability of inherited data properties.
// The bool says whether the assignment happened, which Reflect.set reports and
// a sloppy-mode assignment ignores.
func (r *Runtime) setProp(obj *Object, key Atom, val Value, receiver Value, strict bool) (bool, error) {
	// Almost every assignment writes to the object it was found on, so the
	// question of where the value lands is settled once rather than per step.
	var rcv *Object
	if receiver.IsObject() {
		rcv = receiver.Object()
	}
	// Walk the chain looking for an accessor or a non-writable data property,
	// either of which changes what a plain assignment does. A proxy anywhere
	// along it decides the rest for itself.
	for o := obj; o != nil; o = o.proto {
		if p := proxyOf(o); p != nil {
			return r.proxySet(p, key, val, receiver, strict)
		}
		if o.class == ClassModuleNamespace {
			// A namespace refuses every assignment, whatever the key names and
			// whether or not it has it: what a module exports is the module's
			// to change. It answers for a receiver that inherits from it too,
			// which is why this is in the walk rather than before it.
			return false, r.assignFailed(key, strict,
				"cannot assign to %q of a module namespace")
		}
		if key.IsIndex() {
			if int(key.Index()) < len(o.elems) && !isHole(o.elems[key.Index()]) {
				// A dense element is always a writable data property, so the
				// assignment lands here.
				if o == obj && rcv == obj {
					o.setElem(key.Index(), val)
					return true, nil
				}
				break
			}
		}
		// A function's name and length are synthesized rather than stored, so
		// the walk would not otherwise find them -- and they are non-writable,
		// which is what makes `f.name = "x"` silently do nothing. An ordinary
		// object synthesizes nothing, which is the common case and worth not
		// asking about.
		if o.class == ClassFunction && (key == atomName || key == atomLength) {
			if fd := o.fn(); fd != nil && !fd.propsMaterialized {
				return false, r.assignFailed(key, strict,
					"cannot assign to read-only property %q")
			}
		}
		// A .prototype not built yet is an own property all the same, so it is
		// built before the walk looks for one: assigning to it must overwrite
		// that property rather than create a fresh, enumerable one.
		if o.class == ClassFunction && key == atomPrototype {
			r.materializeFunctionProto(o)
		}
		// The other synthesized own properties -- an array's length, a string
		// wrapper's characters, a typed array's elements -- are own properties
		// too, so the walk stops at them rather than looking for a setter
		// further up that they shadow.
		if o.class != ClassObject && r.hasExoticOwn(o, key) {
			if o == obj && rcv == obj {
				return r.createOwnProp(o, key, val, strict)
			}
			if o.class == ClassTypedArray {
				if ix := r.typedArrayIndex(o, key); ix.numeric && !ix.valid {
					// A numeric key naming no element of the view is dropped
					// even when the write arrived through something else: the
					// view owns every numeric key, so there is nothing for the
					// receiver to shadow. The value is still coerced when the
					// view is the receiver, since that is the view's own write
					// rather than one passing through it.
					if rcv == o {
						_, err := r.toNumericForElement(o, val)
						return err == nil, err
					}
					return true, nil
				}
			}
			break
		}
		p := o.getOwnVisible(key)
		if p == nil {
			continue
		}
		if p.isAccessor() {
			a := p.getterSetter()
			if a == nil || a.setter == nil {
				return false, r.assignFailed(key, strict,
					"cannot assign to %q, which has only a getter")
			}
			_, err := r.call(Obj(a.setter), receiver, []Value{val})
			return err == nil, err
		}
		if p.flags&propWritable == 0 {
			return false, r.assignFailed(key, strict,
				"cannot assign to read-only property %q")
		}
		if o == obj && rcv == obj {
			p.value = val
			if u := mappedArgument(o, key); u != nil {
				u.set(val)
			}
			return true, nil
		}
		// An inherited writable data property is shadowed by a new own
		// property rather than modified in place, and so is one reached
		// through a receiver that is not the object holding it.
		break
	}

	// The assignment lands on the receiver, which is the object itself unless
	// something forwarded to it -- a prototype's setter, or a proxy with no set
	// trap. A receiver that is not the object defines the property through its
	// own machinery, which may be a trap.
	if rcv == nil {
		// A primitive receiver has nowhere to put it.
		return false, r.assignFailed(key, strict, "cannot create property %q on a primitive")
	}
	if rcv != obj {
		return r.setOnReceiver(rcv, key, val, strict)
	}
	return r.createOwnProp(obj, key, val, strict)
}

// setOnReceiver completes an assignment that landed on an object other than the
// one the property was found on.
//
// It goes through the receiver's own define machinery rather than writing into
// its table, because the receiver may be a proxy, and the operation it performs
// is a define rather than a set: an inherited property is shadowed by a new own
// one rather than written through.
func (r *Runtime) setOnReceiver(rcv *Object, key Atom, val Value, strict bool) (bool, error) {
	cur, err := r.ownPropDesc(rcv, key)
	if err != nil {
		return false, err
	}
	if cur != nil {
		if cur.isAccessor() || (cur.hasWritable && !cur.writable) {
			return false, r.assignFailed(key, strict,
				"cannot assign to read-only property %q")
		}
		ok, err := r.defineProperty(rcv, key, &propDesc{value: val, hasValue: true})
		if err != nil || ok {
			return ok, err
		}
		return false, r.assignFailed(key, strict, "cannot assign to property %q")
	}
	ok, err := r.defineProperty(rcv, key, &propDesc{
		value: val, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	if err != nil || ok {
		return ok, err
	}
	return false, r.assignFailed(key, strict, "cannot create property %q")
}

// assignFailed reports an assignment that did not happen, which is an error in
// strict mode and silence otherwise.
func (r *Runtime) assignFailed(key Atom, strict bool, format string) error {
	if strict {
		return r.throwTypeError(format, r.atoms.name(key))
	}
	return nil
}

// hasExoticOwn reports whether a key names one of the own properties a class
// synthesizes rather than stores.
func (r *Runtime) hasExoticOwn(o *Object, key Atom) bool {
	switch o.class {
	case ClassArray:
		return key == atomLength
	case ClassStringWrapper:
		s, ok := o.data.(*String)
		if !ok {
			return false
		}
		return key == atomLength || (key.IsIndex() && int(key.Index()) < s.Len())
	case ClassTypedArray:
		// A view's length is a getter on the prototype rather than one of its
		// own properties, so nothing here shadows it -- and a property a script
		// defines under that name does.
		//
		// Every numeric key belongs to the view, whether or not it names an
		// element: one that does not is dropped rather than looked for on the
		// prototype.
		return r.typedArrayIndex(o, key).numeric
	}
	return false
}

// mappedArgument returns the parameter an index of a mapped arguments object
// aliases, or nil when there is none.
//
// The alias normally lives in dense storage, where getElem and setElem answer
// for it; this is for the property table, which an index redefined with
// attributes moves to.
func mappedArgument(o *Object, key Atom) *upvalue {
	if o.flags&objMappedArguments == 0 || !key.IsIndex() {
		return nil
	}
	return o.argumentBinding(key.Index())
}

// createOwnProp adds a new own data property, applying the exotic rules of
// arrays and respecting extensibility.
//
// The bool says whether the property was actually written, which Reflect.set
// reports and a sloppy assignment ignores; in strict mode a refusal is an error
// instead.
func (r *Runtime) createOwnProp(o *Object, key Atom, val Value, strict bool) (bool, error) {
	// A typed array's numeric writes go into its buffer. One that names no
	// element is dropped rather than added, which is what makes a typed array
	// fixed -- and that covers a["-0"] and a["1.5"] as much as a[5].
	if o.class == ClassTypedArray {
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			if !ix.valid {
				// The value is still coerced, which a valueOf can observe. The
				// write counts as having happened: a typed array owns every
				// numeric key, in range or not.
				_, err := r.toNumericForElement(o, val)
				return err == nil, err
			}
			err := r.setElem(o.data.(*typedArrayData), ix.i, val)
			return err == nil, err
		}
	}
	if o.class == ClassArray {
		if key == atomLength {
			// An array's length is synthesized rather than stored, so the walk
			// that would have found a non-writable property did not.
			if o.flags&objArrayLengthWritable == 0 {
				return false, r.assignFailed(key, strict,
					"cannot assign to read-only property %q")
			}
			// What follows is a define, conversions and all: the value is
			// coerced twice before anything is decided, and a value that
			// watches -- and makes the length non-writable while it is being
			// watched -- can tell.
			ok, err := r.defineArrayLength(o, &propDesc{value: val, hasValue: true})
			if err != nil || ok {
				return ok, err
			}
			return false, r.assignFailed(key, strict,
				"cannot assign to read-only property %q")
		}
		// The largest indices are spelled out rather than carried in the atom,
		// and they are indices of the array all the same.
		if ix, ok := r.atoms.arrayIndex(key); ok {
			if !o.IsExtensible() && int64(ix) >= int64(len(o.elems)) {
				if strict {
					return false, r.throwTypeError("cannot add a property to a non-extensible array")
				}
				return false, nil
			}
			if o.setElem(ix, val) {
				return true, nil
			}
			// Too sparse for dense storage; fall through to an ordinary
			// property and remember that the array is no longer dense.
			o.noteArrayIndex(ix)
		}
	} else if key.IsIndex() && int(key.Index()) <= len(o.elems) && len(o.elems) > 0 {
		// A non-array that already has dense storage keeps using it.
		if o.setElem(key.Index(), val) {
			return true, nil
		}
	}

	// A string wrapper's length and its characters are synthesized too, and
	// none of them may be written.
	if o.class == ClassStringWrapper {
		if s, ok := o.data.(*String); ok {
			if key == atomLength || (key.IsIndex() && int(key.Index()) < s.Len()) {
				return false, r.assignFailed(key, strict,
					"cannot assign to read-only property %q")
			}
		}
	}

	if !o.IsExtensible() {
		if strict {
			return false, r.throwTypeError("cannot add property %q to a non-extensible object",
				r.atoms.name(key))
		}
		return false, nil
	}
	o.setOwnRaw(key, val, propDefault)
	return true, nil
}

// setValueProp assigns through a value.
//
// A primitive has nowhere of its own to put a property, but the chain its
// wrapper inherits may have a setter for the name, and that setter runs -- with
// the primitive itself as the receiver, not the wrapper the walk went through.
// Only when nothing on the chain takes the value does the assignment fail,
// silently in sloppy mode and with a TypeError in strict.
func (r *Runtime) setValueProp(v Value, key Atom, val Value, strict bool) error {
	if v.IsObject() {
		_, err := r.setProp(v.Object(), key, val, v, strict)
		return err
	}
	if v.IsNullish() {
		return r.throwTypeError("cannot set property %q of %s",
			r.atoms.name(key), v.Kind())
	}
	o, err := r.toObject(v)
	if err != nil {
		return err
	}
	_, err = r.setProp(o, key, val, v, strict)
	return err
}

// hasProp implements the `in` operator, walking the prototype chain.
func (r *Runtime) hasProp(o *Object, key Atom) bool {
	// A trap may throw, which this signature cannot report. Callers that can
	// report use hasPropErr; this form is for the ones that cannot, where
	// treating a failed trap as absence is the only available answer.
	res, _ := r.hasPropErr(o, key)
	return res
}

// hasPropErr is hasProp with the error a proxy trap may produce.
//
// The `in` operator and Reflect.has both go through it, because a trap that
// violates an invariant has to be reported rather than turned into a false.
func (r *Runtime) hasPropErr(o *Object, key Atom) (bool, error) {
	if p := proxyOf(o); p != nil {
		return r.proxyHas(p, key)
	}
	if o.class == ClassTypedArray {
		// A numeric key is answered from the buffer and the walk stops there,
		// so a property of the same name on the prototype is not visible
		// through an instance.
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			return ix.valid, nil
		}
	}
	for ; o != nil; o = o.proto {
		if p := proxyOf(o); p != nil {
			return r.proxyHas(p, key)
		}
		if r.hasOwnProp(o, key) {
			return true, nil
		}
	}
	return false, nil
}

// hasOwnProp reports whether the object itself has the property.
func (r *Runtime) hasOwnProp(o *Object, key Atom) bool {
	if o.class == ClassTypedArray {
		// A numeric key is present exactly while it names an element, never in
		// the property table.
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			return ix.valid
		}
	}
	if key.IsIndex() {
		if _, ok := o.getElem(key.Index()); ok {
			return true
		}
		if o.class == ClassStringWrapper {
			if s, ok := o.data.(*String); ok && int(key.Index()) < s.Len() {
				return true
			}
		}
	}
	if key == atomLength {
		switch o.class {
		case ClassArray, ClassStringWrapper:
			return true
		case ClassFunction:
			if fd := o.fn(); fd == nil || !fd.propsMaterialized {
				return true
			}
		}
	}
	if key == atomName && o.class == ClassFunction {
		if fd := o.fn(); fd == nil || !fd.propsMaterialized {
			return true
		}
	}
	if key == atomPrototype && o.class == ClassFunction {
		if fd := o.fn(); fd != nil && fd.protoPending {
			return true
		}
	}
	return o.getOwnVisible(key) != nil
}

// deleteProp implements the delete operator.
func (r *Runtime) deleteProp(o *Object, key Atom, strict bool) (bool, error) {
	if p := proxyOf(o); p != nil {
		return r.proxyDelete(p, key, strict)
	}
	if o.class == ClassTypedArray {
		// An element cannot be removed: the buffer has a slot for it whatever
		// the script says. A numeric key that names no element is absent
		// already, so deleting it succeeds trivially.
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			if ix.valid {
				if strict {
					return false, r.throwTypeError(
						"cannot delete an element of a typed array")
				}
				return false, nil
			}
			return true, nil
		}
	}
	// A property a class synthesizes rather than stores cannot be removed: an
	// array's length and a string wrapper's characters are all there for as
	// long as the object is.
	if (o.class == ClassArray && key == atomLength) ||
		(o.class == ClassStringWrapper && r.hasExoticOwn(o, key)) {
		return false, r.assignFailed(key, strict,
			"cannot delete non-configurable property %q")
	}
	if key.IsIndex() {
		i := key.Index()
		if int(i) < len(o.elems) && !isHole(o.elems[i]) {
			// Deleting leaves a hole wherever it happens, including at the
			// end: an array's length is a number rather than a count of what
			// is present, so `delete a[a.length - 1]` does not shorten it.
			o.elems[i] = elemHole
			// A deleted index no longer names a parameter.
			o.unmapArgument(i)
			return true, nil
		}
	}
	// A function's name and length are configurable, so they can be deleted --
	// but only once they exist as real properties rather than as something
	// synthesized on every read.
	r.materializeFunctionProp(o, key)
	ok := o.deleteOwn(key)
	if !ok && strict {
		return false, r.throwTypeError("cannot delete non-configurable property %q",
			r.atoms.name(key))
	}
	return ok, nil
}

// defineOwnProp implements Object.defineProperty for a data property.
func (r *Runtime) defineOwnProp(o *Object, key Atom, val Value, flags propFlags) error {
	// A function's name and length are synthesized, and a define that lands on
	// one has to find it where it would have been rather than append it.
	r.materializeFunctionProp(o, key)
	if o.flags&objMappedArguments != 0 && key.IsIndex() {
		i := key.Index()
		if u := o.argumentBinding(i); u != nil {
			// The parameter takes the value whatever the attributes say, and
			// keeps the alias unless the property stops being writable: a
			// read-only property is no longer the parameter.
			u.set(val)
			if flags&propWritable == 0 {
				o.unmapArgument(i)
			}
		}
	}
	// Dense storage is where an ordinary element goes -- unless this index is
	// already in the property table, where an earlier define with attributes
	// left it. Writing to the slot as well would leave the index described
	// twice, and deleting it would remove only one of the two.
	if o.class == ClassArray && key.IsIndex() && flags == propDefault &&
		(o.flags&objHasSparseElements == 0 || o.getOwn(key) == nil) {
		if o.setElem(key.Index(), val) {
			return nil
		}
		o.markSparse()
	}
	// A dense element being redefined with non-default attributes must move out
	// of dense storage, which cannot express attributes.
	if key.IsIndex() && int(key.Index()) < len(o.elems) && flags != propDefault {
		i := key.Index()
		o.markSparse()
		o.elems[i] = elemHole
	}
	o.setOwnRaw(key, val, flags)
	return nil
}

// defineAccessor installs a getter or setter half, merging with an existing
// accessor for the same key so that `get x` and `set x` combine.
func (r *Runtime) defineAccessor(o *Object, key Atom, getter, setter *Object, flags propFlags) {
	flags |= propAccessor
	// A function's name and length are synthesized, and an accessor replacing
	// one belongs where it would have been.
	r.materializeFunctionProp(o, key)
	// Dense storage holds plain values and cannot express an accessor, so an
	// index being turned into one has to leave it. Everything that reads a
	// dense element takes it as a data property with default attributes, which
	// would otherwise shadow the accessor being defined here.
	if key.IsIndex() && int(key.Index()) < len(o.elems) {
		o.markSparse()
		o.elems[key.Index()] = elemHole
		// An accessor is not the parameter it replaced.
		o.unmapArgument(key.Index())
	}
	if p := o.getOwnVisible(key); p != nil && p.isAccessor() {
		if a := p.getterSetter(); a != nil {
			if getter != nil {
				a.getter = getter
			}
			if setter != nil {
				a.setter = setter
			}
			p.flags = flags
			return
		}
	}
	a := &accessor{getter: getter, setter: setter}
	o.setOwnRaw(key, Value{num: mkTag(KindObject, 0), ref: a}, flags)
}

// toArrayLength converts a value assigned to an array's length, rejecting
// anything that is not an exact unsigned 32-bit integer.
func (r *Runtime) toArrayLength(v Value) (uint32, error) {
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	u := uint32(n)
	if float64(u) != n {
		return 0, r.throwRangeError("invalid array length")
	}
	return u, nil
}

// toElementValue converts a value to what a typed array's elements hold, which
// is a BigInt for the 64-bit integer kinds and a Number for the rest.
//
// A method that writes one value to many elements converts it once, which a
// valueOf that counts its calls can see.
func (r *Runtime) toElementValue(t *typedArrayData, v Value) (Value, error) {
	if t.info().big {
		b, err := r.toBigIntOperand(v)
		if err != nil {
			return Undefined, err
		}
		return Big(b), nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return Undefined, err
	}
	return Float(n), nil
}
