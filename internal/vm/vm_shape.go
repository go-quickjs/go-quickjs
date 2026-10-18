package vm

import (
	"slices"
	"unsafe"
	"weak"
)

// A shape is an object's layout -- the keys of its property table, their
// attributes and their order -- as an identity that can be compared by
// pointer. Two objects with the same shape have the same keys with the same
// attributes at the same positions, so a property found at one position in one
// of them is at that position in the other: a property access remembers the
// shape it last saw and where the property was, and the next object of that
// shape needs no search. It is what V8 calls a map and QuickJS a shape.
//
// A shape holds its layout's keys and their attributes (keys); the object
// holds only the values, at the same positions (Object.vals). So an object
// with properties always has a shape, and an object's kind of shape is one
// of four.
//
//   - None (nil): the object has no properties, and no identity a cache can
//     use yet.
//   - Loose: the object has no identity a cache can use. An object of a class
//     a cache cannot follow -- a proxy, a typed array, an arguments object --
//     has one, and so does one with no tree to join, such as one whose
//     prototype is null. No cache remembers a loose shape. It is the
//     object's own, changed in place, or a template several objects made
//     alike share until one of them changes (unique says which).
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
//     prototype made with the runtime has. Once a cache has remembered it,
//     its identity is replaced whenever its layout changes, so that the
//     cache misses; until then it is changed in place, which an object
//     that has properties deleted one after another, and is read by no
//     cache in between, does not then pay a new shape for each.
//
// The table's lookup index, for an object with more properties than are
// scanned, lives in the shape: a shared shape's is shared by every object of
// it, since their layouts are the same, and a unique shape's is the object's.
//
// So do the keys: a shared shape's are never changed, and are a prefix of an
// array its first child extends in place (extended), each child after it
// having a copy of its own; a unique shape's are its object's, which the
// object changes in place, and which a new identity for the object takes
// over (layoutChanged), the old one being no object's shape.
type shape struct {
	tree   *shapeTree
	parent *shape
	keys   []propKey
	key    Atom
	flags  propFlags
	unique bool
	// loose says no cache may remember the shape: see the kinds above.
	loose bool
	// extended says, on a shared shape, that a child has extended its keys'
	// array in place: the next child's are a copy.
	extended bool
	// seen says a cache has remembered the shape: a unique shape that is
	// seen is replaced, rather than changed, when its object's layout
	// changes. Each place that keeps a shape sets it, by remember.
	seen bool
	// slack is, on a root, how many properties its objects have come to
	// need: the most an object of it has had when it outgrew the room it
	// was made with, or, on a constructor's once its first objects are
	// built (tracked), the most of them had, up to maxRoom. Its next
	// objects are made with that room (roomFor), rather than outgrowing
	// it in turn, as V8's slack tracking sizes a constructor's objects.
	slack uint8
	// used and built are, on a constructor's root, the most properties its
	// objects built so far have had and how many were built, up to
	// slackTracking of them (noteBuilt); tracked says the room is then
	// what they used.
	used, built uint8
	tracked     bool
	// borrowed says, on a unique shape, that its index is a shared layout's,
	// which its object left by deleting a key or changing an attribute: the
	// keys it still has are where that layout has them. The index is only
	// read, and may name a key the object has since deleted, which findOwn
	// checks against the table; the object makes an index of its own when it
	// adds a key.
	borrowed bool
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
	// info is, on the layout of an object a cache has found a property
	// past as a prototype, its validity cell and the prototypes below it
	// (protoInfo); such an object's layout is its own, as V8 gives a
	// prototype a map of its own (OptimizeAsPrototype).
	info *protoInfo
}

// protoCell is a prototype chain's validity cell, as V8's: valid while no
// object on the chain from the prototype it is made for up has changed its
// layout or its prototype. A read cache entry for a property found past
// two prototypes, or found nowhere, checks the receiver's layout, its
// prototype and the cell, however long the chain (propCache.holder).
type protoCell struct{ valid bool }

// protoInfo is what an object used as a prototype by such an entry keeps,
// on its layout: its chain's cell, and the prototypes whose chains go
// through it, whose cells its change invalidates too, weakly held as V8's
// prototype users are.
type protoInfo struct {
	cell  *protoCell
	users []weak.Pointer[Object]
	// up is the prototype the object is registered with as a user, which
	// a change of its prototype leaves behind.
	up *Object
}

// nullChain is the cell of the empty chain, a receiver's with no
// prototype, which nothing invalidates.
var nullChain = &protoCell{valid: true}

// absentHolder holds what an entry for a property found nowhere on the
// chain answers: a read of it is undefined. Its one key is a tombstone, so
// that what reads an entry's holder for the key finds it is not there.
var absentHolder = &Object{
	vals:  []Value{Undefined},
	shape: &shape{unique: true, n: 1, keys: []propKey{{key: atomEmpty, flags: propDeleted}}},
}

// invalidate makes the cell of the chain from info's object, and of every
// chain through it, invalid, each object given a new one: what an entry
// checked no longer holds.
func (info *protoInfo) invalidate() {
	info.cell.valid = false
	info.cell = &protoCell{valid: true}
	users := info.users[:0]
	for _, w := range info.users {
		u := w.Value()
		if u == nil {
			continue
		}
		users = append(users, w)
		if us := u.shape; us != nil && us.info != nil {
			us.info.invalidate()
		}
	}
	clear(info.users[len(users):])
	info.users = users
}

// protoChanged is called before o's layout or prototype changes: the cells
// of the chains through o, if any, are invalid.
func (s *shape) protoChanged() {
	if s != nil && s.info != nil {
		s.info.invalidate()
	}
}

