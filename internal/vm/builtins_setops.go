package vm

import "math"

// The Set operations: union, intersection, difference, symmetricDifference,
// isSubsetOf, isSupersetOf and isDisjointFrom.
//
// None of them requires the argument to be a Set. Anything with a numeric
// `size` and callable `has` and `keys` will do, which is what lets a Map or a
// user-defined collection stand in on the right-hand side. That contract is a
// set record, read once up front so that a getter cannot change the answer
// halfway through.
//
// Which side gets iterated is chosen by size, not by position: walking the
// smaller collection and probing the larger costs O(min) rather than O(max).
// The choice is observable, because probing calls the argument's `has` method,
// so it is part of the specification rather than an optimization.

// setRecord is the set-like contract an argument must satisfy.
type setRecord struct {
	obj  *Object
	size float64
	has  Value
	keys Value
}

// getSetRecord validates an argument as set-like.
func (r *Runtime) getSetRecord(v Value) (*setRecord, error) {
	if !v.IsObject() {
		return nil, r.throwTypeError("a set-like object is required")
	}
	o := v.Object()

	rawSize, err := r.getValueProp(v, r.atoms.intern("size"))
	if err != nil {
		return nil, err
	}
	// ToNumber rather than a type check, so a size of "3" is accepted, but a
	// size of undefined becomes NaN and is rejected below.
	size, err := r.toNumber(rawSize)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(size) {
		return nil, r.throwTypeError("the size of a set-like object must be a number")
	}
	size = math.Trunc(size)
	if size < 0 {
		return nil, r.throwRangeError("the size of a set-like object cannot be negative")
	}

	has, err := r.getValueProp(v, r.atoms.intern("has"))
	if err != nil {
		return nil, err
	}
	if !isCallable(has) {
		return nil, r.throwTypeError("a set-like object must have a callable has method")
	}
	keys, err := r.getValueProp(v, r.atoms.intern("keys"))
	if err != nil {
		return nil, err
	}
	if !isCallable(keys) {
		return nil, r.throwTypeError("a set-like object must have a callable keys method")
	}
	return &setRecord{obj: o, size: size, has: has, keys: keys}, nil
}

// probe asks the other collection whether it holds a value.
func (r *Runtime) probe(rec *setRecord, v Value) (bool, error) {
	got, err := r.call(rec.has, Obj(rec.obj), []Value{v})
	if err != nil {
		return false, err
	}
	return got.Truthy(), nil
}

// keysCursor walks a set-like object's keys.
//
// It is stepped rather than drained because an operation that can answer early
// must stop asking: `isDisjointFrom` returns as soon as it finds a member in
// common, and the iterator is told so.
type keysCursor struct {
	iter Value
	next Value
}

// openKeys calls the other collection's keys method and prepares to walk what
// it returns.
func (r *Runtime) openKeys(rec *setRecord) (*keysCursor, error) {
	iter, err := r.call(rec.keys, Obj(rec.obj), nil)
	if err != nil {
		return nil, err
	}
	if !iter.IsObject() {
		return nil, r.throwTypeError("the keys method of a set-like object must return an iterator")
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return nil, err
	}
	if !isCallable(next) {
		return nil, r.throwTypeError("the keys iterator must have a next method")
	}
	return &keysCursor{iter: iter, next: next}, nil
}

// step produces the next key, reporting when there are no more.
func (c *keysCursor) step(r *Runtime) (Value, bool, error) {
	res, err := r.call(c.next, c.iter, nil)
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
		return Undefined, true, nil
	}
	v, err := r.getValueProp(res, atomValue)
	if err != nil {
		return Undefined, false, err
	}
	return normalizeZero(v), false, nil
}

// close tells the iterator that nothing more will be asked of it.
func (c *keysCursor) close(r *Runtime) error {
	if !c.iter.IsObject() {
		return nil
	}
	return r.closeIteratorErr(c.iter)
}

// keysOf drains the other collection's key iterator, for the operations that
// need every key before they can answer.
func (r *Runtime) keysOf(rec *setRecord) ([]Value, error) {
	c, err := r.openKeys(rec)
	if err != nil {
		return nil, err
	}
	return c.drain(r)
}

// drain collects what is left of a cursor.
func (c *keysCursor) drain(r *Runtime) ([]Value, error) {
	var out []Value
	for {
		v, done, err := c.step(r)
		if err != nil || done {
			return out, err
		}
		out = append(out, v)
	}
}

// normalizeZero folds -0 to +0, which is how a Set stores it.
func normalizeZero(v Value) Value {
	if v.IsNumber() && v.Number() == 0 {
		return Int(0)
	}
	return v
}

// liveEntries returns the receiver's current members in insertion order.
//
// A fresh slice is taken because every operation may call user code that
// mutates the receiver, and the specification fixes the members at the start.
func liveEntries(m *jsMap) []Value {
	out := make([]Value, 0, m.size)
	for i := range m.entries {
		if !m.entries[i].deleted {
			out = append(out, m.entries[i].key)
		}
	}
	return out
}

// newSetFrom builds a Set holding the given values, skipping duplicates.
func (r *Runtime) newSetFrom(vals []Value) *Object {
	m := newJSMap(false)
	for _, v := range vals {
		m.set(r, v, v)
	}
	o := newObject(r.proto.setProto, ClassSet)
	o.data = m
	return o
}

