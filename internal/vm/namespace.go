package vm

// A module namespace object.
//
// `import * as ns from "m"` gives an object whose properties are the module's
// exports, and it is deliberately rigid: it has no prototype, it is not
// extensible, nothing can be added to it or removed from it, and its properties
// cannot be written. What a module exports is fixed when it is compiled, so an
// object that let any of that change would be lying.
//
// The exports themselves are live: reading one reads the binding now, which is
// what makes a cycle between two modules work. So the storage is an accessor
// while the object reports a data property, which is the one place the two have
// to disagree.
//
// It is a separate object from the module's environment, which doubles as the
// scope its code resolves names against and therefore has to have the global
// object as its prototype -- the opposite of what a namespace needs.

// namespaceObject returns the module's namespace, building it on first use.
func (r *Runtime) namespaceObject(m *Module) (*Object, error) {
	if m.ns != nil {
		return m.ns, nil
	}
	// The object is recorded before it is filled in, because a module may
	// export its own namespace -- directly or round a cycle -- and building it
	// twice would never finish.
	ns := newObject(nil, ClassModuleNamespace)
	ns.data = m
	m.ns = ns

	// The names are resolved through the whole graph, since a star re-export
	// contributes what another module exports, and one the graph cannot decide
	// is left out rather than reported.
	names, err := r.namespaceNames(m)
	if err != nil {
		m.ns = nil
		return nil, err
	}
	for _, exported := range names {
		b, err := r.resolveExport(m, exported, nil)
		if err != nil {
			m.ns = nil
			return nil, err
		}
		if b == nil || b.ambiguous {
			continue
		}
		src := b.module
		if b.local == nsBindingName {
			// The export is another module's namespace rather than one of its
			// bindings, so there is nothing to read through.
			inner, err := r.namespaceObject(src)
			if err != nil {
				m.ns = nil
				return nil, err
			}
			r.defineAccessor(ns, r.atoms.intern(exported),
				r.newNativeFunc("get "+exported, 0,
					func(rt *Runtime, this Value, args []Value) (Value, error) {
						return Obj(inner), nil
					}), nil, propEnumerable|propNamespaceExport)
			continue
		}
		key := r.atoms.intern(b.local)
		getter := r.newNativeFunc("get "+exported, 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				return rt.getProp(src.env, key, Obj(src.env))
			})
		// Writable and enumerable, as the specification says, but not
		// configurable: what a module exports cannot change.
		r.defineAccessor(ns, r.atoms.intern(exported), getter, nil,
			propEnumerable|propNamespaceExport)
	}

	// The tag is the one property that is not an export, and is fixed in every
	// respect.
	r.defineAccessor(ns, r.atoms.internSymbol(r.wellKnown.toStringTag),
		r.newNativeFunc("get [Symbol.toStringTag]", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				return Str(NewString("Module")), nil
			}), nil, propNamespaceTag)

	ns.flags &^= objExtensible
	return ns, nil
}

// namespaceDescriptor reports an export as the data property it is, rather than
// as the accessor it is stored as.
func (r *Runtime) namespaceDescriptor(o *Object, key Atom) *propDesc {
	p := o.getOwnVisible(key)
	if p == nil {
		return nil
	}
	v := Undefined
	if a := p.getterSetter(); a != nil && a.getter != nil {
		got, err := r.call(Obj(a.getter), Obj(o), nil)
		if err != nil {
			// A binding still in its dead zone has no value to report. The
			// property is there, which is what the descriptor says.
			got = Undefined
		}
		v = got
	}
	if p.flags&propNamespaceTag != 0 {
		return &propDesc{
			value: v, hasValue: true,
			writable: false, hasWritable: true,
			enumerable: false, hasEnumerable: true,
			configurable: false, hasConfigurable: true,
		}
	}
	return &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: false, hasConfigurable: true,
	}
}
