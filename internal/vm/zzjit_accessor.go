//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitAccessorCase is an accessor a read or a write found on the prototypes
// of an object of shape: at idx in the table of the last of up, the
// prototypes from the object's on, each of the shape in shapes then; fn
// its getter, or a write's setter.
type jitAccessorCase struct {
	shape  *shape
	idx    int32
	up     []*Object
	shapes []*shape
	fn     *Object
}

// jitAccessorCaseOf is the accessor a read of key from o, of shape sh, or
// a write, finds on o's prototypes, up to jitAddDepth of them, each of a
// class with shapes that does not answer the key before it -- a getter
// for a read, a setter for a write -- or false. o's own accessor, which
// each object may have its own of, is not one.
func (r *Runtime) jitAccessorCaseOf(o *Object, sh *shape, key Atom, write bool) (jitAccessorCase, bool) {
	if key.IsIndex() || sh == nil || sh == noShape || sh.loose || !shapeClass(o.class) || synthesized(o.class, key) || o.findOwn(key) >= 0 {
		return jitAccessorCase{}, false
	}
	// The holder found first, the prototypes only kept for one that has
	// the accessor: a read of a property nothing has -- React's
	// children.$$typeof of an array, at every call Go made of it -- had
	// made the two slices each time.
	depth := 0
	for p := o.proto(); ; p = p.proto() {
		if p == nil || depth == jitAddDepth || r.ensureProtoShape(p) == nil || p.cacheShape() == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return jitAccessorCase{}, false
		}
		depth++
		if i := p.findOwn(key); i >= 0 {
			if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != propAccessor {
				return jitAccessorCase{}, false
			}
			break
		}
	}
	a := jitAccessorCase{shape: sh, up: make([]*Object, 0, depth), shapes: make([]*shape, 0, depth)}
	for p := o.proto(); p != nil; p = p.proto() {
		a.up, a.shapes = append(a.up, p), append(a.shapes, p.shape)
		i := p.findOwn(key)
		if i < 0 {
			continue
		}
		if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != propAccessor || int(i) >= int(p.valn) {
			return jitAccessorCase{}, false
		}
		pair := p.vals()[i].accessorPair()
		if pair == nil {
			return jitAccessorCase{}, false
		}
		a.idx, a.fn = i, pair.getter
		if write {
			a.fn = pair.setter
		}
		if a.fn == nil {
			return jitAccessorCase{}, false
		}
		remember(a.shape)
		for _, s := range a.shapes {
			remember(s)
		}
		return a, true
	}
	return jitAccessorCase{}, false
}

// jitAddAccessorCase adds a to a site's accessors, up to jitPropertyCases,
// and reports whether it did.
func jitAddAccessorCase(p *jitPolySite, a jitAccessorCase) bool {
	if len(p.accessors) >= jitPropertyCases || slices.ContainsFunc(p.accessors, func(k jitAccessorCase) bool { return k.shape == a.shape }) {
		return false
	}
	p.accessors = append(p.accessors, a)
	return true
}

// jitCacheAccessor is the accessor case a property cache's entry holds --
// a getter's, or, write, a setter's (cachedGet, cachedSetter) -- found on a
// prototype, or false.
func jitCacheAccessor(c *propCache, write bool) (jitAccessorCase, bool) {
	if !c.getter || c.shape == nil || c.shape == noShape || c.idx < 0 || c.p1 == nil || write != (c.next == setterNext) || !write && c.next != nil {
		return jitAccessorCase{}, false
	}
	a := jitAccessorCase{shape: c.shape, idx: c.idx, up: []*Object{c.p1}, shapes: []*shape{c.s1}}
	if c.p2 != nil {
		a.up, a.shapes = append(a.up, c.p2), append(a.shapes, c.s2)
	}
	holder := a.up[len(a.up)-1]
	if int(c.idx) >= int(holder.valn) {
		return jitAccessorCase{}, false
	}
	pair := holder.vals()[c.idx].accessorPair()
	if pair == nil {
		return jitAccessorCase{}, false
	}
	a.fn = pair.getter
	if write {
		a.fn = pair.setter
	}
	return a, a.fn != nil
}

// jitAccessorLive reports whether a case still holds as it was found: each
// prototype of its shape, and its accessor's function the one it found.
func jitAccessorLive(a *jitAccessorCase, write bool) bool {
	for i, p := range a.up {
		if p.shape != a.shapes[i] {
			return false
		}
	}
	holder := a.up[len(a.up)-1]
	if int(a.idx) >= int(holder.valn) {
		return false
	}
	pair := holder.vals()[a.idx].accessorPair()
	if pair == nil {
		return false
	}
	if write {
		return pair.setter == a.fn
	}
	return pair.getter == a.fn
}

