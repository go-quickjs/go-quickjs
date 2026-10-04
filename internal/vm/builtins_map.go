package vm

// Map, Set, WeakMap and WeakSet.
//
// The weak collections have storage of their own, in weakmap.go and
// weakset.go: a WeakMap's values live on its keys and a WeakSet holds weak
// references only, and neither is iterated. This file has their methods, and
// Map's and Set's storage.
//
// Map and Set share one structure. Keys are compared by SameValueZero, which is
// strict equality except that NaN equals itself, and iteration follows
// insertion order -- both are observable, so the storage is a hash index over
// an entry list in insertion order rather than a plain Go map.
//
// Deletion tombstones an entry instead of removing it, because the
// specification requires that an entry deleted during iteration is skipped
// while the entries around it keep their positions. The tombstones are
// dropped once they are as many as the live entries, or when the index next
// grows; an iterator that was between entries then finds its place again by
// the order the entries were added in (see mapCursor).

import (
	"hash/maphash"
	"math"
	"sort"
)

// mapEntry is one key/value pair. A Set stores the key in both fields, which is
// what its iteration protocol reports.
type mapEntry struct {
	key, value Value
	// seq numbers the entries in the order they were added, from 1. The list
	// stays in that order through every compaction, so an iterator that has
	// lost its place finds it again by the last entry it gave.
	seq uint64
}

// deletedKey is the key of a deleted entry. No script can hold it, so no key
// matches it, and nothing it referred to is kept alive.
var deletedKey = Value{num: mkTag(KindUninitialized, 1)}

func (e *mapEntry) deleted() bool { return e.key.sameBits(deletedKey) }

// mapSlot is one place in a collection's index: the hash of an entry's key,
// and 1 + where the entry is, or 0 in a slot never used.
type mapSlot struct {
	hash uint32
	at   int32
}

// jsMap is the shared storage for Map and Set.
type jsMap struct {
	entries []mapEntry
	// index finds an entry by its key: open addressing with linear probing,
	// over a power-of-two table at most half full. Every entry has a slot,
	// a deleted one too, until the next rebuild drops it; so a lookup never
	// has to tell an emptied slot from a used one, and a delete leaves the
	// index alone.
	//
	// It is the collection's own rather than a Go map because a key's hash
	// and its equality are SameValueZero's: a Go map would need a key struct
	// boxing every kind of key, or one map for each kind.
	index []mapSlot
	size  int
	// seq is the last seq given to an entry.
	seq uint64
	// gen counts the compactions, each of which moves entries; a cursor that
	// last looked at another generation finds its place again.
	gen uint32
}

func newJSMap() *jsMap { return &jsMap{} }

// mapSeed seeds the hash of every key, so that a script cannot pick keys that
// all land in one place of the index without knowing it. It is made once per
// process and never changes, so any number of runtimes may share it.
var mapSeed = maphash.MakeSeed()

// mapSalt is mapSeed for the keys hashed by their bits rather than by
// maphash.
var mapSalt = maphash.String(mapSeed, "jsMap")

// mixHash spreads a word over a hash: murmur3's finalizer, salted.
func mixHash(x uint64) uint32 {
	x ^= mapSalt
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return uint32(x)
}

// keyHash is the hash of a key of a Map or a Set, which canonicalKey has
// made the one form of its value: equal keys hash alike.
//
// A string is hashed by its text and a BigInt by its value, since two of
// either can be the same key and different values. Anything else is the same
// key exactly when its bits are the same: an object or a symbol by its
// address, which Go's collector never moves.
func keyHash(k Value) uint32 {
	bits := math.Float64bits(k.num)
	if bits&tagMask == tagBase && Kind(bits&0xFF) == KindString {
		return uint32(maphash.String(mapSeed, k.String().Go()))
	}
	if bits == heapBigBits {
		// One held in its Value is hashed by its bits, below, which are its
		// value's alone.
		b := &k.BigInt().V
		h := uint64(b.Sign())
		for _, w := range b.Bits() {
			h = (h ^ uint64(w)) * 0x100000001b3
		}
		return mixHash(h)
	}
	return mixHash(bits ^ uint64(uintptr(k.ref))*0x9e3779b97f4a7c15)
}

// sameKey is SameValueZero for an entry's key and a key canonicalKey has made,
// which are the same key when their bits are -- but for strings and BigInts,
// which are compared by what they hold.
func sameKey(a, k Value) bool {
	if a.sameBits(k) {
		return true
	}
	switch k.Kind() {
	case KindString:
		return a.IsString() && a.String().Equals(k.String())
	case KindBigInt:
		// One held in its Value is the same key only as the same bits.
		return a.isHeapBig() && k.isHeapBig() && a.BigInt().V.Cmp(&k.BigInt().V) == 0
	}
	return false
}

