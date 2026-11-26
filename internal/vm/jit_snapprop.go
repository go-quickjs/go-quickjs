//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The property sites' answers derived on the worker (jit_snapderive.go):
// Property's, from a copy of the function's caches and of what its code
// learned of them, which the snapshot's gather makes once on the runtime's
// goroutine (gatherProperty), as Property itself derives them from the live
// caches (jitPropertyOf), one implementation of either.

// jitPropSrc is what a property site's answer is derived from besides the
// function's code (jitPropCode): the caches, what the code learned at its
// exits, and what of the heap the runtime's goroutine may change while a
// worker derives -- the live state (jitPropLive) or a snapshot's copy of
// it (jitPropData).
type jitPropSrc interface {
	// cache is the closure's cache of the site numbered site, nil past
	// them.
	cache(site int) *propCache
	// litRead is regexp_lit's read's at register pc rpc, the register
	// instruction w (regLitCache); knownRead a read's whose own cache
	// never filled (regKnownRead); forward a forwarded callee's read
	// (forwardProperty).
	litRead(rpc int, w bytecode.RInstr) (Atom, *propCache, bool)
	knownRead(rpc int, key Atom) (*propCache, bool)
	forward(rpc int, key Atom) (ssa.PropertySite, bool)
	// polyAt is what the code learned at register pc rpc (jitPolySite),
	// nil for nothing.
	polyAt(rpc int) *jitPolySite
	// symbol is the symbol a private member's key atom denotes.
	symbol(a Atom) *Symbol
	// unindexed reports whether next, the shape an addition makes, wants
	// its table's index built, which native code leaves to Go; cellAdd is
	// the addition a cache of a chain's cell makes (fb.add).
	unindexed(next *shape) bool
	cellAdd(c *propCache) (jitDeepAdd, bool)
	// keep is s, which the code is compiled for, remembered (remember)
	// where it may not be yet.
	keep(s *shape) *shape
	ctx() jitPropCtx
}

// jitPropCtx is what of the asked feedback an answer is derived from: that
// it has learned state (e), is an inlined callee's (root), whose learned
// state is a register entry's (e.reg), and whether a write of a
// primitive's property is left to deoptimize (jitStress.primLeave).
type jitPropCtx struct {
	learned, callee, reg, primLeave bool
}

// jitPropLive is the live state of f's function, as the runtime's
// goroutine reads it; f is nil for a question of a cache alone (siteOf).
type jitPropLive struct {
	f *jitRegFeedback
	r *Runtime
}

func (l jitPropLive) cache(site int) *propCache {
	if cl := l.f.fb.cl; cl != nil && site < len(cl.ic) {
		return &cl.ic[site]
	}
	return nil
}

func (l jitPropLive) litRead(rpc int, w bytecode.RInstr) (Atom, *propCache, bool) {
	return l.f.regLitCache(rpc, w)
}

func (l jitPropLive) knownRead(rpc int, key Atom) (*propCache, bool) {
	return l.f.regKnownRead(rpc, key)
}

func (l jitPropLive) forward(rpc int, key Atom) (ssa.PropertySite, bool) {
	if f := l.f; f.fwd != nil && f.fwd.ctor != nil {
		return f.forwardProperty(rpc, key)
	}
	return ssa.PropertySite{}, false
}

func (l jitPropLive) polyAt(rpc int) *jitPolySite {
	if e := l.f.fb.e; e != nil {
		return jitPolyAt(e.poly, rpc)
	}
	return nil
}

func (l jitPropLive) symbol(a Atom) *Symbol { return l.r.atoms.symbol(a) }

func (l jitPropLive) unindexed(next *shape) bool {
	return next.index == nil && int(next.n) > linearScanLimit
}

func (l jitPropLive) cellAdd(c *propCache) (jitDeepAdd, bool) {
	if !c.cell.valid {
		return jitDeepAdd{}, false
	}
	return l.r.jitDeepAddFrom(c.shape, c.next, c.p1, c.next.key)
}

