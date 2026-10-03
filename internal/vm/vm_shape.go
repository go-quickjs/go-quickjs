package vm

import "unsafe"

// A shape is an object's layout -- the keys of its property table, their
// attributes and their order -- as an identity that can be compared by
// pointer. Two objects with the same shape have the same keys with the same
// attributes at the same positions, so a property found at one position in one
// of them is at that position in the other: a property access remembers the
// shape it last saw and where the property was, and the next object of that
// shape needs no search. It is what V8 calls a map and QuickJS a shape.
//
// An object's shape is one of three kinds.
//
//   - None (nil): the object has no identity a cache can use. An object of a
//     class a cache cannot follow -- a proxy, a typed array, an arguments
//     object -- stays so.
//   - Shared: the object's layout is a node in its runtime's tree of shapes,
//     reached from a root by one transition per property added, in the order
//     they were added. Objects built the same way share the node. An object
//     literal's objects start from a root of the literal's own, a
//     constructor's from one of the function's, and any other object from
//     its class's -- when it is given its first property, or when a cache
//     first meets it with none.
//   - Unique: the object has a shape of its own. It is what an object gets
//     whose layout changes other than by adding a property -- a delete, an
//     attribute changed, a table grown past what the tree holds -- and what a
//     prototype made with the runtime has. Its identity is replaced whenever
//     its layout changes, so a cache that remembered it misses.
//
// The table's lookup index, for an object with more properties than are
// scanned, lives in the shape: a shared shape's is shared by every object of
// it, since their layouts are the same, and a unique shape's is the object's.
type shape struct {
	tree   *shapeTree
	parent *shape
	key    Atom
	flags  propFlags
	unique bool
	// n is how many properties the layout has.
	n int32
	// next and nextEdge are the first transition out of the shape, which is
	// the only one most shapes ever have; more holds the rest.
	next     *shape
	nextEdge shapeEdge
	more     map[shapeEdge]*shape
	index    *propIndex
	// forIn is what a for-in loop visits on an object of a shared shape,
	// made the first time one is walked; see startForIn.
	forIn *forInKeys
}

// forInKeys is the keys a for-in loop visits, as the strings it hands out
// and the atoms they name.
type forInKeys struct {
	keys  []Value
	atoms []Atom
	// slots is each key's place in the table, which the layout fixes.
	slots []int32
	// copyable says that every enumerable property of the layout, string or
	// symbol keyed, is one of these and holds data: what an object spread
	// copies is then the values at slots.
	copyable bool
}

// shapeEdge is a property added: a transition's key.
type shapeEdge struct {
	key   Atom
	flags propFlags
}

// shapeTree is a runtime's shapes. It is the runtime's own: atoms are, and
// runtimes run on separate goroutines.
type shapeTree struct {
	roots [ClassJSONObject + 1]*shape
	// count bounds the tree: past maxShapes, an object that would have
	// needed a new shape is given a unique one instead, so that a program
	// making objects with keys of its own invention cannot grow the tree
	// without end.
	count int
	// building says the runtime is making a realm: what is made then is one
	// of a kind, and gets a unique shape.
	building bool
	// shared is the cache of the reads that have given up their own, made
	// when one first does.
	shared *sharedCache
}

const (
	// maxShapeDepth is the most properties a shared layout has; a larger
	// object's is unique.
	maxShapeDepth = 64
	// maxShapeFanout is the most transitions out of one shape. More means
	// keys chosen at run time, which a cache would not keep up with.
	maxShapeFanout = 64
	// maxShapes is the most shapes a tree holds.
	maxShapes = 1 << 16
)

// noShape is what an empty cache entry holds: no object has it.
var noShape = &shape{}

// shapeClass reports whether objects of a class may have a shape: whether a
// cache that has found a property on one can find it on another of the same
// layout without asking the class. The others answer some keys themselves or
// intercept every access. An array and a function synthesize a few keys, which
// synthesized names, and are shaped for the rest.
//
// The classes kept out are the ones whose objects answer keys of their own: a
// String wrapper its length and characters, a typed array its elements and
// numeric strings, a module namespace its exports, which it can be asked to
// evaluate, and a proxy all of them. An arguments object is kept out as well.
// Every other class -- a WeakMap, a generator, an iterator, an ArrayBuffer --
// has nothing for a key but its table, and a method read on one is as worth
// remembering as on a Map.
func shapeClass(c Class) bool {
	switch c {
	case ClassStringWrapper, ClassTypedArray, ClassModuleNamespace, ClassProxy, ClassArguments:
		return false
	}
	return true
}