// canonicalKey is a key as a Map or Set keeps it: -0 as +0, which
// Map.prototype.set and Set.prototype.add both say, and every NaN as the one.
// The two zeros are the one key, and the one kept is what keys(), forEach and
// the rest hand back.
func canonicalKey(k Value) Value {
	if k.IsNumber() {
		if k.num == 0 {
			return Int(0)
		}
		if k.num != k.num {
			return Float(math.NaN())
		}
	}
	return k
}

// find is where the canonical key's entry is in entries, or -1.
func (m *jsMap) find(k Value, h uint32) int {
	if len(m.index) == 0 {
		return -1
	}
	mask := uint32(len(m.index) - 1)
	for i := h & mask; ; i = (i + 1) & mask {
		s := m.index[i]
		if s.at == 0 {
			return -1
		}
		if s.hash != h {
			continue
		}
		j := int(s.at - 1)
		if sameKey(m.entries[j].key, k) {
			// A deleted entry's key matches nothing.
			return j
		}
	}
}

// lookup is find for any key a script passes.
func (m *jsMap) lookup(k Value) int {
	k = canonicalKey(k)
	return m.find(k, keyHash(k))
}

// hashAt is the hash of entry j's key, which is live.
func (m *jsMap) hashAt(j int) uint32 {
	return keyHash(m.entries[j].key)
}

// place gives entry j a slot in the index, which has a free one.
func (m *jsMap) place(h uint32, j int) {
	mask := uint32(len(m.index) - 1)
	i := h & mask
	for m.index[i].at != 0 {
		i = (i + 1) & mask
	}
	m.index[i] = mapSlot{hash: h, at: int32(j + 1)}
}

// add appends an entry for a key find did not find, with its hash.
func (m *jsMap) add(k, v Value, h uint32) {
	if 2*(len(m.entries)+1) > len(m.index) {
		m.rebuild()
	}
	j := len(m.entries)
	m.seq++
	m.entries = append(m.entries, mapEntry{key: k, value: v, seq: m.seq})
	m.place(h, j)
	m.size++
}

// rebuild drops deleted entries and makes the index again with room for at
// least as many entries again as are left.
//
// It runs when the index is half full, or the entries are half deleted, so
// its cost is a constant per entry added or deleted however large the
// collection grows.
func (m *jsMap) rebuild() {
	if m.size != len(m.entries) {
		m.compact()
	}
	n := 8
	for n < 3*(m.size+1) {
		n <<= 1
	}
	if n == len(m.index) {
		clear(m.index)
	} else {
		m.index = make([]mapSlot, n)
	}
	for j := range m.entries {
		m.place(m.hashAt(j), j)
	}
}

// compact drops the deleted entries, keeping the others in their order.
func (m *jsMap) compact() {
	kept := 0
	for j := range m.entries {
		if m.entries[j].deleted() {
			continue
		}
		m.entries[kept] = m.entries[j]
		kept++
	}
	clear(m.entries[kept:])
	m.entries = m.entries[:kept]
	// A list that has emptied is given back rather than kept at the most it
	// ever held.
	if c := cap(m.entries); c > 64 && c > 4*kept {
		m.entries = append([]mapEntry(nil), m.entries...)
	}
	m.gen++
}

func (m *jsMap) get(r *Runtime, k Value) (Value, bool) {
	if j := m.lookup(k); j >= 0 {
		return m.entries[j].value, true
	}
	return Undefined, false
}

func (m *jsMap) set(r *Runtime, k, v Value) {
	k = canonicalKey(k)
	h := keyHash(k)
	if j := m.find(k, h); j >= 0 {
		// Re-setting an existing key updates the value and keeps its position.
		m.entries[j].value = v
		return
	}
	m.add(k, v, h)
}

func (m *jsMap) delete(k Value) bool {
	j := m.lookup(k)
	if j < 0 {
		return false
	}
	// Tombstone rather than remove, so that an iteration in progress keeps its
	// position in the entry list.
	e := &m.entries[j]
	e.key, e.value = deletedKey, Undefined
	m.size--
	if dead := len(m.entries) - m.size; dead >= 16 && dead > m.size {
		m.rebuild()
	}
	return true
}

