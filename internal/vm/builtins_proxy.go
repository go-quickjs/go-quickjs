package vm

// Proxy and Reflect.
//
// A proxy intercepts the internal methods of an object. Every property path in
// the engine therefore has to ask, before doing anything else, whether the
// object it is about to operate on is a proxy -- which is why proxyData is
// checked at the top of getProp, setProp and their relatives rather than being
// handled as an exotic class alongside Array.
//
// Reflect exposes the same internal methods as ordinary functions. It exists
// mainly so that a proxy trap can forward to the default behaviour without
// reimplementing it, which is why the two are defined together.

// proxyData holds a proxy's target and handler.
type proxyData struct {
	target  *Object
	handler *Object
	// revoked proxies throw on every operation, which is what the revoke
	// function produced by Proxy.revocable turns on.
	revoked bool
}

// proxyOf returns an object's proxy state, or nil if it is not a proxy.
func proxyOf(o *Object) *proxyData {
	if o == nil || o.class != ClassProxy {
		return nil
	}
	p, _ := o.data.(*proxyData)
	return p
}

// trap looks up a handler method, reporting whether the proxy defines one.
func (r *Runtime) trap(p *proxyData, name string) (Value, bool, error) {
	if p.revoked {
		return Undefined, false, r.throwTypeError("cannot perform an operation on a revoked proxy")
	}
	v, err := r.getProp(p.handler, r.atoms.intern(name), Obj(p.handler))
	if err != nil {
		return Undefined, false, err
	}
	if v.IsNullish() {
		return Undefined, false, nil
	}
	if !isCallable(v) {
		return Undefined, false, r.throwTypeError("the %q trap is not a function", name)
	}
	return v, true, nil
}

// proxyGet implements the get trap.
func (r *Runtime) proxyGet(p *proxyData, key Atom, receiver Value) (Value, error) {
	fn, ok, err := r.trap(p, "get")
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return r.getProp(p.target, key, receiver)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), r.keyToValue(key), receiver,
	})
	if err != nil {
		return Undefined, err
	}
	// A property the target has promised not to change is one the trap may not
	// report differently: that promise is what other code is entitled to rely
	// on, and a proxy may not break it.
	cur, err := r.ownPropDesc(p.target, key)
	if err != nil || cur == nil || cur.configurable {
		return res, err
	}
	switch {
	case cur.isData() && !cur.writable && !res.SameValue(cur.value):
		return Undefined, r.throwTypeError(
			"the proxy \"get\" trap reported a different value for the "+
				"non-writable, non-configurable property %q", r.atoms.name(key))
	case cur.isAccessor() && cur.getter == nil && !res.IsUndefined():
		return Undefined, r.throwTypeError(
			"the proxy \"get\" trap reported a value for %q, which has no getter",
			r.atoms.name(key))
	}
	return res, nil
}

// proxySet implements the set trap.
func (r *Runtime) proxySet(p *proxyData, key Atom, val, receiver Value, strict bool) (bool, error) {
	fn, ok, err := r.trap(p, "set")
	if err != nil {
		return false, err
	}
	if !ok {
		// The receiver stays what it was: an ordinary set on the target ends
		// by defining the property on the receiver, which is this proxy, and
		// that goes through its traps again.
		return r.setProp(p.target, key, val, receiver, strict)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), r.keyToValue(key), val, receiver,
	})
	if err != nil {
		return false, err
	}
	// A set trap that reports failure is a TypeError in strict mode, which is
	// the only way an assignment learns it did not happen.
	if !res.Truthy() {
		if strict {
			return false, r.throwTypeError("the proxy \"set\" trap returned false for %q",
				r.atoms.name(key))
		}
		return false, nil
	}
	cur, err := r.ownPropDesc(p.target, key)
	if err != nil || cur == nil || cur.configurable {
		return err == nil, err
	}
	switch {
	case cur.isData() && !cur.writable && !val.SameValue(cur.value):
		return false, r.throwTypeError(
			"the proxy \"set\" trap claimed to write the non-writable, "+
				"non-configurable property %q", r.atoms.name(key))
	case cur.isAccessor() && cur.setter == nil:
		return false, r.throwTypeError(
			"the proxy \"set\" trap claimed to write %q, which has no setter",
			r.atoms.name(key))
	}
	return true, nil
}

