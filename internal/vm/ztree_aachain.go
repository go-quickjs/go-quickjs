package vm

// Additions past a long prototype chain, remembered by the chain's validity
// cell. A write site's entry holds the prototype chain an addition passes
// as at most two objects and their shapes (propCache.adds), and an object
// of a class two below another -- every subclass's instance, its chain its
// class's prototype, its parent's and Object.prototype -- has three: each
// property a constructor added to one had been made the long way, the
// chain asked for setters at every write, until the site gave up after
// maxCacheFills and went to the runtime's table of additions (deepStore),
// a hash lookup and a walk of the chain each. As V8's transitioning
// stores check the receiver's map and its prototype chain's validity cell,
// such an entry is the receiver's shape, its prototype, and the cell of
// the chain from that prototype up (chainCell), which any change of a
// layout or a prototype on the chain invalidates -- a setter or a
// read-only property of the name added on it among them.
//
// storeCached answers such an entry by its cell, and setPropCached fills
// one by fillStoreAt, which it calls where it had called fillStoreCache:
// closures, so that those functions stay as long as they were -- a
// function that grows moves every function laid out after it, and the
// closures (AGENTS.md). propCache.adds refuses such an entry, its p1 being no
// prototype of s1's shape, so that a pure body's write and a leaf's
// (pureCallAt, leafStores), which ask only adds, leave such an addition to
// the ordinary write.
//
// The code is closures assigned in this file's init, the package's first
// init with closures, which are laid out after every other closure (see
// ztree_aaeq.go).

var (
	// fillStoreAt is fillStoreCache, which setPropCached calls it in place
	// of, then, where that remembered nothing, fillChainStore; it reports
	// whether the write still counts as a fill, being one no entry holds.
	fillStoreAt func(r *Runtime, c *propCache, o *Object, before *shape, key Atom) bool

	// fillChainStore remembers in c the addition of key to o, of shape
	// before, along the shared transition to its shape now, where o's
	// prototype chain is longer than an entry holds, if no prototype on it
	// intercepts the write and the chain can have a cell: the entry is then
	// before, the transition, o's prototype and the chain's cell.
	fillChainStore func(r *Runtime, c *propCache, o *Object, before *shape, key Atom)
)

func init() {
	fillStoreAt = func(r *Runtime, c *propCache, o *Object, before *shape, key Atom) bool {
		r.fillStoreCache(c, o, before, key)
		if c.shape != before {
			fillChainStore(r, c, o, before, key)
		}
		return c.shape != before && c.fills < maxCacheFills
	}

	fillChainStore = func(r *Runtime, c *propCache, o *Object, before *shape, key Atom) {
		const plain = propAccessor | propPrivate | propDeleted | propUninit
		next := o.shape
		if c.shape == nil || before.unique || next == nil || next == before || next.unique || next.loose ||
			next.parent != before || next.key != key || next.flags&(plain|propWritable) != propWritable {
			// A cache of no site's, which the JIT fills to learn o's
			// layout, or a write fillStoreCache refuses for its
			// transition.
			return
		}
		depth := 0
		for p := o.proto; p != nil; p = p.proto {
			if depth++; depth > maxChainDepth {
				return
			}
			if r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
				return
			}
		}
		if depth <= 2 {
			// A chain an entry holds, which fillStoreCache refused for
			// something of its own.
			return
		}
		cell := r.chainCell(o.proto)
		if cell == nil {
			return
		}
		c.refill(propCache{shape: remember(before), next: next, fills: c.fills + 1, p1: o.proto, cell: cell}, jitBuilt && r.jitOn())
	}
}
