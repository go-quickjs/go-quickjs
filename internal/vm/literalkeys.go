package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

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
// (bytecode.RegCode.ObjKeysHash): a root starts empty, and each key takes its
// transition, so that the hash need only tell literals apart, and two lists of
// one hash, sharing a root, have their right layouts and lose only the first
// transition, as a root for each count did. A script's literals start from
// them as it is prepared (prepareRun), a Program's as it first runs in the
// realm (keyProgramSites), and a function's literal as it first makes its
// object (regLiteralKeyedRoot), or native code as it is compiled for it
// (keySite). Code new for each request then makes none of the shapes its
// literals need, and what reads their objects, kept or not, has read them
// before.
//
// {} names no keys, and its root is the realm's for no keys, in any code:
// V8's empty literal is made of its context's Object function's initial
// map, every site's (CreateEmptyObjectLiteral), which the keys given it
// then leave by transitions. A root of each site's own had grown a chain
// for each site's objects: Octane's CodeLoad, which evaluates a library
// anew some thousands of times and keeps each copy's namespace, {} given
// some fifty keys, had a chain of fifty for each copy kept, until the
// tree was full and every object made after it was of a layout of its
// own. The root's room is fixed, as that map's is (V8's
// kInitialGlobalObjectUnusedPropertiesCount): learned on the root, one {}
// grown to a table of hundreds of keys had sized every {} of the realm.
// The room a site's objects are made with is the site's own, learned from
// its own objects (emptySiteMake). It is not the root new Object() and Object.create take
// (rootFor), which their prototype's layout keeps for as long as the
// realm, with every transition out of it: V8 shares the two, whose
// transitions are weak.
//
// A literal with a spread, an accessor, a computed key or __proto__, which
// says no list of keys, has a root of its own, as before.
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

// noKeysHash is the hash of no keys (bytecode.HashLiteralKeys), {}'s, which
// keysOf says for a new_object.
var noKeysHash = bytecode.HashLiteralKeys(nil)

// emptyRootClass is the rootClass of the realm's root for {}: no object's
// class, so that outgrowProps and noteGrowth, which learn a root's room,
// leave its room as it is made (emptyLiteralRoom).
const emptyRootClass = ClassCursor + 2

// emptyLiteralRoom is the room of the objects {} makes, fixed: V8's
// kInitialGlobalObjectUnusedPropertiesCount, the in-object properties of
// its Object function's initial map.
const emptyLiteralRoom = 4

// literalRoots is the roots of a realm's literals.
type literalRoots struct {
	// byKeys is by the hash of the keys (RegCode.ObjKeysHash).
	byKeys map[uint64]*shape
	// budget is the slot of a budget of its own, which all the roots hold
	// (chargeChild).
	budget *budgetSlot
}

// keysOf is the hash of the keys of rc's object literal at pc, a
// new_object_lit's (bytecode.RegCode.ObjKeysHash) or {}'s, a new_object of
// no properties, which has none (noKeysHash), 0 if it does not say them: a
// new_object of properties is a literal with a spread, an accessor, a
// computed key or __proto__, which say no list of keys.
func keysOf[_ any](rc *bytecode.RegCode, pc int) uint64 {
	if rc != nil && pc >= 0 && pc < len(rc.Code) && rc.Code[pc].Op() == bytecode.RNewObject {
		if rc.Code[pc].B() == 0 {
			return noKeysHash
		}
		return 0
	}
	if rc == nil || pc < 0 || pc >= len(rc.Code) || rc.Code[pc].Op() != bytecode.RNewObjectLit ||
		pc >= len(rc.Aux) || int(rc.Aux[pc]) >= len(rc.ObjKeysHash) {
		return 0
	}
	return rc.ObjKeysHash[rc.Aux[pc]]
}

// keySite is regLiteralKeyedRoot for the site of cl's literal at pc of its
// register code, for native code about to make its objects, whose root is
// the site's for good: what the interpreter then asks for is that root.
func keySite[_ any](r *Runtime, cl *closure, pc int, site uint32) {
	ic := &cl.ic[site]
	if s := ic.shape; s != nil && s.next == nil && (s == noShape || s.proto == r.proto.object) {
		if keys := keysOf[struct{}](cl.fn.RegCompiled(), pc); keys != 0 {
			keyedRoot[struct{}](r, keys, ic)
		}
	}
}

// prepareRun is prepare for fn, a script to run once, the caches of its
// literals started from the realm's roots for their keys. A function it
// makes asks for one as it needs it (regLiteralKeyedRoot).
//
//go:noinline
func prepareRun[_ any](r *Runtime, fn *bytecode.Function) *closure {
	cl := r.prepare(fn)
	if rc := fn.RegCompiled(); rc != nil {
		for pc, w := range rc.Code {
			if keys := keysOf[struct{}](rc, pc); keys != 0 && int(w.C()) < len(cl.ic) {
				keyedRoot[struct{}](r, keys, &cl.ic[w.C()])
			}
		}
	}
	return cl
}

// keyEmptySite is regLiteralKeyedRoot for a new_object's site, whose cache
// is as the code was prepared (noShape): started from the realm's root for
// {}.
//
//go:noinline
func keyEmptySite[_ any](r *Runtime, ic *propCache) {
	keyedRoot[struct{}](r, noKeysHash, ic)
}