// root is the empty layout of a class.
func (t *shapeTree) root(c Class) *shape {
	s := t.roots[c]
	if s == nil {
		s = &shape{tree: t}
		t.roots[c] = s
		t.count++
	}
	return s
}

// siteRoot is the empty shape the objects an object literal makes start from,
// kept in the literal's cache. Objects made at different sites mostly have
// different properties, and a root of their own saves each of them a search
// among the others' transitions out of their class's root. It is nil once the
// tree is full, or while the realm is being made.
func (t *shapeTree) siteRoot(c *propCache) *shape {
	if s := c.shape; s != noShape {
		return s
	}
	var s *shape
	if !t.building && t.count < maxShapes {
		s = &shape{tree: t}
		t.count++
	}
	c.shape = s
	return s
}

// ctorRoot is siteRoot for a constructor, whose objects start from a shape of
// the function's own: functions made from the same code are often different
// classes -- a library's class maker returns one -- whose objects have
// different properties.
func (t *shapeTree) ctorRoot(fd *funcData) *shape {
	if s := fd.ctorShape; s != nil {
		return s
	}
	if t.building || t.count >= maxShapes {
		return nil
	}
	s := &shape{tree: t}
	t.count++
	fd.ctorShape = s
	return s
}

// newUniqueShape makes a shape of its own for an object of n properties,
// keeping its index.
func newUniqueShape(t *shapeTree, n int, index *propIndex) *shape {
	return &shape{tree: t, unique: true, n: int32(n), index: index}
}

// child is the shape one property added makes, or nil where the tree will not
// hold it.
func (s *shape) child(e shapeEdge) *shape {
	if s.next != nil {
		if s.nextEdge == e {
			return s.next
		}
		if c := s.more[e]; c != nil {
			return c
		}
	}
	t := s.tree
	if s.n >= maxShapeDepth || t.count >= maxShapes || len(s.more) >= maxShapeFanout {
		return nil
	}
	c := &shape{tree: t, parent: s, key: e.key, flags: e.flags, n: s.n + 1}
	t.count++
	if s.next == nil {
		s.next, s.nextEdge = c, e
	} else {
		if s.more == nil {
			s.more = map[shapeEdge]*shape{}
		}
		s.more[e] = c
	}
	return c
}

// shapeIndex is the lookup index of the object's table, or nil for one small
// enough to scan.
func (o *Object) shapeIndex() *propIndex {
	if s := o.shape; s != nil {
		return s.index
	}
	return nil
}

// shapeAdded brings the object's shape up to the property just appended to its
// table.
func (o *Object) shapeAdded(p Property) {
	s := o.shape
	n := len(o.props)
	if s == nil {
		// A first property: an object whose prototype has a tree joins it,
		// at the root for its class -- or, while the realm is being made,
		// gets a layout of its own, being one of a kind.
		if n == 1 && shapeClass(o.class) && o.proto != nil && o.proto.shape != nil {
			t := o.proto.shape.tree
			if t.building {
				o.shape = newUniqueShape(t, 1, nil)
				return
			}
			if c := t.root(o.class).child(shapeEdge{p.key, p.flags}); c != nil {
				o.shape = c
				return
			}
			o.shape = newUniqueShape(t, 1, nil)
			return
		}
		if n > linearScanLimit {
			// No tree to join, but a table that needs an index: a layout
			// of its own holds it, which no cache will see.
			o.shape = &shape{unique: true, n: int32(n)}
			o.buildIndex()
		}
		return
	}
	if s.unique {
		if s.tree == nil || !s.tree.building {
			// A new identity, so that what was cached of the old one misses.
			s = newUniqueShape(s.tree, n, s.index)
			o.shape = s
		} else {
			s.n = int32(n)
		}
		if s.index != nil {
			s.index.add(p.key, int32(n-1))
		} else if n > linearScanLimit {
			o.buildIndex()
		}
		return
	}
	if c := s.child(shapeEdge{p.key, p.flags}); c != nil {
		o.shape = c
		if n > linearScanLimit && c.index == nil {
			// The objects of this shape all have this layout, so the index
			// built from this one is theirs.
			o.buildIndex()
		}
		return
	}
	o.shape = newUniqueShape(s.tree, n, nil)
	if n > linearScanLimit {
		o.buildIndex()
	}
}

