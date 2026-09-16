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
		// Dense elements come first, since an array index is the hottest key.
		if key.IsIndex() {
			if v, ok := o.getElem(key.Index()); ok {
				return v, nil
			}
			// A class with exotic index behaviour handles indices not present
			// in its dense storage.
			if v, ok, err := r.getExoticIndex(o, key.Index()); ok || err != nil {
				return v, err
			}
		} else if v, ok, err := r.getExoticNamed(o, key); ok || err != nil {
			return v, err
		}

		if p := o.getOwn(key); p != nil {
			if p.isAccessor() {
				a := p.getterSetter()
				if a == nil || a.getter == nil {
					// An accessor with no getter reads as undefined.
					return Undefined, nil
				}
				return r.call(Obj(a.getter), receiver, nil)
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
	case ClassFunction:
		// name and length are materialized on first read rather than created
		// with every function, since most functions are never asked.
		if fd := o.fn(); fd != nil {
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
	}
	return Undefined, false, nil
}

// getExoticIndex handles index reads that are not backed by dense storage.
func (r *Runtime) getExoticIndex(o *Object, idx uint32) (Value, bool, error) {
	if o.class == ClassStringWrapper {
		if s, ok := o.data.(*String); ok && int(idx) < s.Len() {
			return Str(s.Substring(int(idx), int(idx)+1)), true, nil
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
func (r *Runtime) setProp(obj *Object, key Atom, val Value, receiver Value, strict bool) error {
	// Walk the chain looking for an accessor or a non-writable data property,
	// either of which changes what a plain assignment does.
	for o := obj; o != nil; o = o.proto {
		if key.IsIndex() {
			if int(key.Index()) < len(o.elems) && !isHole(o.elems[key.Index()]) {
				// A dense element is always a writable data property, so the
				// assignment lands here.
				if o == obj {
					o.elems[key.Index()] = val
					return nil
				}
				break
			}
		}
		p := o.getOwn(key)
		if p == nil {
			continue
		}
		if p.isAccessor() {
			a := p.getterSetter()
			if a == nil || a.setter == nil {
				if strict {
					return r.throwTypeError("cannot assign to %q, which has only a getter",
						r.atoms.name(key))
				}
				return nil
			}
			_, err := r.call(Obj(a.setter), receiver, []Value{val})
			return err
		}
		if p.flags&propWritable == 0 {
			if strict {
				return r.throwTypeError("cannot assign to read-only property %q",
					r.atoms.name(key))
			}
			return nil
		}
		if o == obj {
			p.value = val
			return nil
		}
		// An inherited writable data property is shadowed by a new own
		// property rather than modified in place.
		break
	}

	// The assignment creates an own property on the receiver.
	target := obj
	if receiver.IsObject() && receiver.Object() != obj {
		target = receiver.Object()
	}
	return r.createOwnProp(target, key, val, strict)
}

// createOwnProp adds a new own data property, applying the exotic rules of
// arrays and respecting extensibility.
func (r *Runtime) createOwnProp(o *Object, key Atom, val Value, strict bool) error {
	if o.class == ClassArray {
		if key == atomLength {
			n, err := r.toArrayLength(val)
			if err != nil {
				return err
			}
			o.setArrayLength(n)
			return nil
		}
		if key.IsIndex() {
			if !o.IsExtensible() && int(key.Index()) >= len(o.elems) {
				if strict {
					return r.throwTypeError("cannot add a property to a non-extensible array")
				}
				return nil
			}
			if o.setElem(key.Index(), val) {
				return nil
			}
			// Too sparse for dense storage; fall through to an ordinary
			// property and remember that the array is no longer dense.
			o.flags |= objHasSparseElements
		}
	} else if key.IsIndex() && int(key.Index()) <= len(o.elems) && len(o.elems) > 0 {
		// A non-array that already has dense storage keeps using it.
		if o.setElem(key.Index(), val) {
			return nil
		}
	}

	if !o.IsExtensible() {
		if strict {
			return r.throwTypeError("cannot add property %q to a non-extensible object",
				r.atoms.name(key))
		}
		return nil
	}
	o.setOwnRaw(key, val, propDefault)
	return nil
}

// setValueProp assigns through a value, which is a no-op on a primitive in
// sloppy mode and a TypeError in strict mode.
func (r *Runtime) setValueProp(v Value, key Atom, val Value, strict bool) error {
	if v.IsObject() {
		return r.setProp(v.Object(), key, val, v, strict)
	}
	if v.IsNullish() {
		return r.throwTypeError("cannot set property %q of %s",
			r.atoms.name(key), v.Kind())
	}
	if strict {
		return r.throwTypeError("cannot create property %q on %s",
			r.atoms.name(key), v.Kind())
	}
	return nil
}

// hasProp implements the `in` operator, walking the prototype chain.
func (r *Runtime) hasProp(o *Object, key Atom) bool {
	for ; o != nil; o = o.proto {
		if r.hasOwnProp(o, key) {
			return true
		}
	}
	return false
}

// hasOwnProp reports whether the object itself has the property.
func (r *Runtime) hasOwnProp(o *Object, key Atom) bool {
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
			return true
		}
	}
	if key == atomName && o.class == ClassFunction {
		return true
	}
	return o.getOwn(key) != nil
}

// deleteProp implements the delete operator.
func (r *Runtime) deleteProp(o *Object, key Atom, strict bool) (bool, error) {
	if key.IsIndex() {
		i := key.Index()
		if int(i) < len(o.elems) && !isHole(o.elems[i]) {
			// Deleting from the middle leaves a hole; deleting the last
			// element shortens the dense storage.
			if int(i) == len(o.elems)-1 {
				o.elems = o.elems[:i]
			} else {
				o.elems[i] = elemHole
			}
			return true, nil
		}
	}
	ok := o.deleteOwn(key)
	if !ok && strict {
		return false, r.throwTypeError("cannot delete non-configurable property %q",
			r.atoms.name(key))
	}
	return ok, nil
}

// defineOwnProp implements Object.defineProperty for a data property.
func (r *Runtime) defineOwnProp(o *Object, key Atom, val Value, flags propFlags) error {
	if o.class == ClassArray && key.IsIndex() && flags == propDefault {
		if o.setElem(key.Index(), val) {
			return nil
		}
		o.flags |= objHasSparseElements
	}
	// A dense element being redefined with non-default attributes must move out
	// of dense storage, which cannot express attributes.
	if key.IsIndex() && int(key.Index()) < len(o.elems) && flags != propDefault {
		i := key.Index()
		o.elems[i] = elemHole
		o.flags |= objHasSparseElements
	}
	o.setOwnRaw(key, val, flags)
	return nil
}

// defineAccessor installs a getter or setter half, merging with an existing
// accessor for the same key so that `get x` and `set x` combine.
func (r *Runtime) defineAccessor(o *Object, key Atom, getter, setter *Object, flags propFlags) {
	flags |= propAccessor
	if p := o.getOwn(key); p != nil && p.isAccessor() {
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