// chainCell is the validity cell of the prototype chain from p up, or nil
// if one cannot be kept: each object on it given a layout of its own and
// its info, and registered with the one above it.
func (r *Runtime) chainCell(p *Object) *protoCell {
	if p == nil {
		return nullChain
	}
	s := p.shape
	switch {
	case s == nil:
		if !shapeClass(p.class) || r.shapes.building {
			return nil
		}
		p.shape = newUniqueShape(r.shapes, nil, nil)
	case s.loose:
		if r.ensureProtoShape(p); p.shape.loose {
			return nil
		}
	case !s.unique:
		p.layoutChanged()
	}
	s = p.shape
	if s.info == nil || s.info.up != p.proto {
		if r.chainCell(p.proto) == nil {
			return nil
		}
		if s.info == nil {
			s.info = &protoInfo{cell: &protoCell{valid: true}}
		}
		if q := p.proto; q != nil {
			q.shape.info.users = append(q.shape.info.users, weak.Make(p))
		}
		s.info.up = p.proto
	}
	return s.info.cell
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
	// deep is the cache of the additions made at the sites that have given
	// up their own (deepStore), made when one first is.
	deep *deepAdds
	// keyed is the object a keyed store -- o[k] = v -- is adding to, for
	// the store's length (setIndexed): past fastPropertiesSoftLimit of
	// them beyond its layout's room, the object is a dictionary
	// (shapeAdded).
	keyed *Object
}

const (
	// maxShapeDepth is the most properties a shared layout has, V8's most
	// descriptors in a map (kMaxNumberOfDescriptors); a larger object's is
	// unique. At 64, an object built alike by a constructor of more -- the
	// 215 of Octane's Gameboy emulator -- had a shape of its own each time,
	// and every cache met a new shape with each.
	maxShapeDepth = 1020
	// maxShapeFanout is the most transitions out of one shape. More means
	// keys chosen at run time, which a cache would not keep up with.
	maxShapeFanout = 64
	// maxShapes is the most shapes a tree holds.
	maxShapes = 1 << 16
	// fastPropertiesSoftLimit is how many properties a keyed store adds
	// to an object beyond its layout's room before the object is a
	// dictionary, V8's fast_properties_soft_limit (Map's
	// TooManyFastProperties, for keyed stores only): an object used as a
	// table of keys of the program's own would otherwise grow a chain of
	// shared layouts for each set of keys, and fill the tree.
	fastPropertiesSoftLimit = 12
)

// noShape is what an empty cache entry holds: no object has it.
var noShape = &shape{}

// dictShape is what a cache remembers for dictionary objects: ordinary
// objects whose layout is their own -- a key deleted, a keyed store past
// the soft limit, a tree full -- or holds no tree, a null prototype's.
// It is V8's normalized map, which such objects share
// (NormalizedMapCache), so that a read meeting many of them stays
// monomorphic: no object has it, so no inline test of a shape takes it,
// and an entry of it answers by a lookup in the object's own table
// (dictRead), as V8's handler for a dictionary-mode receiver does. Each
// had been remembered by its own layout, a site meeting many of them
// polymorphic, then megamorphic, and the layout's identity was replaced
// at each change of a table a cache had remembered.
var dictShape = &shape{}

// dictObject reports whether o is a dictionary object (dictShape). The
// global object is not, whose globals built when first read are not in
// its table until then.
func (r *Runtime) dictObject(o *Object) bool {
	s := o.shape
	return s != nil && (s.unique || s.loose) && o.class == ClassObject && o != r.global
}

// dictRead is the read of key from dictionary object o that c's entry for
// dictionary objects answers: an own plain data property, found in o's
// table; or, where the entry names prototypes, the property it found on
// them, o having none of the name.
func (r *Runtime) dictRead(c *propCache, o *Object, key Atom) (Value, bool) {
	if !r.dictObject(o) {
		return Undefined, false
	}
	i := o.findOwn(key)
	if c.p1 == nil {
		if i < 0 || o.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
			return Undefined, false
		}
		return o.vals[i], true
	}
	if i >= 0 || o.proto != c.p1 || c.p1.shape != c.s1 {
		return Undefined, false
	}
	h := c.p1
	if c.p2 != nil {
		if h.proto != c.p2 || c.p2.shape != c.s2 {
			return Undefined, false
		}
		h = c.p2
	}
	return h.vals[c.idx], true
}

// dictSite reports whether a site's cache c, meeting dictionary object o,
// is to remember dictionary objects rather than o's layout: it already
// does, or it remembers another such layout -- a second dictionary object,
// or o's own replaced -- or o's layout no cache may hold.
func dictSite(c *propCache, o *Object) bool {
	if c.shape == nil {
		// A cache of no site's, which the JIT fills to learn o's layout:
		// a site's starts at noShape (newPropCaches).
		return false
	}
	if o.shape.loose || c.shape == dictShape {
		return true
	}
	s := c.shape
	return c.fills > 0 && s != nil && s != noShape && s != o.shape && (s.unique || s.loose)
}

// fillDictCache is fillPropCache for a dictionary object at a site that
// remembers them (dictSite): an own plain data property, or one found on
// its first two prototypes, which the entry checks as any entry does.
// The entry is no shape the JIT's code is compiled for.
func (r *Runtime) fillDictCache(c *propCache, o *Object, key Atom) {
	if i := o.findOwn(key); i >= 0 {
		if o.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
			c.refill(propCache{shape: dictShape, idx: -1, fills: c.fills + 1}, false)
		}
		return
	}
	var up [2]*Object
	h := o
	for depth := 0; depth < 2; depth++ {
		if h = h.proto; h == nil {
			return
		}
		r.ensureProtoShape(h)
		if h.cacheShape() == nil || !shapeClass(h.class) || synthesized(h.class, key) {
			return
		}
		up[depth] = h
		if j := h.findOwn(key); j >= 0 {
			if h.shape.keys[j].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
				return
			}
			n := propCache{shape: dictShape, idx: j, fills: c.fills + 1, p1: up[0], s1: remember(up[0].shape)}
			if depth == 1 {
				n.p2, n.s2 = up[1], remember(up[1].shape)
			}
			c.refill(n, false)
			return
		}
	}
}

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