// transition is the shape the object's would become with key added with the
// given attributes, where its layout is shared and was last extended so -- the
// common case of objects built alike -- or nil. A transition out of a layout
// is for a key the layout does not have, so the object does not have it.
func (o *Object) transition(key Atom, flags propFlags) *shape {
	if s := o.shape; s != nil && s.next != nil && s.nextEdge == (shapeEdge{key, flags}) {
		return s.next
	}
	return nil
}

// appendTransition appends a property the object is known not to have,
// giving it the shape transition found for it.
func (o *Object) appendTransition(p Property, next *shape) {
	if o.flags&objInlineProps != 0 && len(o.props) == cap(o.props) {
		o.leaveInlineProps(1)
	}
	o.props = append(o.props, p)
	o.shape = next
	if next.index == nil && len(o.props) > linearScanLimit {
		o.buildIndex()
	}
	if verifyShapes {
		checkShape(o)
	}
}

// layoutChanged gives the object a layout of its own before its table changes
// other than by an append -- a property deleted or moved, an attribute changed
// -- so that no cache takes it for what it was. The index is kept, which the
// change then keeps up to date.
func (o *Object) layoutChanged() {
	s := o.shape
	if s == nil {
		return
	}
	var index *propIndex
	if s.unique {
		// A shared index is the layout's, which this object is leaving:
		// it gets one of its own, built below.
		index = s.index
	}
	o.shape = newUniqueShape(s.tree, len(o.props), index)
	if index == nil && len(o.props) > linearScanLimit {
		o.buildIndex()
	}
}

// propCache is a property read's memory of where it last found its property:
// the shape of the object it read, and the table position, in that object or
// in a prototype one or two up, that the property was at. An object of that
// shape, whose prototypes are the same objects with the same shapes, has the
// property at that position -- its layout says it has no property of the name
// itself, or has it there -- so the read is a comparison or three and a load.
//
// Only a plain data property is remembered: an accessor's getter is called,
// which a cache would not save much of.
//
// A write's cache is either of a property the object has, at idx, or of one
// it adds: next is then the shape the addition makes, and p1 and p2 are the
// whole prototype chain -- none, one or two objects -- with the shapes that
// said none of them has a setter or a read-only property of the name to
// intercept the write.
type propCache struct {
	shape  *shape
	next   *shape
	p1, p2 *Object
	s1, s2 *shape
	idx    int32
	// getter marks a read's entry for an accessor, whose getter is called:
	// only cachedGet fills one, for OpGetProp.
	getter bool
	// fills counts the times the entry has been filled. A site that keeps
	// meeting new shapes stops trying: filling costs a lookup of its own. A
	// read then uses the runtime's shared cache.
	fills uint8
}

// cachedProp is a property read of o that its cache answers. It reports false
// for anything else, which is left to getValueProp, having filled the cache
// for the next read where it can.
func (r *Runtime) cachedProp(c *propCache, o *Object, key Atom) (Value, bool) {
	if h := c.holder(o); h != nil {
		if verifyShapes {
			r.verifyPropCache(o, h, c, key)
		}
		return h.props[c.idx].value, true
	}
	if c.fills >= maxCacheFills {
		return r.sharedProp(o, key)
	}
	r.fillPropCache(c, o, key, false)
	return Undefined, false
}

// cachedGet is cachedProp for OpGetProp, whose cache may also remember an
// accessor: a hit on one calls the getter, with o as this, as a call from the
// interpreter's loop is made. A class's getter is read as often as a field.
// It reports false for anything the cache does not answer, which is left to
// getValueProp, and an error only with true.
func (r *Runtime) cachedGet(c *propCache, o *Object, key Atom) (Value, bool, error) {
	if h := c.holder(o); h != nil {
		p := &h.props[c.idx]
		if !c.getter {
			if verifyShapes {
				r.verifyPropCache(o, h, c, key)
			}
			return p.value, true, nil
		}
		if verifyShapes {
			checkShape(o)
			checkShape(h)
			if p.key != key || p.flags&propAccessor == 0 {
				panic("property cache's accessor is not where it was for " + r.atoms.name(key))
			}
		}
		a := p.getterSetter()
		if a == nil || a.getter == nil {
			return Undefined, true, nil
		}
		v, err := r.callFromLoop(Obj(a.getter), Obj(o), nil)
		return v, true, err
	}
	if c.fills >= maxCacheFills {
		v, ok := r.sharedProp(o, key)
		return v, ok, nil
	}
	r.fillPropCache(c, o, key, true)
	return Undefined, false, nil
}