func (m *jsMap) clear() {
	// What an iterator in progress sees next is whatever is added from now
	// on, which the new generation tells it.
	clear(m.entries)
	if cap(m.entries) > 64 {
		m.entries, m.index = nil, nil
	} else {
		m.entries = m.entries[:0]
		clear(m.index)
	}
	m.size = 0
	m.gen++
}

// mapCursor is a place in a collection's entries, for walking them in order
// while what the walk calls may add, delete, clear or compact.
//
// A deleted entry stays where it is until a compaction, so the place is an
// index into the list; a compaction moves the entries after it, so the place
// is also the seq of the last entry given, which finds it again in a list
// still in seq order.
type mapCursor struct {
	i    int
	gen  uint32
	last uint64
}

// next is the place in entries of the next live entry, or -1 at the end.
func (m *jsMap) next(c *mapCursor) int {
	if c.gen != m.gen {
		c.gen = m.gen
		c.i = sort.Search(len(m.entries), func(j int) bool { return m.entries[j].seq > c.last })
	}
	for c.i < len(m.entries) {
		j := c.i
		c.i++
		if !m.entries[j].deleted() {
			c.last = m.entries[j].seq
			return j
		}
	}
	return -1
}

// mapUpsertStore is a Map's storage or a WeakMap's, which getOrInsertComputed
// serves both of.
type mapUpsertStore interface {
	get(*Runtime, Value) (Value, bool)
	set(*Runtime, Value, Value)
}

