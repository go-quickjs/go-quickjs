package vm

import (
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Literals with the same keys, in any code of a realm, have one root.
//
// V8 keeps, for each native context, a map for each count of an object
// literal's properties (Factory::ObjectLiteralMapFromCache), and a literal's
// keys take the transitions out of it, whatever script it is in. Here what
// reads and makes a layout counts on each layout's first transition being the
// one its objects take (shape.next), which a root shared by literals of
// different keys has for one key of its own: tried, a root for each count had
// object_spread 2.7x and prop_clone 2.9x slower with the JIT, and nothing to
// show for them that this has not. So a literal's root is shared with the
// literals of its keys alone, whose objects take the one chain of transitions
// out of it, and which share what room their objects came to need. A realm
// keeps the roots by a hash of those keys, which the compiler says
// (bytecode.Function.LiteralKeys): a root starts empty, and each key takes its
// transition, so that the hash need only tell literals apart, and two lists of
// one hash, sharing a root, have their right layouts and lose only the first
// transition, as a root for each count did. A script's literals start from them as it
// is prepared (prepareRun), a Program's as it first runs in the realm
// (keyProgramSites), and a function's tree node as it first makes its object
// (literalRoot), or native code as it is compiled for it (keySite). Code new
// for each request then makes none of the
// shapes its literals need, and what reads their objects, kept or not, has read
// them before.
//
// A literal with a spread, an accessor, a computed key or __proto__, {}, and
// one in a function that runs in the interpreter or that a direct eval or a
// module's own code makes, have a root of their own, as before.
//
// A root and what grows from it is the realm's, for as long as it lives. So
// they are counted against a budget of their own, and past a quarter of the
// tree the realm starts again with new roots: the code that has the old ones
// keeps them for as long as it runs, and what no code has goes. What the keys
// made up by a program at run time add to a literal's objects is at most
// maxShapeFanout transitions out of its last layout: past them each object has
// a layout of its own, where code that is new for each run had shapes of its
// own for each.

// literalShapes is the most shapes a realm's roots may have grown.
const literalShapes = maxShapes / 4

// literalRoots is the roots of a realm's literals.
type literalRoots struct {
	// byKeys is by the hash of the keys (LiteralKeys).
	byKeys map[uint64]*shape
	// budget is the slot of a budget of its own, which all the roots hold
	// (chargeChild).
	budget *budgetSlot
}

// keysOf is the hash of the keys of fn's object literal of site, 0 if it does
// not say them.
func keysOf[_ any](fn *bytecode.Function, site uint32) uint64 {
	ks := fn.LiteralKeys
	i := sort.Search(len(ks), func(i int) bool { return ks[i].Site >= site })
	if i == len(ks) || ks[i].Site != site {
		return 0
	}
	return ks[i].Keys
}

// literalRoot starts ic, the cache of lit's site, whose root has no transition
// yet, from the realm's root for lit's keys, if the compiler said them and the
// tree has room.
//
//go:noinline
func literalRoot[_ any](c *tctx, lit *litOf, ic *propCache) {
	if s := ic.shape; s == noShape || s.proto == c.r.proto.object {
		keyedRoot[struct{}](c.r, lit.keys, ic)
	}
}

// keySite is literalRoot for the site of cl's literal, for native code about to
// make its objects, whose root is the site's for good: what a tree node then
// asks for is that root.
func keySite[_ any](r *Runtime, cl *closure, site uint32) {
	ic := &cl.ic[site]
	if s := ic.shape; s != nil && s.next == nil && (s == noShape || s.proto == r.proto.object) {
		if keys := keysOf[struct{}](cl.fn, site); keys != 0 {
			keyedRoot[struct{}](r, keys, ic)
		}
	}
}

// prepareRun is prepare for fn, a script to run once, the caches of its literals
// started from the realm's roots for their keys: a script's own code runs in
// the interpreter most often, whose literals ask for no root. A function it
// makes is run by the tree tier, whose node asks as it needs one (literalRoot).
//
//go:noinline
func prepareRun[_ any](r *Runtime, fn *bytecode.Function) *closure {
	cl := r.prepare(fn)
	ks := fn.LiteralKeys
	for i := range ks {
		keyedRoot[struct{}](r, ks[i].Keys, &cl.ic[ks[i].Site])
	}
	return cl
}

// keyedRoot starts ic from the realm's root for the literal's keys, of that
// hash.
func keyedRoot[_ any](r *Runtime, keys uint64, ic *propCache) {
	if root := rootOfKeys[struct{}](r, keys); root != nil {
		ic.shape = root
	}
}

// rootOfKeys is the realm's root for the keys of that hash, or nil where there
// is none to be had.
func rootOfKeys[_ any](r *Runtime, keys uint64) *shape {
	t, p := r.shapes, r.proto.object
	if t.building {
		return nil
	}
	side := sideOf[struct{}](p.ownLayout())
	ps := side.programs
	if ps == nil {
		ps = &programTable{}
		side.programs = ps
	}
	l := ps.roots
	if l == nil || l.budget.b.n.Load() >= literalShapes {
		p.flags |= objProtoMet
		t.chunk = nil
		l = &literalRoots{byKeys: map[uint64]*shape{}, budget: budgetSlotOf[struct{}](t)}
		t.chunk = nil // The chunk is the realm's roots' alone.
		ps.roots = l
	}
	root := l.byKeys[keys]
	if root == nil {
		if !takeShape[struct{}](t, nil) {
			return nil
		}
		l.budget.b.n.Add(1)
		root = &shape{tree: t, proto: p, budget: l.budget}
		l.byKeys[keys] = root
	}
	return root
}
