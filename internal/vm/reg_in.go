package vm

import "unsafe"

// The in operator answered from what plain objects and arrays hold: the
// register interpreter's handler (rhIn) asks hasIndexQuick and hasCached,
// and native code's call of Go (jitHas) hasQuick, before the slow path,
// which answers the rest -- a proxy's trap, a key to convert, a typed
// array's index, a module namespace, an exotic object's own keys.
//
// The answers hasQuick finds for a key string that names no index are
// remembered in the realm's hasCache, by the string, the object's layout
// and its prototype, as V8's megamorphic stub cache remembers a load's: the
// interpreter's handler looks there first, and so does native code, before
// it calls Go (mir's keyedHas).
//
// Its functions are variables, each assigned a closure in init, laid
// out after every other closure of the package, so that no hot code
// moves for them (see AGENTS.md).

// hasCache is a realm's cache of the in operator's answers for key
// strings that name no index, made when one is first remembered. An entry
// answers key in o -- has -- where key's string is the entry's, o's layout
// is shape and its prototype proto, and cell, the validity cell of the
// chain from proto up (chainCell), is valid: the layout tells o's own keys,
// and its class, which an array's length is of; the cell, that no object up
// the chain has changed its keys or its prototype since. Only an ordinary
// object's or an array's answer, of a chain of them (hasQuick), is
// remembered, whose layout is one a cache may remember, so that it is
// replaced at any change of the object's keys. Elements do not matter to a
// name. An entry is one of the hasCacheWays of its set, found by the
// string's and the layout's addresses (hasSetOf), as V8's stub caches are
// found by their maps' and names' addresses: the first is the newest, and
// a new entry takes its place, the others each moving one way on and the
// oldest leaving, so that up to four answers asked in turn of one layout
// -- "a" in o, "z" in o, "c" in o, "y" in o -- keep their places however
// their addresses fall, and never displace each other. With two ways to a
// set, three keys of one layout whose addresses shared a set displaced
// each other at every turn, each a call of Go, in one run and not in
// another (prop_in_keyed's 72 calls in 96 turns). Native code finds them
// so too, newest first, their fields at the offsets the JIT's abi names
// (abi.HasEntrySize, abi.HasCacheWays and the others).
type hasCache [hasCacheSize]hasEntry

// hasEntry is an entry of hasCache, of 64 bytes.
type hasEntry struct {
	key   unsafe.Pointer
	shape *shape
	proto *Object
	cell  *protoCell
	has   bool
	_     [3]uint64
}

const (
	// hasCacheBits is the bits of an entry's index: 256 entries, in sets
	// of hasCacheWays, 1<<hasCacheWaysLog, the set the top
	// hasCacheBits-hasCacheWaysLog bits of the hash.
	hasCacheBits    = 8
	hasCacheSize    = 1 << hasCacheBits
	hasCacheWaysLog = 2
	hasCacheWays    = 1 << hasCacheWaysLog
	// hasCacheMul is the multiplier of the hash, Fibonacci's.
	hasCacheMul = 0x9E3779B97F4A7C15
)

var (
	// hasQuick is key in o where the answer is in o's and its prototypes'
	// own storage and asking it runs nothing: each of them an ordinary
	// object or an array, whose own properties are its elements and its
	// table, and an array's length -- as hasPropErr and hasOwnProp find
	// them -- by a key that is an index, a number's or a string's, or a
	// string whose atom the runtime remembers with it (keyAtoms), as
	// toPropertyName finds it. An index absent from an object's elements
	// is looked for in its table only where the table may have one
	// (noIndexKeys). It reports false for anything else, which inOperator
	// answers: a key of another kind, a string whose atom is not
	// remembered, a chain with any other object on it.
	hasQuick func(r *Runtime, key Value, o *Object) (has, ok bool)

	// hasIndexQuick is hasQuick of the index i, below atomIndexTag.
	hasIndexQuick func(o *Object, i uint32) (has, ok bool)

	// hasCached is key in o where the realm's hasCache remembers it.
	hasCached func(r *Runtime, key Value, o *Object) (has, ok bool)

	// hasNote remembers in the realm's hasCache that key, a string that
	// names no index, is in o where has says, as hasQuick found it, where
	// o's layout and its chain's cell can be kept.
	hasNote func(r *Runtime, key Value, o *Object, has bool)

	// hasSetOf is the set of hasCache a key string's answer of an object
	// of layout sh is in, of the string's and the layout's addresses, as
	// native code finds it (abi.HasCacheMul).
	hasSetOf func(key unsafe.Pointer, sh *shape) uint64
)

