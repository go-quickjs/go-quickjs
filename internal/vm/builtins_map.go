package vm

// Map, Set, WeakMap and WeakSet.
//
// All four share one structure. Keys are compared by SameValueZero, which is
// strict equality except that NaN equals itself, and iteration follows
// insertion order -- both are observable, so the storage is a hash index over
// an append-only entry list rather than a plain Go map.
//
// Deletion tombstones an entry instead of removing it, because the
// specification requires that an entry deleted during iteration is skipped
// while the entries around it keep their positions.

// mapKey is the comparable form of a Value, used as a Go map key.
//
// The fields are separated by kind so that values of different types never
// collide: the number 1, the string "1" and a boolean are distinct keys even
// though they might otherwise hash alike.
type mapKey struct {
	kind Kind
	num  float64
	str  string
	// ref holds the pointer for an object or symbol key, whose identity is the
	// comparison.
	ref any
}

// mapKeyOf converts a value to its comparable key form.
//
// weak selects the form a WeakMap or WeakSet uses, where an object or symbol
// key is identified by a weak.Pointer rather than by the pointer itself. A
// weak.Pointer is comparable and equal exactly when it names the same object,
// which is what lets the index find an entry without the index being what keeps
// the key alive.
func (r *Runtime) mapKeyOf(v Value, weakKey bool) mapKey {
	if weakKey && (v.IsObject() || v.IsSymbol()) {
		return mapKey{kind: v.Kind(), ref: makeWeak(v)}
	}
	return r.strongKeyOf(v)
}