// accessorOf is Accessor's site, of either form's code: the accessor the
// property site at pc, its cache c, of key, a read or a write, calls, by
// its cases, its function, and the closure the code may call it as, of the
// realm's and called with the receiver as its this; nil where Go calls it.
func (fb *jitFeedback) accessorOf(pc int, c *propCache, key Atom, write bool) (ssa.AccessorSite, *Object, *closure, bool) {
	if _, ok := jitCacheAccessor(c, write); !ok && fb.dataSeen(pc, c) {
		// Data its cache holds now, or the code met at its exits: a read
		// or a write of data first, whose accessors' receivers leave for
		// Go, as before.
		return ssa.AccessorSite{}, nil, nil, false
	}
	var cases []jitAccessorCase
	add := func(a jitAccessorCase) {
		if len(cases) < jitPropertyCases && (len(cases) == 0 || a.fn == cases[0].fn) && jitAccessorLive(&a, write) &&
			!slices.ContainsFunc(cases, func(k jitAccessorCase) bool { return k.shape == a.shape }) {
			cases = append(cases, a)
		}
	}
	if a, ok := jitCacheAccessor(c, write); ok {
		add(a)
	}
	if seen := c.seenOf(); seen != nil {
		for i := range seen.entries[:seen.n] {
			if a, ok := jitCacheAccessor(&seen.entries[i], write); ok {
				add(a)
			}
		}
	}
	if fb.e != nil && fb.root == nil {
		for i := range fb.e.poly {
			if p := &fb.e.poly[i]; int(p.pc) == pc {
				for _, a := range p.accessors {
					add(a)
				}
			}
		}
	}
	if len(cases) == 0 {
		return ssa.AccessorSite{}, nil, nil, false
	}
	k := fb.keep()
	holders := func(a *jitAccessorCase) ([2]ssa.Holder, []ssa.Holder) {
		var hs [2]ssa.Holder
		var more []ssa.Holder
		k.shapes = append(k.shapes, remember(a.shape))
		for i, p := range a.up {
			h := ssa.Holder{Object: uintptr(unsafe.Pointer(p)), Shape: uintptr(unsafe.Pointer(a.shapes[i]))}
			k.shapes, k.holders = append(k.shapes, remember(a.shapes[i])), append(k.holders, p)
			if i < len(hs) {
				hs[i] = h
			} else {
				more = append(more, h)
			}
		}
		return hs, more
	}
	site := ssa.PropertySite{Key: uint32(key), Shape: uintptr(unsafe.Pointer(cases[0].shape)), Index: cases[0].idx}
	site.Holders, site.More = holders(&cases[0])
	for i := range cases[1:] {
		a := &cases[1+i]
		pc := ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(a.shape)), Index: a.idx}
		pc.Holders, pc.More = holders(a)
		site.Cases = append(site.Cases, pc)
	}
	callee := cases[0].fn
	k.holders = append(k.holders, callee)
	a := ssa.AccessorSite{Property: site, Function: uintptr(unsafe.Pointer(callee)), Setter: write}
	fd := callee.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != fb.r.Realm {
		return a, callee, nil, true
	}
	cl := fd.closure
	if cl.fn.Kind == bytecode.KindArrow || isGeneratorTemplate(cl.fn) || isClassConstructorKind(cl.fn.Kind) {
		// An arrow's this is the one it captured; a generator's call
		// makes its object.
		return a, callee, nil, true
	}
	return a, callee, cl, true
}

// megamorphicOf is megamorphic's, of either form's code: the site at pc,
// its cache c (nil for none), a write's or a read's.
func (fb *jitFeedback) megamorphicOf(pc int, c *propCache, write bool, site ssa.PropertySite) bool {
	if len(site.Cases)+len(site.Adds) >= jitPropertyCases {
		return true
	}
	if c != nil && c.fills >= maxCacheFills {
		// An inlined callee's write whose cache gave up for additions
		// along chains deeper than it holds, which it noted, is compiled
		// for them (seenAdds) -- an initializer a class and its subclasses
		// share -- and is megamorphic all the same, as the callee's own
		// code is: another receiver is Go's from native code, as the
		// root's write's miss is (ssa's OpPropWrite). Typescript's ASTSpan,
		// whose subclasses' constructors call it through _super.call(this),
		// is constructed itself too, new ASTSpan(), inlined: its own
		// objects, of none of the subclasses' shapes, had left native code
		// at each.
		return true
	}
	if c != nil {
		if write && (c.shape == nil || c.shape == noShape) && c.fills != 0 && len(site.Cases)+len(site.Adds) == 0 {
			// A write its cache counted and remembered nothing of: an
			// addition to an object that keeps its prototype in its table,
			// whose layout says none (fillStoreCache refuses it), as
			// Object.create of a prototype made for it makes one. It is
			// not unfed, which would deoptimize at it each time the code
			// ran -- prop_in's three such writes before its loop left the
			// code each run, and its loop ran on from the interpreter --
			// but megamorphic: Go makes what the receiver's table and the
			// megamorphic cache do not, from native code, as main's code
			// did for the fresh prototypes' layouts such writes then met.
			return true
		}
		if c.shape == dictShape || c.cell != nil {
			// A cache of dictionary objects (dictShape), or of a chain's
			// cell (protoCell) -- a property up a long chain, or one
			// nothing has: native code reads the receiver's own table and
			// the megamorphic cache, whose entries name a chain's holder,
			// or that the chain has none (abi.MegaChain, MegaAbsent).
			return true
		}
	}
	e := fb.e
	if fb.root != nil && (e == nil || !e.reg) {
		// An inlined callee's whose feedback holds no register entry:
		// nothing its own code learned. A register callee's holds what
		// its code learned (regLearned), which a deoptimization of the
		// code inlining it learns in too.
		return false
	}
	if e != nil {
		for i := range e.poly {
			if p := &e.poly[i]; int(p.pc) == pc && p.mega {
				return true
			}
		}
	}
	return false
}

// dataSeen reports whether the property site at pc, its cache c, has met
// data: its cache holds a data property's entry now, or the code met data
// at its exits (jitPolySeen, jitPolyWriteSeen).
func (fb *jitFeedback) dataSeen(pc int, c *propCache) bool {
	if c.shape != nil && c.shape != noShape && !c.getter && c.idx >= 0 {
		return true
	}
	if fb.e != nil && fb.root == nil {
		for i := range fb.e.poly {
			if p := &fb.e.poly[i]; int(p.pc) == pc && (len(p.cases) != 0 || len(p.reads) != 0 || len(p.adds) != 0) {
				return true
			}
		}
	}
	return false
}

// jitB2i is 1 for true, 0 for false.
func jitB2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