// proxyHas implements the has trap.
func (r *Runtime) proxyHas(p *proxyData, key Atom) (bool, error) {
	fn, ok, err := r.trap(p, "has")
	if err != nil {
		return false, err
	}
	if !ok {
		return r.hasProp(p.target, key), nil
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target), r.keyToValue(key)})
	if err != nil {
		return false, err
	}
	if !res.Truthy() {
		// A property that cannot be deleted cannot be denied either.
		if prop := p.target.getOwnVisible(key); prop != nil {
			if prop.flags&propConfigurable == 0 || !p.target.IsExtensible() {
				return false, r.throwTypeError(
					"the proxy \"has\" trap denied the non-configurable property %q",
					r.atoms.name(key))
			}
		}
	}
	return res.Truthy(), nil
}

// proxyDelete implements the deleteProperty trap.
func (r *Runtime) proxyDelete(p *proxyData, key Atom, strict bool) (bool, error) {
	fn, ok, err := r.trap(p, "deleteProperty")
	if err != nil {
		return false, err
	}
	if !ok {
		return r.deleteProp(p.target, key, strict)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target), r.keyToValue(key)})
	if err != nil {
		return false, err
	}
	if !res.Truthy() {
		return false, nil
	}
	cur, err := r.ownPropDesc(p.target, key)
	if err != nil {
		return false, err
	}
	if cur == nil {
		return true, nil
	}
	if !cur.configurable {
		return false, r.throwTypeError(
			"the proxy \"deleteProperty\" trap deleted the non-configurable property %q",
			r.atoms.name(key))
	}
	// Nothing may be removed from a target that has been sealed against it.
	ext, err := r.isExtensibleOf(p.target)
	if err != nil {
		return false, err
	}
	if !ext {
		return false, r.throwTypeError(
			"the proxy \"deleteProperty\" trap deleted %q from a non-extensible target",
			r.atoms.name(key))
	}
	return true, nil
}

// proxyOwnKeys implements the ownKeys trap.
func (r *Runtime) proxyOwnKeys(p *proxyData) ([]Atom, error) {
	fn, ok, err := r.trap(p, "ownKeys")
	if err != nil {
		return nil, err
	}
	if !ok {
		return r.ownKeysOf(p.target, true)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target)})
	if err != nil {
		return nil, err
	}
	items, err := r.arrayToSlice(res)
	if err != nil {
		return nil, err
	}
	keys := make([]Atom, 0, len(items))
	seen := make(map[Atom]bool, len(items))
	for _, it := range items {
		// Only a property key is a property key: a number in the list is a
		// mistake rather than something to coerce.
		if !it.IsString() && !it.IsSymbol() {
			return nil, r.throwTypeError(
				"the proxy \"ownKeys\" trap must return strings and symbols")
		}
		k, err := r.toPropertyKey(it)
		if err != nil {
			return nil, err
		}
		if seen[k] {
			return nil, r.throwTypeError(
				"the proxy \"ownKeys\" trap returned %q twice", r.atoms.name(k))
		}
		seen[k] = true
		keys = append(keys, k)
	}

	// A key that cannot be deleted has to be listed, and a non-extensible
	// target's list has to be exactly its own.
	for _, k := range p.target.ownKeys(true, r.atoms) {
		if seen[k] {
			continue
		}
		if prop := p.target.getOwnVisible(k); prop != nil && prop.flags&propConfigurable == 0 {
			return nil, r.throwTypeError(
				"the proxy \"ownKeys\" trap omitted the non-configurable property %q",
				r.atoms.name(k))
		}
		if !p.target.IsExtensible() {
			return nil, r.throwTypeError(
				"the proxy \"ownKeys\" trap omitted a property of a non-extensible target")
		}
	}
	targetExt, err := r.isExtensibleOf(p.target)
	if err != nil {
		return nil, err
	}
	if !targetExt {
		own := make(map[Atom]bool)
		targetKeys, err := r.ownKeysOf(p.target, true)
		if err != nil {
			return nil, err
		}
		for _, k := range targetKeys {
			own[k] = true
		}
		for _, k := range keys {
			if !own[k] {
				return nil, r.throwTypeError(
					"the proxy \"ownKeys\" trap invented %q on a non-extensible target",
					r.atoms.name(k))
			}
		}
	}
	return keys, nil
}