// keyedRoot starts ic from the realm's root for the literal's keys, of that
// hash.
func keyedRoot[_ any](r *Runtime, keys uint64, ic *propCache) {
	if root := rootOfKeys[struct{}](r, keys); root != nil {
		ic.shape = root
		if keys == noKeysHash && ic.idx == 0 {
			emptySiteStart[struct{}](ic)
		}
	}
}

// A {} site's room.
//
// The realm's root for {} is every {} site's, and its room is fixed
// (emptyLiteralRoom); the room a site's objects are made with is the
// site's, kept in its own cache entry, as V8 keeps a literal's
// AllocationSite in its function's feedback vector, a slot for each
// literal (runtime-literals.cc:520-560), the map shared and the site its
// own. The entry of a {} site holds the root alone, and its record takes
// two of the fields no literal's entry uses: idx is the room, from
// emptyLiteralRoom up to maxRoom, and fills how many samples the site has
// left to take, slackTracking+1 before its first object, 0 once settled.
// No literal's entry is filled again, and jitFeedbackMark leaves both
// out.
//
// The site learns what its own objects came to hold, as a constructor's
// root does (noteBuilt) and as V8's slack tracking settles a map's room
// from what its objects reached, over kSlackTrackingCounterStart (7)
// constructions (MapUpdater::CompleteInobjectSlackTracking,
// map-updater.cc:469-497, Map::ComputeMinObjectSlack, map.cc:2421). A {}
// object has no end of construction, but its site's next execution is
// one: by then the object it made before is, almost always, as large as
// it will get. So each execution while the site learns reads the
// object the one before made, its sample, and the room is the most any
// sample held, of a layout of the tree's -- a dictionary, which a table
// of names becomes (soft limit), teaches nothing, and the soft limit
// counts the root's fixed room, not the site's, so a table's keys never
// size a site's objects. The new object is the next sample. After
// slackTracking samples the room is settled, as a constructor's is, and
// later objects that grow past it grow by doubling, as V8's past the
// slack it settled. An object taken early -- a recursive builder makes
// its child's {} before its own is filled -- can only teach too little,
// never too much.
//
// The first execution takes no sample, so that code run once holds
// nothing. The sample is the Runtime's (emptySamples), one for a site at
// most, by its entry: no object points at its site. A sweep after a
// collection and the turn's end (clearReturnedFrames) read each sample a
// last time, as a sample, and let it go (emptySamplesLetGo), so that a
// sample is held until its site's next execution, the next sweep or the
// turn's end, whichever is first. Native code's pools of a {} learn into
// the same record: each fill reads the sample and samples the object the
// pool gives last (jitFillLiteral). A Program keeps its {} sites' records
// from run to run (programSites' rooms).

// emptySiteStart starts the record of the {} site ic, before its first
// object.
func emptySiteStart[_ any](ic *propCache) {
	ic.idx, ic.fills = emptyLiteralRoom, slackTracking+1
}

// emptySiteMake is regNewObject's {} of root, the realm's root for {}, at
// site ic while it learns its room: the sample before it read, the object
// made with the room, and kept as the next.
//
//go:noinline
func emptySiteMake[_ any](r *Runtime, ic *propCache, root *shape) *Object {
	sample := emptySiteLearn[struct{}](r, ic)
	o := newShapedLiteral(root, ClassObject, int(ic.idx))
	if sample {
		emptySiteSample[struct{}](r, ic, o)
	}
	return o
}

// emptySiteLearn has the {} site ic, about to make an object, read its
// sample, and reports whether it is to sample the object it makes.
func emptySiteLearn[_ any](r *Runtime, ic *propCache) bool {
	if ic.fills > slackTracking {
		// The first: no sample.
		ic.fills = slackTracking
		return false
	}
	if o := r.emptySamples[ic]; o != nil {
		emptySiteObserve[struct{}](ic, o)
		if ic.fills == 0 {
			delete(r.emptySamples, ic)
		}
	}
	return ic.fills != 0
}

// emptySiteSample keeps o as the {} site ic's sample.
func emptySiteSample[_ any](r *Runtime, ic *propCache, o *Object) {
	if r.emptySamples == nil {
		r.emptySamples = map[*propCache]*Object{}
	}
	r.emptySamples[ic] = o
}

// emptySiteObserve counts o as a sample of the {} site ic: the room is the
// most a sample of a layout of the tree's held, up to maxRoom.
func emptySiteObserve[_ any](ic *propCache, o *Object) {
	if s := o.shape; s != nil && !s.unique && !s.loose {
		ic.idx = int32(max(int(ic.idx), min(int(o.valn), maxRoom)))
	}
	if ic.fills != 0 {
		ic.fills--
	}
}

// emptySamplesLetGo is clearReturnedFrames' release of the {} sites'
// samples, each read a last time.
//
//go:noinline
func emptySamplesLetGo[_ any](r *Runtime) {
	for ic, o := range r.emptySamples {
		emptySiteObserve[struct{}](ic, o)
	}
	r.emptySamples = nil
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
		if keys == noKeysHash {
			root.rootClass, root.slack = emptyRootClass, emptyLiteralRoom
		}
		l.byKeys[keys] = root
	}
	return root
}
