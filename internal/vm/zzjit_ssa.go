//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"errors"
	"iter"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/fdlibm"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"golang.org/x/sys/cpu"
)

// jitSSABackend reports whether this architecture has the new pipeline.
const jitSSABackend = true

// jitEncoding is how a Value lies in memory, for generated code, and what
// of an Object it reads: an array's class, flags, length and elements,
// which no Go code changes while native code runs, since nothing else runs
// on a runtime's goroutine then and no other goroutine may use the runtime.
// TestJITObjectLayout holds the fields' widths to what native code loads.
var jitEncoding = abi.Encoding{
	ValueSize:     int32(unsafe.Sizeof(Value{})),
	NumOffset:     int32(unsafe.Offsetof(Value{}.num)),
	RefOffset:     int32(unsafe.Offsetof(Value{}.ref)),
	Undefined:     math.Float64bits(Undefined.num),
	Null:          math.Float64bits(Null.num),
	True:          math.Float64bits(True.num),
	False:         math.Float64bits(False.num),
	Uninitialized: math.Float64bits(uninitialized.num),
	CanonicalNaN:  canonicalNaN,
	UndefinedCell: uint64(uintptr(unsafe.Pointer(&jitUndefinedCell))),
	Object:        objectBits,
	UpvalueSlot:   int32(unsafe.Offsetof(upvalue{}.slot)),
	ObjectShape:   int32(unsafe.Offsetof(Object{}.shape)),
	ObjectProps:   int32(unsafe.Offsetof(Object{}.props)),
	ObjectProto:   int32(unsafe.Offsetof(Object{}.proto)),
	WriteBarrier:  jit.WriteBarrier(),
	CallGo:        jit.CallGo(),
	Round:         runtime.GOARCH == "arm64" || cpu.X86.HasSSE41,
	PropertySize:  int32(unsafe.Sizeof(Property{})),
	PropertyKey:   int32(unsafe.Offsetof(Property{}.key)),
	PropertyFlags: int32(unsafe.Offsetof(Property{}.flags)),
	PropertyValue: int32(unsafe.Offsetof(Property{}.value)),

	ClassObject:     uint8(ClassObject),
	ClassProxy:      uint8(ClassProxy),
	ClassFunction:   uint8(ClassFunction),
	PropNotData:     uint8(propAccessor | propPrivate | propDeleted),
	PropNotWritable: uint8(propAccessor | propPrivate | propDeleted | propUninit | propWritable),
	PropWritable:    uint8(propWritable),
	PropUninit:      uint8(propUninit),

	String:       math.Float64bits(mkTag(KindString, 0)),
	Symbol:       math.Float64bits(mkTag(KindSymbol, 0)),
	StringData:   int32(unsafe.Offsetof(String{}.s)),
	StringLeft:   int32(unsafe.Offsetof(String{}.left)),
	StringLength: int32(unsafe.Offsetof(String{}.length)),
	StringASCII:  int32(unsafe.Offsetof(String{}.ascii)),
	StringU16:    int32(unsafe.Offsetof(String{}.u16)),

	ObjectClass:        int32(unsafe.Offsetof(Object{}.class)),
	ObjectFlags:        int32(unsafe.Offsetof(Object{}.flags)),
	ObjectArrayLen:     int32(unsafe.Offsetof(Object{}.arrayLen)),
	ObjectElems:        int32(unsafe.Offsetof(Object{}.elems)),
	ObjectData:         int32(unsafe.Offsetof(Object{}.data)),
	IterState:          jitTypeWord((*iterState)(nil)),
	IterDone:           int32(unsafe.Offsetof(iterState{}.done)),
	IterArr:            int32(unsafe.Offsetof(iterState{}.arr)),
	IterIdx:            int32(unsafe.Offsetof(iterState{}.idx)),
	FuncData:           jitTypeWord((*funcData)(nil)),
	FuncClosure:        int32(unsafe.Offsetof(funcData{}.closure)),
	ClosureFn:          int32(unsafe.Offsetof(closure{}.fn)),
	ClosureUpvalues:    int32(unsafe.Offsetof(closure{}.upvalues)),
	ClosureEnv:         int32(unsafe.Offsetof(closure{}.env)),
	ClosureRealm:       int32(unsafe.Offsetof(closure{}.realm)),
	ClassArray:         uint8(ClassArray),
	FlagSparse:         uint8(objHasSparseElements),
	FlagHTMLDDA:        uint8(objHTMLDDA),
	FlagExtensible:     uint8(objExtensible),
	FlagLengthWritable: uint8(objArrayLengthWritable),
}

// jitReoptimizations is how many times a function's code is compiled again
// for speculations that failed (jitDeoptimized); after that its guards
// deoptimize, as any do, and jitSSAProfit decides whether it pays.
const jitReoptimizations = 4

// jitDeoptimized notes a guard that failed in e's code, at the site
// ExitSite names, or, for -1, an entry's speculation at the entry the
// stretch started at: the code is compiled again at its next entry, with
// the site, or the entry, generic (jitFeedback.Generic). A site the
// builder has no generic form for is built as before, and its next failure
// is not new.
func jitDeoptimized(e *jitEntry, ctx *abi.Context, start int) {
	if e.reopts >= jitReoptimizations {
		return
	}
	if site := int32(int64(ctx.ExitSite)); site >= 0 {
		if !slices.Contains(e.failed, site) {
			e.failed = append(e.failed, site)
			e.reopt = true
		}
	} else if !slices.Contains(e.failedEntries, uint32(start)) {
		e.failedEntries = append(e.failedEntries, uint32(start))
		e.reopt = true
	}
}

// jitFedSite is a property site, by PC, and the shape its cache knew when
// the code was compiled, by address, or 0 for none; and how often it has
// left native code since its cache learned another.
type jitFedSite struct {
	pc     uint32
	shape  uintptr
	missed uint32
	// seen is the shape its cache knew at the last of those exits: they are
	// counted while it stays the one.
	seen uintptr
}

// jitPolySite is a read that met objects of shapes the code was not
// compiled for (jitEntry.poly): where each had the property, as a cache
// filled for it has it (fillPropCache).
type jitPolySite struct {
	pc    uint32
	cases []propCache
	// reads are the reads' cases past what a cache holds (jitDeepRead).
	reads []jitDeepRead
	// adds, for a write, are additions along chains deeper than a cache
	// holds (jitDeepAdd).
	adds []jitDeepAdd
}

// jitDeepAdd is an addition of a property along a shared transition, from
// from to next, to an object whose prototype chain is up, each with the
// shape in shapes, none intercepting it: what fillStoreCache finds, but
// for a chain of up to jitAddDepth, which a cache, holding two, cannot.
type jitDeepAdd struct {
	from, next *shape
	up         []*Object
	shapes     []*shape
}

// jitAddDepth is how deep a prototype chain an addition is compiled for.
const jitAddDepth = 6

// jitDeepAddOf is the addition of key a write just made to o, of shape
// before then, if it took a shared transition along a chain of plain
// prototypes up to jitAddDepth that none intercepts, as fillStoreCache
// requires of a cache's -- or false.
func (r *Runtime) jitDeepAddOf(o *Object, before *shape, key Atom) (jitDeepAdd, bool) {
	return r.jitDeepAddFrom(before, o.shape, o.proto, key)
}

// jitDeepAddFrom is jitDeepAddOf's addition of key, from shape before to
// next, to an object whose prototype is proto, with the chain as it is now.
func (r *Runtime) jitDeepAddFrom(before, next *shape, proto *Object, key Atom) (jitDeepAdd, bool) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	if before == nil || before == noShape || before.unique || next == nil || next.unique || next.parent != before || next.key != key ||
		next.flags&(plain|propWritable) != propWritable {
		return jitDeepAdd{}, false
	}
	a := jitDeepAdd{from: before, next: next}
	for p := proto; p != nil; p = p.proto {
		if len(a.up) == jitAddDepth || r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
			return jitDeepAdd{}, false
		}
		a.up, a.shapes = append(a.up, p), append(a.shapes, p.shape)
	}
	return a, true
}

// jitPolyWriteSeen notes, after Go made the write at pc that left native
// code, what its cache knows of the object it wrote -- an own property, or
// the transition it was added along -- for the code to be compiled again
// for it with the shapes the site met before (writeCases), up to
// jitPropertyCases, as for reads (jitPolySeen). DeltaBlue's Constraint
// adds strength to the receivers of four constructors, each of a shape of
// its own; RayTrace writes hitCount to infos of two.
func (r *Runtime) jitPolyWriteSeen(f *frame, e *jitEntry, pc int, in bytecode.Instr, o *Object, before *shape) {
	if int(in.B) >= len(f.cl.ic) {
		return
	}
	i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
	if i < 0 {
		e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
		i = len(e.poly) - 1
	}
	p := &e.poly[i]
	if len(p.cases)+len(p.adds) >= jitPropertyCases {
		return
	}
	c := f.cl.ic[in.B]
	if c.shape == nil || c.shape == noShape || c.getter || c.next == setterNext || c.idx < 0 || c.next == nil && c.p1 != nil {
		// No cache: an addition along a chain deeper than one holds, as
		// the object took it.
		if o == nil || int(in.A) >= len(f.cl.names) {
			return
		}
		a, ok := r.jitDeepAddOf(o, before, f.cl.names[in.A])
		if !ok || slices.ContainsFunc(p.adds, func(k jitDeepAdd) bool { return k.from == a.from }) {
			return
		}
		p.adds = append(p.adds, a)
		e.polyPending, e.polySettled = true, 0
		return
	}
	if slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape }) {
		return
	}
	p.cases = append(p.cases, c)
	e.polyPending, e.polySettled = true, 0
}

// jitWriteTarget is the object a set_prop about to be made writes, its
// operands the two values below sp, and its shape now; nil for another
// instruction or a primitive.
func jitWriteTarget(stack []Value, sp int, in bytecode.Instr) (*Object, *shape) {
	if in.Op != bytecode.OpSetProp || sp < 2 || sp > len(stack) || !stack[sp-2].IsObject() {
		return nil, nil
	}
	o := stack[sp-2].Object()
	return o, o.shape
}

// jitPropertyCases is how many shapes a read is compiled for, as V8's
// polymorphic inline caches keep: one more leaves for Go.
const jitPropertyCases = 4

// jitPolySeen notes a read native code left to Go at pc, its receiver on top
// of the frame's operands, up to sp: one of a shape the read has not met --
// found on a prototype, or the receiver's own -- joins those it has, up to
// jitPropertyCases, and the code is compiled again for them once they have
// settled (jitPolySettled). An own property native code also scans for
// (mir's property), but only in a table of up to abi.MaxScan: RayTrace's
// IntersectionInfo has more.
func (r *Runtime) jitPolySeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	recv := r.stack[sp-1]
	if recv.IsString() && in.Op == bytecode.OpGetPropThis && !slices.Contains(e.strSites, uint32(pc)) {
		// A string's method: read off String.prototype by the code
		// compiled again, as for a shape the read had not met.
		e.strSites = append(e.strSites, uint32(pc))
		e.polyPending, e.polySettled = true, 0
		return
	}
	if !recv.IsObject() || int(in.A) >= len(f.cl.names) {
		return
	}
	var c propCache
	r.fillPropCache(&c, recv.Object(), f.cl.names[in.A], false)
	if c.shape == nil {
		// One a cache cannot hold, found past its two prototypes.
		if d, ok := r.jitDeepReadOf(recv.Object(), f.cl.names[in.A]); ok && jitAddDeepRead(jitPolySiteAt(e, pc), d) {
			e.polyPending, e.polySettled = true, 0
		}
		return
	}
	if c.shape == noShape || c.getter || c.idx < 0 {
		return
	}
	holder := recv.Object()
	if c.p2 != nil {
		holder = c.p2
	} else if c.p1 != nil {
		holder = c.p1
	}
	if c.idx < int32(len(holder.props)) && !holder.props[c.idx].value.IsNumber() {
		jitNumberMissed(e, pc)
	}
	if !jitAddReadCase(jitPolySiteAt(e, pc), c) {
		return
	}
	e.polyPending, e.polySettled = true, 0
}

// jitPrivateSeen notes a private member's read or write, or #x in o,
// native code left to Go at pc, its operands on the frame's up to sp: an
// object of a shape the site has not met, whose own data the member is --
// a field, or, but for a write, a method -- joins the site's cases, as a property read's do
// (jitPolySeen), and the code is compiled again for them. A key in a local,
// of a class the function itself evaluates, is Go's.
func (r *Runtime) jitPrivateSeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	if e == nil || in.B&1 == 0 || int(in.B>>1) >= len(f.cl.upvalues) {
		return
	}
	at := sp - 1
	if in.Op == bytecode.OpSetPrivate {
		at = sp - 2
	}
	if at < 0 || !r.stack[at].IsObject() {
		return
	}
	o := r.stack[at].Object()
	sh, i := o.shape, o.findOwn(privateKey(f, f.cl, in.B))
	if sh == nil || sh == noShape || i < 0 || i >= sh.n || int(i) >= len(o.props) {
		return
	}
	if p := &o.props[i]; p.isAccessor() || in.Op == bytecode.OpSetPrivate && p.flags&propWritable == 0 {
		return
	} else if in.Op == bytecode.OpGetPrivate && !p.value.IsNumber() {
		// A read that met a value not a number reads it as a value.
		jitNumberMissed(e, pc)
	}
	if jitAddReadCase(jitPolySiteAt(e, pc), propCache{shape: sh, idx: i}) {
		e.polyPending, e.polySettled = true, 0
	}
}

// privateProperty is Property's site for a private member's read or write
// at pc: the property of the key the closure's binding holds -- a symbol
// each evaluation of the class mints, which the code checks the binding
// holds (ssa.PropertySite's Private) -- in the objects of the shapes the
// site met (jitPrivateSeen), the first its own, the rest its cases. A site
// that has met none, or whose key is in a local, is Go's.
func (fb *jitFeedback) privateProperty(pc int, in bytecode.Instr) (ssa.PropertySite, bool) {
	ref := int(in.B >> 1)
	if in.B&1 == 0 || ref >= len(fb.cl.upvalues) || fb.cl.upvalues[ref] == nil {
		return ssa.PropertySite{}, false
	}
	key := fb.cl.upvalues[ref].get()
	e := fb.e
	if fb.root != nil || e == nil {
		// An inlined callee's: what its own code met.
		e = fb.r.jit.cache[weak.Make(fb.fn)]
	}
	if !key.IsSymbol() || e == nil {
		return ssa.PropertySite{}, false
	}
	var cases []propCache
	for i := range e.poly {
		if int(e.poly[i].pc) == pc {
			cases = e.poly[i].cases
		}
	}
	if len(cases) == 0 {
		return ssa.PropertySite{}, false
	}
	site := ssa.PropertySite{Key: uint32(fb.r.atoms.internSymbol(key.Symbol())), Private: uintptr(unsafe.Pointer(key.Symbol()))}
	k := fb.keep()
	for i, c := range cases {
		k.shapes = append(k.shapes, remember(c.shape))
		if i == 0 {
			site.Shape, site.Index = uintptr(unsafe.Pointer(c.shape)), c.idx
			continue
		}
		site.Cases = append(site.Cases, ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(c.shape)), Index: c.idx})
	}
	return site, true
}

// jitNumberMissed notes that the read at pc met a value that is not a
// number. Lowering makes a read a number's where any use of its value
// wants one (ir.PropertyRead) -- a use on a branch never taken, too:
// RayTrace's setPixel multiplies options.pixelWidth, the string "5", only
// when it draws to a canvas -- and its code leaves for Go wherever the
// value is not, which no cache's shape changes. So the read is generic
// once it has (jitEntry.failed), as a failed guard's site is, and the code
// compiled again reads it as a value, as V8's code deoptimized for a
// value not a number reads it tagged once optimized again; its uses that
// want a number unbox it, each with its own guard.
func jitNumberMissed(e *jitEntry, pc int) {
	if e.reopts >= jitReoptimizations || slices.Contains(e.failed, int32(pc)) {
		return
	}
	if p := e.ssaProg; e.ssa == nil || p == nil || pc >= len(p.Code) || p.Code[pc].Op != ir.PropertyRead {
		return
	}
	e.failed = append(e.failed, int32(pc))
	e.reopt = true
}

// jitPolySiteAt is e's site at pc (jitEntry.poly), made if it has none.
func jitPolySiteAt(e *jitEntry, pc int) *jitPolySite {
	i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
	if i < 0 {
		e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
		i = len(e.poly) - 1
	}
	return &e.poly[i]
}

// jitAddReadCase adds c to a read's cases (jitPolySite), up to
// jitPropertyCases, and reports whether it did. A case it supersedes goes
// -- one of a shape its own came from by adding properties, as an object
// whose methods give it fields after its construction moves on from its
// constructor's shape, which no object of the site may have any more --
// and one superseded by a case there is not added: the stale shapes took
// the cases, and the code left native code at every read of the live
// ones, compiled again and again for the same stale ones until its
// compiles ran out. V8 drops deprecated maps from its inline caches so.
func jitAddReadCase(p *jitPolySite, c propCache) bool {
	if slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape || jitSuperseded(c.shape, k.shape) }) {
		return false
	}
	p.cases = slices.DeleteFunc(p.cases, func(k propCache) bool { return jitSuperseded(k.shape, c.shape) })
	if len(p.cases) >= jitPropertyCases {
		return false
	}
	p.cases = append(p.cases, c)
	return true
}

// jitSuperseded reports whether t came from s by adding properties: s is
// one of t's ancestors along the shapes' transitions.
func jitSuperseded(s, t *shape) bool {
	for a := t.parent; a != nil; a = a.parent {
		if a == s {
			return true
		}
	}
	return false
}

// jitCacheSeen is a cache's earlier entries (propCache.seen), nil until
// the site has met objects of a second shape.
type jitCacheSeen = *propSeen

// propSeen is a site's earlier cache entries, each of its own shape, the
// latest last.
type propSeen struct {
	n       uint8
	entries [3]propCache
	// deep are the additions the write made along prototype chains deeper
	// than a cache holds (jitNoteDeepAdd), up to jitPropertyCases; reads,
	// the properties a read found so (jitNoteDeepRead).
	deep  []jitDeepSeen
	reads []jitDeepRead
}

// jitDeepRead is a read of a plain data property an object of shape found
// on a prototype further up than a cache holds (jitDeepReadOf): at idx in
// the table of the last of up, the prototypes from the object's on, each
// of the shape in shapes when it was found, which the code checks.
type jitDeepRead struct {
	shape  *shape
	idx    int32
	up     []*Object
	shapes []*shape
}

// jitDeepReadOf is the read of key from o, if o's prototype chain answers
// it past the two prototypes a cache holds, up to jitAddDepth, with a
// plain data property, each object on the way of a class with shapes that
// does not answer the key itself, as fillPropCache requires of a cache's
// -- or false.
func (r *Runtime) jitDeepReadOf(o *Object, key Atom) (jitDeepRead, bool) {
	if key.IsIndex() || o.shape == nil || o.shape == noShape || !shapeClass(o.class) || synthesized(o.class, key) || o.findOwn(key) >= 0 {
		return jitDeepRead{}, false
	}
	d := jitDeepRead{shape: o.shape}
	for p := o.proto; p != nil; p = p.proto {
		if len(d.up) == jitAddDepth || r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return jitDeepRead{}, false
		}
		d.up, d.shapes = append(d.up, p), append(d.shapes, p.shape)
		if i := p.findOwn(key); i >= 0 {
			if p.props[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || len(d.up) <= 2 {
				return jitDeepRead{}, false
			}
			d.idx = int32(i)
			remember(d.shape)
			for _, s := range d.shapes {
				remember(s)
			}
			return d, true
		}
	}
	return jitDeepRead{}, false
}

// jitNoteDeepRead notes, in c's entries seen, the read of key from o a
// cache could not remember, its prototype chain deeper than one holds, if
// a prototype answers it (jitDeepReadOf): a method a class two levels
// below the one that has it inherits, as DeltaBlue's StayConstraint does
// isInput and addConstraint. Code compiled for the site reads it natively
// from the first (jitFeedback.Property); before, it left native code at
// every such read, learning nothing, until its callers stopped calling it
// natively.
func (r *Runtime) jitNoteDeepRead(c *propCache, o *Object, key Atom) {
	s := c.seen
	if s != nil {
		for i := range s.reads {
			if s.reads[i].shape == o.shape {
				return
			}
		}
		if len(s.reads) >= jitPropertyCases {
			return
		}
	}
	d, ok := r.jitDeepReadOf(o, key)
	if !ok {
		return
	}
	if s == nil {
		s = &propSeen{}
		c.seen = s
	}
	s.reads = append(s.reads, d)
}

// jitAddDeepRead adds d to a read's deep cases (jitPolySite.reads), up to
// jitPropertyCases with its others, and reports whether it did.
func jitAddDeepRead(p *jitPolySite, d jitDeepRead) bool {
	if len(p.cases)+len(p.reads) >= jitPropertyCases || slices.ContainsFunc(p.reads, func(k jitDeepRead) bool { return k.shape == d.shape }) ||
		slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == d.shape }) {
		return false
	}
	p.reads = append(p.reads, d)
	return true
}

// jitDeepSeen is an addition a write made, from shape from to next, to an
// object whose prototype was proto: what jitDeepAddOf finds of it, the
// chain as it is when the code is compiled (jitDeepAddFrom).
type jitDeepSeen struct {
	from, next *shape
	proto      *Object
}

// jitNoteDeepAdd notes, in c's entries seen, an addition a write made to an
// object whose prototype chain is deeper than a cache holds, which the
// cache cannot remember -- a constructor's this.x = v in a class two levels
// below another, as DeltaBlue's constraints' are -- for code compiled for
// the site to make it natively from the first (jitSeenFeedback). Before,
// native code learned such additions one exit and one compile at a time:
// a constructor's at each of its sites in turn, for each of its receivers'
// shapes.
func jitNoteDeepAdd(c *propCache, from, next *shape, proto *Object) {
	s := c.seen
	if s != nil {
		for _, d := range s.deep {
			if d.from == from {
				return
			}
		}
		if len(s.deep) >= jitPropertyCases {
			return
		}
	} else {
		s = &propSeen{}
		c.seen = s
	}
	s.deep = append(s.deep, jitDeepSeen{from: remember(from), next: remember(next), proto: proto})
}

// jitNoteSeen keeps old, what a cache remembered before it remembers n,
// among the entries it has seen, if it was of another shape: replacing
// the one of its shape, or the oldest when they are full.
func jitNoteSeen(s *propSeen, old, n propCache) *propSeen {
	if old.shape == nil || old.shape == noShape || old.shape == n.shape {
		return s
	}
	old.seen = nil
	if s == nil {
		s = &propSeen{}
	}
	for i := range s.n {
		if s.entries[i].shape == old.shape {
			s.entries[i] = old
			return s
		}
	}
	if int(s.n) < len(s.entries) {
		s.entries[s.n] = old
		s.n++
		return s
	}
	copy(s.entries[:], s.entries[1:])
	s.entries[len(s.entries)-1] = old
	return s
}

// jitSeenFeedback adds to e's sites what cl's caches have seen besides
// what they remember now (propCache.seen): the shapes the interpreter and
// the tree tier met at each property read and write, which the code is
// compiled for from the first, as V8's optimizing compiler takes the maps
// its inline caches met, not learned one exit and one compile at a time.
// Each is taken as an exit's would be (jitPolySeen, jitPolyWriteSeen).
func (r *Runtime) jitSeenFeedback(cl *closure, e *jitEntry) {
	for pc, in := range cl.fn.Code {
		write := in.Op == bytecode.OpSetProp
		if !write && in.Op != bytecode.OpGetProp && in.Op != bytecode.OpGetPropThis || int(in.B) >= len(cl.ic) || cl.ic[in.B].seen == nil {
			continue
		}
		seen, now := cl.ic[in.B].seen, cl.ic[in.B].shape
		for _, c := range seen.entries[:seen.n] {
			if c.shape == nil || c.shape == noShape || c.getter || c.idx < 0 || write && (c.next == setterNext || c.next == nil && c.p1 != nil) {
				continue
			}
			if !write && now != nil && jitSuperseded(c.shape, now) {
				// What the site's objects were before they took the shape
				// its cache remembers now.
				continue
			}
			i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
			if i < 0 {
				e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
				i = len(e.poly) - 1
			}
			p := &e.poly[i]
			if !write {
				jitAddReadCase(p, c)
				continue
			}
			if len(p.cases)+len(p.adds) >= jitPropertyCases || slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape }) {
				continue
			}
			p.cases = append(p.cases, c)
		}
		if !write {
			// The reads a cache could not hold, past its two prototypes.
			for _, d := range seen.reads {
				jitAddDeepRead(jitPolySiteAt(e, pc), d)
			}
			continue
		}
		if len(seen.deep) == 0 || int(in.A) >= len(cl.names) {
			continue
		}
		// The additions along chains deeper than a cache holds, as the chain
		// is now; one that changed since is left to Go.
		i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
		if i < 0 {
			e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
			i = len(e.poly) - 1
		}
		p := &e.poly[i]
		for _, d := range seen.deep {
			if len(p.cases)+len(p.adds) >= jitPropertyCases || slices.ContainsFunc(p.adds, func(k jitDeepAdd) bool { return k.from == d.from }) {
				continue
			}
			if a, ok := r.jitDeepAddFrom(d.from, d.next, d.proto, cl.names[in.A]); ok {
				p.adds = append(p.adds, a)
			}
		}
	}
}

// jitPolySettle is how many exits in a row native code makes, once a read
// met a shape it was not compiled for, that find no other at any read
// before the code is compiled again for them: a method whose reads meet a
// class's subclasses one by one, as DeltaBlue's constraints are met, is
// compiled again once for them all, not at each -- as V8 optimizes again
// once a function's feedback has settled rather than at each new map.
const jitPolySettle = 4

// jitPolySettled counts an exit from e's code toward compiling it again for
// the shapes its reads have met (jitPolySeen).
func jitPolySettled(e *jitEntry) {
	if e.polyPending {
		if e.polySettled++; e.polySettled >= jitPolySettle {
			e.polyPending, e.polySettled, e.polyReopt = false, 0, true
		}
	}
}

// jitWrongShapeExits is how often a site compiled for one shape leaves
// native code in a row, its cache knowing one other, before the code is
// compiled again: compiling is not free, and a site whose objects are of
// more shapes than one would have it compiled for each in turn.
const jitWrongShapeExits = 32

// jitFed notes an exit to Go at pc. A property site whose cache has met
// objects of a shape the code was not compiled for -- none, for code
// compiled on its first call or a branch not taken until then; another,
// when its receivers' shape changed after -- leaves for Go every time;
// once its cache knows a shape, the code is compiled again for it, as V8
// deoptimizes for insufficient feedback, or for a wrong map, after
// jitWrongShapeExits in a row with one shape. Code compiled before a site
// had run -- a function with a loop is compiled in its first call, before
// its later branches have -- is compiled again at the function's next
// invocation, not at once (feedReopt): its other sites without feedback
// learn theirs meanwhile, at their exits, and one compile takes them all,
// as V8's code deoptimized for insufficient feedback leaves the rest of
// the invocation to the interpreter, which gathers it, and optimizes once
// more. Compiled again at once, at each such site in turn, DeltaBlue's
// Planner methods were compiled four times each in their first runs, and
// then left native code at the sites the fourth compile had not seen.
func jitFed(cl *closure, e *jitEntry, pc uint32) {
	i := slices.IndexFunc(e.fed, func(s jitFedSite) bool { return s.pc == pc })
	if i < 0 || e.reopts >= jitReoptimizations || int(pc) >= len(cl.fn.Code) {
		return
	}
	in := cl.fn.Code[pc]
	if int(in.B) >= len(cl.ic) {
		return
	}
	c, site := &cl.ic[in.B], &e.fed[i]
	if c.shape == noShape || uintptr(unsafe.Pointer(c.shape)) == site.shape {
		return
	}
	if now := uintptr(unsafe.Pointer(c.shape)); now != site.seen {
		site.seen, site.missed = now, 0
	}
	if site.missed++; site.shape == 0 {
		e.fed = slices.Delete(e.fed, i, i+1)
		if !e.feedReopt {
			e.feedReopt, e.feedStarts, e.feedWork = true, e.starts+e.link.in, e.ssaStats.work
		}
	} else if site.missed >= jitWrongShapeExits {
		e.fed = slices.Delete(e.fed, i, i+1)
		e.reopt = true
	}
}

// jitLiteral is an object literal's site, at pc, that native code makes
// the objects of from pool, each of shape root, the literal's as the
// closure compiled for had it (shapeTree.siteRoot): its fields are
// compiled for the transitions out of it.
type jitLiteral struct {
	pc   int32
	root *shape
	pool *abi.ObjectPool
	// array marks an array literal's (new_array), whose arrays have room
	// for n elements and none yet.
	array bool
	n     int
	// lit is a regular expression literal's pattern (new_regexp), whose
	// objects are its RegExps.
	lit *bytecode.Constant
}

// jitLiteralAt is e's literal at pc, made for cl's site if it has none, or
// nil if the literal's objects have no shape to start from.
func (r *Runtime) jitLiteralAt(e *jitEntry, cl *closure, pc int) *jitLiteral {
	for i := range e.literals {
		if int(e.literals[i].pc) == pc {
			return &e.literals[i]
		}
	}
	in := cl.fn.Code[pc]
	if in.Op == bytecode.OpNewArray && in.A <= ir.MaxArrayLiteral {
		e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), array: true, n: int(in.A)})
		l := &e.literals[len(e.literals)-1]
		l.pool.Source = unsafe.Pointer(&jitPoolSource{array: true, n: l.n})
		r.jitFillLiteral(l)
		return l
	}
	if in.Op == bytecode.OpNewRegExp && int(in.A) < len(cl.fn.Constants) {
		// Fresh RegExps of the pattern, each with its lastIndex 0, which
		// nothing sees before the code takes one: what the literal makes
		// at each evaluation. A pattern that does not compile has Go
		// throw.
		lit := &cl.fn.Constants[in.A]
		if _, err := r.newRegExpLiteral(lit); err != nil {
			return nil
		}
		e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), lit: lit})
		l := &e.literals[len(e.literals)-1]
		l.pool.Source = unsafe.Pointer(&jitPoolSource{lit: lit})
		r.jitFillLiteral(l)
		return l
	}
	if in.Op != bytecode.OpNewObject || int(in.B) >= len(cl.ic) {
		return nil
	}
	root := r.shapes.siteRoot(&cl.ic[in.B])
	if root == nil {
		return nil
	}
	e.literals = append(e.literals, jitLiteral{pc: int32(pc), root: root, pool: new(abi.ObjectPool), n: int(in.A)})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(&jitPoolSource{root: root, n: l.n})
	r.jitFillLiteral(l)
	return l
}