// The invariants.
//
// A trap may lie, but not about anything a caller could already have observed
// and relied on. A non-configurable property cannot be made to disappear, a
// non-extensible object cannot gain one, and a prototype that cannot change
// cannot be reported as something else. Enforcing that is most of what
// separates a proxy from an object with clever getters, and is why each trap
// below compares its answer against the target before returning it.

// proxyGetPrototypeOf implements the getPrototypeOf trap.
func (r *Runtime) proxyGetPrototypeOf(p *proxyData) (Value, error) {
	fn, ok, err := r.trap(p, "getPrototypeOf")
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return r.protoOf(p.target)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target)})
	if err != nil {
		return Undefined, err
	}
	if !res.IsObject() && !res.IsNull() {
		return Undefined, r.throwTypeError(
			"the proxy \"getPrototypeOf\" trap must return an object or null")
	}
	// A target that can still change its prototype may be reported as
	// anything; one that cannot must be reported truthfully.
	ext, err := r.isExtensibleOf(p.target)
	if err != nil {
		return Undefined, err
	}
	if !ext {
		want, err := r.protoOf(p.target)
		if err != nil {
			return Undefined, err
		}
		if !res.SameValue(want) {
			return Undefined, r.throwTypeError(
				"the proxy \"getPrototypeOf\" trap disagrees with a non-extensible target")
		}
	}
	return res, nil
}

// proxySetPrototypeOf implements the setPrototypeOf trap.
func (r *Runtime) proxySetPrototypeOf(p *proxyData, proto Value) (bool, error) {
	fn, ok, err := r.trap(p, "setPrototypeOf")
	if err != nil {
		return false, err
	}
	if !ok {
		if pp := proxyOf(p.target); pp != nil {
			return r.proxySetPrototypeOf(pp, proto)
		}
		return setProtoOf(p.target, proto), nil
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target), proto})
	if err != nil {
		return false, err
	}
	if !res.Truthy() {
		return false, nil
	}
	ext, err := r.isExtensibleOf(p.target)
	if err != nil {
		return false, err
	}
	if !ext {
		want, err := r.protoOf(p.target)
		if err != nil {
			return false, err
		}
		if !proto.SameValue(want) {
			return false, r.throwTypeError(
				"the proxy \"setPrototypeOf\" trap changed the prototype of a non-extensible target")
		}
	}
	return true, nil
}

// proxyIsExtensible implements the isExtensible trap.
func (r *Runtime) proxyIsExtensible(p *proxyData) (bool, error) {
	fn, ok, err := r.trap(p, "isExtensible")
	if err != nil {
		return false, err
	}
	if !ok {
		return r.isExtensibleOf(p.target)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target)})
	if err != nil {
		return false, err
	}
	targetExt, err := r.isExtensibleOf(p.target)
	if err != nil {
		return false, err
	}
	// Extensibility is not something a proxy may misreport at all: a caller
	// that saw an object as sealed must not later see it as open.
	if res.Truthy() != targetExt {
		return false, r.throwTypeError(
			"the proxy \"isExtensible\" trap disagrees with its target")
	}
	return res.Truthy(), nil
}

// proxyPreventExtensions implements the preventExtensions trap.
func (r *Runtime) proxyPreventExtensions(p *proxyData) (bool, error) {
	fn, ok, err := r.trap(p, "preventExtensions")
	if err != nil {
		return false, err
	}
	if !ok {
		if pp := proxyOf(p.target); pp != nil {
			return r.proxyPreventExtensions(pp)
		}
		p.target.flags &^= objExtensible
		return true, nil
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target)})
	if err != nil {
		return false, err
	}
	targetExt, err := r.isExtensibleOf(p.target)
	if err != nil {
		return false, err
	}
	if res.Truthy() && targetExt {
		return false, r.throwTypeError(
			"the proxy \"preventExtensions\" trap reported success on an extensible target")
	}
	return res.Truthy(), nil
}

