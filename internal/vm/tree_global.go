package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// getGlobalAt is get_global for the tree tier: what the interpreter's case
// does, in the same order.
func (r *Runtime) getGlobalAt(c *tctx, in bytecode.Instr, pc int) (Value, error) {
	cl, f := c.cl, c.f
	name := cl.names[in.A]
	env := cl.scope()
	if f.evalVars != nil {
		if p := evalVarProp(f.evalVars, name); p != nil {
			return p.value, nil
		}
	}
	if r.lexShadows(name) {
		if p := r.globalLexProp(env, name); p != nil {
			if p.value.IsUninitialized() {
				c.at(pc)
				return Undefined, r.throwReferenceError(
					"cannot access %q before it is initialized", r.atoms.name(name))
			}
			return p.value, nil
		}
	}
	site := &cl.ic[in.B]
	if i := uint(site.idx); i < uint(len(env.props)) {
		if p := &env.props[i]; p.key == name &&
			p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
			return p.value, nil
		}
	}
	if x := env.shapeIndex(); x != nil {
		if e := &x.recent[name&15]; e.key == name && e.idx >= 0 && int(e.idx) < len(env.props) {
			if p := &env.props[e.idx]; p.key == name &&
				p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
				site.idx = e.idx
				return p.value, nil
			}
		}
	}
	if i := env.findOwn(name); i >= 0 {
		if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
			site.idx = i
			return p.value, nil
		}
	}
	c.at(pc)
	if !r.isGlobalScope(env) {
		if p := r.moduleLexProp(env, name); p != nil {
			if p.flags&propUninit != 0 {
				return Undefined, r.throwReferenceError(
					"cannot access %q before it is initialized", r.atoms.name(name))
			}
			if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
				return p.value, nil
			}
		}
	}
	if chainHasProxy(env) {
		has, err := r.hasPropErr(env, name)
		if err != nil {
			return Undefined, err
		}
		if !has {
			return Undefined, r.throwReferenceError("%s is not defined", r.atoms.name(name))
		}
	}
	v, err := r.getProp(env, name, Obj(env))
	if err != nil {
		return Undefined, err
	}
	if v.IsUndefined() && !chainHasProxy(env) && !r.hasProp(env, name) {
		return Undefined, r.throwReferenceError("%s is not defined", r.atoms.name(name))
	}
	return v, nil
}

// setGlobalAt is set_global for the tree tier: what the interpreter's case
// does, in the same order.
func (r *Runtime) setGlobalAt(c *tctx, in bytecode.Instr, v Value, pc int) error {
	cl, f := c.cl, c.f
	name := cl.names[in.A]
	env := cl.scope()
	if f.evalVars != nil {
		if p := evalVarProp(f.evalVars, name); p != nil {
			p.value = v
			return nil
		}
	}
	c.at(pc)
	if p := r.globalLexProp(env, name); p != nil {
		switch {
		case p.value.IsUninitialized():
			return r.throwReferenceError(
				"cannot access %q before it is initialized", r.atoms.name(name))
		case p.flags&propWritable == 0:
			return r.throwTypeError("assignment to constant variable %q", r.atoms.name(name))
		}
		p.value = v
		return nil
	}
	if !r.isGlobalScope(env) {
		if p := r.moduleLexProp(env, name); p != nil {
			switch {
			case p.flags&propUninit != 0:
				return r.throwReferenceError(
					"cannot access %q before it is initialized", r.atoms.name(name))
			case p.flags&(propWritable|propAccessor) == 0:
				return r.throwTypeError("assignment to constant variable %q", r.atoms.name(name))
			}
			if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
				p.value = v
				return nil
			}
		}
	}
	if i := globalSlot(env, &cl.ic[in.B], name); i >= 0 {
		if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 &&
			p.flags&propWritable != 0 {
			p.value = v
			return nil
		}
	}
	if cl.fn.Strict && !(r.nodeQuirks && chainHasProxy(env)) {
		has, err := r.hasPropErr(env, name)
		if err != nil {
			return err
		}
		if !has {
			return r.throwReferenceError("%s is not defined", r.atoms.name(name))
		}
	}
	_, err := r.setProp(env, name, v, Obj(env), cl.fn.Strict)
	return err
}