// own is the value of the property the cache says o has itself: o is of the
// shape it remembers, and the property is its own and holds data. The shape
// settles the layout and the attributes, so the value is where the cache
// says. It is small enough to be inlined where a read is made, and asks
// less than scanning even a small table.
func (c *propCache) own(o *Object) (Value, bool) {
	if s := o.shape; s == c.shape && s != nil && c.p1 == nil && !c.getter {
		return o.props[c.idx].value, true
	}
	return Undefined, false
}

// storeOwn writes v to the property a write site's cache says o has: o is of
// the shape it remembers, and the write is to a property the shape already
// has, writable. It is small enough to be inlined where a write is made; it
// reports false for anything else, which setPropCached does.
func (c *propCache) storeOwn(o *Object, v Value) bool {
	if s := o.shape; s == c.shape && s != nil && c.next == nil {
		if verifyShapes {
			checkShape(o)
		}
		o.props[c.idx].value = v
		return true
	}
	return false
}

// noteOwn remembers a plain own property a read found at index i of o's
// table, for own to answer the next object of its shape -- unless the site
// has met too many shapes to keep trying.
func (c *propCache) noteOwn(o *Object, i int32) {
	if c.fills < maxCacheFills && o.shape != nil {
		*c = propCache{shape: o.shape, idx: i, fills: c.fills + 1}
	}
}

// ownScan is a plain own property of an ordinary object's small table, which
// the cache then remembers for objects of o's shape.
func ownScan(c *propCache, o *Object, key Atom) (Value, bool) {
	v, i, ok := plainOwnAt(o, key)
	if ok {
		c.noteOwn(o, i)
	}
	return v, ok
}

// holder is the object the cache says has the property for o, or nil if o is
// not what the cache remembers.
func (c *propCache) holder(o *Object) *Object {
	if o.shape != c.shape {
		return nil
	}
	p := c.p1
	if p == nil {
		return o
	}
	if o.proto != p || p.shape != c.s1 {
		return nil
	}
	q := c.p2
	if q == nil {
		return p
	}
	if p.proto != q || q.shape != c.s2 {
		return nil
	}
	return q
}

// sharedCache is a runtime's cache for the reads whose own caches have given
// up, which meet objects of many shapes: one read of a method every class
// calls, say. It is what V8 calls the megamorphic stub cache: an entry is
// found by the shape and the key together, so it serves every such read.
type sharedCache [sharedCacheSize]sharedEntry

const sharedCacheSize = 512

type sharedEntry struct {
	key Atom
	c   propCache
}

// sharedProp is cachedProp for a read whose own cache has given up, answered
// by the runtime's shared cache, and filling it.
func (r *Runtime) sharedProp(o *Object, key Atom) (Value, bool) {
	s := r.ensureShape(o)
	if s == nil || key.IsIndex() {
		return Undefined, false
	}
	t := r.shapes
	if t.shared == nil {
		t.shared = new(sharedCache)
	}
	e := &t.shared[(uintptr(unsafe.Pointer(s))>>4^uintptr(key)*0x9e3779b1)%sharedCacheSize]
	if e.key == key {
		if h := e.c.holder(o); h != nil {
			if verifyShapes {
				r.verifyPropCache(o, h, &e.c, key)
			}
			return h.props[e.c.idx].value, true
		}
	}
	var c propCache
	r.fillPropCache(&c, o, key, false)
	if c.fills == 0 {
		return Undefined, false
	}
	e.key, e.c = key, c
	return c.holder(o).props[c.idx].value, true
}

// maxCacheFills is how many times a cache is filled before it gives up.
const maxCacheFills = 16

// newPropCaches makes the caches of a function's n property reads and writes
// and object literals, which are numbered from 1.
func newPropCaches(n int) []propCache {
	ic := make([]propCache, n+1)
	for i := range ic {
		ic[i].shape = noShape
	}
	return ic
}

