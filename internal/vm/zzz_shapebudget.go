package vm

import (
	"runtime"
	"sync/atomic"
)

// The tree's budget (shapeTree.count, maxShapes) is of the shapes the tree
// holds, and a shape made with a prototype -- every shape but a root of no
// prototype's (shapeTree.root) -- is a part of the tree for as long as its
// prototype lives, and no longer: each holds its prototype (shape.proto),
// and the prototype's own layout holds the roots its objects start from
// (protoRoots), which hold the shapes grown from them. So each such shape
// is counted too against a budget the prototype's layout holds a slot of
// (shapeBudget, budgetChunk), and when the slots' prototypes are all
// collected, a cleanup gives what it counted back to the tree
// (releaseShapes). A cleanup of each prototype's own had cost more than the
// objects made on it: 600 ns, where an Object.create and a property added
// take 250. Objects made on prototypes made and dropped -- a scope
// chain of Object.create's, an object literal's __proto__, a class made by a
// factory -- had used the budget up for good once each prototype had roots
// of its own, and every layout after it was of its own, which every cache
// met anew.
//
// The first ordinary object made on a prototype keeps it in its table, of a
// layout of the runtime's for such objects (zzz_polyproto.go), most
// prototypes made and dropped being of one object each; where it cannot --
// a prototype of a layout of its own, or in no tree -- it joins no tree: its
// layout is its own (firstRoot). A constructor's root is counted as its first object is given
// a property, and kept on the prototype's own layout (keptCtorRoot). A
// literal site's root, made in a few instructions of the interpreter's loop,
// is counted only until code stops running (settleSites): what it holds is
// no shape but those grown from it, and it lives no longer than the code it
// is of, or an object made from it. Those shapes are held by the root, which
// they hold, and so are garbage with it, whatever their prototype: they are
// counted against a budget the root takes a slot of (chargeChild). Counted
// against the realm's Object.prototype, as their prototype is, they outlived
// the code in a realm that outlives it.

// protoRoots is what the own layout of an object that is another's prototype
// keeps of the objects it is the prototype of: the roots they start from,
// one for each class (rootFor) and one for the objects constructions make
// (keptCtorRoot), as V8's prototype info keeps Object.create's map -- the
// first made, most often the only one, then the rest -- and the slot of the
// budget the shapes made with the object as their prototype are counted
// against. The first root and the budget are the layout's (shape.protoRoot,
// which its parent field holds, and shape.budget), which is what most
// prototypes have; the rest of the roots are on its side (shapeSide.roots),
// which a prototype's layout has only once a second is kept.

// shapeBudget is the count of the shapes a tree holds with the prototypes
// of a budgetChunk's slots as theirs: n, which the chunk's cleanup adds to
// the tree's freed when the chunk is collected, and the tree takes back from
// its count as it next needs room (reclaimShapes). It holds nothing that
// reaches the chunk, or the cleanup would never run, and its counts are
// atomic: the cleanup runs on a goroutine of its own.
type shapeBudget struct {
	n     atomic.Int64
	freed *atomic.Int64
}

// budgetSlots is how many prototypes share a budget.
const budgetSlots = 16

// budgetChunk is the slots of one budget, each held by the layout of one
// prototype (protoRoots), which is so held by every shape counted against
// it: the chunk, and its cleanup, outlives them all. A chunk whose other
// prototypes are gone gives back nothing while one lives: what a prototype
// that lives long can keep counted is what fifteen others made.
type budgetChunk [budgetSlots]budgetSlot

// budgetSlot is one prototype's slot of a budgetChunk: its budget.
type budgetSlot struct{ b *shapeBudget }

// ctorRootClass is the rootClass of a constructor's root (ctorRoot,
// keptCtorRoot): no object's class, so that no class's root kept on the
// same layout is taken for it (rootFor, rootIn).
const ctorRootClass = ClassCursor + 1

// rootAt is the root at position i of the roots ps keeps, or nil past the
// last.
func rootAt[_ any](ps *shape, i int) *shape {
	if i == 0 {
		return ps.protoRoot()
	}
	if ps.side == nil || i > len(ps.side.roots) {
		return nil
	}
	return ps.side.roots[i-1]
}

// setRoot makes s the root at position at of the roots ps keeps, or, for
// at -1, adds it.
//
//go:noinline
func setRoot[_ any](ps *shape, at int, s *shape) {
	switch {
	case at == 0 || at < 0 && ps.protoRoot() == nil:
		ps.parent = s // protoRoot
	case at > 0:
		ps.side.roots[at-1] = s
	case ps.side == nil:
		ps.side = &shapeSide{roots: append(make([]*shape, 0, 2), s)}
	default:
		ps.side.roots = append(ps.side.roots, s)
	}
}