// jitFillLiteral fills l's pool with objects as the literal makes them,
// room for its n fields: Size of them, as jitFillPool fills a pool. An
// array literal's are arrays with room for its elements, none yet, of the
// layout a property cache gives an array (ensureShape), as jitFillPool's
// for new Array() are.
func (r *Runtime) jitFillLiteral(l *jitLiteral) {
	pool := l.pool
	if pool.Size == 0 {
		pool.Size = abi.PoolSize
	}
	clear(pool.Objects[:])
	if l.lit != nil {
		for i := range int(pool.Size) {
			v, err := r.newRegExpLiteral(l.lit)
			if err != nil {
				pool.Count = 0
				return
			}
			pool.Objects[i] = unsafe.Pointer(v.Object())
		}
		pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.regexp)
		return
	}
	for i := range int(pool.Size) {
		if l.array {
			a := newArrayObject(r.proto.array, l.n)
			a.elems = a.elems[:0]
			r.ensureShape(a)
			pool.Objects[i] = unsafe.Pointer(a)
			continue
		}
		// Room for what its objects came to take after it, as a
		// constructor's are made (jitPoolObject): a property added to
		// one natively needs the room (addAlong).
		n := l.n
		if l.root != nil {
			n = max(n, int(l.root.slack))
		}
		o := newLiteralObject(r.proto.object, ClassObject, n)
		o.shape = l.root
		pool.Objects[i] = unsafe.Pointer(o)
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.object)
	if l.array {
		pool.Proto = unsafe.Pointer(r.proto.array)
	}
}

// jitDefineSeen notes an exit at a literal's field, pc, that the code was
// compiled for before its transition was made: Go, defining it, made it,
// and the code is compiled again for it (reoptDue), as for any feedback it
// lacked.
func jitDefineSeen(e *jitEntry, pc int) {
	if i := slices.Index(e.definesUnfed, int32(pc)); i >= 0 {
		e.definesUnfed = slices.Delete(e.definesUnfed, i, i+1)
		e.polyReopt = true
	}
}

// jitRefillLiteral fills again, with more objects, the pool of the literal
// at pc that native code left at, its pool run out.
func (r *Runtime) jitRefillLiteral(f *frame, e *jitEntry, pc int) {
	for i := range e.literals {
		if l := &e.literals[i]; int(l.pc) == pc && l.pool.Count == 0 {
			l.pool.Size = min(2*max(l.pool.Size, abi.PoolSize), abi.PoolCapacity)
			r.jitFillLiteral(l)
		}
	}
}

// StringCell is ssa.ConstantFeedback's: a cell holding the string constant
// pushed at pc, as the closure has it, which the code keeps
// (jitEntry.ssaCells): natively read, the constant had left native code at
// each push for Go to make it (jitHost).
func (fb *jitFeedback) StringCell(pc int) (uintptr, bool) {
	if fb.cl == nil || pc >= len(fb.fn.Code) {
		return 0, false
	}
	in := fb.fn.Code[pc]
	cell := new(Value)
	switch {
	case in.Op == bytecode.OpPushEmptyString:
		*cell = Str(emptyString)
	case in.Op != bytecode.OpPushConst || int(in.A) >= len(fb.cl.consts) || !fb.cl.consts[in.A].IsString():
		return 0, false
	default:
		*cell = fb.cl.consts[in.A]
	}
	k := fb.keep()
	k.cells = append(k.cells, cell)
	return uintptr(unsafe.Pointer(cell)), true
}

// Literal is ssa.LiteralFeedback's: the object literal at pc, made natively
// from its pool, in the function's own code.
func (fb *jitFeedback) Literal(pc int) (ssa.LiteralSite, bool) {
	if fb.e == nil || fb.root != nil || fb.fwd != nil || fb.cl == nil || pc >= len(fb.fn.Code) {
		return ssa.LiteralSite{}, false
	}
	l := fb.r.jitLiteralAt(fb.e, fb.cl, pc)
	if l == nil {
		return ssa.LiteralSite{}, false
	}
	k := fb.keep()
	k.pools = append(k.pools, l.pool)
	if l.root != nil {
		k.shapes = append(k.shapes, remember(l.root))
	}
	return ssa.LiteralSite{Pool: uintptr(unsafe.Pointer(l.pool))}, true
}

// Define is ssa.LiteralFeedback's: what the literal's field at pc adds, as
// define_field adds it along the shapes' transitions -- from the shape the
// literal's object has there, its root's after each field before it,
// found in the code back to its new_object. A literal with anything else
// done to its object -- a spread, an accessor, a computed key, a
// prototype set -- has Go define its fields.
func (fb *jitFeedback) Define(pc int) (*ssa.PropertyAdd, bool) {
	fn := fb.fn
	if fb.e == nil || fb.root != nil || fb.fwd != nil || fb.cl == nil || pc >= len(fn.Code) || fn.Code[pc].Op != bytecode.OpDefineField {
		return nil, false
	}
	if fb.prog == nil {
		p, err := fb.r.jitLower(fn, jitLowerInline)
		if err != nil {
			if p, err = fb.r.jitLower(fn, jitLowerSSA); err != nil {
				return nil, false
			}
		}
		fb.prog = p
	}
	maps := fb.prog.Maps
	if len(maps) != len(fn.Code) || maps[pc].Depth < 2 {
		return nil, false
	}
	object := maps[pc].Depth - 2
	var keys []Atom
	q := -1
	for k := pc - 1; k >= 0 && q < 0; k-- {
		d := maps[k].Depth
		if d < object {
			return nil, false
		}
		switch in := fn.Code[k]; in.Op {
		case bytecode.OpNewObject:
			if d == object {
				q = k
			}
		case bytecode.OpDefineField:
			if d-2 == object {
				keys = append(keys, fb.cl.names[in.A])
			}
		case bytecode.OpCopyDataProps, bytecode.OpSetProtoOf, bytecode.OpDefineGetter, bytecode.OpDefineSetter:
			if d-2 == object {
				return nil, false
			}
		case bytecode.OpDefineIndex, bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
			if d-3 == object {
				return nil, false
			}
		}
	}
	if q < 0 {
		return nil, false
	}
	l := fb.r.jitLiteralAt(fb.e, fb.cl, q)
	if l == nil {
		return nil, false
	}
	s := l.root
	slices.Reverse(keys)
	for _, key := range keys {
		if s.next == nil || s.nextEdge != (shapeEdge{key, propDefault}) {
			return nil, false
		}
		s = s.next
	}
	key := fb.cl.names[fn.Code[pc].A]
	if s.next == nil || s.nextEdge != (shapeEdge{key, propDefault}) {
		if s.next == nil {
			// Not made yet: Go makes it at the code's exit there, and the
			// code is compiled again for it (jitDefineSeen).
			fb.definesUnfed = append(fb.definesUnfed, int32(pc))
		}
		return nil, false
	}
	next := s.next
	if next.index == nil && int(next.n) > linearScanLimit {
		return nil, false
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(s), remember(next))
	return &ssa.PropertyAdd{From: uintptr(unsafe.Pointer(s)), Next: uintptr(unsafe.Pointer(next)), Flags: uint8(next.flags), Define: true, Key: uint32(key)}, true
}

// jitFedInlined is a property site of a callee e's code inlines, cl's
// at pc, whose cache knew nothing when the code was compiled: the code
// leaves there, as V8's leaves for feedback it lacked, and Go, running
// the call, fills the cache (jitInlineFed).
type jitFedInlined struct {
	cl *closure
	pc uint32
}

// jitInlineFed reports whether a site of a callee e's code inlines, whose
// cache knew nothing when the code was compiled, has learned a shape
// since, and has the code compiled again for it, as V8 optimizes again
// once it has the feedback it lacked: an exit there is not the callee's
// leaving native code, which would stop it being inlined (jitInlineLeft).
func (r *Runtime) jitInlineFed(e *jitEntry) bool {
	if e.inlineReopts >= 2*jitInlineReoptimizations {
		return false
	}
	for i, s := range e.fedInlined {
		in := s.cl.fn.Code[s.pc]
		if int(in.B) >= len(s.cl.ic) {
			continue
		}
		if c := &s.cl.ic[in.B]; c.shape != nil && c.shape != noShape {
			e.fedInlined = slices.Delete(e.fedInlined, i, i+1)
			e.inlineReopt = true
			return true
		}
	}
	return false
}

// jitReoptimize compiles cl's function again for e, its entry, with what
// failed in its code generic, and replaces its code, which runs nowhere:
// native code leaves for Go to do anything else. If the function no longer
// compiles, the code it has is kept.
// reoptPending reports whether e's code is to be compiled again, for any
// of the reasons jitReoptimize takes.
func (e *jitEntry) reoptPending() bool {
	return e.reopt || e.inlineReopt || e.polyReopt || e.upgradeReopt || e.unwindReopt
}

// reoptDue reports whether e's code is to be compiled again now: for a
// speculation that failed, or one that leaves too often, at once; for
// what it has learned to make natively -- calls to inline or call, shapes
// met, a callee that inlines more -- once the code has run
// jitReoptBudget stretches since it was compiled, as V8 optimizes again
// once a function has spent its budget again: what is learned meanwhile
// is compiled for together, not each in a compile of its own. RayTrace
// with construction on compiled its functions some 100 times in three
// runs, a quarter of its time.
func (e *jitEntry) reoptDue() bool {
	// Nothing pending, the usual answer, asked in line: a call of Go asks
	// at every call.
	if e.reopt || e.inlineReopt || e.polyReopt || e.upgradeReopt || e.unwindReopt || e.feedReopt {
		return e.reoptDueNow()
	}
	return false
}

// reoptDueNow is reoptDue's answer for code with something pending.
func (e *jitEntry) reoptDueNow() bool {
	if e.reopt || e.unwindReopt {
		return true
	}
	if e.feedReopt && (e.starts+e.link.in > e.feedStarts || e.ssaStats.work-e.feedWork >= uint64(e.reoptBudget)*jitReoptPer*jitReoptWork) {
		return true
	}
	return e.reoptPending() && (e.ssaStats.entries+e.ssaStats.hosts+e.ssaStats.goCalls+e.link.in+uint64(e.slowTries) >= uint64(e.reoptBudget) ||
		e.ssaStats.work >= uint64(e.reoptBudget)*jitReoptPer*jitReoptWork)
}

// jitReoptBudget is how many native stretches -- entries from Go, exits to
// it, native calls in -- code runs before it is compiled again for what it
// has learned (reoptDue), for each jitReoptPer instructions of its
// function, as V8's budget grows with a function's bytecode: a small
// function learns its callees' calls soon, a large one, which costs more
// to compile, waits for more.
// Or the work the code has done, in instructions (jitSSAProfit), reaches
// jitReoptWork times its function's: a loop that runs long, entered once.
const (
	jitReoptBudget = 8
	jitReoptPer    = 4
	jitReoptWork   = 16
)

func (r *Runtime) jitReoptimize(cl *closure, e *jitEntry) {
	switch {
	case e.reopt || e.feedReopt:
		e.reopts++
	case e.inlineReopt:
		e.inlineReopts++
	case e.upgradeReopt:
		e.upgradeReopts++
	}
	e.reopt, e.inlineReopt, e.polyReopt, e.upgradeReopt, e.unwindReopt, e.feedReopt = false, false, false, false, false, false
	// The misses that count toward every entry are the new code's.
	e.entryMisses = 0
	// What it compiles for, the reads' shapes met since included.
	e.polyPending, e.polySettled = false, 0
	fn := cl.fn
	r.jitInlineNativeCalls(e)
	kind := jitLowerSSA
	if e.ssaCallee {
		kind = jitLowerInline
	}
	p, err := r.jitLower(fn, kind)
	if err != nil {
		return
	}
	// Not dropped to make room for the compiles this makes, its callees',
	// nor for its own code.
	e.busy++
	defer func() { e.busy-- }()
	// The calls whose targets the existing tiers have met since.
	r.jitSeedCalls(cl, e, p)
	code, fb, _ := r.compileSSA(fn, cl, p, e)
	if code == nil {
		return
	}
	defer r.jitWantedCallees(e, fb)
	old := e.ssa
	// What the old code holds, which a call of Go suspended in it needs.
	oldKeep := []any{e.ssaShapes, e.ssaHolders, e.ssaInlined, e.ssaPools, e.ssaLinks, e.ssaCells}
	if len(fb.inlined) > len(e.ssaInlined) {
		// It inlines more: its native callers may inline it now, and
		// theirs, with it.
		r.jitCallersReopt(e, jitInlineDepth)
	}
	e.ssa, e.ssaShapes, e.ssaHolders, e.fed, e.fedInlined, e.ssaStats = code, fb.shapes, fb.holders, fb.fed, fb.fedInlined, jitSSAStats{}
	e.ssaInlinedAt, e.definesUnfed, e.ssaCells = fb.inlinedAt, fb.definesUnfed, fb.cells
	e.ssaStrings, e.ssaInlined, e.ssaPools = e.ssaStrings || fb.strings, fb.inlined, fb.pools
	r.jit.setLinks(e, fb.links)
	e.ssaKeeps = max(e.ssaKeeps, fb.keeps)
	// What it leaves on is counted afresh: it may have left on what it was
	// compiled for now -- a construction, a call it inlines now. So Go
	// enters it again, and native callers call it, until the new code is
	// found to leave too often in turn, as V8 judges code it optimized
	// again by what it does then; its backoff (nativeBackoff) stays. Code
	// compiled for native callers alone (ssaCallee) Go does not enter.
	e.link.in, e.nativeOut, e.slowTries = 0, 0, 0
	e.notNative, e.nativeRetry = false, 0
	if !e.ssaCallee {
		e.entrySlow = false
	}
	e.link.entry = code.EntryAddress(0)
	r.jit.closeSSA(old, oldKeep)
	r.jit.reoptimized++
	// Compiled for what it does now: young again.
	e.used = r.jit.epoch()
	r.jit.account(e)
	r.jit.settle()
}

// compileSSA compiles a lowered function with the new pipeline, or returns
// nil. It takes functions whose slots are the frame's locals, its captured
// bindings, read through their cells, the receiver, which Go puts in the
// context, and its operands: no global slots yet.
//
// The closure's caches say where its sites' properties are (jitFeedback),
// which it returns: the shapes the code compares objects with, and the
// prototypes it compares receivers' with, to be kept alive, and the sites
// whose caches were empty.
func (r *Runtime) compileSSA(fn *bytecode.Function, cl *closure, p *ir.Program, e *jitEntry) (*jit.SSACode, *jitFeedback, error) {
	this := 0
	if p.This {
		this = 1
	}
	if len(p.Globals) != 0 || p.Locals != fn.LocalCount+len(fn.Upvalues)+this {
		return nil, nil, jitRefused
	}
	if e != nil && cl != nil {
		r.jitSeenFeedback(cl, e)
	}
	code, fb, err := r.compileSSAKeeping(fn, cl, p, e, 0)
	if errors.Is(err, mir.ErrKeeps) {
		// More keep cells live at once than the context has, which the
		// build could not know: built again with no more than that, the
		// calls past them left to Go.
		code, fb, err = r.compileSSAKeeping(fn, cl, p, e, abi.MaxKeeps)
	}
	return code, fb, err
}

// compileSSAKeeping is compileSSA's build and compile, the build's keep
// cells limited to keepLimit (ssa.KeepFeedback), or not, if 0. The code is
// placed only if the JIT has the room for all its entry is to hold then
// (roomFor), its code and what describes it included: jit.ErrCodeBudget
// otherwise.
func (r *Runtime) compileSSAKeeping(fn *bytecode.Function, cl *closure, p *ir.Program, e *jitEntry, keepLimit int) (*jit.SSACode, *jitFeedback, error) {
	fb := &jitFeedback{r: r, fn: fn, cl: cl, e: e, keepLimit: keepLimit, prog: p}
	// The compile's memory comes from the runtime's workspaces, taken back
	// once its code is placed.
	s := r.jit
	s.compiling = true
	defer func() { s.compiling = false }()
	defer s.ssaWork.Rewind()
	defer s.mirWork.Rewind()
	f, err := ssa.BuildIn(&s.ssaWork, p, fb)
	if err != nil {
		return nil, nil, jitRefused
	}
	f.FrameLocals = fn.LocalCount
	if p.This {
		f.ThisSlot = fn.LocalCount + len(fn.Upvalues)
	}
	ssa.Optimize(f)
	mc, err := mir.CompileIn(&s.mirWork, f, jitEncoding)
	if err != nil && strings.Contains(err.Error(), "runtime error") {
		// A refusal the backend's own panic made: a bug, which the tests
		// look for (backendPanics).
		s.backendPanics++
	}
	if errors.Is(err, mir.ErrKeeps) && keepLimit == 0 {
		return nil, nil, err
	}
	if err != nil {
		return nil, nil, jitRefused
	}
	// The cells the backend gave the kept values (mir's keep cells).
	fb.keeps = f.Keeps
	s.ssaKeeps = max(s.ssaKeeps, f.Keeps)
	need := jitEntryFixed + jit.CodeFootprint(mc) + fb.compiledBytes()
	if e != nil {
		need += e.learnedBytes()
	}
	if e == nil || e.hint == 0 {
		// The hint table's slot, which remember takes once it is placed.
		need += s.hintGrowth()
	}
	// The lists of native callers its callees keep, which it joins once
	// placed (setLinks).
	need += s.callersGrowth(e, fb.links)
	if !s.roomFor(e, fb.links, need) {
		return nil, nil, jit.ErrCodeBudget
	}
	code, err := jit.NewSSACode(r.jit.arena, mc)
	if err != nil {
		return nil, nil, err
	}
	// What the entry holds, and its function's program.
	s.learn(len(fn.Code), need+p.Footprint())
	return code, fb, nil
}

// jitRefused is compileSSA's refusal of a program it does not compile:
// for good, unlike jit.ErrCodeBudget's, which the cache's releasing code
// may lift, or jit.ErrUnavailable's, which no compile will.
var jitRefused = errors.New("jit: the program is refused")

// jitFeedback is ssa.Feedback from a closure: its names' atoms, and its
// property caches. A site that has met objects of one shape, the property
// their own plain data -- and writable, for a write -- knows where it is in
// any object of that shape: the shape settles the table's layout and the
// attributes. A shape a cache remembers is marked seen, so the VM replaces
// it rather than change it when an object's layout changes; the code holds
// it by address, and the entry keeps it alive. Objects of other shapes, and
// of none -- the VM gives a small object a shape only when a cache asks --
// are searched for the key.
type jitFeedback struct {
	r      *Runtime
	fn     *bytecode.Function
	cl     *closure
	shapes []*shape
	// holders are the prototypes the code compares receivers' with
	// (ssa.Holder), held by address, which the entry keeps alive.
	holders []*Object
	// fed are the property sites, each with the shape its cache knew, or
	// none (jitFed); fedInlined, the root's, the inlined callees' sites
	// whose caches knew nothing.
	fed        []jitFedSite
	fedInlined []jitFedInlined
	// definesUnfed, the root's, are the literals' fields it compiled before
	// their transitions were made; cells the string constants' it reads.
	definesUnfed []int32
	cells        []*Value
	// inlinedAt, the root's, are the calls of its own it inlines.
	inlinedAt []int32
	// root is the function's feedback for an inlined callee's, which keeps
	// what the callee's sites hold for the code; nil for the function's
	// own. strings marks an inlined callee that calls charCodeAt. links
	// are those of the functions the code calls natively (jitLink).
	root    *jitFeedback
	strings bool
	links   []*jitLink
	// inlined are the closures of the callees the code inlines; pools the
	// object pools its constructions take from.
	inlined []*closure
	pools   []*abi.ObjectPool
	// wanted are callees of calls to inline that the compile made calls
	// instead -- past its budget (ssa's maxInlines) -- which had no code
	// for native callers: compiled after it (jitWantedCallees).
	wanted []*closure
	// keeps is how many keep cells the code uses (ssa.Func.Keeps), and
	// keepLimit how many the build may number (ssa.KeepFeedback), or 0.
	keeps, keepLimit int
	// e is the entry being compiled again, whose failed speculations
	// (jitEntry.failed) are built generic; nil for a first compile.
	e *jitEntry
	// fwd, for a forwarding constructor's inlined frame, is the
	// construction it is inlined at, whose receiver the frame's sites are
	// known from (jitForwardFrame).
	fwd *jitForwardFrame
	// prog is the function lowered for inlining, its stack depths what
	// Intrinsic reads, made on first use.
	prog *ir.Program
}

// Intrinsic is ssa.IntrinsicFeedback's: a method call of one argument at
// pc whose function is the realm's Math.sqrt or Math.abs, as its reads
// give it now (jitChainValue) -- Math.sqrt(x) -- which the code makes
// itself, as V8 reduces such a call; the call checks it calls it.
func (fb *jitFeedback) Intrinsic(pc int) (ssa.Intrinsic, bool) {
	fn := fb.fn
	if fb.cl == nil || pc < 2 || pc >= len(fn.Code) || jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod || fn.Code[pc].A < 1 || fn.Code[pc].A > jitMathArgs {
		return ssa.Intrinsic{}, false
	}
	if fb.prog == nil {
		p, err := fb.r.jitLower(fn, jitLowerInline)
		if err != nil {
			return ssa.Intrinsic{}, false
		}
		fb.prog = p
	}
	p := fb.prog
	if len(p.Maps) != len(fn.Code) {
		return ssa.Intrinsic{}, false
	}
	// The function, read by the get_prop_this at its slot's depth, with
	// nothing after it but the arguments' instructions, above it.
	slot, q := p.Maps[pc].Depth-int(fn.Code[pc].A)-1, -1
	for k := pc - 1; k >= 0; k-- {
		d := p.Maps[k].Depth
		if d == slot && fn.Code[k].Op == bytecode.OpGetPropThis {
			q = k
			break
		}
		if d < slot+1 {
			return ssa.Intrinsic{}, false
		}
	}
	if q < 0 {
		return ssa.Intrinsic{}, false
	}
	v, ok := fb.r.jitChainValue(fb.cl, p, q, jitChainReads)
	if !ok || !v.IsObject() {
		return ssa.Intrinsic{}, false
	}
	o := v.Object()
	k := ssa.Intrinsic{Callee: uintptr(unsafe.Pointer(o))}
	// The realm's Math functions of numbers, each made natively, as V8's
	// code makes them: Math.pow where exact, and by Go called from there
	// otherwise; Math.max and Math.min of any number of them; a rounding,
	// the sign, fround, sqrt and abs by instructions of their own; every
	// other by Go called from there, the VM's very function.
	fd, n := o.fn(), int(fn.Code[pc].A)
	switch {
	case o == fb.r.powFn && n == 2:
		k.Op = ssa.OpPowF64
	case o == fb.r.atan2Fn && n == 2:
		k.Op, k.Go = ssa.OpMathCall, ssa.GoAtan2
	case o == fb.r.hypotFn && n == 2:
		k.Op, k.Go = ssa.OpMathCall, ssa.GoHypot
	case fd == nil || fd.native == nil:
		return ssa.Intrinsic{}, false
	case fd.mathOp == mathMax:
		k.Op, k.Args = ssa.OpMaxF64, n
	case fd.mathOp == mathMin:
		k.Op, k.Args = ssa.OpMinF64, n
	case fd.mathOp != 0 && int(fd.mathOp) <= len(unaryMathNames) && n == 1:
		k.Op = jitUnaryMath(unaryMathNames[fd.mathOp-1])
		if k.Op == ssa.OpMathCall {
			k.Go, k.Fn = ssa.GoMath, int(fd.mathOp)
		}
	default:
		return ssa.Intrinsic{}, false
	}
	keep := fb.keep()
	keep.holders = append(keep.holders, o)
	return k, true
}

// jitMathArgs is the most arguments a call of Math.max or Math.min is made
// natively with, as ssa takes them.
const jitMathArgs = 8

// jitUnaryMath is the op native code makes a Math function of one number
// with, named name: an instruction of its own, or a call of Go
// (ssa.OpMathCall), which roundings are without SSE4.1 (abi.Encoding's
// Round).
func jitUnaryMath(name string) ssa.Op {
	switch name {
	case "sqrt":
		return ssa.OpSqrtF64
	case "abs":
		return ssa.OpAbsF64
	case "sign":
		return ssa.OpSignF64
	case "fround":
		return ssa.OpFroundF64
	}
	if jitEncoding.Round {
		switch name {
		case "floor":
			return ssa.OpFloorF64
		case "ceil":
			return ssa.OpCeilF64
		case "trunc":
			return ssa.OpTruncF64
		case "round":
			return ssa.OpRoundF64
		}
	}
	return ssa.OpMathCall
}

// InstanceOf is ssa.InstanceOfFeedback's: the constructor of the instanceof
// at pc, as the read just before it gives it now (jitChainValue), a
// function the script made, not bound, if Symbol.hasInstance is the realm's
// on it, found on Function.prototype, and its prototype property its own
// data property, an object: the sites of both reads, as caches filled for
// it have them.
func (fb *jitFeedback) InstanceOf(pc int) (ssa.InstanceOfSite, bool) {
	fn, r := fb.fn, fb.r
	if fb.cl == nil || pc < 1 || pc >= len(fn.Code) || fn.Code[pc].Op != bytecode.OpInstanceOf || r.hasInstanceFn == nil {
		return ssa.InstanceOfSite{}, false
	}
	if fb.prog == nil {
		p, err := fb.r.jitLower(fn, jitLowerInline)
		if err != nil {
			return ssa.InstanceOfSite{}, false
		}
		fb.prog = p
	}
	p := fb.prog
	if len(p.Maps) != len(fn.Code) || p.Maps[pc-1].Depth < 0 || p.Maps[pc].Depth != p.Maps[pc-1].Depth+1 && fn.Code[pc-1].Op == bytecode.OpGetGlobal {
		return ssa.InstanceOfSite{}, false
	}
	v, ok := r.jitChainValue(fb.cl, p, pc-1, jitChainReads)
	if !ok || !v.IsObject() {
		return ssa.InstanceOfSite{}, false
	}
	ctor := v.Object()
	fd := ctor.fn()
	if fd == nil || fd.bound || proxyOf(ctor) != nil {
		// A built-in is one too: sc_Vector is Array.
		return ssa.InstanceOfSite{}, false
	}
	// Its prototype property, made now if it is not yet, is in its table
	// from then on: the site is the constructor's shape and the index there,
	// which no cache keeps for a function (synthesized) -- the operator
	// checks it has this very function.
	r.materializeFunctionProto(ctor)
	var hc propCache
	r.fillPropCache(&hc, ctor, r.hasInstanceAtom, false)
	has, _ := fb.siteOf(r.hasInstanceAtom, false, &hc)
	i := ctor.findOwn(atomPrototype)
	if has.Shape == 0 || has.Holders[0].Object == 0 || i < 0 || ctor.shape == nil || uintptr(unsafe.Pointer(ctor.shape)) != has.Shape ||
		ctor.props[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return ssa.InstanceOfSite{}, false
	}
	proto := ssa.PropertySite{Key: uint32(atomPrototype), Shape: has.Shape, Index: int32(i)}
	h := hc.p1
	if hc.p2 != nil {
		h = hc.p2
	}
	if hv := h.props[hc.idx].value; !hv.IsObject() || hv.Object() != r.hasInstanceFn {
		return ssa.InstanceOfSite{}, false
	}
	k := fb.keep()
	k.holders = append(k.holders, ctor, r.hasInstanceFn)
	return ssa.InstanceOfSite{Ctor: uintptr(unsafe.Pointer(ctor)), HasInstance: has,
		HasInstanceFn: uintptr(unsafe.Pointer(r.hasInstanceFn)), Prototype: proto}, true
}

// jitForwardFrame is the construction a forwarding constructor
// (bytecode.LeafForward) is inlined at: the constructor's function object,
// the pool its receivers come from, and the construction's argument count.
// What its code reads and calls is known from the receiver, as V8 knows a
// property of an object of a known map: this.k, found on the prototype; k's
// apply, the realm's; and the call, of k, inlined with the construction's
// arguments.
type jitForwardFrame struct {
	ctor *Object
	pool *abi.ObjectPool
	argc int
}

// keep is the feedback that keeps what the code holds by address: the
// function's own, for an inlined callee's too.
func (fb *jitFeedback) keep() *jitFeedback {
	if fb.root != nil {
		return fb.root
	}
	return fb
}

// Inline is ssa.Feedback's: a call jitCallSeen has seen call one function
// it may inline, unless that stopped. A callee inlines nothing itself.
func (fb *jitFeedback) Inline(pc int) (ssa.InlineSite, bool) {
	if pc >= len(fb.fn.Code) {
		return ssa.InlineSite{}, false
	}
	var targets []jitInline
	if fb.root != nil {
		// An inlined callee's calls, inlined in it as its own code calls
		// them (jitInlineSite).
		if in, ok := fb.r.jitInlineSite(fb.fn, pc, jitInlineDepth-1); ok {
			targets = []jitInline{in}
		}
	} else if fb.e != nil && !slices.Contains(fb.e.notInline, int32(pc)) {
		targets = fb.e.inlines
	}
	if fb.fwd != nil {
		return fb.forwardInline(pc)
	}
	k := fb.keep()
	for _, in := range targets {
		if int(in.pc) != pc {
			continue
		}
		kind := jitLowerInline
		if in.cl.fn.Leaf == bytecode.LeafForward && fb.fn.Code[pc].Op == bytecode.OpNew {
			kind = jitLowerForward
		}
		p, err := fb.r.jitLower(in.cl.fn, kind)
		if err != nil {
			return ssa.InlineSite{}, false
		}
		call := jitCallInstr(fb.fn.Code[pc])
		this := -1
		if p.This {
			this = in.cl.fn.LocalCount + len(in.cl.fn.Upvalues)
		}
		for _, x := range p.Code {
			k.strings = k.strings || x.Op == ir.StringMethod || x.Op == ir.StringCode
		}
		callee := &jitFeedback{r: fb.r, fn: in.cl.fn, cl: in.cl, root: k}
		site := ssa.InlineSite{
			Program: p, Feedback: callee,
			Callee: uintptr(unsafe.Pointer(in.obj)), Closure: uintptr(unsafe.Pointer(in.cl)),
			Argc: int(call.A), Method: call.Op == bytecode.OpCallMethod,
			Params: in.cl.fn.ParamCount, ThisSlot: this, Coerce: in.cl.fn.CoerceThis,
		}
		if call.Op == bytecode.OpNew {
			// Its receiver from its pool, as a native construction's.
			i := in.obj.findOwn(atomPrototype)
			if in.pool == nil || i < 0 {
				return ssa.InlineSite{}, false
			}
			site.Construct, site.Coerce = true, false
			site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(in.pool)), int(i), uint32(atomPrototype)
			k.pools = append(k.pools, in.pool)
			if in.cl.fn.Leaf == bytecode.LeafForward {
				// Its frame Go cannot make: an exit in it makes the
				// construction over again.
				site.Restart = true
				callee.fwd = &jitForwardFrame{ctor: in.obj, pool: in.pool, argc: int(call.A)}
			}
		}
		k.holders = append(k.holders, in.obj)
		k.inlined = append(k.inlined, in.cl)
		if fb.root == nil {
			k.inlinedAt = append(k.inlinedAt, int32(pc))
		}
		return site, true
	}
	return ssa.InlineSite{}, false
}

