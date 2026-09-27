package vm

// Contextified realms, as node:vm makes them.
//
// A context is a realm whose global names are an object the host supplied --
// the sandbox -- before they are the realm's own. A name the sandbox has is
// read from it, whatever the realm's global object says; a name assigned or
// declared with var or function is written to it; and the realm's built-ins
// are there behind it for every name the sandbox does not have. The realm's
// let, const and class declarations stay the realm's, as a script's always
// do, rather than becoming the sandbox's properties.
//
// What the code in the context sees as its global object -- globalThis, a
// sloppy function's this, the scope names resolve against -- is a proxy of the
// realm's global object whose behavior is this forwarding, carried out here
// rather than by a handler. Unlike a script's proxy it answers without
// invariant checks: the sandbox is not its target, and the two may disagree
// about a property in ways no handler could be allowed to report.

// Contextify makes realm re a context of sandbox: its global names are the
// sandbox's, and then its own.
func (r *Runtime) Contextify(re *Realm, sandbox *Object) {
	scope := newObject(nil, ClassProxy)
	scope.data = &proxyData{target: re.global, sandbox: sandbox}
	re.scope = scope
	re.globalThis = Obj(scope)
	re.global.setOwnRaw(r.atoms.intern("globalThis"), Obj(scope), propWritable|propConfigurable)
	re.sandbox = sandbox
}

// Sandbox returns the object a context's global names are, or nil for a
// realm that is not a context.
func (re *Realm) Sandbox() *Object { return re.sandbox }

// globalScope is the object global code resolves names against: the global
// object, or for a context the proxy that forwards to its sandbox.
func (re *Realm) globalScope() *Object {
	if re.scope != nil {
		return re.scope
	}
	return re.global
}

// isGlobalScope reports whether env is the current realm's global scope
// rather than a module's environment.
func (r *Runtime) isGlobalScope(env *Object) bool {
	return env == r.global || (r.scope != nil && env == r.scope)
}

// sandboxGet reads a name through a context's global: the sandbox's, if it
// has one, and otherwise the realm's.
func (r *Runtime) sandboxGet(p *proxyData, key Atom, receiver Value) (Value, error) {
	has, err := r.hasPropErr(p.sandbox, key)
	if err != nil {
		return Undefined, err
	}
	if has {
		return r.getProp(p.sandbox, key, Obj(p.sandbox))
	}
	return r.getProp(p.target, key, receiver)
}

// sandboxSet writes a name through a context's global, which writes the
// sandbox -- unless the realm's global holds the name read-only, as it holds
// undefined, NaN and Infinity, which no context may shadow by assignment.
func (r *Runtime) sandboxSet(p *proxyData, key Atom, val Value, strict bool) (bool, error) {
	if own := p.target.getOwn(key); own != nil && !own.isAccessor() && own.flags&propWritable == 0 {
		if has, err := r.hasPropErr(p.sandbox, key); err != nil || !has {
			return false, err
		}
	}
	return r.setProp(p.sandbox, key, val, Obj(p.sandbox), strict)
}

// sandboxHas reports whether a context's global has a name: the sandbox or
// the realm's global object.
func (r *Runtime) sandboxHas(p *proxyData, key Atom) (bool, error) {
	has, err := r.hasPropErr(p.sandbox, key)
	if err != nil || has {
		return has, err
	}
	return r.hasPropErr(p.target, key)
}

// sandboxDelete deletes a name from both the sandbox and the realm's global,
// so that it is gone from the context rather than uncovered.
func (r *Runtime) sandboxDelete(p *proxyData, key Atom) (bool, error) {
	fromSandbox, err := r.deleteProp(p.sandbox, key, false)
	if err != nil {
		return false, err
	}
	fromGlobal, err := r.deleteProp(p.target, key, false)
	if err != nil {
		return false, err
	}
	return fromSandbox && fromGlobal, nil
}

// sandboxOwnKeys lists a context's global names: the realm's global object's
// own, then the sandbox's it does not have, which is the order node lists
// them in.
func (r *Runtime) sandboxOwnKeys(p *proxyData) ([]Atom, error) {
	keys, err := r.ownKeysOf(p.target, true)
	if err != nil {
		return nil, err
	}
	seen := make(map[Atom]bool, len(keys))
	for _, k := range keys {
		seen[k] = true
	}
	own, err := r.ownKeysOf(p.sandbox, true)
	if err != nil {
		return nil, err
	}
	for _, k := range own {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// sandboxOwnDescriptor describes a context's global name: the sandbox's own
// property, or else the realm's.
func (r *Runtime) sandboxOwnDescriptor(p *proxyData, key Atom) (Value, error) {
	d, err := r.ownDescriptorOf(p.sandbox, key)
	if err != nil || !d.IsUndefined() {
		return d, err
	}
	return r.ownDescriptorOf(p.target, key)
}

// sandboxDefine defines a name on a context's global, which defines it on the
// sandbox.
func (r *Runtime) sandboxDefine(p *proxyData, key Atom, desc Value) (bool, error) {
	if pp := proxyOf(p.sandbox); pp != nil {
		return r.proxyDefineProperty(pp, key, desc)
	}
	d, err := r.toDescriptor(desc)
	if err != nil {
		return false, err
	}
	return r.defineProperty(p.sandbox, key, d)
}
