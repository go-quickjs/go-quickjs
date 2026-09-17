package vm

// WeakRef and FinalizationRegistry.
//
// Both are about observing garbage collection, which Go's runtime now supports
// well enough to do properly: weak.Pointer refers to an object without keeping
// it alive, and runtime.AddCleanup runs a function once one becomes
// unreachable. See weak.go for the two properties of that machinery that shape
// what follows -- chiefly that a cleanup runs on another goroutine, so it
// queues work rather than doing any.

// weakRefData is a WeakRef's target.
type weakRefData struct {
	target weakTarget
}

// finalizationCell is one registration in a FinalizationRegistry.
//
// The target and the token are held weakly and the held value strongly, which
// is the whole shape of the thing: a registration must not be what keeps its
// target alive, and the held value has to survive to be handed to the callback.
type finalizationCell struct {
	target weakTarget
	held   Value
	token  weakTarget
	hasTok bool
	// unregistered and done retire a cell. A cleanup may already be queued when
	// unregister is called, and the target may be collected twice over if the
	// program registers it twice, so the cell rather than the queue decides
	// whether the callback runs.
	unregistered bool
	done         bool
}

// finalizationData is a FinalizationRegistry's state.
type finalizationData struct {
	cleanup Value
	cells   []*finalizationCell
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
		o.data = &weakRefData{target: makeWeak(target)}
		// A target is kept alive for the rest of the turn it was registered
		// in, so that a reference cannot be created and found empty in the
		// same breath.
		rt.keepDuringJob(target)
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
		v, alive := d.target.get()
		if !alive {
			return Undefined, nil
		}
		// Two calls in one turn have to answer the same way: a program that
		// checks a reference and then uses it cannot have the value vanish in
		// between.
		rt.keepDuringJob(v)
		return v, nil
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
		rt.registries = append(rt.registries, makeWeak(Obj(o)))
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
		cell := &finalizationCell{
			target: makeWeak(target),
			held:   held,
			hasTok: hasTok,
		}
		if hasTok {
			cell.token = makeWeak(token)
		}
		d.cells = append(d.cells, cell)
		rt.watchForCollection(this.Object(), cell, target)
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
			if tok, alive := c.token.get(); c.hasTok && alive && tok.StrictEquals(token) {
				// Marked as well as dropped: a cleanup for it may already be
				// queued, and the cell is what tells the drain to skip it.
				c.unregistered = true
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

// pruneRegistries drops the registrations whose callbacks have run.
//
// A cell is retired rather than removed when its cleanup fires, because the
// cleanup runs on the collector's goroutine and the cell list belongs to the
// interpreter's.
func (r *Runtime) pruneRegistries() {
	kept := r.registries[:0]
	for _, w := range r.registries {
		v, alive := w.get()
		if !alive {
			continue
		}
		kept = append(kept, w)
		d, ok := v.Object().data.(*finalizationData)
		if !ok {
			continue
		}
		live := d.cells[:0]
		for _, c := range d.cells {
			if c.done || c.unregistered {
				continue
			}
			live = append(live, c)
		}
		d.cells = live
	}
	clear(r.registries[len(kept):])
	r.registries = kept
}
