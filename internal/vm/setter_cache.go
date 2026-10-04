package vm

// A write site's cache that remembers a setter, as a read's remembers a
// getter: setPropCached calls these, which are kept out of the hot files
// (see the package comment) since only a write that reaches a setter needs
// them.

// setterNext is a write cache entry's next for a setter's entry: no shape,
// so that a check for a plain write, that next is nil, refuses it, and
// nothing mistakes it for an addition's.
var setterNext = &shape{}

// cachedSetter is a write of key to o by a setter its cache found, if the
// accessor is where it was: its setter, as it now is, is called with o as
// this. It reports false for one with no setter, or not found, which the
// long way is left to answer.
func (r *Runtime) cachedSetter(c *propCache, o *Object, key Atom, v Value) (bool, error) {
	h := c.holder(o)
	if h == nil {
		return false, nil
	}
	if verifyShapes {
		checkShape(o)
		checkShape(h)
		if p := &h.props[c.idx]; p.key != key || p.flags&propAccessor == 0 {
			panic("write cache's accessor is not where it was for " + r.atoms.name(key))
		}
	}
	a := h.props[c.idx].getterSetter()
	if a == nil || a.setter == nil {
		return false, nil
	}
	return true, r.callSetter(a.setter, o, v)
}

// fillSetterCache remembers, after a write of key to o that left its layout
// before as it was, the setter the write may have called: the read cache's
// walk finds one as it finds a getter. It reports whether it did; anything
// else the walk finds is not kept.
func (r *Runtime) fillSetterCache(c *propCache, o *Object, key Atom, before *shape) bool {
	saved := *c
	r.fillPropCache(c, o, key, true)
	if c.getter && c.shape == before {
		c.next = setterNext
		return true
	}
	*c = saved
	return false
}

// callSetter calls a setter with o as this and v, from the runtime's
// argument stack rather than a list made for the call.
func (r *Runtime) callSetter(setter, o *Object, v Value) error {
	i := len(r.argStack)
	r.argStack = append(r.argStack, v)
	_, err := r.callFromLoop(Obj(setter), Obj(o), r.argStack[i:i+1:i+1])
	r.argStack[i] = Undefined
	r.argStack = r.argStack[:i]
	return err
}