func (l jitPropLive) keep(s *shape) *shape { return remember(s) }

func (l jitPropLive) ctx() jitPropCtx {
	fb := l.f.fb
	return jitPropCtx{learned: fb.e != nil, callee: fb.root != nil, reg: fb.e != nil && fb.e.reg, primLeave: fb.r.jitStress.primLeave}
}

// jitPolyAt is poly's learned site at register pc rpc, nil for none: one
// at most at a PC (jitPolySiteAt).
func jitPolyAt(poly []jitPolySite, rpc int) *jitPolySite {
	for i := range poly {
		if p := &poly[i]; int(p.pc) == rpc {
			return p
		}
	}
	return nil
}

// jitPropData is a snapshot's copy of what its function's property sites'
// answers are made of (jitPropSrc), which its gather made on the runtime's
// goroutine: the closure's caches, each one's earlier entries a copy of its
// own (its closure's copy, jitClosureCopy); what the code learned of its sites; the caches the feedback makes
// of its own (regLitCache, regKnownRead), copied; and what of the heap the
// derivation asks -- the symbols of private members' keys, the shapes
// whose index an addition wants built, the additions of chains' cells. A
// shape a copied cache names is one its cache remembered, or a shared one,
// whose fields the worker reads as they are: no change of an object's
// layout changes it (remember).
type jitPropData struct {
	code jitPropCode
	ic   []propCache
	// lit and known are the caches of regexp_lit's reads and of known
	// reads, by register pc, copied with their keys.
	lit, known map[int32]jitPropCopied
	poly       []jitPolySite
	syms       map[Atom]*Symbol
	unindex    map[*shape]bool
	cells      []jitPropCellAdd
	x          jitPropCtx
	// accessors and strings are the answers of Accessor and
	// StringProperty asked by register pc (gatherPropertyAsked).
	accessors []jitPropAsked[jitSnapAnsAccessor]
	strings   []jitPropAsked[jitSnapAnsStringProperty]
	// eager marks a forwarded callee's snapshot (forwardProperty), whose
	// reads of its forwarding frame's receiver read the heap: Property
	// is asked at its PCs on the goroutine, as before.
	eager bool
}

// jitPropCopied is a cache the feedback made of its own, copied, with the
// key it reads.
type jitPropCopied struct {
	key Atom
	c   *propCache
}

// jitPropCellAdd is the addition of a cache of a chain's cell (cellAdd),
// by the entry's from, next and receiver's prototype.
type jitPropCellAdd struct {
	from, next *shape
	p1         *Object
	add        jitDeepAdd
	ok         bool
}

func (d *jitPropData) cache(site int) *propCache {
	if site < len(d.ic) {
		return &d.ic[site]
	}
	return nil
}

func (d *jitPropData) litRead(rpc int, w bytecode.RInstr) (Atom, *propCache, bool) {
	c, ok := d.lit[int32(rpc)]
	return c.key, c.c, ok
}

func (d *jitPropData) knownRead(rpc int, key Atom) (*propCache, bool) {
	c, ok := d.known[int32(rpc)]
	return c.c, ok
}

func (d *jitPropData) forward(rpc int, key Atom) (ssa.PropertySite, bool) {
	return ssa.PropertySite{}, false
}

func (d *jitPropData) polyAt(rpc int) *jitPolySite { return jitPolyAt(d.poly, rpc) }

func (d *jitPropData) symbol(a Atom) *Symbol { return d.syms[a] }

func (d *jitPropData) unindexed(next *shape) bool { return d.unindex[next] }

func (d *jitPropData) cellAdd(c *propCache) (jitDeepAdd, bool) {
	for i := range d.cells {
		if k := &d.cells[i]; k.from == c.shape && k.next == c.next && k.p1 == c.p1 {
			return k.add, k.ok
		}
	}
	return jitDeepAdd{}, false
}