func init() {
	hasSetOf = func(key unsafe.Pointer, sh *shape) uint64 {
		return (uint64(uintptr(key))>>4 ^ uint64(uintptr(unsafe.Pointer(sh)))>>4) * hasCacheMul >> (64 - hasCacheBits + hasCacheWaysLog)
	}
	hasCached = func(r *Runtime, key Value, o *Object) (bool, bool) {
		c, sh := r.has, o.shape
		if c == nil || !key.IsString() || sh == nil {
			return false, false
		}
		set := hasSetOf(key.ref, sh)
		for i := set * hasCacheWays; i < (set+1)*hasCacheWays; i++ {
			// The layout says the prototype, but for an object that keeps
			// it in its table (objPolyProto): only that one is compared.
			if e := &c[i]; e.key == key.ref && e.shape == sh && e.cell.valid && (o.flags&objPolyProto == 0 && e.proto == sh.proto || o.flags&objPolyProto != 0 && e.proto == (*Object)(o.valp.ref)) {
				if o.flags&objPolyProto != 0 {
					// Asked again of a singleton made on a prototype of its
					// own: its layout its own, holding the prototype, as a
					// second read past it makes it (polyReread), for the
					// next answer to be remembered by that layout.
					depoly[struct{}](o)
				}
				return e.has, true
			}
		}
		return false, false
	}
	hasNote = func(r *Runtime, key Value, o *Object, has bool) {
		// An object with no properties is given its class's first layout,
		// as a read's cache gives it (fillPropCache).
		sh := r.ensureShape(o)
		if sh == nil || r.shapes.building || o.flags&objPolyProto != 0 && o.proto() == nil {
			// Or o keeps a prototype of none in its table, which native code
			// would read as its layout's, none, for an object of the layout
			// with another (mir's keyedHas).
			return
		}
		cell := r.chainCell(o.proto())
		if cell == nil {
			return
		}
		if r.has == nil {
			r.has = new(hasCache)
		}
		set := hasSetOf(key.ref, sh)
		ways := r.has[set*hasCacheWays : (set+1)*hasCacheWays]
		// The ways before this answer's entry, if it has one -- which its
		// new cell replaces -- else all but the oldest, each move one way
		// on, and the answer is the newest.
		last := hasCacheWays - 1
		for i := range ways {
			if e := &ways[i]; e.key == key.ref && e.shape == sh && e.proto == o.proto() {
				last = i
				break
			}
		}
		copy(ways[1:last+1], ways[:last])
		ways[0] = hasEntry{key: key.ref, shape: remember(sh), proto: o.proto(), cell: cell, has: has}
	}
	hasIndexQuick = func(o *Object, i uint32) (bool, bool) {
		for p := o; p != nil; p = p.proto() {
			if p.class > ClassArray {
				return false, false
			}
			if e := p.elemsOf(); uint(i) < uint(len(e)) && !isHole(e[i]) {
				return true, true
			}
			if !p.noIndexKeys() {
				if j := p.findOwn(Atom(atomIndexTag | i)); j >= 0 && p.shape.keys[j].flags&propPrivate == 0 {
					return true, true
				}
			}
		}
		return false, true
	}
	hasQuick = func(r *Runtime, key Value, o *Object) (bool, bool) {
		var a Atom
		switch {
		case key.IsNumber():
			// An index, as toPropertyName makes it: -0 is 0.
			f := key.num
			if i := uint32(f); float64(i) == f && i < atomIndexTag {
				return hasIndexQuick(o, i)
			}
			return false, false
		case key.IsString():
			s := key.String()
			e := r.keyFind(s)
			if e == nil {
				// Looked up, and its atom, or that it names none,
				// remembered there (toPropertyName), so that a key whose
				// entry its set's newer keys displaced is answered here
				// and remembered in hasCache, not left to inOperator.
				if _, err := r.toPropertyName(key); err != nil {
					return false, false
				}
				if e = r.keyFind(s); e == nil {
					return false, false
				}
			}
			if e.none {
				// A key no property has, while it names no atom, on a
				// chain of objects that holds its keys.
				if e.atom != r.atoms.next() {
					return false, false
				}
				for p := o; p != nil; p = p.proto() {
					if p.class > ClassArray {
						return false, false
					}
				}
				hasNote(r, key, o, false)
				return false, true
			}
			a = e.atom
		default:
			return false, false
		}
		if a.IsIndex() {
			return hasIndexQuick(o, a.Index())
		}
		for p := o; p != nil; p = p.proto() {
			if p.class > ClassArray {
				return false, false
			}
			if a == atomLength && p.class == ClassArray {
				hasNote(r, key, o, true)
				return true, true
			}
			if j := p.findOwn(a); j >= 0 && p.shape.keys[j].flags&propPrivate == 0 {
				hasNote(r, key, o, true)
				return true, true
			}
		}
		hasNote(r, key, o, false)
		return false, true
	}
}