// NativeCalls is ssa.Feedback's: the functions jitCallSeen has seen a call
// call whose native code a caller's may call -- compiled, and not reading
// what a frame made natively does not have: captured bindings,
// charCodeAt's intrinsic. The callees' entries are kept by the code.
func (fb *jitFeedback) NativeCalls(pc int) []ssa.CallSite {
	if fb.e == nil || fb.root != nil || pc >= len(fb.fn.Code) {
		return nil
	}
	var sites []ssa.CallSite
	targets := jitTargets(fb.e)
	for _, x := range targets {
		// The calls it makes natively, and those it inlines, which a
		// compile past its budget for inlining (ssa's maxInlines) calls
		// instead, as V8's does: they had been made by Go.
		in := *x
		if int(in.pc) != pc || slices.ContainsFunc(sites, func(s ssa.CallSite) bool { return s.Callee == uintptr(unsafe.Pointer(in.obj)) }) {
			continue
		}
		if in.cl != nil && slices.ContainsFunc(sites, func(s ssa.CallSite) bool {
			return s.ByCode == uintptr(unsafe.Pointer(in.cl.fn)) && s.Env == uintptr(unsafe.Pointer(in.cl.env)) && s.Realm == uintptr(unsafe.Pointer(in.cl.realm))
		}) {
			// Another object of a function the call checks by its code.
			continue
		}
		if in.cl == nil && in.pop {
			// Array.prototype.pop, whose fast path the call's checks keep
			// to: the array's own last element, of the realm's prototype.
			fb.holders = append(fb.holders, in.obj)
			sites = append(sites, ssa.CallSite{Callee: uintptr(unsafe.Pointer(in.obj)), ThisSlot: -1, Method: true, Pop: true,
				Protos: [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(fb.r.proto.array))}}})
			continue
		}
		if in.cl == nil && in.push {
			// Array.prototype.push, whose fast path the call's checks
			// keep to: the prototypes as they are now.
			ap, op := fb.r.proto.array, fb.r.proto.object
			if ap.shape == nil || op.shape == nil || ap.proto != op || op.proto != nil {
				continue
			}
			k := fb.keep()
			k.shapes = append(k.shapes, remember(ap.shape), remember(op.shape))
			k.holders = append(k.holders, in.obj, ap, op)
			sites = append(sites, ssa.CallSite{Callee: uintptr(unsafe.Pointer(in.obj)), ThisSlot: -1, Argc: 1, Method: true, Push: true,
				Protos: [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(ap)), Shape: uintptr(unsafe.Pointer(ap.shape))},
					{Object: uintptr(unsafe.Pointer(op)), Shape: uintptr(unsafe.Pointer(op.shape))}}})
			continue
		}
		if in.cl == nil {
			// A built-in's construction with nothing to run.
			fb.holders, fb.pools = append(fb.holders, in.obj), append(fb.pools, in.pool)
			sites = append(sites, ssa.CallSite{Callee: uintptr(unsafe.Pointer(in.obj)), ThisSlot: -1,
				Pool: uintptr(unsafe.Pointer(in.pool)), Alloc: true})
			continue
		}
		// Only looked up: a compile here would share the workspaces of the
		// one asking. jitCallSeen compiled it.
		ce := fb.r.jit.cache[weak.Make(in.cl.fn)]
		if ce == nil || ce.ssa == nil {
			if (ce == nil || ce.ssa == nil) && !slices.Contains(fb.wanted, in.cl) {
				k := fb.keep()
				k.wanted = append(k.wanted, in.cl)
			}
			continue
		}
		call, fn := jitCallInstr(fb.fn.Code[pc]), in.cl.fn
		// The call is made through the function's link (jitLink), to
		// whatever code it has then: what it writes for the callee is the
		// function's -- its receiver if its body reads one -- not this
		// code's, which a compile of it later may not be.
		this := -1
		if fn.UsesThis {
			this = fn.LocalCount + len(fn.Upvalues)
		}
		fb.links = append(fb.links, ce.link)
		fb.holders = append(fb.holders, in.obj)
		site := ssa.CallSite{
			Callee: uintptr(unsafe.Pointer(in.obj)), Closure: uintptr(unsafe.Pointer(in.cl)),
			Entry: uintptr(unsafe.Pointer(&ce.link.entry)), Count: uintptr(unsafe.Pointer(&ce.link.in)),
			Argc: int(call.A), Method: call.Op == bytecode.OpCallMethod,
			Params: fn.ParamCount, LocalCount: fn.LocalCount, MaxStack: fn.MaxStack, ThisSlot: this, Coerce: fn.CoerceThis,
		}
		if fn.HasTailCall {
			// Whether its calls in tail position may give its frame up, as
			// the interpreter gives up a frame that is no construction's,
			// nor an arrow's that shares a derived constructor's this.
			fd := in.obj.fn()
			site.Tail = true
			site.TailFrame = call.Op != bytecode.OpNew && (fd == nil || !fd.arrow || fd.extra == nil || fd.extra.lexThisRef == nil)
		}
		if len(in.cl.upvalues) != 0 {
			// Its captured bindings, which its code reads through its
			// context; the call checks it calls this closure's function
			// object, which keeps them.
			site.Upvalues = uintptr(unsafe.Pointer(unsafe.SliceData(in.cl.upvalues)))
		}
		if in.via {
			site.Via = uintptr(unsafe.Pointer(fb.r.callFn))
			fb.holders = append(fb.holders, fb.r.callFn)
		}
		if call.Op == bytecode.OpNew {
			i := in.obj.findOwn(atomPrototype)
			if in.pool == nil || i < 0 {
				continue
			}
			site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(in.pool)), int(i), uint32(atomPrototype)
			site.Coerce = false
			fb.pools = append(fb.pools, in.pool)
		}
		byCode := len(in.cl.upvalues) != 0 || in.byCode
		for _, y := range targets {
			if byCode {
				break
			}
			byCode = int(y.pc) == pc && y.cl != nil && y.obj != in.obj && jitSameCode(y.cl, in.cl)
		}
		if site.Pool == 0 && !in.via && byCode {
			// A closure: checked by its function, its closure the callee's
			// at run time (ByCode), as the one an enclosing function makes
			// at each of its calls is a new object each time.
			site.ByCode, site.Env, site.Realm = uintptr(unsafe.Pointer(in.cl.fn)), uintptr(unsafe.Pointer(in.cl.env)), uintptr(unsafe.Pointer(in.cl.realm))
		}
		sites = append(sites, site)
	}
	if len(sites) == 0 {
		// None it calls natively: Go makes it, called from native code
		// (jitGoCall), as V8's code calls a function it has no code for.
		call := jitCallInstr(fb.fn.Code[pc])
		n := int(call.A) + 1
		if call.Op == bytecode.OpCallMethod {
			n++
		}
		if (call.Op == bytecode.OpCall || call.Op == bytecode.OpCallMethod || call.Op == bytecode.OpNew) && n <= abi.MaxGoArgs {
			sites = append(sites, ssa.CallSite{Go: ssa.GoCall, Argc: int(call.A), Method: call.Op == bytecode.OpCallMethod, New: call.Op == bytecode.OpNew, ThisSlot: -1})
		}
	}
	return sites
}

// jitInline is a call jitCallSeen has seen call a function it may inline:
// the call's PC, the function, and how often native code has left the
// inlined callee for Go there.
type jitInline struct {
	pc    int32
	cl    *closure
	obj   *Object
	exits uint32
	// pool, for a construction made natively, is where its objects come
	// from (abi.ObjectPool); via marks a call of Function.prototype.call
	// that calls cl's function (jitCalledVia).
	pool *abi.ObjectPool
	via  bool
	// push marks a call of Array.prototype.push (jitPushes), pop one of
	// Array.prototype.pop (jitPops).
	push, pop bool
	// byCode marks a function the call met two objects of, closures its
	// maker makes anew at each call: called by its code (jitSameCode).
	byCode bool
	// fwd, for an inlined forwarding constructor's construction
	// (bytecode.LeafForward), is the method it forwards to, as the code was
	// last compiled for (jitForwardMethod).
	fwd *Object
}

// jitCallsToInline is how often a call leaves native code before it is
// looked at for inlining: a call seldom made is not worth compiling for.
// jitInlineReoptimizations is how many times a function's code is
// compiled again for its calls, apart from jitReoptimizations: a method
// whose calls are on different branches, as Richards' tasks' are, learns
// them at different times.
// jitCallsHot is how often a call leaves native code, its function past
// those compiles, before it is looked at all the same: up to twice as many.
const (
	jitCallsToInline         = 4
	jitInlineReoptimizations = 4
	jitCallsHot              = 128
)

// What jitCallSeen knows of a call (jitEntry.callSites), past its count: that
// it is made natively, that it is neither that nor inlined, or that it is
// inlined.
const (
	jitCallNative  = 253
	jitCallDone    = 254
	jitCallInlined = 255
)

// jitCallTargets is how many functions a call may call natively: one it
// calls that is not among them leaves for Go, which makes the call.
const jitCallTargets = 4

// jitInlineExits is how often native code may leave an inlined callee for
// Go before the call stops being inlined: each such exit has Go make the
// call over again.
const jitInlineExits = 16

// jitCallSeen notes a call native code left to Go at pc, with the frame's
// operands up to sp: from inside the callee, where it is inlined, which
// after jitInlineExits stops it being inlined; otherwise, the first time,
// whether it calls a function that may be inlined (jitInlinable). The code
// is compiled again for the calls found to inline when a call seen before
// leaves native code again -- once the calls around them, a loop's, have
// been seen too -- or at its next entry. A call seen before costs a look
// at the lists.
func (r *Runtime) jitCallSeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	// A construction that left for its pool to be filled again: an inlined
	// one's exit is not its constructor's.
	refilled := in.Op == bytecode.OpNew && r.jitRefillPools(f, e, pc, sp, in)
	if e.callSites != nil && e.callSites[pc] == jitCallInlined {
		if o, cl := r.jitCalled(f, sp, in); cl != nil && !slices.ContainsFunc(e.inlines, func(x jitInline) bool { return int(x.pc) == pc && x.obj == o }) {
			r.jitInlinedCalls(e, pc, o, cl, in)
			return
		}
	}
	if e.callSites != nil && e.callSites[pc] == jitCallNative {
		r.jitCallTarget(f, e, pc, sp, in)
		if e.inlinePending && e.inlineReopts < 2*jitInlineReoptimizations {
			e.inlinePending, e.inlineReopt = false, true
		}
		return
	}
	if e.inlineReopts >= 2*jitInlineReoptimizations {
		return
	}
	// Past its compiles for its calls, a call that keeps leaving earns one
	// more, which every call left to Go costs less than.
	settle := uint8(jitCallsToInline)
	if e.inlineReopts >= jitInlineReoptimizations {
		settle = jitCallsHot
	}
	if e.callSites == nil {
		e.callSites = make([]uint8, len(f.cl.fn.Code))
	}
	switch n := e.callSites[pc]; {
	case n == jitCallInlined:
		if in.Op == bytecode.OpNew && e.inlineReopts < 2*jitInlineReoptimizations {
			if o, cl := r.jitCalled(f, sp, in); cl != nil && r.jitForwardChanged(e, pc, o) {
				e.inlineReopt = true
				return
			}
		}
		if r.jitInlineFed(e) {
			return
		}
		if !refilled && !e.reoptPending() && slices.Contains(e.ssaInlinedAt, int32(pc)) {
			// Not the code compiled before it was decided, which leaves
			// there until it is compiled again; nor code to be compiled
			// again for what it has learned (reoptDue), which may be what
			// it leaves for.
			r.jitInlineLeft(e, pc)
		}
		fallthrough
	case n == jitCallDone:
		if e.inlinePending {
			e.inlinePending, e.inlineReopt = false, true
		}
		return
	case n+1 < settle:
		e.callSites[pc]++
		if e.inlinePending && n > 0 {
			e.inlinePending, e.inlineReopt = false, true
		}
		return
	}
	e.callSites[pc] = jitCallDone
	o, cl := r.jitCalled(f, sp, in)
	if cl == nil {
		if o, cl := r.jitCalledVia(f, sp, in); cl != nil {
			// G.call(this, ...): G called natively, never inlined.
			if ce := r.jitNativeCallee(cl); ce != nil {
				e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), cl: cl, obj: o, via: true})
				e.callSites[pc] = jitCallNative
				e.inlinePending = true
			}
			return
		}
		if o := r.jitPops(sp, in); o != nil {
			// a.pop(), made natively where its fast path makes it.
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, pop: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitPushes(sp, in); o != nil {
			// a.push(v), made natively where its fast path makes it.
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, push: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitAllocates(sp, in); o != nil {
			// A built-in's construction with nothing to run: made from a
			// pool.
			pool := r.jitNewPool(o)
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, pool: pool})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
		}
		return
	}
	if cl.fn != f.cl.fn && r.jitInlinesAt(cl, o, in) {
		e.inlines = append(e.inlines, r.jitInlineTarget(int32(pc), cl, o, in))
		e.callSites[pc] = jitCallInlined
		e.inlinePending = true
		return
	}
	// One that is not inlined is called natively, if its code allows it
	// (NativeCalls), the call itself for a recursive one.
	if in.Op == bytecode.OpNew && !r.jitConstructs(o) {
		return
	}
	if r.jitNativeCallee(cl) != nil {
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), cl, o, in))
		e.callSites[pc] = jitCallNative
		e.inlinePending = true
	}
}

// jitNativeCallers is how many callers' entries an entry tells when it
// inlines more (jitEntry.nativeCallers).
const jitNativeCallers = 8

// jitCallersReopt has the code of e's native callers, and theirs, depth
// levels up, compiled again where it would inline a call it makes
// natively now (jitInlineNativeCalls): one entered from Go is, at its next
// entry or exit, up to jitInlineReoptimizations times, apart from those
// its own calls have.
func (r *Runtime) jitCallersReopt(e *jitEntry, depth int) {
	if depth == 0 {
		return
	}
	for _, c := range e.nativeCallers {
		if c.upgradeReopts < jitInlineReoptimizations && r.jitNativeCallsInline(c, nil) {
			c.upgradeReopt = true
		}
		r.jitCallersReopt(c, depth-1)
	}
}

// jitInlineNativeCalls has e's code, compiled again, inline the calls
// it made natively to one function that may now be inlined with the calls
// it makes, which its own code has come to inline (jitInlinesCalls), as V8
// inlines a callee's callees when it optimizes again.
func (r *Runtime) jitInlineNativeCalls(e *jitEntry) {
	r.jitNativeCallsInline(e, func(i int) {
		x := e.nativeCalls[i]
		e.nativeCalls = slices.Delete(e.nativeCalls, i, i+1)
		e.inlines = append(e.inlines, jitInline{pc: x.pc, cl: x.cl, obj: x.obj, pool: x.pool})
		e.callSites[x.pc] = jitCallInlined
	})
}

// jitNativeCallsInline reports whether e's code calls natively, at a call
// that calls it alone, a function that may be inlined with its calls
// now, calling inline with the index of each in e.nativeCalls, which it
// may delete, if it is not nil.
func (r *Runtime) jitNativeCallsInline(e *jitEntry, inline func(int)) bool {
	found := false
	for i := 0; i < len(e.nativeCalls); i++ {
		x := e.nativeCalls[i]
		pc := int(x.pc)
		one := !slices.ContainsFunc(e.nativeCalls, func(y jitInline) bool { return y.pc == x.pc && y.cl != x.cl })
		if !one || x.cl == nil || r.jit.cache[weak.Make(x.cl.fn)] == e || e.callSites == nil || pc >= len(e.callSites) ||
			e.callSites[pc] != jitCallNative || slices.Contains(e.notInline, x.pc) || !r.jitInlinesCalls(x.cl, jitInlineDepth-1) ||
			x.pool != nil && !r.jitInlinesAt(x.cl, x.obj, bytecode.Instr{Op: bytecode.OpNew}) {
			continue
		}
		found = true
		if inline == nil {
			return true
		}
		inline(i)
		i--
	}
	return found
}

func init() { jit.HostCall = jitHostCall }