func (r *Runtime) strongKeyOf(v Value) mapKey {
	switch v.Kind() {
	case KindNumber:
		n := v.Number()
		switch {
		case n != n:
			// SameValueZero makes NaN equal to itself, so every NaN has to
			// produce one key. Go's NaN is not equal to itself, so a sentinel
			// stands in for it.
			return mapKey{kind: KindNumber, str: "NaN"}
		case n == 0:
			// +0 and -0 are the same key.
			return mapKey{kind: KindNumber, num: 0}
		}
		return mapKey{kind: KindNumber, num: n}
	case KindString:
		return mapKey{kind: KindString, str: v.String().Go()}
	case KindBool:
		return mapKey{kind: KindBool, num: boolToFloat(v.BoolValue())}
	case KindBigInt:
		// Two BigInts with the same value are the same key, so the decimal
		// form rather than the pointer identifies them.
		return mapKey{kind: KindBigInt, str: v.BigInt().String()}
	case KindObject:
		return mapKey{kind: KindObject, ref: v.Object()}
	case KindSymbol:
		return mapKey{kind: KindSymbol, ref: v.Symbol()}
	}
	return mapKey{kind: v.Kind()}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// mapEntry is one key/value pair. A Set stores the key in both fields, which is
// what its iteration protocol reports.
type mapEntry struct {
	key, value Value
	deleted    bool
	// weakKey holds a WeakMap or WeakSet key, which the entry must not be what
	// keeps alive. key is left empty for those, so that reading one back means
	// asking whether it is still there.
	weakKey weakTarget
}

// jsMap is the shared storage for all four collection types.
type jsMap struct {
	entries []mapEntry
	index   map[mapKey]int
	size    int
	// weak marks a WeakMap or WeakSet, whose keys are held weakly: an entry
	// stops existing once nothing else refers to its key.
	//
	// The value is still held strongly, so a value that refers to its own key
	// keeps that key alive. Breaking that cycle needs ephemeron marking, which
	// Go's collector does not offer; it is the one thing about these that is
	// not the real article.
	weak bool
	// nextSweep is the entry count at which the next scan for collected keys
	// happens. Doubling it after each scan is what makes the scanning cost a
	// constant per insertion however large the collection grows.
	nextSweep int
}

func newJSMap(weak bool) *jsMap {
	return &jsMap{index: make(map[mapKey]int), weak: weak}
}

func (m *jsMap) get(r *Runtime, k Value) (Value, bool) {
	i, ok := m.index[r.mapKeyOf(k, m.weak)]
	if !ok || m.entries[i].deleted || !m.live(i) {
		return Undefined, false
	}
	return m.entries[i].value, true
}

// live reports whether an entry's key is still there, which only a weak
// collection can answer no to.
func (m *jsMap) live(i int) bool {
	if !m.weak {
		return true
	}
	return m.entries[i].weakKey.alive()
}

func (m *jsMap) set(r *Runtime, k, v Value) {
	mk := r.mapKeyOf(k, m.weak)
	if i, ok := m.index[mk]; ok && !m.entries[i].deleted && m.live(i) {
		// Re-setting an existing key updates the value and keeps its position.
		m.entries[i].value = v
		return
	}
	e := mapEntry{key: k, value: v}
	if m.weak {
		// The key is not stored, only a weak reference to it: an entry must
		// not be what keeps its own key alive.
		e.key = Undefined
		e.weakKey = makeWeak(k)
		m.sweep()
	}
	m.entries = append(m.entries, e)
	m.index[mk] = len(m.entries) - 1
	m.size++
}

// sweep drops the entries whose keys have been collected.
//
// Finding them means walking the list, so the threshold doubles after each
// scan: the cost is then a constant per insertion however large the collection
// grows, and a collection that is only read never pays it at all.
func (m *jsMap) sweep() {
	if len(m.entries) < m.nextSweep {
		return
	}
	kept := m.entries[:0]
	clear(m.index)
	m.size = 0
	for _, e := range m.entries {
		if e.deleted || !e.weakKey.alive() {
			continue
		}
		kept = append(kept, e)
		m.index[mapKey{kind: e.weakKey.kind(), ref: e.weakKey}] = len(kept) - 1
		m.size++
	}
	clear(m.entries[len(kept):])
	m.entries = kept
	m.nextSweep = 2*len(m.entries) + 16
}

func (m *jsMap) delete(r *Runtime, k Value) bool {
	mk := r.mapKeyOf(k, m.weak)
	i, ok := m.index[mk]
	if !ok || m.entries[i].deleted || !m.live(i) {
		return false
	}
	// Tombstone rather than remove, so that an iteration in progress keeps its
	// position in the entry list.
	m.entries[i].deleted = true
	m.entries[i].key = Undefined
	m.entries[i].value = Undefined
	delete(m.index, mk)
	m.size--
	return true
}

func (m *jsMap) clear() {
	for i := range m.entries {
		m.entries[i].deleted = true
		m.entries[i].key = Undefined
		m.entries[i].value = Undefined
	}
	clear(m.index)
	m.size = 0
}

// mapOf recovers the storage from a receiver, checking the class so that a
// method called on the wrong kind of object reports it.
func (r *Runtime) mapOf(this Value, class Class, name string) (*jsMap, error) {
	if !this.IsObject() || this.Object().class != class {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	m, ok := this.Object().data.(*jsMap)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized collection", name)
	}
	return m, nil
}

func (r *Runtime) initMapBuiltins() {
	r.initMapIteratorProto(r.proto.mapIter, "Map Iterator")
	r.initMapIteratorProto(r.proto.setIter, "Set Iterator")
	r.initArrayIteratorProto()
	r.initStringIteratorProto()
	r.initRegExpStringIteratorProto()

	r.proto.mapProto = newObject(r.proto.object, ClassObject)
	p := r.proto.mapProto

	ctor := r.newCtor("Map", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Map"); err != nil {
			return Undefined, err
		}
		o := newObject(rt.protoFromNewTarget(rt.proto.mapProto), ClassMap)
		o.data = newJSMap(false)
		// An iterable argument seeds the map with its [key, value] pairs,
		// through the object's own set method: a subclass that overrides it
		// sees every entry go by.
		if err := rt.seedFromEntries(o, arg(args, 0), "set"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defSpecies(ctor)

	// Map.groupBy differs from Object.groupBy in what it returns: a Map can be
	// keyed by anything, where an object's keys are strings and symbols only.
	r.defMethod(ctor, "groupBy", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		cb := arg(args, 1)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("Map.groupBy requires a function")
		}
		m := newJSMap(false)
		i := 0
		err := rt.iterate(arg(args, 0), func(v Value) error {
			key, err := rt.call(cb, Undefined, []Value{v, Int(i)})
			i++
			if err != nil {
				return err
			}
			key = normalizeZero(key)
			group, ok := m.get(rt, key)
			if !ok {
				m.set(rt, key, Obj(rt.newArrayFrom([]Value{v})))
				return nil
			}
			if group.IsObject() {
				g := group.Object()
				g.elems = append(g.elems, v)
			}
			return nil
		})
		if err != nil {
			return Undefined, err
		}
		o := newObject(rt.proto.mapProto, ClassMap)
		o.data = m
		return Obj(o), nil
	})
	_ = ctor

	r.defMethod(p, "get", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.get")
		if err != nil {
			return Undefined, err
		}
		v, _ := m.get(rt, arg(args, 0))
		return v, nil
	})
	r.defMethod(p, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.set")
		if err != nil {
			return Undefined, err
		}
		m.set(rt, arg(args, 0), arg(args, 1))
		// set returns the map, which is what makes chaining work.
		return this, nil
	})
	r.defMethod(p, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.has")
		if err != nil {
			return Undefined, err
		}
		_, ok := m.get(rt, arg(args, 0))
		return Bool(ok), nil
	})
	r.defMethod(p, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(m.delete(rt, arg(args, 0))), nil
	})
	r.defMethod(p, "clear", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.clear")
		if err != nil {
			return Undefined, err
		}
		m.clear()
		return Undefined, nil
	})
	r.defGetter(p, "size", func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.size")
		if err != nil {
			return Undefined, err
		}
		return Int(m.size), nil
	})
	r.defMethod(p, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.forEach")
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("Map.prototype.forEach requires a function")
		}
		// The index is re-read each step because the callback may add entries,
		// which the specification requires the iteration to visit.
		for i := 0; i < len(m.entries); i++ {
			if m.entries[i].deleted {
				continue
			}
			if _, err := rt.call(cb, arg(args, 1),
				[]Value{m.entries[i].value, m.entries[i].key, this}); err != nil {
				return Undefined, err
			}
		}
		return Undefined, nil
	})

	r.defMapIterator(p, ClassMap, "entries", mapIterEntries)
	r.defMapIterator(p, ClassMap, "keys", mapIterKeys)
	r.defMapIterator(p, ClassMap, "values", mapIterValues)
	// A Map iterates as its entries: the same function object, not another one
	// that does the same thing, which a script can tell apart.
	r.aliasMethod(p, r.atoms.internSymbol(r.wellKnown.iterator), "entries")
	r.defToStringTag(p, "Map")
}

