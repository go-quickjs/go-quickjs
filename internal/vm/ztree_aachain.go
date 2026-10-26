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
// The code is a closure assigned in this file's init, the package's first
// init with closures, which are laid out after every other closure (see
// ztree_aaeq.go).

// fillChainStore remembers in c the addition of key to o, of shape before,
// along the shared transition to next, where o's prototype chain is longer
// than an entry holds, if no prototype on it intercepts the write and the
// chain can have a cell: the entry is then before, next, o's prototype and
// the chain's cell.
var fillChainStore func(r *Runtime, c *propCache, o *Object, before, next *shape, key Atom)

func init() {
	fillChainStore = func(r *Runtime, c *propCache, o *Object, before, next *shape, key Atom) {
		if c.shape == nil {
			// A cache of no site's, which the JIT fills to learn o's layout
			// (jitNoteDeepAdd notes what it needs).
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
		cell := r.chainCell(o.proto)
		if cell == nil {
			return
		}
		c.refill(propCache{shape: remember(before), next: next, fills: c.fills + 1, p1: o.proto, cell: cell}, jitBuilt && r.jitOn())
	}
}
