package vm

// Object spread into the empty object its literal just made -- {...src},
// {...src, x: 1} -- by adopting the layout such a spread makes, as V8's
// CloneObjectIC gives the clone the map it keeps for the source's map.
// copyDataProps had defined each enumerable property of a plain source on
// the target in turn (defineOwnProp): a search of the target's table, an
// append and a transition looked up for each key. What that makes is fixed
// by the source's layout and the target's: the source's enumerable own
// string keys, in its keys' order (shapeKeys), each added to the target's
// root with the default attributes. So the layout it reaches from a root is
// made once, by replaying those transitions (shape.child, which the
// definitions would have taken), and kept on the side of the source's
// layout, with its keys (shapeSide.spread), for each root the source is spread to; the next
// spread of a source of that layout into an empty object of that root
// takes it at once, and its values in one copy, or one gather where the
// source's table has other properties among them.
//
// The idea is from a port of QuickJS that measured it on a clone
// microbenchmark (github.com/bellard/quickjs/issues/602).
//
// The layout is the one the definitions would have reached, from the same
// root, so the target is in every way the object they made: its keys, their
// order and attributes, its room (reserveProps, which notes the room the
// literal's objects come to need on its root), its index. A spread into an
// object that already has a property -- {a: 1, ...src}, a second spread --
// or of a source that is not a plain object of a shared layout whose
// enumerable properties all hold data under string keys, is
// copyDataProps's, as it was.
//
// A layout kept with a source's keys keeps its root, and so the root's
// prototype: it is kept only where that prototype is on the source's own
// layout's chain, which the source's layout keeps already, so that a
// source of one realm spread in another keeps no realm alive.
//
// Its functions are generic, each instantiated with struct{} alone: Go lays
// a generic function's instantiations out after every function and closure
// of the package (see zzz_direct.go). copyDataProps asks spreadShaped where
// it reserved the target's room, and grows within the padding that rounds
// it to 32 bytes.

// spreadRoots is how many roots a source's layout keeps the spread's
// layout for: the literals spreading objects of one layout, which a
// reducer's {...state, key: v} at each of its cases makes several of.
const spreadRoots = 4

// spreadLayouts is what a source's layout keeps of the spreads of its
// objects (shapeSide.spread): for each of the last roots spread into, the
// layout the spread reaches, and whether the source's values are its
// first, in order, the spread's copy then one block.
type spreadLayouts struct {
	from  [spreadRoots]*shape
	to    [spreadRoots]*shape
	next  uint8
	block bool
}

// spreadShaped is object spread of o, a plain object of a shared layout
// whose enumerable own properties all hold data under string keys -- c,
// shapeKeys's, copyable -- into t, by its layout (spreadLayout), where t is
// an ordinary extensible object with no properties and a shared root:
// reporting that it did. Otherwise it reserves the room the definitions
// copyDataProps then makes need, and reports that it did not.
//
// The table it allocates is charged to no memory limit as it is made: a
// shared layout has at most maxShapeDepth keys, a table of 16 KB, short of
// what a charge is made for (builderChunk), and the walk counts it as it
// counts a literal's.
//
//go:noinline
func spreadShaped[_ any](r *Runtime, t, o *Object, c *forInKeys) bool {
	n := len(c.atoms)
	from := t.shape
	if t.valn != 0 || t.class != ClassObject || t.flags&(objExtensible|objPolyProto|objHasExt) != objExtensible ||
		n == 0 || from == nil || from.n != 0 || from.unique || from.loose || from.tree == nil || from.tree.building ||
		int(o.valn) != int(o.shape.n) {
		if t.class == ClassObject && n > 1 {
			// The table grows once rather than at every doubling.
			t.reserveProps(int(t.valn) + n)
		}
		return false
	}
	to, block := spreadLayout[struct{}](c, o, from)
	if to == nil {
		if n > 1 {
			t.reserveProps(n)
		}
		return false
	}
	t.reserveProps(n)
	dst := t.vals()[:n]
	if src := o.vals(); block {
		copy(dst, src[:n])
	} else {
		for i, j := range c.slots {
			dst[i] = src[j]
		}
	}
	t.valn = uint32(n)
	t.shape = to
	if to.index == nil && n > linearScanLimit {
		t.buildIndex()
	}
	if verifyShapes {
		checkShape(t)
	}
	return true
}

// spreadLayout is the layout a spread of o, of the keys c, makes of an
// empty object of root from, and whether o's values are its first, in
// order: the one kept on o's layout's side, beside c, or one made by the transitions the
// definitions would take and kept, where from's prototype is on the chain
// of o's layout's. It is nil where it is not kept and where the tree holds
// no such layout.
func spreadLayout[_ any](c *forInKeys, o *Object, from *shape) (*shape, bool) {
	side := o.shape.side
	sp := side.spread
	if sp != nil {
		for i, f := range sp.from {
			if f == from {
				return sp.to[i], sp.block
			}
		}
	}
	p := o.shape.proto
	for p != nil && p != from.proto {
		p = p.proto()
	}
	if p == nil {
		return nil, false
	}
	to := from
	for _, k := range c.atoms {
		if to = to.child(shapeEdge{k, propDefault}); to == nil {
			return nil, false
		}
	}
	if sp == nil {
		sp = &spreadLayouts{block: true}
		for i, j := range c.slots {
			if int(j) != i {
				sp.block = false
			}
		}
		side.spread = sp
	}
	sp.from[sp.next], sp.to[sp.next] = from, to
	sp.next = (sp.next + 1) % spreadRoots
	return to, sp.block
}