func (r *Runtime) initSetBuiltins() {
	r.proto.setProto = newObject(r.proto.object, ClassObject)
	p := r.proto.setProto

	setCtor := r.newCtor("Set", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Set"); err != nil {
			return Undefined, err
		}
		o := newObject(rt.protoFromNewTarget(rt.proto.setProto), ClassSet)
		o.data = newJSMap(false)
		if err := rt.seedFromValues(o, arg(args, 0), "add"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defSpecies(setCtor)

	r.defMethod(p, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.add")
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		// A Set stores the value as both key and value, which is what its
		// entries iterator reports.
		m.set(rt, v, v)
		return this, nil
	})
	r.defMethod(p, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.has")
		if err != nil {
			return Undefined, err
		}
		_, ok := m.get(rt, arg(args, 0))
		return Bool(ok), nil
	})
	r.defMethod(p, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(m.delete(rt, arg(args, 0))), nil
	})
	r.defMethod(p, "clear", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.clear")
		if err != nil {
			return Undefined, err
		}
		m.clear()
		return Undefined, nil
	})
	r.defGetter(p, "size", func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.size")
		if err != nil {
			return Undefined, err
		}
		return Int(m.size), nil
	})
	r.defMethod(p, "forEach", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassSet, "Set.prototype.forEach")
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("Set.prototype.forEach requires a function")
		}
		for i := 0; i < len(m.entries); i++ {
			if m.entries[i].deleted {
				continue
			}
			if _, err := rt.call(cb, arg(args, 1),
				[]Value{m.entries[i].value, m.entries[i].key, this}); err != nil {
				return Undefined, err
			}
		}
		return Undefined, nil
	})

	r.defMapIterator(p, ClassSet, "values", mapIterValues)
	r.defMapIterator(p, ClassSet, "entries", mapIterEntries)
	// A Set's keys are its values, and it iterates as them -- the same function
	// object each time, which is what a script comparing them sees.
	r.aliasMethod(p, r.atoms.intern("keys"), "values")
	r.aliasMethod(p, r.atoms.internSymbol(r.wellKnown.iterator), "values")
	r.initSetOps(p)
	r.defToStringTag(p, "Set")
}

