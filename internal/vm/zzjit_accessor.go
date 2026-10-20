//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
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
	a := jitAccessorCase{shape: sh}
	for p := o.proto; p != nil; p = p.proto {
		if len(a.up) == jitAddDepth || r.ensureProtoShape(p) == nil || p.cacheShape() == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return jitAccessorCase{}, false
		}
		a.up, a.shapes = append(a.up, p), append(a.shapes, p.shape)
		i := p.findOwn(key)
		if i < 0 {
			continue
		}
		if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != propAccessor || int(i) >= len(p.vals) {
			return jitAccessorCase{}, false
		}
		pair := p.vals[i].accessorPair()
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
	if int(c.idx) >= len(holder.vals) {
		return jitAccessorCase{}, false
	}
	pair := holder.vals[c.idx].accessorPair()
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
	if int(a.idx) >= len(holder.vals) {
		return false
	}
	pair := holder.vals[a.idx].accessorPair()
	if pair == nil {
		return false
	}
	if write {
		return pair.setter == a.fn
	}
	return pair.getter == a.fn
}

// Accessor is ssa.AccessorFeedback's: the getter a read found on the
// prototypes of its receivers, or the setter a write found -- a class's
// accessor, or one Object.defineProperty put on a prototype -- as V8's code
// calls an accessor its feedback knows rather than leave its site. The
// site's cache's (cachedGet, cachedSetter), those it held before
// (propCache.seen) and those the code met at its exits (jitPolySeen,
// jitPolyWriteSeen), each a receiver's shape with the prototypes it was
// found on, are its cases, up to jitPropertyCases, all of one function:
// the first's. The callee is inlined where it may be, as a method called
// there would be (jitInlinesAt); one that is no closure the code may call
// -- a built-in's, a bound function's -- Go calls, called from native code
// to make the read or the write. An accessor the receivers have
// themselves, each its own -- an object literal's get x() -- Go's at an
// exit, as before.
func (fb *jitFeedback) Accessor(pc int) (ssa.AccessorSite, bool) {
	if fb.cl == nil || fb.fwd != nil || pc >= len(fb.fn.Code) {
		return ssa.AccessorSite{}, false
	}
	in := fb.fn.Code[pc]
	write := in.Op == bytecode.OpSetProp
	if in.Op != bytecode.OpGetProp && !write || int(in.A) >= len(fb.cl.names) || int(in.B) >= len(fb.cl.ic) {
		return ssa.AccessorSite{}, false
	}
	c := &fb.cl.ic[in.B]
	if _, ok := jitCacheAccessor(c, write); !ok && fb.dataSeen(pc, c) {
		// Data its cache holds now, or the code met at its exits: a read
		// or a write of data first, whose accessors' receivers leave for
		// Go, as before.
		return ssa.AccessorSite{}, false
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
	if c.seen != nil {
		for i := range c.seen.entries[:c.seen.n] {
			if a, ok := jitCacheAccessor(&c.seen.entries[i], write); ok {
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
		return ssa.AccessorSite{}, false
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
	site := ssa.PropertySite{Key: uint32(fb.cl.names[in.A]), Shape: uintptr(unsafe.Pointer(cases[0].shape)), Index: cases[0].idx}
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
		return a, true
	}
	cl := fd.closure
	if cl.fn.Kind == bytecode.KindArrow || isGeneratorTemplate(cl.fn) || isClassConstructorKind(cl.fn.Kind) {
		// An arrow's this is the one it captured; a generator's call
		// makes its object.
		return a, true
	}
	if site, ok := fb.accessorCall(cl, callee, write); ok {
		a.Call = &site
	}
	if cl.fn == fb.fn || !fb.r.jitInlinesAt(cl, callee, bytecode.Instr{Op: bytecode.OpCallMethod}) {
		return a, true
	}
	p, err := fb.r.jitLower(cl.fn, jitLowerInline)
	if err != nil {
		return a, true
	}
	this := -1
	if p.This {
		this = cl.fn.LocalCount + len(cl.fn.Upvalues)
	}
	a.Inline = &ssa.InlineSite{
		Program: p, Feedback: &jitFeedback{r: fb.r, fn: cl.fn, cl: cl, root: k},
		Callee: uintptr(unsafe.Pointer(callee)), Closure: uintptr(unsafe.Pointer(cl)),
		Argc: jitB2i(write), Method: true, Params: cl.fn.ParamCount, ThisSlot: this, Coerce: cl.fn.CoerceThis,
	}
	k.inlined = append(k.inlined, cl)
	return a, true
}

// megamorphic reports whether the property site at pc, compiled for site,
// is megamorphic: it has met more than its cases hold -- as many shapes as
// they hold, read or written, or so many its cache gave up (maxCacheFills,
// after which the VM reads and writes through the runtime's shared caches)
// -- or what no case can hold (jitPolySite's mega), which the code's
// deoptimizations there found (jitPropertyDeopt). Native code then looks
// in the receiver's own table and the megamorphic cache for any other
// receiver, and leaves for Go only what neither has (ssa.PropertySite's
// Megamorphic); anywhere else such a receiver deoptimizes, as V8's map
// checks do, and the code is compiled again for what the cache learned. An
// inlined callee's site is as the callee's own code has found it.
func (fb *jitFeedback) megamorphic(pc int, site ssa.PropertySite) bool {
	if len(site.Cases)+len(site.Adds) >= jitPropertyCases {
		return true
	}
	if in := fb.fn.Code[pc]; int(in.B) < len(fb.cl.ic) && fb.cl.ic[in.B].fills >= maxCacheFills {
		// But an inlined callee's write whose cache gave up for additions
		// along chains deeper than it holds, which it noted and the site
		// is compiled for (seenAdds) -- an initializer a class and its
		// subclasses share -- as V8's code is compiled for the maps it met:
		// another receiver deoptimizes.
		if c := &fb.cl.ic[in.B]; fb.root == nil || in.Op != bytecode.OpSetProp || c.seen == nil || len(c.seen.deep) == 0 || len(site.Adds) == 0 {
			return true
		}
	}
	if in := fb.fn.Code[pc]; int(in.B) < len(fb.cl.ic) {
		if c := &fb.cl.ic[in.B]; c.shape == dictShape || c.cell != nil {
			// A cache of dictionary objects (dictShape), or of a chain's
			// cell (protoCell) -- a property up a long chain, or one
			// nothing has: native code reads the receiver's own table and
			// the megamorphic cache, whose entries name a chain's holder,
			// or that the chain has none (abi.MegaChain, MegaAbsent).
			return true
		}
	}
	e := fb.e
	if fb.root != nil {
		e = fb.r.jit.entryFor(fb.fn)
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

// accessorCall is the native call of an accessor, callee, cl its closure,
// a read or a write makes, through its function's link (jitLink), as a
// call the code makes natively is (NativeCalls): the receiver its this, a
// setter's value its argument. Only the function's own code calls one
// natively; a callee with no code for native callers yet is compiled
// after (wanted, linked), and called by Go until it has.
func (fb *jitFeedback) accessorCall(cl *closure, callee *Object, write bool) (ssa.CallSite, bool) {
	if fb.root != nil {
		return ssa.CallSite{}, false
	}
	ce := fb.r.jit.entryFor(cl.fn)
	var link *jitLink
	switch {
	case ce != nil && ce.ssa != nil:
		link = ce.link
	case ce != nil && (ce.calleeRefused || ce.ssaCallee), !fb.r.jitLinkable(cl):
		if !slices.Contains(fb.wanted, cl) {
			fb.wanted = append(fb.wanted, cl)
		}
		return ssa.CallSite{}, false
	default:
		link = fb.r.jit.linkOf(cl.fn)
		if !slices.Contains(fb.linked, cl) {
			fb.linked = append(fb.linked, cl)
		}
	}
	fn := cl.fn
	this := -1
	if fn.UsesThis {
		this = fn.LocalCount + len(fn.Upvalues)
	}
	fb.links = append(fb.links, link)
	site := ssa.CallSite{
		Callee: uintptr(unsafe.Pointer(callee)), Closure: uintptr(unsafe.Pointer(cl)),
		Entry: uintptr(unsafe.Pointer(&link.entry)), Count: uintptr(unsafe.Pointer(&link.in)),
		Argc: jitB2i(write), Method: true,
		Params: fn.ParamCount, LocalCount: fn.LocalCount, MaxStack: fn.MaxStack, ThisSlot: this, Coerce: fn.CoerceThis,
	}
	if fn.HasTailCall {
		// As a method call's: its calls in tail position may give its
		// frame up.
		site.Tail, site.TailFrame = true, true
	}
	if len(cl.upvalues) != 0 {
		// Its captured bindings, the closure's of the one function object
		// the guards found.
		site.Upvalues = uintptr(unsafe.Pointer(unsafe.SliceData(cl.upvalues)))
	}
	site.StartOnly = jitcompile.StartOnly(fn)
	return site, true
}

// jitLevelCallee is the function object a level native code made a frame
// for at the call at pc of caller's code is running, where it is known: an
// accessor's, a read's or a write's, whose getter or setter the receiver,
// this, has, as a stack trace names it (methodName). Or nil.
func (r *Runtime) jitLevelCallee(caller *bytecode.Function, pc int, this Value) *Object {
	if caller == nil || pc < 0 || pc >= len(caller.Code) || !this.IsObject() {
		return nil
	}
	in := caller.Code[pc]
	if in.Op != bytecode.OpGetProp && in.Op != bytecode.OpSetProp || int(in.A) >= len(caller.Names) {
		return nil
	}
	key := r.atoms.intern(caller.Names[in.A])
	for o := this.Object(); o != nil; o = o.proto {
		i := o.findOwn(key)
		if i < 0 {
			continue
		}
		if int(i) >= len(o.vals) {
			return nil
		}
		pair := o.vals[i].accessorPair()
		switch {
		case pair == nil:
			return nil
		case in.Op == bytecode.OpSetProp:
			return pair.setter
		}
		return pair.getter
	}
	return nil
}

// jitPropertyMade makes, for native code, the property read or write of
// the instruction at pc of cl's code, its operands on the VM's stack up to
// sp (abi.GoCallGet, GoCallSet), as the interpreter makes it, through the
// site's cache: a getter or a setter is called, with the receiver as its
// this. A write's result is undefined.
func (r *Runtime) jitPropertyMade(cl *closure, pc, sp int) (Value, error) {
	in := cl.fn.Code[pc]
	key := cl.names[in.A]
	if in.Op == bytecode.OpSetProp {
		obj, val := r.stack[sp-2], r.stack[sp-1]
		if obj.IsObject() && int(in.B) < len(cl.ic) {
			return Undefined, r.setPropCached(&cl.ic[in.B], obj.Object(), key, val, cl.fn.Strict)
		}
		return Undefined, r.setValueProp(obj, key, val, cl.fn.Strict)
	}
	obj := r.stack[sp-1]
	if obj.IsObject() && int(in.B) < len(cl.ic) {
		o := obj.Object()
		if in.Op == bytecode.OpGetPropThis {
			// As the interpreter reads a method: through its cache, which
			// holds no getter (cachedProp), where cachedGet's entry for one
			// had been taken by the interpreter for the getter's pair as the
			// property's value, the method an object no call could make.
			if v, ok := r.cachedProp(&cl.ic[in.B], o, key); ok {
				return v, nil
			}
			return r.getValueProp(obj, key)
		}
		if v, ok := plainOwn(o, key); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(&cl.ic[in.B], o, key); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, key)
}

// jitB2i is 1 for true, 0 for false.
func jitB2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