// keep is s: the gather remembered what was not yet.
func (d *jitPropData) keep(s *shape) *shape { return s }

func (d *jitPropData) ctx() jitPropCtx { return d.x }

func init() {
	jitSnapDerived |= 1<<snapProperty | 1<<snapAccessor | 1<<snapStringProperty
	jitSnapAskedAtGather |= 1<<snapAccessor | 1<<snapStringProperty
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherProperty, gatherPropertyAsked)
	jitSnapBinders = append(jitSnapBinders, bindPropertyAsked)
	jitSnapDerivers = append(jitSnapDerivers, deriveProperty)
}

// gatherProperty copies, for s, the snapshot of fb, what its property
// sites' answers are made of (jitPropData), on the runtime's goroutine:
// a bulk copy of the closure's caches and of what its code learned, and
// the few questions of the heap the register code's sites ask -- the
// caches regexp_lit's reads and known reads are given (regLitCache,
// regKnownRead), made where the asking made them, the symbols of private
// members' keys -- and of the additions the caches name.
func gatherProperty(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.prop
	if fb.fwd != nil && fb.fwd.ctor != nil {
		// A forwarded callee's: asked at its PCs, as before.
		d.eager = true
		for pc, op := range s.ops {
			if jitSnapAsks[op]&(1<<snapProperty) != 0 {
				jitRegSnapAskProperty(s, fb, pc)
			}
		}
		return
	}
	f, r := fb.fb, fb.fb.r
	// Of the register code: the program, which the answers are put at,
	// is bound after (bindPropertyAsked).
	d.code = fb.propCode()
	d.code.p = nil
	d.x = jitPropLive{f: fb, r: r}.ctx()
	cl := f.cl
	if cl == nil {
		return
	}
	d.ic = s.root.d.closureCopy(cl).ic
	if e := f.e; e != nil {
		d.poly = jitPolyCopy(e.poly)
	}
	rc := fb.rc
	for rpc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RLit:
			if key, c, ok := fb.regLitCache(rpc, w); ok {
				if d.lit == nil {
					d.lit = map[int32]jitPropCopied{}
				}
				d.lit[int32(rpc)] = jitPropCopied{key, jitPropCopyCache(c)}
			}
		case bytecode.RGetPrivate, bytecode.RSetPrivate:
			if rpc >= len(rc.Aux) {
				continue
			}
			c := d.cache(int(rc.Aux[rpc] >> 16))
			if c == nil {
				continue
			}
			for _, write := range [2]bool{false, true} {
				if _, atom, ok := jitPrivateCases(c, write); ok {
					if d.syms == nil {
						d.syms = map[Atom]*Symbol{}
					}
					d.syms[atom] = r.atoms.symbol(atom)
				}
			}
		case bytecode.RGetProp, bytecode.RGetPropNext, bytecode.RGetMethod, bytecode.RGetMethodW:
			site := int(w.C())
			if site >= len(d.ic) || site >= len(rc.SiteName) || int(rc.SiteName[site]) >= len(cl.names) ||
				!jitKnownReadWanted(&d.ic[site], false) || !jitKnownReadAt(rc, rpc) {
				continue
			}
			key := cl.names[rc.SiteName[site]]
			if c, ok := fb.regKnownRead(rpc, key); ok {
				if d.known == nil {
					d.known = map[int32]jitPropCopied{}
				}
				d.known[int32(rpc)] = jitPropCopied{key, jitPropCopyCache(c)}
			}
		}
	}
	// The additions the caches and the learned sites name: whether their
	// next shapes want their index built, a chain's cell's addition, and
	// the prototypes' shapes an addition along a chain is compiled for,
	// remembered as the asking remembered them.
	live := jitPropLive{f: fb, r: r}
	add := func(c *propCache) {
		next := c.next
		if next == nil {
			return
		}
		if live.unindexed(next) {
			if d.unindex == nil {
				d.unindex = map[*shape]bool{}
			}
			d.unindex[next] = true
			return
		}
		if c.cell == nil || c.shape == nil || c.shape == noShape {
			return
		}
		a, ok := live.cellAdd(c)
		d.cells = append(d.cells, jitPropCellAdd{from: c.shape, next: next, p1: c.p1, add: a, ok: ok})
		if ok {
			d.deep(live, a)
		}
	}
	for i := range d.ic {
		add(&d.ic[i])
	}
	for _, c := range d.lit {
		add(c.c)
	}
	for _, c := range d.known {
		add(c.c)
	}
	for i := range d.poly {
		p := &d.poly[i]
		for j := range p.cases {
			add(&p.cases[j])
		}
		for _, a := range p.adds {
			d.deep(live, a)
		}
	}
}