func (r *Runtime) initWeakCollections() {
	// WeakMap and WeakSet accept only object keys, which is the one behaviour
	// that distinguishes them here.
	wmProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakMap", 0, wmProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("WeakMap"); err != nil {
			return Undefined, err
		}
		o := newObject(rt.protoFromNewTarget(wmProto), ClassWeakMap)
		o.data = newJSMap(true)
		// An iterable of [key, value] pairs populates it, exactly as for Map.
		if err := rt.seedFromEntries(o, arg(args, 0), "set"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defMethod(wmProto, "get", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakMap, "WeakMap.prototype.get")
		if err != nil {
			return Undefined, err
		}
		v, _ := m.get(rt, arg(args, 0))
		return v, nil
	})
	r.defMethod(wmProto, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakMap, "WeakMap.prototype.set")
		if err != nil {
			return Undefined, err
		}
		k := arg(args, 0)
		if !rt.canBeWeak(k) {
			return Undefined, rt.throwTypeError(
				"a WeakMap key must be an object or an unregistered symbol")
		}
		m.set(rt, k, arg(args, 1))
		return this, nil
	})
	r.defMethod(wmProto, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakMap, "WeakMap.prototype.has")
		if err != nil {
			return Undefined, err
		}
		_, ok := m.get(rt, arg(args, 0))
		return Bool(ok), nil
	})
	r.defMethod(wmProto, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakMap, "WeakMap.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(m.delete(rt, arg(args, 0))), nil
	})
	r.defToStringTag(wmProto, "WeakMap")

	wsProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakSet", 0, wsProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("WeakSet"); err != nil {
			return Undefined, err
		}
		o := newObject(rt.protoFromNewTarget(wsProto), ClassWeakSet)
		o.data = newJSMap(true)
		if err := rt.seedFromValues(o, arg(args, 0), "add"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defMethod(wsProto, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakSet, "WeakSet.prototype.add")
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		if !rt.canBeWeak(v) {
			return Undefined, rt.throwTypeError(
				"a WeakSet value must be an object or an unregistered symbol")
		}
		m.set(rt, v, v)
		return this, nil
	})
	r.defMethod(wsProto, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakSet, "WeakSet.prototype.has")
		if err != nil {
			return Undefined, err
		}
		_, ok := m.get(rt, arg(args, 0))
		return Bool(ok), nil
	})
	r.defMethod(wsProto, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakSet, "WeakSet.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(m.delete(rt, arg(args, 0))), nil
	})
	r.defToStringTag(wsProto, "WeakSet")
}

// mapIterKind selects what a collection iterator yields.
type mapIterKind uint8

const (
	mapIterKeys mapIterKind = iota
	mapIterValues
	mapIterEntries
)

// defMapIterator defines one of the keys/values/entries methods.
func (r *Runtime) defMapIterator(p *Object, class Class, name string, kind mapIterKind) {
	r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, class, name)
		if err != nil {
			return Undefined, err
		}
		proto := rt.proto.mapIter
		if class == ClassSet {
			proto = rt.proto.setIter
		}
		return rt.newMapIterator(m, kind, proto), nil
	})
}

// mapIterData is where a Map or Set iterator is in its collection.
//
// The cursor is an index into the entry list rather than a snapshot, so an
// entry added during iteration is visited and one deleted during it is skipped,
// which is what the specification requires.
type mapIterData struct {
	m    *jsMap
	kind mapIterKind
	i    int
	// done marks an iterator that reached the end. It stays done even if the
	// collection grows afterwards: an exhausted iterator is finished with,
	// rather than waiting for more.
	done bool
}

// newMapIterator returns an iterator over a collection's live entries.
//
// Its next method lives on the shared prototype rather than on the iterator, so
// that every iterator of a kind has the same one -- which a script can check,
// and which is what makes the prototype worth having.
func (r *Runtime) newMapIterator(m *jsMap, kind mapIterKind, proto *Object) Value {
	iter := newObject(proto, ClassIterator)
	iter.data = &mapIterData{m: m, kind: kind}
	return Obj(iter)
}