// getOrInsertComputed is the shared body of Map's and WeakMap's method once
// the receiver, the key and the callback have been checked.
//
// The callback may itself have added the key, and its result still wins: set
// overwrites the value in the position the callback gave the entry.
func (r *Runtime) getOrInsertComputed(m mapUpsertStore, key, cb Value) (Value, error) {
	if v, ok := m.get(r, key); ok {
		return v, nil
	}
	v, err := r.call(cb, Undefined, []Value{key})
	if err != nil {
		return Undefined, err
	}
	m.set(r, key, v)
	return v, nil
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

// weakMapOf is mapOf for a WeakMap.
func (r *Runtime) weakMapOf(this Value, name string) (*weakMap, error) {
	if !this.IsObject() || this.Object().class != ClassWeakMap {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	m, ok := this.Object().data.(*weakMap)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized collection", name)
	}
	return m, nil
}

// weakSetOf is mapOf for a WeakSet.
func (r *Runtime) weakSetOf(this Value, name string) (*weakSet, error) {
	if !this.IsObject() || this.Object().class != ClassWeakSet {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	s, ok := this.Object().data.(*weakSet)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized collection", name)
	}
	return s, nil
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
		proto, err := rt.protoFromNewTargetErr(rt.proto.mapProto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassMap)
		o.data = newJSMap()
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
		m := newJSMap()
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
	r.defMethod(p, "getOrInsert", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.getOrInsert")
		if err != nil {
			return Undefined, err
		}
		key := normalizeZero(arg(args, 0))
		if v, ok := m.get(rt, key); ok {
			return v, nil
		}
		v := arg(args, 1)
		m.set(rt, key, v)
		return v, nil
	})
	r.defMethod(p, "getOrInsertComputed", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.mapOf(this, ClassMap, "Map.prototype.getOrInsertComputed")
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 1)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("Map.prototype.getOrInsertComputed requires a function")
		}
		key := normalizeZero(arg(args, 0))
		return rt.getOrInsertComputed(m, key, cb)
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
		return Bool(m.delete(arg(args, 0))), nil
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
		// The cursor takes each step from where the collection now is,
		// because the callback may add entries, which the specification
		// requires the iteration to visit, or delete them.
		var c mapCursor
		for i := m.next(&c); i >= 0; i = m.next(&c) {
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
		proto, err := rt.protoFromNewTargetErr(rt.proto.setProto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassSet)
		o.data = newJSMap()
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
		v := canonicalKey(arg(args, 0))
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
		return Bool(m.delete(arg(args, 0))), nil
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
		var c mapCursor
		for i := m.next(&c); i >= 0; i = m.next(&c) {
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
	// WeakMap and WeakSet accept objects and unregistered symbols as keys.
	wmProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakMap", 0, wmProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("WeakMap"); err != nil {
			return Undefined, err
		}
		proto, err := rt.protoFromNewTargetErr(wmProto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassWeakMap)
		o.data = newWeakMap()
		// An iterable of [key, value] pairs populates it, exactly as for Map.
		if err := rt.seedFromEntries(o, arg(args, 0), "set"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defMethod(wmProto, "get", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.get")
		if err != nil {
			return Undefined, err
		}
		v, _ := m.get(rt, arg(args, 0))
		return v, nil
	})
	r.defMethod(wmProto, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.set")
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
	r.defMethod(wmProto, "getOrInsert", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.getOrInsert")
		if err != nil {
			return Undefined, err
		}
		k := arg(args, 0)
		if !rt.canBeWeak(k) {
			return Undefined, rt.throwTypeError(
				"a WeakMap key must be an object or an unregistered symbol")
		}
		if v, ok := m.get(rt, k); ok {
			return v, nil
		}
		v := arg(args, 1)
		m.set(rt, k, v)
		return v, nil
	})
	r.defMethod(wmProto, "getOrInsertComputed", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.getOrInsertComputed")
		if err != nil {
			return Undefined, err
		}
		// The key is checked before the callback, the reverse of Map, whose
		// every key is acceptable.
		k := arg(args, 0)
		if !rt.canBeWeak(k) {
			return Undefined, rt.throwTypeError(
				"a WeakMap key must be an object or an unregistered symbol")
		}
		cb := arg(args, 1)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("WeakMap.prototype.getOrInsertComputed requires a function")
		}
		return rt.getOrInsertComputed(m, k, cb)
	})
	r.defMethod(wmProto, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.has")
		if err != nil {
			return Undefined, err
		}
		_, ok := m.get(rt, arg(args, 0))
		return Bool(ok), nil
	})
	r.defMethod(wmProto, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		m, err := rt.weakMapOf(this, "WeakMap.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(m.delete(arg(args, 0))), nil
	})
	r.defToStringTag(wmProto, "WeakMap")

	wsProto := newObject(r.proto.object, ClassObject)
	r.newCtor("WeakSet", 0, wsProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("WeakSet"); err != nil {
			return Undefined, err
		}
		proto, err := rt.protoFromNewTargetErr(wsProto)
		if err != nil {
			return Undefined, err
		}
		o := newObject(proto, ClassWeakSet)
		o.data = newWeakSet()
		if err := rt.seedFromValues(o, arg(args, 0), "add"); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
	r.defMethod(wsProto, "add", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.weakSetOf(this, "WeakSet.prototype.add")
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		if !rt.canBeWeak(v) {
			return Undefined, rt.throwTypeError(
				"a WeakSet value must be an object or an unregistered symbol")
		}
		// The set keeps only a weak reference to v, so being a member keeps
		// nothing alive.
		s.add(v)
		return this, nil
	})
	r.defMethod(wsProto, "has", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.weakSetOf(this, "WeakSet.prototype.has")
		if err != nil {
			return Undefined, err
		}
		return Bool(s.has(arg(args, 0))), nil
	})
	r.defMethod(wsProto, "delete", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.weakSetOf(this, "WeakSet.prototype.delete")
		if err != nil {
			return Undefined, err
		}
		return Bool(s.delete(arg(args, 0))), nil
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
// The cursor is a place in the entry list rather than a snapshot, so an
// entry added during iteration is visited and one deleted during it is skipped,
// which is what the specification requires.
type mapIterData struct {
	m    *jsMap
	kind mapIterKind
	at   mapCursor
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
	next := r.defMethod(proto, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.mapIterOf(this, tag)
		if err != nil {
			return Undefined, err
		}
		v, ok := rt.mapIterStep(d)
		return Obj(rt.iterResult(v, !ok)), nil
	})
	next.fn().iterNext = iterNextMap
	proto.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString(tag)), propConfigurable)
}

// mapIterStep is a Map or Set iterator's next, as its value and false once it
// is done, without the result object.
func (r *Runtime) mapIterStep(d *mapIterData) (Value, bool) {
	i := -1
	if !d.done {
		i = d.m.next(&d.at)
	}
	if i < 0 {
		d.done = true
		return Undefined, false
	}
	e := d.m.entries[i]
	switch d.kind {
	case mapIterKeys:
		return e.key, true
	case mapIterValues:
		return e.value, true
	}
	return Obj(r.newArrayFrom([]Value{e.key, e.value})), true
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

// protoFromNewTargetErr resolves the prototype a constructed object should
// have, which is GetPrototypeFromConstructor: new.target's prototype, or the
// fallback's counterpart in new.target's realm.
//
// A subclass's instance needs it before the constructor finishes, because the
// constructor reads the method it adds entries through from the object -- and
// that method is the subclass's when the subclass overrode it.
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
		// The fallback is new.target's realm's, which is not always the
		// constructor's.
		return r.protoForNewTarget(nt.Object(), fallback)
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