// maxSlack is the most room an object carries in its own allocation; room
// for more is a table of its own, made at that size (newLiteralObject).
const maxSlack = 8

// maxRoom is the most room a layout's objects are made with, V8's most
// in-object properties (JSObject::kMaxInObjectProperties).
const maxRoom = 252

// slackTracking is how many of a constructor's objects are built before
// its objects' room is what they used, V8's kSlackTrackingCounterStart.
const slackTracking = 7

// root is the shape the layout grew from.
func (s *shape) root() *shape {
	for s.parent != nil {
		s = s.parent
	}
	return s
}

// noteGrowth records, on the root of a shared layout, that an object of it
// came to need n properties' room.
func (s *shape) noteGrowth(n int) {
	if s == nil || s.unique || s.loose {
		return
	}
	if root := s.root(); n > int(root.slack) {
		// Past maxSlack the room is a table of the object's own, made at
		// that size: none is carried and outgrown.
		root.slack = uint8(min(n, maxRoom))
	}
}

// roomFor is the room an object of constructor root is made with, its body
// counting thisProps properties of this (bytecode.Function's ThisProps):
// as many as it counted, or that its objects came to need; once its first
// objects are built, what they used (noteBuilt), as V8 sizes a
// constructor's objects by the expected properties until slack tracking
// has measured them.
func (root *shape) roomFor(thisProps int) int {
	if root == nil {
		return thisProps
	}
	if root.tracked {
		return int(root.slack)
	}
	return max(thisProps, int(root.slack))
}

// noteBuilt records, on a constructor's root, that one of its objects was
// built with n properties; after slackTracking of them, its objects' room
// is the most any had, which a body counting more than it assigns -- a
// property in each branch of a switch -- would otherwise have given every
// object.
func (root *shape) noteBuilt(n int) {
	if root == nil || root.tracked {
		return
	}
	root.used = uint8(max(int(root.used), min(n, maxRoom)))
	if root.built++; root.built >= slackTracking {
		root.tracked = true
		root.slack = root.used
	}
}

// remember is s, marked as kept by a cache, for the cache to keep.
func remember(s *shape) *shape {
	if s != nil {
		s.seen = true
	}
	return s
}

// newUniqueShape makes a shape of its own for an object of the given keys,
// keeping its index.
func newUniqueShape(t *shapeTree, keys []propKey, index *propIndex) *shape {
	return &shape{tree: t, unique: true, n: int32(len(keys)), keys: keys, index: index}
}

// newLooseShape makes a loose shape of its own for an object of the given
// keys.
func newLooseShape(keys []propKey) *shape {
	return &shape{unique: true, loose: true, n: int32(len(keys)), keys: keys}
}

// extendKeys is s's keys with k after them: in s's array, if no child has
// extended it yet, or in a copy.
func (s *shape) extendKeys(k propKey) []propKey {
	keys := s.keys[:s.n:s.n]
	if !s.extended && !s.unique {
		s.extended = true
		keys = s.keys[:s.n]
	}
	return append(keys, k)
}

