package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Keyed stores by their sites' caches: set_index's o[k] = v and
// define_index's { [k]: v } -- a literal's computed key, a class's field --
// remember, as V8's keyed store inline caches do, for each pair of an
// object's shape and a key they met, where the value went: a plain
// writable property the object has (an own entry: the shape and the slot,
// the key the shape's at that slot), or the transition that adds it (the
// shape, the transition, whose key is the key, and for an assignment the
// prototype chain found to have no setter or read-only property of the
// name, as a named store's entry holds it). An entry is a propCache, the
// site's in the closure's ic (its Aux, regcomp's keyedSite), with up to
// maxPolyEntries more in its poly, so that a site storing a few keys into
// objects of a few shapes -- o = { [keys[i & 3]]: i } -- finds each pair's
// entry by comparing the object's shape and the entry's key. A key is a
// string, its atom found in keyAtoms (stringKey), or a symbol; an array
// index's, a number's, an element, is set_index's element paths' as
// before, and is never an entry's. A pair no entry holds is stored the
// long way and then remembered, where it can be, up to maxCacheFills,
// after which the site stores the long way only: a site that keeps
// meeting new keys -- a table filled in by its keys -- is no cache's.
//
// set_index's slow path, rsSetIndex, hands this file's rsSetIndexKeyed a
// key that is a string or a symbol, so that its handler, which the
// native handlers mirror, is unchanged, and a number's store takes no
// step more; define_index's Go handler calls regDefineIndex, which asks
// the cache.

// rsSetIndexKeyed is set_index's slow path for a key that is a string or
// a symbol: stored by the site's cache (keyedStore), or stored the long
// way and remembered (keyedSet); the long way (setIndexed) where the
// object is none, the key's atom an index's, the code has no site or the
// site has given up -- one that keeps meeting new keys, whose atoms the
// long way finds anyway: their conversion made twice had made
// json_stringify_new_layout's stores of new keys 4% slower.
func rsSetIndexKeyed(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r := st.r
	obj, key := regs[w.A()], regs[w.B()]
	if obj.IsObject() {
		if c := regKeyedSite(st.c, st.cl, pc); c != nil && c.fills < maxCacheFills {
			if k := r.keyedAtom(key); !k.IsIndex() {
				o, v := obj.Object(), regs[w.C()]
				if keyedStore(c, o, k, v, r.shapes) {
					return pc + 1
				}
				st.at(pc)
				return st.done(r.keyedSet(c, o, k, key, v, st.cl.fn.Strict))
			}
		}
	}
	st.at(pc)
	return st.done(r.setIndexed(obj, key, regs[w.C()], st.cl.fn.Strict))
}

// regKeyedSite is the cache of the keyed store at pc of c, a set_index or
// a define_index, in cl's caches: its Aux's. Nil where it has none.
func regKeyedSite(c *regCode, cl *closure, pc int) *propCache {
	if pc < len(c.rc.Aux) {
		if s := c.rc.Aux[pc]; s != 0 && int(s) < len(cl.ic) {
			return &cl.ic[s]
		}
	}
	return nil
}

// keyedAtom is the atom of a key that is a string or a symbol: a string's
// from keyAtoms where it is there (stringKey).
func (r *Runtime) keyedAtom(key Value) Atom {
	if key.IsString() {
		return r.stringKey(key.String())
	}
	return r.atoms.internSymbol(key.Symbol())
}

// keyedStore is the store of v to o's key k that an entry of keyed site c
// answers -- its own, or one of its poly's -- and reports whether it made
// it: an entry of o's shape whose key is k, a slot o has or a transition
// whose test is storeCached's, of what the addition asks of o and its
// prototypes. An assignment's addition is made as setIndexed makes it,
// under tree's keyed (shapeAdded, outgrowProps): an object a keyed store
// grows teaches its literal's root no room.
func keyedStore(c *propCache, o *Object, k Atom, v Value, tree *shapeTree) bool {
	s := o.shape
	if s == nil {
		return false
	}
	if c.shape == s && keyedEntryStore(c, o, k, v, tree) {
		return true
	}
	if p := c.poly; p != nil {
		for i := range p.n {
			if e := &p.entries[i]; e.shape == s && keyedEntryStore(e, o, k, v, tree) {
				return true
			}
		}
	}
	return false
}

// keyedEntryStore is keyedStore's store by entry e, which is of o's shape,
// where its key is k.
func keyedEntryStore(e *propCache, o *Object, k Atom, v Value, tree *shapeTree) bool {
	if e.next == nil {
		if e.shape.keys[e.idx].key != k {
			return false
		}
		if verifyShapes {
			checkShape(o)
		}
		o.vals()[e.idx] = v
		return true
	}
	if e.next == setterNext || e.next.key != k {
		return false
	}
	if o.flags&objExtensible != 0 && (e.p1 == nil || e.p1.shape == e.s1 || e.cell != nil && e.cell.valid) &&
		(e.p2 == nil || e.p2.shape == e.s2) {
		if verifyShapes {
			checkShape(o)
		}
		if tree != nil {
			was := tree.keyed
			tree.keyed = o
			o.appendTransition(v, e.next)
			tree.keyed = was
			return true
		}
		o.appendTransition(v, e.next)
		return true
	}
	return false
}

