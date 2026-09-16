package vm

// WeakRef and FinalizationRegistry.
//
// Both are about observing garbage collection, and neither can observe Go's.
// The runtime has no hook that fires when an object becomes unreachable, and
// synthesizing one with runtime.SetFinalizer would resurrect the object into
// engine data structures the collector has already decided nothing reaches.
//
// So a WeakRef here holds its target strongly and always derefs to it, and a
// FinalizationRegistry records its registrations and never calls back. Both are
// conforming: the specification never requires that anything be collected, only
// that a collected target stop being observable. A program that would have seen
// a cleared reference instead sees a live one, which is the same thing it sees
// on any engine that has not yet run a collection.
//
// The cost is retention, not wrong answers. It is documented rather than hidden
// because a host caching large values behind a WeakRef will not get the
// reclamation it is expecting.

// weakRefData is a WeakRef's target.
type weakRefData struct {
	target Value
	// cleared is set by nothing today, but deref consults it so that the
	// meaning of the field is unambiguous if collection ever becomes possible.
	cleared bool
}

// finalizationCell is one registration in a FinalizationRegistry.
type finalizationCell struct {
	target Value
	held   Value
	token  Value
	hasTok bool
}

// finalizationData is a FinalizationRegistry's state.
type finalizationData struct {
	cleanup Value
	cells   []finalizationCell
}

// canBeWeak reports whether a value may be a weak target.
//
// Objects always can. Symbols can too, unless they are in the global registry,
// where Symbol.for guarantees they live forever and a weak reference to one
// would be meaningless.
func (r *Runtime) canBeWeak(v Value) bool {
	if v.IsObject() {
		return true
	}
	if v.IsSymbol() {
		return !r.isRegisteredSymbol(v.Symbol())
	}
	return false
}

func (r *Runtime) initWeakRefBuiltins() {
	wrProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakRef", 1, wrProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("WeakRef requires new")
		}
		target := arg(args, 0)
		if !rt.canBeWeak(target) {
			return Undefined, rt.throwTypeError("a WeakRef target must be an object or an unregistered symbol")
		}
		o := newObject(wrProto, ClassWeakRef)
		o.data = &weakRefData{target: target}
		return Obj(o), nil
	})

	r.defMethod(wrProto, "deref", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() || this.Object().class != ClassWeakRef {
			return Undefined, rt.throwTypeError("WeakRef.prototype.deref called on an incompatible receiver")
		}
		d, ok := this.Object().data.(*weakRefData)
		if !ok {
			return Undefined, rt.throwTypeError("WeakRef.prototype.deref called on an uninitialized WeakRef")
		}
		if d.cleared {
			return Undefined, nil
		}
		return d.target, nil
	})
	r.defToStringTag(wrProto, "WeakRef")

	frProto := newObject(r.proto.object, ClassObject)
	r.newCtor("FinalizationRegistry", 1, frProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("FinalizationRegistry requires new")
		}
		cleanup := arg(args, 0)
		if !isCallable(cleanup) {
			return Undefined, rt.throwTypeError("a FinalizationRegistry requires a cleanup callback")
		}
		o := newObject(frProto, ClassFinalizationRegistry)
		o.data = &finalizationData{cleanup: cleanup}
		return Obj(o), nil
	})

	registryOf := func(rt *Runtime, this Value, name string) (*finalizationData, error) {
		if !this.IsObject() || this.Object().class != ClassFinalizationRegistry {
			return nil, rt.throwTypeError("%s called on an incompatible receiver", name)
		}
		d, ok := this.Object().data.(*finalizationData)
		if !ok {
			return nil, rt.throwTypeError("%s called on an uninitialized FinalizationRegistry", name)
		}
		return d, nil
	}

	r.defMethod(frProto, "register", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := registryOf(rt, this, "FinalizationRegistry.prototype.register")
		if err != nil {
			return Undefined, err
		}
		target, held, token := arg(args, 0), arg(args, 1), arg(args, 2)
		if !rt.canBeWeak(target) {
			return Undefined, rt.throwTypeError("the target must be an object or an unregistered symbol")
		}
		// Registering a target as its own held value would keep the cell alive
		// forever even on an engine that does collect, so it is rejected.
		if target.StrictEquals(held) {
			return Undefined, rt.throwTypeError("the held value cannot be the target itself")
		}
		hasTok := !token.IsUndefined()
		if hasTok && !rt.canBeWeak(token) {
			return Undefined, rt.throwTypeError("the unregister token must be an object or an unregistered symbol")
		}
		d.cells = append(d.cells, finalizationCell{target: target, held: held, token: token, hasTok: hasTok})
		return Undefined, nil
	})

	r.defMethod(frProto, "unregister", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := registryOf(rt, this, "FinalizationRegistry.prototype.unregister")
		if err != nil {
			return Undefined, err
		}
		token := arg(args, 0)
		if !rt.canBeWeak(token) {
			return Undefined, rt.throwTypeError("the unregister token must be an object or an unregistered symbol")
		}
		kept := d.cells[:0]
		removed := false
		for _, c := range d.cells {
			if c.hasTok && c.token.StrictEquals(token) {
				removed = true
				continue
			}
			kept = append(kept, c)
		}
		d.cells = kept
		return Bool(removed), nil
	})

	r.defToStringTag(frProto, "FinalizationRegistry")
}

// isRegisteredSymbol reports whether a symbol came from Symbol.for, and so is
// kept alive by the registry regardless of what else refers to it.
func (r *Runtime) isRegisteredSymbol(sym *Symbol) bool {
	for _, s := range r.symbolRegistry {
		if s == sym {
			return true
		}
	}
	return false
}