// chargeShape counts a shape just made in t with prototype p, against the
// tree's budget and, for a prototype, against p's own (shapeBudget). p's
// layout is its own already: a shape of prototype p grows from a root kept on
// it. One grown from a literal's root is chargeChild's.
//
//go:noinline
func chargeShape[_ any](t *shapeTree, p *Object) {
	if t.freed != nil && t.freed.Load() != 0 {
		// Kept up to date, so that a literal's site, which asks only the
		// count, does not find it spent by shapes since gone (siteRoot).
		reclaimShapes[struct{}](t)
	}
	t.count++
	t.settled++
	if p == nil {
		return
	}
	p.flags |= objProtoMet
	ps := p.ownLayout()
	if ps.budget == nil {
		ps.budget = budgetSlotOf[struct{}](t)
	}
	ps.budget.b.n.Add(1)
}

// chargeChild is chargeShape for a shape just grown from s. One grown from a
// literal's root is counted against the root's slot of a budget, taken with
// the first, as a prototype's own layout has one. What it asks is the root's
// because a shape does not say: a layout grown by a prototype's slot says no
// prototype (polySetProto).
//
//go:noinline
func chargeChild[_ any](s *shape) {
	s = s.root()
	p := s.proto
	if p != nil && !s.generic && s.rootClass == ClassObject {
		b := s.budget
		if b == nil {
			p.flags |= objProtoMet
			b = budgetSlotOf[struct{}](s.tree)
			s.budget = b
		}
		b.b.n.Add(1)
		p = nil
	}
	chargeShape[struct{}](s.tree, p)
}

// budgetSlotOf is the next slot of t's budgets, for a prototype's layout or
// a literal's root.
//
//go:noinline
func budgetSlotOf[_ any](t *shapeTree) *budgetSlot {
	if t.chunk == nil || t.chunkUsed == budgetSlots {
		if t.freed == nil {
			t.freed = new(atomic.Int64)
		}
		b := &shapeBudget{freed: t.freed}
		c := new(budgetChunk)
		for i := range c {
			c[i].b = b
		}
		runtime.AddCleanup(c, releaseShapes[struct{}], b)
		t.chunk, t.chunkUsed = c, 0
	}
	t.chunkUsed++
	return &t.chunk[t.chunkUsed-1]
}

// firstRoot is rootIn's for the first ordinary object made with p as its
// prototype, which marks p as met: a layout of the object's own (unique), in
// no tree, which a cache may remember, changed in place as the object is
// given properties until one does, as any layout of an object's own is. An
// object used as the prototype of one object alone -- a scope's in a chain
// of Object.create's, the __proto__ of an object literal, a fresh object
// given to setPrototypeOf -- had had its layout made its own, a root and a
// shape for each property its one object was given, three to five
// allocations where main made none, and as many shapes counted until it was
// collected; now its object makes one. It had been a loose template, which
// the object copied as it was given its first property, two allocations, and
// whose layout no cache could remember: a read of such an object, a
// singleton made on a prototype of its own, was a dictionary's, a lookup in
// its table each time, and code compiled for the read left native code at
// it -- four to fifteen times main's time with the JIT, twice without.
// While the realm is being made, each object's layout is its own too, and p
// is not marked. An object of another class -- the first closure a runtime
// makes on Function.prototype, an array -- starts from the prototype's root
// from the first: rootIn asks firstRoot only for an ordinary object.
//
//go:noinline
func firstRoot[_ any](_ Class, p *Object) *shape {
	var t *shapeTree
	if s := p.shape; s != nil {
		t = s.tree
	}
	if t == nil || !t.building {
		p.flags |= objProtoMet
	}
	return newUniqueShape(t, p, nil, nil)
}

// takeShape reports whether t has room for one more shape, of prototype p,
// counting it if so (chargeShape).
//
//go:noinline
func takeShape[_ any](t *shapeTree, p *Object) bool {
	if t.count >= maxShapes && !reclaimShapes[struct{}](t) {
		return false
	}
	chargeShape[struct{}](t, p)
	return true
}

// releaseShapes is the cleanup of a collected budgetChunk: what its budget
// counted is freed, and its shapes, which held it, are gone with it.
func releaseShapes[_ any](b *shapeBudget) {
	b.freed.Add(b.n.Load())
}

// reclaimShapes takes back from the count of a tree whose budget is spent
// the shapes collected since it last did, reporting whether it has room.
//
//go:noinline
func reclaimShapes[_ any](t *shapeTree) bool {
	if t.freed != nil {
		n := int(t.freed.Swap(0))
		t.count -= n
		t.settled -= n
	}
	return t.count < maxShapes
}

// settleSites takes the literals' roots made since it last did, which the
// tree's count alone can say a site made, back out of the count. The count
// bounds what keys a program makes up can grow, where nothing but the tree
// holds it. A root is no such thing: there is one for each literal of the
// code that lives, which holds it, or for each object made from it. Each run
// of a program has its literals' roots anew: counted for good, or against
// the realm's Object.prototype, they filled the tree in some tens of
// thousands of runs, and the shapes after them had no room.
//
//go:noinline
func settleSites[_ any](r *Runtime) {
	t := r.shapes
	t.count = t.settled
}

