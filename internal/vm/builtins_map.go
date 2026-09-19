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
	// Exactly one of these carries the key: ref for an object, a symbol or a
	// weak reference to one, whose identity is the comparison, and val for
	// everything else. They are indexed separately, so what is stored is one
	// or the other rather than room for both.
	val valueKey
	ref any
}

// valueKey identifies a key that is compared by value rather than by identity.
type valueKey struct {
	kind Kind
	num  float64
	str  string
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
		return mapKey{ref: makeWeak(v)}
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
			return mapKey{val: valueKey{kind: KindNumber, str: "NaN"}}
		case n == 0:
			// +0 and -0 are the same key.
			return mapKey{val: valueKey{kind: KindNumber}}
		}
		return mapKey{val: valueKey{kind: KindNumber, num: n}}
	case KindString:
		return mapKey{val: valueKey{kind: KindString, str: v.String().Go()}}
	case KindBool:
		return mapKey{val: valueKey{kind: KindBool, num: boolToFloat(v.BoolValue())}}
	case KindBigInt:
		// Two BigInts with the same value are the same key, so the decimal
		// form rather than the pointer identifies them.
		return mapKey{val: valueKey{kind: KindBigInt, str: v.BigInt().String()}}
	case KindObject:
		return mapKey{ref: v.Object()}
	case KindSymbol:
		return mapKey{ref: v.Symbol()}
	}
	return mapKey{val: valueKey{kind: v.Kind()}}
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
}

// jsMap is the shared storage for all four collection types.
type jsMap struct {
	entries []mapEntry
	// weakKeys holds a weak reference to each entry's key, for a WeakMap or a
	// WeakSet only: an entry of one must not be what keeps its own key alive,
	// and its key field is left empty so that reading it back means asking
	// whether the key is still there.
	//
	// It is a list of its own rather than a field of the entry because a Map
	// and a Set, which is most of them, would carry three words per entry that
	// nothing ever reads. It has one element per entry when it is there at all.
	weakKeys []weakTarget
	// byValue and byRef index the entries by key. They are separate because
	// what identifies a key is either its value or its address, never both,
	// and a map that holds one kind should not pay for room for the other.
	// Each is created when the first key of its kind arrives.
	byValue map[valueKey]int
	byRef   map[any]int
	size    int
	// weak marks a WeakMap or WeakSet, whose keys are held weakly: an entry
	// stops existing once nothing else refers to its key.
	//
	// A WeakMap value is still held strongly, so a value that refers to its own
	// key keeps that key alive. Breaking that cycle needs ephemeron marking,
	// which Go's collector does not offer; it is the one thing about these that
	// is not the real article. WeakSet stores no value beside its weak key.
	weak bool
	// nextSweep is the entry count at which the next scan for collected keys
	// happens. Doubling it after each scan is what makes the scanning cost a
	// constant per insertion however large the collection grows.
	nextSweep int
}

func newJSMap(weak bool) *jsMap {
	return &jsMap{weak: weak}
}

// lookup finds the entry a key indexes, if any.
func (m *jsMap) lookup(k mapKey) (int, bool) {
	if k.ref != nil {
		i, ok := m.byRef[k.ref]
		return i, ok
	}
	i, ok := m.byValue[k.val]
	return i, ok
}

// record points a key at an entry.
func (m *jsMap) record(k mapKey, i int) {
	if k.ref != nil {
		if m.byRef == nil {
			m.byRef = make(map[any]int)
		}
		m.byRef[k.ref] = i
		return
	}
	if m.byValue == nil {
		m.byValue = make(map[valueKey]int)
	}
	m.byValue[k.val] = i
}

// forget removes a key from the index.
func (m *jsMap) forget(k mapKey) {
	if k.ref != nil {
		delete(m.byRef, k.ref)
		return
	}
	delete(m.byValue, k.val)
}

// clearIndex empties the index, keeping what it has allocated.
func (m *jsMap) clearIndex() {
	clear(m.byValue)
	clear(m.byRef)
}

func (m *jsMap) get(r *Runtime, k Value) (Value, bool) {
	i, ok := m.lookup(r.mapKeyOf(k, m.weak))
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
	return m.weakKeys[i].alive()
}