// synthesized reports whether objects of a class answer a key themselves,
// before their tables: an array's length, a function's name, length and
// prototype, which are made on demand.
func synthesized(c Class, key Atom) bool {
	switch c {
	case ClassArray:
		return key == atomLength
	case ClassFunction:
		return key == atomLength || key == atomName || key == atomPrototype
	}
	return false
}

// fillPropCache remembers where a property read found key on o, where the
// read can be remembered: o and the prototypes searched have shapes of
// classes whose tables are all there is to them for the key, and what is
// found is a plain data property no more than two prototypes up. It only
// looks; the read itself is done by the caller.
//
// With accessors set, an accessor property is remembered too, for cachedGet.
func (r *Runtime) fillPropCache(c *propCache, o *Object, key Atom, accessors bool) {
	if c.fills >= maxCacheFills || key.IsIndex() || r.ensureShape(o) == nil {
		return
	}
	var up [2]*Object
	h := o
	for depth := 0; ; depth++ {
		if h.shape == nil || !shapeClass(h.class) || synthesized(h.class, key) {
			return
		}
		if i := h.findOwn(key); i >= 0 {
			getter := h.props[i].flags&propAccessor != 0
			if h.props[i].flags&(propPrivate|propDeleted|propUninit) != 0 || getter && !accessors {
				return
			}
			c.fills++
			*c = propCache{shape: o.shape, idx: i, fills: c.fills, getter: getter}
			if depth >= 1 {
				c.p1, c.s1 = up[0], up[0].shape
			}
			if depth == 2 {
				c.p2, c.s2 = up[1], up[1].shape
			}
			return
		}
		if depth == 2 || h.proto == nil {
			return
		}
		h = h.proto
		r.ensureProtoShape(h)
		up[depth] = h
	}
}

// setPropCached is setValueProp for an object, with the site's cache: a write
// the cache answers is done here, and anything else is done as usual and then
// remembered where it can be.
func (r *Runtime) setPropCached(c *propCache, o *Object, key Atom, v Value, strict bool) error {
	if s := o.shape; s == c.shape {
		if c.next == nil {
			if verifyShapes {
				checkShape(o)
			}
			o.props[c.idx].value = v
			return nil
		}
		if c.adds(o) {
			if verifyShapes {
				r.verifyStoreCache(c, o, key)
			}
			o.appendTransition(Property{key: key, flags: c.next.flags, value: v}, c.next)
			return nil
		}
	}
	before := o.shape
	if before == nil && c.fills < maxCacheFills {
		before = r.ensureShape(o)
	}
	if _, err := r.setProp(o, key, v, Obj(o), strict); err != nil {
		return err
	}
	if before != nil && c.fills < maxCacheFills && !key.IsIndex() && shapeClass(o.class) && !synthesized(o.class, key) {
		r.fillStoreCache(c, o, before, key)
	}
	return nil
}

// adds reports whether the property a write cache says is added may be
// added to o, which has the shape the cache remembers: o is extensible, and
// its prototype chain is the one found to have no setter or read-only
// property of the name to intercept the write.
func (c *propCache) adds(o *Object) bool {
	return o.flags&objExtensible != 0 && o.proto == c.p1 && (c.p1 == nil || c.p1.shape == c.s1 && c.p1.proto == c.p2) &&
		(c.p2 == nil || c.p2.shape == c.s2 && c.p2.proto == nil)
}

// verifyStoreCache checks a write the cache answers for o, of key, in a build
// with the quickjs_verify tag: o's layout is its shape's, and nothing up its
// prototype chain intercepts an addition of the name.
func (r *Runtime) verifyStoreCache(c *propCache, o *Object, key Atom) {
	checkShape(o)
	if c.next == nil {
		if p := o.props[c.idx]; p.key != key || p.flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) != propWritable {
			panic("write cache's property is not a plain writable " + r.atoms.name(key))
		}
		return
	}
	for p := o.proto; p != nil; p = p.proto {
		checkShape(p)
		if !passesWrite(p, key) {
			panic("write cache adds " + r.atoms.name(key) + " past a prototype that intercepts it")
		}
	}
}

