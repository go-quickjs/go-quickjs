package vm

import "unsafe"

// The in operator answered from what plain objects and arrays hold, by
// every tier: the tree tier's node (inNode) and native code's call of Go
// (jitHas) ask hasQuick first, and inOperator, as executeAt's case does,
// only for what it does not answer -- a proxy's trap, a key to convert, a
// typed array's index, a module namespace, an exotic object's own keys.
//
// The answers hasQuick finds for a key string that names no index are
// remembered in the runtime's hasCache, by the string, the object's layout
// and its prototype, as V8's megamorphic stub cache remembers a load's: the
// tree tier's node looks there first, and so does native code, before it
// calls Go (mir's keyedHas).
//
// The code is closures assigned in this file's init, the first of the
// package's inits with closures, whose closures are laid out after every
// other closure (see ztree_zkself.go): code added here moves nothing the
// tree tier or the interpreter runs.

// hasCache is a runtime's cache of the in operator's answers for key
// strings that name no index, made when one is first remembered. An entry
// answers key in o -- has -- where key's string is the entry's, o's layout
// is shape and its prototype proto, and cell, the validity cell of the
// chain from proto up (chainCell), is valid: the layout tells o's own keys,
// and its class, which an array's length is of; the cell, that no object up
// the chain has changed its keys or its prototype since. Only an ordinary
// object's or an array's answer, of a chain of them (hasQuick), is
// remembered, whose layout is one a cache may remember, so that it is
// replaced at any change of the object's keys. Elements do not matter to a
// name. An entry is one of the two of its set, found by the string's and
// the layout's addresses: the first is the newer, and a new entry takes
// its place, the older going to the second's, so that two answers asked
// in turn -- "a" in o and "b" in o -- keep their places however their
// addresses fall. Native code finds them so too, their fields at the
// offsets the JIT's abi names (abi.HasEntrySize and the others).
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
	// of two, the set the top hasCacheBits-1 bits of the hash.
	hasCacheBits = 8
	hasCacheSize = 1 << hasCacheBits
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

	// hasCached is key in o where the runtime's hasCache remembers it.
	hasCached func(r *Runtime, key Value, o *Object) (has, ok bool)

	// hasNote remembers in the runtime's hasCache that key, a string that
	// names no index, is in o where has says, as hasQuick found it, where
	// o's layout and its chain's cell can be kept.
	hasNote func(r *Runtime, key Value, o *Object, has bool)

	// inNode builds in, the in operator, at pc: hasCached's or hasQuick's
	// answer where it has one, inOperator's otherwise.
	inNode func(b *tbuilder, pc int)
)

func init() {
	hasCached = func(r *Runtime, key Value, o *Object) (bool, bool) {
		c, sh := r.shapes.has, o.shape
		if c == nil || !key.IsString() || sh == nil {
			return false, false
		}
		set := (uint64(uintptr(key.ref))>>4 ^ uint64(uintptr(unsafe.Pointer(sh)))>>4) * hasCacheMul >> (64 - hasCacheBits + 1)
		for i := set * 2; i < set*2+2; i++ {
			if e := &c[i]; e.key == key.ref && e.shape == sh && e.proto == o.proto && e.cell.valid {
				return e.has, true
			}
		}
		return false, false
	}
	hasNote = func(r *Runtime, key Value, o *Object, has bool) {
		// An object with no properties is given its class's first layout,
		// as a read's cache gives it (fillPropCache).
		sh := r.ensureShape(o)
		if sh == nil || r.shapes.building {
			return
		}
		cell := r.chainCell(o.proto)
		if cell == nil {
			return
		}
		if r.shapes.has == nil {
			r.shapes.has = new(hasCache)
		}
		set := (uint64(uintptr(key.ref))>>4 ^ uint64(uintptr(unsafe.Pointer(sh)))>>4) * hasCacheMul >> (64 - hasCacheBits + 1)
		ways := r.shapes.has[set*2 : set*2+2]
		if e := &ways[0]; e.key != key.ref || e.shape != sh || e.proto != o.proto {
			// The newer is now the older, unless it is this one's, which
			// its new cell replaces.
			ways[1] = ways[0]
		}
		ways[0] = hasEntry{key: key.ref, shape: remember(sh), proto: o.proto, cell: cell, has: has}
	}
	hasIndexQuick = func(o *Object, i uint32) (bool, bool) {
		for p := o; p != nil; p = p.proto {
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
			e := &r.keyAtoms[keySlot(s)]
			if e.s != s {
				// Looked up, and its atom, or that it names none,
				// remembered there (toPropertyName): two keys whose
				// strings share a slot had each found the other's there
				// at every evaluation, and left every one to inOperator,
				// never remembered in hasCache.
				if _, err := r.toPropertyName(key); err != nil || e.s != s {
					return false, false
				}
			}
			if e.none {
				// A key no property has, while it names no atom, on a
				// chain of objects that holds its keys.
				if e.atom != r.atoms.next() {
					return false, false
				}
				for p := o; p != nil; p = p.proto {
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
		for p := o; p != nil; p = p.proto {
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
	inNode = func(b *tbuilder, pc int) {
		y, x := b.pop().tree(), b.pop().tree()
		b.push(func(c *tctx) Value {
			l := x(c)
			r := y(c)
			if r.IsObject() {
				// An index straight to its walk; a name to the cache,
				// then to its walk.
				o := r.object()
				if i := uint32(l.num); float64(i) == l.num && i < atomIndexTag {
					if has, ok := hasIndexQuick(o, i); ok {
						return Bool(has)
					}
				} else if has, ok := hasCached(c.r, l, o); ok {
					return Bool(has)
				} else if has, ok := hasQuick(c.r, l, o); ok {
					return Bool(has)
				}
			}
			c.at(pc)
			v, err := inOperator(c.r, l, r)
			if err != nil {
				c.throw(err)
			}
			return v
		})
	}
}