// jitHostCall is Go's side of a call of Go from native code (jit.HostCall,
// ssa.CallSite's Go) by the runtime the context belongs to: it does what
// the call names, its result to the call's keep cell, and reports whether
// it did. What it refuses, the code leaves for Go to make; it may not run
// the script's code, which is the interpreter's then.
func jitHostCall(ctx *abi.Context) bool {
	r := (*Runtime)(ctx.Host)
	if r == nil || ctx.GoKeep >= abi.MaxKeeps {
		return false
	}
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoCall && jitGoCallWork == nil {
		// The most frequent, asked first.
		return r.jitGoCall(ctx)
	}
	if jitGoCallWork != nil {
		jitGoCallWork()
	}
	switch ssa.GoOp(ctx.GoOp & 0xff) {
	case ssa.GoAdd:
		a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
		// Primitives, and nothing that throws: an object's conversion may
		// call anything, and making an error may too (a getter of
		// Error.stackTraceLimit) -- a symbol's, a BigInt's with a number,
		// a string too long, a memory limit reached. Go makes those after
		// the code leaves.
		if !jitPlainOperand(a) || !jitPlainOperand(b) || r.meter != nil ||
			a.IsString() && b.IsString() && a.String().length+b.String().length > maxStringLength-64 {
			return false
		}
		v, err := r.add(a, b)
		if err != nil {
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		return true
	case ssa.GoConcat:
		// As concat joins its parts: primitives, converted without running
		// anything, and nothing that throws -- the same refusals as GoAdd's.
		n := int(ctx.GoOp>>abi.GoOpArgcShift) & 0xff
		if n < 1 || n > abi.MaxGoArgs || r.meter != nil {
			return false
		}
		var parts [abi.MaxGoArgs]Value
		total := 0
		for i := range n {
			p := jitGoArg(ctx, i)
			if !jitPlainOperand(p) {
				return false
			}
			if !p.IsString() && !p.IsNumber() {
				s, err := r.toString(p)
				if err != nil {
					return false
				}
				p = Str(s)
			}
			if p.IsString() {
				if total += p.String().length; total > maxStringLength-64 {
					return false
				}
			}
			parts[i] = p
		}
		s, err := r.joinParts(parts[:n])
		if err != nil || s.length > maxStringLength-64 {
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Str(s)
		return true
	case ssa.GoRefill:
		// The pool's address, a constant of the code's, which keeps it.
		return r.jitRefillPool(*(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref)))
	case ssa.GoStore:
		// A cell holding a value -- a property's, a global binding's, a
		// captured binding's -- which what native code read keeps alive,
		// and the value, which its source keeps: stored as Go stores,
		// with its write barrier.
		cell := *(**Value)(unsafe.Pointer(&ctx.GoArgs[1].Ref))
		*cell = jitGoArg(ctx, 0)
		return true
	case ssa.GoCall:
		return r.jitGoCall(ctx)
	case ssa.GoClosure, ssa.GoClose:
		// On the frame whose locals are where native code assigns
		// captured ones: the one Go entered the code with, or a native
		// callee's, which its context stands for (jitCalleeFrame).
		f := r.jitRunFrame(ctx)
		callee := f == nil
		if callee {
			if f = r.jitCalleeFrame(ctx); f == nil {
				return false
			}
		}
		i := int(ctx.GoOp >> abi.GoOpIndexShift)
		v := Undefined
		if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
			if i >= len(f.cl.consts) || callee && jitNeedsMaker(f.cl.consts[i]) {
				return false
			}
			v = Obj(r.makeClosure(f, f.cl.consts[i]))
		} else if len(f.openUpvalues) != 0 {
			r.closeUpvaluesFrom(f, i)
		}
		if callee {
			r.jit.calleeOpen[ctx.Level] = f.openUpvalues
		}
		ctx.Open = uint64(len(f.openUpvalues))
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		return true
	case ssa.GoElemCell:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		return o != nil && jitElemAbsent(o, math.Float64frombits(ctx.GoArgs[1].Num))
	case ssa.GoElemWrite:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		k, v := math.Float64frombits(ctx.GoArgs[1].Num), Float(math.Float64frombits(ctx.GoArgs[2].Num))
		return o != nil && jitStoreElem(o, k, v)
	case ssa.GoTake:
		// The pool's address, a constant of the code's, which keeps it;
		// the call's checks found an object in it, made for the
		// prototype they checked. Native code takes it so while the
		// collector does not mark (mir's poolTake).
		pool := *(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		if pool.Count == 0 {
			return false
		}
		pool.Count--
		o := (*Object)(pool.Objects[pool.Count])
		pool.Objects[pool.Count] = nil
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(o)
		return true
	case ssa.GoAddAlong:
		// The object, whose shape, extensibility, prototypes and room the
		// code checked, and the shape its transition leads to, which the
		// code keeps: the entry appended in the room, as native code
		// appends it while the collector does not mark (mir's addAlong).
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		next := *(**shape)(unsafe.Pointer(&ctx.GoArgs[1].Ref))
		if len(o.props) == cap(o.props) {
			return false
		}
		o.props = append(o.props, Property{key: Atom(uint32(ctx.GoArgs[0].Num)), flags: propFlags(ctx.GoArgs[0].Num >> 32), value: jitGoArg(ctx, 2)})
		o.shape = next
		return true
	case ssa.GoPoll:
		// A debugger's interrupt may stop and show the frames, which the
		// exit makes; stress (QJS_JIT_STRESS's budget) wants the exits.
		s := r.jit
		if r.debug != nil || r.jitStress.budget != 0 {
			return false
		}
		old := r.backEdges
		r.backEdges = s.backEdgeBudget()
		s.pollCredit += r.backEdges - old
		s.budgets++
		return r.checkInterruptNow() == nil
	case ssa.GoPow, ssa.GoMath, ssa.GoAtan2, ssa.GoHypot, ssa.GoMod:
		// Two numbers' bits -- or one, and which function of it -- the
		// result's in the first's place.
		x, y := math.Float64frombits(ctx.GoArgs[0].Num), math.Float64frombits(ctx.GoArgs[1].Num)
		var z float64
		switch ssa.GoOp(ctx.GoOp & 0xff) {
		case ssa.GoPow:
			z = jsPow(x, y)
		case ssa.GoMath:
			fn := ctx.GoArgs[1].Num
			if fn == 0 || fn >= uint64(len(unaryMath)) {
				return false
			}
			z = unaryMath[fn](x)
		case ssa.GoAtan2:
			z = fdlibm.Atan2(x, y)
		case ssa.GoHypot:
			z = jsHypot([]float64{x, y})
		case ssa.GoMod:
			z = jsMod(x, y)
		}
		ctx.GoArgs[0].Num = math.Float64bits(z)
		return true
	}
	return false
}

// jitGoRun is what a run of native code in a context runs: its frame,
// entry and code (jitState's goRuns).
type jitGoRun struct {
	f      *frame
	e      *jitEntry
	code   *jit.SSACode
	pinned bool
	// pinnedAt is where its pins start in goPinned: its own code, then
	// that of the native callees whose calls of Go it is suspended in
	// (jitGoCallNested), all released when its native code leaves. serial
	// names those pins: no other run's, nor this run's after they are
	// released, have it (jitState's pinSerial).
	pinnedAt int
	serial   uint64
}

// jitGoUnpin ends what calls of Go the run in context idx made: its code,
// which native code has left, may be closed now, and the code retired
// meanwhile that no other call is suspended in is.
func (r *Runtime) jitGoUnpin(idx int) {
	s := r.jit
	s.goRuns[idx].pinned = false
	clear(s.goPinned[s.goRuns[idx].pinnedAt:])
	s.goPinned = s.goPinned[:s.goRuns[idx].pinnedAt]
	if len(s.retired) != 0 {
		r.jitCloseRetired()
	}
}

// jitGoRunAt notes that context idx runs e's code for f.
func (r *Runtime) jitGoRunAt(idx int, f *frame, e *jitEntry) {
	s := r.jit
	if idx >= len(s.goRuns) {
		s.goRuns = append(s.goRuns, make([]jitGoRun, idx+1-len(s.goRuns))...)
	}
	s.goRuns[idx] = jitGoRun{f: f, e: e, code: e.ssa}
}

// jitGoPending is a call Go made from native code and left native code
// after (jitGoCall): at its exit, Go puts its result, or throws. ctx is the
// level of the code that made it: a run's own, or a native callee's
// (jitGoCallNested), which Go finishes at its exit (jitFinishExit).
type jitGoPending struct {
	set bool
	pc  int
	ctx int
	v   Value
	err error
}

// jitRetired is code compiled again or dropped while a call of Go was
// suspended in it, with what it holds (closeSSA).
type jitRetired struct {
	code *jit.SSACode
	keep any
}

// closeSSA closes code, unless a call of Go is suspended in it, which
// returns there: then code and keep, what it holds -- the shapes it
// compares, the objects and pools it reads -- stay until none is.
func (s *jitState) closeSSA(code *jit.SSACode, keep any) error {
	if code != nil && slices.Contains(s.goPinned, code) {
		s.retired = append(s.retired, jitRetired{code, keep})
		return nil
	}
	return code.Close()
}

// jitGoCall makes, for native code, the call at its exit PC, from the
// operands it passed (ssa.GoCall), as an exit there would have Go make it
// (jitCallAt): the operands past the VM's stack's top, which the call
// leaves above them -- not in the frame's own operand slots, where native
// code may read a value it never loaded -- its result in the call's keep
// cell. Native code goes on, as V8's goes on
// after a call of a function it has no code for, rather than leaving. Only
// at a run's own level: a native call's callee has no frame of the VM's
// for a stack trace to show. A call that threw, or after which the code
// is due to be compiled again, or the runtime stopped, is left for Go to
// finish at its exit (goPending).
func (r *Runtime) jitGoCall(ctx *abi.Context) bool {
	s := r.jit
	idx := int(ctx.Level)
	if ctx.Live == abi.LiveCall {
		return r.jitGoCallNested(ctx, idx)
	}
	if ctx.Live != 0 || idx >= len(s.goRuns) || s.goRuns[idx].f == nil || s.ssaCtxs[idx] != ctx {
		return false
	}
	run := &s.goRuns[idx]
	// The run's, not a pointer into goRuns, which a run the call begins
	// may grow.
	f, e, code := run.f, run.e, run.code
	// The built-in's frame goes where native callees' were made.
	s.levels.serial = 0
	pc, depth := int(ctx.ExitPC), int(ctx.ExitDepth)
	in, n := jitGoCallInstr(ctx.GoOp)
	top := r.stackTop
	if n > abi.MaxGoArgs || depth < n || top+n > len(r.stack) {
		return false
	}
	for i := range n {
		r.stack[top+i] = jitGoArg(ctx, i)
	}
	sp := top + n
	r.stackTop, r.stackHigh = sp, max(r.stackHigh, sp)
	step := in.Op == bytecode.OpIterNextOrJump
	if ctx.GoOp&abi.GoOpMarking == 0 {
		// A call made through Go only because the collector marks costs
		// the code nothing it chose (abi.GoOpMarking). A loop's step Go
		// makes costs what any call of Go does: a loop of them, over a Map
		// or a generator, whose body does little, the tree tier runs (one
		// over an array native code steps itself, and calls nothing).
		e.ssaStats.goCalls++
	}
	s.goCalls++
	if !step && jitCallUnsettled(e, pc, in) {
		r.jitCallSeen(f, e, pc, sp, in)
	}
	// Past its call meanwhile, as the interpreter's frame is during one,
	// which a stack trace shows.
	f.pc = uint32(pc + 1)
	last, shared := s.ssaCtx, s.ssaSharedGen
	if !run.pinned {
		r.jitGoPin(idx)
	}
	v, err := r.jitCallMade(in, sp)
	jitClearArgs(r.stack, top, sp)
	r.stackTop = top
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		// What the call ran shared other values with the contexts past
		// this one, which this code's calls run in.
		r.jitShareContexts(idx, f)
	}
	if err != nil || r.stopped != nil || e.reoptDue() || e.ssa != code {
		s.goPending = jitGoPending{set: true, pc: pc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitRunFrame is the frame Go entered the code running in ctx with, or
// nil for code a native call runs in it, which has none of the VM's.
func (r *Runtime) jitRunFrame(ctx *abi.Context) *frame {
	s := r.jit
	idx := int(ctx.Level)
	if ctx.Live != 0 || idx >= len(s.goRuns) || s.ssaCtxs[idx] != ctx {
		return nil
	}
	return s.goRuns[idx].f
}

// jitCalleeFrame is a frame for the native callee running in ctx, which
// has none of the VM's, to make a closure on or close what closures
// captured (ssa.GoClosure, GoClose): its closure, its locals where its
// context has them, its receiver, and the upvalues its closures have
// captured of those locals, which its context keeps (calleeOpen) -- none
// once its context's Open says so, as a native call clears it. Its code
// closes them as it returns, and Go gives them to the frame it makes of
// the callee if it finishes it (jitUnwindNative). Or nil.
func (r *Runtime) jitCalleeFrame(ctx *abi.Context) *frame {
	s := r.jit
	cl := (*closure)(ctx.Closure)
	if ctx.Live != abi.LiveCall || cl == nil || ctx.Locals == nil {
		return nil
	}
	idx := int(ctx.Level)
	if len(s.calleeOpen) < len(s.ssaCtxs) {
		s.calleeOpen = append(s.calleeOpen, make([][]*upvalue, len(s.ssaCtxs)-len(s.calleeOpen))...)
	}
	if ctx.Open == 0 {
		clear(s.calleeOpen[idx])
		s.calleeOpen[idx] = s.calleeOpen[idx][:0]
	}
	f := &s.calleeFrame
	*f = frame{}
	f.cl = cl
	f.locals = unsafe.Slice((*Value)(ctx.Locals), cl.fn.LocalCount)
	f.openUpvalues = s.calleeOpen[idx]
	f.this, f.newTarget = *(*Value)(unsafe.Pointer(&ctx.This)), Undefined
	return f
}

// jitNeedsMaker reports whether the function template c makes is an arrow
// that reads, or whose arrows read, its maker's new.target or super,
// which a native callee's context does not have (jitCalleeFrame).
func jitNeedsMaker(c Value) bool {
	t := c.template()
	if t == nil || t.fn.Kind != bytecode.KindArrow {
		return false
	}
	var reads func(fn *bytecode.Function) bool
	reads = func(fn *bytecode.Function) bool {
		for _, in := range fn.Code {
			switch in.Op {
			case bytecode.OpNewTarget, bytecode.OpSuperCall, bytecode.OpGetSuperProp, bytecode.OpGetSuperIndex,
				bytecode.OpSetSuperProp, bytecode.OpSetSuperIndex, bytecode.OpSuperBase:
				return true
			}
		}
		for _, k := range fn.Constants {
			if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Kind == bytecode.KindArrow && reads(k.Fn) {
				return true
			}
		}
		return false
	}
	return reads(t.fn)
}

// jitGoPin pins the code the run in context idx runs, until its native code
// leaves (jitGoUnpin), if it has not been yet.
func (r *Runtime) jitGoPin(idx int) {
	s := r.jit
	if run := &s.goRuns[idx]; !run.pinned {
		s.pinSerial++
		run.pinned, run.pinnedAt, run.serial = true, len(s.goPinned), s.pinSerial
		s.goPinned = append(s.goPinned, run.code)
	}
}

// jitEntryOf is the entry of cl's function: the one cl's hint names, or,
// missing that, the cache's, which cl's hint then names, as an entry from
// Go would have it (tryJITAt). A call of Go from a native callee finds
// each level's so, at every call: by the cache's weak key, a tenth of
// RegExp's time went to finding them.
func (r *Runtime) jitEntryOf(cl *closure) *jitEntry {
	s := r.jit
	if e := s.hint(cl); e != nil {
		return e
	}
	e := s.cache[weak.Make(cl.fn)]
	if e != nil && e.ssa != nil {
		cl.setHint(hintFor(e))
	}
	return e
}

// jitGoCallNested is jitGoCall for code a native call runs (abi.LiveCall),
// as V8's code calls a function it has no code of its own for wherever it
// is inlined or called: RayTrace's rayTrace, called natively, constructs
// what it could not inline by a call of Go, and had left native code at
// every such call, its callers' too, Go finishing each level. The
// levels from the run's own up to this one have no frames of the VM's;
// each is given one while the call runs, as the interpreter's frame is
// during a call -- its function, its receiver, past the call it makes --
// for a stack trace, and the depth of calls, to show, made as jitUnwindNative
// makes them and popped after. The code of each level is pinned until the
// run's native code leaves: what the call runs may compile any of them
// again. A call that threw, or after which this code is due to be compiled
// again, is left for Go to finish at its exit (goPending), as at a run's
// own level.
func (r *Runtime) jitGoCallNested(ctx *abi.Context, idx int) bool {
	s := r.jit
	ctxs := s.ssaCtxs
	if idx >= len(ctxs) || ctxs[idx] != ctx {
		return false
	}
	root := idx - 1
	for root >= 0 && ctxs[root].Live == abi.LiveCall {
		root--
	}
	if root < 0 || ctxs[root].Live != 0 || root >= len(s.goRuns) || s.goRuns[root].f == nil {
		return false
	}
	pc, depth := int(ctx.ExitPC), int(ctx.ExitDepth)
	in, n := jitGoCallInstr(ctx.GoOp)
	top := r.stackTop
	if n > abi.MaxGoArgs || depth < n || top+n > len(r.stack) {
		return false
	}
	// The run's code pinned, and each level's, which is given a frame:
	// one walk, the levels' entries found once. A level's code pinned for
	// this run before is known by its entry (pinSerial), not searched for.
	run := &s.goRuns[root]
	if !run.pinned {
		r.jitGoPin(root)
	}
	runF, serial := run.f, run.serial
	depthBefore := r.frameDepth
	var f *frame
	var e *jitEntry
	lv := &s.levels
	if lv.serial == serial && lv.root == root && lv.idx == idx && lv.depth == depthBefore && lv.n == idx-root {
		// The frames the last call made for these levels, as they are
		// still: each level the same call's, only past another call.
		for k := range lv.n {
			c := ctxs[root+1+k]
			if c.Closure != lv.at[k].cl || c.Base != lv.at[k].base || c.This != lv.at[k].this {
				goto made
			}
		}
		for k := range lv.n {
			lv.at[k].f.pc = uint32(ctxs[root+1+k].ExitPC) + 1
		}
		f, e = lv.at[lv.n-1].f, lv.e
		r.frameDepth = depthBefore + lv.n
		goto framed
	}
made:
	lv.serial = 0
	for i := root + 1; i <= idx; i++ {
		c := ctxs[i]
		lcl := (*closure)(c.Closure)
		if lcl == nil {
			r.frameDepth = depthBefore
			return false
		}
		if e = r.jitEntryOf(lcl); e == nil || e.ssa == nil || int(c.ExitPC) >= len(lcl.fn.Code) {
			r.frameDepth = depthBefore
			return false
		}
		if e.pinSerial != serial || e.pinCode != e.ssa {
			if !slices.Contains(s.goPinned[run.pinnedAt:], e.ssa) {
				s.goPinned = append(s.goPinned, e.ssa)
			}
			e.pinSerial, e.pinCode = serial, e.ssa
		}
		base := int(c.Base)
		f = r.pushFrame()
		f.cl = lcl
		f.locals = r.stack[base : base+lcl.fn.LocalCount : base+lcl.fn.LocalCount]
		f.base = base + lcl.fn.LocalCount
		f.pc = uint32(c.ExitPC) + 1
		f.this, f.newTarget, f.callee, f.args = *(*Value)(unsafe.Pointer(&c.This)), Undefined, nil, nil
		f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
		f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
		if k := i - root - 1; k < len(lv.at) {
			lv.at[k] = jitLevelFrame{f: f, cl: c.Closure, base: c.Base, this: c.This}
		}
	}
	if idx-root <= len(lv.at) {
		lv.serial, lv.root, lv.idx, lv.depth, lv.n, lv.e = serial, root, idx, depthBefore, idx-root, e
	}
framed:
	code := e.ssa
	// The run's frame past the call it makes.
	runF.pc = uint32(ctxs[root].ExitPC) + 1
	for i := range n {
		r.stack[top+i] = jitGoArg(ctx, i)
	}
	sp := top + n
	r.stackTop, r.stackHigh = sp, max(r.stackHigh, sp)
	marking := ctx.GoOp&abi.GoOpMarking != 0
	if !marking {
		e.ssaStats.goCalls++
	}
	s.goCalls++
	if in.Op != bytecode.OpIterNextOrJump && jitCallUnsettled(e, pc, in) {
		r.jitCallSeen(f, e, pc, sp, in)
	}
	if !marking {
		// As at a run's own level, a call made through Go only because the
		// collector marks is not the code's.
		r.jitCalleeProfit(e, serial, pc)
	}
	last, shared := s.ssaCtx, s.ssaSharedGen
	// What the call runs natively runs in contexts past the levels', which
	// hold their state until they go on.
	ctxTop := s.ctxTop
	s.ctxTop = max(ctxTop, idx+1)
	v, err := r.jitCallMade(in, sp)
	s.ctxTop = ctxTop
	jitClearArgs(r.stack, top, sp)
	r.stackTop = top
	r.frameDepth = depthBefore
	s.ssaCtx = last
	if !marking {
		s.goMark = jitGoMark{serial: serial, edges: r.backEdges - s.pollCredit, hint: e.hint, pc: pc}
	}
	if s.ssaSharedGen != shared {
		// What the call ran shared other values with the contexts past
		// the run's, which its levels' calls run in.
		r.jitShareContexts(root, runF)
	}
	if err != nil || r.stopped != nil || e.reoptDue() || e.ssa != code {
		s.goPending = jitGoPending{set: true, pc: pc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitLevels is the frames the last call of Go from a native callee made
// for its levels (jitGoCallNested), at most four, which the next call from
// the same levels takes as they are, but for their PCs: in the run whose
// pins serial names (0 for none), from context root up to idx, n of them,
// above depth; e the innermost level's entry. Nothing else makes a frame
// there while native code runs: a call of Go at the run's own level, which
// makes the built-in's there, forgets them (serial 0), as does the run's
// native code leaving; what a call runs makes its frames above them.
type jitLevels struct {
	serial              uint64
	root, idx, depth, n int
	e                   *jitEntry
	at                  [4]jitLevelFrame
}

// jitLevelFrame is a level's frame, and the closure, stack base and
// receiver its context had when it was made.
type jitLevelFrame struct {
	f    *frame
	cl   unsafe.Pointer
	base uint64
	this abi.Slot
}

// jitGoMark is where the last call of Go a native callee made returned
// (jitGoCallNested): in which run's pins (serial), the back-edge counter
// then, net of the polls' budgets, and its entry's hint -- which names it
// without keeping it -- and PC. hint 0 is none yet.
type jitGoMark struct {
	serial uint64
	edges  int
	hint   uint32
	pc     int
}

// jitCalleeProfit accounts for a call of Go that e's code makes at pc as a
// native callee, in the run whose pins serial names, and every jitSSAProbe
// such calls decides whether its native code pays, as jitSSAProfit decides
// for code Go enters: each call of Go costs what an exit and an entry
// would (jitSSAMinWork), against the work the code did natively since the
// last -- the back edges taken, at its loops' mean length, and the
// distance from the last call it made, at a later PC in the same run. Called natively,
// its calls cost it the same as entered from Go, but its native callers
// pay no entry: called through Go instead (ExitEnter), each call of it
// would cost one. So it pays while its work makes up for its calls of Go
// beyond its calls in (its link's in); if not -- a function of loops that only
// call built-ins, RegExp's run blocks, a call of Go at every few
// instructions -- Go stops entering it and native callers stop calling it
// (notNative), and the existing tiers run it, its calls of built-ins the
// tree tier's own, cheaper than calls of Go from native code.
func (r *Runtime) jitCalleeProfit(e *jitEntry, serial uint64, pc int) {
	s := r.jit
	st := &e.ssaStats
	// The back edges since the last such call, whatever ran them -- the
	// tiers share the counter, so the guess errs toward the native code --
	// and, in the same run, the distance from the call before it.
	m := &s.goMark
	if back := m.edges - (r.backEdges - s.pollCredit); m.hint != 0 && back > 0 {
		st.calleeWork += uint64(back) * uint64(e.ssaLoop)
	}
	if m.serial == serial && m.hint == e.hint && pc > m.pc {
		st.calleeWork += uint64(pc - m.pc)
	}
	st.calleeGoCalls++
	if st.calleeGoCalls%jitSSAProbe != 0 || st.calleeGoCalls <= e.link.in || jitMarking() {
		return
	}
	if st.calleeWork < (st.calleeGoCalls-e.link.in)*jitSSAMinWork {
		e.entrySlow = true
		e.notNative, e.link.entry, e.nativeRetry = true, 0, 0
		e.nativeBackoff = min(e.nativeBackoff+1, jitNativeBackoffs)
	}
}

// jitStoreElem stores v at index n of o as an assignment would, where that
// runs no script and cannot throw (ssa.GoElemWrite): over an element in
// its dense storage, as the interpreter's own fast path does; into a hole
// there, at the array's length or past it, as setElem grows it, when it
// may grow and nothing up its chain could take the store -- a setter, a
// read-only element. Crypto's BigInteger fills its arrays from their ends
// down, and extends them at their lengths: native code had left for Go at
// each such store.
func jitStoreElem(o *Object, n float64, v Value) bool {
	i := uint32(n)
	if float64(i) != n || o.flags&objMappedArguments != 0 {
		return false
	}
	if uint(i) < uint(len(o.elems)) && !isHole(o.elems[i]) {
		o.elems[i] = v
		return true
	}
	const want = objExtensible | objArrayLengthWritable
	if o.class != ClassArray || o.flags&(want|objHasSparseElements) != want || !o.noIndexKeys() {
		return false
	}
	for p := o.proto; p != nil; p = p.proto {
		if p.class != ClassObject && p.class != ClassArray || len(p.elems) != 0 || !p.noIndexKeys() {
			return false
		}
	}
	// What setValueProp would do: setElem, unless the store is so far past
	// the end that the element is to be a property of its own.
	return o.setElem(i, v)
}

// jitUndefinedCell is the cell native code reads an absent element from
// (abi.Encoding's UndefinedCell): undefined, and nothing writes it.
var jitUndefinedCell = Undefined

// jitElemAbsent reports whether o[n] is undefined with nothing to ask
// (ssa.GoElemCell): n an index o has no element at -- past its length, a
// hole -- and neither o nor anything up its chain, each an ordinary object
// or array, has an element or an index of its own, which could hold a
// value or a getter. A lookup table read past what it holds,
// BI_RC[s.charCodeAt(i)], had left native code at each such read.
func jitElemAbsent(o *Object, n float64) bool {
	i := uint32(n)
	if float64(i) != n || i == math.MaxUint32 {
		return false
	}
	if uint(i) < uint(len(o.elems)) && !isHole(o.elems[i]) {
		return false
	}
	for p := o; p != nil; p = p.proto {
		if p.class != ClassObject && p.class != ClassArray || p.flags&(objHasSparseElements|objMappedArguments) != 0 || !p.noIndexKeys() {
			return false
		}
		if p != o && len(p.elems) != 0 {
			return false
		}
	}
	return true
}

// jitCloseRetired closes the retired code no call of Go is suspended in.
func (r *Runtime) jitCloseRetired() {
	s := r.jit
	kept := s.retired[:0]
	for _, x := range s.retired {
		if slices.Contains(s.goPinned, x.code) {
			kept = append(kept, x)
			continue
		}
		x.code.Close()
	}
	clear(s.retired[len(kept):])
	s.retired = kept
}

// jitPlainOperand reports whether v is a primitive + takes without a
// conversion that may throw or call anything: not an object, a symbol or a
// BigInt. A string and a number's, at most 25 code units, are below
// maxStringLength's margin unless the string is near it.
func jitPlainOperand(v Value) bool {
	return !v.IsObject() && !v.IsSymbol() && !v.IsBigInt() && (!v.IsString() || v.String().length <= maxStringLength-64)
}

// jitGoCallWork, which tests set, runs at each call of Go before it is
// answered: what Go may do there, the collector included.
var jitGoCallWork func()

// KeepLimit is ssa.KeepFeedback's: the build's keep cells' limit, 0 for
// the default (ssa.VirtualKeeps).
func (fb *jitFeedback) KeepLimit() int { return fb.keepLimit }

// GoCalls is ssa.GoFeedback's: the VM answers calls of Go (jitHostCall).
func (fb *jitFeedback) GoCalls() bool { return true }

// jitGoCallInstr is the call a call of Go makes (abi.Context's GoOp, which
// native code writes): its instruction, as the bytecode has it at the
// call, and how many operands it takes from the stack. Read from the
// bytecode, it was three loads, one after another, at every call.
func jitGoCallInstr(op uint64) (bytecode.Instr, int) {
	argc := uint32(op>>abi.GoOpArgcShift) & 0xff
	in := bytecode.Instr{Op: bytecode.OpCall, A: argc}
	n := int(argc) + 1
	switch op >> abi.GoOpKindShift & 0xff {
	case abi.GoCallMethod:
		in.Op = bytecode.OpCallMethod
		n++
	case abi.GoCallNew:
		in.Op = bytecode.OpNew
	case abi.GoCallStep:
		// A loop's step, its cursor the one operand.
		return bytecode.Instr{Op: bytecode.OpIterNextOrJump}, 1
	}
	return in, n
}

// jitCallUnsettled reports whether jitCallSeen has anything to note of a
// call of Go at pc: not, once the call is settled as one Go makes
// (jitCallDone) and the code has no inlining pending, unless it constructs
// (a pool to fill). Most calls of Go are such, a built-in's: asked here,
// they cost no call.
func jitCallUnsettled(e *jitEntry, pc int, in bytecode.Instr) bool {
	return in.Op == bytecode.OpNew || e.inlinePending || uint(pc) >= uint(len(e.callSites)) || e.callSites[pc] != jitCallDone
}

// jitCallMade makes a call of Go, in, its operands on the VM's stack up to
// sp, as jitCallAt does; a built-in of this realm -- what most calls native
// code leaves to Go call -- as callDirect calls one, without first asking
// whether it is a closure with a tree, Math's, or Function.prototype.call:
// as V8's code calls a builtin it knows through its own stub, not the
// generic Call.
func (r *Runtime) jitCallMade(in bytecode.Instr, sp int) (Value, error) {
	if in.Op == bytecode.OpIterNextOrJump {
		// A loop's step: its next value, or, the cursor done, the
		// uninitialized value (abi.GoCallStep).
		v, ok, err := r.iterNext(r.stack[sp-1])
		if err == nil && !ok {
			v = uninitialized
		}
		return v, err
	}
	if in.Op != bytecode.OpNew {
		argc := int(in.A)
		if callee := r.stack[sp-argc-1]; callee.IsObject() {
			o := callee.Object()
			if fd := o.fn(); fd != nil && fd.native != nil && fd.mathOp == 0 && !fd.bound && o != r.callFn && (fd.realm == nil || fd.realm == r.Realm) {
				this := Undefined
				if in.Op == bytecode.OpCallMethod {
					this = r.stack[sp-argc-2]
				}
				args := r.stack[sp-argc : sp]
				if err := r.tick(); err != nil {
					return Undefined, err
				}
				if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
					return Undefined, err
				}
				v, err := fd.native(r, this, args)
				r.frameDepth--
				if r.meter != nil && err == nil {
					err = r.chargeResult(v)
				}
				return v, err
			}
		}
	}
	v, _, err := r.jitCallAt(in, sp)
	return v, err
}

// jitClearArgs clears the operands a call of Go put past the VM stack's
// top, from top to sp, so that they keep nothing: a few, each store's
// write barrier tested in line, which clear's call of the runtime costs
// more than.
func jitClearArgs(stack []Value, top, sp int) {
	for i := top; i < sp; i++ {
		stack[i] = Value{}
	}
}

// jitGoArg is a call of Go's operand i (abi.Context's GoArgs).
func jitGoArg(ctx *abi.Context, i int) Value {
	return *(*Value)(unsafe.Pointer(&ctx.GoArgs[i]))
}

// jitMarking reports whether the collector marks (jit.Marking): a
// variable, which the tests set.
var jitMarking = jit.Marking

// jitInlineLeft counts an exit from the callee inlined at pc, or from its
// call's checks: after jitInlineExits the call is not inlined but made
// natively, if the callee's code allows it, and the code is compiled
// again. An exit while the collector marks is not counted: native code had
// left then wherever it would store a pointer without a write barrier --
// a construction's receiver adding its properties, which Go now does for
// it -- and sixteen such exits in one collection had EarleyBoyer's sc_Pair
// constructions stop being inlined for good, some runs in three.
func (r *Runtime) jitInlineLeft(e *jitEntry, pc int) {
	if jitMarking() {
		return
	}
	for i := range e.inlines {
		if x := &e.inlines[i]; int(x.pc) == pc && !slices.Contains(e.notInline, x.pc) {
			if x.exits++; x.exits >= jitInlineExits {
				e.notInline = append(e.notInline, x.pc)
				if e.callSites != nil {
					e.callSites[pc] = jitCallDone
					if r.jitNativeCallee(x.cl) != nil {
						// A construction's with its pool.
						e.nativeCalls = append(e.nativeCalls, jitInline{pc: x.pc, cl: x.cl, obj: x.obj, pool: x.pool})
						e.callSites[pc] = jitCallNative
					}
				}
				e.inlineReopt = true
			}
		}
	}
}

// jitInlineDepth is how many levels of calls are inlined: a callee, the
// calls it makes, theirs (ssa's maxInlineDepth).
const jitInlineDepth = 3

// jitInlinesCalls reports whether a function that makes calls may be
// inlined with them, as V8 inlines a callee's callees: it may be but for
// its calls (ssa.InlineCalls), and every one is an intrinsic, Math.sqrt
// (jitFeedback.Intrinsic), or its own code inlines it, a function that may
// be inlined, with its calls if depth allows. cl is one of its closures.
func (r *Runtime) jitInlinesCalls(cl *closure, depth int) bool {
	fn := cl.fn
	if depth <= 0 || len(fn.Upvalues) != 0 || fn.TopLevel || fn.IsModule || fn.UsesArguments || fn.HasDirectEval || isGeneratorTemplate(fn) {
		return false
	}
	p, err := r.jitLower(fn, jitLowerInline)
	if err != nil {
		return false
	}
	calls, ok := ssa.InlineCalls(p)
	if !ok || len(calls) == 0 {
		return false
	}
	fb := &jitFeedback{r: r, fn: fn, cl: cl, prog: p}
	var e *jitEntry
	for _, pc := range calls {
		if _, ok := fb.Intrinsic(pc); ok {
			continue
		}
		if _, ok := fb.InstanceOf(pc); ok {
			continue
		}
		if e == nil {
			if e = r.jit.cache[weak.Make(fn)]; e == nil || e.callSites == nil {
				return false
			}
		}
		if _, ok := r.jitInlineSite(fn, pc, depth); !ok {
			return false
		}
	}
	return true
}

// jitInlineSite is the function fn's call at pc is inlined with, inside a
// callee inlined with fn, depth levels allowing: the one fn's own code
// inlines there, or the one it calls natively there, and no other, which
// may be inlined, with its own calls if depth allows (jitInlinesCalls).
func (r *Runtime) jitInlineSite(fn *bytecode.Function, pc, depth int) (jitInline, bool) {
	e := r.jit.cache[weak.Make(fn)]
	if e == nil || pc >= len(e.callSites) || slices.Contains(e.notInline, int32(pc)) {
		return jitInline{}, false
	}
	var list []jitInline
	switch e.callSites[pc] {
	case jitCallInlined:
		list = e.inlines
	case jitCallNative:
		list = e.nativeCalls
	}
	var in jitInline
	n := 0
	for _, x := range list {
		if int(x.pc) == pc {
			in, n = x, n+1
		}
	}
	if n != 1 || in.cl == nil || in.cl.fn == fn || in.via {
		// Not one call of a function inlined there: a call through
		// Function.prototype.call, which an inlined frame's call does not
		// make natively, would leave native code there at every call.
		return jitInline{}, false
	}
	if in.pool != nil && in.cl.fn.Leaf == bytecode.LeafForward {
		// A forwarding constructor's construction, inlined with the
		// method it forwards to.
		if r.jitForwardTarget(in.cl, in.obj, depth) == nil {
			return jitInline{}, false
		}
		return in, true
	}
	if !r.jitInlinable(in.cl.fn) && !r.jitInlinesCalls(in.cl, depth-1) {
		return jitInline{}, false
	}
	return in, true
}

// jitSeedCalls decides, before e's code for cl's function is compiled --
// first, or again for whatever reason -- the calls and constructions whose
// target the existing tiers have met (jitCalleeAt): each is inlined, or
// made natively if its target has code, at once -- as jitCallSeen would
// decide it once the call had left native code, which costs a compile for
// each call learned so, and as V8's TurboFan takes a call's target from
// its load's feedback. A target with no code is compiled for native
// callers then. A call that is not seen so is left to jitCallSeen. A
// target that turns out wrong costs an exit: a call checks its callee.
func (r *Runtime) jitSeedCalls(cl *closure, e *jitEntry, p *ir.Program) {
	fn := cl.fn
	if len(p.Maps) != len(fn.Code) {
		return
	}
	// The functions whose calls are being decided, up the compiles seeding
	// started: a target among them is not compiled again (jitSeedCallee).
	s := r.jit
	s.seedingFns = append(s.seedingFns, fn)
	defer func() { s.seedingFns = s.seedingFns[:len(s.seedingFns)-1] }()
	for pc, in := range fn.Code {
		in = jitCallInstr(in)
		switch in.Op {
		case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		default:
			continue
		}
		if e.callSites != nil && e.callSites[pc] >= jitCallNative {
			// Decided; one still counted toward its decision is not.
			continue
		}
		obj, read := r.jitCalleeObjectAt(cl, p, pc)
		if obj == nil {
			continue
		}
		o, callee := r.jitCallableClosure(cl, obj)
		if callee == nil {
			r.jitSeedBuiltin(cl, e, p, pc, in, obj, read)
			continue
		}
		if in.Op == bytecode.OpNew && !r.jitConstructs(o) {
			continue
		}
		if e.callSites == nil {
			e.callSites = make([]uint8, len(fn.Code))
		}
		if more := r.jitSeenCallees(cl, p, pc, o); len(more) != 0 {
			// A method read that has met objects of other classes, whose
			// methods there the call has called too: each called natively,
			// as the call would be once it had left native code for each,
			// as V8's code calls a polymorphic call's targets.
			r.jitSeedNativeCalls(e, fn, pc, in, append([]jitCallee{{o, callee}}, more...))
			continue
		}
		if callee.fn != fn && r.jitInlinesAt(callee, o, in) {
			e.inlines = append(e.inlines, r.jitInlineTarget(int32(pc), callee, o, in))
			e.callSites[pc] = jitCallInlined
			continue
		}
		cf := callee.fn
		if cf == fn || cf.TopLevel || cf.IsModule || cf.UsesArguments || cf.HasDirectEval || isGeneratorTemplate(cf) {
			continue
		}
		ce := r.jit.cache[weak.Make(cf)]
		if ce == nil || ce.ssa == nil {
			// A target the existing tiers have met is compiled for native
			// callers now, as the call would have it compiled once learned
			// (jitSeedCallee). How often it was called is not known: the
			// tree tier may run a call without one (closure.jitCalls).
			if ce = r.jitSeedCallee(callee); ce == nil {
				continue
			}
			if callee.fn != fn && r.jitInlinesAt(callee, o, in) {
				// Its calls decided now, it may be inlined with them, as
				// V8 inlines a callee's callees: it would have been once
				// its callers were compiled again for them.
				e.inlines = append(e.inlines, r.jitInlineTarget(int32(pc), callee, o, in))
				e.callSites[pc] = jitCallInlined
				continue
			}
		}
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), callee, o, in))
		e.callSites[pc] = jitCallNative
	}
}

// jitSeedBuiltin decides, as jitSeedCalls does a script's function, a
// call of a built-in native code makes itself whose target the existing
// tiers have met: a.push(v) and a.pop(), the realm's own; new Array(); and
// G.call(this, ...), G read before it -- each as jitCallSeen decides it
// once the call has left native code, which, for calls the code had made
// in the existing tiers every time, cost each function a compile after its
// first: DeltaBlue's collections' and constraints' constructors. The call
// checks what jitCallSeen finds out at its exit -- the receiver an array
// the fast path takes, the function G -- and leaves for Go otherwise.
func (r *Runtime) jitSeedBuiltin(cl *closure, e *jitEntry, p *ir.Program, pc int, in bytecode.Instr, o *Object, read int) {
	fd := o.fn()
	var x jitInline
	switch {
	case in.Op == bytecode.OpNew && in.A == 0 && o == r.proto.arrayCtor:
		x = jitInline{pc: int32(pc), obj: o, pool: r.jitNewPool(o)}
	case in.Op != bytecode.OpCallMethod || fd == nil:
		return
	case in.A == 0 && fd.elemOp == elemPop && fd.realm == r.Realm:
		x = jitInline{pc: int32(pc), obj: o, pop: true}
	case in.A == 1 && fd.elemOp == elemPush && fd.realm == r.Realm:
		x = jitInline{pc: int32(pc), obj: o, push: true}
	case in.A >= 1 && o == r.callFn && read > 0 && (cl.fn.Code[read-1].Op == bytecode.OpGetProp || cl.fn.Code[read-1].Op == bytecode.OpGetGlobal):
		v, ok := r.jitChainValue(cl, p, read-1, jitChainReads)
		if !ok || !v.IsObject() {
			return
		}
		g, gcl := r.jitCallableClosure(cl, v.Object())
		if gcl == nil {
			return
		}
		if ce := r.jit.cache[weak.Make(gcl.fn)]; (ce == nil || ce.ssa == nil) && r.jitSeedCallee(gcl) == nil {
			return
		}
		x = jitInline{pc: int32(pc), cl: gcl, obj: g, via: true}
	default:
		return
	}
	if e.callSites == nil {
		e.callSites = make([]uint8, len(cl.fn.Code))
	}
	e.nativeCalls = append(e.nativeCalls, x)
	e.callSites[pc] = jitCallNative
}

// jitSeedDepth is how many compiles deep seeding a call's target may
// start compiles (jitSeedCallee): a callee's calls' targets, theirs, as
// deep as calls are inlined.
const jitSeedDepth = jitInlineDepth

// jitSeedCallee compiles cl's function for native callers, a target a call
// seeding decided (jitSeedCalls) had no code for, and returns its entry, or
// nil: as deep as jitSeedDepth -- the target's own calls decided so in turn,
// as the calls they would be once learned at the exits of code calling them
// natively, in a callee, which leave for Go and are made by it -- and not
// for a function whose calls are being decided further up, a cycle. Before,
// a compile seeding started started none: the calls of DeltaBlue's
// constraints' constructors and methods, compiled for their callers, were
// learned one exit, and one compile, at a time.
func (r *Runtime) jitSeedCallee(cl *closure) *jitEntry {
	s := r.jit
	if s.seeding >= jitSeedDepth || slices.Contains(s.seedingFns, cl.fn) {
		return nil
	}
	s.seeding++
	defer func() { s.seeding-- }()
	return r.jitNativeCallee(cl)
}

// jitCallee is a function object a call calls, and its closure.
type jitCallee struct {
	o  *Object
	cl *closure
}

// jitSeedNativeCalls decides the call at pc to call each of targets
// natively, up to jitCallTargets, those with no code compiled for native
// callers now, as jitSeedCalls does one.
func (r *Runtime) jitSeedNativeCalls(e *jitEntry, fn *bytecode.Function, pc int, in bytecode.Instr, targets []jitCallee) {
	for _, t := range targets {
		cf := t.cl.fn
		if len(e.nativeCalls) != 0 && slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return int(x.pc) == pc && x.obj == t.o }) ||
			cf == fn || cf.TopLevel || cf.IsModule || cf.UsesArguments || cf.HasDirectEval || isGeneratorTemplate(cf) ||
			jitCountAt(e.nativeCalls, pc) >= jitCallTargets {
			continue
		}
		ce := r.jit.cache[weak.Make(cf)]
		if ce == nil || ce.ssa == nil {
			if ce = r.jitSeedCallee(t.cl); ce == nil {
				continue
			}
		}
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), t.cl, t.o, in))
		e.callSites[pc] = jitCallNative
	}
}

// jitCountAt is how many of calls are at pc.
func jitCountAt(calls []jitInline, pc int) int {
	n := 0
	for _, x := range calls {
		if int(x.pc) == pc {
			n++
		}
	}
	return n
}

// jitSeenCallees are the other functions a method call at pc has called,
// besides o, as its method read's cache saw them (propCache.seen): of each
// shape it met, the method a prototype held, as it holds it now -- the
// call checks its callee -- each a function jitCalled would take. Or none.
func (r *Runtime) jitSeenCallees(cl *closure, p *ir.Program, pc int, o *Object) []jitCallee {
	fn := cl.fn
	if jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod {
		return nil
	}
	depth := p.Maps[pc].Depth
	slot := depth - int(fn.Code[pc].A) - 1
	if slot < 1 {
		return nil
	}
	read := -1
	for q := pc - 1; q >= 0; q-- {
		d := p.Maps[q].Depth
		if d == slot && fn.Code[q].Op == bytecode.OpGetPropThis {
			read = q
			break
		}
		if d < slot+1 || d == slot+1 && p.Maps[q+1].Depth < slot+2 {
			return nil
		}
	}
	if read < 0 {
		return nil
	}
	in := fn.Code[read]
	if int(in.B) >= len(cl.ic) || int(in.A) >= len(cl.names) || cl.ic[in.B].seen == nil {
		return nil
	}
	var out []jitCallee
	seen := cl.ic[in.B].seen
	for _, c := range seen.entries[:seen.n] {
		h := c.p1
		if c.p2 != nil {
			h = c.p2
		}
		if c.shape == nil || c.shape == noShape || c.getter || h == nil || c.idx < 0 || int(c.idx) >= len(h.props) {
			continue
		}
		prop := &h.props[c.idx]
		if prop.key != cl.names[in.A] || prop.flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || !prop.value.IsObject() {
			continue
		}
		mo, mc := r.jitCallableClosure(cl, prop.value.Object())
		if mc == nil || mo == o || slices.ContainsFunc(out, func(x jitCallee) bool { return x.o == mo }) {
			continue
		}
		out = append(out, jitCallee{mo, mc})
	}
	return out
}

// jitCalleeAt is the function the call or construction at pc calls as the
// existing tiers have met it, and its closure: the read of its callee --
// the nearest before it at the callee's depth, with nothing between but
// the arguments, above it: a method call's get_prop_this, whose cache
// found one plain data property on a prototype, or another's get_global,
// whose cache found the global's, or get_prop of an object so read, a
// namespace's constructor, new Flog.RayTracer.Vector(...), whose value now
// is taken as V8 folds a constant's loads (jitChainValue) -- a function
// jitCalled would take. Or nil. The call checks the function it calls.
func (r *Runtime) jitCalleeAt(cl *closure, p *ir.Program, pc int) (*Object, *closure) {
	o, _ := r.jitCalleeObjectAt(cl, p, pc)
	if o == nil {
		return nil, nil
	}
	return r.jitCallableClosure(cl, o)
}