// cachedData is a read of o's key that its cache answers with a plain data
// property, for a read that may not call a getter: an accessor's entry,
// which cachedGet keeps, is no answer. A cache that has given up leaves the
// read to the runtime's shared cache, as cachedGet's does. It reports false
// for anything else, and fills nothing of the site's.
func (r *Runtime) cachedData(c *propCache, o *Object, key Atom) (Value, bool) {
	if h := c.holder(o); h != nil {
		if c.getter {
			return Undefined, false
		}
		if verifyShapes {
			r.verifyPropCache(o, h, c, key)
		}
		return h.props[c.idx].value, true
	}
	if c.fills >= maxCacheFills {
		return r.sharedProp(o, key)
	}
	return Undefined, false
}

// ensureShape gives an object with an empty table, of a class that may have a
// shape, its class's empty shape, which is its layout, so that a cache can
// remember it: most arrays and functions never have a property of their own
// added, which is what gives an object a shape otherwise. It returns the
// object's shape, or nil.
func (r *Runtime) ensureShape(o *Object) *shape {
	if s := o.shape; s != nil || len(o.props) != 0 || !shapeClass(o.class) || r.shapes.building {
		return s
	}
	o.shape = r.shapes.root(o.class)
	return o.shape
}

// ensureProtoShape is ensureShape for an object a cache meets as a prototype.
// One that has properties and no shape got its first while its own prototype
// had none -- as %MapIteratorPrototype% did, made in the realm before
// %IteratorPrototype% was filled in -- and is given a layout of its own,
// which changes with its table, so that the methods on it can be remembered.
// A receiver is not: an object that never had a shape, such as one with a
// null prototype used as a dictionary, would then pay for a new layout at
// every key added to it.
func (r *Runtime) ensureProtoShape(o *Object) *shape {
	if o.shape == nil && len(o.props) != 0 && shapeClass(o.class) && !r.shapes.building {
		o.shape = newUniqueShape(r.shapes, len(o.props), nil)
		return o.shape
	}
	return r.ensureShape(o)
}

// passesWrite reports whether a prototype lets an assignment of key to an
// object inheriting from it add the property to the object: it has no such
// property, or a plain writable one, which the new one shadows -- not a setter,
// and not a read-only one.
func passesWrite(p *Object, key Atom) bool {
	i := p.findOwn(key)
	return i < 0 || p.props[i].flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable
}

// fillStoreCache remembers a write of key to o, whose shape was before: a
// write to a plain writable property o had, or the addition of one along a
// shared transition with a prototype chain the cache can check.
func (r *Runtime) fillStoreCache(c *propCache, o *Object, before *shape, key Atom) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	if o.shape == before {
		if i := o.findOwn(key); i >= 0 && o.props[i].flags&(plain|propWritable) == propWritable {
			c.fills++
			*c = propCache{shape: before, idx: i, fills: c.fills}
		}
		return
	}
	next := o.shape
	if before.unique || next == nil || next.unique || next.parent != before || next.key != key ||
		next.flags&(plain|propWritable) != propWritable {
		return
	}
	var up [2]*Object
	p := o.proto
	for depth := 0; p != nil; depth++ {
		if depth == 2 || r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
			return
		}
		up[depth] = p
		p = p.proto
	}
	c.fills++
	*c = propCache{shape: before, next: next, fills: c.fills, p1: up[0], p2: up[1]}
	if up[0] != nil {
		c.s1 = up[0].shape
	}
	if up[1] != nil {
		c.s2 = up[1].shape
	}
}

// verifyPropCache checks a cache's answer against the read it stands for, and
// the layout of the object read against its shape. It runs in a build with
// the quickjs_verify tag, which is what the conformance suite is run with to
// catch a table changed without its shape.
func (r *Runtime) verifyPropCache(o, h *Object, c *propCache, key Atom) {
	checkShape(o)
	checkShape(h)
	want, err := r.getValueProp(Obj(o), key)
	got := h.props[c.idx].value
	if err != nil || !want.sameBits(got) {
		panic("property cache disagrees with lookup for " + r.atoms.name(key))
	}
}

// checkShape panics if an object's table does not have the layout its shape
// says.
func checkShape(o *Object) {
	s := o.shape
	if s == nil {
		return
	}
	if int(s.n) != len(o.props) {
		panic("shape size disagrees with table")
	}
	if s.unique {
		return
	}
	for i := len(o.props) - 1; i >= 0; i-- {
		p := &o.props[i]
		if s == nil || p.key != s.key || p.flags != s.flags {
			panic("shape disagrees with table")
		}
		s = s.parent
	}
	if s == nil || s.parent != nil {
		panic("shape deeper than table")
	}
}
