package vm

// The `with` statement.
//
// Inside a `with` body an unqualified name may resolve to a property of the
// object rather than to any binding: what `x` means depends on what the object
// holds at the moment the name is evaluated. Nothing else in the language works
// that way, which is why `with` is forbidden in strict mode and why nothing
// else here has to pay for it.
//
// The cost is confined to the body. Every name used inside one compiles to a
// probe followed by the instruction that would have been emitted anyway: the
// probe walks the enclosing `with` objects, and either answers and jumps over
// the static instruction or falls through to it. Code outside a `with` is
// unchanged, and there is no `with` in strict mode, in a module, or in a class.
//
// A function created inside a `with` body keeps the objects, because the names
// in its own body resolve against them too -- so the chain is copied into the
// closure rather than read from the frame, which is gone by the time the
// closure runs.

// withFound reports whether a name resolves against an object in the chain, and
// to which object.
//
// Symbol.unscopables is how an object hides a name from `with`: Array.prototype
// carries one listing `keys`, `values` and the rest, so that adding a method to
// it does not change what an unqualified `values` means inside
// `with (someArray)`.
func (r *Runtime) withFound(scopes []*Object, name Atom, limit int) (*Object, bool, error) {
	// Only the innermost limit objects are consulted: a binding declared
	// between two `with` statements is shadowed by the inner one and not by
	// the outer, and the compiler works out how many that leaves.
	if limit < len(scopes) {
		scopes = scopes[len(scopes)-limit:]
	}
	for i := len(scopes) - 1; i >= 0; i-- {
		o := scopes[i]
		has, err := r.hasPropErr(o, name)
		if err != nil {
			return nil, false, err
		}
		if !has {
			continue
		}
		blocked, err := r.unscopable(o, name)
		if err != nil {
			return nil, false, err
		}
		if blocked {
			continue
		}
		return o, true, nil
	}
	return nil, false, nil
}

// withRead reads a binding through a `with` object.
//
// It asks again whether the property is there, because deciding that the object
// answers for the name runs user code -- the unscopables getter, or a proxy's
// traps -- and the property may be gone by the time the read happens. A binding
// that vanished reads as undefined in sloppy code and is a ReferenceError in
// strict code, which is what an environment record does with one.
func (r *Runtime) withRead(o *Object, name Atom, strict bool) (Value, error) {
	has, err := r.hasPropErr(o, name)
	if err != nil {
		return Undefined, err
	}
	if !has {
		if strict {
			return Undefined, r.throwReferenceError("%s is not defined", r.atoms.name(name))
		}
		return Undefined, nil
	}
	return r.getProp(o, name, Obj(o))
}

// withWrite assigns to a binding through a `with` object, asking the same
// question first for the same reason.
func (r *Runtime) withWrite(o *Object, name Atom, v Value, strict bool) error {
	has, err := r.hasPropErr(o, name)
	if err != nil {
		return err
	}
	if !has && strict {
		return r.throwReferenceError("%s is not defined", r.atoms.name(name))
	}
	_, err = r.setProp(o, name, v, Obj(o), strict)
	return err
}

// unscopable reports whether an object's Symbol.unscopables hides a name.
func (r *Runtime) unscopable(o *Object, name Atom) (bool, error) {
	key := r.atoms.internSymbol(r.wellKnown.unscopables)
	v, err := r.getProp(o, key, Obj(o))
	if err != nil {
		return false, err
	}
	if !v.IsObject() {
		return false, nil
	}
	blocked, err := r.getValueProp(v, name)
	if err != nil {
		return false, err
	}
	return blocked.Truthy(), nil
}

// withObject coerces the operand of a `with` statement.
func (r *Runtime) withObject(v Value) (*Object, error) {
	if v.IsNullish() {
		return nil, r.throwTypeError("cannot use %s as a with scope", v.Kind())
	}
	return r.toObject(v)
}
