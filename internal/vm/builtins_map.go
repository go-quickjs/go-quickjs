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
func (r *Runtime) mapKeyOf(v Value) mapKey {
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
}

// jsMap is the shared storage for all four collection types.
type jsMap struct {
	entries []mapEntry
	index   map[mapKey]int
	size    int
	// weak marks a WeakMap or WeakSet. The references are still strong: Go's
	// garbage collector has no way to tell the engine that a key became
	// unreachable, so entries live until deleted. That is observable only as
	// memory retention, never as behaviour.
	weak bool
}

func newJSMap(weak bool) *jsMap {
	return &jsMap{index: make(map[mapKey]int), weak: weak}
}

func (m *jsMap) get(r *Runtime, k Value) (Value, bool) {
	i, ok := m.index[r.mapKeyOf(k)]
	if !ok || m.entries[i].deleted {
		return Undefined, false
	}
	return m.entries[i].value, true
}

func (m *jsMap) set(r *Runtime, k, v Value) {
	mk := r.mapKeyOf(k)
	if i, ok := m.index[mk]; ok && !m.entries[i].deleted {
		// Re-setting an existing key updates the value and keeps its position.
		m.entries[i].value = v
		return
	}
	m.entries = append(m.entries, mapEntry{key: k, value: v})
	m.index[mk] = len(m.entries) - 1
	m.size++
}

func (m *jsMap) delete(r *Runtime, k Value) bool {
	mk := r.mapKeyOf(k)
	i, ok := m.index[mk]
	if !ok || m.entries[i].deleted {
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
	r.proto.mapProto = newObject(r.proto.object, ClassObject)
	p := r.proto.mapProto

	ctor := r.newCtor("Map", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(rt.proto.mapProto, ClassMap)
		m := newJSMap(false)
		o.data = m
		// An iterable argument seeds the map with its [key, value] pairs.
		if src := arg(args, 0); !src.IsNullish() {
			err := rt.iterate(src, func(entry Value) error {
				k, err := rt.getIndexed(entry, Int(0))
				if err != nil {
					return err
				}
				v, err := rt.getIndexed(entry, Int(1))
				if err != nil {
					return err
				}
				m.set(rt, k, v)
				return nil
			})
			if err != nil {
				return Undefined, err
			}
		}
		return Obj(o), nil
	})
	r.defSpecies(ctor)
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
	// A Map iterates as its entries.
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			m, err := rt.mapOf(this, ClassMap, "Map.prototype[Symbol.iterator]")
			if err != nil {
				return Undefined, err
			}
			return rt.newMapIterator(m, mapIterEntries), nil
		})
	r.defToStringTag(p, "Map")
}

func (r *Runtime) initSetBuiltins() {
	r.proto.setProto = newObject(r.proto.object, ClassObject)
	p := r.proto.setProto

	setCtor := r.newCtor("Set", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(rt.proto.setProto, ClassSet)
		m := newJSMap(false)
		o.data = m
		if src := arg(args, 0); !src.IsNullish() {
			err := rt.iterate(src, func(v Value) error {
				m.set(rt, v, v)
				return nil
			})
			if err != nil {
				return Undefined, err
			}
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
	r.defMapIterator(p, ClassSet, "keys", mapIterValues)
	r.defMapIterator(p, ClassSet, "entries", mapIterEntries)
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			m, err := rt.mapOf(this, ClassSet, "Set.prototype[Symbol.iterator]")
			if err != nil {
				return Undefined, err
			}
			return rt.newMapIterator(m, mapIterValues), nil
		})
	r.initSetOps(p)
	r.defToStringTag(p, "Set")
}

func (r *Runtime) initWeakCollections() {
	// WeakMap and WeakSet accept only object keys, which is the one behaviour
	// that distinguishes them here.
	wmProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakMap", 0, wmProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(wmProto, ClassWeakMap)
		o.data = newJSMap(true)
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
		if !k.IsObject() && !k.IsSymbol() {
			return Undefined, rt.throwTypeError("a WeakMap key must be an object")
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
		o := newObject(wsProto, ClassWeakSet)
		o.data = newJSMap(true)
		return Obj(o), nil
	})
	r.defMethod(wsProto, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassWeakSet, "WeakSet.prototype.add")
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		if !v.IsObject() && !v.IsSymbol() {
			return Undefined, rt.throwTypeError("a WeakSet value must be an object")
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
		return rt.newMapIterator(m, kind), nil
	})
}

// newMapIterator returns an iterator over a collection's live entries.
//
// The cursor is an index into the entry list rather than a snapshot, so an
// entry added during iteration is visited and one deleted during it is skipped,
// which is what the specification requires.
func (r *Runtime) newMapIterator(m *jsMap, kind mapIterKind) Value {
	i := 0
	iter := newObject(r.proto.iterator, ClassIterator)
	r.defMethod(iter, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		for i < len(m.entries) && m.entries[i].deleted {
			i++
		}
		res := newObject(rt.proto.object, ClassObject)
		if i >= len(m.entries) {
			res.setOwnRaw(atomValue, Undefined, propDefault)
			res.setOwnRaw(atomDone, True, propDefault)
			return Obj(res), nil
		}
		e := m.entries[i]
		i++

		var v Value
		switch kind {
		case mapIterKeys:
			v = e.key
		case mapIterValues:
			v = e.value
		default:
			v = Obj(rt.newArrayFrom([]Value{e.key, e.value}))
		}
		res.setOwnRaw(atomValue, v, propDefault)
		res.setOwnRaw(atomDone, False, propDefault)
		return Obj(res), nil
	})
	r.defSymbolMethod(iter, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	return Obj(iter)
}