// keptCtorRoot is the root kept on the own layout of the prototype of s, a
// constructor's root (ctorRoot), for the objects constructions make with it:
// s itself, kept and counted, if none is yet. A constructor's root is kept
// so as its first object is given a property, and is in the tree only then:
// one that constructs with another prototype, or for a new.target of
// another -- a base class's, as each subclass's super() or Reflect.construct
// makes them, as V8 keeps a map for each new.target (GetDerivedMap) -- or one
// given another prototype, starts again from a root of its own, whose
// objects grow from the prototype's kept one, which the prototype had met
// before: a root made anew for each had made each object's layout new.
//
//go:noinline
func keptCtorRoot[_ any](s *shape) *shape {
	t, ps := s.tree, s.proto.ownLayout()
	for i := 0; ; i++ {
		k := rootAt[struct{}](ps, i)
		if k == nil {
			break
		}
		if k.rootClass == ctorRootClass && k.tree == t {
			return k
		}
	}
	if takeShape[struct{}](t, s.proto) {
		setRoot[struct{}](ps, -1, s)
	}
	// Else no room to keep it, and its objects' layouts are their own
	// (child).
	return s
}

// ctorChild is child for s, a constructor's root: where s is not its
// prototype's kept one (keptCtorRoot), the kept one's child for e, which
// s's objects take, as its first transition, so that the next of them
// follow it as they would s's own, and true. Its parent is the kept root:
// the layouts are alike, s's having no keys.
//
//go:noinline
func ctorChild[_ any](s *shape, e shapeEdge) (*shape, bool) {
	k := keptCtorRoot[struct{}](s)
	if k == s {
		return nil, false
	}
	c := k.child(e)
	if c != nil && s.next == nil {
		s.setNext(c, e)
	}
	return c, true
}

// classProto gives ctor, a class constructor made a moment ago and so of a
// shared layout of Function.prototype's, parent as its prototype (static
// inheritance; linkClass). The first class made to extend parent is given a
// layout of its own (setProto), and parent is marked as met; those made
// after it take the layout reached from the root kept on parent's own layout
// for functions by the keys ctor has, as V8's prototype transitions give a
// map's objects one map for each new prototype: a subclass made by each call
// of a mixin or a factory, which had had a layout of its own made for each,
// a shape and a copy of its keys, shares one with the others. linkClass so
// gives the class's prototype object its parent's prototype too: a
// subclass made by each call had had a layout of its own made for its
// prototype, on which an object then made could not keep the prototype in
// its table (zzz_polyproto.go), as on a prototype of a layout no other has.
//
//go:noinline
func classProto[_ any](ctor, parent *Object) {
	s := ctor.shape
	if s == nil || s.unique || s.loose || s.tree == nil || s.tree.building || s.proto == parent ||
		parent.flags&objProtoMet == 0 || !shapeClass(ctor.class) || ctor.flags&objPolyProto != 0 {
		parent.flags |= objProtoMet
		ctor.setProto(parent)
		return
	}
	c := rootIn(nil, ctor.class, parent)
	for i := int32(0); c != nil && !c.unique && !c.loose && c.tree != nil && i < s.n; i++ {
		c = c.child(shapeEdge(s.keys[i]))
	}
	if c == nil || c.unique || c.loose || c.tree == nil || c.n != s.n {
		ctor.setProto(parent)
		return
	}
	s.protoChanged()
	ctor.shape = c
	if c.index == nil && int(ctor.valn) > linearScanLimit {
		ctor.buildIndex()
	}
	if verifyShapes {
		checkShape(ctor)
	}
}

// infoOf is the protoInfo of s, nil if s is nil or has none.
func infoOf[_ any](s *shape) *protoInfo {
	if s == nil || s.side == nil {
		return nil
	}
	return s.side.info
}

// sideOf is the side of s, made if it has none.
func sideOf[_ any](s *shape) *shapeSide {
	if s.side == nil {
		s.side = &shapeSide{}
	}
	return s.side
}

// keepForIn keeps c, the keys a for-in loop visits on an object of
// layout s, on s's side (shapeKeys).
//
//go:noinline
func keepForIn[_ any](s *shape, c *forInKeys) {
	sideOf[struct{}](s).forIn = c
}

// newProtoInfo is a protoInfo, of a new valid cell, kept on s's side for
// the object of layout s, a prototype (chainCell).
//
//go:noinline
func newProtoInfo[_ any](s *shape) *protoInfo {
	info := &protoInfo{cell: &protoCell{valid: true}}
	sideOf[struct{}](s).info = info
	return info
}

// passSide gives ns, a new identity for the layout of a prototype whose
// layout was s, what goes with the layout: the roots of the objects it is
// the prototype of, their budget, and its side, which holds the rest of
// the roots and the prototype's cell and users. s is no object's layout
// after it, and its side is ns's alone: what a cache keeps of s is only
// compared, never asked for its side.
//
//go:noinline
func passSide[_ any](ns, s *shape) {
	ns.parent, ns.budget = s.protoRoot(), s.budget
	ns.side, s.side = s.side, nil
}