// initMapIteratorProto fills in %MapIteratorPrototype% or
// %SetIteratorPrototype%, which differ only in the tag they report.
func (r *Runtime) initMapIteratorProto(proto *Object, tag string) {
	r.defMethod(proto, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.mapIterOf(this, tag)
		if err != nil {
			return Undefined, err
		}
		for !d.done && d.i < len(d.m.entries) && d.m.entries[d.i].deleted {
			d.i++
		}
		if d.done || d.i >= len(d.m.entries) {
			d.done = true
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		e := d.m.entries[d.i]
		d.i++
		var v Value
		switch d.kind {
		case mapIterKeys:
			v = e.key
		case mapIterValues:
			v = e.value
		default:
			v = Obj(rt.newArrayFrom([]Value{e.key, e.value}))
		}
		return Obj(rt.iterResult(v, false)), nil
	})
	proto.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString(tag)), propConfigurable)
}

// mapIterOf recovers an iterator's state, refusing anything else.
func (r *Runtime) mapIterOf(this Value, tag string) (*mapIterData, error) {
	if this.IsObject() {
		if d, ok := this.Object().data.(*mapIterData); ok {
			return d, nil
		}
	}
	return nil, r.throwTypeError("%s.prototype.next called on an incompatible receiver", tag)
}

// iterateOptional walks an iterable, treating undefined and null as empty.
//
// Every collection constructor takes its contents this way: `new Set()` and
// `new Set(undefined)` both mean an empty one, and anything else has to be
// iterable.
func (r *Runtime) iterateOptional(v Value, visit func(Value) error) error {
	if v.IsNullish() {
		return nil
	}
	return r.iterate(v, visit)
}

// protoFromNewTarget resolves the prototype a constructed object should have.
//
// A subclass's instance needs it before the constructor finishes, because the
// constructor reads the method it adds entries through from the object -- and
// that method is the subclass's when the subclass overrode it.
func (r *Runtime) protoFromNewTarget(fallback *Object) *Object {
	nt := r.newTarget()
	if !nt.IsObject() {
		return fallback
	}
	p, err := r.getProp(nt.Object(), atomPrototype, nt)
	if err != nil || !p.IsObject() {
		return fallback
	}
	return p.Object()
}

// collectionAdder reads the method a collection constructor adds through.
//
// It comes from the object rather than from the intrinsic prototype, so a
// subclass that overrides add or set sees every entry the constructor was
// given.
func (r *Runtime) collectionAdder(o *Object, name string) (Value, error) {
	adder, err := r.getProp(o, r.atoms.intern(name), Obj(o))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(adder) {
		return Undefined, r.throwTypeError("%s is not callable", name)
	}
	return adder, nil
}

// seedFromValues fills a set-like collection from an iterable, one call to its
// own adder per value.
func (r *Runtime) seedFromValues(o *Object, src Value, name string) error {
	if src.IsNullish() {
		return nil
	}
	adder, err := r.collectionAdder(o, name)
	if err != nil {
		return err
	}
	return r.iterate(src, func(v Value) error {
		_, err := r.call(adder, Obj(o), []Value{v})
		return err
	})
}

// seedFromEntries fills a map-like collection from an iterable of two-element
// entries, one call to its own adder per entry.
func (r *Runtime) seedFromEntries(o *Object, src Value, name string) error {
	if src.IsNullish() {
		return nil
	}
	adder, err := r.collectionAdder(o, name)
	if err != nil {
		return err
	}
	return r.eachEntry(src, func(k, v Value) error {
		_, err := r.call(adder, Obj(o), []Value{k, v})
		return err
	})
}

// aliasMethod gives a prototype a second name for a method it already has,
// sharing the one function object rather than making another that behaves the
// same: `Set.prototype.keys === Set.prototype.values` is observable.
func (r *Runtime) aliasMethod(p *Object, key Atom, from string) {
	src := p.getOwn(r.atoms.intern(from))
	if src == nil {
		return
	}
	p.setOwnRaw(key, src.value, propWritable|propConfigurable)
}

// eachEntry walks an iterable of two-element entries, which is the shape Map
// and WeakMap take.
func (r *Runtime) eachEntry(v Value, visit func(k, value Value) error) error {
	return r.iterateOptional(v, func(item Value) error {
		if !item.IsObject() {
			return r.throwTypeError("a collection entry must be an object")
		}
		k, err := r.getValueProp(item, internIndex(0))
		if err != nil {
			return err
		}
		val, err := r.getValueProp(item, internIndex(1))
		if err != nil {
			return err
		}
		return visit(k, val)
	})
}