// deep notes a, an addition along a chain: whether its next shape wants
// its index built, and if not, the chain's prototypes' shapes remembered,
// as deepAdd keeps them.
func (d *jitPropData) deep(live jitPropLive, a jitDeepAdd) {
	if live.unindexed(a.next) {
		if d.unindex == nil {
			d.unindex = map[*shape]bool{}
		}
		d.unindex[a.next] = true
		return
	}
	for _, s := range a.shapes {
		remember(s)
	}
}

// jitPropCopyCache is a copy of c, as a closure's copy copies its caches
// (jitCopyCaches).
func jitPropCopyCache(c *propCache) *propCache {
	out, _ := jitCopyCaches([]propCache{*c})
	return &out[0]
}

// jitPolyCopy is a copy of poly, a learned entry's sites, of what a
// property site's answer reads of them: their cases, which the runtime's
// goroutine changes in place, their reads and additions, to which it only
// appends.
func jitPolyCopy(poly []jitPolySite) []jitPolySite {
	if len(poly) == 0 {
		return nil
	}
	out := make([]jitPolySite, len(poly))
	for i := range poly {
		p := &poly[i]
		q := &out[i]
		q.pc, q.mega, q.prim = p.pc, p.mega, p.prim
		q.reads, q.adds = p.reads, p.adds
		if len(p.cases) != 0 {
			q.cases = make([]propCache, len(p.cases))
			copy(q.cases, p.cases)
			for j := range q.cases {
				// A case's earlier entries no answer reads.
				q.cases[j].poly = nil
			}
		}
	}
	return out
}

// deriveProperty puts s's Property answers, at every PC whose op asks it,
// derived from what its gather copied, their effects appended to its sink.
func deriveProperty(s *jitSnapshot) {
	d := &s.d.prop
	if d.eager {
		return
	}
	sink := s.sinkOf()
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapProperty) == 0 {
			continue
		}
		before := s.begin(sink)
		site, ok := jitPropertyOf(&d.code, pc, d, sink)
		eff := s.endDerived(sink, before)
		s.heard(snapProperty, int64(pc), ok, site, ok)
		if ok || eff != 0 {
			s.a.Property.reserve(int(s.room[snapProperty]))
			s.a.Property.put(int64(pc), eff, ok, jitSnapAnsProperty{site, ok})
		}
	}
}