// jitCalleeObjectAt is the function object jitCalleeAt finds, built-ins'
// too, and the PC of the method read it was found by, or -1.
func (r *Runtime) jitCalleeObjectAt(cl *closure, p *ir.Program, pc int) (*Object, int) {
	fn := cl.fn
	depth := p.Maps[pc].Depth
	slot := depth - int(fn.Code[pc].A) - 1
	op := bytecode.OpGetGlobal
	if jitCallOp(fn.Code[pc].Op) == bytecode.OpCallMethod {
		op = bytecode.OpGetPropThis
	}
	if depth < 0 || slot < 0 || op == bytecode.OpGetPropThis && slot < 1 {
		return nil, -1
	}
	read := -1
	for q := pc - 1; q >= 0; q-- {
		d := p.Maps[q].Depth
		if d == slot && fn.Code[q].Op == op {
			read = q
			break
		}
		if d == slot+1 && op == bytecode.OpGetGlobal && fn.Code[q].Op == bytecode.OpGetProp {
			// A property of an object read so, its value now.
			v, ok := r.jitChainValue(cl, p, q, jitChainReads)
			if !ok || !v.IsObject() {
				return nil, -1
			}
			return v.Object(), -1
		}
		// Only the arguments' instructions, which leave the callee be: the
		// first pushes on it, and the rest work above.
		if d < slot+1 || d == slot+1 && p.Maps[q+1].Depth < slot+2 {
			return nil, -1
		}
	}
	if read < 0 {
		return nil, -1
	}
	in := fn.Code[read]
	if int(in.B) >= len(cl.ic) || int(in.A) >= len(cl.names) {
		return nil, -1
	}
	c := &cl.ic[in.B]
	if op == bytecode.OpGetPropThis && c.p1 == nil {
		// Not found on a prototype: a method read off an object the reads
		// before it name, RayTrace's Flog.RayTracer.Vector.prototype.add(...),
		// its value now (jitChainValue). Calls there had been learned one
		// exit at a time, and its functions ran out of compiles first.
		if v, ok := r.jitChainValue(cl, p, read, jitChainReads); ok && v.IsObject() {
			return v.Object(), read
		}
		return nil, -1
	}
	h, idx := c.p1, c.idx
	if op == bytecode.OpGetGlobal {
		// The global environment, where the read found the name at the
		// cache's index, as pureGlobalSlow looks: not a name a script's
		// lexical binding shadows. Or, where the cache has not found it
		// -- native code reads a global without it, from where the global
		// object had it when compiled (jitFeedback.Global) -- where the
		// environment has it now, as that read finds it: a constructor
		// only native code had read was learned one exit, and one compile,
		// at a time, every construction of DeltaBlue's. A script's
		// lexical binding of the name, a class's say, is where the
		// realm's global scope has it.
		if h = cl.scope(); len(r.globalLex.props) != 0 && r.lexShadows(cl.names[in.A]) {
			if !r.isGlobalScope(h) {
				return nil, -1
			}
			h, idx = r.globalLex, -1
		}
		if h != nil && (idx < 0 || int(idx) >= len(h.props) || h.props[idx].key != cl.names[in.A]) {
			idx = h.findOwn(cl.names[in.A])
		}
	} else {
		if c.shape == nil || c.shape == noShape || c.getter {
			return nil, -1
		}
		if c.p2 != nil {
			h = c.p2
		}
	}
	if h == nil || idx < 0 || int(idx) >= len(h.props) {
		return nil, -1
	}
	prop := &h.props[idx]
	if prop.key != cl.names[in.A] || prop.flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || !prop.value.IsObject() {
		return nil, -1
	}
	return prop.value.Object(), read
}

// jitCallableClosure is o and its closure, if a call in cl's code may make
// it natively or inline it, as jitCalled has a callee; or nil.
func (r *Runtime) jitCallableClosure(cl *closure, o *Object) (*Object, *closure) {
	fd := o.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || isGeneratorTemplate(fd.closure.fn) || fd.closure.realm != r.Realm ||
		fd.closure.scope() != cl.scope() {
		return nil, nil
	}
	return o, fd.closure
}

// jitChainReads is how many reads jitChainValue follows: a global and the
// properties after it, Flog.RayTracer.Vector's three, and the five of
// Flog.RayTracer.Vector.prototype.subtract, a method's, with one to spare.
const jitChainReads = 6

// jitChainValue is the value the read at q of cl's code gives now, if it
// is a global's read, by its cache, or a data property's of an object a
// read just before it gives so, at most n reads in all: a guess, which
// whatever takes it checks.
func (r *Runtime) jitChainValue(cl *closure, p *ir.Program, q, n int) (Value, bool) {
	fn := cl.fn
	if n <= 0 || q < 0 || q >= len(fn.Code) || p.Maps[q].Depth < 0 {
		return Undefined, false
	}
	in := fn.Code[q]
	if int(in.A) >= len(cl.names) {
		return Undefined, false
	}
	switch in.Op {
	case bytecode.OpGetGlobal:
		// The global object's data property of the name -- or the
		// script's lexical binding that shadows it, in the realm's global
		// scope -- looked up, not through the read's cache, which native
		// code that reads it never fills.
		h := cl.scope()
		if h != nil && len(r.globalLex.props) != 0 && r.lexShadows(cl.names[in.A]) {
			if !r.isGlobalScope(h) {
				return Undefined, false
			}
			h = r.globalLex
		}
		if h == nil {
			return Undefined, false
		}
		prop := h.getOwnVisible(cl.names[in.A])
		if prop == nil || prop.flags&propAccessor != 0 || prop.value.IsUninitialized() {
			return Undefined, false
		}
		return prop.value, true
	case bytecode.OpGetProp, bytecode.OpGetPropThis:
		recv, ok := r.jitChainReceiver(cl, p, q, n)
		if !ok {
			return Undefined, false
		}
		prop := recv.getOwnVisible(cl.names[in.A])
		if prop == nil || prop.flags&propAccessor != 0 {
			return Undefined, false
		}
		return prop.value, true
	}
	return Undefined, false
}

// jitChainReceiver is the object the property read at q of cl's code reads
// from, if the read just before it gives it so (jitChainValue), at most n
// reads in all: the value that read left on top.
func (r *Runtime) jitChainReceiver(cl *closure, p *ir.Program, q, n int) (*Object, bool) {
	fn := cl.fn
	if n <= 0 || q <= 0 || q >= len(fn.Code) || p.Maps[q].Depth < 0 || p.Maps[q-1].Depth < 0 {
		return nil, false
	}
	switch prev := fn.Code[q-1].Op; {
	case prev == bytecode.OpGetGlobal && p.Maps[q-1].Depth == p.Maps[q].Depth-1,
		prev == bytecode.OpGetProp && p.Maps[q-1].Depth == p.Maps[q].Depth:
	default:
		return nil, false
	}
	recv, ok := r.jitChainValue(cl, p, q-1, n-1)
	if !ok || !recv.IsObject() {
		return nil, false
	}
	return recv.Object(), true
}

// jitInlinedCalls has a call inlined for one function that calls another,
// o, cl's, call them natively instead, and others, up to jitCallTargets
// (jitCallTarget): the code is compiled again for it at once.
func (r *Runtime) jitInlinedCalls(e *jitEntry, pc int, o *Object, cl *closure, in bytecode.Instr) {
	e.notInline = append(e.notInline, int32(pc))
	e.callSites[pc] = jitCallNative
	e.inlineReopt = true
	same := false
	for _, x := range e.inlines {
		if int(x.pc) == pc && r.jitNativeCallee(x.cl) != nil {
			// A construction's with its pool. Another closure of the
			// function inlined -- one its maker makes anew -- is called by
			// its code (jitSameCode), decided now, not at a compile more.
			byCode := x.obj != o && jitSameCode(x.cl, cl)
			same = same || byCode
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: x.pc, cl: x.cl, obj: x.obj, pool: x.pool, byCode: byCode})
		}
	}
	if !same && (in.Op != bytecode.OpNew || r.jitConstructs(o)) && r.jitNativeCallee(cl) != nil {
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), cl, o, in))
	}
}

// jitCalled is the function object a call at the top of the frame's
// operands, up to sp, calls, and its closure, if it is one native code may
// call or inline: one the script made, in the frame's realm and scope.
func (r *Runtime) jitCalled(f *frame, sp int, in bytecode.Instr) (*Object, *closure) {
	callee := r.stack[sp-int(in.A)-1]
	if !callee.IsObject() {
		return nil, nil
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || isGeneratorTemplate(fd.closure.fn) || fd.closure.realm != r.Realm ||
		fd.closure.scope() != f.cl.scope() {
		return nil, nil
	}
	return o, fd.closure
}

// jitCallTarget notes a call made natively that left for Go at pc: a
// function it calls that it was not compiled to call joins those it calls
// natively, up to jitCallTargets, and the code is compiled again for it at
// once -- as V8's call feedback goes polymorphic.
func (r *Runtime) jitCallTarget(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	o, cl := r.jitCalled(f, sp, in)
	if cl == nil {
		return
	}
	n := 0
	for i := range e.nativeCalls {
		x := &e.nativeCalls[i]
		if int(x.pc) == pc {
			if x.cl != nil && x.obj != o && jitSameCode(x.cl, cl) {
				// Another closure of the function: called by its code from
				// the next compile on -- unless the call checks it so
				// already, a closure that captures bindings being called so
				// from the first (NativeCalls), and left for another reason:
				// its callee did, which a compile would not change.
				if !x.byCode && len(x.cl.upvalues) == 0 {
					e.inlineReopt = true
				}
				x.byCode = true
			}
			if x.obj == o || x.cl == nil || x.byCode && jitSameCode(x.cl, cl) {
				// One it calls, or a built-in's construction, which a site
				// makes alone. One it calls whose code native callers do not
				// call for now is called again after so many such calls.
				if ce := r.jit.cache[weak.Make(cl.fn)]; (x.obj == o || x.byCode) && ce != nil && ce.notNative {
					jitRetryNative(ce)
				}
				return
			}
			n++
		}
	}
	if n >= jitCallTargets || in.Op == bytecode.OpNew && !r.jitConstructs(o) {
		return
	}
	if ce := r.jitNativeCallee(cl); ce != nil {
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), cl, o, in))
		e.inlineReopt = true
	}
}

// jitReadsUpvalues reports whether cl's code reads or assigns a binding it
// captured.
func jitReadsUpvalues(cl *closure) bool {
	return len(cl.upvalues) != 0 && slices.ContainsFunc(cl.fn.Code, func(x bytecode.Instr) bool {
		switch x.Op {
		case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck, bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
			return true
		}
		return false
	})
}

// jitSameCode reports whether a and b are closures of one function, in one
// environment and realm: a call native code makes of either may check the
// function, and run the closure it finds (ssa.CallSite's ByCode), as each
// call of their maker makes a new one.
func jitSameCode(a, b *closure) bool {
	return a.fn == b.fn && a.env == b.env && a.realm == b.realm
}

// jitNativeTarget is a function a call at pc calls natively: for a
// construction, with the pool its objects come from, filled.
// jitInlinesAt reports whether a call of o, cl's function, by in may be
// inlined: cl's function may be, with its calls, and a construction's
// constructor is one native code constructs with (jitConstructs) that
// returns nothing, so that its receiver is the result, as V8 inlines a
// constructor.
func (r *Runtime) jitInlinesAt(cl *closure, o *Object, in bytecode.Instr) bool {
	if in.Op == bytecode.OpNew && cl.fn.Leaf == bytecode.LeafForward {
		// this.k.apply(this, arguments): k, inlined in it.
		return r.jitConstructs(o) && r.jitForwardTarget(cl, o, jitInlineDepth) != nil
	}
	if !r.jitInlinable(cl.fn) && !r.jitInlinesCalls(cl, jitInlineDepth-1) {
		return false
	}
	if jitReadsUpvalues(cl) {
		// A closure that reads what it captured, which an inlined callee
		// leaves to Go: called natively, by its code (jitSameCode), as a
		// closure made anew at each call of its maker is a new object.
		return false
	}
	return in.Op != bytecode.OpNew || r.jitConstructs(o) &&
		!slices.ContainsFunc(cl.fn.Code, func(x bytecode.Instr) bool { return x.Op == bytecode.OpReturn })
}

// jitForwardTarget is the closure of the method a forwarding constructor,
// o, cl's function (bytecode.LeafForward), forwards its arguments to, if
// it may be inlined in the constructor: k of this.k.apply(this, arguments),
// a data property of o's prototype, a function the script made, of cl's
// realm and scope, as jitCalled has a callee; or nil. The constructor
// does not count toward how deep calls are inlined, depth from it on: the
// method's calls are inlined depth-1 levels on. A method whose calls no
// code has decided yet -- one it constructs with, IntersectionInfo's
// initialize making a Color -- is compiled for native callers now, which
// decides them (jitSeedCalls), once: not by a compile seeding started.
func (r *Runtime) jitForwardTarget(cl *closure, o *Object, depth int) *closure {
	t := r.jitForwardMethod(cl, o)
	if t == nil {
		return nil
	}
	tc := t.fn().closure
	if tc.fn == cl.fn {
		return nil
	}
	if r.jitInlinable(tc.fn) {
		return tc
	}
	if e := r.jit.cache[weak.Make(tc.fn)]; (e == nil || e.callSites == nil) && r.jit.seeding == 0 {
		r.jit.seeding = jitSeedDepth
		r.jitNativeCallee(tc)
		r.jit.seeding = 0
	}
	if !r.jitInlinesCalls(tc, depth-1) {
		return nil
	}
	return tc
}

// jitForwardMethod is the function object jitForwardTarget's closure is
// of, or nil.
func (r *Runtime) jitForwardMethod(cl *closure, o *Object) *Object {
	fn := cl.fn
	if fn.Leaf != bytecode.LeafForward || len(fn.Code) < 2 || fn.Code[1].Op != bytecode.OpGetProp || int(fn.Code[1].A) >= len(cl.names) {
		return nil
	}
	p := o.getOwnVisible(atomPrototype)
	if p == nil || p.flags&propAccessor != 0 || !p.value.IsObject() {
		return nil
	}
	m := p.value.Object().getOwnVisible(cl.names[fn.Code[1].A])
	if m == nil || m.flags&propAccessor != 0 || !m.value.IsObject() {
		return nil
	}
	t := m.value.Object()
	fd := t.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || isGeneratorTemplate(fd.closure.fn) || fd.closure.realm != r.Realm || fd.closure.scope() != cl.scope() {
		return nil
	}
	return t
}

// forwardProperty is a forwarding constructor's inlined frame's property
// site (jitForwardFrame), known from its receiver: this.k, at its first
// read, as a pool's object finds it; k's apply, at its second, as k does.
func (fb *jitFeedback) forwardProperty(pc int) (ssa.PropertySite, bool) {
	in := fb.fn.Code[pc]
	var recv *Object
	switch {
	case pc == 1 && in.Op == bytecode.OpGetProp:
		pool := fb.fwd.pool
		for i := range pool.Count {
			if o := (*Object)(pool.Objects[i]); o != nil && o.shape != nil {
				recv = o
				break
			}
		}
		if recv == nil {
			// The pool ran out: an object as the next fill makes, whose
			// read the code is compiled for -- else the read would be
			// compiled knowing nothing, and leave every time.
			recv = fb.r.jitPoolObject(fb.fwd.ctor)
		}
	case pc == 2 && in.Op == bytecode.OpGetPropThis:
		recv = fb.r.jitForwardMethod(fb.cl, fb.fwd.ctor)
	}
	site := ssa.PropertySite{Key: uint32(fb.cl.names[in.A])}
	if recv == nil {
		return site, true
	}
	var c propCache
	fb.r.fillPropCache(&c, recv, fb.cl.names[in.A], false)
	return fb.siteFrom(in, &c)
}

// forwardInline is a forwarding constructor's inlined frame's call,
// apply_arguments, made the call of the method it forwards to, inlined
// with the construction's arguments (ssa.InlineSite's Forward).
func (fb *jitFeedback) forwardInline(pc int) (ssa.InlineSite, bool) {
	r := fb.r
	if pc >= len(fb.fn.Code) || fb.fn.Code[pc].Op != bytecode.OpApplyArguments || r.applyFn == nil {
		return ssa.InlineSite{}, false
	}
	t := r.jitForwardMethod(fb.cl, fb.fwd.ctor)
	if t == nil {
		return ssa.InlineSite{}, false
	}
	tc := t.fn().closure
	p, err := fb.r.jitLower(tc.fn, jitLowerInline)
	if err != nil {
		return ssa.InlineSite{}, false
	}
	this := -1
	if p.This {
		this = tc.fn.LocalCount + len(tc.fn.Upvalues)
	}
	k := fb.keep()
	for _, x := range p.Code {
		k.strings = k.strings || x.Op == ir.StringMethod || x.Op == ir.StringCode
	}
	k.holders = append(k.holders, t, r.applyFn)
	k.inlined = append(k.inlined, tc)
	return ssa.InlineSite{
		Program: p, Feedback: &jitFeedback{r: r, fn: tc.fn, cl: tc, root: k},
		Callee: uintptr(unsafe.Pointer(t)), Closure: uintptr(unsafe.Pointer(tc)),
		Argc: fb.fwd.argc, Method: true, Params: tc.fn.ParamCount, ThisSlot: this,
		Forward: true, Apply: uintptr(unsafe.Pointer(r.applyFn)),
	}, true
}

// jitInlineTarget is the call by in at pc of o, cl's function, to inline:
// a construction's with its pool.
func (r *Runtime) jitInlineTarget(pc int32, cl *closure, o *Object, in bytecode.Instr) jitInline {
	x := jitInline{pc: pc, cl: cl, obj: o}
	if in.Op == bytecode.OpNew {
		x.pool = r.jitNewPool(o)
		x.fwd = r.jitForwardMethod(cl, o)
	}
	return x
}

// jitForwardChanged reports whether the forwarding constructor inlined at
// pc in e's code, a construction of o, now forwards to a method other than
// the one the code was compiled for -- its prototype's was replaced -- and
// notes the new one: the code is compiled again for it, as V8 optimizes
// again for a call's new target, rather than the exits counted against
// inlining it (jitInlineLeft).
func (r *Runtime) jitForwardChanged(e *jitEntry, pc int, o *Object) bool {
	for i := range e.inlines {
		x := &e.inlines[i]
		if int(x.pc) != pc || x.obj != o || x.fwd == nil {
			continue
		}
		if t := r.jitForwardMethod(x.cl, o); t != nil && t != x.fwd {
			x.fwd = t
			return true
		}
	}
	return false
}

func (r *Runtime) jitNativeTarget(pc int32, cl *closure, o *Object, in bytecode.Instr) jitInline {
	x := jitInline{pc: pc, cl: cl, obj: o}
	if in.Op == bytecode.OpNew {
		x.pool = r.jitNewPool(o)
	}
	return x
}

// jitCalledVia is the function a method call at the top of the stack
// calls through Function.prototype.call, the realm's own, G.call(this,
// ...): G, its receiver, and its closure, if native code may call it, as
// jitCalled has it; or nil.
func (r *Runtime) jitCalledVia(f *frame, sp int, in bytecode.Instr) (*Object, *closure) {
	if in.Op != bytecode.OpCallMethod || in.A == 0 || r.callFn == nil {
		return nil, nil
	}
	callee, recv := r.stack[sp-int(in.A)-1], r.stack[sp-int(in.A)-2]
	if !callee.IsObject() || callee.Object() != r.callFn || !recv.IsObject() {
		return nil, nil
	}
	o := recv.Object()
	fd := o.fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || isGeneratorTemplate(fd.closure.fn) || fd.closure.realm != r.Realm ||
		fd.closure.scope() != f.cl.scope() {
		return nil, nil
	}
	return o, fd.closure
}

// jitPops is Array.prototype.pop, the realm's own, if a method call at the
// top of the stack calls it with no argument on an array its fast path
// pops -- dense, of the realm's prototype -- or nil.
func (r *Runtime) jitPops(sp int, in bytecode.Instr) *Object {
	if in.Op != bytecode.OpCallMethod || in.A != 0 {
		return nil
	}
	callee, recv := r.stack[sp-1], r.stack[sp-2]
	if !callee.IsObject() {
		return nil
	}
	fd := callee.Object().fn()
	if fd == nil || fd.elemOp != elemPop || fd.realm != r.Realm || r.plainArray(recv) == nil {
		return nil
	}
	return callee.Object()
}

// jitPushes is Array.prototype.push, the realm's own, if a method call at
// the top of the stack calls it with one argument on an array its fast
// path appends to -- dense, of the realm's prototype, which with its own
// has no element or indexed property (noInheritedIndices) -- or nil.
func (r *Runtime) jitPushes(sp int, in bytecode.Instr) *Object {
	if in.Op != bytecode.OpCallMethod || in.A != 1 {
		return nil
	}
	callee, recv := r.stack[sp-2], r.stack[sp-3]
	if !callee.IsObject() {
		return nil
	}
	fd := callee.Object().fn()
	if fd == nil || fd.elemOp != elemPush || fd.realm != r.Realm {
		return nil
	}
	if o := r.plainArray(recv); o == nil || !r.noInheritedIndices(o) {
		return nil
	}
	return callee.Object()
}

// jitAllocates is the built-in a construction at the top of the stack
// makes with nothing to run, which native code makes from a pool: `new
// Array()`, the realm's own; or nil.
func (r *Runtime) jitAllocates(sp int, in bytecode.Instr) *Object {
	if in.Op != bytecode.OpNew || in.A != 0 {
		return nil
	}
	if c := r.stack[sp-1]; c.IsObject() && c.Object() == r.proto.arrayCtor {
		return c.Object()
	}
	return nil
}

// jitConstructs reports whether native code may construct with o, `new
// o(...)`, as constructWithTarget would: a plain function of this realm,
// compiled, a base constructor with no fields to give its instances, its
// prototype its own data property.
func (r *Runtime) jitConstructs(o *Object) bool {
	fd := o.fn()
	return fd != nil && fd.closure != nil && fd.native == nil && !fd.bound && fd.ctorKind == ctorBase && fd.fieldInit == nil &&
		fd.closure.realm == r.Realm && o.class == ClassFunction && o.findOwn(atomPrototype) >= 0
}

// jitFillPool makes pool's objects for constructions with o, as
// constructWithTarget makes one -- o's prototype, its constructor's root
// shape, room for what its body adds -- while o's prototype is its own
// data property holding an object; the pool is left empty otherwise, and
// native code leaves every construction to Go.
func (r *Runtime) jitFillPool(pool *abi.ObjectPool, o *Object) {
	clear(pool.Objects[:])
	pool.Count, pool.Proto = 0, nil
	if pool.Size == 0 {
		pool.Size = abi.PoolSize
	}
	n := int(pool.Size)
	if o == r.proto.arrayCtor {
		// `new Array()`: an empty array, of the realm's prototype, with the
		// layout a property cache gives an array it meets (ensureShape),
		// which native code compares receivers' with: a method read on one
		// it made, a.push, would leave for Go to give it.
		for i := range n {
			a := r.newArrayFrom(nil)
			r.ensureShape(a)
			pool.Objects[i] = unsafe.Pointer(a)
		}
		pool.Count, pool.Proto = uint64(n), unsafe.Pointer(r.proto.array)
		return
	}
	first := r.jitPoolObject(o)
	if first == nil {
		return
	}
	pool.Objects[0] = unsafe.Pointer(first)
	for i := 1; i < n; i++ {
		pool.Objects[i] = unsafe.Pointer(r.jitPoolObject(o))
	}
	pool.Count, pool.Proto = uint64(n), unsafe.Pointer(first.proto)
}

// jitNewPool is a pool of objects for constructions with o, filled.
func (r *Runtime) jitNewPool(o *Object) *abi.ObjectPool {
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{ctor: o})}
	r.jitFillPool(pool, o)
	return pool
}

// jitPoolSource is what an object pool is filled from (abi.ObjectPool's
// Source): a construction's function, or a literal's root shape and how
// many fields or elements it has.
type jitPoolSource struct {
	ctor  *Object
	root  *shape
	n     int
	array bool
	lit   *bytecode.Constant
}

// jitRefillPool fills pool again, run out, with more objects (ssa's
// GoRefill), as an exit at its site has Go fill it, and reports whether it
// has one now: a construction's function whose prototype is no longer an
// object leaves it empty, and the site to Go.
func (r *Runtime) jitRefillPool(pool *abi.ObjectPool) bool {
	src := (*jitPoolSource)(pool.Source)
	if src == nil || pool.Count != 0 {
		return pool.Count != 0
	}
	pool.Size = min(2*max(pool.Size, abi.PoolSize), abi.PoolCapacity)
	if src.ctor != nil {
		r.jitFillPool(pool, src.ctor)
	} else {
		r.jitFillLiteral(&jitLiteral{root: src.root, pool: pool, n: src.n, array: src.array, lit: src.lit})
	}
	return pool.Count != 0
}

// jitPoolObject is an object for a construction with o, as jitFillPool
// makes them, or nil if o's prototype is not its own data property holding
// an object.
func (r *Runtime) jitPoolObject(o *Object) *Object {
	p := o.getOwnVisible(atomPrototype)
	fd := o.fn()
	if p == nil || p.flags&propAccessor != 0 || !p.value.IsObject() || fd == nil || fd.closure == nil {
		return nil
	}
	props := int(fd.closure.fn.ThisProps)
	root := r.shapes.ctorRoot(fd)
	if root != nil {
		props = max(props, int(root.slack))
	}
	obj := newLiteralObject(p.value.Object(), ClassObject, props)
	obj.shape = root
	return obj
}

// jitRefillPools fills, at a construction that left native code at pc,
// the pool of the function it constructs with, if native code constructs
// with it there and the pool is empty, or was made for another prototype;
// it reports whether it filled an empty one.
func (r *Runtime) jitRefillPools(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) (refilled bool) {
	c := r.stack[sp-int(in.A)-1]
	if !c.IsObject() {
		return false
	}
	o := c.Object()
	// Both lists, without making one of them: this runs at every exit at
	// a construction.
	for _, x := range jitTargets(e) {
		if int(x.pc) != pc || x.obj != o || x.pool == nil {
			continue
		}
		if x.pool.Count == 0 {
			// Run out: more next time.
			x.pool.Size = min(2*max(x.pool.Size, abi.PoolSize), abi.PoolCapacity)
			r.jitFillPool(x.pool, o)
			refilled = true
		} else if x.cl == nil {
			// A built-in's, whose prototype the realm fixed.
		} else if p := o.getOwnVisible(atomPrototype); p == nil || !p.value.IsObject() || unsafe.Pointer(p.value.Object()) != x.pool.Proto {
			r.jitFillPool(x.pool, o)
		}
	}
	return refilled
}

// jitNativeCallee is the entry of cl's function, compiled now if it is not,
// if a call may make it natively (NativeCalls), or nil: the new pipeline
// compiles it, with a frame of its own, as a call makes it. One LowerSSA
// refuses for leaving native code with no loop to pay for Go's entries to
// it is compiled for native callers alone, who pay none of that, from
// LowerSSAInline, and Go does not enter it (entrySlow). It is compiled
// here, not when a caller's code asks for it: that compile would share the
// caller's workspaces. Code native callers do not call for now, for
// leaving too often (notNative), is the entry all the same: a call's code
// reads where to call at each call, and leaves for Go while there is
// nowhere, until the callee is called natively again (jitRetryNative).
// One that does not compile is not tried again (calleeRefused), unless for
// want of code budget, which may change.
func (r *Runtime) jitNativeCallee(cl *closure) *jitEntry {
	fn := cl.fn
	if fn.TopLevel || fn.IsModule || fn.UsesArguments || fn.HasDirectEval || isGeneratorTemplate(fn) {
		// A call of a generator or an async function makes its object: its
		// body is not what a call runs.
		return nil
	}
	e := r.jitFor(cl)
	if e == nil || e.ssa != nil || e.deferred || e.calleeRefused {
		if e != nil && e.ssa != nil {
			return e
		}
		return nil
	}
	p, err := r.jitLower(fn, jitLowerInline)
	if err != nil {
		e.calleeRefused = true
		return nil
	}
	s := r.jit
	s.budget = r.jitBudget()
	if !s.makeRoom(len(fn.Code) * s.perInstr) {
		return nil
	}
	e.busy++
	defer func() { e.busy-- }()
	r.jitSeedCalls(cl, e, p)
	code, fb, err := r.compileSSA(fn, cl, p, e)
	if code == nil {
		if errors.Is(err, jit.ErrCodeBudget) {
			// It may fit once the cache has released code (jitForFunction).
			e.deferred, e.generation = true, s.generation
		} else {
			e.calleeRefused = true
		}
		return nil
	}
	e.setSSA(s, fn, p, code, fb)
	e.ssaCallee, e.entrySlow = true, true
	s.compiled++
	s.account(e)
	s.settle()
	r.jitWantedCallees(e, fb)
	return e
}

// jitWantedCallees compiles for native callers the callees the compile fb
// made calls to, in place of inlining them, that had no code (wanted), and
// has e's code compiled again to call them, once, from what remains of the
// budget for upgrades (jitInlineReoptimizations). Not inside another
// compile, whose workspaces a compile would share, nor a compile of these.
func (r *Runtime) jitWantedCallees(e *jitEntry, fb *jitFeedback) {
	if len(fb.wanted) == 0 || r.jit.seeding != 0 || e.upgradeReopts >= jitInlineReoptimizations {
		return
	}
	r.jit.seeding = jitSeedDepth
	defer func() { r.jit.seeding = 0 }()
	for _, cl := range fb.wanted {
		if r.jitNativeCallee(cl) != nil {
			e.upgradeReopt = true
		}
	}
}

// setSSA gives e the code the new pipeline compiled for fn from p, which
// native callers call through its link.
func (e *jitEntry) setSSA(s *jitState, fn *bytecode.Function, p *ir.Program, code *jit.SSACode, fb *jitFeedback) {
	e.ssa, e.this, e.ssaShapes, e.ssaHolders, e.fed, e.fedInlined = code, p.This, fb.shapes, fb.holders, fb.fed, fb.fedInlined
	e.ssaProg = p
	e.ssaInlinedAt, e.definesUnfed, e.ssaCells = fb.inlinedAt, fb.definesUnfed, fb.cells
	e.reoptBudget = uint32(max(jitReoptBudget, len(fn.Code)/jitReoptPer))
	e.ssaStrings, e.ssaInlined, e.ssaPools = fb.strings, fb.inlined, fb.pools
	s.setLinks(e, fb.links)
	e.ssaKeeps = max(e.ssaKeeps, fb.keeps)
	e.link.entry = code.EntryAddress(0)
	e.ssaLoop = jitLoopLength(fn)
	for _, in := range p.Code {
		e.ssaStrings = e.ssaStrings || in.Op == ir.StringMethod || in.Op == ir.StringCode
	}
	s.account(e)
}

// jitLowering is a function's programs for the new pipeline, each lowered
// once, as its kind says (jitLowerKind), and kept by the runtime: a
// compile asks for its function's and for each callee's it inlines, and a
// function is compiled again as it learns, and inlined into each of its
// callers -- lowering them again had been some 8% of a compile. A program
// is never changed once lowered. construct is jitcompile.SSAConstruct when
// they were. inlinable is jitInlinable's answer, once asked: 1 if the
// function may be inlined, 2 if not.
//
// bytes is what the lowering holds, its programs' (ir.Program's
// Footprint), which the JIT counts (jitState.lowerBytes); used the epoch
// it was last asked for in, as of the cache's last look (age). The cache
// drops the lowerings of functions it has no entry for to make room
// (makeRoom), the least recently used first, before any code.
type jitLowering struct {
	construct bool
	inlinable uint8
	done      [jitLowerKinds]bool
	progs     [jitLowerKinds]*ir.Program
	errs      [jitLowerKinds]error
	bytes     int
	used      uint64
}