// proxyGetOwnPropertyDescriptor implements the getOwnPropertyDescriptor trap.
func (r *Runtime) proxyGetOwnPropertyDescriptor(p *proxyData, key Atom) (Value, error) {
	fn, ok, err := r.trap(p, "getOwnPropertyDescriptor")
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return r.ownDescriptorOf(p.target, key)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target), r.keyToValue(key)})
	if err != nil {
		return Undefined, err
	}
	if !res.IsObject() && !res.IsUndefined() {
		return Undefined, r.throwTypeError(
			"the proxy \"getOwnPropertyDescriptor\" trap must return an object or undefined")
	}
	target, err := r.ownPropDesc(p.target, key)
	if err != nil {
		return Undefined, err
	}
	ext, err := r.isExtensibleOf(p.target)
	if err != nil {
		return Undefined, err
	}
	if res.IsUndefined() {
		// A property that cannot be deleted cannot be hidden either, and one
		// cannot be removed from a non-extensible object.
		if target != nil && !target.configurable {
			return Undefined, r.throwTypeError(
				"the proxy \"getOwnPropertyDescriptor\" trap hid the non-configurable property %q",
				r.atoms.name(key))
		}
		if target != nil && !ext {
			return Undefined, r.throwTypeError(
				"the proxy \"getOwnPropertyDescriptor\" trap hid a property of a non-extensible target")
		}
		return Undefined, nil
	}
	if target == nil && !ext {
		return Undefined, r.throwTypeError(
			"the proxy \"getOwnPropertyDescriptor\" trap invented a property on a non-extensible target")
	}
	d, err := r.toDescriptor(res)
	if err != nil {
		return Undefined, err
	}
	completeDescriptor(d)
	// With no property on the target there is nothing to be incompatible with;
	// an extensible target may grow one.
	if target != nil && !r.validateRedefine(target, d) {
		return Undefined, r.throwTypeError(
			"the proxy \"getOwnPropertyDescriptor\" trap reported %q incompatibly "+
				"with the target", r.atoms.name(key))
	}
	if !d.configurable {
		// A property the trap calls permanent has to be one the target agrees
		// is permanent, or the promise is the proxy's to break.
		if target == nil || target.configurable {
			return Undefined, r.throwTypeError(
				"the proxy \"getOwnPropertyDescriptor\" trap reported %q as "+
					"non-configurable, which the target does not",
				r.atoms.name(key))
		}
		if d.hasWritable && !d.writable && target.isData() && target.writable {
			return Undefined, r.throwTypeError(
				"the proxy \"getOwnPropertyDescriptor\" trap reported %q as "+
					"non-writable, which the target does not", r.atoms.name(key))
		}
	}
	return res, nil
}

// proxyDefineProperty implements the defineProperty trap.
func (r *Runtime) proxyDefineProperty(p *proxyData, key Atom, desc Value) (bool, error) {
	fn, ok, err := r.trap(p, "defineProperty")
	if err != nil {
		return false, err
	}
	if !ok {
		if pp := proxyOf(p.target); pp != nil {
			return r.proxyDefineProperty(pp, key, desc)
		}
		return true, r.definePropertyFromDescriptor(p.target, key, desc)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{Obj(p.target), r.keyToValue(key), desc})
	if err != nil {
		return false, err
	}
	if !res.Truthy() {
		return false, nil
	}
	d, err := r.toDescriptor(desc)
	if err != nil {
		return false, err
	}
	target, err := r.ownPropDesc(p.target, key)
	if err != nil {
		return false, err
	}
	ext, err := r.isExtensibleOf(p.target)
	if err != nil {
		return false, err
	}
	settingPermanent := d.hasConfigurable && !d.configurable
	if target == nil {
		if !ext {
			return false, r.throwTypeError(
				"the proxy \"defineProperty\" trap added a property to a non-extensible target")
		}
		if settingPermanent {
			return false, r.throwTypeError(
				"the proxy \"defineProperty\" trap made %q non-configurable on a "+
					"target that does not have it", r.atoms.name(key))
		}
		return true, nil
	}
	if !r.validateRedefine(target, d) {
		return false, r.throwTypeError(
			"the proxy \"defineProperty\" trap redefined %q incompatibly with the target",
			r.atoms.name(key))
	}
	if settingPermanent && target.configurable {
		return false, r.throwTypeError(
			"the proxy \"defineProperty\" trap made the configurable property %q permanent",
			r.atoms.name(key))
	}
	if target.isData() && !target.configurable && target.writable &&
		d.hasWritable && !d.writable {
		return false, r.throwTypeError(
			"the proxy \"defineProperty\" trap made the writable, non-configurable "+
				"property %q read-only", r.atoms.name(key))
	}
	return true, nil
}