// gatherPropertyAsked asks fb, for s, Accessor and StringProperty by
// register pc, at the sites whose caches, as gatherProperty copied them,
// may answer: an accessor's read or write whose cache, an earlier entry of
// it or what the code learned there holds an accessor (accessorOf's
// cases); a method's read whose code learned the primitives it met there,
// or whose cache and code know nothing of it (stringPropertyOf). Every
// other site's answer is none, with no effect, as the feedback's asking
// gives it: most of the reads and writes these methods are asked at,
// which cost the goroutine the asking. Their answers read the heap -- the
// accessors' pairs, a getter's register code and its lowering, the
// primitives' prototypes -- and are asked so, on the goroutine, of the
// register code: each kept with its register pc and whether it is a
// write's, which bindPropertyAsked puts at the program's PCs lowered of it
// (jitSnapBinders). A forwarded callee's are asked at every PC
// (gatherProperty).
func gatherPropertyAsked(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.prop
	if d.eager {
		for pc, op := range s.ops {
			if jitSnapAsks[op]&(1<<snapAccessor) != 0 {
				jitRegSnapAskAccessor(s, fb, pc)
			}
			if jitSnapAsks[op]&(1<<snapStringProperty) != 0 {
				jitRegSnapAskStringProperty(s, fb, pc)
			}
		}
		return
	}
	cl, e := fb.fb.cl, fb.fb.e
	if cl == nil {
		return
	}
	var poly []jitPolySite
	if e != nil {
		poly = e.poly
	}
	live := jitPropLive{f: fb, r: fb.fb.r}
	code := &d.code
	for rpc, w := range code.rc.Code {
		var read, write, method, plain bool
		switch w.Op() {
		case bytecode.RGetProp, bytecode.RGetPropNext:
			read, plain = true, true
		case bytecode.RGetMethod, bytecode.RGetMethodW:
			read, method = true, true
		case bytecode.RSetProp:
			write = true
		case bytecode.RAddProp, bytecode.RAddPropI:
			// Its read and its store share the store's site.
			read, write = true, true
		case bytecode.RLit:
			// regexp_lit's read of its method, of a cache of the code's own
			// (regLitCache): a method's, which only StringProperty asks of.
			if e == nil {
				continue
			}
			if _, c, ok := d.litRead(rpc, w); !ok || !jitStringPropertyMaybe(e, rpc, c, poly) {
				continue
			}
			if key, lc, ok := live.litRead(rpc, w); ok {
				d.askString(s, fb, rpc, key, lc)
			}
			continue
		default:
			continue
		}
		key, c, ok := code.siteAt(rpc, w, d)
		if !ok || c == nil {
			continue
		}
		p := jitPolyAt(poly, rpc)
		lc := live.cache(int(w.C()))
		if (plain || write) && jitAccessorMaybe(&d.x, c, p) {
			if read && plain {
				d.askAccessor(s, fb, rpc, key, lc, false)
			}
			if write {
				d.askAccessor(s, fb, rpc, key, lc, true)
			}
		}
		if read && method && e != nil && jitStringPropertyMaybe(e, rpc, c, poly) {
			d.askString(s, fb, rpc, key, lc)
		}
	}
}

// jitPropAsked is an answer gatherPropertyAsked asked of the feedback at a
// register pc, a write's where write says: its effect's index past 0, and
// its results.
type jitPropAsked[T any] struct {
	rpc   int32
	write bool
	eff   int32
	ok    bool
	v     T
	// bound marks one put at a PC of the program (bindPropertyAsked).
	bound bool
}

// askAccessor asks fb Accessor's answer for the read or write of key at
// register pc rpc, its cache c, as its asker does at a PC (the effects
// taken back, the inline site's callee asked in turn).
func (d *jitPropData) askAccessor(s *jitSnapshot, fb *jitRegFeedback, rpc int, key Atom, c *propCache, write bool) {
	before := s.begin(fb.fb)
	r0, r1 := fb.accessorAt(rpc, key, c, write)
	eff := s.end(fb.fb, before)
	if r0.Inline != nil {
		nested := s.nestReg(*r0.Inline)
		r0.Inline = &nested
	}
	if r1 || eff != 0 {
		d.accessors = append(d.accessors, jitPropAsked[jitSnapAnsAccessor]{rpc: int32(rpc), write: write, eff: eff, ok: r1, v: jitSnapAnsAccessor{r0, r1}})
	}
}