// jitLowerKind is how a program is lowered: for a function's own code
// (jitcompile.LowerSSA), for a callee inlined (LowerSSAInline), for a
// forwarding constructor inlined (LowerSSAForward).
type jitLowerKind uint8

const (
	jitLowerSSA jitLowerKind = iota
	jitLowerInline
	jitLowerForward
	jitLowerKinds
)

// lowering is fn's lowering, made if the runtime has none, and found used
// now.
func (s *jitState) lowering(fn *bytecode.Function) *jitLowering {
	key := weak.Make(fn)
	l := s.lowered[key]
	if l == nil || l.construct != jitcompile.SSAConstruct {
		if s.lowered == nil {
			s.lowered = map[weak.Pointer[bytecode.Function]]*jitLowering{}
		}
		if l != nil {
			s.dropLowering(key, l)
		}
		l = &jitLowering{construct: jitcompile.SSAConstruct, bytes: jitLoweringFixed}
		s.lowered[key] = l
		s.lowerBytes += l.bytes
	}
	l.used = s.agedAt
	return l
}

// jitLower is fn's program, lowered as kind says, from the runtime's
// programs (jitLowering).
func (r *Runtime) jitLower(fn *bytecode.Function, kind jitLowerKind) (*ir.Program, error) {
	s := r.jit
	if s == nil {
		// Before the runtime has any state, which no compile is.
		s = &jitState{}
	}
	l := s.lowering(fn)
	if !l.done[kind] {
		switch kind {
		case jitLowerSSA:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSA(fn)
		case jitLowerInline:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAInline(fn)
		case jitLowerForward:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAForward(fn)
		}
		l.done[kind] = true
		n := l.progs[kind].Footprint()
		l.bytes += n
		s.lowerBytes += n
	}
	return l.progs[kind], l.errs[kind]
}

// jitInlinable reports whether fn may be inlined into a caller: it
// captures nothing, and ssa.Inlinable takes its program. Each function is
// asked once, while the runtime keeps its lowering.
func (r *Runtime) jitInlinable(fn *bytecode.Function) bool {
	l := r.jit.lowering(fn)
	if l.inlinable == 0 {
		ok := false
		if len(fn.Upvalues) == 0 && !fn.TopLevel && !fn.IsModule && !fn.UsesArguments && !fn.HasDirectEval {
			p, err := r.jitLower(fn, jitLowerInline)
			ok = err == nil && ssa.Inlinable(p)
		}
		l.inlinable = 2
		if ok {
			l.inlinable = 1
		}
	}
	return l.inlinable == 1
}

// Generic and EntryGeneric are ssa.Feedback's: the sites and entries whose
// guards failed in code compiled before (jitDeoptimized).
func (fb *jitFeedback) Generic(pc int) bool {
	return fb.e != nil && slices.Contains(fb.e.failed, int32(pc))
}

// WantsEntry is ssa.EntryFeedback's: Go has gone on in the interpreter,
// having found no entry, too often in the function's own code (jitGoOn).
func (fb *jitFeedback) WantsEntry(int) bool {
	return fb.e != nil && fb.root == nil && fb.fwd == nil && fb.e.allEntries
}

func (fb *jitFeedback) EntryGeneric(pc int) bool {
	return fb.e != nil && slices.Contains(fb.e.failedEntries, uint32(pc))
}

// Global is where the global a site reads was last found in the closure's
// scope (the site's idx, which the interpreter keeps there), or, before the
// interpreter has run the site, where the scope has the name now. Native
// code checks the key at that index, and, as the interpreter does, that no
// script-level lexical binding of the name shadows it (lexShadows). A name
// that one has, in the realm's global scope, is that binding's (lexGlobal).
// A module's code reads where its environment's binding is (moduleGlobal).
func (fb *jitFeedback) Global(pc int) (ssa.GlobalSite, bool) {
	if fb.cl == nil || pc >= len(fb.fn.Code) {
		return ssa.GlobalSite{}, false
	}
	in := fb.fn.Code[pc]
	write := in.Op == bytecode.OpSetGlobal || in.Op == bytecode.OpSetGlobalStrict
	check := in.Op == bytecode.OpCheckGlobalRef
	if in.Op != bytecode.OpGetGlobal && in.Op != bytecode.OpGetGlobalOpt && !write && !check || int(in.A) >= len(fb.cl.names) {
		return ssa.GlobalSite{}, false
	}
	name, env := fb.cl.names[in.A], fb.cl.scope()
	if env == nil {
		return ssa.GlobalSite{}, false
	}
	if !fb.r.isGlobalScope(env) {
		return fb.moduleGlobal(name, env, in, write, check)
	}
	if fb.r.lexShadows(name) {
		return fb.lexGlobal(name, env, write)
	}
	i := int32(-1)
	if int(in.B) < len(fb.cl.ic) {
		i = fb.cl.ic[in.B].idx
	}
	if i < 0 || int(i) >= len(env.props) || env.props[i].key != name {
		i = env.findOwn(name)
	}
	if i < 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i}
	if check {
		// A strict assignment's check: data, which the cell's guard
		// finds is there.
		if env.props[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		return site, true
	}
	if write {
		// An assignment: a writable data property, which the code
		// checks it still is.
		if p := &env.props[i]; p.isAccessor() || p.flags&propWritable == 0 {
			return ssa.GlobalSite{}, false
		}
		return site, true
	}
	if p := &env.props[i]; !p.isAccessor() && p.flags&(propWritable|propConfigurable) == 0 {
		switch {
		case p.value.IsUndefined():
			site.Fixed, site.Constant = true, ir.Value{Kind: ir.Undefined}
		case p.value.IsNumber():
			site.Fixed, site.Constant = true, ir.Float(p.value.Number())
		}
	}
	return site, true
}

// moduleGlobal is Global's site for name in a module's code, whose scope,
// env, is the module's environment, as V8 reads a module's variables in its
// context and an import in its exporter's cell: the bindings are fixed once
// the module is linked, none added or removed -- nothing but the module's
// own code reaches the environment -- so a site reads the object that has
// the binding, at its address (GlobalSite's Holder), with no check that
// the scope is the one it was compiled for. A binding of the environment's
// own is read there, before any of a script's lexical bindings, as the
// interpreter reads it; an import, an accessor, where the binding it names
// is (jitImportedBinding); and a name the module does not declare where the
// global object, the environment's prototype, has it, if no script-level
// lexical binding of the name shadows it. The rest is Go's.
func (fb *jitFeedback) moduleGlobal(name Atom, env *Object, in bytecode.Instr, write, check bool) (ssa.GlobalSite, bool) {
	i := int32(-1)
	if int(in.B) < len(fb.cl.ic) {
		i = fb.cl.ic[in.B].idx
	}
	if i < 0 || int(i) >= len(env.props) || env.props[i].key != name {
		i = env.findOwn(name)
	}
	holder := env
	shadowable := false
	switch {
	case i >= 0 && env.props[i].isAccessor():
		// An import, which only a read reads: an assignment to one throws.
		if write || check {
			return ssa.GlobalSite{}, false
		}
		h, key, ok := fb.r.jitImportedBinding(env, name)
		if !ok {
			return ssa.GlobalSite{}, false
		}
		if i = h.findOwn(key); i < 0 || h.props[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		holder, name = h, key
	case i < 0:
		g := env.proto
		if write || check || g == nil || fb.r.lexShadows(name) || proxyOf(g) != nil {
			return ssa.GlobalSite{}, false
		}
		if i = g.findOwn(name); i < 0 || g.props[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		holder, shadowable = g, true
	}
	p := &holder.props[i]
	if write && p.flags&propWritable == 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i, Holder: uintptr(unsafe.Pointer(holder)), Shadowable: shadowable}
	if !write && !check && p.flags&(propWritable|propConfigurable) == 0 {
		// A const, or an import of one, once it has its value; or a
		// global that never changes, undefined, NaN or Infinity.
		switch {
		case p.value.IsUndefined() && p.flags&propUninit == 0:
			site.Fixed, site.Constant = true, ir.Value{Kind: ir.Undefined}
		case p.value.IsNumber():
			site.Fixed, site.Constant = true, ir.Float(p.value.Number())
		}
	}
	return site, true
}

// jitImportedBinding is where the import name binds in a module's
// environment, env, reads its value: the binding of another module's
// environment, and its name there, as linking resolved it (bindImport,
// resolveExport) -- found again here, from the modules loaded already, and
// asking the host nothing; or false where it is not one this finds, a
// namespace's export or one a star re-exports, which Go reads.
func (r *Runtime) jitImportedBinding(env *Object, name Atom) (*Object, Atom, bool) {
	var m *Module
	for _, x := range r.modules {
		if x.env == env {
			m = x
			break
		}
	}
	if m == nil {
		return nil, 0, false
	}
	local := r.atoms.name(name)
	// The binding local of m is an import: where its module exports what
	// it names, re-exported along the way or not; and that is m's own
	// binding, or another import, followed in turn. A cycle, or a chain
	// past the steps, is Go's.
	for range 16 {
		i := slices.IndexFunc(m.imports, func(imp moduleImport) bool { return imp.local == local })
		if i < 0 {
			if m.env == env {
				return nil, 0, false
			}
			return m.env, r.atoms.intern(local), true
		}
		imp := m.imports[i]
		if imp.namespace {
			return nil, 0, false
		}
		src, want := r.jitLoadedModule(m, imp.specifier), imp.imported
		if imp.isDefault {
			want = "default"
		}
		for range 16 {
			if src == nil {
				return nil, 0, false
			}
			exported, ok := src.exports[want]
			if !ok || strings.HasPrefix(exported, nsExportPrefix) {
				return nil, 0, false
			}
			spec, imported, indirect := indirectSource(exported)
			if !indirect {
				m, local = src, exported
				break
			}
			// export { x } from "...".
			src, want = r.jitLoadedModule(src, spec), imported
		}
		if src == nil || m != src {
			return nil, 0, false
		}
	}
	return nil, 0, false
}

// jitLoadedModule is the module m's request of specifier loaded, as
// resolvedNameOf finds it without asking the host, or nil.
func (r *Runtime) jitLoadedModule(m *Module, specifier string) *Module {
	plain, _ := bytecode.SplitDeferRequest(specifier)
	plain, _ = bytecode.SplitSourceRequest(plain)
	if bare, typ := bytecode.SplitModuleRequest(plain); typ != "" || bare != specifier {
		return nil
	}
	return r.requested[[2]string{m.Specifier, specifier}]
}

// lexGlobal is Global's site for a script-level lexical binding of name,
// which a let, a const or a class declared: in the realm's global scope,
// env, where the interpreter reads it (globalLexProp). The binding stays
// where it is, and a const's value, once it has one, is fixed. A write
// to a const is Go's, which throws.
func (fb *jitFeedback) lexGlobal(name Atom, env *Object, write bool) (ssa.GlobalSite, bool) {
	lex := fb.r.globalLex
	if !fb.r.isGlobalScope(env) {
		return ssa.GlobalSite{}, false
	}
	i := lex.findOwn(name)
	if i < 0 {
		return ssa.GlobalSite{}, false
	}
	p := &lex.props[i]
	if write && p.flags&propWritable == 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i, Lexical: true}
	if !write && p.flags&propWritable == 0 && p.value.IsNumber() {
		site.Fixed, site.Constant = true, ir.Float(p.value.Number())
	}
	return site, true
}

// StringProperty is ssa.StringFeedback's: where String.prototype has the
// method a read at pc names, for a read whose receivers have been strings
// (jitEntry.strSites) and objects never -- one that has met objects reads
// them as their cache says. A read whose cache knows nothing is taken for
// a string's, as V8 takes what its feedback says, if String.prototype has
// the method and Object.prototype does not: the interpreter's cache holds
// no string, so a string's method read would otherwise be learned at an
// exit, and the function compiled again, at each such read in turn --
// RegExp's run blocks, of hundreds of instructions, had dozens. An object
// that meets it leaves native code once, and the read is compiled again
// for its shape (jitPolySeen), as for a cache that knew nothing. The code
// checks the receiver is a string, and String.prototype's shape and the
// key at the index.
func (fb *jitFeedback) StringProperty(pc int) (ssa.PropertySite, bool) {
	e := fb.e
	if e == nil || fb.cl == nil || pc >= len(fb.fn.Code) {
		return ssa.PropertySite{}, false
	}
	in := fb.fn.Code[pc]
	if in.Op != bytecode.OpGetPropThis || int(in.A) >= len(fb.cl.names) || int(in.B) >= len(fb.cl.ic) {
		return ssa.PropertySite{}, false
	}
	c := &fb.cl.ic[in.B]
	if c.shape != nil && c.shape != noShape || slices.ContainsFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc }) {
		return ssa.PropertySite{}, false
	}
	key, proto := fb.cl.names[in.A], fb.r.proto.str
	if key.IsIndex() || key == atomLength || proto == nil {
		return ssa.PropertySite{}, false
	}
	if !slices.Contains(e.strSites, uint32(pc)) &&
		(c.fills != 0 || fb.r.proto.object == nil || fb.r.proto.object.findOwn(key) >= 0) {
		return ssa.PropertySite{}, false
	}
	sh := fb.r.ensureProtoShape(proto)
	i := proto.findOwn(key)
	if sh == nil || sh == noShape || i < 0 || proto.props[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return ssa.PropertySite{}, false
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(sh))
	return ssa.PropertySite{Key: uint32(key), Shape: uintptr(unsafe.Pointer(sh)), Index: i}, true
}

func (fb *jitFeedback) Property(pc int) (ssa.PropertySite, bool) {
	if fb.cl != nil && pc < len(fb.fn.Code) {
		if in := fb.fn.Code[pc]; in.Op == bytecode.OpGetPrivate || in.Op == bytecode.OpSetPrivate || in.Op == bytecode.OpPrivateIn {
			return fb.privateProperty(pc, in)
		}
	}
	site, ok := fb.property(pc)
	deep := false
	if ok && fb.fwd == nil && site.Shape == 0 && site.Holders[0].Object == 0 {
		// A read its cache could not hold, the property past the two
		// prototypes it holds: compiled for the first of the shapes it met,
		// the others its cases.
		if in := fb.fn.Code[pc]; in.Op != bytecode.OpSetProp && int(in.B) < len(fb.cl.ic) && (fb.cl.ic[in.B].shape == nil || fb.cl.ic[in.B].shape == noShape) {
			if cases := fb.deepCases(pc); len(cases) != 0 {
				k := cases[0]
				site.Shape, site.Index, site.Holders, site.More, site.Cases = k.Shape, k.Index, k.Holders, k.More, cases[1:]
				deep = true
			}
		}
	}
	if ok && fb.root != nil && fb.fwd == nil && site.Shape == 0 && site.Add == nil && site.Holders[0].Object == 0 &&
		int(fb.fn.Code[pc].B) < len(fb.cl.ic) && fb.cl.ic[fb.fn.Code[pc].B].fills < maxCacheFills {
		// An inlined callee's site its cache knew nothing of (jitInlineFed).
		k := fb.keep()
		k.fedInlined = append(k.fedInlined, jitFedInlined{cl: fb.cl, pc: uint32(pc)})
	}
	if ok && fb.e != nil && fb.root == nil {
		if fb.fn.Code[pc].Op == bytecode.OpSetProp {
			site.Cases, site.Adds = fb.writeCases(pc, site)
		} else if site.Shape != 0 && !deep {
			site.Cases = fb.cases(pc, site.Shape)
		}
	}
	if ok && fb.root == nil && int(fb.fn.Code[pc].B) < len(fb.cl.ic) && fb.cl.ic[fb.fn.Code[pc].B].fills < maxCacheFills &&
		!slices.ContainsFunc(fb.fed, func(s jitFedSite) bool { return s.pc == uint32(pc) }) {
		// A site whose cache may yet learn a shape (jitFed).
		// The cache's shape, though the code may not use it (a getter's,
		// a write that adds the property): what changing it says; or,
		// for one it knew nothing of, the shape the code was compiled
		// for (chainSite), which its filling with says nothing new.
		known := uintptr(0)
		if c := fb.cl.ic[fb.fn.Code[pc].B].shape; c != noShape {
			known = uintptr(unsafe.Pointer(c))
		}
		if known == 0 && !deep {
			known = site.Shape
		}
		fb.fed = append(fb.fed, jitFedSite{pc: uint32(pc), shape: known})
	}
	return site, ok
}

// writeCases are what the write at pc met at objects of shapes other than
// site's (jitPolyWriteSeen): an own property it wrote, or a transition it
// added the property along (fb.add), which the code tries in turn.
func (fb *jitFeedback) writeCases(pc int, site ssa.PropertySite) ([]ssa.PropertyCase, []*ssa.PropertyAdd) {
	var cases []ssa.PropertyCase
	var adds []*ssa.PropertyAdd
	for _, p := range fb.e.poly {
		if int(p.pc) != pc {
			continue
		}
		for i := range p.cases {
			c := &p.cases[i]
			from := uintptr(unsafe.Pointer(c.shape))
			if from == site.Shape || site.Add != nil && from == site.Add.From {
				continue
			}
			if c.next != nil {
				if a := fb.add(c); a != nil {
					adds = append(adds, a)
				}
				continue
			}
			if c.p1 == nil && c.idx >= 0 && c.idx < c.shape.n {
				k := fb.keep()
				k.shapes = append(k.shapes, remember(c.shape))
				cases = append(cases, ssa.PropertyCase{Shape: from, Index: c.idx})
			}
		}
		for _, d := range p.adds {
			if from := uintptr(unsafe.Pointer(d.from)); from == site.Shape || site.Add != nil && from == site.Add.From {
				continue
			}
			if d.next.index == nil && int(d.next.n) > linearScanLimit {
				// Its next shape wants its table's index built, as fb.add
				// leaves to Go.
				continue
			}
			k := fb.keep()
			k.shapes = append(k.shapes, remember(d.from), remember(d.next))
			a := &ssa.PropertyAdd{From: uintptr(unsafe.Pointer(d.from)), Next: uintptr(unsafe.Pointer(d.next)), Flags: uint8(d.next.flags)}
			for j, up := range d.up {
				h := ssa.Holder{Object: uintptr(unsafe.Pointer(up)), Shape: uintptr(unsafe.Pointer(d.shapes[j]))}
				k.shapes, k.holders = append(k.shapes, remember(d.shapes[j])), append(k.holders, up)
				if j < len(a.Protos) {
					a.Protos[j] = h
				} else {
					a.More = append(a.More, h)
				}
			}
			adds = append(adds, a)
		}
	}
	return cases, adds
}

// cases are the shapes other than first the read at pc met (jitPolySeen),
// as ssa.PropertyCase has them: the code keeps them and their holders.
func (fb *jitFeedback) cases(pc int, first uintptr) []ssa.PropertyCase {
	var cases []ssa.PropertyCase
	for _, p := range fb.e.poly {
		if int(p.pc) != pc {
			continue
		}
		for _, c := range p.cases {
			if uintptr(unsafe.Pointer(c.shape)) == first {
				continue
			}
			k := ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(c.shape)), Index: c.idx}
			fb.shapes = append(fb.shapes, c.shape)
			for i, h := range [2]struct {
				p *Object
				s *shape
			}{{c.p1, c.s1}, {c.p2, c.s2}} {
				if h.p == nil {
					break
				}
				k.Holders[i] = ssa.Holder{Object: uintptr(unsafe.Pointer(h.p)), Shape: uintptr(unsafe.Pointer(h.s))}
				fb.shapes, fb.holders = append(fb.shapes, h.s), append(fb.holders, h.p)
			}
			cases = append(cases, k)
		}
		for i := range p.reads {
			if d := &p.reads[i]; uintptr(unsafe.Pointer(d.shape)) != first {
				cases = append(cases, fb.deepCase(d))
			}
		}
	}
	return cases
}

// deepCase is a read's case past what a cache holds (jitDeepRead), whose
// shapes and prototypes the code keeps.
func (fb *jitFeedback) deepCase(d *jitDeepRead) ssa.PropertyCase {
	k := ssa.PropertyCase{Shape: uintptr(unsafe.Pointer(d.shape)), Index: d.idx}
	keep := fb.keep()
	keep.shapes = append(keep.shapes, d.shape)
	for i, up := range d.up {
		h := ssa.Holder{Object: uintptr(unsafe.Pointer(up)), Shape: uintptr(unsafe.Pointer(d.shapes[i]))}
		keep.shapes, keep.holders = append(keep.shapes, d.shapes[i]), append(keep.holders, up)
		if i < len(k.Holders) {
			k.Holders[i] = h
		} else {
			k.More = append(k.More, h)
		}
	}
	return k
}

// deepCases are the cases of the read at pc, whose cache knew nothing --
// none it could hold, the property found past its two prototypes -- as
// e's exits learned them (cases), and as the cache noted them
// (jitNoteDeepRead), for an inlined callee's site too: up to
// jitPropertyCases.
func (fb *jitFeedback) deepCases(pc int) []ssa.PropertyCase {
	var cases []ssa.PropertyCase
	if fb.e != nil && fb.root == nil {
		cases = fb.cases(pc, 0)
	}
	if in := fb.fn.Code[pc]; int(in.B) < len(fb.cl.ic) && fb.cl.ic[in.B].seen != nil {
		reads := fb.cl.ic[in.B].seen.reads
		for i := range reads {
			d := &reads[i]
			if len(cases) < jitPropertyCases && !slices.ContainsFunc(cases, func(k ssa.PropertyCase) bool { return k.Shape == uintptr(unsafe.Pointer(d.shape)) }) {
				cases = append(cases, fb.deepCase(d))
			}
		}
	}
	return cases
}

func (fb *jitFeedback) property(pc int) (ssa.PropertySite, bool) {
	if fb.cl == nil || pc >= len(fb.fn.Code) {
		return ssa.PropertySite{}, false
	}
	in := fb.fn.Code[pc]
	if in.Op != bytecode.OpGetProp && in.Op != bytecode.OpGetPropThis && in.Op != bytecode.OpSetProp || int(in.A) >= len(fb.cl.names) {
		return ssa.PropertySite{}, false
	}
	if fb.fwd != nil {
		return fb.forwardProperty(pc)
	}
	if int(in.B) >= len(fb.cl.ic) {
		return ssa.PropertySite{Key: uint32(fb.cl.names[in.A])}, true
	}
	site, ok := fb.siteFrom(in, &fb.cl.ic[in.B])
	if c := &fb.cl.ic[in.B]; ok && (c.shape == nil || c.shape == noShape) && c.fills == 0 && in.Op != bytecode.OpSetProp {
		site = fb.chainSite(pc, site)
	}
	return site, ok
}

// chainSite is the site of the read at pc, whose cache knows nothing yet,
// for the object the reads before it name (jitChainReceiver) -- Math, for
// Math.pow -- if the property is its own data property: compiled for the
// object's shape and the property's index now, as a cache filled by a read
// of it would have it, and checked as such a site is. V8 folds a load from
// an object it knows so. Compiled knowing nothing, the read searched the
// object's table, which gives up past abi.MaxScan properties: Math's
// methods left native code at every read, and each compile learned one
// more site, until the compiles ran out.
//
// Or the object a captured binding read just before it holds now, or one
// like a regular expression literal's (jitKnownReceiver), and the property
// found on it or on its prototypes,
// as a cache filled by a read of it would have it: re0.exec, in the second
// of RegExp's runBlock0's two dozen loops, had no feedback when the first
// loop compiled the function, left native code until it had, and had the
// function compiled again -- once for each loop, fourteen compiles.
func (fb *jitFeedback) chainSite(pc int, site ssa.PropertySite) ssa.PropertySite {
	p := fb.program()
	if p == nil || len(p.Maps) != len(fb.fn.Code) {
		return site
	}
	in := fb.fn.Code[pc]
	if recv, ok := fb.r.jitKnownReceiver(fb.cl, p, pc); ok {
		// Its cache as a read of the object fills it, not the site's own,
		// which the interpreter fills.
		var c propCache
		fb.r.fillPropCache(&c, recv, fb.cl.names[in.A], false)
		if c.shape == nil || c.shape == noShape || c.getter || c.idx < 0 {
			return site
		}
		if s, ok := fb.siteFrom(in, &c); ok {
			return s
		}
		return site
	}
	recv, ok := fb.r.jitChainReceiver(fb.cl, p, pc, jitChainReads)
	if !ok {
		return site
	}
	sh := recv.shape
	if sh == nil || sh == noShape {
		return site
	}
	i := recv.findOwn(fb.cl.names[in.A])
	if i < 0 || i >= sh.n || int(i) >= len(recv.props) || recv.props[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return site
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(sh))
	site.Shape, site.Index = uintptr(unsafe.Pointer(sh)), i
	return site
}

// jitKnownReceiver is the object the property read at q of cl's code
// reads from, as the instruction just before it gives it: the object a
// captured binding holds now, or a RegExp a regular expression literal
// makes, one like those it makes -- a guess, which the read's checks test.
func (r *Runtime) jitKnownReceiver(cl *closure, p *ir.Program, q int) (*Object, bool) {
	fn := cl.fn
	if q <= 0 || q >= len(fn.Code) || p.Maps[q].Depth < 0 || p.Maps[q-1].Depth != p.Maps[q].Depth-1 {
		return nil, false
	}
	var v Value
	switch prev := fn.Code[q-1]; prev.Op {
	case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck:
		if int(prev.A) >= len(cl.upvalues) || cl.upvalues[prev.A] == nil {
			return nil, false
		}
		v = cl.upvalues[prev.A].get()
	case bytecode.OpNewRegExp:
		if int(prev.A) >= len(fn.Constants) || fn.Constants[prev.A].Kind != bytecode.ConstRegExp {
			return nil, false
		}
		var err error
		if v, err = r.newRegExpLiteral(&fn.Constants[prev.A]); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	if !v.IsObject() {
		return nil, false
	}
	return v.Object(), true
}

// program is the slot IR program of fb's function, for what its
// instructions' depths say: lowered once, as the inlining's is.
func (fb *jitFeedback) program() *ir.Program {
	if fb.prog == nil {
		p, err := fb.r.jitLower(fb.fn, jitLowerInline)
		if err != nil {
			if p, err = fb.r.jitLower(fb.fn, jitLowerSSA); err != nil {
				return nil
			}
		}
		fb.prog = p
	}
	return fb.prog
}

// siteFrom is the site of a property instruction, in, as cache c knows it.
func (fb *jitFeedback) siteFrom(in bytecode.Instr, c *propCache) (ssa.PropertySite, bool) {
	return fb.siteOf(fb.cl.names[in.A], in.Op == bytecode.OpSetProp, c)
}

// siteOf is the site of a read of key, or a write, as cache c knows it.
func (fb *jitFeedback) siteOf(key Atom, write bool, c *propCache) (ssa.PropertySite, bool) {
	site := ssa.PropertySite{Key: uint32(key)}
	k := fb.keep()
	if write && c.next != nil && c.next != setterNext && !c.getter {
		site.Add = fb.add(c)
		return site, true
	}
	if c.shape == nil || c.shape == noShape || c.getter || c.next != nil || c.idx < 0 {
		return site, true
	}
	if c.p1 == nil {
		if c.idx < c.shape.n {
			k.shapes = append(k.shapes, remember(c.shape))
			site.Shape, site.Index = uintptr(unsafe.Pointer(c.shape)), c.idx
		}
		return site, true
	}
	// Found on a prototype (propCache.holder): the receiver's shape, then
	// each prototype's, which the code compares as the cache does, and
	// the index in the last one's table. A read only: a write makes a
	// property of the receiver's own.
	last := c.s1
	if c.p2 != nil {
		last = c.s2
	}
	if write || c.s1 == nil || c.s1 == noShape || c.p2 != nil && (c.s2 == nil || c.s2 == noShape) || c.idx >= last.n {
		return site, true
	}
	k.shapes = append(k.shapes, remember(c.shape), remember(c.s1))
	k.holders = append(k.holders, c.p1)
	site.Holders[0] = ssa.Holder{Object: uintptr(unsafe.Pointer(c.p1)), Shape: uintptr(unsafe.Pointer(c.s1))}
	if c.p2 != nil {
		k.shapes = append(k.shapes, remember(c.s2))
		k.holders = append(k.holders, c.p2)
		site.Holders[1] = ssa.Holder{Object: uintptr(unsafe.Pointer(c.p2)), Shape: uintptr(unsafe.Pointer(c.s2))}
	}
	site.Shape, site.Index = uintptr(unsafe.Pointer(c.shape)), c.idx
	return site, true
}

// add is what a write whose cache adds its property adds (propCache.adds),
// as ssa.PropertyAdd has it, or nil for one native code leaves to Go: one
// whose next shape wants its table's index built (appendTransition). The
// code keeps the shapes and prototypes it compares.
func (fb *jitFeedback) add(c *propCache) *ssa.PropertyAdd {
	next := c.next
	if c.shape == nil || c.shape == noShape || next.index == nil && int(next.n) > linearScanLimit {
		return nil
	}
	add := &ssa.PropertyAdd{From: uintptr(unsafe.Pointer(c.shape)), Next: uintptr(unsafe.Pointer(next)), Flags: uint8(next.flags)}
	for i, h := range [2]struct {
		p *Object
		s *shape
	}{{c.p1, c.s1}, {c.p2, c.s2}} {
		if h.p == nil {
			break
		}
		if h.s == nil || h.s == noShape {
			return nil
		}
		add.Protos[i] = ssa.Holder{Object: uintptr(unsafe.Pointer(h.p)), Shape: uintptr(unsafe.Pointer(h.s))}
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(c.shape), remember(next))
	for i, h := range [2]struct {
		p *Object
		s *shape
	}{{c.p1, c.s1}, {c.p2, c.s2}} {
		if add.Protos[i].Object != 0 {
			k.shapes, k.holders = append(k.shapes, remember(h.s)), append(k.holders, h.p)
		}
	}
	return add
}

// jitSlot is a frame's slot as the JIT numbers them: its locals, its
// captured bindings, the receiver if the code reads it (from the context,
// where Go put it), then its operands.
func (r *Runtime) jitSlot(f *frame, e *jitEntry, i int) *Value {
	n, u := f.cl.fn.LocalCount, len(f.cl.upvalues)
	switch {
	case i < n:
		return &f.locals[i]
	case i < n+u:
		return f.cl.upvalues[i-n].slot
	case e.this && i == n+u:
		return (*Value)(unsafe.Pointer(&r.jit.ssaCtx.This))
	}
	if e.this {
		u++
	}
	return &r.stack[f.base+i-n-u]
}

// jitStringMethod is String.prototype.charCodeAt for native code: the
// intrinsic, while the prototype still has it as it was made; and
// otherwise nothing.
func (r *Runtime) jitStringMethod() Value {
	if r.jitCharCodeAt.ref == nil {
		return Value{}
	}
	s := r.jit
	if !s.charCodeAtKnown {
		s.charCodeAtKey, s.charCodeAtKnown = r.atoms.intern("charCodeAt"), true
	}
	m := r.proto.str.getOwn(s.charCodeAtKey)
	if m == nil || m.flags&^propDefault != 0 || !m.value.StrictEquals(r.jitCharCodeAt) {
		return Value{}
	}
	return r.jitCharCodeAt
}

// jitSource is the value a run-time source names (internal/jit/ssa,
// origin.go), which word holds as native code wrote it: the value's
// address -- a slot's in the frame, the receiver's in a context, a captured
// binding's, or a heap cell's native code loaded a reference from -- or nil
// for a primitive's 0. It still holds the reference: native code stores no
// pointer, and nothing else has run since it read it. Reading the address
// back as a pointer is outside unsafe.Pointer's documented rules, and sound
// while Go's heap does not move (internal/jit/README.md, "Reading the heap
// in place").
func jitSource(word *uint64) *Value {
	if *word == 0 {
		return nil
	}
	return *(**Value)(unsafe.Pointer(word))
}

// jitApplyRecords writes the slots an exit left to Go (abi.Record): the
// references it moved, and the primitives it put where a reference was.
// The slots records read hold their values from entry until the first
// write, so every one is read first; one record, the usual case, reads its
// value and writes it.
func (r *Runtime) jitApplyRecords(f *frame, e *jitEntry, ctx *abi.Context) {
	n := int(ctx.Records)
	if n == 0 {
		return
	}
	r.jit.ssaRecords += uint64(n)
	const flags = abi.RecordScalar | abi.RecordMaybe | abi.RecordDirect
	if n == 1 {
		rec := &ctx.Record[0]
		*r.jitSlot(f, e, int(rec.Slot&^flags)) = r.jitRecordValue(f, e, ctx, 0)
		return
	}
	var buf [8]Value
	src := buf[:0]
	for i := range ctx.Record[:n] {
		src = append(src, r.jitRecordValue(f, e, ctx, i))
	}
	for i, rec := range ctx.Record[:n] {
		*r.jitSlot(f, e, int(rec.Slot&^flags)) = src[i]
	}
}

// jitRecordValue is the value a record writes, read from the frame as the
// exit left it.
func (r *Runtime) jitRecordValue(f *frame, e *jitEntry, ctx *abi.Context, i int) Value {
	switch rec := &ctx.Record[i]; {
	case rec.Slot&abi.RecordDirect != 0:
		slot := abi.Slot{Num: rec.Word, Ref: ctx.RecordRef[i]}
		return *(*Value)(unsafe.Pointer(&slot))
	case rec.Slot&abi.RecordScalar != 0:
		return Value{num: math.Float64frombits(rec.Word)}
	case rec.Slot&abi.RecordMaybe != 0:
		if s := jitSource(&rec.Arg); s != nil && s.ref != nil {
			return *s
		}
		return Value{num: math.Float64frombits(rec.Word)}
	default:
		return *r.jitSlot(f, e, int(rec.Arg))
	}
}

// jitSSAStats counts what one function's new-pipeline code does. Each
// entry starts a native stretch, which ends in a return, an exit to Go or
// a failed guard (ended counts those two), or a poll; work estimates, in
// slot IR instructions, what the stretches did natively.
type jitSSAStats struct {
	entries, hosts, guards, polls, records uint64
	ended, work                            uint64
	// goCalls counts the calls native code had Go make (jitGoCall).
	goCalls uint64
	// calleeGoCalls counts those it made as a native callee
	// (jitGoCallNested), and calleeWork the work it did natively before
	// each, as jitSSAProfit estimates a stretch's (jitCalleeProfit), since
	// its calls in (jitLink's in) were last counted from 0.
	calleeGoCalls, calleeWork uint64
	// judged is how many stretches, calls of Go included, there had been
	// when jitSSAProfit last judged the code.
	judged uint64
	// guardsAt counts failed guards by their site (abi.Context.ExitSite:
	// the slot IR PC of the operation, or -1 for an entry's speculation);
	// nil until one fails.
	guardsAt map[int32]uint64
}

// jitSSAProbe is how many native stretches a function runs between looks
// at whether its native code pays (jitSSAProfit), and jitSSAMinWork the
// work a stretch must do, on average, for it to: an entry, and the exit or
// return that ends the stretch, cost what the tree tier takes for about
// that many instructions (the V8 suite's medians of five: 10 left Richards'
// task methods, called from the tree tier and calling out again, native at
// a loss; 20 is best overall, 1,374.8 ms against 1,413.6).
const (
	jitSSAProbe   = 64
	jitSSAMinWork = 20
)

// jitSSAProfit accounts for a native stretch that started at pc, with the
// back-edge counter at edges, and ended as ctx says; every jitSSAProbe
// stretches it decides whether the function's native code pays. Code whose
// stretches do little work before they end -- leaving for Go, a method
// call at every iteration, or returning, a small method the tree tier calls
// that calls out again -- costs more there than it saves, and the tree tier
// runs it from then on. The work is estimated: the distance from the entry
// to the exit or return (the return's block), in slot IR instructions, and
// a loop's mean length for each back-edge taken natively.
func (r *Runtime) jitSSAProfit(e *jitEntry, ctx *abi.Context, start, edges int) {
	st := &e.ssaStats
	if ctx.ExitMarking != 0 {
		// A stretch the collector's marking ended -- at a native call Go
		// cannot make for it (mir's markingCall) -- says nothing
		// of the code, as its exits do not elsewhere (jitInlineLeft,
		// jitUnwound): through a long marking every iteration left, and
		// Go stopped entering the code for good, nothing learned to
		// compile it again for. Any other exit then counts: one that left
		// for Go anyway, while the collector happened to mark, is the
		// code's.
		st.entries--
		r.jit.markingExits++
		return
	}
	work := uint64(0)
	if back := edges - (r.backEdges - r.jit.pollCredit); back > 0 {
		work = uint64(back) * uint64(e.ssaLoop)
	}
	if ctx.ExitKind == abi.ExitHost || ctx.ExitKind == abi.ExitDeopt || ctx.ExitKind == abi.ExitReturn {
		if ctx.ExitKind != abi.ExitReturn {
			st.ended++
		}
		if site := int64(ctx.ExitSite); site > int64(start) {
			work += uint64(site - int64(start))
		}
	}
	st.work += work
	// A call of Go a stretch makes costs it what an exit and an entry
	// would, which it saves (ssa.GoCall): each counts as a stretch, so a
	// method whose call Go now makes is judged as it was when it left --
	// and as often: every jitSSAProbe stretches, its calls of Go among
	// them. Judged every jitSSAProbe entries, a function entered a few
	// times, each a loop of a hundred thousand calls of built-ins --
	// RegExp's run blocks -- was never judged.
	stretches := st.entries + st.goCalls
	if stretches-st.judged < jitSSAProbe {
		return
	}
	st.judged = stretches
	if st.work < stretches*jitSSAMinWork {
		// Go stops entering it; native callers, which pay neither, go on
		// calling it (NativeCalls).
		e.entrySlow = true
	}
}

// jitLoopLength is the mean length of fn's loops, in instructions: the
// distance each backward branch jumps; 1 for a function with none.
func jitLoopLength(fn *bytecode.Function) uint32 {
	total, loops := 0, 0
	for pc, in := range fn.Code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			if int(in.A) <= pc {
				total += pc - int(in.A) + 1
				loops++
			}
		}
	}
	if loops == 0 {
		return 1
	}
	return uint32(total / loops)
}