// keyedSet is o[key] = v, k key's atom, made the long way (setIndexed),
// then remembered by keyed site c, which has not given up, where it can
// be.
func (r *Runtime) keyedSet(c *propCache, o *Object, k Atom, key, v Value, strict bool) error {
	before := o.shape
	if err := r.setIndexed(Obj(o), key, v, strict); err != nil {
		return err
	}
	if before != nil && !before.loose && r.keyedCacheable(o, k) {
		// What an assignment's named site remembers: a plain writable
		// property o has, or the addition along a shared transition past
		// a chain of prototypes that do not intercept it, by the chain's
		// objects and their shapes or by its cell (fillStoreAt), into an
		// entry of no site's, then made the site's.
		t := propCache{shape: noShape, fills: c.fills}
		fillStoreAt(r, &t, o, before, k)
		if t.shape == before && t.next != setterNext {
			c.refillKeyed(t)
			return nil
		}
	}
	c.fills++
	return nil
}

// keyedCacheable reports whether a store of k to o may be remembered by
// its shape: o's table is all there is to it for k, and k is no array
// index -- not even one past an index atom's reach, an interned name
// that names an element of an array as much as an index atom does.
func (r *Runtime) keyedCacheable(o *Object, k Atom) bool {
	if k.IsIndex() || !shapeClass(o.class) || synthesized(o.class, k) || r.dictObject(o) {
		return false
	}
	_, index := r.atoms.arrayIndex(k)
	return !index
}

// keyedDefine is define_index's store of v to o's key k, CreateDataProperty
// with a literal's attributes, by keyed site c's entries -- a site that has
// not given up -- or made the long way and then remembered where it can
// be: a property o has that is a plain data property of those
// attributes, whose value the define alone changes, or one the define
// added along a shared transition. A define passes no prototype, so the
// entry has none to check.
func (r *Runtime) keyedDefine(c *propCache, o *Object, k Atom, v Value) error {
	if keyedStore(c, o, k, v, nil) {
		return nil
	}
	before := o.shape
	if err := r.createDataProperty(o, k, v, memberFlags(0)); err != nil {
		return err
	}
	const attrs = propAccessor | propPrivate | propDeleted | propUninit | propDefault
	if before != nil && !before.loose && o.flags&objPolyProto == 0 && r.keyedCacheable(o, k) {
		if next := o.shape; next == before {
			if i := o.findOwn(k); i >= 0 && next.keys[i].flags&attrs == propDefault {
				c.refillKeyed(propCache{shape: remember(before), idx: i, fills: c.fills + 1})
				return nil
			}
		} else if !before.unique && next != nil && !next.unique && !next.loose && next.parent == before &&
			next.key == k && next.flags&attrs == propDefault {
			c.refillKeyed(propCache{shape: remember(before), next: next, fills: c.fills + 1})
			return nil
		}
	}
	c.fills++
	return nil
}

// refillKeyed makes keyed site c's entry n, keeping the entry it had among
// its others (noteKeyed).
func (c *propCache) refillKeyed(n propCache) {
	poly := noteKeyed(c.poly, *c, n)
	n.ownShape = ownOf(&n)
	*c = n
	c.poly = poly
}

// sameKeyed reports whether keyed entries a and b are for the same pair of
// a shape and a key: of one shape, an own entry's slot, which says its
// key, or a transition's, whose shape does.
func sameKeyed(a, b *propCache) bool {
	return a.shape == b.shape && a.next == b.next && a.idx == b.idx
}

// noteKeyed is notePoly for a keyed site, whose entries of one shape are
// as many as the keys it met: old, the entry the site had, is kept among
// p's unless it is for n's pair, and an entry of p's for n's pair -- one
// whose prototypes changed since, which the store met again -- leaves.
func noteKeyed(p *propPoly, old, n propCache) *propPoly {
	if p != nil {
		k := uint8(0)
		for i := range p.n {
			if !sameKeyed(&p.entries[i], &n) {
				p.entries[k] = p.entries[i]
				k++
			}
		}
		for i := k; i < p.n; i++ {
			p.entries[i] = propCache{}
		}
		p.n = k
	}
	if old.shape == nil || old.shape == noShape || sameKeyed(&old, &n) {
		return p
	}
	old.poly = nil
	if p == nil {
		p = &propPoly{}
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