// askString asks fb StringProperty's answer for the method's read of key
// at register pc rpc, its cache c.
func (d *jitPropData) askString(s *jitSnapshot, fb *jitRegFeedback, rpc int, key Atom, c *propCache) {
	before := s.begin(fb.fb)
	r0, r1 := fb.stringPropertyAt(rpc, key, c)
	eff := s.end(fb.fb, before)
	if r1 || eff != 0 {
		d.strings = append(d.strings, jitPropAsked[jitSnapAnsStringProperty]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsStringProperty{r0, r1}})
	}
}

// bindPropertyAsked puts the answers gatherPropertyAsked asked by register
// pc at each PC of s's program lowered of it whose op asks their method:
// one of a read at a read's, a write's at a write's (propertyAt).
func bindPropertyAsked(s *jitSnapshot) {
	d := &s.d.prop
	d.code.p = s.p
	if d.eager || len(d.accessors)+len(d.strings) == 0 {
		return
	}
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & (1<<snapAccessor | 1<<snapStringProperty)
		if asks == 0 {
			continue
		}
		rpc, _, _, write, _, _, ok := d.code.propertyAt(pc, d)
		if !ok {
			continue
		}
		if asks&(1<<snapAccessor) != 0 {
			for i := range d.accessors {
				if a := &d.accessors[i]; int(a.rpc) == rpc && a.write == write {
					a.bound = true
					s.heard(snapAccessor, int64(pc), a.ok, a.v.r0, a.v.r1)
					s.a.Accessor.reserve(int(s.room[snapAccessor]))
					s.a.Accessor.put(int64(pc), a.eff, a.ok, a.v)
				}
			}
		}
		if asks&(1<<snapStringProperty) != 0 && !write {
			for i := range d.strings {
				if a := &d.strings[i]; int(a.rpc) == rpc {
					a.bound = true
					s.heard(snapStringProperty, int64(pc), a.ok, a.v.r0, a.v.r1)
					s.a.StringProperty.reserve(int(s.room[snapStringProperty]))
					s.a.StringProperty.put(int64(pc), a.eff, a.ok, a.v)
				}
			}
		}
	}
}

// jitAccessorMaybe reports whether accessorOf may find an accessor for a
// site whose cache is c and whose learned site is p (nil for none), of a
// feedback as x has it: its cache's entry or one of its earlier ones is an
// accessor's, of either a read or a write, or the code met one there.
func jitAccessorMaybe(x *jitPropCtx, c *propCache, p *jitPolySite) bool {
	maybe := func(c *propCache) bool {
		return c.getter && c.shape != nil && c.shape != noShape && c.idx >= 0 && c.p1 != nil && (c.next == nil || c.next == setterNext)
	}
	if maybe(c) {
		return true
	}
	if seen := c.seenOf(); seen != nil {
		for i := range seen.entries[:seen.n] {
			if maybe(&seen.entries[i]) {
				return true
			}
		}
	}
	return x.learned && !x.callee && p != nil && len(p.accessors) != 0
}

// jitStringPropertyMaybe reports whether stringPropertyOf may answer for
// the method's read at register pc rpc of e's code, its cache c: the code
// learned the primitives it met there, or neither the cache nor the code
// knows anything of it.
func jitStringPropertyMaybe(e *jitEntry, rpc int, c *propCache, poly []jitPolySite) bool {
	if jitPrimitiveLearned(e, rpc) {
		return true
	}
	return (c.shape == nil || c.shape == noShape) && c.fills == 0 && jitPolyAt(poly, rpc) == nil
}

// siteAt is the property site of the register instruction w at register
// pc rpc -- a read's, a write's or add_prop's, which share the store's
// site -- its key, and its cache as src has it (nil past the closure's
// caches): propertyAt's, of the register code alone.
func (c *jitPropCode) siteAt(rpc int, w bytecode.RInstr, src jitPropSrc) (key Atom, cache *propCache, ok bool) {
	if !c.cl {
		return 0, nil, false
	}
	site := int(w.C())
	if site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(c.names) {
		return 0, nil, false
	}
	return c.names[c.rc.SiteName[site]], src.cache(site), true
}
