package vm

// An object's prototype is its layout's (shape.proto), as V8's map holds its
// prototype, and a cache that remembers a layout knows the prototype with
// it. What that costs is a layout for each prototype: an object made on a
// prototype no object had before -- Object.create of an object made for it,
// a literal's __proto__, setPrototypeOf, the first instance of a class made
// by a factory -- needs one of its own, and the prototype one of its own to
// keep the roots of its objects. A loop making such objects made two shapes
// and more for each, where main, which kept the prototype in the object,
// made none.
//
// So the first ordinary object made on a prototype keeps the prototype
// beside its properties instead, as JavaScriptCore's poly-proto structures
// keep it in a slot of the object: its table's first value, under
// atomProtoSlot, a private key no name maps to, which no enumeration
// reports. objPolyProto says so, and Object.proto reads it there. Its
// layout is a node of the tree the runtime keeps for such objects, from one
// root whatever their prototypes are (shapeTree.poly), or of a literal's
// root grown by the slot, and its proto is nil:
// their own properties are remembered by caches as any shared layout's, and
// a loop making them makes no shape after the first of each.
//
// What such a layout does not say is the prototype, so no cache entry that
// rests on the receiver's layout saying its prototype is made for one: a
// read of a property found up its chain, an addition past its chain, a
// setter of its prototypes', an answer of the in operator -- each is found
// by a walk, as main found it for objects made on prototypes made and
// dropped, whose caches met a new prototype each time. Native code reads
// the prototype where it is (mir's loadProto, abi.Encoding's FlagPoly).
//
// A prototype proves shared when a second object is made on it, when a cache
// finds a property past it, or when a layout of another kind takes it as
// its objects' prototype: it is then given a layout of its own with the
// prototype in it (depoly), as any prototype is (ownLayout), and its slot
// is cleared. V8 likewise leaves an object it has not seen as a prototype
// in its map's ordinary transitions, and makes it a prototype map only
// once one is made with it.

// polyable reports whether an ordinary object made on p is made with p in
// its table (newPolyObject): p has been no object's prototype, and its
// layout, shared, is a tree's not being built -- not a prototype's of the
// realm, nor any other of its own, which a cache may know as a prototype's.
func polyable[_ any](p *Object) bool {
	if p == nil || p.flags&objProtoMet != 0 {
		return false
	}
	s := p.shape
	return s != nil && !s.unique && !s.loose && s.tree != nil && !s.tree.building
}

// polyRoot is the layout of t's objects whose prototype is in their table:
// one key, atomProtoSlot, private, grown from a root of no prototype, so
// that the slot is the first of their values whatever they are given after
// it, and a layout reconfigured past it (reconfigured) finds the root.
func polyRoot[_ any](t *shapeTree) *shape {
	if s := t.poly; s != nil {
		return s
	}
	base := &shape{tree: t}
	t.count++
	s := base.child(shapeEdge{key: atomProtoSlot, flags: propPrivate})
	if s == nil {
		return nil
	}
	t.poly = s
	return s
}

// newPolyObject is an ordinary object of prototype p, polyable, with p in
// its table and room for n more properties.
//
//go:noinline
func newPolyObject[_ any](p *Object, n int) *Object {
	root := polyRoot[struct{}](p.shape.tree)
	if root == nil {
		return newShapedLiteral(rootFor(ClassObject, p), ClassObject, n)
	}
	p.flags |= objProtoMet
	o := newShapedLiteral(root, ClassObject, n+1)
	o.appendVal(Obj(p))
	o.flags |= objPolyProto
	return o
}

// newOrdinary is the object a construction with fd makes, of prototype
// proto, with room for n properties: of root, fd's root (ctorRoot), or,
// where there is none, made as newObject makes one -- with proto in its
// table if no object has had it as its prototype.
//
// ctorRoot gives none for a prototype no object has had where another
// closure of fd's code has met such a prototype before (freshProtos), as a
// class a factory makes anew at each call, or a subclass each call makes,
// does. The first is made of a root of its own, as any constructor's
// objects are -- a class made once has its objects' layouts from the first
// -- and marks the code here.
//
//go:noinline
func newOrdinary[_ any](fd *funcData, root *shape, proto *Object, n int) *Object {
	if root == nil {
		if polyable[struct{}](proto) {
			return newPolyObject[struct{}](proto, n)
		}
	} else if proto.flags&objProtoMet == 0 && fd.closure != nil {
		fd.closure.freshProtos = true
	}
	return newShapedLiteral(root.orRoot(ClassObject, proto), ClassObject, n)
}

// polySetProto is setProto for o, whose prototype is in its table, or, for
// o empty, of a shared layout and no prototype's yet, with p made for it:
// the slot written, the layout as it was, which says nothing of the
// prototype. It reports false where o is neither.
//
//go:noinline
func polySetProto[_ any](o, p *Object) bool {
	if o.flags&objPolyProto == 0 {
		if o.class != ClassObject || o.valn != 0 || o.shape != nil && o.shape.unique || !polyable[struct{}](p) {
			return false
		}
		var root *shape
		if s := o.shape; s != nil && !s.unique && !s.loose && s.n == 0 && s.parent == nil &&
			s.rootClass != ctorRootClass && s.tree == p.shape.tree {
			// The root it was made of, a literal's say, grown by the slot:
			// the objects of a site keep their own transitions, which the
			// properties it then defines take (transition), rather than
			// those of every such object.
			// Its proto, s's, is nil, as every such layout's is, so that
			// Object.proto reads the slot: the layouts grown from it copy it.
			if root = s.child(shapeEdge{key: atomProtoSlot, flags: propPrivate}); root != nil {
				root.proto = nil
			}
		}
		if root == nil {
			if root = polyRoot[struct{}](p.shape.tree); root == nil {
				return false
			}
		}
		p.flags |= objProtoMet
		if o.valc == 0 && o.flags&objInlineProps != 0 {
			o.leaveInlineProps(1)
		}
		o.shape = root
		o.appendVal(Obj(p))
		o.flags |= objPolyProto
		return true
	}
	// A cell of a chain through o is no longer the chain's.
	o.shape.protoChanged()
	v := Null
	if p != nil {
		p.flags |= objProtoMet
		v = Obj(p)
	}
	o.vals()[0] = v
	return true
}

// depoly gives o, whose prototype is in its table, a layout of its own that
// holds it, as any prototype's does (ownLayout), its prototype first given
// one the same way if it needs it, and clears the slot, which stays a
// private property of no use.
//
//go:noinline
func depoly[_ any](o *Object) {
	p := o.proto()
	if p != nil && p.flags&objPolyProto != 0 {
		depoly[struct{}](p)
	}
	s := o.shape
	s.protoChanged()
	keys := s.keys[:s.n]
	if !s.unique {
		keys = append([]propKey(nil), keys...)
	}
	ns := newUniqueShape(s.tree, p, keys, s.index)
	ns.borrowed = s.borrowed || !s.unique && s.index != nil
	if s.unique {
		passSide[struct{}](ns, s)
	}
	o.vals()[0] = Undefined
	o.flags &^= objPolyProto
	o.shape = ns
	if verifyShapes {
		checkShape(o)
	}
}

// Native code finds objPolyProto in the byte after the flags byte
// (abi.Encoding's FlagPoly): a flag moved into the low byte fails here.
const _ uint8 = uint8(objPolyProto>>8) - 1