// ownPropDesc describes an object's own property, asking a proxy's trap when
// the object is one.
func (r *Runtime) ownPropDesc(o *Object, key Atom) (*propDesc, error) {
	pp := proxyOf(o)
	if pp == nil {
		return r.currentDescriptor(o, key)
	}
	v, err := r.proxyGetOwnPropertyDescriptor(pp, key)
	if err != nil || !v.IsObject() {
		return nil, err
	}
	d, err := r.toDescriptor(v)
	if err != nil {
		return nil, err
	}
	completeDescriptor(d)
	return d, nil
}

// isExtensibleOf reports whether an object is extensible, asking a proxy's trap
// when the object is one.
func (r *Runtime) isExtensibleOf(o *Object) (bool, error) {
	if pp := proxyOf(o); pp != nil {
		return r.proxyIsExtensible(pp)
	}
	return o.IsExtensible(), nil
}

// completeDescriptor fills in the fields a partial descriptor left out, which
// the invariant checks compare against.
func completeDescriptor(d *propDesc) {
	if d.isAccessor() {
		d.hasGet, d.hasSet = true, true
	} else {
		d.hasValue, d.hasWritable = true, true
	}
	d.hasEnumerable, d.hasConfigurable = true, true
}

// protoOf returns an object's prototype, asking a proxy's trap when the object
// is one.
func (r *Runtime) protoOf(o *Object) (Value, error) {
	if pp := proxyOf(o); pp != nil {
		return r.proxyGetPrototypeOf(pp)
	}
	return protoValue(o), nil
}

// protoValue renders an object's prototype as the null-or-object a trap sees.
func protoValue(o *Object) Value {
	if o.proto == nil {
		return Null
	}
	return Obj(o.proto)
}

// setProtoOf assigns a prototype, refusing when the object is sealed against it.
func setProtoOf(o *Object, proto Value) bool {
	if !o.IsExtensible() {
		return proto.SameValue(protoValue(o))
	}
	switch {
	case proto.IsObject():
		o.proto = proto.Object()
	case proto.IsNull():
		o.proto = nil
	default:
		return false
	}
	return true
}

// keyToValue converts an atom back to the string or symbol a trap receives.
func (r *Runtime) keyToValue(key Atom) Value {
	if sym := r.atoms.symbol(key); sym != nil {
		return Sym(sym)
	}
	return Str(NewString(r.atoms.name(key)))
}

// proxyCall implements the apply trap.
func (r *Runtime) proxyCall(p *proxyData, this Value, args []Value) (Value, error) {
	fn, ok, err := r.trap(p, "apply")
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return r.callObject(p.target, this, args, Undefined)
	}
	return r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), this, Obj(r.newArrayFrom(args)),
	})
}

// proxyConstruct implements the construct trap.
func (r *Runtime) proxyConstruct(p *proxyData, args []Value, newTarget Value) (Value, error) {
	fn, ok, err := r.trap(p, "construct")
	if err != nil {
		return Undefined, err
	}
	if !ok {
		return r.constructWithTarget(Obj(p.target), args, newTarget)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), Obj(r.newArrayFrom(args)), newTarget,
	})
	if err != nil {
		return Undefined, err
	}
	if !res.IsObject() {
		return Undefined, r.throwTypeError("the proxy \"construct\" trap must return an object")
	}
	return res, nil
}