// jitGoOn has the interpreter run the rest of an invocation whose native
// code Go goes on in at pc no more: code that does not pay (entrySlow),
// as a deoptimized invocation is run; or code with no entry at pc, which
// is compiled for none after a guard, which seldom fails (ssa's
// EntryFeedback). The interpreter then runs only to the next backward
// jump of the frame's own, the interpreter's check made due there, where
// Go goes on natively, at the loop's header, in this run (again, at
// goOnPC with goOnDepth operands; tryJITLoop): an iteration that left as
// V8's code deoptimizes leaves the rest of that iteration to the
// interpreter, not the rest of the loop. Code Go has gone on without an
// entry in jitEntryMisses times is compiled again at once with an entry
// after every guard that may leave for Go (allEntries), as every entry
// had been, and Go goes on in it at pc.
func (r *Runtime) jitGoOn(f *frame, e *jitEntry, pc, sp int) (v Value, err error, done, again bool) {
	if e.entrySlow {
		v, err, done = r.jitInterpret(f, sp, nil)
		return v, err, done, false
	}
	s := r.jit
	// A miss that taught the code what it is to be compiled again for --
	// a shape, a call's target -- says little of the misses to come: the
	// guard it left at may never fail again.
	if !e.allEntries && !e.reoptPending() && !e.feedReopt && !e.polyPending && !e.inlinePending {
		if e.entryMisses++; e.entryMisses >= jitEntryMisses {
			// Compiled again for its entries alone, the code does what it
			// did: its stretches end where they did, and it is judged by
			// them as by those to come (jitSSAProfit).
			e.allEntries = true
			stats := e.ssaStats
			r.jitReoptimize(f.cl, e)
			e.ssaStats = stats
			// Calls in are counted afresh (jitLink's in), and the calls of Go
			// they are weighed against with them (jitCalleeProfit).
			e.ssaStats.calleeGoCalls, e.ssaStats.calleeWork = 0, 0
			if e.ssa.HasEntry(pc) && !e.entrySlow {
				s.goOnPC, s.goOnDepth = pc, sp-f.base
				return Undefined, nil, false, true
			}
		}
	}
	s.interpreted++
	defer func(f *frame) { s.goOn = f }(s.goOn)
	s.goOn = f
	r.backEdges = 1
	v, err = r.executeAt(f, sp, nil)
	if err == errGoOnNatively {
		s.wentOn++
		return Undefined, nil, false, true
	}
	return v, err, true, false
}

// jitEntryMisses is how often Go goes on in the interpreter, having found
// no entry, before a function's code is compiled with every one (jitGoOn).
const jitEntryMisses = 2

// errGoOnNatively ends the interpreter's run of a frame Go goes on in
// natively, at a loop's header (jitGoOn).
var errGoOnNatively = errors.New("jit: go on natively")

// runSSA runs a function compiled by the new pipeline from pc, where the
// frame has depth operands, until it returns or leaves native code for the
// rest of the invocation.
func (r *Runtime) runSSA(f *frame, e *jitEntry, pc, depth int) (Value, error, bool) {
	if !e.ssa.HasEntry(pc) {
		return Undefined, nil, false
	}
	s := r.jit
	idx := s.ctxTop
	if idx >= len(s.ssaCtxs)-1 && !r.jitGrowContexts() {
		// As deep in native code, through Go, as there are contexts.
		return Undefined, nil, false
	}
	return r.runSSAIn(f, e, pc, depth, idx, 0)
}

// runSSAIn is runSSA in context idx, from the entry at pc, or, if resume is
// not 0, where the code goes on after the native call it made in that
// context, whose callee Go finished, its result and frame in the context
// past (jitUnwindNative).
func (r *Runtime) runSSAIn(f *frame, e *jitEntry, pc, depth, idx int, resume uintptr) (Value, error, bool) {
	s := r.jit
	top := s.ctxTop
	s.ctxTop = idx + 1
	e.ssaRuns++
	// One return, so that the defer is open-coded: a deferred call's record
	// cost every entry more than the rest of entering.
	defer r.jitRunDone(e, idx, top)
	return r.runSSALoop(f, e, pc, depth, idx, resume)
}

// jitRunDone ends a run runSSAIn began in context idx, ctxTop top before
// it. What native code kept (abi.Context.Keep), and the pointer words its
// native calls recorded (RecordRef), its callees' too, are not kept past
// it; nor its receiver and captured bindings, nor what its native calls'
// callees ran with -- closure, receiver, result, keep cells -- in the
// contexts past it, each a frame gone once it returned, as V8's are: one
// receiver left there kept a benchmark's whole object graph alive after it
// ended. The contexts the calls ran in go from the one past it, each given
// a closure by the call, up to the first without, which no call ran in
// since Go last cleared it here (a run of Go's there gives it none, and
// clears those past it itself): none past that is looked at.
func (r *Runtime) jitRunDone(e *jitEntry, idx, top int) {
	s := r.jit
	e.ssaRuns--
	s.ctxTop = top
	if idx < len(s.goRuns) {
		s.goRuns[idx] = jitGoRun{}
	}
	c := s.ssaCtxs[idx]
	clear(c.Keep[:e.ssaKeeps])
	c.This, c.Upvalues = abi.Slot{}, nil
	if c.RecordHigh != 0 {
		clear(c.RecordRef[:c.RecordHigh])
		c.RecordHigh = 0
	}
	for i := idx + 1; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
		if c.Closure == nil {
			break
		}
		if c.RecordHigh != 0 {
			clear(c.RecordRef[:c.RecordHigh])
			c.RecordHigh = 0
		}
		clear(c.Keep[:s.ssaKeeps])
		c.Closure, c.Upvalues = nil, nil
		c.This, c.RetValue = abi.Slot{}, abi.Slot{}
	}
}

// runSSALoop is runSSAIn's work, between its run's start and end.
func (r *Runtime) runSSALoop(f *frame, e *jitEntry, pc, depth, idx int, resume uintptr) (Value, error, bool) {
	s := r.jit
	ctx := s.ssaCtxs[idx]
	// resume, when not 0, is where native code goes on after a native
	// call whose callee Go finished, in place of an entry.
	for {
		// What Go has run since the last entry may have changed the frame, how
		// deep calls are, or the scope.
		s.ssaCtx = ctx
		ctx.ReturnTo, ctx.Live, ctx.TailReturn, ctx.ExitMarking = 0, 0, 0, 0
		r.jitShareContexts(idx, f)
		ctx.Locals = unsafe.Pointer(unsafe.SliceData(f.locals))
		ctx.Stack = unsafe.Pointer(&r.stack[f.base])
		ctx.Upvalues = unsafe.Pointer(unsafe.SliceData(f.cl.upvalues))
		ctx.Open = uint64(len(f.openUpvalues))
		if f.cl.fn.HasTailCall {
			// Its calls of itself in tail position, deep in the stack, give
			// the frame up natively where the interpreter would
			// (ssa.OpTailSelf): its closure, and whether it would.
			ctx.Closure, ctx.TailFrame, ctx.FrameThis = unsafe.Pointer(f.cl), 0, unsafe.Pointer(&f.this)
			if f.newTarget.IsUndefined() && f.thisRef == nil {
				ctx.TailFrame = 1
			}
		}
		if e.this {
			this, bound := f.thisValue()
			if !bound {
				this = uninitialized
			}
			*(*Value)(unsafe.Pointer(&ctx.This)) = this
		}
		s.entries++
		s.ssaEntries++
		e.ssaStats.entries++
		start, edges := pc, r.backEdges-s.pollCredit
		r.jitGoRunAt(idx, f, e)
		var err error
		if resume != 0 {
			err, resume = e.ssa.Resume(resume, s.ssaCtxs[idx+1]), 0
		} else {
			err = e.ssa.Run(pc, ctx)
		}
		if s.goRuns[idx].pinned {
			r.jitGoUnpin(idx)
		}
		if err != nil {
			return r.jitInterpret(f, f.base+depth, nil)
		}
		// The frame of the code that left, if it left that to Go: this
		// code's, after a resume, or its deepest native callee's.
		k := idx
		for k+1 < len(s.ssaCtxs) && s.ssaCtxs[k+1].Live == abi.LiveCall {
			k++
		}
		if c := s.ssaCtxs[k]; k != idx && c.ExitMarking != 0 {
			// A native callee's exit the collector's marking made ends
			// this run's stretch for it, as this code's own would
			// (jitSSAProfit); its context, which Go does not enter, is
			// cleared of it here.
			c.ExitMarking, ctx.ExitMarking = 0, 1
		}
		if c := s.ssaCtxs[k]; c.ExitKind == abi.ExitTable {
			// Asked here, not in Apply: a return, the most common way
			// out, costs no call.
			s.ssaRecords += uint64(s.exitScratch.Apply(c))
		}
		if k == idx && ctx.ExitKind == abi.ExitHost && r.jitTailGivesUp(f, int(ctx.ExitPC)) {
			// A call in tail position deep in the stack, which gives up a
			// frame Go entered for another function's -- what only Go can
			// do there, run's work, as the interpreter's -- says nothing of
			// the code, as the collector's marking's exits do not.
			e.ssaStats.entries--
		} else {
			r.jitSSAProfit(e, ctx, start, edges)
		}
		if c := s.ssaCtxs[idx+1]; c.Live == abi.LiveCall {
			// A native call's callee left native code: Go finishes it
			// (jitUnwindNative), then this code goes on natively where the
			// callee would have returned to, its state where it left it --
			// as V8's lazy deoptimization leaves a caller's optimized frame
			// alone. Not while the collector marks, as native code takes
			// pointers there; nor into code compiled again or dropped
			// meanwhile (Size 0, closed), nor after a throw: then as below,
			// from the frame the call wrote.
			s.hosts++
			e.ssaStats.hosts++
			back, base, code := uintptr(c.ReturnTo), c.Base, e.ssa
			exitPC, exitDepth := int(ctx.ExitPC), int(ctx.ExitDepth)
			// Past its call meanwhile, as the interpreter's frame is during
			// one, which a stack trace shows (stackFrame).
			f.pc = uint32(exitPC) + 1
			v, err := r.jitUnwindNative(idx, e, f.cl.fn)
			if err == nil && r.stopped == nil && back != 0 && e.ssa == code && code.Contains(back) && !jit.Marking() {
				// Native code run meanwhile may have used the context.
				c.Base, c.Live, c.ReturnTo = base, 0, 0
				*(*Value)(unsafe.Pointer(&c.RetValue)) = v
				s.resumed++
				resume, pc = back, exitPC
				continue
			}
			e.ssaStats.records += ctx.Records
			r.jitApplyRecords(f, e, ctx)
			jitHandlers(f, e.ssaProg, exitPC)
			sp, ok := r.jitCallResult(f, exitPC, exitDepth, v, err)
			if !ok || r.stopped != nil {
				sp, err, native := r.jitCatch(f, e, sp, err)
				if !native {
					return r.jitInterpret(f, sp, err)
				}
				pc, depth = int(f.pc), sp-f.base
				continue
			}
			pc, depth = int(f.pc), sp-f.base
			if e.reoptDue() {
				r.jitReoptimize(f.cl, e)
			}
			if !e.ssa.HasEntry(pc) || e.entrySlow {
				v, err, done, again := r.jitGoOn(f, e, pc, sp)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
			continue
		}
		if s.ssaCtxs[idx+1].Live != 0 {
			// An inlined callee's exit: Go makes its frame and finishes it
			// (jitUnwindNative), then this code goes on after the call, as
			// after one Go made.
			s.hosts++
			e.ssaStats.hosts++
			// An inlined callee's exit while the collector marks leaves its
			// records, its frame's included, to Go.
			e.ssaStats.records += ctx.Records
			r.jitApplyRecords(f, e, ctx)
			exitPC, exitDepth := int(ctx.ExitPC), int(ctx.ExitDepth)
			jitHandlers(f, e.ssaProg, exitPC)
			// Past its call meanwhile, as a stack trace shows it.
			f.pc = uint32(exitPC) + 1
			v, err := r.jitUnwindNative(idx, e, f.cl.fn)
			sp, ok := r.jitCallResult(f, exitPC, exitDepth, v, err)
			if !ok || r.stopped != nil {
				sp, err, native := r.jitCatch(f, e, sp, err)
				if !native {
					return r.jitInterpret(f, sp, err)
				}
				pc, depth = int(f.pc), sp-f.base
				continue
			}
			pc, depth = int(f.pc), sp-f.base
			if e.reoptDue() {
				r.jitReoptimize(f.cl, e)
			}
			if !e.ssa.HasEntry(pc) || e.entrySlow {
				v, err, done, again := r.jitGoOn(f, e, pc, sp)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
			continue
		}
		if ctx.ExitKind == abi.ExitReturn {
			if s := jitSource(&ctx.RetFrom); s != nil && s.ref != nil {
				return *s, nil, true
			}
			return Value{num: math.Float64frombits(ctx.Ret)}, nil, true
		}
		e.ssaStats.records += ctx.Records
		r.jitApplyRecords(f, e, ctx)
		jitHandlers(f, e.ssaProg, int(ctx.ExitPC))
		switch ctx.ExitKind {
		case abi.ExitDeopt:
			// The frame holds the state at the guard; the interpreter runs
			// the rest of this invocation.
			s.guards++
			e.ssaStats.guards++
			if e.ssaStats.guardsAt == nil {
				e.ssaStats.guardsAt = map[int32]uint64{}
			}
			e.ssaStats.guardsAt[int32(int64(ctx.ExitSite))]++
			jitDeoptimized(e, ctx, start)
			f.pc = uint32(ctx.ExitPC)
			return r.jitInterpret(f, f.base+int(ctx.ExitDepth), nil)
		case abi.ExitHost:
			// Go runs the one instruction, then native code goes on from the
			// next entry, if there is one.
			s.hosts++
			e.ssaStats.hosts++
			// The context is every invocation's: Go may run native code of
			// its own, a call's, which overwrites it.
			exitPC := int(ctx.ExitPC)
			in := f.cl.fn.Code[exitPC]
			f.pc = uint32(exitPC)
			if p := s.goPending; p.set {
				// A call Go made from native code (jitGoCall), its result or
				// its throw here.
				s.goPending = jitGoPending{}
				if p.pc == exitPC && p.ctx == idx {
					sp, ok := r.jitCallResult(f, exitPC, int(ctx.ExitDepth), p.v, p.err)
					if !ok || r.stopped != nil {
						sp, err, native := r.jitCatch(f, e, sp, p.err)
						if !native {
							return r.jitInterpret(f, sp, err)
						}
						pc, depth = int(f.pc), sp-f.base
						continue
					}
					pc, depth = int(f.pc), sp-f.base
					if e.reoptDue() {
						r.jitReoptimize(f.cl, e)
					}
					if !e.ssa.HasEntry(pc) || e.entrySlow {
						v, err, done, again := r.jitGoOn(f, e, pc, sp)
						if !again {
							return v, err, done
						}
						pc, depth = s.goOnPC, s.goOnDepth
					}
					continue
				}
			}
			switch jitCallOp(in.Op) {
			case bytecode.OpYield, bytecode.OpAwait, bytecode.OpYieldStar:
				return r.jitSuspend(f, in, f.base+int(ctx.ExitDepth))
			case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
				r.jitCallSeen(f, e, exitPC, f.base+int(ctx.ExitDepth), jitCallInstr(in))
			case bytecode.OpGetProp, bytecode.OpGetPropThis:
				r.jitPolySeen(f, e, exitPC, f.base+int(ctx.ExitDepth), in)
			case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn:
				r.jitPrivateSeen(f, e, exitPC, f.base+int(ctx.ExitDepth), in)
			case bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpNewRegExp:
				r.jitRefillLiteral(f, e, exitPC)
			case bytecode.OpDefineField:
				jitDefineSeen(e, exitPC)
			}
			if r.jitTailGivesUp(f, exitPC) {
				// Deep in the stack, a call in tail position gives up the
				// frame for the callee's, as the interpreter's tail_call
				// does: run makes it (errTailCall), not the interpreter.
				if e.reoptDue() {
					r.jitReoptimize(f.cl, e)
				}
				sp, argc := f.base+int(ctx.ExitDepth), int(in.A)
				this := Undefined
				if in.Op == bytecode.OpTailCallMethod {
					this = r.stack[sp-argc-2]
				}
				r.pendingTail = tailCall{callee: r.stack[sp-argc-1], this: this, args: append([]Value(nil), r.stack[sp-argc:sp]...)}
				return Undefined, errTailCall, true
			}
			o, before := jitWriteTarget(r.stack, f.base+int(ctx.ExitDepth), in)
			sp, steps, err := r.jitHost(f, f.base+int(ctx.ExitDepth), 1)
			if err == nil && in.Op == bytecode.OpSetProp {
				r.jitPolyWriteSeen(f, e, exitPC, in, o, before)
			}
			if err != nil || r.stopped != nil || steps == 0 {
				sp, err, native := r.jitCatch(f, e, sp, err)
				if !native {
					return r.jitInterpret(f, sp, err)
				}
				pc, depth = int(f.pc), sp-f.base
				continue
			}
			pc, depth = int(f.pc), sp-f.base
			switch in.Op {
			case bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp:
				jitFed(f.cl, e, uint32(exitPC))
			}
			jitPolySettled(e)
			if e.reoptDue() {
				// The code is compiled again now, not at the next call: a
				// loop in this one may run long.
				r.jitReoptimize(f.cl, e)
			}
			if !e.ssa.HasEntry(pc) || e.entrySlow {
				// No entry here; or native code does not pay (jitSSAProfit),
				// and the interpreter runs the rest of this invocation.
				v, err, done, again := r.jitGoOn(f, e, pc, sp)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
		case abi.ExitPoll:
			// The interpreter's back-edge check, at a loop header native
			// code can be entered again at.
			s.budgets++
			e.ssaStats.polls++
			r.backEdges = s.backEdgeBudget()
			if err := r.checkInterruptNow(); err != nil {
				return Undefined, err, true
			}
			pc, depth = int(ctx.ExitPC), int(ctx.ExitDepth)
			f.pc = uint32(pc)
			if r.jitStressDeopt(s) {
				// Under stress, the interpreter finishes the invocation
				// from where native code got to.
				return r.jitInterpret(f, f.base+depth, nil)
			}
		}
	}
}

// jitShared is what every context native code runs in shares with the one
// it was called from (abi.Context): where the global names are, the
// VM's stack, how deep calls may go.
type jitShared struct {
	global, lexNames, globalLex, strMethods, stackBase unsafe.Pointer
	stackEnd, levelLimit                               uint64
	// tailLevel is abi.Context's TailLevel: the level whose frame is
	// tailCallDepth deep, the run's frame at idx.
	tailLevel uint64
	// charCodeAt is String.prototype.charCodeAt as native code calls it
	// (jitStringMethod), which every level's code reads in its own
	// context: written only into a run's own, a native callee's charCodeAt
	// left native code at every call -- Crypto's intAt, until its callers
	// stopped calling it natively, and then Go ran each of its calls.
	// The function's object, or nil, so that the struct compares: a
	// Value's word is a NaN, unequal to itself. strProto, strShape and
	// ccOffset are where it is in String.prototype (abi.Context's
	// StringProto), when it is there.
	charCodeAt *Object
	strProto   *Object
	strShape   *shape
	ccOffset   uint64
}

// jitShareContexts writes what the contexts share into context idx, which
// code is entered in, and every one past it, which its native calls run
// in, with each one's level, when it is not what they hold: a native call
// then writes none of it. They hold it from the last time it was written
// (jitSharedContexts), if that was from idx or before, as nothing else
// writes it.
func (r *Runtime) jitShareContexts(idx int, f *frame) {
	s := r.jit
	sh := jitShared{
		global: unsafe.Pointer(f.cl.scope()), lexNames: unsafe.Pointer(&r.lexNames),
		stackBase: unsafe.Pointer(unsafe.SliceData(r.stack)), stackEnd: uint64(len(r.stack)),
		levelLimit: uint64(min(len(s.ssaCtxs), idx+max(0, r.maxFrames-r.frameDepth))),
		tailLevel:  uint64(idx + max(0, tailCallDepth-r.frameDepth)),
	}
	if r.isGlobalScope(f.cl.scope()) {
		// The interpreter reads a script's lexical bindings where the
		// scope is the realm's (globalLexProp).
		sh.globalLex = unsafe.Pointer(r.globalLex)
	}
	if p := r.proto.str; p != nil {
		sh.strMethods = unsafe.Pointer(p)
	}
	if m := r.jitStringMethod(); m.IsObject() {
		sh.charCodeAt = m.Object()
		if p := r.proto.str; p.shape != nil {
			if i := p.findOwn(s.charCodeAtKey); i >= 0 {
				// Its shape remembered: a change of its layout gives it
				// another, which native code's check then misses.
				sh.strProto, sh.strShape = p, remember(p.shape)
				sh.ccOffset = uint64(i)*uint64(jitEncoding.PropertySize) + uint64(jitEncoding.PropertyValue)
			}
		}
	}
	if sh == s.ssaShared && s.ssaSharedFrom <= idx {
		return
	}
	for i := idx; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
		c.Level, c.LevelLimit, c.TailLevel = uint64(i), sh.levelLimit, sh.tailLevel
		c.StackBase, c.StackEnd = sh.stackBase, sh.stackEnd
		c.StackTop, c.StackHigh = &r.stackTop, &r.stackHigh
		c.BackEdges = &r.backEdges
		c.Global, c.LexNames, c.GlobalLex, c.StringMethods = sh.global, sh.lexNames, sh.globalLex, sh.strMethods
		*(*Value)(unsafe.Pointer(&c.CharCodeAt)) = Value{}
		if sh.charCodeAt != nil {
			*(*Value)(unsafe.Pointer(&c.CharCodeAt)) = Obj(sh.charCodeAt)
		}
		c.StringProto, c.StringShape, c.CharCodeAtOffset = unsafe.Pointer(sh.strProto), uint64(uintptr(unsafe.Pointer(sh.strShape))), sh.ccOffset
	}
	s.ssaShared, s.ssaSharedFrom = sh, idx
	s.ssaSharedGen++
}

// jitContexts is how many contexts are made first: one for each runSSA
// running, each native call another (abi.Context.Level); as many more
// each time they run out, as a stack grows; and jitContextsMax how many
// there come to be. A native call past the last exits, and Go finishes
// every level (jitUnwindNative): with no more than 24, EarleyBoyer's
// unifier, a recursion deeper than that, finished most of its calls in
// Go. A context is some 17 KB, of the JIT's budget (jitBudget): 24 made
// at once had been 400 KB of every runtime that ran native code, however
// deep its calls, and more than the JIT's eighth of a runtime limited to
// 3 MB.
const (
	jitContexts    = 4
	jitContextsMax = 96
)

// jitGrowContexts adds contexts, linked to the last -- jitContexts, or as
// many as there are -- if there are fewer than jitContextsMax and the
// JIT's budget has the room, and reports whether it did. The others stay
// where they are, and native code may be running in them: each one whose
// calls the contexts limited may go as deep as the new ones allow, and
// each new one holds what they share (jitShareContexts).
func (r *Runtime) jitGrowContexts() bool {
	s := r.jit
	old := len(s.ssaCtxs)
	add := min(max(old, jitContexts), jitContextsMax-old)
	if add <= 0 {
		return false
	}
	s.budget = r.jitBudget()
	if !s.makeRoom(add * jitContextBytes) {
		return false
	}
	chunk := make([]abi.Context, add)
	for i := range chunk {
		chunk[i].Host = unsafe.Pointer(r)
		chunk[i].SetConsts(&jitEncoding)
		s.ssaCtxs = append(s.ssaCtxs, &chunk[i])
	}
	for i := max(old, 1); i < len(s.ssaCtxs); i++ {
		c, prev := s.ssaCtxs[i], s.ssaCtxs[i-1]
		prev.Next, c.Prev = unsafe.Pointer(c), unsafe.Pointer(prev)
	}
	if old == 0 {
		return true
	}
	n := uint64(len(s.ssaCtxs))
	last := s.ssaCtxs[old-1]
	for i, c := range s.ssaCtxs {
		switch {
		case i >= old:
			c.Level, c.LevelLimit, c.TailLevel = uint64(i), n, last.TailLevel
			c.StackBase, c.StackEnd, c.StackTop, c.StackHigh = last.StackBase, last.StackEnd, last.StackTop, last.StackHigh
			c.BackEdges, c.Global, c.LexNames, c.GlobalLex = last.BackEdges, last.Global, last.LexNames, last.GlobalLex
			c.StringMethods = last.StringMethods
			c.CharCodeAt = last.CharCodeAt
			c.StringProto, c.StringShape, c.CharCodeAtOffset = last.StringProto, last.StringShape, last.CharCodeAtOffset
		case c.LevelLimit == uint64(old):
			c.LevelLimit = n
		}
	}
	if s.ssaShared.levelLimit == uint64(old) {
		s.ssaShared.levelLimit = n
		s.ssaSharedGen++
	}
	return true
}

// jitNativeLevel is a frame a native call made whose callee left native
// code, or an inlined callee's an exit inside it wrote (abi.Context.Live):
// what Go makes the VM's frame from, and finishes it with. An inlined
// callee's has its program's locals and its receiver's slot plus one
// (abi.Context's Inline fields), and top, the stack's top before it.
type jitNativeLevel struct {
	cl                    *closure
	base                  int
	this                  Value
	kind, pc, depth, site uint64
	inline                bool
	locals, thisSlot, top int
	// construct marks a callee a construction called, whose result, if not
	// an object, is its receiver, and newTarget is then the function the
	// construction called, the frame's new.target.
	construct bool
	newTarget Value
	// ctx is the level's context's index.
	ctx int
	// returnTo is where its native caller goes on after the call
	// (abi.Context.ReturnTo), and code the caller's code then: the caller
	// is resumed there once Go has finished this level (jitUnwindNative).
	returnTo uintptr
	code     *jit.SSACode
}

// jitUnwindNative finishes the native calls the code running in context
// idx, e's, made, the innermost of which left native code, and the inlined
// callee an exit inside wrote the frame of: it makes the VM's frame of
// each, as runFD would have, from the outermost; then finishes each from
// the innermost -- that one from its exit, as runSSA does, the others from
// after their call, with what the one they called returned or threw -- and
// returns what the outermost returned or threw.
func (r *Runtime) jitUnwindNative(idx int, e *jitEntry, code *bytecode.Function) (Value, error) {
	s := r.jit
	if c := s.ssaCtxs[idx+1]; c.Live == abi.LiveCall && c.ExitKind == abi.ExitEnter && (idx+2 == len(s.ssaCtxs) || s.ssaCtxs[idx+2].Live == 0) {
		return r.jitEnterOne(c, idx+1, code, int(s.ssaCtxs[idx].ExitPC))
	}
	// Where the call each level runs for is: e's code's at first, then
	// that of the level before.
	caller, callerPC := code, int(s.ssaCtxs[idx].ExitPC)
	// Its levels go past those of the unwinds it runs inside, which hold
	// theirs until they finish: they are reached by index, the slices
	// growing as the unwinds it runs need.
	start := len(s.unwinding)
	var left *jitEntry
	leftPC := 0
	for i := idx + 1; i < len(s.ssaCtxs) && s.ssaCtxs[i].Live != 0; i++ {
		c := s.ssaCtxs[i]
		l := jitNativeLevel{base: int(c.Base), kind: c.ExitKind, pc: c.ExitPC, depth: c.ExitDepth, site: c.ExitSite, ctx: i}
		if c.Live == abi.LiveInline {
			// The callee its caller -- e's code or a native call's callee's
			// -- inlined at the call it left at.
			l.inline, l.locals, l.thisSlot = true, int(c.InlineLocals), int(c.InlineThis)
			l.construct = callerPC < len(caller.Code) && caller.Code[callerPC].Op == bytecode.OpNew
			if l.construct && c.InlineCallee != 0 {
				// The object, which the caller's code keeps alive.
				l.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.InlineCallee)))
			}
			// Its code is that of the nearest level below it not inlined,
			// or e's, which inlined it, and the callees it is inlined in.
			parent, parentPC := e, s.ssaCtxs[idx].ExitPC
			outer := true
			for k := len(s.unwinding) - 1; k >= start; k-- {
				if caller := &s.unwinding[k]; !caller.inline {
					parent, parentPC = s.cache[weak.Make(caller.cl.fn)], caller.pc
					break
				}
				outer = false
			}
			for _, cl := range parent.ssaInlined {
				if uintptr(unsafe.Pointer(cl)) == uintptr(c.InlineClosure) {
					l.cl = cl
				}
			}
			if l.cl == nil {
				panic("jit: an inlined callee's closure is not its caller's")
			}
			if outer {
				// Counted once, for the call the outermost is inlined at,
				// below.
				left, leftPC = parent, int(parentPC)
			}
		} else {
			l.cl, l.this = (*closure)(c.Closure), *(*Value)(unsafe.Pointer(&c.This))
			l.construct = callerPC < len(caller.Code) && caller.Code[callerPC].Op == bytecode.OpNew
			if l.construct && c.NewTarget != 0 {
				l.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.NewTarget)))
			}
			l.returnTo = c.ReturnTo
			if k := len(s.unwinding) - 1; k >= start && !s.unwinding[k].inline {
				// The caller, a level of its own: its code now.
				if ce := s.cache[weak.Make(s.unwinding[k].cl.fn)]; ce != nil {
					l.code = ce.ssa
				}
			}
		}
		caller, callerPC = l.cl.fn, int(l.pc)
		s.unwinding = append(s.unwinding, l)
		c.Live, c.ReturnTo = 0, 0
	}
	n := len(s.unwinding) - start
	if left != nil && (n == 0 || !r.jitPoolEmptied(&s.unwinding[len(s.unwinding)-1])) && !r.jitInlineFed(left) && !left.reoptPending() {
		// An exit inside an inlined callee -- but one at a construction
		// inlined in it whose pool ran out, which Go fills again, as
		// jitCallSeen does not count at the call itself.
		r.jitInlineLeft(left, leftPC)
	}
	if n != 0 {
		// The innermost left -- but for one that never entered native code
		// (ExitEnter), a call Go makes; the others only return through Go.
		if l := &s.unwinding[len(s.unwinding)-1]; !l.inline && l.kind != abi.ExitEnter && !r.jitPoolEmptied(l) {
			// Its entry by its function: a callee only native callers
			// call has no hint. Not one that left for its pool to be
			// filled again.
			if e := s.cache[weak.Make(l.cl.fn)]; e != nil {
				r.jitUnwound(e)
			}
		}
	}
	defer func() {
		clear(s.unwinding[start:])
		clear(s.unwindingFrames[start:])
		s.unwinding, s.unwindingFrames = s.unwinding[:start], s.unwindingFrames[:start]
	}()
	s.unwound += uint64(n)
	if n != 0 {
		// A call from the last context, which the contexts, not the
		// frames the VM allows, limited: more of them.
		l := &s.unwinding[start+n-1]
		if c := s.ssaCtxs[l.ctx]; l.ctx == len(s.ssaCtxs)-1 && c.LevelLimit == uint64(len(s.ssaCtxs)) && l.kind == abi.ExitHost {
			r.jitGrowContexts()
		}
	}
	if n > 1 {
		// What Go runs to finish a level runs in contexts past every
		// level's: a level's native code, resumed after its call, finds
		// its spills and keeps where it left them.
		top := s.ctxTop
		s.ctxTop = max(top, s.unwinding[start+n-1].ctx+1)
		defer func() { s.ctxTop = top }()
	}
	for k := range n {
		l := &s.unwinding[start+k]
		fn := l.cl.fn
		if l.inline {
			// The exit wrote its slots as its program has them -- locals,
			// receiver, operands -- past its caller's operands: the
			// receiver goes to the frame, the operands down to the locals.
			l.top = r.stackTop
			l.this = Undefined
			if l.thisSlot > 0 {
				l.this = r.stack[l.base+l.thisSlot-1]
			}
			from, to := l.base+l.locals, l.base+fn.LocalCount
			copy(r.stack[to:to+int(l.depth)], r.stack[from:from+int(l.depth)])
			clear(r.stack[to+int(l.depth) : from+int(l.depth)])
			r.stackTop = max(r.stackTop, l.base+fn.LocalCount+fn.MaxStack)
			r.stackHigh = max(r.stackHigh, r.stackTop)
		} else {
			l.top = l.base
		}
		f := r.pushFrame()
		if c := s.ssaCtxs[l.ctx]; !l.inline && c.Records != 0 {
			// Records its exit left to Go, the collector marking: before
			// anything else reads its frame, an inlined callee's past it
			// included.
			if e := s.cache[weak.Make(fn)]; e != nil {
				f.cl, f.locals, f.base = l.cl, r.stack[l.base:l.base+fn.LocalCount:l.base+fn.LocalCount], l.base+fn.LocalCount
				saved := s.ssaCtx
				s.ssaCtx = c
				r.jitApplyRecords(f, e, c)
				s.ssaCtx = saved
				l.this = *(*Value)(unsafe.Pointer(&c.This))
			}
		}
		f.cl = l.cl
		f.locals = r.stack[l.base : l.base+fn.LocalCount : l.base+fn.LocalCount]
		f.base = l.base + fn.LocalCount
		f.pc = uint32(l.pc)
		if k < n-1 {
			// Past the call it makes, as the interpreter's frame is during
			// one, which a stack trace shows.
			f.pc++
		}
		f.this, f.newTarget, f.callee, f.args = l.this, Undefined, nil, nil
		if l.construct && l.newTarget.IsObject() {
			f.newTarget = l.newTarget
		}
		f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
		if c := s.ssaCtxs[l.ctx]; !l.inline && c.Open != 0 && l.ctx < len(s.calleeOpen) {
			// What its closures captured, which its context kept, the
			// frame's now, which closes it as it is popped.
			f.openUpvalues = append(f.openUpvalues, s.calleeOpen[l.ctx]...)
			clear(s.calleeOpen[l.ctx])
			s.calleeOpen[l.ctx], c.Open = s.calleeOpen[l.ctx][:0], 0
		}
		f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
		if !l.inline {
			// Those in force at its exit, or at its call: an exception
			// from the call unwinds the frame from there.
			if e := s.cache[weak.Make(fn)]; e != nil {
				jitHandlers(f, e.ssaProg, int(l.pc))
			}
		}
		s.unwindingFrames = append(s.unwindingFrames, f)
	}
	var v Value
	var err error
	for k := n - 1; k >= 0; k-- {
		f, l := s.unwindingFrames[start+k], s.unwinding[start+k]
		e := r.jit.hint(f.cl)
		if e == nil {
			e = r.jitFor(f.cl)
		}
		var next *jitNativeLevel
		if k < n-1 {
			next = &s.unwinding[start+k+1]
		}
		if k == n-1 {
			v, err = r.jitFinishExit(f, e, &l)
		} else if !l.inline && !next.inline && next.returnTo != 0 && err == nil && r.stopped == nil &&
			e != nil && e.ssa != nil && e.ssa.Contains(next.returnTo) && !jit.Marking() {
			// Its native code goes on where the call returns to, as runSSA's
			// does after a callee leaves: its context, spills and keeps are
			// as it left them, and its callee's, frame base included.
			c := s.ssaCtxs[next.ctx]
			c.Live, c.ReturnTo = 0, 0
			*(*Value)(unsafe.Pointer(&c.RetValue)) = v
			s.resumed++
			s.resumedLevels++
			var native bool
			if v, err, native = r.runSSAIn(f, e, int(l.pc), int(l.depth), l.ctx, next.returnTo); !native {
				v, err, _ = r.jitInterpret(f, f.base+int(l.depth), nil)
			}
		} else {
			// A frame native code made goes on in native code where it can,
			// whatever Go's entries to it cost (entrySlow).
			sp, ok := r.jitCallResult(f, int(l.pc), int(l.depth), v, err)
			switch {
			case !ok || r.stopped != nil || e == nil || e.ssa == nil || !e.ssa.HasEntry(int(f.pc)):
				v, err, _ = r.jitInterpret(f, sp, err)
			default:
				var native bool
				if v, err, native = r.runSSA(f, e, int(f.pc), sp-f.base); !native {
					v, err, _ = r.jitInterpret(f, sp, nil)
				}
			}
		}
		if l.construct && err == nil && !v.IsObject() {
			v = l.this
		}
		r.popFrameOf(f, l.top)
		if err == errTailCall {
			// It ended in a call in tail position, deep in the stack, its
			// frame given up as run gives it up: the call is made here, the
			// ordinary way, as jitEnterCallee makes one.
			tc := r.pendingTail
			r.pendingTail = tailCall{}
			v, err = r.call(tc.callee, tc.this, tc.args)
		}
	}
	return v, err
}

