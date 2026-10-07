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
			if site.p1 != env {
				site.p1 = env
			}
			return p.value, nil
		}
	}
	if x := env.shapeIndex(); x != nil {
		if e := &x.recent[name&15]; e.key == name && e.idx >= 0 && int(e.idx) < len(env.props) {
			if p := &env.props[e.idx]; p.key == name &&
				p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
				noteGlobalSlot(site, env, e.idx)
				return p.value, nil
			}
		}
	}
	if i := env.findOwn(name); i >= 0 {
		if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
			noteGlobalSlot(site, env, i)
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
	c.at(pc)
	return r.setGlobalIn(c.f, c.cl, in, v)
}

// setGlobalIn stores v to the global name the set_global or
// set_global_strict in names, in frame f of closure cl.
func (r *Runtime) setGlobalIn(f *frame, cl *closure, in bytecode.Instr, v Value) error {
	name := cl.names[in.A]
	env := cl.scope()
	if f.evalVars != nil {
		if p := evalVarProp(f.evalVars, name); p != nil {
			p.value = v
			return nil
		}
	}
	var lex *Property
	if len(r.globalLex.props) != 0 {
		lex = r.globalLexProp(env, name)
	}
	if p := lex; p != nil {
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
		if s := &cl.ic[in.B]; s.p1 != env {
			s.p1 = env
		}
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

// globalRefResolves is strict mode's question before an assignment to the
// global name: whether it resolves to anything, which an assignment to a
// name that does not is an error. Each case is a way to find the name; the
// global object's own table, which is where a global variable is, is asked
// first. site is the check's cache site.
func (r *Runtime) globalRefResolves(f *frame, cl *closure, site uint32, name Atom) (bool, error) {
	env := cl.scope()
	switch {
	case hasOwnGlobal(env, &cl.ic[site], name):
	case f.evalVars != nil && evalVarProp(f.evalVars, name) != nil:
	case r.globalLexProp(env, name) != nil:
	case !r.isGlobalScope(env) && r.moduleLexProp(env, name) != nil:
	case r.nodeQuirks && chainHasProxy(env):
		// V8 asks a proxy on the global object's chain nothing before the
		// store, which it counts as finding the name.
	default:
		return r.hasPropErr(env, name)
	}
	return true, nil
}

// setGlobalStrict is set_global_strict: check_global_ref, assert_resolved
// and set_global for a value that ran no code, so asked one straight after
// the other.
func (r *Runtime) setGlobalStrict(f *frame, cl *closure, in bytecode.Instr, v Value) error {
	name := cl.names[in.A]
	found, err := r.globalRefResolves(f, cl, in.B, name)
	if err != nil {
		return err
	}
	if !found {
		return r.throwReferenceError("%s is not defined", r.atoms.name(name))
	}
	return r.setGlobalIn(f, cl, in, v)
}