func (r *Runtime) initProxyBuiltins() {
	proxyProto := newObject(r.proto.object, ClassObject)

	makeProxy := func(rt *Runtime, args []Value) (*Object, error) {
		target, handler := arg(args, 0), arg(args, 1)
		if !target.IsObject() || !handler.IsObject() {
			return nil, rt.throwTypeError("Proxy requires an object target and handler")
		}
		o := newObject(proxyProto, ClassProxy)
		o.data = &proxyData{target: target.Object(), handler: handler.Object()}
		// A proxy whose target is callable is itself callable, which is what
		// makes the apply and construct traps reachable.
		if target.Object().IsCallable() {
			o.class = ClassProxy
		}
		return o, nil
	}

	ctor := r.newCtor("Proxy", 2, proxyProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Proxy"); err != nil {
			return Undefined, err
		}
		o, err := makeProxy(rt, args)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	// A proxy has no prototype of its own -- what `new Proxy(...)` returns is
	// the proxy, never an instance of anything -- so the constructor has no
	// prototype property to hand out.
	if pr := ctor.getOwn(atomPrototype); pr != nil {
		pr.flags |= propConfigurable
	}
	ctor.deleteOwn(atomPrototype)

	r.defMethod(ctor, "revocable", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := makeProxy(rt, args)
		if err != nil {
			return Undefined, err
		}
		p := o.data.(*proxyData)
		// The revocation function is anonymous, which a script can check.
		revoke := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			p.revoked = true
			return Undefined, nil
		})
		res := newObject(rt.proto.object, ClassObject)
		res.setOwnRaw(rt.atoms.intern("proxy"), Obj(o), propDefault)
		res.setOwnRaw(rt.atoms.intern("revoke"), Obj(revoke), propDefault)
		return Obj(res), nil
	})
}

func (r *Runtime) initReflectBuiltins() {
	rf := newObject(r.proto.object, ClassObject)
	r.defValue(r.global, "Reflect", Obj(rf))
	r.defToStringTag(rf, "Reflect")

	r.defMethod(rf, "get", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.get requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		receiver := target
		if len(args) > 2 {
			receiver = args[2]
		}
		return rt.getProp(target.Object(), key, receiver)
	})

	r.defMethod(rf, "set", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.set requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		receiver := target
		if len(args) > 3 {
			receiver = args[3]
		}
		// Reflect.set reports success rather than throwing, which is what a
		// set trap forwards.
		ok, err := rt.setProp(target.Object(), key, arg(args, 2), receiver, false)
		if err != nil {
			return Undefined, err
		}
		return Bool(ok), nil
	})

	r.defMethod(rf, "has", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.has requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		has, err := rt.hasPropErr(target.Object(), key)
		return Bool(has), err
	})

	r.defMethod(rf, "deleteProperty", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.deleteProperty requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		ok, err := rt.deleteProp(target.Object(), key, false)
		if err != nil {
			return Undefined, err
		}
		return Bool(ok), nil
	})

	r.defMethod(rf, "ownKeys", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.ownKeys requires an object")
		}
		var keys []Atom
		if p := proxyOf(target.Object()); p != nil {
			var err error
			if keys, err = rt.proxyOwnKeys(p); err != nil {
				return Undefined, err
			}
		} else {
			keys = target.Object().ownKeys(true, rt.atoms)
		}
		out := make([]Value, len(keys))
		for i, k := range keys {
			out[i] = rt.keyToValue(k)
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(rf, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.getPrototypeOf requires an object")
		}
		if p := proxyOf(target.Object()); p != nil {
			return rt.proxyGetPrototypeOf(p)
		}
		return protoValue(target.Object()), nil
	})

	r.defMethod(rf, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.setPrototypeOf requires an object")
		}
		proto := arg(args, 1)
		if !proto.IsObject() && !proto.IsNull() {
			return Undefined, rt.throwTypeError("the prototype must be an object or null")
		}
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxySetPrototypeOf(p, proto)
			return Bool(ok), err
		}
		return Bool(rt.setProtoOfChecked(target.Object(), proto)), nil
	})

	r.defMethod(rf, "defineProperty", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.defineProperty requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxyDefineProperty(p, key, arg(args, 2))
			return Bool(ok), err
		}
		rt.materializeFunctionProp(target.Object(), key)
		if err := rt.definePropertyFromDescriptor(target.Object(), key, arg(args, 2)); err != nil {
			return False, nil
		}
		return True, nil
	})

	r.defMethod(rf, "getOwnPropertyDescriptor", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.getOwnPropertyDescriptor requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if p := proxyOf(target.Object()); p != nil {
			return rt.proxyGetOwnPropertyDescriptor(p, key)
		}
		return rt.describeProperty(target.Object(), key)
	})

	r.defMethod(rf, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.isExtensible requires an object")
		}
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxyIsExtensible(p)
			return Bool(ok), err
		}
		return Bool(target.Object().IsExtensible()), nil
	})

	r.defMethod(rf, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.preventExtensions requires an object")
		}
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxyPreventExtensions(p)
			return Bool(ok), err
		}
		target.Object().flags &^= objExtensible
		return True, nil
	})

	r.defMethod(rf, "apply", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Unlike Function.prototype.apply, there is no shorthand for "no
		// arguments" here: the list is read as an array-like whatever it is,
		// and a primitive is refused.
		callArgs, err := rt.argumentList(arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		return rt.call(arg(args, 0), arg(args, 1), callArgs)
	})

	r.defMethod(rf, "construct", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !isConstructor(target) {
			return Undefined, rt.throwTypeError("Reflect.construct requires a constructor")
		}
		// new.target defaults to the constructor being run, which is what makes
		// the two-argument form behave like new.
		newTarget := target
		if len(args) > 2 {
			newTarget = args[2]
			if !isConstructor(newTarget) {
				return Undefined, rt.throwTypeError(
					"the new.target given to Reflect.construct is not a constructor")
			}
		}
		callArgs, err := rt.argumentList(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return rt.constructWithTarget(target, callArgs, newTarget)
	})
}