func (r *Runtime) initSetOps(p *Object) {
	r.defMethod(p, "union", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.union")
		if err != nil {
			return Undefined, err
		}
		cur, err := rt.openKeys(rec)
		if err != nil {
			return Undefined, err
		}
		// The receiver's members are taken before the argument is walked --
		// its iterator may change the receiver -- and they come first, so the
		// result keeps its order and the argument's new members follow.
		mine := liveEntries(m)
		other, err := cur.drain(rt)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newSetFrom(append(mine, other...))), nil
	})

	r.defMethod(p, "intersection", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.intersection")
		if err != nil {
			return Undefined, err
		}
		var out []Value
		if float64(m.size) <= rec.size {
			// The receiver is smaller: walk it and probe the argument. The
			// result is in the receiver's order.
			for _, v := range liveEntries(m) {
				in, err := rt.probe(rec, v)
				if err != nil {
					return Undefined, err
				}
				if in {
					out = append(out, v)
				}
			}
		} else {
			// The argument is smaller: walk it instead. The result is then in
			// the argument's order, which the specification makes observable.
			keys, err := rt.keysOf(rec)
			if err != nil {
				return Undefined, err
			}
			for _, v := range keys {
				if _, ok := m.get(rt, v); ok {
					out = append(out, v)
				}
			}
		}
		return Obj(rt.newSetFrom(out)), nil
	})

	r.defMethod(p, "difference", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.difference")
		if err != nil {
			return Undefined, err
		}
		mine := liveEntries(m)
		if float64(m.size) <= rec.size {
			var out []Value
			for _, v := range mine {
				in, err := rt.probe(rec, v)
				if err != nil {
					return Undefined, err
				}
				if !in {
					out = append(out, v)
				}
			}
			return Obj(rt.newSetFrom(out)), nil
		}
		// Removing the argument's members from a copy of the receiver is
		// cheaper when the argument is the smaller of the two.
		result := rt.newSetFrom(mine)
		keys, err := rt.keysOf(rec)
		if err != nil {
			return Undefined, err
		}
		rm := result.data.(*jsMap)
		for _, v := range keys {
			rm.delete(rt, v)
		}
		return Obj(result), nil
	})

	r.defMethod(p, "symmetricDifference", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.symmetricDifference")
		if err != nil {
			return Undefined, err
		}
		cur, err := rt.openKeys(rec)
		if err != nil {
			return Undefined, err
		}
		// The result starts as the receiver was when the walk began; whether a
		// key is in the receiver is asked of it as it is then, which the
		// iterator may have changed.
		result := rt.newSetFrom(liveEntries(m))
		rm := result.data.(*jsMap)
		for {
			v, done, err := cur.step(rt)
			if err != nil {
				return Undefined, err
			}
			if done {
				break
			}
			if _, ok := m.get(rt, v); ok {
				rm.delete(rt, v)
			} else {
				rm.set(rt, v, v)
			}
		}
		return Obj(result), nil
	})

	r.defMethod(p, "isSubsetOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.isSubsetOf")
		if err != nil {
			return Undefined, err
		}
		// A larger set cannot be a subset, and checking the sizes first avoids
		// calling the argument's has method at all.
		if float64(m.size) > rec.size {
			return False, nil
		}
		// The receiver is walked as it is at each step rather than from a copy:
		// the argument's has method may remove a member before it is reached.
		for i := 0; i < len(m.entries); i++ {
			if m.entries[i].deleted {
				continue
			}
			in, err := rt.probe(rec, m.entries[i].key)
			if err != nil {
				return Undefined, err
			}
			if !in {
				return False, nil
			}
		}
		return True, nil
	})

	r.defMethod(p, "isSupersetOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.isSupersetOf")
		if err != nil {
			return Undefined, err
		}
		if float64(m.size) < rec.size {
			return False, nil
		}
		cur, err := rt.openKeys(rec)
		if err != nil {
			return Undefined, err
		}
		for {
			v, done, err := cur.step(rt)
			if err != nil {
				return Undefined, err
			}
			if done {
				return True, nil
			}
			if _, ok := m.get(rt, v); !ok {
				// The answer is settled, so the iterator is told to stop
				// rather than being walked to the end.
				if err := cur.close(rt); err != nil {
					return Undefined, err
				}
				return False, nil
			}
		}
	})

	r.defMethod(p, "isDisjointFrom", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, rec, err := rt.setPair(this, args, "Set.prototype.isDisjointFrom")
		if err != nil {
			return Undefined, err
		}
		if float64(m.size) <= rec.size {
			for i := 0; i < len(m.entries); i++ {
				if m.entries[i].deleted {
					continue
				}
				in, err := rt.probe(rec, m.entries[i].key)
				if err != nil {
					return Undefined, err
				}
				if in {
					return False, nil
				}
			}
			return True, nil
		}
		cur, err := rt.openKeys(rec)
		if err != nil {
			return Undefined, err
		}
		for {
			v, done, err := cur.step(rt)
			if err != nil {
				return Undefined, err
			}
			if done {
				return True, nil
			}
			if _, ok := m.get(rt, v); ok {
				if err := cur.close(rt); err != nil {
					return Undefined, err
				}
				return False, nil
			}
		}
	})
}

// setPair validates both operands of a set operation.
//
// The receiver is checked before the argument, so calling one of these on a
// non-Set reports that rather than complaining about the argument.
func (r *Runtime) setPair(this Value, args []Value, name string) (*jsMap, *setRecord, error) {
	m, err := r.mapOf(this, ClassSet, name)
	if err != nil {
		return nil, nil, err
	}
	rec, err := r.getSetRecord(arg(args, 0))
	if err != nil {
		return nil, nil, err
	}
	return m, rec, nil
}
