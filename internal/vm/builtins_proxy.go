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
	return r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), r.keyToValue(key), receiver,
	})
}

// proxySet implements the set trap.
func (r *Runtime) proxySet(p *proxyData, key Atom, val, receiver Value, strict bool) error {
	fn, ok, err := r.trap(p, "set")
	if err != nil {
		return err
	}
	if !ok {
		return r.setProp(p.target, key, val, Obj(p.target), strict)
	}
	res, err := r.call(fn, Obj(p.handler), []Value{
		Obj(p.target), r.keyToValue(key), val, receiver,
	})
	if err != nil {
		return err
	}
	// A set trap that reports failure is a TypeError in strict mode, which is
	// the only way the caller learns the assignment did not happen.
	if !res.Truthy() && strict {
		return r.throwTypeError("the proxy \"set\" trap returned false for %q",
			r.atoms.name(key))
	}
	return nil
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
	return res.Truthy(), nil
}

// proxyOwnKeys implements the ownKeys trap.
func (r *Runtime) proxyOwnKeys(p *proxyData) ([]Atom, error) {
	fn, ok, err := r.trap(p, "ownKeys")
	if err != nil {
		return nil, err
	}
	if !ok {
		return p.target.ownKeys(true, r.atoms), nil
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
	for _, it := range items {
		k, err := r.toPropertyKey(it)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
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
		return r.construct(Obj(p.target), args)
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
		o, err := makeProxy(rt, args)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})

	r.defMethod(ctor, "revocable", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := makeProxy(rt, args)
		if err != nil {
			return Undefined, err
		}
		p := o.data.(*proxyData)
		revoke := rt.newNativeFunc("revoke", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
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
		if err := rt.setProp(target.Object(), key, arg(args, 2), receiver, false); err != nil {
			return False, nil
		}
		return True, nil
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
		return Bool(rt.hasProp(target.Object(), key)), nil
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
		if p := target.Object().proto; p != nil {
			return Obj(p), nil
		}
		return Null, nil
	})

	r.defMethod(rf, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.setPrototypeOf requires an object")
		}
		switch pv := arg(args, 1); {
		case pv.IsObject():
			target.Object().proto = pv.Object()
		case pv.IsNull():
			target.Object().proto = nil
		default:
			return False, nil
		}
		return True, nil
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
		return rt.describeProperty(target.Object(), key), nil
	})

	r.defMethod(rf, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.isExtensible requires an object")
		}
		return Bool(target.Object().IsExtensible()), nil
	})

	r.defMethod(rf, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Reflect.preventExtensions requires an object")
		}
		target.Object().flags &^= objExtensible
		return True, nil
	})

	r.defMethod(rf, "apply", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var callArgs []Value
		if list := arg(args, 2); !list.IsNullish() {
			var err error
			if callArgs, err = rt.arrayToSlice(list); err != nil {
				return Undefined, err
			}
		}
		return rt.call(arg(args, 0), arg(args, 1), callArgs)
	})

	r.defMethod(rf, "construct", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var callArgs []Value
		if list := arg(args, 1); !list.IsNullish() {
			var err error
			if callArgs, err = rt.arrayToSlice(list); err != nil {
				return Undefined, err
			}
		}
		return rt.construct(arg(args, 0), callArgs)
	})
}