// ownKeysOf lists an object's own keys, asking a proxy's trap when the object
// is one.
//
// Every operation that enumerates keys has to go through here rather than
// reading the property table: a proxy's own table is empty, and what it reports
// is whatever its trap says -- or, with no trap, whatever its target reports,
// which may be another proxy.
func (r *Runtime) ownKeysOf(o *Object, includeSymbols bool) ([]Atom, error) {
	p := proxyOf(o)
	if p == nil {
		return o.ownKeys(includeSymbols, r.atoms), nil
	}
	keys, err := r.proxyOwnKeys(p)
	if err != nil {
		return nil, err
	}
	if includeSymbols {
		return keys, nil
	}
	out := keys[:0:0]
	for _, k := range keys {
		if !r.atoms.IsSymbol(k) {
			out = append(out, k)
		}
	}
	return out, nil
}

// ownDescriptorOf describes an own property, asking a proxy's trap when the
// object is one.
func (r *Runtime) ownDescriptorOf(o *Object, key Atom) (Value, error) {
	if p := proxyOf(o); p != nil {
		return r.proxyGetOwnPropertyDescriptor(p, key)
	}
	return r.describeProperty(o, key)
}

// hasOwnPropOf reports whether an object has an own property, asking a proxy's
// getOwnPropertyDescriptor trap when the object is one.
func (r *Runtime) hasOwnPropOf(o *Object, key Atom) (bool, error) {
	p := proxyOf(o)
	if p == nil {
		if o.class == ClassModuleNamespace {
			// Asking whether a namespace has an export reads it, since the
			// answer comes from the module's environment rather than from a
			// property table.
			d, err := r.namespaceDescriptor(o, key)
			if err != nil || d != nil {
				return d != nil, err
			}
		}
		return r.hasOwnProp(o, key), nil
	}
	desc, err := r.proxyGetOwnPropertyDescriptor(p, key)
	if err != nil {
		return false, err
	}
	return desc.IsObject(), nil
}