func (m *jsMap) set(r *Runtime, k, v Value) {
	mk := r.mapKeyOf(k, m.weak)
	if i, ok := m.lookup(mk); ok && !m.entries[i].deleted && m.live(i) {
		// Re-setting an existing key updates the value and keeps its position.
		m.entries[i].value = v
		return
	}
	e := mapEntry{key: k, value: v}
	if m.weak {
		// The key is not stored, only a weak reference to it: an entry must
		// not be what keeps its own key alive.
		e.key = Undefined
		m.sweep()
		m.weakKeys = append(m.weakKeys, makeWeak(k))
	}
	m.entries = append(m.entries, e)
	m.record(mk, len(m.entries)-1)
	m.size++
}

// addWeak records WeakSet membership without retaining the member as an entry
// value. WeakMap uses set because its separately supplied value is strong.
func (m *jsMap) addWeak(r *Runtime, value Value) {
	m.set(r, value, Undefined)
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
	keptWeak := m.weakKeys[:0]
	m.clearIndex()
	m.size = 0
	for i, e := range m.entries {
		w := m.weakKeys[i]
		if e.deleted || !w.alive() {
			continue
		}
		kept = append(kept, e)
		keptWeak = append(keptWeak, w)
		m.record(mapKey{ref: w}, len(kept)-1)
		m.size++
	}
	clear(m.entries[len(kept):])
	clear(m.weakKeys[len(keptWeak):])
	m.entries = kept
	m.weakKeys = keptWeak
	m.nextSweep = 2*len(m.entries) + 16
}

func (m *jsMap) delete(r *Runtime, k Value) bool {
	mk := r.mapKeyOf(k, m.weak)
	i, ok := m.lookup(mk)
	if !ok || m.entries[i].deleted || !m.live(i) {
		return false
	}
	// Tombstone rather than remove, so that an iteration in progress keeps its
	// position in the entry list.
	m.entries[i].deleted = true
	m.entries[i].key = Undefined
	m.entries[i].value = Undefined
	m.forget(mk)
	m.size--
	return true
}

func (m *jsMap) clear() {
	for i := range m.entries {
		m.entries[i].deleted = true
		m.entries[i].key = Undefined
		m.entries[i].value = Undefined
	}
	m.clearIndex()
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
		// The arguments are the same three every time, so the list they go in
		// is made once rather than per entry: a callee may not keep it, any
		// more than it may keep the interpreter's own stack.
		var argv [3]Value
		// The index is re-read each step because the callback may add entries,
		// which the specification requires the iteration to visit.
		for i := 0; i < len(m.entries); i++ {
			if m.entries[i].deleted {
				continue
			}
			argv[0], argv[1], argv[2] = m.entries[i].value, m.entries[i].key, this
			if _, err := rt.call(cb, arg(args, 1), argv[:]); err != nil {
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
		var argv [3]Value
		for i := 0; i < len(m.entries); i++ {
			if m.entries[i].deleted {
				continue
			}
			argv[0], argv[1], argv[2] = m.entries[i].value, m.entries[i].key, this
			if _, err := rt.call(cb, arg(args, 1), argv[:]); err != nil {
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
		// Membership needs only the weak key. Storing v as the entry value would
		// create a second, strong reference and prevent it from ever being
		// collected.
		m.addWeak(rt, v)
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
	p, _ := r.protoFromNewTargetErr(fallback)
	return p
}

// protoFromNewTargetErr is protoFromNewTarget with the error the read may
// produce, which a constructor whose specification reads the property at a
// definite point has to report rather than swallow.
func (r *Runtime) protoFromNewTargetErr(fallback *Object) (*Object, error) {
	nt := r.newTarget()
	if !nt.IsObject() {
		return fallback, nil
	}
	// Recorded so that the construct that called this does not read the same
	// property again on its way out: a prototype getter would see both.
	r.usedNewTargetProto = true
	p, err := r.getProp(nt.Object(), atomPrototype, nt)
	if err != nil {
		return nil, err
	}
	if !p.IsObject() {
		return fallback, nil
	}
	return p.Object(), nil
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
		k, err := r.getValueProp(item, r.atoms.indexAtom(0))
		if err != nil {
			return err
		}
		val, err := r.getValueProp(item, r.atoms.indexAtom(1))
		if err != nil {
			return err
		}
		return visit(k, val)
	})
}