// cacheShape is the object's shape as a cache may remember it: nil for
// none, or a loose one.
func (o *Object) cacheShape() *shape {
	if s := o.shape; s != nil && !s.loose {
		return s
	}
	return nil
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
	inPlace := !s.extended
	c := &shape{tree: t, parent: s, key: e.key, flags: e.flags, n: s.n + 1, keys: s.extendKeys(propKey(e))}
	if inPlace && s.index != nil {
		// Its keys extend s's in place, and its index s's, as V8's maps
		// along a transition chain share one descriptor array, each the
		// prefix of its own count: the index then names keys past a
		// shape's own, which every lookup checks against the table.
		// Built anew for each shape, a 215-property layout's indexes had
		// cost the square of its length.
		c.index = s.index
		c.index.add(e.key, s.n)
	}
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

// reconfigured is the shared layout s would be with the property at
// position i given flags, or nil where the tree will not hold it: the one
// reached from s's ancestor before that key by the key with its new
// flags, then by s's keys after it, as V8's MapUpdater replays a map's
// transitions past the descriptor it reconfigures. The positions are
// s's, so an object's values stay where they are; objects that make the
// same change -- Object.freeze of a class's instances, a defineProperty
// in a constructor -- share the layout it makes, where each had been
// given one of its own, which every cache met anew.
func (s *shape) reconfigured(i int32, flags propFlags) *shape {
	if s.unique || s.loose || s.tree == nil || s.tree.building {
		return nil
	}
	a := s
	for a.n > i {
		a = a.parent
	}
	c := a.child(shapeEdge{key: s.keys[i].key, flags: flags})
	for j := i + 1; c != nil && j < s.n; j++ {
		c = c.child(shapeEdge(s.keys[j]))
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
// table, whose value is the last of vals.
func (o *Object) shapeAdded(k propKey) {
	s := o.shape
	n := len(o.vals)
	if s == nil {
		// A first property: an object whose prototype has a tree joins it,
		// at the root for its class -- or, while the realm is being made,
		// gets a layout of its own, being one of a kind.
		if n == 1 && shapeClass(o.class) && o.proto != nil && o.proto.cacheShape() != nil {
			t := o.proto.shape.tree
			if t.building {
				o.shape = newUniqueShape(t, []propKey{k}, nil)
				return
			}
			if c := t.root(o.class).child(shapeEdge(k)); c != nil {
				o.shape = c
				return
			}
			o.shape = newUniqueShape(t, []propKey{k}, nil)
			return
		}
		// No tree to join: a layout no cache will see holds the keys.
		o.shape = newLooseShape(append(make([]propKey, 0, max(n, 2)), k))
		if n > linearScanLimit {
			o.buildIndex()
		}
		return
	}
	if s.loose {
		if !s.unique {
			// A template: the object's own copy, then its key.
			s = newLooseShape(append(s.keys[:s.n:s.n], k))
			o.shape = s
		} else {
			s.keys = append(s.keys[:s.n], k)
			s.n = int32(n)
		}
		if s.index != nil && !s.borrowed {
			s.index.add(k.key, int32(n-1))
		} else if s.index != nil || n > linearScanLimit {
			// A table that needs an index of its own: no tree to join, but a
			// layout a cache may remember, as one of a kind.
			s.loose = false
			o.buildIndex()
		}
		return
	}
	if s.unique {
		s.protoChanged()
		if s.seen && (s.tree == nil || !s.tree.building) {
			// A new identity, so that what was cached of the old one misses.
			ns := newUniqueShape(s.tree, s.keys[:s.n], s.index)
			ns.borrowed, ns.info = s.borrowed, s.info
			s = ns
			o.shape = s
		}
		s.keys = append(s.keys[:s.n], k)
		s.n = int32(n)
		if s.index != nil && !s.borrowed {
			s.index.add(k.key, int32(n-1))
		} else if s.index != nil || n > linearScanLimit {
			// A borrowed index is the layout's, which has no such key: the
			// object's own is made of its table.
			o.buildIndex()
		}
		return
	}
	if s.tree.keyed == o && int(s.n) >= fastPropertiesSoftLimit && int(s.n) >= fastPropertiesSoftLimit+int(s.root().slack) {
		// A keyed store's past the soft limit: a dictionary, as V8's
		// TooManyFastProperties has it, rather than a chain of layouts
		// for each set of keys a program makes up.
		o.shape = newUniqueShape(s.tree, append(s.keys[:s.n:s.n], k), nil)
		o.buildIndex()
		return
	}
	if c := s.child(shapeEdge(k)); c != nil {
		o.shape = c
		if n > linearScanLimit && c.index == nil {
			// The objects of this shape all have this layout, so the index
			// built from this one is theirs.
			o.buildIndex()
		}
		return
	}
	o.shape = newUniqueShape(s.tree, append(s.keys[:s.n:s.n], k), nil)
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

// appendTransition appends the value of a property the object is known not
// to have, giving it the shape transition found for it, which has its key.
func (o *Object) appendTransition(v Value, next *shape) {
	if o.flags&objInlineProps != 0 && len(o.vals) == cap(o.vals) {
		o.leaveInlineProps(1)
	}
	o.vals = append(o.vals, v)
	o.shape = next
	if next.index == nil && len(o.vals) > linearScanLimit {
		o.buildIndex()
	}
	if verifyShapes {
		checkShape(o)
	}
}

// layoutChanged gives the object a layout of its own before its table changes
// other than by an append -- a property deleted or moved, an attribute changed
// -- so that no cache takes it for what it was, and so that its keys are its
// own to change. The index is kept, which the change then keeps up to date.
func (o *Object) layoutChanged() {
	s := o.shape
	if s == nil {
		return
	}
	s.protoChanged()
	if s.loose {
		if !s.unique {
			o.shape = newLooseShape(slices.Clone(s.keys[:s.n]))
			o.shape.index = s.index
			o.shape.borrowed = s.index != nil
		}
		return
	}
	if s.unique && !s.seen {
		// No cache has remembered the layout, so none can take it for what
		// it was: it changes in place.
		if s.index == nil && len(o.vals) > linearScanLimit {
			o.buildIndex()
		}
		return
	}
	index := s.index
	keys := s.keys[:s.n]
	if !s.unique {
		// A shared layout's keys are not the object's to change.
		keys = slices.Clone(keys)
	}
	ns := newUniqueShape(s.tree, keys, index)
	// A shared index is the layout's, which this object is leaving: it
	// borrows it rather than copying it, its table's positions being the
	// layout's until it adds a key (see borrowed).
	ns.borrowed = s.borrowed || !s.unique && index != nil
	if s.unique {
		// A prototype's cell and users go with its layout.
		ns.info = s.info
	}
	o.shape = ns
	if index == nil && len(o.vals) > linearScanLimit {
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
//
// A global name's site keeps idx, where in the environment's table the name
// last was, and p1, the environment itself once a fill has found it there
// (see noteGlobalSlot): every closure that shares a site's caches shares its
// environment, and a table position once given to a name holds that name or
// a tombstone for good, so the tree tier reads the slot without asking the
// closure for its scope or comparing the key.
type propCache struct {
	shape  *shape
	next   *shape
	p1, p2 *Object
	s1, s2 *shape
	// cell is, for a read found past two prototypes or found nowhere, the
	// validity cell of the chain from p1, o's prototype, up (protoCell);
	// p2 is then the object holding the property, or absentHolder, and s1
	// and s2 are nil. Native code is compiled for no such entry.
	cell *protoCell
	// poly holds the entries the site remembered before for objects of
	// other shapes, up to maxPolyEntries: V8's polymorphic inline cache,
	// of four maps (polyEntry). Made when a site first meets a second
	// shape.
	poly *propPoly
	// seen, with the JIT on, holds the entries the site had before, the
	// last few, of other shapes, which no read or write here uses: the
	// shapes the optimizing tier compiles the site for besides this one,
	// as V8's inline caches keep up to four maps for its optimizing
	// compiler, so that it knows them when it first compiles rather than
	// at the exits of code compiled without them (jitNoteSeen). A build
	// without the JIT has nothing here.
	seen jitCacheSeen
	idx  int32
	// getter marks an entry for an accessor: a read's, whose getter is
	// called, which only cachedGet fills, for OpGetProp; or a write's, whose
	// setter is called, which setPropCached fills.
	getter bool
	// fills counts the times the entry has been filled. A site that keeps
	// meeting new shapes stops trying: filling costs a lookup of its own. A
	// read then uses the runtime's shared cache.
	fills uint8
	// ownShape is shape where the entry is of a plain data property the
	// object has itself and keeps -- not a prototype's, not an accessor, not
	// one a write adds -- and noShape otherwise: what own and storeOwn
	// compare an object's shape with, so that their test is one comparison
	// rather than four. refill sets it from the entry as a whole, and
	// newPropCaches to noShape: the zero value's nil would match an object
	// with no shape.
	ownShape *shape
}

// ownOf is the ownShape of a cache entry n: see propCache.
func ownOf(n *propCache) *shape {
	if n.shape != nil && n.p1 == nil && n.cell == nil && !n.getter && n.next == nil {
		return n.shape
	}
	return noShape
}

// refill makes c n, what the site now remembers; with the JIT on, what it
// remembered before is kept among the entries it has seen (jitNoteSeen).
func (c *propCache) refill(n propCache, jit bool) {
	seen := c.seen
	if jit && c.shape != dictShape && c.cell == nil {
		// Not an entry for dictionary objects, which no code is compiled
		// for.
		seen = jitNoteSeen(seen, *c, n)
	}
	poly := notePoly(c.poly, *c, n)
	n.ownShape = ownOf(&n)
	*c = n
	c.seen, c.poly = seen, poly
}

// propPoly is a site's entries for objects of shapes other than its
// entry's (propCache.poly).
type propPoly struct {
	n       uint8
	entries [maxPolyEntries]propCache
}

// maxPolyEntries is how many shapes a site remembers besides its entry's:
// V8's polymorphic inline caches hold four maps (DEFAULT_MAX_POLYMORPHIC_
// MAP_COUNT) before they are megamorphic.
const maxPolyEntries = 3

// notePoly keeps old, the entry a site remembered before it remembers n,
// among its others, if it was of another shape: replacing the one of its
// shape, or the oldest when they are full. An entry for dictionary
// objects is kept by none: it is the entry of every such object.
func notePoly(p *propPoly, old, n propCache) *propPoly {
	if old.shape == nil || old.shape == noShape || old.shape == dictShape || old.shape == n.shape {
		return p
	}
	var none jitCacheSeen
	old.seen, old.poly = none, nil
	if p == nil {
		p = &propPoly{}
	}
	for i := range p.n {
		if p.entries[i].shape == old.shape && p.entries[i].next == old.next {
			p.entries[i] = old
			return p
		}
	}
	if int(p.n) < len(p.entries) {
		p.entries[p.n] = old
		p.n++
		return p
	}
	copy(p.entries[:], p.entries[1:])
	p.entries[len(p.entries)-1] = old
	return p
}

// polyEntry is the entry among the site's others (poly) that answers a
// read of o, or nil: a site whose objects are of a few shapes in turn --
// a list's two node classes -- reads each from its entry, rather than
// filling again at each, which had counted as a fill each time, the site
// giving up its cache (maxCacheFills) after sixteen.
func (c *propCache) polyEntry(o *Object) (*propCache, *Object) {
	if p := c.poly; p != nil {
		for i := range p.n {
			e := &p.entries[i]
			if h := e.holder(o); h != nil {
				return e, h
			}
		}
	}
	return nil, nil
}

// cachedProp is a property read of o that its cache answers. It reports false
// for anything else, which is left to getValueProp, having filled the cache
// for the next read where it can.
func (r *Runtime) cachedProp(c *propCache, o *Object, key Atom) (Value, bool) {
	if h := c.holder(o); h != nil {
		if verifyShapes {
			r.verifyPropCache(o, h, c, key)
		}
		return h.vals[c.idx], true
	}
	if c.shape == dictShape {
		if v, ok := r.dictRead(c, o, key); ok {
			return v, true
		}
	}
	if e, h := c.polyEntry(o); e != nil && !e.getter {
		if verifyShapes {
			r.verifyPropCache(o, h, e, key)
		}
		return h.vals[e.idx], true
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
		v := h.vals[c.idx]
		if !c.getter {
			if verifyShapes {
				r.verifyPropCache(o, h, c, key)
			}
			return v, true, nil
		}
		if verifyShapes {
			checkShape(o)
			checkShape(h)
			if k := h.shape.keys[c.idx]; k.key != key || k.flags&propAccessor == 0 {
				panic("property cache's accessor is not where it was for " + r.atoms.name(key))
			}
		}
		a := v.accessorPair()
		if a == nil || a.getter == nil {
			return Undefined, true, nil
		}
		v, err := r.callDirect(Obj(a.getter), Obj(o), nil)
		return v, true, err
	}
	if c.shape == dictShape {
		if v, ok := r.dictRead(c, o, key); ok {
			return v, true, nil
		}
	}
	if e, h := c.polyEntry(o); e != nil {
		v := h.vals[e.idx]
		if !e.getter {
			if verifyShapes {
				r.verifyPropCache(o, h, e, key)
			}
			return v, true, nil
		}
		a := v.accessorPair()
		if a == nil || a.getter == nil {
			return Undefined, true, nil
		}
		v, err := r.callDirect(Obj(a.getter), Obj(o), nil)
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
	if o.shape == c.ownShape {
		return o.vals[c.idx], true
	}
	return Undefined, false
}

// storeOwn writes v to the property a write site's cache says o has: o is of
// the shape it remembers, and the write is to a property the shape already
// has, writable. It is small enough to be inlined where a write is made; it
// reports false for anything else, which setPropCached does -- a setter's
// entry included, whose next is setterNext.
func (c *propCache) storeOwn(o *Object, v Value) bool {
	if o.shape == c.ownShape {
		if verifyShapes {
			checkShape(o)
		}
		o.vals[c.idx] = v
		return true
	}
	return false
}

// noteOwn remembers a plain own property a read found at index i of o's
// table, for own to answer the next object of its shape -- unless the site
// has met too many shapes to keep trying.
func (c *propCache) noteOwn(o *Object, i int32) {
	if s := o.shape; c.fills < maxCacheFills && s != nil && (s.unique || s.loose) && dictSite(c, o) {
		// A site meeting dictionary objects remembers them all (dictShape),
		// rather than each one's layout until it gives up.
		if c.shape != dictShape {
			c.refill(propCache{shape: dictShape, idx: -1, fills: c.fills + 1}, false)
		}
		return
	}
	if c.fills < maxCacheFills && o.cacheShape() != nil {
		// Native code searches a small object's own table itself.
		c.refill(propCache{shape: remember(o.shape), idx: i, fills: c.fills + 1}, false)
	}
}

// ownScan is a plain own property of an ordinary object's small table, which
// the cache then remembers for objects of o's shape -- unless one of the
// site's other entries (poly) already does, which answers.
func ownScan(c *propCache, o *Object, key Atom) (Value, bool) {
	if c.poly != nil {
		if e, h := c.polyEntry(o); h == o && !e.getter {
			return o.vals[e.idx], true
		}
	}
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
	if c.cell != nil {
		// Past two prototypes, or nowhere (absentHolder): the chain from o's
		// prototype up as it was (protoCell).
		if o.proto != p || !c.cell.valid {
			return nil
		}
		return c.p2
	}
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
			return h.vals[e.c.idx], true
		}
	}
	var c propCache
	r.fillPropCache(&c, o, key, false)
	if c.fills == 0 {
		return Undefined, false
	}
	e.key, e.c = key, c
	return c.holder(o).vals[c.idx], true
}

// deepAdd is an addition of key along the shared transition from from to
// next, to an object whose prototype chain is the n objects of up, each
// with the shape in shapes, none of which intercepts the write: what
// fillStoreCache keeps in a site's cache, for a chain of up to
// deepAddDepth, which a site's, holding two, cannot.
type deepAdd struct {
	key        Atom
	n          int32
	from, next *shape
	up         [deepAddDepth]*Object
	shapes     [deepAddDepth]*shape
}

// deepAddDepth is how deep a prototype chain deepAdds holds additions past.
const deepAddDepth = 8

// deepAdds is a runtime's cache of additions, by the shape added to and the
// key, as sharedCache is of reads.
type deepAdds [sharedCacheSize]deepAdd

// deepStore is an addition of v as o's key at a site whose cache has given
// up, made as the runtime's cache of additions says (deepAdds), or false
// where it says nothing of o's shape and key, or o's prototypes are not
// the ones it says. An object of a class of a hierarchy three to seven
// deep -- each of Typescript's syntax tree's nodes -- had been given each
// property the long way, every prototype asked for the name, and the
// prototypes asked again by the site's cache, which could not hold the
// chain.
func (r *Runtime) deepStore(o *Object, key Atom, v Value) bool {
	s, t := o.shape, r.shapes.deep
	if s == nil || t == nil {
		return false
	}
	e := &t[(uintptr(unsafe.Pointer(s))>>4^uintptr(key)*0x9e3779b1)%sharedCacheSize]
	if e.key != key || e.from != s || o.flags&objExtensible == 0 {
		return false
	}
	p := o.proto
	for i := range int(e.n) {
		if p != e.up[i] || p.shape != e.shapes[i] {
			return false
		}
		p = p.proto
	}
	if p != nil {
		return false
	}
	if verifyShapes {
		r.verifyStoreCache(&propCache{shape: s, next: e.next}, o, key)
	}
	o.appendTransition(v, e.next)
	return true
}

// fillDeepStore remembers in the runtime's cache of additions the addition
// of key a write just made to o, of shape before then, if it took a shared
// transition along a chain of up to deepAddDepth prototypes none of which
// intercepts it -- what fillStoreCache requires of a site's.
func (r *Runtime) fillDeepStore(o *Object, before *shape, key Atom) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	next := o.shape
	if before.unique || next == nil || next == before || next.unique || next.loose || next.parent != before ||
		next.key != key || next.flags&(plain|propWritable) != propWritable {
		return
	}
	var e deepAdd
	for p := o.proto; p != nil; p = p.proto {
		if e.n == deepAddDepth {
			return
		}
		if r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
			return
		}
		e.up[e.n], e.shapes[e.n] = p, remember(p.shape)
		e.n++
	}
	e.key, e.from, e.next = key, remember(before), next
	t := r.shapes
	if t.deep == nil {
		t.deep = new(deepAdds)
	}
	t.deep[(uintptr(unsafe.Pointer(before))>>4^uintptr(key)*0x9e3779b1)%sharedCacheSize] = e
}

// maxCacheFills is how many times a cache is filled before it gives up.
const maxCacheFills = 16

// newPropCaches makes the caches of a function's n property reads and writes
// and object literals, which are numbered from 1.
func newPropCaches(n int) []propCache {
	ic := make([]propCache, n+1)
	for i := range ic {
		ic[i].shape, ic[i].ownShape = noShape, noShape
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
	if c.fills >= maxCacheFills || key.IsIndex() {
		return
	}
	if r.dictObject(o) && dictSite(c, o) {
		r.fillDictCache(c, o, key)
		return
	}
	if r.ensureShape(o) == nil {
		return
	}
	var up [2]*Object
	h := o
	for depth := 0; ; depth++ {
		if h.cacheShape() == nil || !shapeClass(h.class) || synthesized(h.class, key) {
			return
		}
		if i := h.findOwn(key); i >= 0 {
			flags := h.shape.keys[i].flags
			getter := flags&propAccessor != 0
			if flags&(propPrivate|propDeleted|propUninit) != 0 || getter && !accessors {
				return
			}
			n := propCache{shape: remember(o.shape), idx: i, fills: c.fills + 1, getter: getter}
			if depth >= 1 {
				n.p1, n.s1 = up[0], remember(up[0].shape)
			}
			if depth == 2 {
				n.p2, n.s2 = up[1], remember(up[1].shape)
			}
			c.refill(n, jitBuilt && r.jitOn())
			return
		}
		if depth == 2 || h.proto == nil {
			// The chain's cell first, which gives each prototype on it a
			// layout of its own, so that the JIT notes the layouts the
			// chain keeps.
			r.fillChainCache(c, o, h, depth, key)
			if depth == 2 && h.proto != nil && jitBuilt && r.jitOn() {
				// Deeper than the cache holds: noted for the JIT's code.
				r.jitNoteDeepRead(c, o, key)
			}
			return
		}
		h = h.proto
		r.ensureProtoShape(h)
		up[depth] = h
	}
}

// maxChainDepth bounds how far up a read's cache follows a prototype chain
// for a property (fillChainCache).
const maxChainDepth = 32

// fillChainCache remembers a read of key on o found past its second
// prototype, or found nowhere, h being the last object the read's walk
// looked in, depth up: by the validity cell of o's prototype chain, as
// V8's loads of a property up a long chain, or of one nothing has, check
// one cell rather than each holder (protoCell).
func (r *Runtime) fillChainCache(c *propCache, o *Object, h *Object, depth int, key Atom) {
	if c.shape == nil {
		// A cache of no site's, which the JIT fills to learn o's layout,
		// learning a deep read its own way (jitDeepReadOf).
		return
	}
	holder := absentHolder
	idx := int32(0)
	for h.proto != nil {
		if depth++; depth > maxChainDepth {
			return
		}
		h = h.proto
		r.ensureProtoShape(h)
		if h.cacheShape() == nil || !shapeClass(h.class) || synthesized(h.class, key) {
			return
		}
		if i := h.findOwn(key); i >= 0 {
			if h.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
				return
			}
			holder, idx = h, i
			break
		}
	}
	if holder == absentHolder && c.shape != noShape && c.cell == nil && c.shape != o.shape {
		// A site whose objects have the property: one that has none does
		// not take its entry, as V8's polymorphic cache would keep both
		// and a monomorphic one would trade them at each.
		return
	}
	cell := r.chainCell(o.proto)
	if cell == nil {
		return
	}
	n := propCache{shape: remember(o.shape), idx: idx, fills: c.fills + 1, p1: o.proto, p2: holder, cell: cell}
	c.refill(n, false)
	if jitBuilt && r.jitOn() {
		r.jitNoteChainRead(o, key)
	}
}

// setPropCached is setValueProp for an object, with the site's cache: a write
// the cache answers is done here, and anything else is done as usual and then
// remembered where it can be.
func (r *Runtime) setPropCached(c *propCache, o *Object, key Atom, v Value, strict bool) error {
	if s := o.shape; s == c.shape {
		if done, err := r.storeCached(c, o, key, v); done {
			return err
		}
	}
	if p := c.poly; p != nil && o.shape != nil {
		// The site's entries for its other shapes (poly).
		for i := range p.n {
			if e := &p.entries[i]; e.shape == o.shape {
				if done, err := r.storeCached(e, o, key, v); done {
					return err
				}
			}
		}
	}
	if c.shape == dictShape && r.dictObject(o) {
		// An entry for dictionary objects: a plain writable property the
		// object has, written in its table (dictShape).
		if i := o.findOwn(key); i >= 0 && o.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable {
			o.vals[i] = v
			return nil
		}
	}
	gaveUp := c.fills >= maxCacheFills
	if gaveUp && !key.IsIndex() && r.deepStore(o, key, v) {
		return nil
	}
	before := o.shape
	if before == nil && !gaveUp {
		before = r.ensureShape(o)
	}
	if _, err := r.setProp(o, key, v, Obj(o), strict); err != nil {
		return err
	}
	if before != nil && !key.IsIndex() && shapeClass(o.class) && !synthesized(o.class, key) {
		if gaveUp {
			r.fillDeepStore(o, before, key)
			return nil
		}
		if r.dictObject(o) && dictSite(c, o) {
			if i := o.findOwn(key); i >= 0 && o.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable {
				c.refill(propCache{shape: dictShape, idx: -1, fills: c.fills + 1}, false)
			}
			return nil
		}
		if o.shape == before && r.fillSetterCache(c, o, key, before) {
			return nil
		}
		if r.fillStoreCache(c, o, before, key); c.shape != before && c.fills < maxCacheFills {
			// A write no cache holds -- an addition past more prototypes
			// than a cache keeps, as Typescript's classes' objects have --
			// counts as a fill too, so that the site gives up asking: it
			// had walked the chain again at every write, after the write's
			// own walk.
			c.fills++
		}
	}
	return nil
}

// storeCached is the write of v to o's key that entry c of a write site
// answers, o being of c's shape: a plain writable property o has, a setter
// its prototypes have, or an addition along c's transition. It reports
// whether it made the write.
func (r *Runtime) storeCached(c *propCache, o *Object, key Atom, v Value) (bool, error) {
	if c.next == nil {
		if verifyShapes {
			checkShape(o)
		}
		o.vals[c.idx] = v
		return true, nil
	}
	if c.next == setterNext {
		return r.cachedSetter(c, o, key, v)
	}
	if c.adds(o) {
		if verifyShapes {
			r.verifyStoreCache(c, o, key)
		}
		o.appendTransition(v, c.next)
		return true, nil
	}
	return false, nil
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
		if k := o.shape.keys[c.idx]; k.key != key || k.flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) != propWritable {
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
		return h.vals[c.idx], true
	}
	if c.shape == dictShape {
		if v, ok := r.dictRead(c, o, key); ok {
			return v, true
		}
	}
	if e, h := c.polyEntry(o); e != nil && !e.getter {
		if verifyShapes {
			r.verifyPropCache(o, h, e, key)
		}
		return h.vals[e.idx], true
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
	if s := o.shape; s != nil || !shapeClass(o.class) || r.shapes.building {
		return o.cacheShape()
	}
	o.shape = r.shapes.root(o.class)
	return o.shape
}

// ensureProtoShape is ensureShape for an object a cache meets as a prototype.
// One that has a loose shape got its first property while its own prototype
// had none -- as %MapIteratorPrototype% did, made in the realm before
// %IteratorPrototype% was filled in -- and is given a layout a cache may
// remember, of its own, which changes with its table, so that the methods on
// it can be remembered. A receiver is not: an object with a loose shape, such
// as one with a null prototype used as a dictionary, would then pay for a new
// layout at every key added to it.
func (r *Runtime) ensureProtoShape(o *Object) *shape {
	if s := o.shape; s != nil && s.loose && shapeClass(o.class) && !r.shapes.building {
		keys := s.keys[:s.n]
		if !s.unique {
			keys = slices.Clone(keys)
		}
		ns := newUniqueShape(r.shapes, keys, s.index)
		ns.borrowed = s.borrowed || !s.unique && s.index != nil
		o.shape = ns
		return ns
	}
	return r.ensureShape(o)
}

// passesWrite reports whether a prototype lets an assignment of key to an
// object inheriting from it add the property to the object: it has no such
// property, or a plain writable one, which the new one shadows -- not a setter,
// and not a read-only one.
func passesWrite(p *Object, key Atom) bool {
	i := p.findOwn(key)
	return i < 0 || p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable
}

// fillStoreCache remembers a write of key to o, whose shape was before: a
// write to a plain writable property o had, or the addition of one along a
// shared transition with a prototype chain the cache can check.
func (r *Runtime) fillStoreCache(c *propCache, o *Object, before *shape, key Atom) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	if o.shape == before {
		if i := o.findOwn(key); i >= 0 && o.shape.keys[i].flags&(plain|propWritable) == propWritable {
			c.refill(propCache{shape: remember(before), idx: i, fills: c.fills + 1}, jitBuilt && r.jitOn())
		}
		return
	}
	next := o.shape
	if before.unique || next == nil || next.unique || next.loose || next.parent != before || next.key != key ||
		next.flags&(plain|propWritable) != propWritable {
		return
	}
	var up [2]*Object
	p := o.proto
	for depth := 0; p != nil; depth++ {
		if depth == 2 {
			if jitBuilt && r.jitOn() {
				// Deeper than the cache holds: noted for the JIT's code.
				jitNoteDeepAdd(c, before, next, o.proto)
			}
			return
		}
		if r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
			return
		}
		up[depth] = p
		p = p.proto
	}
	n := propCache{shape: remember(before), next: next, fills: c.fills + 1, p1: up[0], p2: up[1]}
	if up[0] != nil {
		n.s1 = remember(up[0].shape)
	}
	if up[1] != nil {
		n.s2 = remember(up[1].shape)
	}
	c.refill(n, jitBuilt && r.jitOn())
}

// verifyPropCache checks a cache's answer against the read it stands for, and
// the layout of the object read against its shape. It runs in a build with
// the quickjs_verify tag, which is what the conformance suite is run with to
// catch a table changed without its shape.
func (r *Runtime) verifyPropCache(o, h *Object, c *propCache, key Atom) {
	checkShape(o)
	if h != absentHolder {
		checkShape(h)
	}
	want, err := r.getValueProp(Obj(o), key)
	got := h.vals[c.idx]
	if err != nil || !want.sameBits(got) {
		panic("property cache disagrees with lookup for " + r.atoms.name(key))
	}
}

// checkShape panics if an object's table does not have the layout its shape
// says.
func checkShape(o *Object) {
	s := o.shape
	if s == nil {
		if len(o.vals) != 0 {
			panic("properties without a shape")
		}
		return
	}
	if int(s.n) != len(o.vals) || len(s.keys) < len(o.vals) {
		panic("shape size disagrees with table")
	}
	if x := s.index; x != nil {
		// An index may be shared along a chain of layouts, or borrowed from
		// one: it names each live key at its position, and may name keys
		// past the table, or ones deleted where a tombstone is.
		keys := s.keys[:s.n]
		for i, k := range keys {
			if j, ok := x.slots[k.key]; k.flags&propDeleted == 0 && (!ok || j != int32(i)) {
				panic("index disagrees with table")
			}
		}
		for key, j := range x.slots {
			if j < s.n && keys[j].key != key && keys[j].flags&propDeleted == 0 {
				panic("index names another key in the table")
			}
		}
	}
	if s.unique || s.loose {
		return
	}
	for i := len(o.vals) - 1; i >= 0; i-- {
		if s == nil || s.keys[i] != (propKey{s.key, s.flags}) {
			panic("shape disagrees with table")
		}
		s = s.parent
	}
	if s == nil || s.parent != nil {
		panic("shape deeper than table")
	}
}