// jitEnterCallee runs a callee a native call made, whose frame f is, from
// its start, as runFD would once it had made the frame: the callee had no
// code its native callers may call (abi.ExitEnter). Its native caller goes
// on after the call with what it returns (jitUnwindNative), its own frame
// never written: the call costs Go's running the callee, not a host exit's
// writing the caller's frame and making the call.
func (r *Runtime) jitEnterCallee(f *frame, e *jitEntry, ctx int) (Value, error) {
	r.jit.entered++
	if c := r.jit.ssaCtxs[ctx]; c.EnterCallee != 0 {
		// A word native code wrote, the object's address, which the
		// caller's code keeps alive.
		f.callee = *(**Object)(unsafe.Pointer(&c.EnterCallee))
		c.EnterCallee = 0
	}
	// A call Go makes to code native callers no longer call counts toward
	// their calling it again (jitRetryNative), as the call's exit would
	// have (jitCallTarget); tryJITAt counts it for a callee Go enters.
	if e != nil && e.ssa != nil && e.notNative && e.entrySlow {
		jitRetryNative(e)
	}
	// One with no code at all -- the cache dropped it, which native
	// callers' code was compiled to call (jitLink) -- is compiled for them
	// again, as it was when their code was compiled (NativeCalls), and
	// they call it through its link from the next call on: Go would not
	// compile one with no loop (LowerSSA), which only native callers call
	// natively.
	if (e == nil || e.ssa == nil && !e.deferred && !e.calleeRefused) && r.jit.seeding == 0 {
		if ce := r.jitNativeCallee(f.cl); ce != nil {
			e = ce
		}
	}
	// As runFD runs a frame it made: its native code first, entered from
	// Go -- which no native caller may jump to for now (notNative), not
	// none at all -- or its tree.
	v, err, native := r.tryJITFrame(f)
	if !native {
		fn := f.cl.fn
		t := (*tree)(atomic.LoadPointer(&fn.VMCode))
		if t == nil {
			t = firstTree(fn)
		}
		if t != noTree {
			v, err = r.runTree(f, t)
		} else {
			v, err = r.execute(f)
		}
	}
	if err == errTailCall {
		// It ended in a tail call, its frame given up as run would: the
		// call is made here, the ordinary way.
		tc := r.pendingTail
		r.pendingTail = tailCall{}
		v, err = r.call(tc.callee, tc.this, tc.args)
	}
	return v, err
}

// jitEnterOne is jitUnwindNative for the usual case of the calls Go
// finishes for an ExitEnter: one level, context i, c, a callee that never
// entered native code, called by code's call at callerPC. Its frame is made
// as jitUnwindNative makes it, without the levels' bookkeeping.
func (r *Runtime) jitEnterOne(c *abi.Context, i int, code *bytecode.Function, callerPC int) (Value, error) {
	s := r.jit
	cl := (*closure)(c.Closure)
	fn := cl.fn
	base, this := int(c.Base), *(*Value)(unsafe.Pointer(&c.This))
	construct := callerPC < len(code.Code) && code.Code[callerPC].Op == bytecode.OpNew
	newTarget := Undefined
	if construct && c.NewTarget != 0 {
		newTarget = Obj(*(**Object)(unsafe.Pointer(&c.NewTarget)))
	}
	c.Live, c.ReturnTo = 0, 0
	s.unwound++
	f := r.pushFrame()
	f.cl = cl
	f.locals = r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this, f.newTarget, f.callee, f.args = this, newTarget, nil, nil
	f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
	f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
	v, err := r.jitEnterCallee(f, s.hint(cl), i)
	if construct && err == nil && !v.IsObject() {
		v = this
	}
	r.popFrameOf(f, base)
	return v, err
}

// jitPoolEmptied reports whether level l left native code at a construction
// its code makes from a pool, now empty: it left for the pool to be filled
// again (jitRefillPools).
func (r *Runtime) jitPoolEmptied(l *jitNativeLevel) bool {
	fn := l.cl.fn
	if l.kind != abi.ExitHost || int(l.pc) >= len(fn.Code) || fn.Code[l.pc].Op != bytecode.OpNew {
		return false
	}
	e := r.jit.cache[weak.Make(fn)]
	if e == nil {
		return false
	}
	for _, x := range jitTargets(e) {
		if int(x.pc) == int(l.pc) && x.pool != nil && x.pool.Count == 0 {
			return true
		}
	}
	return false
}

// jitTargets iterates over the calls e's code makes natively and those it
// inlines, each list in turn.
func jitTargets(e *jitEntry) iter.Seq2[int, *jitInline] {
	return func(yield func(int, *jitInline) bool) {
		i := 0
		for _, list := range [2][]jitInline{e.nativeCalls, e.inlines} {
			for k := range list {
				if !yield(i, &list[k]) {
					return
				}
				i++
			}
		}
	}
}

// jitUnwindProbe is how many times native code called natively leaves
// between looks at whether it leaves too often, and jitUnwindShare the
// share of its calls it may leave from, as one in so many: Go then finishes
// every native call outside it (jitUnwindNative), which costs more than Go
// making the call.
const (
	jitUnwindProbe = 64
	jitUnwindShare = 4
)

// jitUnwound counts a native call that left native code inside e's code
// (nativeOut). Code that leaves on more than one in jitUnwindShare of its
// calls is compiled again with what its exits taught since (jitCallSeen,
// jitPolySeen), its native callers calling it still, as V8 deoptimizes
// code and optimizes it again with the feedback gathered since: leaving
// while it learned, its calls and constructions not yet decided, is no
// reason to stop calling it natively. Only once those compiles are spent
// (jitUnwindReopts), as V8 gives up optimizing a function that deoptimizes
// too often, do native callers stop calling it and leave for Go at the
// call instead, which makes it (notNative), until jitRetryNative -- or at
// once, if its exits taught nothing since it was compiled (jitLearning),
// for code compiled again would be the same: RayTrace's getReflectionRay,
// constructing with a forwarding constructor its code leaves to Go, and
// wrapUp, whose remainder of fractions Go computes, were compiled three
// times more each for nothing. Nor is one counted while the collector
// marks (jitInlineLeft).
func (r *Runtime) jitUnwound(e *jitEntry) {
	if jitMarking() {
		return
	}
	if e.nativeOut++; e.nativeOut%jitUnwindProbe != 0 || e.nativeOut*jitUnwindShare <= e.link.in {
		return
	}
	if e.ssa != nil && e.unwindReopts < jitUnwindReopts && jitLearning(e) {
		e.unwindReopts++
		e.unwindReopt = true
		e.link.in, e.nativeOut = 0, 0
		e.ssaStats.calleeGoCalls, e.ssaStats.calleeWork = 0, 0
		return
	}
	e.notNative, e.link.entry, e.nativeRetry = true, 0, 0
	e.nativeBackoff = min(e.nativeBackoff+1, jitNativeBackoffs)
}

// jitLearning reports whether e's code has met, since it was compiled,
// what a compile would take that it was not compiled for: what is to be
// compiled again for (reoptPending), shapes and calls its exits learned
// (feedReopt, polyPending, inlinePending), or a call it has left native
// code at but not yet decided, which the compile would decide if the
// existing tiers have met its target (jitSeedCalls).
func jitLearning(e *jitEntry) bool {
	if e.reoptPending() || e.feedReopt || e.polyPending || e.inlinePending {
		return true
	}
	for _, n := range e.callSites {
		if n != 0 && n < jitCallNative {
			return true
		}
	}
	return false
}

// jitUnwindReopts is how many times code that leaves too often when called
// natively is compiled again for it before native callers stop calling it
// (jitUnwound).
const jitUnwindReopts = 3

// jitNativeRetry is how many calls Go makes to code native callers no
// longer call before they call it again, doubled for each time it was
// found to leave too often, up to jitNativeBackoffs times: code that left
// while it learned what to compile for, or whose callers' objects changed,
// may not now.
const (
	jitNativeRetry    = 1024
	jitNativeBackoffs = 8
)

// jitRetryNative counts a call Go makes to e's code, which native callers
// no longer call (notNative) -- one Go enters it for, or one a native
// caller left for Go to make (jitCallTarget), which a function Go does not
// enter, or calls without a frame, has only -- and has them call it again
// after jitNativeRetry, counting its leaving afresh.
func jitRetryNative(e *jitEntry) {
	if e.nativeRetry++; e.nativeRetry >= jitNativeRetry<<(e.nativeBackoff-1) {
		e.notNative, e.link.in, e.nativeOut = false, 0, 0
		e.ssaStats.calleeGoCalls, e.ssaStats.calleeWork = 0, 0
		e.link.entry = e.ssa.EntryAddress(0)
	}
}

// jitFinishExit finishes a frame whose native code, called natively, left
// for Go as l says, as runSSA would have had it been entered from Go: Go
// runs the instruction, or the interpreter the rest, or the interrupt
// check is made; native code goes on where it can.
func (r *Runtime) jitFinishExit(f *frame, e *jitEntry, l *jitNativeLevel) (Value, error) {
	pc, depth := int(l.pc), int(l.depth)
	f.pc = uint32(pc)
	switch l.kind {
	case abi.ExitEnter:
		return r.jitEnterCallee(f, e, l.ctx)
	case abi.ExitHost:
		if p := r.jit.goPending; p.set {
			// A call Go made from this native code (jitGoCallNested), its
			// result or its throw here.
			r.jit.goPending = jitGoPending{}
			if p.pc == pc && p.ctx == l.ctx {
				sp, ok := r.jitCallResult(f, pc, depth, p.v, p.err)
				if !ok || r.stopped != nil {
					v, err, _ := r.jitInterpret(f, sp, p.err)
					return v, err
				}
				if e != nil && e.ssa != nil && e.reoptDue() {
					r.jitReoptimize(f.cl, e)
				}
				pc, depth = int(f.pc), sp-f.base
				break
			}
		}
		// What it learns here it learns as runSSA's exits do: a function
		// only native callers run is entered from Go nowhere else.
		in := f.cl.fn.Code[pc]
		if e != nil && e.ssa != nil {
			e.ssaStats.hosts++
			switch jitCallOp(in.Op) {
			case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
				r.jitCallSeen(f, e, pc, f.base+depth, jitCallInstr(in))
			case bytecode.OpGetProp, bytecode.OpGetPropThis:
				r.jitPolySeen(f, e, pc, f.base+depth, in)
			case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn:
				r.jitPrivateSeen(f, e, pc, f.base+depth, in)
			case bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpNewRegExp:
				r.jitRefillLiteral(f, e, pc)
			case bytecode.OpDefineField:
				jitDefineSeen(e, pc)
			}
		}
		o, before := jitWriteTarget(r.stack, f.base+depth, in)
		sp, steps, err := r.jitHost(f, f.base+depth, 1)
		if err == nil && in.Op == bytecode.OpSetProp && e != nil && e.ssa != nil {
			r.jitPolyWriteSeen(f, e, pc, in, o, before)
		}
		if err != nil || r.stopped != nil || steps == 0 {
			v, err, _ := r.jitInterpret(f, sp, err)
			return v, err
		}
		if e != nil && e.ssa != nil {
			switch in.Op {
			case bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp:
				jitFed(f.cl, e, uint32(pc))
			}
			jitPolySettled(e)
			if e.inlinePending && e.inlineReopts < 2*jitInlineReoptimizations {
				e.inlinePending, e.inlineReopt = false, true
			}
			if e.reoptDue() {
				r.jitReoptimize(f.cl, e)
			}
		}
		pc, depth = int(f.pc), sp-f.base
	case abi.ExitPoll:
		r.backEdges = r.jit.backEdgeBudget()
		if err := r.checkInterruptNow(); err != nil {
			return Undefined, err
		}
	default:
		if e != nil {
			e.ssaStats.guards++
		}
		if e != nil && e.ssa != nil && l.kind == abi.ExitDeopt {
			// A speculation that failed, learned as runSSA learns one: a
			// function only native callers run had kept failing it, at its
			// entry at every call, never compiled again generic.
			jitDeoptimized(e, &abi.Context{ExitSite: l.site}, 0)
			if e.reoptDue() {
				r.jitReoptimize(f.cl, e)
			}
		}
		v, err, _ := r.jitInterpret(f, f.base+depth, nil)
		return v, err
	}
	if e != nil && e.ssa != nil && e.ssa.HasEntry(pc) {
		if v, err, native := r.runSSA(f, e, pc, depth); native {
			return v, err
		}
	}
	f.pc = uint32(pc)
	v, err, _ := r.jitInterpret(f, f.base+depth, nil)
	return v, err
}

// jitTailGivesUp reports whether the instruction at pc of the frame Go
// entered native code with, f, is a call in tail position that gives up the
// frame, as the interpreter's tail_call decides: deep in the stack, in a
// frame no construction's, nor an arrow's sharing a derived constructor's
// this. (The compiler emits none under a handler.)
func (r *Runtime) jitTailGivesUp(f *frame, pc int) bool {
	op := f.cl.fn.Code[pc].Op
	return (op == bytecode.OpTailCall || op == bytecode.OpTailCallMethod) && r.frameDepth >= tailCallDepth &&
		f.newTarget.IsUndefined() && f.thisRef == nil
}

// jitCallOp is a call's opcode as native code makes the call: a call in
// tail position is an ordinary call where the stack is shallow, which is
// the only place native code makes it (ssa.OpTailRoom); deeper, Go makes
// it as the interpreter does (jitHost).
func jitCallOp(op bytecode.Op) bytecode.Op {
	switch op {
	case bytecode.OpTailCall:
		return bytecode.OpCall
	case bytecode.OpTailCallMethod:
		return bytecode.OpCallMethod
	}
	return op
}

// jitCallInstr is in with its opcode as native code makes the call
// (jitCallOp).
func jitCallInstr(in bytecode.Instr) bytecode.Instr {
	in.Op = jitCallOp(in.Op)
	return in
}

// jitCallResult has a frame whose state at the call at pc, depth operands
// deep, is in its frame go on after the call, which returned v or threw
// err: the call's operands go, its result comes, as when Go makes it
// (jitHost). It returns the operands' top, and false for a throw, which
// the interpreter takes from there.
func (r *Runtime) jitCallResult(f *frame, pc, depth int, v Value, err error) (int, bool) {
	in := jitCallInstr(f.cl.fn.Code[pc])
	if in.Op == bytecode.OpIterNextOrJump {
		// A loop's step Go made (abi.GoCallStep): its cursor stays, and the
		// next value goes on it; or, done, the loop's end is next.
		sp := f.base + depth
		f.pc = uint32(pc + 1)
		switch {
		case err != nil:
			return sp, false
		case v.IsUninitialized():
			f.pc = in.A
			return sp, true
		}
		r.stack[sp] = v
		return sp + 1, true
	}
	sp := f.base + depth - int(in.A) - 1
	switch in.Op {
	case bytecode.OpCallMethod:
		sp--
	case bytecode.OpApplyArguments:
		// f, apply and the receiver (a forwarding constructor's inlined
		// frame, ssa.InlineSite's Forward).
		sp = f.base + depth - 3
	}
	f.pc = uint32(pc + 1)
	if err != nil {
		return sp, false
	}
	r.stack[sp] = v
	return sp + 1, true
}

// jitHandlers gives f, a frame native code ran p's code in, the exception
// handlers in force at pc (ir.Program's HandlerAt), as the interpreter's
// push_catch and pop_catch would have left them there: native code
// registers none, and a throw Go unwinds the frame from finds them.
func jitHandlers(f *frame, p *ir.Program, pc int) {
	if p == nil || p.HandlerAt == nil || pc < 0 || pc >= len(p.HandlerAt) {
		return
	}
	n := 0
	for k := p.HandlerAt[pc]; k != 0; k = p.Handlers[k-1].Outer {
		n++
	}
	f.handlers = slices.Grow(f.handlers[:0], n)[:n]
	for k, i := p.HandlerAt[pc], n-1; k != 0; k, i = p.Handlers[k-1].Outer, i-1 {
		h := p.Handlers[k-1]
		f.handlers[i] = handler{pc: h.PC, stackDepth: int(h.Depth), isFinally: h.Finally}
	}
}

// jitSuspend suspends f, a generator's or an async function's frame, at
// in, a yield, a yield* or an await native code left it at, with sp its
// stack's top,
// as the interpreter's yield does: the value handed out popped, the pc
// past the instruction, and the depth beneath it kept for the resumption,
// which enters native code there (jitResume). The suspension is the
// runtime's own, as the tree tier's is (treeSig).
func (r *Runtime) jitSuspend(f *frame, in bytecode.Instr, sp int) (Value, error, bool) {
	sp--
	f.pc++
	f.savedSP = sp
	r.treeSig = suspendSignal{value: r.stack[sp], await: in.Op == bytecode.OpAwait,
		delegate: in.Op == bytecode.OpYieldStar, raw: in.Op == bytecode.OpYieldStar && in.A == 1}
	return Undefined, &r.treeSig, true
}

// jitResume is a generator's or an async function's resumption's hook
// (runGeneratorTree), for a runtime that runs the JIT: once resumptions
// have made the function hot, as calls make any function hot
// (tryJITFrame), its frame runs in native code from the entry at its pc
// -- the first after its parameters, or one after a yield or an await --
// to its next suspension or its end. false where it has no code, or none
// that is entered there.
func (r *Runtime) jitResume(f *frame) (Value, error, bool) {
	if f.cl.jitRefused {
		return Undefined, nil, false
	}
	e := r.jit.hint(f.cl)
	if e != nil && e.entrySlow && !e.reoptPending() {
		e.slowCalls++
		return Undefined, nil, false
	}
	if f.cl.jitCalls < r.jitCallThreshold {
		f.cl.jitCalls++
		if f.cl.jitCalls < r.jitCallThreshold {
			return Undefined, nil, false
		}
	}
	// The depth beneath the resumption's pc, which its entry loads: the
	// stack as the suspension left it, and the value sent in.
	p := (*ir.Program)(nil)
	if e != nil && e.ssa != nil {
		p = e.ssaProg
	} else if p, _ = r.jitLower(f.cl.fn, jitLowerSSA); p == nil {
		// Refused for good, as tryJITAt has a refusal: asked again at each
		// resumption, a short generator's -- one with no loop -- looked
		// its lowering up every time, a quarter of what it cost.
		f.cl.jitRefused = true
		return Undefined, nil, false
	}
	pc := int(f.pc)
	if pc >= len(p.Maps) || p.Maps[pc].Depth < 0 {
		return Undefined, nil, false
	}
	return r.tryJITAt(f, pc, p.Maps[pc].Depth, false)
}

// jitCatch unwinds f, a frame of e's code, for err, which what Go did at
// an exit threw with the operands' top at sp, as the interpreter does
// (unwindToHandler): where a handler of the function catches it and e's
// code has an entry there -- every handler has one -- native code goes on
// from it (true), as V8's code goes on at a handler. Otherwise the
// interpreter takes the frame from what it returns: unwound, with no
// error, where a handler caught it; as it was where none did.
func (r *Runtime) jitCatch(f *frame, e *jitEntry, sp int, err error) (int, error, bool) {
	if err == nil || r.stopped != nil || len(f.handlers) == 0 {
		return sp, err, false
	}
	sp, caught := r.unwindToHandler(f, sp, err)
	if !caught {
		return sp, err, false
	}
	return sp, nil, !e.entrySlow && e.ssa.HasEntry(int(f.pc))
}

// jitTypeWord is the type word an interface holding x has (abi.Encoding's
// IterState): what native code compares an object's data with to know
// what it holds.
func jitTypeWord(x any) uint64 {
	return uint64((*[2]uintptr)(unsafe.Pointer(&x))[0])
}
