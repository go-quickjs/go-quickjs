//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/bits"
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
	// A short BigInt (shortBig).
	ShortBigTag:     shortIntBase,
	ShortBigLowBits: shortBigLowBits,
	ShortBigHigh:    uint64(uintptr(unsafe.Pointer(&shortBigHigh))),
	// One past the int64 range (shortBigMag), and one on the heap.
	ShortBigWideTag:  shortBigBase,
	ShortBigWideHigh: uint64(uintptr(unsafe.Pointer(&shortBigHighX))),
	HeapBigTag:       heapBigBits,
	BigLenMask:       bigLenMask,
	BigDigits:        int32(bigWordSize),
	UpvalueSlot:      int32(unsafe.Offsetof(upvalue{}.slot)),
	UpvalueClosed:    int32(unsafe.Offsetof(upvalue{}.closed)),
	UpvalueSize:      int32(unsafe.Sizeof(upvalue{})),
	RandomCount:      int32(unsafe.Offsetof(mathRandom{}.count)),
	RandomNumbers:    int32(unsafe.Offsetof(mathRandom{}.numbers)),
	ObjectShape:      int32(unsafe.Offsetof(Object{}.shape)),
	ObjectProps:      int32(unsafe.Offsetof(Object{}.vals)),
	ObjectProto:      int32(unsafe.Offsetof(Object{}.proto)),
	WriteBarrier:     jit.WriteBarrier(),
	CallGo:           jit.CallGo(),
	BigFast:          uint64(jitBigFastAddr()),
	StoreRef:         jit.StoreRef(),
	Round:            runtime.GOARCH == "arm64" || cpu.X86.HasSSE41,
	ShapeKeys:        int32(unsafe.Offsetof(shape{}.keys)),
	KeySize:          int32(unsafe.Sizeof(propKey{})),
	PropertyKey:      int32(unsafe.Offsetof(propKey{}.key)),
	PropertyFlags:    int32(unsafe.Offsetof(propKey{}.flags)),

	Accessor:       accessorBits,
	AccessorGetter: int32(unsafe.Offsetof(accessor{}.getter)),
	AccessorSetter: int32(unsafe.Offsetof(accessor{}.setter)),

	ClassObject:     uint8(ClassObject),
	ClassProxy:      uint8(ClassProxy),
	ClassFunction:   uint8(ClassFunction),
	PropNotData:     uint8(propAccessor | propPrivate | propDeleted),
	PropNotWritable: uint8(propAccessor | propPrivate | propDeleted | propUninit | propWritable),
	PropWritable:    uint8(propWritable),
	PropUninit:      uint8(propUninit),

	String:         math.Float64bits(mkTag(KindString, 0)),
	Symbol:         math.Float64bits(mkTag(KindSymbol, 0)),
	StringData:     int32(unsafe.Offsetof(String{}.s)),
	StringLeft:     int32(unsafe.Offsetof(String{}.left)),
	StringLength:   int32(unsafe.Offsetof(String{}.length)),
	StringASCII:    int32(unsafe.Offsetof(String{}.ascii)),
	StringU16:      int32(unsafe.Offsetof(String{}.aux)),
	StringBuilding: int32(unsafe.Offsetof(String{}.building)),

	ObjectClass:        int32(unsafe.Offsetof(Object{}.class)),
	ObjectFlags:        int32(unsafe.Offsetof(Object{}.flags)),
	ObjectExt:          int32(unsafe.Offsetof(Object{}.ext)),
	ObjectArrayLen:     int32(unsafe.Offsetof(Object{}.arrayLen)),
	ObjectElems:        int32(unsafe.Sizeof(Object{}) + unsafe.Offsetof(objectExt{}.elems)),
	ExtElems:           int32(unsafe.Offsetof(objectExt{}.elems)),
	ObjectData:         int32(unsafe.Offsetof(objectExt{}.data)),
	IterState:          jitTypeWord((*iterState)(nil)),
	IterDone:           int32(unsafe.Offsetof(iterState{}.done)),
	IterArr:            int32(unsafe.Offsetof(iterState{}.arr)),
	IterIdx:            int32(unsafe.Offsetof(iterState{}.idx)),
	IterIter:           int32(unsafe.Offsetof(iterState{}.iter)),
	IterNext:           int32(unsafe.Offsetof(iterState{}.next)),
	MapIterData:        jitTypeWord((*mapIterData)(nil)),
	MapIterMap:         int32(unsafe.Offsetof(mapIterData{}.m)),
	MapIterKind:        int32(unsafe.Offsetof(mapIterData{}.kind)),
	MapIterDone:        int32(unsafe.Offsetof(mapIterData{}.done)),
	MapIterAt:          int32(unsafe.Offsetof(mapIterData{}.at) + unsafe.Offsetof(mapCursor{}.i)),
	MapIterGen:         int32(unsafe.Offsetof(mapIterData{}.at) + unsafe.Offsetof(mapCursor{}.gen)),
	MapIterLast:        int32(unsafe.Offsetof(mapIterData{}.at) + unsafe.Offsetof(mapCursor{}.last)),
	MapEntries:         int32(unsafe.Offsetof(jsMap{}.entries)),
	MapGen:             int32(unsafe.Offsetof(jsMap{}.gen)),
	MapEntrySize:       int32(unsafe.Sizeof(mapEntry{})),
	MapEntryValue:      int32(unsafe.Offsetof(mapEntry{}.value)),
	MapEntrySeq:        int32(unsafe.Offsetof(mapEntry{}.seq)),
	MapKeys:            uint8(mapIterKeys),
	MapValues:          uint8(mapIterValues),
	MapDeleted:         math.Float64bits(deletedKey.num),
	FuncData:           jitTypeWord((*funcData)(nil)),
	FuncClosure:        int32(unsafe.Offsetof(funcData{}.closure)),
	ClosureFn:          int32(unsafe.Offsetof(closure{}.fn)),
	ClosureUpvalues:    int32(unsafe.Offsetof(closure{}.upvalues)),
	ClosureEnv:         int32(unsafe.Offsetof(closure{}.env)),
	ClosureRealm:       int32(unsafe.Offsetof(closure{}.realm)),
	ClosureCode:        int32(unsafe.Offsetof(closure{}.closureCode)),
	ClosureHint:        int32(unsafe.Offsetof(closureCode{}.jitHint)),
	Mega:               true,
	FuncExtra:          int32(unsafe.Offsetof(funcData{}.extra)),
	ExtraLexThis:       int32(unsafe.Offsetof(funcExtra{}.lexThis)),
	ExtraLexThisRef:    int32(unsafe.Offsetof(funcExtra{}.lexThisRef)),
	HintSlotSize:       int32(unsafe.Sizeof(jitHintSlot{})),
	HintSlotFn:         int32(unsafe.Offsetof(jitHintSlot{}.fn)),
	HintSlotLink:       int32(unsafe.Offsetof(jitHintSlot{}.link)),
	LinkEntry:          int32(unsafe.Offsetof(jitLink{}.entry)),
	LinkBody:           int32(unsafe.Offsetof(jitLink{}.body)),
	LinkIn:             int32(unsafe.Offsetof(jitLink{}.in)),
	LinkParams:         int32(unsafe.Offsetof(jitLink{}.params)),
	LinkLocals:         int32(unsafe.Offsetof(jitLink{}.locals)),
	LinkFrame:          int32(unsafe.Offsetof(jitLink{}.frame)),
	LinkFlags:          int32(unsafe.Offsetof(jitLink{}.flags)),
	ClassArray:         uint8(ClassArray),
	ClassTypedArray:    uint8(ClassTypedArray),
	TypedData:          jitTypeWord((*typedArrayData)(nil)),
	TypedKind:          int32(unsafe.Offsetof(typedArrayData{}.kind)),
	TypedOffset:        int32(unsafe.Offsetof(typedArrayData{}.byteOffset)),
	TypedLength:        int32(unsafe.Offsetof(typedArrayData{}.fixedLength)),
	TypedStorage:       int32(unsafe.Offsetof(typedArrayData{}.st)),
	TypedEnd:           int32(unsafe.Offsetof(typedArrayData{}.end)),
	BufferBytes:        int32(unsafe.Offsetof(arrayBufferData{}.bytes)),
	BufferImmutable:    int32(unsafe.Offsetof(arrayBufferData{}.immutable)),
	FlagSparse:         uint8(objHasSparseElements),
	FlagHTMLDDA:        uint8(objHTMLDDA),
	FlagExtensible:     uint8(objExtensible),
	LengthKey:          uint32(atomLength),
	FlagLengthWritable: uint8(objArrayLengthWritable),
}

// jitMaxDeopts is how many eager deoptimizations a function's code makes,
// each invalidating it (jitInvalidate), before the function is compiled no
// more and runs in the existing tiers for good, as V8 gives up optimizing
// a function that keeps deoptimizing. Each compile meanwhile waits for the
// existing tiers' budget, and for its caches' feedback to have settled
// (jitTierUp): a function whose feedback has settled deoptimizes no more,
// and one meeting new shapes in phases -- a compiler's walking a syntax
// tree of new kinds of node -- deoptimizes about once a phase.
const jitMaxDeopts = 64

// jitDeoptimized notes a guard that failed in e's code, at the site
// ExitSite names, or, for -1, an entry's speculation at the entry the
// stretch started at: the code, invalidated, is compiled again with the
// site, or the entry, generic (jitFeedback.Generic), as V8's feedback
// widens once its speculation has failed. A site the builder has no
// generic form for is built as before, and its next failure is not new.
func jitDeoptimized(e *jitEntry, ctx *abi.Context, start int) {
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
	// accessors are the getters a read, or the setters a write, found on
	// the prototypes of receivers of shapes the code was not compiled for
	// (jitAccessorCase), which it is compiled again for.
	accessors []jitAccessorCase
	// mega marks a read that met what no case holds, which the
	// megamorphic cache has (ssa.PropertySite's Megamorphic), or a site
	// that met what the code cannot use (jitDeoptSeen); missed are the
	// receivers' shapes the site deoptimized for, each with the compile of
	// the code that did (jitEntry's ssaSerial).
	mega   bool
	missed []jitMissed
}

// jitMissed is a shape a property site deoptimized for, in the code of the
// compile serial names (jitPolySite's missed).
type jitMissed struct {
	shape  *shape
	serial uint32
}

// jitMissedShapes is how many shapes a site deoptimizes for before it is
// megamorphic: one more than its cases hold.
const jitMissedShapes = jitPropertyCases + 1

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
	if o != nil && int(in.A) < len(f.cl.names) {
		// A setter on the object's prototypes, as for a read's getter: of
		// the shape the object had, which the setter may have changed.
		if a, ok := r.jitAccessorCaseOf(o, before, f.cl.names[in.A], true); ok {
			if jitAddAccessorCase(p, a) {
				e.polyReopt = true
			}
			return
		}
	}
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
		e.polyReopt = true
		return
	}
	if slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape }) {
		return
	}
	p.cases = append(p.cases, c)
	e.polyReopt = true
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
// of the frame's operands, up to sp -- a megamorphic site's, whose others
// deoptimize (jitPropertyDeopt), or an inlined callee's: the megamorphic
// cache is filled for the receiver's shape, which the read then finds
// natively; what that cannot hold -- a getter, a read past the prototypes
// it holds, a string's method -- the code is compiled again for (polyReopt),
// on the existing tiers' budget, its code invalidated meanwhile.
func (r *Runtime) jitPolySeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr) {
	recv := r.stack[sp-1]
	if recv.IsString() && in.Op == bytecode.OpGetPropThis && !slices.Contains(e.strSites, uint32(pc)) {
		// A string's method: read off String.prototype by the code
		// compiled again, as for a shape the read had not met.
		e.strSites = append(e.strSites, uint32(pc))
		e.polyReopt = true
		return
	}
	if recv.IsBigInt() && in.Op == bytecode.OpGetPropThis && !slices.Contains(e.bigSites, uint32(pc)) {
		// A BigInt's: off BigInt.prototype, as a string's.
		e.bigSites = append(e.bigSites, uint32(pc))
		e.polyReopt = true
		return
	}
	if !recv.IsObject() || int(in.A) >= len(f.cl.names) {
		return
	}
	if in.Op == bytecode.OpGetPropThis && int(in.B) < len(f.cl.ic) && !slices.Contains(e.strSites, uint32(pc)) {
		if c := &f.cl.ic[in.B]; (c.shape == nil || c.shape == noShape) && c.fills == 0 && r.proto.str != nil && r.proto.str.findOwn(f.cl.names[in.A]) >= 0 {
			// An object's method, at a read the code took for a string's
			// (jitFeedback's StringProperty), its cache knowing nothing: a
			// loop the tree tier ran before it was compiled, a function
			// run once, leaves no read in the interpreter's cache. The
			// cache takes the receiver, as the interpreter's read would
			// have, and the code is compiled again for its shape; it had
			// left there at every entry, and was judged slow for it.
			r.fillPropCache(c, recv.Object(), f.cl.names[in.A], false)
			if c.shape != nil && c.shape != noShape {
				e.polyReopt = true
				return
			}
		}
	}
	if in.Op == bytecode.OpGetProp {
		// A getter on the receiver's prototypes, which the code calls once
		// compiled again for the receiver's shape too (jitFeedback's
		// Accessor).
		if a, ok := r.jitAccessorCaseOf(recv.Object(), recv.Object().shape, f.cl.names[in.A], false); ok {
			if jitAddAccessorCase(jitPolySiteAt(e, pc), a) {
				e.polyReopt = true
			}
			return
		}
	}
	r.jit.megaFill(recv.Object(), f.cl.names[in.A])
	absent := r.jitMegaFillProto(recv.Object(), f.cl.names[in.A])
	var c propCache
	r.fillPropCache(&c, recv.Object(), f.cl.names[in.A], false)
	if c.shape == nil {
		// One a cache cannot hold, found past its two prototypes.
		if d, ok := r.jitDeepReadOf(recv.Object(), f.cl.names[in.A]); ok && jitAddDeepRead(jitPolySiteAt(e, pc), d) {
			e.polyReopt = true
		} else if p := jitPolySiteAt(e, pc); absent && !p.mega {
			// None its cases could hold -- a property nothing has, a leaf's
			// child -- which the megamorphic cache now says: the code is
			// compiled again to look there (ssa.PropertySite's
			// Megamorphic), rather than leave at every such read.
			p.mega = true
			e.polyReopt = true
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
	if c.idx < int32(len(holder.vals)) && !holder.vals[c.idx].IsNumber() {
		jitNumberMissed(e, pc)
	}
	// A shape it was not compiled for: at a megamorphic site, the
	// megamorphic cache, filled above, has it natively from now on.
}

// jitPropertyOp reports whether op is a named property's read or write,
// whose site's cache the interpreter fills (propCache).
func jitPropertyOp(op bytecode.Op) bool {
	return op == bytecode.OpGetProp || op == bytecode.OpGetPropThis || op == bytecode.OpSetProp
}

// jitPropertyDeopt handles a deoptimization at site of the code owner's
// entry holds, at pc of f's function, its operands up to sp: if the
// instruction there is a named property's read or write, its receiver was
// of a shape the site's cases do not have, or its site knew none
// (ssa.PropertySite), as V8's map checks fail and its code deoptimizes for
// insufficient feedback, and it reports true. The interpreter makes the
// operation from the frame Go wrote, its cache learning the shape -- the
// feedback the code is compiled again with (jitSeenFeedback), as V8's
// inline caches gather it -- and the code, which the caller invalidates
// as any eager deoptimization does, is compiled again for it once the
// existing tiers have spent their budget (jitTierUp). What a cache cannot
// hold, e notes (jitDeoptSeen). A site entry's speculation (site -1) is no
// property's.
func (r *Runtime) jitPropertyDeopt(f *frame, e, owner *jitEntry, site int32, pc, sp int) bool {
	if site < 0 || pc >= len(f.cl.fn.Code) || !jitPropertyOp(f.cl.fn.Code[pc].Op) {
		return false
	}
	serial := uint32(0)
	if owner != nil {
		serial = owner.ssaSerial
	}
	r.jitDeoptSeen(f, e, pc, sp, f.cl.fn.Code[pc], serial)
	if e != nil {
		r.jit.account(e)
	}
	return true
}

// jitSiteMegamorphic reports whether the property site at pc of cl's
// function is megamorphic as its code was compiled (jitFeedback's
// megamorphic): its cache gave up, or e, the function's entry, found what
// no case holds.
func (r *Runtime) jitSiteMegamorphic(cl *closure, e *jitEntry, pc int) bool {
	if in := cl.fn.Code[pc]; int(in.B) < len(cl.ic) && cl.ic[in.B].fills >= maxCacheFills {
		return true
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

// jitDeoptSeen notes in e what the property operation in at pc, which
// deoptimized, meets that its site's cache cannot hold, its operands up to
// sp, before the interpreter makes it: a string's method, read
// off String.prototype by the code compiled again; a getter, or a setter,
// on the receiver's prototypes (jitAccessorCase); a read the cache cannot
// hold for its prototypes' depth (jitDeepRead); a read's value that is no
// number, which the read then takes as a value (jitNumberMissed); and a
// receiver the cache cannot hold at all -- a primitive, a property nothing
// has, an object of no shape -- or one the site deoptimized for before, in
// code compiled before the code that did now (serial), its cache holding
// what the code cannot use (an object's own accessor, an addition Go
// makes), or of more shapes than its cases hold,
// which makes the site megamorphic (jitPolySite's mega), as V8's inline
// caches go megamorphic for what they have no handler for: the megamorphic
// cache, filled here, answers what it can natively, and the rest leaves
// for Go rather than deoptimize again. So each deoptimization at a site
// teaches its cache a shape, which its fills bound (maxCacheFills), or
// makes it megamorphic, once. The shapes the site meets, the cache keeps
// (propCache.seen); a write's cache learns them as the interpreter writes.
func (r *Runtime) jitDeoptSeen(f *frame, e *jitEntry, pc, sp int, in bytecode.Instr, serial uint32) {
	if int(in.A) >= len(f.cl.names) {
		return
	}
	if e == nil {
		// Nothing to note in: the cache learns what it can.
		e = &jitEntry{}
	}
	key := f.cl.names[in.A]
	write := in.Op == bytecode.OpSetProp
	at := sp - 1
	if write {
		at = sp - 2
	}
	if at < f.base || at >= len(r.stack) {
		return
	}
	recv := r.stack[at]
	if recv.IsBigInt() && in.Op == bytecode.OpGetPropThis && !slices.Contains(e.bigSites, uint32(pc)) {
		// A BigInt's method: read off BigInt.prototype by the code compiled
		// again, the site no polymorphic one (jitFeedback's StringProperty).
		e.bigSites = append(e.bigSites, uint32(pc))
		return
	}
	p := jitPolySiteAt(e, pc)
	if recv.IsString() && in.Op == bytecode.OpGetPropThis && !slices.Contains(e.strSites, uint32(pc)) {
		// A string's method: read off String.prototype by the code
		// compiled again.
		e.strSites = append(e.strSites, uint32(pc))
		return
	}
	if !recv.IsObject() || int(in.B) >= len(f.cl.ic) {
		p.mega = true
		return
	}
	o := recv.Object()
	i := slices.IndexFunc(p.missed, func(m jitMissed) bool { return m.shape == o.shape })
	if o.shape == nil || i >= 0 && p.missed[i].serial != serial || i < 0 && len(p.missed) >= jitMissedShapes {
		// An object with no shape, which the VM gives one only once a
		// cache asks -- an empty one, made anew each time; one of a shape
		// the site deoptimized for before, compiled again since: what its
		// cache holds of it, the code cannot use; or more shapes than its
		// cases hold.
		p.mega = true
		if !write {
			r.jit.megaFill(o, key)
			r.jitMegaFillProto(o, key)
		}
		return
	}
	if i >= 0 {
		// Again in the code that deoptimized for it, not compiled again
		// yet: the cache has it, and the code will.
		return
	}
	p.missed = append(p.missed, jitMissed{remember(o.shape), serial})
	if a, ok := r.jitAccessorCaseOf(o, o.shape, key, write); ok {
		// A getter, or a setter, on its prototypes, which the code calls
		// once compiled again for the receiver's shape too (Accessor).
		jitAddAccessorCase(p, a)
		return
	}
	c := &f.cl.ic[in.B]
	if write || c.fills >= maxCacheFills {
		// A write's cache learns as the interpreter writes -- an own
		// property, an addition, or, for one it cannot hold, a fill all the
		// same, toward giving up (setPropCached); a cache that has given
		// up is megamorphic.
		return
	}
	// The site's cache learns the receiver's shape here, as the tree
	// tier's read would teach it (ownScan, cachedGet): the interpreter,
	// which makes the read after, reads an object's small table without
	// it (plainOwn).
	if c.holder(o) == nil {
		r.fillPropCache(c, o, key, in.Op == bytecode.OpGetProp)
	}
	if c.fills >= maxCacheFills {
		return
	}
	if c.holder(o) == nil {
		// One a cache cannot hold: past its two prototypes, or none of
		// them has it, or not of a shape the cache may hold.
		if d, ok := r.jitDeepReadOf(o, key); ok && jitAddDeepRead(p, d) {
			return
		}
		r.jit.megaFill(o, key)
		r.jitMegaFillProto(o, key)
		p.mega = true
		return
	}
	if c.getter || c.idx < 0 {
		return
	}
	holder := o
	if c.p2 != nil {
		holder = c.p2
	} else if c.p1 != nil {
		holder = c.p1
	}
	if c.idx < int32(len(holder.vals)) && !holder.vals[c.idx].IsNumber() {
		jitNumberMissed(e, pc)
	}
}

// megaFill has the megamorphic cache (abi.MegaCache) name o's own data
// property key, if it has one, for objects of its shape: a property
// operation that met more shapes than it was compiled for, of an object
// too large to search, finds it there natively. AST nodes of a dozen
// fields and twenty kinds, Octane's Typescript's, had their every read
// and write at such sites left to Go.
func (s *jitState) megaFill(o *Object, key Atom) {
	sh := o.shape
	if s.mega == nil || sh == nil || sh.loose || o.class != ClassObject && o.class != ClassFunction || len(o.vals) <= abi.MaxScan {
		return
	}
	i := o.findOwn(key)
	if i < 0 || int(i) >= len(sh.keys) || int(i) >= len(o.vals) {
		return
	}
	flags := sh.keys[i].flags
	if flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return
	}
	idx := uint32(i)
	if flags&propWritable != 0 {
		idx |= abi.MegaWritable
	}
	// Remembered, so that it never changes, as any cache's shape.
	remember(sh)
	s.megaPut(abi.MegaEntry{Shape: unsafe.Pointer(sh), Key: uint32(key), Index: idx})
}

// megaPut puts n into its set of the megamorphic cache, first, the others
// moved down: an entry of its shape and key replaced, or, with none, the
// set's last let go.
func (s *jitState) megaPut(n abi.MegaEntry) {
	set := s.mega[abi.MegaSet(uintptr(n.Shape), n.Key):][:abi.MegaWays]
	at := len(set) - 1
	for w := range set {
		if set[w].Shape == n.Shape && set[w].Key == n.Key {
			at = w
			break
		}
	}
	copy(set[1:at+1], set[:at])
	set[0] = n
}

// jitNoteChainRead has the megamorphic cache name what a read of key from o
// found up its chain, or that nothing on it has the key, as an interpreter
// cache remembers the read by the chain's cell (fillChainCache): native
// code reads such a site megamorphically (jitFeedback.megamorphic), and
// finds it there from its first run, rather than after its first exit.
func (r *Runtime) jitNoteChainRead(o *Object, key Atom) {
	if r.jit == nil {
		return
	}
	if r.jit.mega == nil {
		r.jit.mega = new(abi.MegaCache)
	}
	r.jitMegaFillProto(o, key)
}

// jitMegaFillProto has the megamorphic cache name the data property key a
// prototype of o has, or that none has, o having none of its own, for
// objects of o's shape:
// found along the chain from o's prototype on, up to abi.MegaDepth of
// them, each of a class with shapes that does not answer the key before
// it, as a cache's walk (fillPropCache, jitDeepReadOf) requires; each with
// its shape, remembered, which native code compares (mir's mega). A read
// whose site met more shapes than it was compiled for -- a method of a
// base class, read of objects of many subclasses -- then finds it
// natively, as V8's megamorphic loads do, where it had left native code.
func (r *Runtime) jitMegaFillProto(o *Object, key Atom) (absent bool) {
	s := r.jit
	sh := o.shape
	if s.mega == nil || sh == nil || sh == noShape || sh.loose || o.class != ClassObject && o.class != ClassFunction ||
		key.IsIndex() || synthesized(o.class, key) || o.findOwn(key) >= 0 {
		return false
	}
	chain := new(abi.MegaChain)
	n := 0
	for p := o.proto; p != nil; p = p.proto {
		if n == abi.MegaDepth || r.ensureProtoShape(p) == nil || p.cacheShape() == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return false
		}
		chain[n] = abi.MegaLink{Object: unsafe.Pointer(p), Shape: unsafe.Pointer(p.shape)}
		n++
		i := p.findOwn(key)
		if i < 0 {
			continue
		}
		if i >= p.shape.n || int(i) >= len(p.vals) || p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
			return false
		}
		remember(sh)
		for _, l := range chain[:n] {
			remember((*shape)(l.Shape))
		}
		s.megaPut(abi.MegaEntry{Shape: unsafe.Pointer(sh), Chain: chain, Key: uint32(key), Index: uint32(i)})
		return false
	}
	// On none of them: the read's value is undefined, the chain all of
	// them (abi.MegaAbsent), as V8's loads of a nonexistent property find
	// it. Box2D's tree nodes, given a child only once they have one, read
	// a leaf's child1 so at every IsLeaf, which had left native code.
	remember(sh)
	for _, l := range chain[:n] {
		remember((*shape)(l.Shape))
	}
	s.megaPut(abi.MegaEntry{Shape: unsafe.Pointer(sh), Chain: chain, Key: uint32(key), Index: abi.MegaAbsent})
	return true
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
	if sh == nil || sh == noShape || sh.loose || i < 0 || i >= sh.n || int(i) >= len(o.vals) {
		return
	}
	if p := o.shape.keys[i]; p.isAccessor() || in.Op == bytecode.OpSetPrivate && p.flags&propWritable == 0 {
		return
	} else if in.Op == bytecode.OpGetPrivate && !o.vals[i].IsNumber() {
		// A read that met a value not a number reads it as a value.
		jitNumberMissed(e, pc)
	}
	if jitAddReadCase(jitPolySiteAt(e, pc), propCache{shape: sh, idx: i}) {
		e.polyReopt = true
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
		e = fb.r.jit.entryFor(fb.fn)
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
	if slices.Contains(e.failed, int32(pc)) {
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
			if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || len(d.up) <= 2 {
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
	old.seen, old.poly = nil, nil
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
// their entries (propCache.poly, propCache.seen): the shapes the interpreter and
// the tree tier met at each property read and write, which the code is
// compiled for from the first, as V8's optimizing compiler takes the maps
// its inline caches met, not learned one exit and one compile at a time.
// Each is taken as an exit's would be (jitPolySeen, jitPolyWriteSeen).
func (r *Runtime) jitSeenFeedback(cl *closure, e *jitEntry) {
	for pc, in := range cl.fn.Code {
		write := in.Op == bytecode.OpSetProp
		if !write && in.Op != bytecode.OpGetProp && in.Op != bytecode.OpGetPropThis || int(in.B) >= len(cl.ic) || cl.ic[in.B].seen == nil && cl.ic[in.B].poly == nil {
			continue
		}
		seen, now := cl.ic[in.B].seen, cl.ic[in.B].shape
		// What the site remembers besides its entry (poly), which every
		// tier's reads and writes fill -- the tree's scans of an object's
		// own table too, which note nothing in seen -- and what it met
		// before that (seen). A chain's cell is no entry the code is
		// compiled for (megamorphic).
		var met []propCache
		if seen != nil {
			met = append(met, seen.entries[:seen.n]...)
		}
		if p := cl.ic[in.B].poly; p != nil {
			for _, c := range p.entries[:p.n] {
				if c.cell == nil {
					met = append(met, c)
				}
			}
		}
		for _, c := range met {
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
		if seen == nil {
			continue
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
	// fn is the function the literal is of: e's, or a callee's e's code
	// inlines.
	fn *bytecode.Function
	// tmpl is a closure's function template (closure), whose objects are
	// its functions, made as the VM makes them in fn's frames, their
	// captured bindings' cells unwritten (jitFillClosures); env is fn's
	// closures' environment.
	tmpl Value
	env  *Object
}

// jitLiteralAt is e's literal at pc of cl's function -- e's, or a callee's
// its code inlines -- made for cl's site if it has none, or nil if the
// literal's objects have no shape to start from.
func (r *Runtime) jitLiteralAt(e *jitEntry, cl *closure, pc int) *jitLiteral {
	for i := range e.literals {
		if int(e.literals[i].pc) == pc && e.literals[i].fn == cl.fn {
			return &e.literals[i]
		}
	}
	in := cl.fn.Code[pc]
	if in.Op == bytecode.OpNewArray && in.A <= ir.MaxArrayLiteral {
		e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), array: true, n: int(in.A), fn: cl.fn})
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
		e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), lit: lit, fn: cl.fn})
		l := &e.literals[len(e.literals)-1]
		l.pool.Source = unsafe.Pointer(&jitPoolSource{lit: lit})
		r.jitFillLiteral(l)
		return l
	}
	if in.Op == bytecode.OpClosure {
		if int(in.A) >= len(cl.consts) || cl.consts[in.A].template() == nil {
			return nil
		}
		tmpl := cl.consts[in.A]
		e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), fn: cl.fn, tmpl: tmpl, env: cl.env})
		l := &e.literals[len(e.literals)-1]
		l.pool.Source = unsafe.Pointer(&jitPoolSource{tmpl: tmpl, fn: cl.fn, env: cl.env})
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
	e.literals = append(e.literals, jitLiteral{pc: int32(pc), root: root, pool: new(abi.ObjectPool), n: int(in.A), fn: cl.fn})
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
	if l.tmpl.template() != nil {
		r.jitFillClosures(pool, l.tmpl, l.fn, l.env)
		return
	}
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
	if !l.array {
		n := l.n
		if l.root != nil {
			n = max(n, int(l.root.slack))
		}
		jitFillObjectPool(pool, r.proto.object, l.root, n)
		return
	}
	for i := range int(pool.Size) {
		a := newArrayObject(r.proto.array, l.n)
		a.extOf().elems = a.elemsOf()[:0]
		r.ensureShape(a)
		pool.Objects[i] = unsafe.Pointer(a)
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.array)
}

// jitDefineSeen notes an exit at a literal's field, pc, that the code was
// compiled for before its transition was made: Go, defining it, made it,
// and the code is compiled again for it (polyReopt), as for any feedback it
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
		if l := &e.literals[i]; int(l.pc) == pc && l.fn == f.cl.fn && l.pool.Count == 0 {
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

// BigIntCell is ssa.BigIntConstFeedback's: the cell a copy of the BigInt
// constant pushed at pc is in, as StringCell's, and its bits if it is held
// in its value: of one in the int64 range, or of one from 2^63 to 2^64-1.
func (fb *jitFeedback) BigIntCell(pc int) (uintptr, uint64, int, bool) {
	if fb.cl == nil || pc >= len(fb.fn.Code) {
		return 0, 0, ssa.BigConstHeap, false
	}
	in := fb.fn.Code[pc]
	if in.Op != bytecode.OpPushConst || int(in.A) >= len(fb.cl.consts) || !fb.cl.consts[in.A].IsBigInt() {
		return 0, 0, ssa.BigConstHeap, false
	}
	cell := new(Value)
	*cell = fb.cl.consts[in.A]
	k := fb.keep()
	k.cells = append(k.cells, cell)
	if cell.isInt64Big() {
		return uintptr(unsafe.Pointer(cell)), uint64(cell.int64Big()), ssa.BigConstInt64, true
	}
	if cell.isShortBig() {
		if neg, mag := cell.shortBigParts(); !neg {
			return uintptr(unsafe.Pointer(cell)), mag, ssa.BigConstUint64, true
		}
	}
	return uintptr(unsafe.Pointer(cell)), 0, ssa.BigConstHeap, true
}

// Literal is ssa.LiteralFeedback's: the object literal at pc, made natively
// from its pool, in the function's own code.
func (fb *jitFeedback) Literal(pc int) (ssa.LiteralSite, bool) {
	// An inlined callee's too, from pools the code that inlines it keeps
	// (the root's entry), as it keeps its own: inlined, Typescript's
	// errorSink = [] and commentStack = [] had left native code at every
	// one.
	e := fb.e
	if fb.root != nil {
		e = fb.root.e
	}
	if e == nil || fb.fwd != nil || fb.cl == nil || pc >= len(fb.fn.Code) {
		return ssa.LiteralSite{}, false
	}
	l := fb.r.jitLiteralAt(e, fb.cl, pc)
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

// Closure is ssa.ClosureFeedback's: the closure made at pc, natively from
// its pool, in the function's own code, whose captured locals are in cells
// of their own -- not an arrow that reads its maker's new.target or super,
// which the pool's are made without (jitNeedsMaker), nor one made in a
// derived constructor, whose this is not a value until super() runs.
func (fb *jitFeedback) Closure(pc int) (ssa.ClosureSite, bool) {
	e := fb.e
	if e == nil || fb.root != nil || fb.fwd != nil || fb.cl == nil || pc >= len(fb.fn.Code) ||
		fb.fn.Code[pc].Op != bytecode.OpClosure || fb.fn.Kind == bytecode.KindDerivedConstructor {
		return ssa.ClosureSite{}, false
	}
	in := fb.fn.Code[pc]
	if int(in.A) >= len(fb.cl.consts) || jitNeedsMaker(fb.cl.consts[in.A]) {
		return ssa.ClosureSite{}, false
	}
	t := fb.cl.consts[in.A].template()
	if t == nil {
		return ssa.ClosureSite{}, false
	}
	l := fb.r.jitLiteralAt(e, fb.cl, pc)
	if l == nil {
		return ssa.ClosureSite{}, false
	}
	caps := make([]int32, len(t.fn.Upvalues))
	for i, desc := range t.fn.Upvalues {
		if desc.FromParent {
			caps[i] = int32(cellOfLocal(fb.fn, int(desc.Index)))<<1 | 1
		} else {
			caps[i] = int32(desc.Index) << 1
		}
	}
	fb.keep().pools = append(fb.keep().pools, l.pool)
	lexThis := t.fn.Kind == bytecode.KindArrow
	fb.lexThis = fb.lexThis || lexThis
	return ssa.ClosureSite{Pool: uintptr(unsafe.Pointer(l.pool)), Captures: caps, LexThis: lexThis}, true
}

// jitFillClosures fills pool with Size functions of the template tmpl as
// the VM makes them in a frame of fn's, its closures' environment env:
// their captured bindings' cells nil, and an arrow's this, new.target and
// home object those of no frame, which native code writes as it takes one
// (mir's closureTake) -- the cells and the this; an arrow that reads the
// others its pool is not made for (jitFeedback's Closure).
func (r *Runtime) jitFillClosures(pool *abi.ObjectPool, tmpl Value, fn *bytecode.Function, env *Object) {
	f := &frame{cl: &closure{fn: fn, env: env, upvalues: make([]*upvalue, len(fn.Upvalues))}, this: Undefined, newTarget: Undefined}
	cells := make([]*upvalue, jitCaptured(fn))
	for i := range int(pool.Size) {
		pool.Objects[i] = unsafe.Pointer(r.newClosureIn(f, tmpl, cells))
	}
	pool.Count, pool.Proto = pool.Size, nil
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

// reoptPending reports whether e's code is to be compiled again, for any
// of the reasons jitReoptimize takes: what it learned at its exits to make
// natively -- a speculation that failed, a shape, a call to inline, a
// callee that inlines more -- or that it leaves too often called natively.
// Code whose guard failed is invalidated (jitInvalidate) and compiled
// again on the existing tiers' budget (jitTierUp); code that learned
// runs on, and is compiled again once it has spent a budget itself
// (jitLearnedDue).
func (e *jitEntry) reoptPending() bool {
	return e.reopt || e.inlineReopt || e.polyReopt || e.upgradeReopt || e.unwindReopt
}

// native reports whether Go may enter e's code, and go on in it after an
// exit: code that does not pay (entrySlow) the existing tiers run, as they
// run code invalidated (jitInvalidate).
func (e *jitEntry) native() bool { return !e.entrySlow && !e.invalid }

// jitRecompileLearned compiles e's code, cl's function's, again for what
// it learned at its exits, if that is due (jitLearnedDue).
func (r *Runtime) jitRecompileLearned(cl *closure, e *jitEntry) {
	if e != nil && r.jitLearnedDue(cl, e) {
		r.jitReoptimize(cl, e)
	}
}

// jitLearnedDue reports whether e's code, cl's function's, valid, is due
// to be compiled again for what it learned at its exits (reoptPending):
// once it has spent a budget (jitTierBudget) since it began to learn, with
// nothing learned meanwhile (jitLearnedMark), as invalidated code waits
// for one (jitTierUp). It is not invalidated for it, as V8 never
// invalidates code for feedback: what it learns at its exits -- a call's
// target at a call of Go, an element's kind at a host exit -- only it
// learns, as V8's baseline code gathers feedback, and the existing tiers,
// whose caches note no such thing, would not learn it in its place. So it
// runs on until its budget is spent, and is replaced by code compiled for
// all it learned. Invalidated for it, a function learned a call a
// compile, each after a budget of the existing tiers: AWFY's CD's recurse
// ran out of compiles with two of its calls still made by Go, judged
// slow, its recursion in the tree tier.
func (r *Runtime) jitLearnedDue(cl *closure, e *jitEntry) bool {
	if e.invalid || e.ssa == nil {
		return false
	}
	if !e.reoptPending() {
		e.learning = false
		return false
	}
	spent := jitLearnedSpent(cl.fn, e)
	if !e.learning {
		e.learning, e.tierBase, e.tierMark = true, spent, jitLearnedMark(cl, e)
		e.tierBudget = r.jitTierBudget(cl.fn, e)
		return false
	}
	if spent-e.tierBase < e.tierBudget {
		return false
	}
	if m := jitLearnedMark(cl, e); m != e.tierMark {
		// Learned more meanwhile: what is left raised, as at a change of
		// the caches' feedback (jitICBudget), for it all.
		e.tierBudget = max(e.tierBudget-(spent-e.tierBase), r.jitICBudget(cl.fn))
		e.tierBase, e.tierMark = spent, m
		return false
	}
	return true
}

// jitLearnedSpent is the work e's code has done since it was compiled, as
// the existing tiers' budget counts it (jitTierBudget): its invocations,
// each fn's length -- Go's entries, native callers' calls, calls of it the
// existing tiers ran for code Go does not enter -- the work its native
// stretches did, their loops' included (jitSSAProfit, jitCalleeProfit),
// and the existing tiers' in its loops before Go entered it at one
// (osrWork). A function entered only by on-stack replacement, each
// invocation leaving its code at once for the interpreter to finish --
// microbench_extra's PrivateCounter's add, at a private read of a shape
// learned at that exit -- had spent nothing, and was never compiled
// again for it, as V8's lower tiers spend the budget however a function
// runs.
func jitLearnedSpent(fn *bytecode.Function, e *jitEntry) int64 {
	st := &e.ssaStats
	calls := e.starts + e.link.in + e.slowCalls
	return int64(calls)*int64(max(len(fn.Code), 1)) + int64(st.work+st.calleeWork) + e.osrWork
}

// jitLearnedMark is jitFeedbackMark of cl's caches with what e's code has
// learned at its exits counted in: a change of either delays the compile
// for it (jitLearnedDue), as V8's NotifyICChanged does.
func jitLearnedMark(cl *closure, e *jitEntry) uint64 {
	n := len(e.failed) + len(e.failedEntries) + len(e.bigints) + len(e.bigwide) + len(e.poly) + len(e.typedReads) + len(e.stringReads) +
		len(e.keyedReads) + len(e.keyedWrites) + len(e.strSites) + len(e.nativeCalls) + len(e.inlines) + len(e.notInline) +
		len(e.literals) + len(e.definesUnfed)
	return jitFeedbackMark(cl) + uint64(n)*0x9e3779b97f4a7c15
}

// jitInvalidate invalidates e's code, cl's function's, for an eager
// deoptimization: Go enters it no more (native), nor do native callers
// call it, its link cleared, but Go calls the function, as V8's
// deoptimizer invalidates a
// function's current code -- "only invalidated code affects tiering
// decisions" -- its tiering requests reset and its interrupt budget with
// them. The existing tiers run the function until they have spent a fresh
// budget (jitTierUp). Runs of the code under way go on in it until they
// next leave it, as V8's frames of invalidated code deoptimize lazily, at
// their next return to it.
func (r *Runtime) jitInvalidate(cl *closure, e *jitEntry) {
	if e == nil || e.ssa == nil {
		return
	}
	if e.deopts < ^uint16(0) {
		e.deopts++
	}
	if e.invalid {
		return
	}
	e.invalid = true
	if e.owned() {
		e.link.entry, e.link.body = 0, 0
	}
	e.tierBudget, e.tierMark = r.jitTierBudget(cl.fn, e), jitFeedbackMark(cl)
	r.jit.invalidated++
	// What it learned for the compile to come, counted as it grows, as a
	// compile counts it: the code waits for the budget meanwhile.
	r.jit.account(e)
	r.jit.settle()
}

// jitTierBudget is the work the existing tiers do with fn, e's function,
// before it is compiled, or compiled again: jitCallThreshold invocations
// (jitInvocationsForMaglev), each worth its instructions -- as V8's
// interrupt budget is so many invocations of its bytecode's length.
func (r *Runtime) jitTierBudget(fn *bytecode.Function, e *jitEntry) int64 {
	return int64(max(r.jitCallThreshold, 1)) * int64(max(len(fn.Code), 1))
}

// jitICBudget is what a change of fn's caches' feedback raises what is
// left of its budget to (jitICThreshold invocations' worth), as
// TieringManager::NotifyICChanged raises V8's.
func (r *Runtime) jitICBudget(fn *bytecode.Function) int64 {
	return int64(max(r.jitICThreshold, 1)) * int64(max(len(fn.Code), 1))
}

// jitTierUp spends work of the budget of e's invalidated code, cl's
// function's, which the existing tiers did -- an invocation, or a loop's
// full budget of back edges -- and reports whether the code was compiled
// again for what it learned, the budget spent, and Go may enter it. The
// function's caches' feedback changed since the budget began (a fill, a
// shape held before, a read past what a cache holds) has it wait for a
// fresh budget, as V8's NotifyICChanged delays a function's tier-up: what
// its sites meet settles where it is gathered, and one compile takes it
// all. Past jitMaxDeopts it is compiled no more.
func (r *Runtime) jitTierUp(cl *closure, e *jitEntry, work int64) bool {
	e.tierSpends++
	if e.deopts > jitMaxDeopts {
		return false
	}
	if e.tierBudget -= work; e.tierBudget > 0 {
		return false
	}
	if m := jitFeedbackMark(cl); m != e.tierMark {
		e.tierBudget, e.tierMark = max(e.tierBudget, r.jitICBudget(cl.fn)), m
		return false
	}
	r.jitReoptimize(cl, e)
	if e.invalid {
		// Not compiled again -- its code in use below, no room, a
		// refusal: tried again once a budget more is spent.
		e.tierBudget = r.jitTierBudget(cl.fn, e)
		return false
	}
	r.jit.tieredUp++
	return true
}

// jitFeedbackMark sums what cl's function's caches hold: each entry a
// cache holds or held before (propCache.seen), by its shape, prototypes and
// index, whether it gave up (maxCacheFills), and the reads and additions
// it noted past what one holds -- its reads and writes' sites, its
// globals', and through them its calls' targets. What a cache learns
// changes it; a cache filled again with what it held before -- a fresh
// array's method, read again at each invocation -- does not, nor the
// order its entries came in.
func jitFeedbackMark(cl *closure) uint64 {
	var m uint64
	for i := range cl.ic {
		c := &cl.ic[i]
		if c.fills >= maxCacheFills {
			m += 0x9e3779b97f4a7c15 * uint64(i+1)
		}
		var held [1 + maxPolyEntries + len(propSeen{}.entries)]*shape
		n := 0
		entry := func(x *propCache) {
			if x.shape == nil || x.shape == noShape {
				return
			}
			for _, h := range held[:n] {
				if h == x.shape {
					return
				}
			}
			if n < len(held) {
				held[n] = x.shape
				n++
			}
			h := uint64(uintptr(unsafe.Pointer(x.shape)))*31 + uint64(uintptr(unsafe.Pointer(x.p1)))*37 +
				uint64(uintptr(unsafe.Pointer(x.s1)))*41 + uint64(uintptr(unsafe.Pointer(x.p2)))*43 +
				uint64(uintptr(unsafe.Pointer(x.s2)))*47 + uint64(uintptr(unsafe.Pointer(x.next)))*53 + uint64(uint32(x.idx))*59
			if x.getter {
				h++
			}
			m += h * uint64(i+1)
		}
		entry(c)
		if p := c.poly; p != nil {
			for k := range p.entries[:p.n] {
				entry(&p.entries[k])
			}
		}
		if c.seen != nil {
			for k := range c.seen.entries[:c.seen.n] {
				entry(&c.seen.entries[k])
			}
			m += uint64(len(c.seen.reads))<<32 + uint64(len(c.seen.deep))<<40
		}
	}
	return m
}

// jitReoptimize compiles cl's function again for e, its entry, for what it
// learned -- what failed in its code generic, the shapes and calls its
// caches met -- and replaces its code, as V8 installs new code while older
// frames are in the old: runs of the old Go is inside go on from their
// frames, never in it (otherwiseInUse). Its code, invalidated, is valid
// again. If the function no longer compiles, the code it has is kept, and
// stays invalid.
func (r *Runtime) jitReoptimize(cl *closure, e *jitEntry) {
	if !e.owned() {
		// The cache dropped it to make room while its code ran, which
		// goes on for now. Code compiled for it would be called through
		// the link its function's next entry holds, which the cache does
		// not hold it for: once it is gone, its code is released, and
		// native callers would call where the code was. Its function's
		// next entry is compiled as it is called.
		return
	}
	if r.jit.otherwiseInUse(e) {
		// A native call of its code goes on in it later: compiled again
		// once it has returned.
		return
	}
	switch {
	case e.reopt:
		e.reopts++
	case e.inlineReopt:
		e.inlineReopts++
	case e.upgradeReopt:
		e.upgradeReopts++
	}
	if e.polyReopt {
		e.polyReopts++
	}
	e.reopt, e.inlineReopt, e.polyReopt, e.upgradeReopt, e.unwindReopt = false, false, false, false, false
	if e.reoptRoom > 0 && !r.jit.roomFor(e, nil, e.bytes+e.reoptRoom) {
		// Its last compile was refused the room its code needed, the
		// budget full of code too young to drop: not compiled again until
		// there is that much, as a first compile asks for its room before
		// it compiles. A compile thrown away takes as long as one kept --
		// Typescript's, inlining more each time, a few milliseconds each,
		// had been an eighth of its run.
		return
	}
	fn := cl.fn
	r.jitInlineNativeCalls(e)
	kind := jitLowerSSA
	switch {
	case e.ssaCallee:
		kind = jitLowerInline
	case e.hot:
		kind = jitLowerHot
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
	e.ssa, e.ssaShapes, e.ssaHolders, e.ssaStats = code, fb.shapes, fb.holders, jitSSAStats{}
	// Its Go entries write This for the arrows it takes from pools, and
	// clear the loops' word, as the new code has them.
	e.lexThis, e.loops = fb.lexThis, fb.loops
	e.setLoopSlots(fn, fb.loopSlots)
	e.ssaSerial++
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
	e.link.in, e.nativeOut = 0, 0
	e.notNative, e.nativeRetry, e.invalid, e.learning = false, 0, false, false
	if !e.ssaCallee {
		e.entrySlow = false
	}
	e.link.setCode(code)
	r.jit.callRefs = max(r.jit.callRefs, uint64(code.CallRefs()))
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
		return nil, nil, fmt.Errorf("%w: a program of globals, or of another frame", jitRefused)
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
	s.workAt = s.epoch() + 1
	defer s.ssaWork.Rewind()
	defer s.mirWork.Rewind()
	f, err := ssa.BuildIn(&s.ssaWork, p, fb)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", jitRefused, err)
	}
	if f.Loops {
		fb.loopSlots = f.LoopsEnd - p.Locals
	}
	f.FrameLocals = fn.LocalCount
	f.Params, f.StartOnly = fn.ParamCount, jitcompile.StartOnly(fn)
	f.Cells = p.Cells
	// Native callers call a function's code at its call entry, which makes
	// its frame (mir's callEntry): code a call makes no frame of its own
	// for -- top-level code's, a generator's, an async function's -- has
	// none.
	f.CallEntry = !fn.TopLevel && !fn.IsModule && !fn.Async && !fn.Generator && !isGeneratorTemplate(fn)
	if p.This {
		f.ThisSlot = fn.LocalCount + len(fn.Upvalues)
	}
	ssa.Optimize(f)
	if jitSSAOptimized != nil {
		jitSSAOptimized(fn, f)
	}
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
		return nil, nil, fmt.Errorf("%w: %v", jitRefused, err)
	}
	// The cells the backend gave the kept values (mir's keep cells).
	fb.keeps = f.Keeps
	s.ssaKeeps = max(s.ssaKeeps, f.Keeps)
	need := jitEntryFixed + jit.CodeFootprint(mc) + fb.compiledBytes()
	if e != nil {
		need += e.learnedBytes()
	}
	// The hint table's slot, which remember takes once it is placed: a
	// step when the table grows, not a function's bytes (learn).
	hint := s.hintRoom(e)
	need += hint
	// The lists of native callers its callees keep, which it joins once
	// placed (setLinks).
	need += s.callersGrowth(e, fb.links)
	if !s.roomFor(e, fb.links, need) {
		// Compiled for nothing: the room it was short of is what a compile
		// of it again asks for first (jitReoptimize).
		s.discarded++
		if e != nil {
			e.reoptRoom = max(need-e.bytes, 1)
		}
		return nil, nil, jit.ErrCodeBudget
	}
	if e != nil {
		e.reoptRoom = 0
	}
	code, err := jit.NewSSACode(r.jit.arena, mc)
	if err != nil {
		return nil, nil, err
	}
	// What the entry holds, its function's program and the lowering that
	// holds it (jitLowering, its own bytes too), and its link, which the
	// entry makes before it is compiled (linkFor): the room a compile asks
	// for first, which had come short of all three at a full budget, and
	// then of the lowering's own bytes -- a compile that met a budget
	// within them was made and thrown away.
	s.learn(len(fn.Code), need-hint+p.Footprint()+jitLoweringFixed+jitLinkFixed)
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
	// lexThis says the code makes an arrow from a pool (Closure), whose
	// this native code writes from the context's This; loops that it makes
	// a loop of a forEach (ForEach), and loopSlots how far past the frame's
	// operands' first their slots go.
	lexThis, loops bool
	loopSlots      int
	r              *Runtime
	fn             *bytecode.Function
	cl             *closure
	shapes         []*shape
	// holders are the prototypes the code compares receivers' with
	// (ssa.Holder), held by address, which the entry keeps alive.
	holders []*Object
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
	// for native callers: compiled after it (jitWantedCallees). linked
	// are the callees it calls through their links before they have code,
	// compiled after it too, which native code then calls, the caller's code
	// as it is.
	wanted, linked []*closure
	// keeps is how many keep cells the code uses (ssa.Func.Keeps), and
	// keepLimit how many the build may number (ssa.KeepFeedback), or 0.
	keeps, keepLimit int
	// e is the entry being compiled again, whose failed speculations
	// (jitEntry.failed) are built generic; nil for a first compile.
	e *jitEntry
	// fwd, for a forwarding constructor's inlined frame, is the
	// construction it is inlined at, whose receiver the frame's sites are
	// known from (jitForwardFrame). fwdArgs, for a frame of a function that
	// only forwards its arguments (bytecode's ForwardsArguments), whose
	// code ran and filled its caches, is the construction whose receiver
	// its apply_arguments calls are made with (jitForwardSiteTarget).
	fwd, fwdArgs *jitForwardFrame
	// prog is the function lowered for inlining, its stack depths what
	// Intrinsic reads, made on first use.
	prog *ir.Program
	// targetsBy are the calls e's code learned (jitTargets), by PC, once
	// NativeCalls asks.
	targetsBy map[int][]*jitInline
}

// CharCode is ssa.CharCodeFeedback's: whether the method call of one
// argument at pc calls String.prototype.charCodeAt, the realm's own, known
// by its target, whatever the method's name, as V8 reduces a call: the
// call has been seen to call it, as Array.prototype.push's and pop's are
// (jitCallSeen, jitSeedBuiltin) -- in fn's own code, or, for a callee
// inlined, which has no feedback of its own, in the code of its entry --
// or the method is read by the get_prop_this before it, the stack never
// below it since, of a name String.prototype has the intrinsic at now,
// as V8 folds a load from a string to the prototype's constant and the
// call of it to the builtin's operation. The new pipeline reads a
// string's charCodeAt so (ir.StringMethod). The code checks the receiver
// is a string and the function the intrinsic.
func (fb *jitFeedback) CharCode(pc int) bool {
	fn, r := fb.fn, fb.r
	if pc < 2 || pc >= len(fn.Code) || jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod || fn.Code[pc].A != 1 || !r.jitCharCodeAt.IsObject() {
		return false
	}
	e := fb.e
	if fb.root != nil || e == nil {
		e = r.jit.entryFor(fn)
	}
	if e != nil {
		for _, x := range jitTargets(e) {
			if int(x.pc) == pc && x.charCode {
				return true
			}
		}
	}
	m := fb.stringMethodRead(pc, 1)
	return m.IsObject() && m.StrictEquals(r.jitCharCodeAt)
}

// stringMethodRead is String.prototype's own data property, as it is now,
// of the name the get_prop_this before the method call of argc arguments at
// pc reads, the stack never below the function since -- as V8 folds a load
// from a string to the prototype's constant -- or undefined. The
// interpreter's cache keeps nothing of a string's reads: a call of
// charCodeAt, or substring, is known so before it has left native code.
func (fb *jitFeedback) stringMethodRead(pc, argc int) Value {
	fn, r := fb.fn, fb.r
	if fb.prog == nil {
		p, err := r.jitLower(fn, jitLowerInline)
		if err != nil {
			return Undefined
		}
		fb.prog = p
	}
	p := fb.prog
	if len(p.Maps) != len(fn.Code) || p.Maps[pc].Depth < argc+2 {
		return Undefined
	}
	// The function's slot: the read is the instruction at its depth, the
	// arguments' above it.
	slot := p.Maps[pc].Depth - argc - 1
	for k := pc - 1; k >= 0; k-- {
		d := p.Maps[k].Depth
		if d == slot && fn.Code[k].Op == bytecode.OpGetPropThis {
			if fb.cl == nil || int(fn.Code[k].A) >= len(fb.cl.names) {
				return Undefined
			}
			m := r.proto.str.getOwn(fb.cl.names[fn.Code[k].A])
			if !m.ok() || m.flags()&^propDefault != 0 {
				return Undefined
			}
			return m.value()
		}
		if d < slot+1 {
			return Undefined
		}
	}
	return Undefined
}

// Substring is ssa.SubstringFeedback's: the address of
// String.prototype's substring, slice, substr or charAt, the realm's own,
// if the method call at pc calls it, known as charCodeAt's is -- the call
// seen to call it (jitCallSeen, jitSeedBuiltin), in fn's own code -- and
// which it is (ssa.OpSubstringChar's Aux). The code checks it calls it,
// and makes a one-character result itself.
func (fb *jitFeedback) Substring(pc int) (uintptr, int, bool) {
	fn, r := fb.fn, fb.r
	if pc >= len(fn.Code) || jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod {
		return 0, 0, false
	}
	e := fb.e
	if fb.root != nil || e == nil {
		return 0, 0, false
	}
	argc := int(fn.Code[pc].A)
	for _, x := range jitTargets(e) {
		if int(x.pc) == pc && x.substring && r.jitIsSubstring(x.obj, argc) {
			kind, _ := r.jitSubstringOf(x.obj)
			return uintptr(unsafe.Pointer(x.obj)), kind, true
		}
	}
	// Or, where the read's cache knows nothing -- what a string's read
	// leaves it -- the method read before it, as CharCode finds charCodeAt:
	// a loop run once, Octane Gameboy's base64_decode, is compiled once,
	// with no call seen since. Not slice, which an array has too, whose
	// call would leave at the check of its function every time.
	if fb.cl == nil || fb.prog == nil && fb.stringMethodRead(pc, argc).IsUndefined() {
		return 0, 0, false
	}
	if o, _ := r.jitCalleeObjectAt(fb.cl, fb.prog, pc); o != nil {
		return 0, 0, false
	}
	if m := fb.stringMethodRead(pc, argc); m.IsObject() && !m.StrictEquals(r.jitSlice) && r.jitIsSubstring(m.Object(), argc) {
		kind, _ := r.jitSubstringOf(m.Object())
		return uintptr(unsafe.Pointer(m.Object())), kind, true
	}
	return 0, 0, false
}

// FromCharCode is ssa.FromCharCodeFeedback's: the address of
// String.fromCharCode, the realm's own, if the method call of one argument
// at pc calls it, known as push's and pop's are -- the call seen to call it
// (jitCallSeen, jitSeedBuiltin), in fn's own code or, for a callee inlined,
// its entry's -- or as Math's are, the method its read finds now
// (jitCalleeObjectAt). The code checks it calls it.
func (fb *jitFeedback) FromCharCode(pc int) (uintptr, bool) {
	fn, r := fb.fn, fb.r
	if pc < 2 || pc >= len(fn.Code) || jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod || fn.Code[pc].A != 1 || !r.jitFromCharCode.IsObject() {
		return 0, false
	}
	o := r.jitFromCharCode.Object()
	addr := uintptr(unsafe.Pointer(o))
	e := fb.e
	if fb.root != nil || e == nil {
		e = r.jit.entryFor(fn)
	}
	if e != nil {
		for _, x := range jitTargets(e) {
			if int(x.pc) == pc && x.fromChar {
				return addr, true
			}
		}
	}
	if fb.cl == nil {
		return 0, false
	}
	if fb.prog == nil {
		p, err := r.jitLower(fn, jitLowerInline)
		if err != nil {
			return 0, false
		}
		fb.prog = p
	}
	if len(fb.prog.Maps) != len(fn.Code) {
		return 0, false
	}
	if c, _ := r.jitCalleeObjectAt(fb.cl, fb.prog, pc); c != o {
		return 0, false
	}
	return addr, true
}

// Intrinsic is ssa.IntrinsicFeedback's: a method call at pc whose function
// is one of the realm's Math functions, as its reads give it now
// (jitChainValue) -- Math.sqrt(x), or Math.random() of no arguments --
// which the code makes itself, as V8 reduces such a call; the call checks
// it calls it.
func (fb *jitFeedback) Intrinsic(pc int) (ssa.Intrinsic, bool) {
	fn := fb.fn
	if fb.cl == nil || pc < 2 || pc >= len(fn.Code) || fn.Code[pc].A > jitMathArgs {
		return ssa.Intrinsic{}, false
	}
	plain := false
	switch jitCallOp(fn.Code[pc].Op) {
	case bytecode.OpCallMethod:
	case bytecode.OpCall:
		// BigInt(x) alone, a global's (jitBigIntIntrinsic).
		plain = true
	default:
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
	read := bytecode.OpGetPropThis
	if plain {
		read = bytecode.OpGetGlobal
	}
	for k := pc - 1; k >= 0; k-- {
		d := p.Maps[k].Depth
		if d == slot && fn.Code[k].Op == read {
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
	if fd := o.fn(); fd != nil && fd.native != nil && (plain || fd.mathOp == mathAsIntN || fd.mathOp == mathAsUintN) {
		if !fb.jitBigIntIntrinsic(&k, fd, plain, pc, q) {
			return ssa.Intrinsic{}, false
		}
		keep := fb.keep()
		keep.holders = append(keep.holders, o)
		return k, true
	}
	if plain {
		return ssa.Intrinsic{}, false
	}
	// The realm's Math functions of numbers, each made natively, as V8's
	// code makes them: Math.pow where exact, and by Go called from there
	// otherwise; Math.max and Math.min of any number of them; a rounding,
	// the sign, fround, sqrt and abs by instructions of their own; every
	// other by Go called from there, the VM's very function.
	fd, n := o.fn(), int(fn.Code[pc].A)
	switch {
	case n == 0:
		// Only Math.random takes none: the next number of the runtime's
		// cache, which the code reads, made now if it is not yet.
		if o != fb.r.randomFn {
			return ssa.Intrinsic{}, false
		}
		k.Op, k.Random = ssa.OpRandomF64, uintptr(unsafe.Pointer(fb.r.mathRandom()))
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

// jitBigIntIntrinsic makes k the intrinsic of the call at pc of fd, read at
// q, if it is BigInt's that native code makes (bigint.go in ssa): BigInt(x)
// of one argument, plain, BigInt.asUintN(n, x) or asIntN's of a width n
// pushed right after the read, an integer from 0 to 64, as V8 reduces
// those whose width is a constant; and reports whether it is.
func (fb *jitFeedback) jitBigIntIntrinsic(k *ssa.Intrinsic, fd *funcData, plain bool, pc, q int) bool {
	fn := fb.fn
	switch {
	case plain:
		if fd.mathOp != mathBigInt || fn.Code[pc].A != 1 {
			return false
		}
		k.Op, k.Plain = ssa.OpBigOfF64, true
		return true
	case fn.Code[pc].A != 2 || q+1 >= pc:
		return false
	}
	w := fn.Code[q+1]
	if w.Op != bytecode.OpPushInt || int32(w.A) < 0 || int32(w.A) > 64 {
		return false
	}
	// The width stays where it was pushed, the stack above it until the
	// call: no instruction before it takes it.
	p, slot := fb.prog, fb.prog.Maps[q+1].Depth
	for k := q + 2; k <= pc; k++ {
		if p.Maps[k].Depth < slot+1 {
			return false
		}
	}
	k.Op, k.Fn = ssa.OpZextW, int(int32(w.A))
	if fd.mathOp == mathAsIntN {
		k.Op = ssa.OpSextW
	}
	return true
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
		ctor.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return ssa.InstanceOfSite{}, false
	}
	proto := ssa.PropertySite{Key: uint32(atomPrototype), Shape: has.Shape, Index: int32(i)}
	h := hc.p1
	if hc.p2 != nil {
		h = hc.p2
	}
	if hv := h.vals[hc.idx]; !hv.IsObject() || hv.Object() != r.hasInstanceFn {
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
		return fb.forwardInline(pc, fb.fwd)
	}
	if fb.fwdArgs != nil && fb.fn.Code[pc].Op == bytecode.OpApplyArguments {
		return fb.forwardInline(pc, fb.fwdArgs)
	}
	k := fb.keep()
	for _, in := range targets {
		if int(in.pc) != pc {
			continue
		}
		kind := jitLowerInline
		reads := fb.fn.Code[pc].Op != bytecode.OpNew && jitReadsArguments(in.cl.fn)
		if (in.cl.fn.Leaf == bytecode.LeafForward || in.cl.fn.ForwardsArguments) && fb.fn.Code[pc].Op == bytecode.OpNew || reads {
			kind = jitLowerForward
		}
		p, err := fb.r.jitLower(in.cl.fn, kind)
		if err != nil || in.cl.fn.Kind == bytecode.KindArrow && p.This {
			// An arrow's this is the one it captured, not the call's
			// receiver, which inlined code would give it.
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
			ReadsArguments: reads,
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
			} else if in.cl.fn.ForwardsArguments {
				// Its frame Go makes with the construction's arguments, its
				// forwards inlined, its captured bindings read from its
				// cells.
				site.Arguments = true
				site.Upvalues = jitUpvalueCells(in.cl)
				callee.fwdArgs = &jitForwardFrame{ctor: in.obj, pool: in.pool, argc: int(call.A)}
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
	// The calls its code learned, by PC, found once: each call had looked
	// through them all, a call site's every target among every site's.
	if fb.targetsBy == nil {
		fb.targetsBy = map[int][]*jitInline{}
		for _, x := range jitTargets(fb.e) {
			fb.targetsBy[int(x.pc)] = append(fb.targetsBy[int(x.pc)], x)
		}
	}
	targets := fb.targetsBy[pc]
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
		if in.cl == nil && (in.charCode || in.fromChar || in.substring || in.forEach || in.parseInt || in.indexOf || in.bigString) {
			// String.prototype.charCodeAt, String.fromCharCode, a substring,
			// a forEach or parseInt, which the code makes itself (CharCode,
			// FromCharCode, Substring, ForEach, ParseInt) where it can, and
			// Go otherwise.
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
			// keep to: the prototypes as they are now, with no index in
			// their tables -- what the shapes the checks compare settle,
			// a shape remembered being replaced before a key is added to
			// it. Object.prototype's had been taken as it was, a setter
			// for an index there included, which native code then passed.
			ap, op := fb.r.proto.array, fb.r.proto.object
			if ap.shape == nil || op.shape == nil || ap.proto != op || op.proto != nil ||
				!ap.noIndexKeys() || !op.noIndexKeys() {
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
			// A built-in's construction with nothing to run: new Array(),
			// or new Array(n), its length the one argument, with a pool
			// of arrays with room (abi.ObjectPool's Room).
			argc := int(jitCallInstr(fb.fn.Code[pc]).A)
			if argc != 0 && (argc != 1 || in.pool.Room == 0) {
				continue
			}
			fb.holders, fb.pools = append(fb.holders, in.obj), append(fb.pools, in.pool)
			sites = append(sites, ssa.CallSite{Callee: uintptr(unsafe.Pointer(in.obj)), ThisSlot: -1,
				Pool: uintptr(unsafe.Pointer(in.pool)), Alloc: true, Argc: argc})
			continue
		}
		site, ok := fb.closureCallSite(pc, in, jitCallInstr(fb.fn.Code[pc]), targets)
		if !ok {
			continue
		}
		sites = append(sites, site)
	}
	if call := jitCallInstr(fb.fn.Code[pc]); len(sites) != 0 && call.Op != bytecode.OpNew && fb.cl != nil && jitCountAt(fb.e.nativeCalls, pc) >= jitCallTargets {
		// As many as it calls natively: any other function it calls through
		// its link (ssa.CallSite's Generic).
		sites[0].Generic = true
		sites[0].GenericEnv, sites[0].GenericRealm = uintptr(unsafe.Pointer(fb.cl.env)), uintptr(unsafe.Pointer(fb.cl.realm))
	}
	if len(sites) == 0 {
		// None it calls natively: Go makes it, called from native code
		// (jitGoCall), as V8's code calls a function it has no code for.
		call := jitCallInstr(fb.fn.Code[pc])
		if call.Op == bytecode.OpForOfStart {
			// Its cursor, which Go makes, called from native code: a
			// destructuring makes one each time.
			return []ssa.CallSite{{Go: ssa.GoCall, ForOf: true, ThisSlot: -1}}
		}
		switch call.Op {
		case bytecode.OpTypeOf:
			// A typeof not compared with a name (jitcompile's
			// typeTests): its name, which Go makes from native code.
			return []ssa.CallSite{{Go: ssa.GoCall, TypeOf: true, ThisSlot: -1}}
		case bytecode.OpInstanceOf:
			// An instanceof whose constructor native code does not know
			// (ssa.InstanceOfSite): Go's, from native code.
			return []ssa.CallSite{{Go: ssa.GoCall, InstanceOf: true, Argc: 1, ThisSlot: -1}}
		case bytecode.OpSuperCall:
			// A derived constructor's super(...) of its arguments on the
			// stack: Go's, on the code's own frame (jitGoCall).
			if call.B == bytecode.SuperArgs && int(call.A)+1 <= abi.MaxGoArgs {
				return []ssa.CallSite{{Go: ssa.GoCall, Super: true, Argc: int(call.A), ThisSlot: -1}}
			}
			return nil
		}
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

// closureCallSite is NativeCalls' native call of in, a closure the call at
// pc has met, made as call says (jitCallInstr): through the function's link,
// to whatever code it has then, its receiver written if its body reads one,
// checked by its code where it is a closure made anew (ByCode) -- or false
// for one Go makes. targets are the others the call has met.
func (fb *jitFeedback) closureCallSite(pc int, in jitInline, call bytecode.Instr, targets []*jitInline) (ssa.CallSite, bool) {
	arrowThis := in.cl.fn.Kind == bytecode.KindArrow && in.cl.fn.UsesThis
	if fd := in.obj.fn(); arrowThis && (fd == nil || fd.extra == nil || fd.extra.lexThisRef != nil || in.via) {
		// An arrow's this is the one it captured, which the call gives
		// it (ssa.CallSite's ArrowThis): one that shares a derived
		// constructor's binding, or called through
		// Function.prototype.call, whose object the call's checks find,
		// Go makes.
		return ssa.CallSite{}, false
	}
	// Only looked up: a compile here would share the workspaces of the
	// one asking. jitCallSeen compiled it.
	ce := fb.r.jit.entryFor(in.cl.fn)
	var link *jitLink
	switch {
	case ce != nil && ce.ssa != nil:
		link = ce.link
	case ce != nil && (ce.calleeRefused || ce.ssaCallee):
		// Refused for good: Go makes its calls.
		if !slices.Contains(fb.wanted, in.cl) {
			k := fb.keep()
			k.wanted = append(k.wanted, in.cl)
		}
		return ssa.CallSite{}, false
	case !fb.r.jitLinkable(in.cl):
		// Go makes its calls until it has code.
		if !slices.Contains(fb.wanted, in.cl) {
			k := fb.keep()
			k.wanted = append(k.wanted, in.cl)
		}
		return ssa.CallSite{}, false
	default:
		link = fb.r.jit.linkOf(in.cl.fn)
		if k := fb.keep(); !slices.Contains(k.linked, in.cl) {
			k.linked = append(k.linked, in.cl)
		}
	}
	fn := in.cl.fn
	// The call is made through the function's link (jitLink), to
	// whatever code it has then: what it writes for the callee is the
	// function's -- its receiver if its body reads one -- not this
	// code's, which a compile of it later may not be.
	this := -1
	if fn.UsesThis {
		this = fn.LocalCount + len(fn.Upvalues)
	}
	fb.links = append(fb.links, link)
	fb.holders = append(fb.holders, in.obj)
	site := ssa.CallSite{
		Callee: uintptr(unsafe.Pointer(in.obj)), Closure: uintptr(unsafe.Pointer(in.cl)),
		Entry: uintptr(unsafe.Pointer(&link.entry)), Count: uintptr(unsafe.Pointer(&link.in)),
		Argc: int(call.A), Method: call.Op == bytecode.OpCallMethod,
		Params: fn.ParamCount, LocalCount: fn.LocalCount, MaxStack: fn.MaxStack, ThisSlot: this, Coerce: fn.CoerceThis,
	}
	site.ArrowThis = arrowThis
	if this >= 0 && !arrowThis && call.Op == bytecode.OpCall && (fn.Kind == bytecode.KindNormal || fn.Kind == bytecode.KindMethod) {
		// Called plainly, its receiver undefined -- or, sloppy, the
		// global object it is coerced to, as callFunction coerces it, of
		// the realm the code's calls run in: a function of another's Go
		// calls. Go had made every such call, the callee's code compiled
		// for native callers never run.
		site.PlainThis, site.Coerce = true, false
		if fn.CoerceThis {
			if in.cl.realm != fb.r.Realm || !fb.r.globalThis.IsObject() {
				fb.links = fb.links[:len(fb.links)-1]
				return ssa.CallSite{}, false
			}
			g := fb.r.globalThis.Object()
			site.Global = uintptr(unsafe.Pointer(g))
			fb.holders = append(fb.holders, g)
		}
	}
	if in.active != nil {
		// A super()'s parent (jitSuperSeen), constructed with the
		// instance: its operands the arguments, its this the instance's
		// (ssa.CallSite's Super). One of a function the code was not
		// compiled for, a class that makes arrows, or of spread or
		// forwarded arguments, Go makes.
		if call.Op != bytecode.OpSuperCall || call.B != bytecode.SuperArgs || fb.fn.Kind != bytecode.KindDerivedConstructor || jitMakesArrow(fb.fn) {
			fb.links = fb.links[:len(fb.links)-1]
			return ssa.CallSite{}, false
		}
		site.Super, site.Active, site.Derived = true, uintptr(unsafe.Pointer(in.active)), fn.Kind == bytecode.KindDerivedConstructor
		site.Method, site.Coerce = false, false
		fb.holders = append(fb.holders, in.active)
		if in.parent != nil {
			site.Parent = uintptr(unsafe.Pointer(in.parent))
			fb.holders = append(fb.holders, in.parent)
		}
	}
	if fn.HasTailCall {
		// Whether its calls in tail position may give its frame up, as
		// the interpreter gives up a frame that is no construction's,
		// nor an arrow's that shares a derived constructor's this.
		fd := in.obj.fn()
		site.Tail = true
		site.TailFrame = call.Op != bytecode.OpNew && call.Op != bytecode.OpSuperCall && (fd == nil || !fd.arrow || fd.extra == nil || fd.extra.lexThisRef == nil)
	}
	if len(in.cl.upvalues) != 0 {
		// Its captured bindings, which its code reads through its
		// context; the call checks it calls this closure's function
		// object, which keeps them.
		site.Upvalues = uintptr(unsafe.Pointer(unsafe.SliceData(in.cl.upvalues)))
	}
	// Asked of its lowering, which keeps the answer (startOnly): each
	// site had walked the callee's code for it again.
	if l := fb.r.jit.lowered[weak.Make(in.cl.fn)]; l != nil {
		if l.startOnly == 0 {
			l.startOnly = 1
			if jitcompile.StartOnly(in.cl.fn) {
				l.startOnly = 2
			}
		}
		site.StartOnly = l.startOnly == 2
	} else {
		site.StartOnly = jitcompile.StartOnly(in.cl.fn)
	}
	if in.via {
		site.Via = uintptr(unsafe.Pointer(fb.r.callFn))
		fb.holders = append(fb.holders, fb.r.callFn)
	}
	if call.Op == bytecode.OpNew {
		i := in.obj.findOwn(atomPrototype)
		if in.pool == nil || i < 0 {
			return ssa.CallSite{}, false
		}
		site.Pool, site.ProtoIndex, site.ProtoKey = uintptr(unsafe.Pointer(in.pool)), int(i), uint32(atomPrototype)
		site.Coerce = false
		// Its callee's code a derived constructor's: the pool's object
		// its instance.
		site.Derived = in.cl.fn.Kind == bytecode.KindDerivedConstructor
		fb.pools = append(fb.pools, in.pool)
		if in.parent != nil {
			// A derived class's, with its parent's code.
			site.Parent = uintptr(unsafe.Pointer(in.parent))
			fb.holders = append(fb.holders, in.parent)
		}
	}
	byCode := len(in.cl.upvalues) != 0 || in.byCode
	for _, y := range targets {
		if byCode {
			break
		}
		byCode = int(y.pc) == pc && y.cl != nil && y.obj != in.obj && jitSameCode(y.cl, in.cl)
	}
	if site.Pool == 0 && !in.via && !site.Super && byCode {
		// A closure: checked by its function, its closure the callee's
		// at run time (ByCode), as the one an enclosing function makes
		// at each of its calls is a new object each time.
		site.ByCode, site.Env, site.Realm = uintptr(unsafe.Pointer(in.cl.fn)), uintptr(unsafe.Pointer(in.cl.env)), uintptr(unsafe.Pointer(in.cl.realm))
	}
	return site, true
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
	// Array.prototype.pop (jitPops), charCode one of
	// String.prototype.charCodeAt (jitCharCodes), fromChar one of
	// String.fromCharCode (jitFromCharCodes), substring one of
	// String.prototype's substring, slice, substr or charAt (jitSubstrings).
	push, pop, charCode, fromChar, substring bool
	// forEach marks a call of Array.prototype.forEach, or another of
	// jitLoops, on an array (jitForEaches), cb the closure of the callback
	// it was given and cbObj its function object.
	forEach bool
	cb      *closure
	cbObj   *Object
	// proto is a typed array's loop's typed arrays' prototype
	// (jitLoopTyped), nil for an array's.
	proto *Object
	// parseInt marks a plain call of the realm's parseInt with a number
	// (jitParseInts), indexOf a string's or an array's call of one of
	// jitSearches (jitIndexOfs), bigString a BigInt's call of
	// BigInt.prototype.toString (jitBigStrings).
	parseInt, indexOf, bigString bool
	// byCode marks a function the call met two objects of, closures its
	// maker makes anew at each call: called by its code (jitSameCode).
	byCode bool
	// fwd, for an inlined forwarding constructor's construction
	// (bytecode.LeafForward), is the method it forwards to, as the code was
	// last compiled for (jitForwardMethod).
	fwd *Object
	// parent, for a construction with a class whose constructor is the
	// one a class without one has (jitDerivedBase), is the class it
	// extends, whose constructor cl is: the call checks obj's prototype is
	// still it (ssa.CallSite's Parent).
	parent *Object
	// active, for a super() native code makes, is the class whose
	// constructor's super() it is, whose prototype obj is: the parent,
	// which cl's code -- obj's, or, obj a class with no constructor of its
	// own, parent's -- constructs with (jitSuperTarget).
	active *Object
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
	if o, cb, cbObj, proto := r.jitForEaches(sp, in); o != nil {
		r.jitForEachSeen(e, f.cl.fn, pc, o, cb, cbObj, proto)
		return
	}
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
		if !refilled && !e.reoptPending() && slices.Contains(e.ssaInlinedAt, int32(pc)) {
			// Not the code compiled before it was decided, which leaves
			// there until it is compiled again; nor code to be compiled
			// again for what it has learned (reoptPending), which may be
			// what it leaves for.
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
		if o := r.jitCharCodes(sp, in); o != nil {
			// s.charCodeAt(i), made natively (jitFeedback.CharCode).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, charCode: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitFromCharCodes(sp, in); o != nil {
			// String.fromCharCode(c), made natively (jitFeedback.FromCharCode).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, fromChar: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitIndexOfs(sp, in); o != nil {
			// s.indexOf(t), a.includes(x) or another search, made by a call
			// of Go of its own (jitFeedback.IndexOf).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, indexOf: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitBigStrings(sp, in); o != nil {
			// x.toString() of a BigInt, made by a call of Go of its own
			// (jitFeedback.BigString).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, bigString: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitParseInts(sp, in); o != nil {
			// parseInt(x) of a number, made natively (jitFeedback.ParseInt).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, parseInt: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o := r.jitSubstrings(sp, in); o != nil {
			// s.substring(i, j), s.slice(i, j), s.substr(i, j) or
			// s.charAt(i), made natively where it is one character
			// (jitFeedback.Substring).
			e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, substring: true})
			e.callSites[pc] = jitCallNative
			e.inlinePending = true
			return
		}
		if o, room := r.jitAllocates(sp, in); o != nil {
			// A built-in's construction with nothing to run: made from a
			// pool.
			pool := r.jitNewPool(o)
			if in.A == 1 {
				pool = r.jitNewArrayPool(o, room)
			}
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
	// (NativeCalls), the call itself for a recursive one; a derived class
	// with no constructor of its own is constructed with its parent's code.
	if in.Op == bytecode.OpNew && !r.jitConstructs(o) && !r.jitConstructsDerived(o) {
		p := r.jitDerivedBase(o)
		if p == nil {
			return
		}
		cl = p.fn().closure
	}
	if r.jitNativeOrLinked(cl) {
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
		if !one || x.cl == nil || r.jit.entryFor(x.cl.fn) == e || e.callSites == nil || pc >= len(e.callSites) ||
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
		// The most frequent, asked first; one that runs nothing made
		// without a frame.
		return r.jitMaybePlain(ctx) && r.jitGoCallPlain(ctx) || r.jitGoCall(ctx)
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
		if a.IsBigInt() || b.IsBigInt() {
			if a.IsBigInt() && b.IsBigInt() {
				return r.jitGoBigAdd(ctx, a, b)
			}
			if !jitAddOperand(a, b) || !jitAddOperand(b, a) {
				return false
			}
		} else if !jitPlainOperand(a) || !jitPlainOperand(b) {
			return false
		}
		if r.meter != nil || a.IsString() && b.IsString() && a.String().length+b.String().length > maxStringLength-64 {
			return false
		}
		v, err := r.add(a, b)
		if err != nil {
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		return true
	case ssa.GoArith:
		// An operator of two BigInts that throws nothing.
		op, ok := jitArithOp(ir.Operator(ctx.GoOp >> abi.GoOpIndexShift))
		return ok && r.jitGoBig(ctx, op, jitGoArg(ctx, 0), jitGoArg(ctx, 1))
	case ssa.GoCompare:
		// A relational comparison of primitives, which converts them
		// without calling anything (jitPlainOperand) -- a BigInt too, which
		// compares with anything that is no symbol: its boolean.
		a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
		if !jitComparable(a) || !jitComparable(b) {
			return false
		}
		c, err := r.compare(a, b)
		if err != nil {
			return false
		}
		op := bytecode.OpGe
		switch ir.Operator(ctx.GoOp >> abi.GoOpIndexShift) {
		case ir.Lt:
			op = bytecode.OpLt
		case ir.Le:
			op = bytecode.OpLe
		case ir.Gt:
			op = bytecode.OpGt
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Bool(relationalResult(op, c))
		return true
	case ssa.GoAppend:
		// A loop's string builder's s += x (jitAppend): what GoAdd takes,
		// a string's grown in place or made a builder of.
		a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
		if a.IsBigInt() && b.IsBigInt() {
			// A loop's sum of BigInts, which it takes for a string's.
			return r.jitGoBigAdd(ctx, a, b)
		}
		if a.IsBigInt() || b.IsBigInt() {
			if !jitAddOperand(a, b) || !jitAddOperand(b, a) {
				return false
			}
		} else if !jitPlainOperand(a) || !jitPlainOperand(b) {
			return false
		}
		if r.meter != nil {
			return false
		}
		var v Value
		if a.IsString() {
			x := b
			if !b.IsString() {
				s, err := r.toString(b)
				if err != nil {
					return false
				}
				x = Str(s)
			}
			s, ok := r.jitAppend(a.String(), x.String())
			if !ok {
				return false
			}
			v = Str(s)
		} else {
			var err error
			if v, err = r.add(a, b); err != nil {
				return false
			}
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
			if p.IsBigInt() {
				// Every part is joined to a string (the lowering's
				// stringConcats), a BigInt too.
				if !jitConcatOperand(p) {
					return false
				}
			} else if !jitPlainOperand(p) {
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
	case ssa.GoCall:
		return r.jitMaybePlain(ctx) && r.jitGoCallPlain(ctx) || r.jitGoCall(ctx)
	case ssa.GoCells:
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoClosurePool {
			// Cells native code takes from their pool where it can: run
			// out, filled again, with more.
			jitRefillCells(*(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref)))
		}
		return r.jitMakeCells(ctx)
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
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoClosurePool {
			// One native code takes from its pool where it can, or the
			// cells a scope's end takes (mir's cellsClose): run out, filled
			// again, with more.
			if pool := *(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[0].Ref)); pool.Cells != 0 {
				jitRefillCells(pool)
			} else {
				r.jitRefillPool(pool)
			}
		}
		if ctx.Cells != nil {
			// Code whose captured locals are in cells of their own.
			return r.jitCellsClosure(ctx, f, i)
		}
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
		if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
			// A close's writes none, so that one made with every cell
			// taken (ssa's goFrame) shares one.
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		}
		return true
	case ssa.GoNewArray:
		// map's or filter's array, of no length, room for the array's in
		// map's: where the array's species is Array's (plainSpecies), as
		// their ArraySpeciesCreate would make it.
		o := r.plainArray(jitGoArg(ctx, 0))
		n := jitGoArg(ctx, 1)
		if o == nil || !n.IsNumber() || !r.plainSpecies(o) {
			return false
		}
		room := int64(0)
		if ctx.GoOp>>abi.GoOpIndexShift&1 != 0 {
			room = int64(n.Number())
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(r.newArrayOfLength(room))
		return true
	case ssa.GoElemCell:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		return o != nil && jitElemAbsent(o, math.Float64frombits(ctx.GoArgs[1].Num))
	case ssa.GoIndexOf:
		// A search of a string or an array (jitSearch), what it searches
		// for, and its position if the call gives one.
		form := int(ctx.GoOp >> abi.GoOpIndexShift)
		argv := &r.jit.searchArgs
		args := argv[:1]
		argv[0] = jitGoArg(ctx, 1)
		if form&ssa.IndexOfFrom != 0 {
			args, argv[1] = argv[:2], jitGoArg(ctx, 2)
		}
		v, ok := r.jitSearch(form>>1, jitGoArg(ctx, 0), args)
		*argv = [2]Value{}
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		}
		return ok
	case ssa.GoKeyedWrite:
		// An element's write, of any receiver, by any key (jitKeyedWrite):
		// undefined, the call's result, which nothing reads, as native
		// code's store writes it (mir's keyedStore).
		if !r.jitKeyedWrite(jitGoArg(ctx, 0), jitGoArg(ctx, 1), jitGoArg(ctx, 2), true) {
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Undefined
		return true
	case ssa.GoBigString:
		// x.toString() of a BigInt, its radix if the call gives one.
		var argv [1]Value
		args := argv[:0]
		if ctx.GoOp>>abi.GoOpIndexShift != 0 {
			args, argv[0] = argv[:1], jitGoArg(ctx, 1)
		}
		v, ok := jitBigString(jitGoArg(ctx, 0), args)
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		}
		return ok
	case ssa.GoIndex:
		v, ok := r.jitKeyedRead(jitGoArg(ctx, 0), jitGoArg(ctx, 1))
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		}
		return ok
	case ssa.GoHas:
		has, ok := r.jitHas(jitGoArg(ctx, 0), jitGoArg(ctx, 1))
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Bool(has)
		}
		return ok
	case ssa.GoDelete, ssa.GoDeleteStrict:
		gone, ok := r.jitDelete(jitGoArg(ctx, 0), jitGoArg(ctx, 1), ssa.GoOp(ctx.GoOp&0xff) == ssa.GoDeleteStrict)
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Bool(gone)
		}
		return ok
	case ssa.GoElemWrite:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		k, v := math.Float64frombits(ctx.GoArgs[1].Num), Float(math.Float64frombits(ctx.GoArgs[2].Num))
		return o != nil && jitStoreElem(o, k, v)
	case ssa.GoElemStore:
		// Any value, by its words, which its source keeps alive.
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		if ctx.GoOp>>abi.GoOpIndexShift&ssa.StoreFresh != 0 {
			return o != nil && jitAppendFresh(o, math.Float64frombits(ctx.GoArgs[1].Num), jitGoArg(ctx, 2))
		}
		return o != nil && jitStoreElem(o, math.Float64frombits(ctx.GoArgs[1].Num), jitGoArg(ctx, 2))
	case ssa.GoEqual:
		// == or ===, what native code does not compare (OpEqTagged): no
		// conversion equality makes runs code but an object's to a
		// primitive, compared loosely with one not null or undefined.
		a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
		var eq bool
		if ctx.GoOp>>abi.GoOpIndexShift&1 != 0 {
			eq = a.StrictEquals(b)
		} else {
			if a.IsObject() != b.IsObject() && !a.IsNullish() && !b.IsNullish() {
				return false
			}
			var err error
			if eq, err = r.looseEquals(a, b); err != nil {
				return false
			}
		}
		ctx.GoArgs[0].Num = 0
		if eq {
			ctx.GoArgs[0].Num = 1
		}
		return true
	case ssa.GoNumber:
		// An arithmetic operand's number (OpOddballF64): a primitive's,
		// whose conversion runs nothing and does not throw -- a string's,
		// parsed. An object's, a symbol's or a BigInt's, Go makes the
		// operation of after the code leaves.
		v := jitGoArg(ctx, 0)
		if !jitPlainOperand(v) {
			return false
		}
		n, err := r.toNumber(v)
		if err != nil {
			return false
		}
		ctx.GoArgs[0].Num = math.Float64bits(n)
		return true
	case ssa.GoTypedRead:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		v, ok := jitTypedRead(o, math.Float64frombits(ctx.GoArgs[1].Num))
		if ok {
			ctx.GoArgs[0].Num = math.Float64bits(v)
		}
		return ok
	case ssa.GoPoll:
		// A debugger's interrupt may stop and show the frames, which the
		// exit makes; stress (QJS_JIT_STRESS's budget) wants the exits.
		s := r.jit
		if r.debug != nil || r.jitStress.budget != 0 {
			return false
		}
		if idx := int(ctx.Level); ctx.Live == 0 && idx < len(s.goRuns) && s.goRuns[idx].f != nil && s.ssaCtxs[idx] == ctx {
			if e := s.goRuns[idx].e; e != nil && e.reoptPending() && !e.invalid {
				// The run's code learned what to be compiled again for, at a
				// call of Go, say, which an exit would have counted: its
				// loop leaves at the header (abi.ExitPoll), where the
				// stretch's work is counted toward the code's budget
				// (jitLearnedDue) and the code compiled again once it is
				// spent, as V8's back edges spend its interrupt budget.
				// Never leaving, a loop that ran a whole invocation natively
				// -- each of microbench's tests -- counted none of its work,
				// and its code, its calls of a function made anew by Go, was
				// never compiled again.
				return false
			}
		}
		old := r.backEdges
		r.backEdges = s.backEdgeBudget()
		s.pollCredit += r.backEdges - old
		s.budgets++
		return r.checkInterruptNow() == nil
	case ssa.GoRandom:
		// Math.random's cache, found empty: filled again, its next number
		// the result.
		ctx.GoArgs[0].Num = math.Float64bits(r.random())
		return true
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
	f    *frame
	e    *jitEntry
	code *jit.SSACode
	// inlined are the closures code inlines (jitEntry's ssaInlined), whose
	// frames Go makes at an exit inside one (jitUnwindNative): e's lists
	// are another code's once e is compiled again while the run goes on
	// (jitReoptimize), and code's, closed once its native code has left,
	// keep nothing.
	inlined []*closure
	pinned  bool
	// pinnedAt is where its pins start in goPinned: its own code, then
	// that of the native callees whose calls of Go it is suspended in
	// (jitGoCallNested), all released when its native code leaves. serial
	// names those pins: no other run's, nor this run's after they are
	// released, have it (jitState's pinSerial).
	pinnedAt int
	serial   uint64
	// edges and pc are where its last call of Go returned, or its
	// stretch began: the back-edge counter then, net of the polls'
	// budgets, and the PC -- the start of the work its next call of Go
	// is judged against (jitGoCall), in a run on-stack replacement began
	// (osr).
	edges, pc int
	osr       bool
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

// jitGoRunAt notes that context idx runs e's code for f, in a stretch
// from pc, with the back-edge counter, net of the polls' budgets, at
// edges, of a run on-stack replacement began if osr.
func (r *Runtime) jitGoRunAt(idx int, f *frame, e *jitEntry, pc, edges int, osr bool) {
	s := r.jit
	if idx >= len(s.goRuns) {
		s.goRuns = append(s.goRuns, make([]jitGoRun, idx+1-len(s.goRuns))...)
	}
	s.goRuns[idx] = jitGoRun{f: f, e: e, code: e.ssa, inlined: e.ssaInlined, edges: edges, pc: pc, osr: osr}
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
	if n > abi.MaxGoArgs || depth < jitGoStacked(in, n) || top+n > len(r.stack) || !jitGoPropertyAt(f.cl.fn, pc, in) {
		return false
	}
	for i := range n {
		r.stack[top+i] = jitGoArg(ctx, i)
	}
	sp := top + n
	r.stackTop, r.stackHigh = sp, max(r.stackHigh, sp)
	step := in.Op == bytecode.OpIterNextOrJump || in.Op == bytecode.OpIterStep || in.Op == bytecode.OpForOfStart || in.Op == bytecode.OpTypeOf || in.Op == bytecode.OpInstanceOf || in.Op == bytecode.OpSuperCall
	prop := in.Op == bytecode.OpGetProp || in.Op == bytecode.OpSetProp
	// A loop's step Go makes costs what any call of Go does: a loop of
	// them, over a Map or a generator, whose body does little, the tree
	// tier runs (one over an array native code steps itself, and calls
	// nothing).
	e.ssaStats.goCalls++
	s.goCalls++
	// Judged, in a run begun at a loop, where the call is one Go makes for
	// good: a step, a property, a built-in's call (jitCallDone). One Go
	// makes until it is decided -- a call native code will make natively,
	// once the callee is compiled -- is not.
	judged := run.osr && (step || prop || !jitCallUnsettled(e, pc, in))
	slow := judged && r.jitRunGoProfit(e, run, pc)
	if !step && !prop && jitCallUnsettled(e, pc, in) {
		r.jitCallSeen(f, e, pc, sp, in)
	}
	// Past its call meanwhile, as the interpreter's frame is during one,
	// which a stack trace shows.
	f.pc = uint32(pc + 1)
	last, shared := s.ssaCtx, s.ssaSharedGen
	if !run.pinned {
		r.jitGoPin(idx)
	}
	goCallDepth := s.goCallDepth
	s.goCallDepth = r.frameDepth + 1
	var v Value
	var err error
	switch {
	case prop:
		if in.Op == bytecode.OpGetProp && r.jitSiteMegamorphic(f.cl, e, pc) {
			// A megamorphic site's read that native code did not find,
			// learned as at an exit (jitPolySeen): the megamorphic cache
			// filled for the receiver's shape, which the next such read
			// finds natively, as V8's miss handler fills its stub cache.
			r.jitPolySeen(f, e, pc, sp, f.cl.fn.Code[pc])
		}
		v, err = r.jitPropertyMade(f.cl, pc, sp)
	case in.Op == bytecode.OpSuperCall:
		// A derived constructor's super(...) (abi.GoCallSuper), on the
		// code's frame, as executeAt's case makes it: the this it binds.
		// Its first operand, the frame's this slot, is the binding's.
		r.jitSuperSeen(f, e, pc)
		if err = r.jitSuperCall(f, slices.Clone(r.stack[sp-n+1:sp])); err == nil {
			v = f.this
		}
	default:
		v, err = r.jitCallMade(in, sp)
	}
	s.goCallDepth = goCallDepth
	jitClearArgs(r.stack, top, sp)
	r.stackTop = top
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		// What the call ran shared other values with the contexts past
		// this one, which this code's calls run in.
		r.jitShareContexts(idx, f)
	}
	if judged {
		// The work the next call is judged against starts here: what
		// this one ran, which the existing tiers would run alike, is none
		// of it.
		s.goRuns[idx].edges, s.goRuns[idx].pc = r.backEdges-s.pollCredit, pc
	}
	// Its calls of Go judged not to pay, the code leaves after this one,
	// and the existing tiers run the rest of the invocation (jitGoOn).
	if slow || err != nil || r.stopped != nil || e.invalid || r.jitLearnedDue(f.cl, e) || e.ssa != code {
		s.goPending = jitGoPending{set: true, pc: pc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitRunGoProfit accounts for a call of Go that e's code makes at pc in
// run, a run Go entered at a loop by on-stack replacement, and every
// jitSSAProbe such calls judges whether they pay, as jitCalleeProfit
// judges a native callee's: each costs native code what an exit and an
// entry would (jitSSAMinWork) more than the tree tier's own call of the
// built-in, against the work the code did natively since the call before
// -- the back edges taken, at its loops' mean length, or, with none, the
// distance from that call. Judged at the end of its stretches alone
// (jitSSAProfit), such a run, whose one stretch is the loop, was judged
// when the loop was over: a loop of calls of Date.now, of a generator's
// steps, of Date's methods, ran natively to its end, and so again at each
// invocation, the tree tier making none of its calls. Code whose calls do
// not pay is not entered again, at its start nor by on-stack replacement
// (loopSlow), until it is retried (jitRetrySlow); and it reports so, for
// the code to leave. A run entered at the function's start is judged at
// its end, as before: judged at its calls, AWFY's DeltaBlue's callback of
// Plan.execute, whose call of c.execute() Go made until it was decided,
// was no longer entered by Go, and DeltaBlue took 31% longer.
func (r *Runtime) jitRunGoProfit(e *jitEntry, run *jitGoRun, pc int) bool {
	st := &e.ssaStats
	if back := run.edges - (r.backEdges - r.jit.pollCredit); back > 0 {
		st.runWork += uint64(back) * uint64(e.ssaLoop)
	} else if pc > run.pc {
		st.runWork += uint64(pc - run.pc)
	}
	st.runGoCalls++
	if st.runGoCalls%jitSSAProbe == 0 && st.runWork < st.runGoCalls*jitSSAMinWork && !jitCallsNatively(e) {
		e.entrySlow, e.loopSlow = true, true
	}
	return e.loopSlow
}

// jitCallsNatively reports whether e's code calls functions natively: in
// the existing tiers, its callees would run there too, and their work,
// which the back edges count only in part, is native while it runs. Its
// calls of Go are left unjudged: Octane's RegExp's runBlock0, whose calls
// of string methods Go makes, and whose calls of Exec are native, had run
// in the tree, Exec with it, and RegExp took 11% longer.
func jitCallsNatively(e *jitEntry) bool {
	return len(e.nativeCalls) != 0 || slices.Contains(e.callSites, jitCallNative)
}

// jitGoStacked is how many of a call of Go's n operands, for in, are the
// frame's operands: a super()'s first is its this slot, a local.
func jitGoStacked(in bytecode.Instr, n int) int {
	if in.Op == bytecode.OpSuperCall {
		return n - 1
	}
	return n
}

// jitGoPropertyAt reports whether a call of Go, in, may be made at pc of
// fn: a property's read or write only where fn's instruction there is one
// (jitPropertyMade), which reads it; any other call anywhere.
func jitGoPropertyAt(fn *bytecode.Function, pc int, in bytecode.Instr) bool {
	switch in.Op {
	case bytecode.OpSetProp:
		return pc < len(fn.Code) && fn.Code[pc].Op == bytecode.OpSetProp
	case bytecode.OpGetProp:
		return pc < len(fn.Code) && (fn.Code[pc].Op == bytecode.OpGetProp || fn.Code[pc].Op == bytecode.OpGetPropThis)
	}
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

// jitCaptured counts fn's captured locals: the cells of native code whose
// captured locals are in cells of their own (ssa.GoCells).
func jitCaptured(fn *bytecode.Function) int {
	n := 0
	for _, l := range fn.Locals {
		if l.Captured {
			n++
		}
	}
	return n
}

// jitCellsOf is the context's cells (abi.Context's Cells), fn's code's.
func jitCellsOf(ctx *abi.Context, fn *bytecode.Function) []*upvalue {
	return unsafe.Slice((**upvalue)(ctx.Cells), jitCaptured(fn))
}

// jitMakeCells makes, at an entry of code whose captured locals are in
// cells of their own (ir.Program's Cells, ssa.GoCells), those cells, the
// context's, as V8's code allocates a function's context. On the frame Go
// entered the code with, they are open on its slots -- those closures it
// made before took among them -- which it closes as it is popped. For a
// native callee, whose frame Go does not keep, each is a cell of its own,
// closed, holding its parameter's argument or undefined, as the frame's
// slot is at the call: what a closure captures, and native code reads and
// assigns, from then on, and what Go puts in the frame it makes if the
// callee leaves native code (jitReopenCells).
func (r *Runtime) jitMakeCells(ctx *abi.Context) bool {
	if f := r.jitRunFrame(ctx); f != nil {
		fn := f.cl.fn
		n := jitCaptured(fn)
		if n == 0 {
			return false
		}
		cells, room := jitNewCells(n)
		k := 0
		for slot, l := range fn.Locals {
			if !l.Captured {
				continue
			}
			u := openUpvalue(f, slot)
			if u == nil {
				u = &room[k]
				u.slot = &f.locals[slot]
				f.openUpvalues = append(f.openUpvalues, u)
			}
			cells[k] = u
			k++
		}
		ctx.Cells = unsafe.Pointer(unsafe.SliceData(cells))
		ctx.Open = uint64(len(f.openUpvalues))
		return true
	}
	cl := (*closure)(ctx.Closure)
	if ctx.Live != abi.LiveCall || cl == nil || ctx.Locals == nil {
		return false
	}
	fn := cl.fn
	n := jitCaptured(fn)
	if n == 0 {
		return false
	}
	locals := unsafe.Slice((*Value)(ctx.Locals), fn.LocalCount)
	cells, room := jitNewCells(n)
	k := 0
	for slot, l := range fn.Locals {
		if !l.Captured {
			continue
		}
		u := &room[k]
		u.closed = Undefined
		if slot < fn.ParamCount {
			u.closed = locals[slot]
		}
		u.slot = &u.closed
		cells[k] = u
		k++
	}
	ctx.Cells = unsafe.Pointer(unsafe.SliceData(cells))
	return true
}

// jitNewCells is n cells and the array of pointers to them that code whose
// captured locals are in cells of their own reads them through
// (abi.Context's Cells): for up to four, one allocation, as a closure's
// object holds its captured bindings' (newScriptFuncObject) -- the cells
// had been two in five of AWFY DeltaBlue's allocations with the JIT.
func jitNewCells(n int) ([]*upvalue, []upvalue) {
	switch n {
	case 1:
		x := &struct {
			p [1]*upvalue
			u [1]upvalue
		}{}
		return x.p[:], x.u[:]
	case 2:
		x := &struct {
			p [2]*upvalue
			u [2]upvalue
		}{}
		return x.p[:], x.u[:]
	case 3:
		x := &struct {
			p [3]*upvalue
			u [3]upvalue
		}{}
		return x.p[:], x.u[:]
	case 4:
		x := &struct {
			p [4]*upvalue
			u [4]upvalue
		}{}
		return x.p[:], x.u[:]
	}
	return make([]*upvalue, n), make([]upvalue, n)
}

// jitRefillCells fills pool, a pool of cells (abi.ObjectPool's Cells),
// again, with more, if native code ran it out.
func jitRefillCells(pool *abi.ObjectPool) {
	if pool.Count == 0 {
		pool.Size = min(2*max(pool.Size, abi.PoolSize), abi.PoolCapacity)
		jitFillCells(pool)
	}
}

// jitFillCells fills pool with Size blocks of its Cells cells, as
// jitNewCells makes them, each cell's pointer to the value it holds itself
// (upvalue's closed) and the block's pointers to its cells written, which
// native code takes at its function's entries and writes the values of
// (mir's cellsTake).
func jitFillCells(pool *abi.ObjectPool) {
	n := int(pool.Cells)
	for i := range int(pool.Size) {
		cells, room := jitNewCells(n)
		for k := range room {
			room[k].slot = &room[k].closed
			cells[k] = &room[k]
		}
		pool.Objects[i] = unsafe.Pointer(unsafe.SliceData(cells))
	}
	pool.Count = pool.Size
}

// CellsPool is ssa.CellsFeedback's: the pool the function's code takes its
// captured locals' cells from at its entries (mir's cellsTake), the
// entry's, made at its first compile and filled, and the cells' locals,
// |1 a parameter's -- up to four, which one allocation holds
// (jitNewCells), the block's pointers first.
func (fb *jitFeedback) CellsPool() (uintptr, []int32, bool) {
	e, fn := fb.e, fb.fn
	if e == nil || fb.root != nil || fb.fwd != nil || fb.cl == nil {
		return 0, nil, false
	}
	n := jitCaptured(fn)
	if n == 0 || n > 4 {
		return 0, nil, false
	}
	cells := make([]int32, 0, n)
	for slot, l := range fn.Locals {
		if l.Captured {
			x := int32(slot) << 1
			if slot < fn.ParamCount {
				x |= 1
			}
			cells = append(cells, x)
		}
	}
	if e.cellsPool == nil {
		e.cellsPool = &abi.ObjectPool{Size: abi.PoolSize, Cells: uint64(n)}
		jitFillCells(e.cellsPool)
	}
	fb.keep().pools = append(fb.keep().pools, e.cellsPool)
	return uintptr(unsafe.Pointer(e.cellsPool)), cells, true
}

// jitCellsClosure is GoClosure and GoClose for code whose captured locals
// are in cells of their own (abi.Context's Cells), on f, the frame Go
// entered it with or a native callee's stand-in (jitCalleeFrame): a
// closure, its constant i, capturing the cells; or a scope's end, from
// local i on, whose captured locals take fresh cells holding what they
// hold -- the closures that captured the old ones keep them, as closing
// an upvalue leaves them, and a loop's next iteration's capture new ones.
// On the frame Go entered, the old ones are closed and the new ones open
// on its slots, as the interpreter's are.
func (r *Runtime) jitCellsClosure(ctx *abi.Context, f *frame, i int) bool {
	fn := f.cl.fn
	cells := jitCellsOf(ctx, fn)
	run := r.jitRunFrame(ctx) == f
	v := Undefined
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
		if i >= len(f.cl.consts) || !run && jitNeedsMaker(f.cl.consts[i]) {
			return false
		}
		o := r.newClosureIn(f, f.cl.consts[i], cells)
		if fd := o.fn(); fd != nil && atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
			planTreeCall(fd)
		}
		v = Obj(o)
	} else {
		if run && len(f.openUpvalues) != 0 {
			r.closeUpvaluesFrom(f, i)
		}
		k := 0
		for slot, l := range fn.Locals {
			if !l.Captured {
				continue
			}
			if slot >= i {
				if run {
					// What the cell holds, which may be one native code
					// gave it (mir's cellsClose), not open on the slot.
					f.locals[slot] = *cells[k].slot
					u := &upvalue{slot: &f.locals[slot]}
					f.openUpvalues = append(f.openUpvalues, u)
					cells[k] = u
				} else {
					u := &upvalue{closed: *cells[k].slot}
					u.slot = &u.closed
					cells[k] = u
				}
			}
			k++
		}
	}
	if run {
		ctx.Open = uint64(len(f.openUpvalues))
	}
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	}
	return true
}

// jitReopenCells gives f, the frame Go made for native code that left it,
// whose captured locals are in cells of their own (abi.Context's Cells),
// those cells: each local's value in the frame's slot, the cell open on it,
// the frame's to close as it is popped -- the closures that captured it
// see what the interpreter assigns from then on. The context keeps them:
// resumed after its call (jitUnwindNative), the code goes on with them.
func jitReopenCells(f *frame, ctx *abi.Context) {
	cells := jitCellsOf(ctx, f.cl.fn)
	k := 0
	for slot, l := range f.cl.fn.Locals {
		if !l.Captured {
			continue
		}
		if u := cells[k]; u.slot != &f.locals[slot] {
			f.locals[slot] = *u.slot
			u.slot = &f.locals[slot]
			f.openUpvalues = append(f.openUpvalues, u)
		}
		k++
	}
}

// jitReopenRunCells gives f, the frame Go entered code with whose captured
// locals are in cells of their own, the cells native code left in ctx, as
// the interpreter's frame has them: a cell native code gave a local at a
// scope's end (mir's cellsClose), closed, is open on its slot again,
// holding what it held, and those it closed, no longer on the slots, are
// the frame's no more.
func jitReopenRunCells(f *frame, ctx *abi.Context) {
	if len(f.locals) != 0 {
		lo, hi := uintptr(unsafe.Pointer(&f.locals[0])), uintptr(unsafe.Pointer(&f.locals[len(f.locals)-1]))
		kept := f.openUpvalues[:0]
		for _, u := range f.openUpvalues {
			if p := uintptr(unsafe.Pointer(u.slot)); p >= lo && p <= hi {
				kept = append(kept, u)
			}
		}
		clear(f.openUpvalues[len(kept):])
		f.openUpvalues = kept
	}
	jitReopenCells(f, ctx)
	ctx.Open = uint64(len(f.openUpvalues))
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
	e := s.entryFor(cl.fn)
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
	if n > abi.MaxGoArgs || depth < jitGoStacked(in, n) || top+n > len(r.stack) {
		return false
	}
	if cl := (*closure)(ctx.Closure); cl == nil || !jitGoPropertyAt(cl.fn, pc, in) {
		return false
	}
	// Where each level is: at the native call it makes, as its code
	// described it (abi.Context's CallState); this one at this call of Go,
	// which its code wrote -- a description its context holds is an
	// earlier call's.
	var prevPC uint64 // below; declared before the goto past it
	levelPC := func(i int) uint64 {
		if i == idx {
			return ctxs[i].ExitPC
		}
		pc, _ := jit.CallPC(ctxs[i])
		return pc
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
	// The frames the last call made for the levels these share with its,
	// from the run's on, as they are still: each level the same call's,
	// only past another call. Those past them are made.
	k := 0
	// A loop of a built-in's call under way at a level, or the run's: its
	// built-in's frame between the level and its callee, and no frames
	// shared with the last call, whose levels may have had none.
	loops := jitLoopIn(run.e.loops, ctxs[root], runF.cl.fn, int(levelPC(root))) != 0
	for i := root + 1; i < idx && !loops; i++ {
		if lcl := (*closure)(ctxs[i].Closure); lcl != nil && ctxs[i].Loop != 0 {
			loops = jitLoopIn(r.jitEntryOf(lcl).makesLoops(), ctxs[i], lcl.fn, int(levelPC(i))) != 0
		}
	}
	if !loops && lv.serial == serial && lv.root == root && lv.depth == depthBefore {
		for k < min(lv.n, idx-root) {
			c, at := ctxs[root+1+k], &lv.at[k]
			if c.Closure != at.cl || c.Base != at.base || c.This != at.this || c.Instance != at.instance {
				break
			}
			at.f.pc = uint32(levelPC(root+1+k)) + 1
			k++
		}
	}
	r.frameDepth = depthBefore + k
	if k == idx-root {
		f, e = lv.at[k-1].f, lv.at[k-1].e
		goto framed
	}
	// Made past them, the frames the last call made deeper are no more,
	// and, should a level not be given one, the unwind that follows makes
	// its own where these are.
	lv.serial = 0
	// The PC of the call the level below each made, which says whether a
	// level runs an accessor (jitLevelCallee): the last level's own call,
	// carried from one level to the next, so that each level's description
	// is read once.
	if first := root + 1 + k; first <= idx {
		prevPC = levelPC(first - 1)
	}
	if loops {
		if loop := jitLoopIn(run.e.loops, ctxs[root], runF.cl.fn, int(levelPC(root))); loop != 0 {
			r.jitPushLoopFrame(runF, loop, false)
		}
	}
	for i := root + 1 + k; i <= idx; i++ {
		c := ctxs[i]
		lcl := (*closure)(c.Closure)
		if lcl == nil {
			r.frameDepth = depthBefore
			return false
		}
		callPC := levelPC(i)
		if e = r.jitEntryOf(lcl); e == nil || e.ssa == nil || int(callPC) >= len(lcl.fn.Code) {
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
		f.pc = uint32(callPC) + 1
		f.this, f.newTarget, f.callee, f.args = *(*Value)(unsafe.Pointer(&c.This)), Undefined, nil, nil
		// Its function object, where the call it runs for says (an
		// accessor's): the level's caller's, at its call. Asked only of a
		// property's read or write: this runs for every call of Go a native
		// callee makes, and each level's lookup had cost AWFY's Json 2%.
		callerFn := runF.cl.fn
		if i > root+1 {
			callerFn = (*closure)(ctxs[i-1].Closure).fn
		}
		if pc := int(prevPC); pc < len(callerFn.Code) {
			if op := callerFn.Code[pc].Op; op == bytecode.OpGetProp || op == bytecode.OpSetProp {
				f.callee = r.jitLevelCallee(callerFn, pc, f.this)
			}
		}
		prevPC = callPC
		f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
		f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
		if lcl.fn.Kind == bytecode.KindDerivedConstructor {
			jitDerivedFrame(f, c)
		}
		at := jitLevelFrame{f: f, e: e, cl: c.Closure, base: c.Base, this: c.This, instance: c.Instance}
		if k := i - root - 1; k < len(lv.at) {
			lv.at[k] = at
		} else {
			lv.at = append(lv.at, at)
		}
		if loops && i < idx {
			if loop := jitLoopIn(e.makesLoops(), c, lcl.fn, int(callPC)); loop != 0 {
				r.jitPushLoopFrame(f, loop, false)
			}
		}
	}
	lv.serial, lv.root, lv.depth = serial, root, depthBefore
	if loops {
		// Frames no other call shares.
		lv.serial = 0
	}
framed:
	// What the call runs makes its frames past these levels', where a
	// deeper call's had been.
	lv.n = idx - root
	code := e.ssa
	// The run's frame past the call it makes.
	runF.pc = uint32(levelPC(root)) + 1
	for i := range n {
		r.stack[top+i] = jitGoArg(ctx, i)
	}
	sp := top + n
	r.stackTop, r.stackHigh = sp, max(r.stackHigh, sp)
	e.ssaStats.goCalls++
	s.goCalls++
	prop := in.Op == bytecode.OpGetProp || in.Op == bytecode.OpSetProp
	if in.Op != bytecode.OpIterNextOrJump && in.Op != bytecode.OpIterStep && in.Op != bytecode.OpForOfStart && in.Op != bytecode.OpTypeOf && in.Op != bytecode.OpInstanceOf && in.Op != bytecode.OpSuperCall && !prop && jitCallUnsettled(e, pc, in) {
		r.jitCallSeen(f, e, pc, sp, in)
	}
	r.jitCalleeProfit(e, serial, pc)
	// The run's code, which what the call runs may compile again (the run
	// itself does not hold it, as V8's frames do not): its native code
	// then leaves when its callee returns to it.
	runE, runCode := run.e, run.code
	last, shared := s.ssaCtx, s.ssaSharedGen
	// What the call runs natively runs in contexts past the levels', which
	// hold their state until they go on.
	ctxTop := s.ctxTop
	s.ctxTop = max(ctxTop, idx+1)
	goCallDepth := s.goCallDepth
	s.goCallDepth = r.frameDepth + 1
	var v Value
	var err error
	if prop {
		if in.Op == bytecode.OpGetProp && r.jitSiteMegamorphic(f.cl, e, pc) {
			// A megamorphic site's read that native code did not find,
			// learned as at an exit (jitPolySeen): the megamorphic cache
			// filled for the receiver's shape, which the next such read
			// finds natively, as V8's miss handler fills its stub cache.
			r.jitPolySeen(f, e, pc, sp, f.cl.fn.Code[pc])
		}
		v, err = r.jitPropertyMade(f.cl, pc, sp)
	} else if in.Op == bytecode.OpSuperCall {
		// A native callee's super(...), a derived constructor's
		// (abi.GoCallSuper): on its level's frame, made as a derived
		// construction's is (jitDerivedFrame), its binding as the this
		// slot, the first operand, has it -- bound, a second super(),
		// which throws once the parent has run.
		if f.thisRef == nil || f.callee == nil {
			err = r.throwTypeError("\"super\" is only valid in a derived constructor")
		} else {
			if this := r.stack[sp-n]; !this.IsUninitialized() {
				f.this, f.thisRef = this, &thisBinding{value: this, init: true}
			} else {
				f.thisRef = &thisBinding{value: f.this}
			}
			r.jitSuperSeen(f, e, pc)
			if err = r.jitSuperCall(f, slices.Clone(r.stack[sp-n+1:sp])); err == nil {
				// The level's binding, its context's This, which an exit
				// does not write (ir.Program's This, a snapshot): the frame
				// Go makes for it is bound from it (jitDerivedFrame).
				v = f.this
				*(*Value)(unsafe.Pointer(&ctx.This)) = v
			}
		}
	} else {
		v, err = r.jitCallMade(in, sp)
	}
	s.goCallDepth = goCallDepth
	s.ctxTop = ctxTop
	jitClearArgs(r.stack, top, sp)
	r.stackTop = top
	r.frameDepth = depthBefore
	s.ssaCtx = last
	s.goMark = jitGoMark{serial: serial, edges: r.backEdges - s.pollCredit, hint: e.hint, pc: pc}
	if s.ssaSharedGen != shared {
		// What the call ran shared other values with the contexts past
		// the run's, which its levels' calls run in.
		r.jitShareContexts(root, runF)
	}
	if err != nil || r.stopped != nil || e.invalid || r.jitLearnedDue(f.cl, e) || e.ssa != code || runE.ssa != runCode {
		s.goPending = jitGoPending{set: true, pc: pc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitLevels is the frames the last call of Go from a native callee made
// for its levels (jitGoCallNested), which the next call from the same
// levels, or from those of them it shares, takes as they are, but for
// their PCs: in the run whose pins serial names (0 for none), from context
// root on, n of them, above depth. Nothing else makes a frame there while
// native code runs: a call of Go at the run's own level, which makes the
// built-in's there, forgets them (serial 0), as does the run's native code
// leaving; what a call runs makes its frames above them. Any number of
// levels: limited to four, every call of AWFY's Json's parser, its
// readValue native, made a frame for each of its six to twelve levels.
type jitLevels struct {
	serial         uint64
	root, depth, n int
	at             []jitLevelFrame
}

// jitLevelFrame is a level's frame and entry, and the closure, stack base
// and receiver its context had when it was made.
type jitLevelFrame struct {
	f    *frame
	e    *jitEntry
	cl   unsafe.Pointer
	base uint64
	this abi.Slot
	// instance is a derived construction's (abi.Context's Instance),
	// which its frame's this is until its super() binds it: each
	// construction's its own, its This the same unbound value.
	instance abi.Slot
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
	if st.calleeGoCalls%jitSSAProbe != 0 || st.calleeGoCalls <= e.link.in {
		return
	}
	if st.calleeWork < (st.calleeGoCalls-e.link.in)*jitSSAMinWork {
		e.entrySlow = true
		e.notNative, e.link.entry, e.link.body, e.nativeRetry = true, 0, 0, 0
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
	if o.class == ClassTypedArray {
		// What native code does not store: before the view's storage is
		// looked at, which typedWrite does, for native code's next store;
		// a Float16 element. A view out of its buffer, or past its length,
		// leaves for Go.
		return typedWrite(o, i, v)
	}
	if uint(i) < uint(len(o.elemsOf())) && !isHole(o.elemsOf()[i]) {
		o.extOf().elems[i] = v
		return true
	}
	const want = objExtensible | objArrayLengthWritable
	if o.class != ClassArray || o.flags&(want|objHasSparseElements) != want || !o.noIndexKeys() {
		return false
	}
	for p := o.proto; p != nil; p = p.proto {
		if p.class != ClassObject && p.class != ClassArray || len(p.elemsOf()) != 0 || !p.noIndexKeys() {
			return false
		}
	}
	// What setValueProp would do: setElem, unless the store is so far past
	// the end that the element is to be a property of its own.
	return o.setElem(i, v)
}

// jitAppendFresh appends v to o, an array a loop of map's or filter's made
// (ssa.StoreFresh), at n, its length, as CreateDataProperty defines it --
// as Go's arrayOut pushes it -- whatever its prototypes hold: the array is
// an Array no code but the loop's has. It refuses any other store.
func jitAppendFresh(o *Object, n float64, v Value) bool {
	if o.class != ClassArray || o.flags&objHasSparseElements != 0 || n != float64(len(o.elemsOf())) {
		return false
	}
	o.extOf().elems = append(o.elemsOf(), v)
	return true
}

// jitTypedRead reads element n of the typed array o where native code did
// not (ssa.GoTypedRead): before the view's storage was looked at, which it
// looks at here, for native code's next read; a Float16 element. An
// element that is not a number -- a BigInt, an index the view does not
// have -- it leaves for an exit.
func jitTypedRead(o *Object, n float64) (float64, bool) {
	t, i, ok := typedElemIndex(o, n)
	if !ok || t.tracking {
		return 0, false
	}
	if t.st == nil && t.fixedStorage() == nil {
		return 0, false
	}
	v := t.getElem(i)
	if !v.IsNumber() {
		return 0, false
	}
	return v.num, true
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
	if uint(i) < uint(len(o.elemsOf())) && !isHole(o.elemsOf()[i]) {
		return false
	}
	for p := o; p != nil; p = p.proto {
		if p.class != ClassObject && p.class != ClassArray || p.flags&(objHasSparseElements|objMappedArguments) != 0 || !p.noIndexKeys() {
			return false
		}
		if p != o && len(p.elemsOf()) != 0 {
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

// jitGoBigAdd is GoAdd's sum of two BigInts, a and b, to the call's keep
// cell, where it throws nothing (jitBigArithSafe).
func (r *Runtime) jitGoBigAdd(ctx *abi.Context, a, b Value) bool {
	return r.jitGoBig(ctx, bytecode.OpAdd, a, b)
}

// jitGoBig is op of a and b, BigInts, to the call's keep cell, where it
// throws nothing (jitBigArithSafe), and what of its operands the call
// gives back (jitBigRelease): Go's side of a BigInt's operator from
// native code, ssa.GoArith's, and GoAdd's and GoAppend's of two. A sum, a
// difference or a product -- the operators of BigInts on the heap that
// loops make most -- is checked here and made by bigOp: the checks
// jitBigArithSafe makes, the shortcut jitBigOp tries for two held in their
// values, and the release had each been a call of their own.
func (r *Runtime) jitGoBig(ctx *abi.Context, op bytecode.Op, a, b Value) bool {
	const limit = maxBigIntBits / bits.UintSize
	var v Value
	var err error
	switch {
	case !a.IsBigInt() || !b.IsBigInt() || r.meter != nil:
		return false
	case a.isShortBig() && b.isShortBig() || op != bytecode.OpAdd && op != bytecode.OpSub && op != bytecode.OpMul:
		if !r.jitBigArithSafe(op, a, b) {
			return false
		}
		v, err = r.jitBigOp(op, a, b)
	case op == bytecode.OpMul && jitBigWords(a)+jitBigWords(b) <= limit, op != bytecode.OpMul && max(jitBigWords(a), jitBigWords(b)) < limit:
		v, err = r.bigOp(op, a, b)
	default:
		return false
	}
	if err != nil {
		return false
	}
	if rel := uint8(ctx.GoOp >> abi.GoOpKindShift); rel != 0 {
		r.jitBigRelease(ctx, v, a, b)
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitBigOp is op of a and b, two BigInts, as the interpreter makes it,
// without its conversions, which a BigInt needs none of.
func (r *Runtime) jitBigOp(op bytecode.Op, a, b Value) (Value, error) {
	if a.isShortBig() && b.isShortBig() {
		if v, ok := shortArith(op, a, b); ok {
			return v, nil
		}
	}
	switch op {
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr:
		v, _, err := r.bigBitwise(op, a, b)
		return v, err
	}
	return r.bigOp(op, a, b)
}

// jitConcatOperand reports whether v, joined to a string, is converted
// without running anything or throwing: a primitive jitPlainOperand takes,
// or a BigInt whose digits are far fewer than a string may have.
func jitConcatOperand(v Value) bool {
	return jitPlainOperand(v) || v.IsBigInt() && jitBigWords(v) <= jitConcatBigWords
}

// jitConcatBigWords bounds the words of a BigInt joined to a string from
// native code: some 300,000 digits.
const jitConcatBigWords = 1 << 14

// jitAddOperand reports whether v, added to other, is converted without
// running anything or throwing (jitPlainOperand): a BigInt too, added to a
// string -- with a number it throws -- its digits and the string's well
// within a string's length.
func jitAddOperand(v, other Value) bool {
	if jitPlainOperand(v) {
		return true
	}
	return other.IsString() && jitConcatOperand(v) && other.String().length < maxStringLength-64-jitConcatBigWords*20
}

// jitComparable reports whether a relational comparison converts v
// without a conversion that may throw or call anything: a primitive, a
// BigInt too, that is no symbol.
func jitComparable(v Value) bool {
	return jitPlainOperand(v) || v.IsBigInt()
}

// jitArithOp is the bytecode operator of ir's arithmetic, bitwise or shift
// operator op.
func jitArithOp(op ir.Operator) (bytecode.Op, bool) {
	switch op {
	case ir.Add:
		return bytecode.OpAdd, true
	case ir.Sub:
		return bytecode.OpSub, true
	case ir.Mul:
		return bytecode.OpMul, true
	case ir.Div:
		return bytecode.OpDiv, true
	case ir.Mod:
		return bytecode.OpMod, true
	case ir.Pow:
		return bytecode.OpPow, true
	case ir.BitAnd:
		return bytecode.OpBitAnd, true
	case ir.BitOr:
		return bytecode.OpBitOr, true
	case ir.BitXor:
		return bytecode.OpBitXor, true
	case ir.Shl:
		return bytecode.OpShl, true
	case ir.Shr:
		return bytecode.OpShr, true
	}
	return 0, false
}

// jitBigArithSafe reports whether op of a and b, both BigInts, throws
// nothing, so that Go makes it from native code (ssa.GoAdd, GoArith):
// making an error may call anything (a getter of Error.stackTraceLimit).
// No result past maxBigIntBits, no division by zero, no memory limit (a
// meter) -- the checks bigArith and bigBitwise make, made before.
func (r *Runtime) jitBigArithSafe(op bytecode.Op, a, b Value) bool {
	if !a.IsBigInt() || !b.IsBigInt() || r.meter != nil {
		return false
	}
	const limit = maxBigIntBits / bits.UintSize
	switch op {
	case bytecode.OpAdd, bytecode.OpSub:
		return max(jitBigWords(a), jitBigWords(b)) < limit
	case bytecode.OpMul:
		return jitBigWords(a)+jitBigWords(b) <= limit
	case bytecode.OpDiv, bytecode.OpMod:
		// A BigInt on the heap is never zero.
		return !b.isInt64Big() || b.int64Big() != 0
	case bytecode.OpPow:
		// Of an exponent that is no negative, and a power as many bits as
		// its base's times it at most, within the limit.
		if !b.isInt64Big() {
			return false
		}
		y := b.int64Big()
		return y >= 0 && y < maxBigIntBits && int64(jitBigWords(a)*bits.UintSize)*y <= maxBigIntBits
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor:
		return true
	case bytecode.OpShl, bytecode.OpShr:
		// Either way a shift by at most what keeps a left shift's result
		// within the limit.
		if !b.isInt64Big() {
			return false
		}
		y := b.int64Big()
		return y > -maxBigIntBits && y < maxBigIntBits && jitBigWords(a)*bits.UintSize+int(max(y, -y)) <= maxBigIntBits
	}
	return false
}

// jitBigWords bounds how many words a BigInt's magnitude has: two for one
// held in its value, of at most 64 bits, which are two words on a 32-bit
// machine. The limit is far enough, maxBigIntBits, that bounding it
// refuses little.
func jitBigWords(v Value) int {
	if v.isShortBig() {
		return 2
	}
	return (*BigInt)(v.ref).digits()
}

// jitGoCallWork, which tests set, runs at each call of Go before it is
// answered: what Go may do there, the collector included.
var jitGoCallWork func()

// jitSSAOptimized, which tests set, is given each function's SSA once it
// is optimized, before the backend compiles it.
var jitSSAOptimized func(fn *bytecode.Function, f *ssa.Func)

// KeepLimit is ssa.KeepFeedback's: the build's keep cells' limit, 0 for
// the default (ssa.VirtualKeeps).
func (fb *jitFeedback) KeepLimit() int { return fb.keepLimit }

// ArrayProtos is ssa.AppendFeedback's: the realm's Array.prototype and
// Object.prototype, as a push the code makes natively checks them, where
// neither has an index in its table (jitStoreElem's walk up the chain,
// which native code's checks of their shapes stand for).
func (fb *jitFeedback) ArrayProtos() ([2]ssa.Holder, bool) {
	ap, op := fb.r.proto.array, fb.r.proto.object
	if ap.shape == nil || op.shape == nil || ap.shape.loose || op.shape.loose || ap.proto != op || op.proto != nil ||
		!ap.noIndexKeys() || !op.noIndexKeys() {
		return [2]ssa.Holder{}, false
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(ap.shape), remember(op.shape))
	k.holders = append(k.holders, ap, op)
	return [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(ap)), Shape: uintptr(unsafe.Pointer(ap.shape))},
		{Object: uintptr(unsafe.Pointer(op)), Shape: uintptr(unsafe.Pointer(op.shape))}}, true
}

// GoCalls is ssa.GoFeedback's: the VM answers calls of Go (jitHostCall).
func (fb *jitFeedback) GoCalls() bool { return true }

// WalkedCloseCell is where native code finds the validity cell that says
// closing an array walked in place runs nothing (arrayIterNoReturn): the
// runtime's arrayIterCell, whose first byte is its valid flag. Asked now,
// so that the code finds a cell from its first run if there is one.
func (fb *jitFeedback) WalkedCloseCell() uintptr {
	fb.r.arrayIterNoReturn()
	return uintptr(unsafe.Pointer(&fb.r.arrayIterCell))
}

// A protoCell's valid flag is its first byte, which native code reads
// (WalkedCloseCell).
var _ [0]struct{} = [unsafe.Offsetof(protoCell{}.valid)]struct{}{}

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
	case abi.GoCallFill:
		// A destructuring's step, its cursor the one operand.
		return bytecode.Instr{Op: bytecode.OpIterStep}, 1
	case abi.GoCallForOf:
		// A cursor made, of the iterable, the one operand.
		return bytecode.Instr{Op: bytecode.OpForOfStart}, 1
	case abi.GoCallGet:
		// A property's read, of the object, which the instruction at the
		// call's PC says (jitPropertyMade).
		return bytecode.Instr{Op: bytecode.OpGetProp, A: uint32(op >> abi.GoOpIndexShift)}, 1
	case abi.GoCallSet:
		// A property's write, of the object and the value.
		return bytecode.Instr{Op: bytecode.OpSetProp, A: uint32(op >> abi.GoOpIndexShift)}, 2
	case abi.GoCallTypeOf:
		return bytecode.Instr{Op: bytecode.OpTypeOf}, 1
	case abi.GoCallInstanceOf:
		return bytecode.Instr{Op: bytecode.OpInstanceOf}, 2
	case abi.GoCallSuper:
		// The frame's this slot, then the arguments.
		return bytecode.Instr{Op: bytecode.OpSuperCall, A: argc}, int(argc) + 1
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
	if in.Op == bytecode.OpIterStep {
		// A destructuring's step: its next value, or, the cursor done,
		// undefined (abi.GoCallFill).
		return r.iterStep(r.stack[sp-1])
	}
	if in.Op == bytecode.OpForOfStart {
		// A for...of's or a destructuring's cursor (abi.GoCallForOf).
		return r.startForOf(r.stack[sp-1])
	}
	if in.Op == bytecode.OpTypeOf {
		// A typeof's name (abi.GoCallTypeOf), as executeAt's case makes it.
		return Str(r.typeofString(r.stack[sp-1])), nil
	}
	if in.Op == bytecode.OpInstanceOf {
		// An instanceof (abi.GoCallInstanceOf), as executeAt's case makes
		// it, its Symbol.hasInstance read uncached.
		yes, err := r.instanceOf(r.stack[sp-2], r.stack[sp-1])
		return Bool(yes), err
	}
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
			if fd := o.fn(); fd != nil && fd.closure != nil && fd.closure.hint() == 0 {
				// A closure made since its function was compiled, which a
				// generic call finds by its hint (ssa.CallSite's Generic):
				// given its function's now, not after its warm-up, its next
				// call is native. One whose function has no code is warmed
				// by the calls Go makes of it for native code, as V8's
				// budget counts a function's every invocation, and compiled
				// for native callers once warm: a leaf, which the existing
				// tiers run with no frame (leafCall), had its calls warm
				// nothing, a generic call's of it Go's for good.
				if e := r.jit.entryFor(fd.closure.fn); e != nil {
					fd.closure.setHint(e.hint)
				} else if cl := fd.closure; !cl.jitRefused && cl.realm == r.Realm {
					if a := r.jitFeedbackCalls(); cl.jitCalls < a {
						cl.jitCalls++
					} else if ce := r.jitNativeCallee(cl); ce != nil {
						cl.setHint(ce.hint)
					}
				}
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

// jitInlineLeft counts an exit from the callee inlined at pc, or from its
// call's checks: after jitInlineExits the call is not inlined but made
// natively, if the callee's code allows it, and the code is compiled
// again.
func (r *Runtime) jitInlineLeft(e *jitEntry, pc int) {
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
// (jitFeedback.Intrinsic) or s.charCodeAt (jitFeedback.CharCode), which
// is no call at any depth, or, depth allowing, its own code inlines it, a
// function that may be inlined, with its calls if depth allows. cl is one
// of its closures.
func (r *Runtime) jitInlinesCalls(cl *closure, depth int) bool {
	fn := cl.fn
	if len(fn.Upvalues) != 0 || fn.TopLevel || fn.IsModule || fn.UsesArguments || fn.HasDirectEval || isGeneratorTemplate(fn) {
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
		// What the code makes itself, at any depth: no call.
		if _, ok := fb.Intrinsic(pc); ok {
			continue
		}
		if _, ok := fb.InstanceOf(pc); ok {
			continue
		}
		if fb.CharCode(pc) {
			continue
		}
		if _, ok := fb.FromCharCode(pc); ok {
			continue
		}
		if depth <= 0 {
			return false
		}
		if e == nil {
			if e = r.jit.entryFor(fn); e == nil || e.callSites == nil {
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
	e := r.jit.entryFor(fn)
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
	if in.pool != nil && in.cl.fn.ForwardsArguments {
		// One that forwards its arguments, with what it forwards them to.
		if !r.jitForwardsInline(in.cl, in.obj, depth) {
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
		if e.callSites != nil && (e.callSites[pc] == jitCallNative || e.callSites[pc] == jitCallInlined) {
			// Decided; one still counted toward its decision is not, nor
			// one left to Go (jitCallDone), which was decided from what one
			// call met then -- a callee with no code, a target not seen
			// since -- and is decided again from what the caches know now.
			continue
		}
		obj, read := r.jitCalleeObjectAt(cl, p, pc)
		if obj == nil {
			continue
		}
		o, callee := r.jitClosureFor(cl, obj, in.Op == bytecode.OpNew)
		if callee == nil {
			r.jitSeedBuiltin(cl, e, p, pc, in, obj, read)
			continue
		}
		if in.Op == bytecode.OpNew && !r.jitConstructs(o) {
			// A derived class with no constructor of its own: constructed
			// with its parent's code (jitDerivedBase), never inlined.
			p := r.jitDerivedBase(o)
			if p == nil {
				continue
			}
			callee = p.fn().closure
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
		ce := r.jit.entryFor(cf)
		if ce == nil || ce.ssa == nil {
			// A target the existing tiers have met is compiled for native
			// callers now, as the call would have it compiled once learned
			// (jitSeedCallee). How often it was called is not known: the
			// tree tier may run a call without one (closure.jitCalls).
			if ce = r.jitSeedCallee(callee); ce == nil {
				if r.jitLinkable(callee) {
					e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), callee, o, in))
					e.callSites[pc] = jitCallNative
				}
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
// tiers have met: a.push(v), a.pop() and s.charCodeAt(i), the realm's own;
// new Array(); and G.call(this, ...), G read before it -- each as
// jitCallSeen decides it once the call has left native code, which, for
// calls the code had made in the existing tiers every time, cost each
// function a compile after its first: DeltaBlue's collections' and
// constraints' constructors. The call checks what jitCallSeen finds out at
// its exit -- the receiver an array the fast path takes, or a string, the
// function G -- and leaves for Go otherwise.
func (r *Runtime) jitSeedBuiltin(cl *closure, e *jitEntry, p *ir.Program, pc int, in bytecode.Instr, o *Object, read int) {
	fd := o.fn()
	var x jitInline
	switch {
	case in.Op == bytecode.OpNew && in.A == 0 && o == r.proto.arrayCtor:
		x = jitInline{pc: int32(pc), obj: o, pool: r.jitNewPool(o)}
	case in.Op == bytecode.OpNew && in.A == 1 && o == r.proto.arrayCtor:
		// new Array(n): its arrays with room for the fewest elements a
		// length asks for, more at a refill once one asks for more.
		x = jitInline{pc: int32(pc), obj: o, pool: r.jitNewArrayPool(o, jitArrayRoom(0))}
	case in.Op != bytecode.OpCallMethod || fd == nil:
		return
	case in.A == 0 && fd.elemOp == elemPop && fd.realm == r.Realm:
		x = jitInline{pc: int32(pc), obj: o, pop: true}
	case in.A == 1 && fd.elemOp == elemPush && fd.realm == r.Realm:
		x = jitInline{pc: int32(pc), obj: o, push: true}
	case in.A == 1 && r.jitCharCodeAt.IsObject() && o == r.jitCharCodeAt.Object():
		x = jitInline{pc: int32(pc), obj: o, charCode: true}
	case in.A == 1 && r.jitFromCharCode.IsObject() && o == r.jitFromCharCode.Object():
		x = jitInline{pc: int32(pc), obj: o, fromChar: true}
	case r.jitIsSubstring(o, int(in.A)):
		x = jitInline{pc: int32(pc), obj: o, substring: true}
	case in.A >= 1 && o == r.callFn && read > 0 && (cl.fn.Code[read-1].Op == bytecode.OpGetProp || cl.fn.Code[read-1].Op == bytecode.OpGetGlobal):
		v, ok := r.jitChainValue(cl, p, read-1, jitChainReads)
		if !ok || !v.IsObject() {
			return
		}
		g, gcl := r.jitCallableClosure(cl, v.Object())
		if gcl == nil {
			return
		}
		if ce := r.jit.entryFor(gcl.fn); (ce == nil || ce.ssa == nil) && r.jitSeedCallee(gcl) == nil {
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

// jitCallsNatively reports whether a call cl's function makes, as its
// caches know its callees now, would be made natively once it is
// compiled: one of itself, a recursion; or of a function native callers
// call -- with code they call (not notNative), or none yet, which a call
// seeded for it compiles for them (jitSeedCallee). A function compiled
// once hot for its calls (LowerSSAHot) whose every call is of one native
// callers do not call -- AWFY's Json's readChar, of read, which leaves
// for Go at each substring -- had made each through Go, from native code
// Go entered, where the tree tier made them itself: Json took 5% longer.
func (r *Runtime) jitCallsNatively(cl *closure) bool {
	p, err := r.jitLower(cl.fn, jitLowerHot)
	if err != nil || len(p.Maps) != len(cl.fn.Code) {
		return false
	}
	for pc, in := range cl.fn.Code {
		in = jitCallInstr(in)
		switch in.Op {
		case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		default:
			continue
		}
		obj, _ := r.jitCalleeObjectAt(cl, p, pc)
		if obj == nil {
			continue
		}
		_, callee := r.jitClosureFor(cl, obj, in.Op == bytecode.OpNew)
		if callee == nil {
			continue
		}
		if callee.fn == cl.fn {
			return true
		}
		if ce := r.jit.entryFor(callee.fn); ce == nil || ce.ssa != nil && !ce.notNative {
			return true
		}
	}
	return false
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
		ce := r.jit.entryFor(cf)
		if ce == nil || ce.ssa == nil {
			if ce = r.jitSeedCallee(t.cl); ce == nil && !r.jitLinkable(t.cl) {
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
		if c.shape == nil || c.shape == noShape || c.getter || h == nil || c.idx < 0 || int(c.idx) >= len(h.vals) {
			continue
		}
		prop := h.propAt(c.idx)
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
		if h = cl.scope(); len(r.globalLex.vals) != 0 && r.lexShadows(cl.names[in.A]) {
			if !r.isGlobalScope(h) {
				return nil, -1
			}
			h, idx = r.globalLex, -1
		}
		if h != nil && (idx < 0 || int(idx) >= len(h.vals) || h.shape.keys[idx].key != cl.names[in.A]) {
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
	if h == nil || idx < 0 || int(idx) >= len(h.vals) {
		return nil, -1
	}
	prop := h.propAt(idx)
	if prop.key != cl.names[in.A] || prop.flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || !prop.value.IsObject() {
		return nil, -1
	}
	return prop.value.Object(), read
}

// jitCallableClosure is o and its closure, if a call in cl's code may make
// it natively or inline it, as jitCalled has a callee; or nil.
func (r *Runtime) jitCallableClosure(cl *closure, o *Object) (*Object, *closure) {
	return r.jitClosureFor(cl, o, false)
}

// jitClosureFor is jitCallableClosure for a call, or for a construction
// (jitNativeTargetOf).
func (r *Runtime) jitClosureFor(cl *closure, o *Object, construct bool) (*Object, *closure) {
	if !r.jitNativeTargetOf(o.fn(), cl, construct) {
		return nil, nil
	}
	return o, o.fn().closure
}

// jitNativeTargetOf reports whether native code of cl's may call fd's
// function, or construct with it: one of the script's, unbound, no
// generator's or async function's template, of cl's realm and scope; and
// a class's constructor only for a construction, as a call of one throws,
// which Go does -- a call, through call or apply too, had run its body.
func (r *Runtime) jitNativeTargetOf(fd *funcData, cl *closure, construct bool) bool {
	return fd != nil && fd.native == nil && !fd.bound && fd.closure != nil && !isGeneratorTemplate(fd.closure.fn) && fd.closure.realm == r.Realm &&
		fd.closure.scope() == cl.scope() && (construct || !isClassConstructorKind(fd.closure.fn.Kind))
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
	if in.Op == bytecode.OpGetUpvalue || in.Op == bytecode.OpGetUpvalueCheck {
		// A captured binding's value now, as jitKnownReceiver has it: a
		// constructor a module's closures capture, Box2D's a instanceof S,
		// which had left native code at every test.
		if int(in.A) >= len(cl.upvalues) || cl.upvalues[in.A] == nil {
			return Undefined, false
		}
		if v := cl.upvalues[in.A].get(); !v.IsUninitialized() {
			return v, true
		}
		return Undefined, false
	}
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
		if h != nil && len(r.globalLex.vals) != 0 && r.lexShadows(cl.names[in.A]) {
			if !r.isGlobalScope(h) {
				return Undefined, false
			}
			h = r.globalLex
		}
		if h == nil {
			return Undefined, false
		}
		prop := h.getOwnVisible(cl.names[in.A])
		if !prop.ok() || prop.flags()&propAccessor != 0 || prop.value().IsUninitialized() {
			return Undefined, false
		}
		return prop.value(), true
	case bytecode.OpGetProp, bytecode.OpGetPropThis:
		recv, ok := r.jitChainReceiver(cl, p, q, n)
		if !ok {
			return Undefined, false
		}
		prop := recv.getOwnVisible(cl.names[in.A])
		if !prop.ok() || prop.flags()&propAccessor != 0 {
			return Undefined, false
		}
		return prop.value(), true
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
	case (prev == bytecode.OpGetGlobal || prev == bytecode.OpGetUpvalue || prev == bytecode.OpGetUpvalueCheck) && p.Maps[q-1].Depth == p.Maps[q].Depth-1,
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
	if !same && (in.Op != bytecode.OpNew || r.jitConstructs(o) || r.jitConstructsDerived(o)) && r.jitNativeOrLinked(cl) {
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
	if !r.jitNativeTargetOf(o.fn(), f.cl, in.Op == bytecode.OpNew) {
		return nil, nil
	}
	return o, o.fn().closure
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
				if ce := r.jit.entryFor(cl.fn); (x.obj == o || x.byCode) && ce != nil && ce.notNative {
					jitRetryNative(ce)
				} else if x.cl != nil && (ce == nil || ce.ssa == nil) {
					// One it calls with no code: refused for Go to enter -- a
					// function of calls and no loop, which a caller's call
					// pays for -- and compiled for native callers, which
					// call it then, as a callee its caller seeded is. Left so,
					// a benchmark harness's call, decided for the first
					// benchmark's and taking the next, had every call of
					// AWFY's Towers made by Go, its recursion in the tree tier.
					r.jitNativeCallee(cl)
				}
				return
			}
			n++
		}
	}
	if n >= jitCallTargets || in.Op == bytecode.OpNew && !r.jitConstructs(o) && !r.jitConstructsDerived(o) {
		return
	}
	if r.jitNativeOrLinked(cl) {
		e.nativeCalls = append(e.nativeCalls, r.jitNativeTarget(int32(pc), cl, o, in))
		e.inlineReopt = true
	}
}

// jitNativeOrLinked compiles cl's function for native callers, or reports
// whether a native call may call it through its link before it has code:
// one not refused for good, which native code calls with ExitEnter until
// it is compiled (jitEnterCallee).
func (r *Runtime) jitNativeOrLinked(cl *closure) bool {
	return r.jitNativeCallee(cl) != nil || r.jitLinkable(cl)
}

// jitLinkable reports whether native callers may call cl's function
// through its link with no code yet: one Go runs from the frame native code
// made (abi.ExitEnter), its arguments in its parameters' locals, as a
// generic call makes it (jitLinkFlags) -- not one whose frame is more, an
// arguments object's or a rest parameter's, which that frame would lose.
func (r *Runtime) jitLinkable(cl *closure) bool {
	fn := cl.fn
	if fn.TopLevel || fn.IsModule || fn.UsesArguments || fn.HasDirectEval || isGeneratorTemplate(fn) ||
		jitLinkFlags(fn)&abi.LinkNotGeneric != 0 || fn.Kind == bytecode.KindArrow && (fn.UsesThis || fn.HasTailCall) {
		return false
	}
	ce := r.jit.entryFor(fn)
	if ce != nil && (ce.ssa != nil || ce.calleeRefused) {
		return ce != nil && ce.ssa != nil
	}
	// Nor one whose program native callers' code cannot be compiled from
	// (LowerSSAInline), for good: a call through its link would have Go
	// make its frame at every call (ExitEnter), not the call -- AWFY's
	// Json's readValue, which the lowering refuses, so 60,000 times. A
	// lowering made here, past any compile's room, is held within the
	// budget at once (settle): code waiting for the existing tiers'
	// budget, and calls seen at exits, lower callees between compiles.
	_, err := r.jitLower(fn, jitLowerInline)
	r.jit.settle()
	return err == nil
}

// jitIndexOperands is the receiver and key of the element read in at an
// exit, sp past its operands: a[i]'s, its receiver's kept beneath
// (get_index_this) or not, on the stack; a local's element by a local
// (get_local_index), in the frame. Each is an ir.ArrayRead, which
// jitIndexSeen learns: only a[i]'s had been, so a local's typed array's
// left native code at every invocation -- PDF.js's Huffman tables',
// lengths[val], the rest of each by the interpreter.
func (r *Runtime) jitIndexOperands(f *frame, in bytecode.Instr, sp int) (o, k Value, ok bool) {
	switch in.Op {
	case bytecode.OpGetIndex, bytecode.OpGetIndexThis:
		if sp < 2 || sp > len(r.stack) {
			return Value{}, Value{}, false
		}
		return r.stack[sp-2], r.stack[sp-1], true
	case bytecode.OpGetLocalIndex:
		if int(in.A) >= len(f.locals) || int(in.B) >= len(f.locals) {
			return Value{}, Value{}, false
		}
		return f.locals[in.A], f.locals[in.B], true
	}
	return Value{}, Value{}, false
}

// jitIndexSeen notes an element read at pc that left native code, of
// receiver o at key k (jitIndexOperands): one compiled to read an
// element's cell (ir.Instruction's Reference) whose receiver is a typed
// array, which has no cells, is compiled again to read a typed array's
// number there (ssa.ElementFeedback). Mandreel's code reads its heap's
// views so wherever it only compares what it read, and Go had read every
// one.
func (r *Runtime) jitIndexSeen(e *jitEntry, pc int, o, k Value) {
	p := e.ssaProg
	// Not bounded by jitReoptimizations, which speculations that failed
	// spend: each read is learned once, and the compile waits for the next
	// invocation, taking every one met meanwhile.
	if p == nil || pc >= len(p.Code) {
		return
	}
	in := p.Code[pc]
	if in.Op != ir.ArrayRead || slices.Contains(e.typedReads, int32(pc)) || slices.Contains(e.stringReads, int32(pc)) || slices.Contains(e.keyedReads, int32(pc)) {
		return
	}
	if !in.Reference {
		// Read as a number, for what its uses take it for: a string's
		// element is none, nor an object's property by its name, as a rule
		// -- PDF.js's map[command], stored to an array's element -- and is
		// read as it is from the next lowering on (jitcompile.Hints'
		// ElementReads), and as a string's, or by Go (keyedReads).
		// A string read so, a string's character or an array's element,
		// is a hint of its own (StringReads): the additions of it are Go's
		// from the same lowering on, not learned an exit each.
		keyed := o.IsObject() && k.IsString()
		chars := o.IsString() && k.IsNumber()
		l := r.jit.lowering(e.link.key.Value())
		switch {
		case keyed:
			if _, ok := r.jitKeyedRead(o, k); !ok {
				return
			}
		case o.IsObject() && k.IsNumber():
			// An array's element that is a string, read as it is from the
			// next compile on anyway, its guard having failed.
			if v, ok := r.jitKeyedRead(o, k); !ok || !v.IsString() || slices.Contains(l.hints.StringReads, int32(pc)) {
				return
			}
		case !chars:
			return
		}
		hint := &l.hints.ElementReads
		if !keyed {
			hint = &l.hints.StringReads
		}
		before := jitCapBytes(*hint)
		*hint = append(*hint, int32(pc))
		l.bytes += jitCapBytes(*hint) - before
		r.jit.lowerBytes += jitCapBytes(*hint) - before
		r.jit.relower(l)
		if keyed {
			e.keyedReads = append(e.keyedReads, int32(pc))
		} else if chars {
			e.stringReads = append(e.stringReads, int32(pc))
		}
		e.reopt = true
		return
	}
	switch {
	case o.IsString() && k.IsNumber():
		// A string's element, s[i], compiled for a string from then on
		// (ssa.OpStringChar). PDF.js's font parsers read their programs
		// so, every character leaving native code.
		e.stringReads = append(e.stringReads, int32(pc))
	case o.IsObject() && o.Object().class == ClassTypedArray:
		e.typedReads = append(e.typedReads, int32(pc))
	case o.IsObject() && k.IsString():
		// An object's property by its name, obj[name], which Go reads from
		// native code from then on (ssa.GoIndex), if it would have read
		// this one: PDF.js's Type1 fonts look their commands up so, map[command],
		// every invocation left there for the interpreter.
		if _, ok := r.jitKeyedRead(o, k); !ok {
			return
		}
		e.keyedReads = append(e.keyedReads, int32(pc))
	default:
		return
	}
	// Compiled again on the existing tiers' budget (jitTierUp): its other
	// reads learn theirs meanwhile, and one compile takes them all --
	// Mandreel's sorting reads its heap's views at a dozen sites.
	e.reopt = true
}

// StringRead is ssa.StringElementFeedback's (jitIndexSeen).
func (fb *jitFeedback) StringRead(pc int) bool {
	e := fb.ownEntry()
	return e != nil && slices.Contains(e.stringReads, int32(pc))
}

// TypedRead is ssa.ElementFeedback's (jitIndexSeen).
func (fb *jitFeedback) TypedRead(pc int) bool {
	e := fb.ownEntry()
	return e != nil && slices.Contains(e.typedReads, int32(pc))
}

// KeyedRead is ssa.KeyedFeedback's (jitIndexSeen).
func (fb *jitFeedback) KeyedRead(pc int) bool {
	e := fb.ownEntry()
	return e != nil && slices.Contains(e.keyedReads, int32(pc))
}

// ownEntry is the entry whose code met what fb's function's element reads
// met (jitIndexSeen): the function's own, for an inlined callee too, as an
// inlined callee's property sites are what its own code found. An inlined
// callee's had been the root's alone, nothing: Gameboy's memoryReadNormal,
// parentObj.memory[address] of a typed array, inlined into its callers,
// read it as an array's, which left native code at each read, though its
// own code had learned it.
func (fb *jitFeedback) ownEntry() *jitEntry {
	if fb.root != nil {
		return fb.r.jit.entryFor(fb.fn)
	}
	return fb.e
}

// jitKeyedRead is o[k] where reading it runs nothing (ssa.GoIndex): an
// object's data property by a string key, which the runtime's shared
// property cache finds -- no getter, no proxy, no class that answers keys
// itself -- or a dense array's element, or a typed array's, by an
// integer; and false for any other, which Go reads after an exit.
func (r *Runtime) jitKeyedRead(o, k Value) (Value, bool) {
	if !o.IsObject() {
		return Undefined, false
	}
	obj := o.Object()
	if k.IsNumber() {
		if i := uint32(k.Number()); float64(i) == k.Number() && uint(i) < uint(len(obj.elemsOf())) && obj.flags&objMappedArguments == 0 {
			if v := obj.elemsOf()[i]; !isHole(v) {
				return v, true
			}
		}
		if t, i, ok := typedElemIndex(obj, k.num); ok {
			return t.getElem(i), true
		}
		return Undefined, false
	}
	if !k.IsString() {
		return Undefined, false
	}
	// A string's name, found as it is: no conversion runs anything, and no
	// atom is made -- a key no atom has is no property's.
	n, err := r.toPropertyName(k)
	if err != nil {
		return Undefined, false
	}
	a, ok := n.atom, n.hasAtom
	if !ok {
		a, ok = r.atoms.lookup(n.name)
	}
	if !ok {
		// A name no atom has, which no property has: undefined, if
		// nothing up the chain could answer it itself.
		if jitAbsentKey(obj, 0, false) {
			return Undefined, true
		}
		return Undefined, false
	}
	if a.IsIndex() {
		// A canonical index's name, "44": the element its number names,
		// read as by the number -- as V8's keyed loads take a string's
		// array index -- where a table of tokens indexed by the strings a
		// keyword table answers had left native code at every read.
		return r.jitKeyedRead(o, Uint32(a.Index()))
	}
	if v, ok := r.sharedProp(obj, a); ok {
		return v, true
	}
	if jitAbsentKey(obj, a, true) {
		return Undefined, true
	}
	return Undefined, false
}

// jitHas is key in obj, as the interpreter answers it, where answering it
// runs nothing and throws nothing (ssa.GoHas): an object none of whose
// chain is a proxy, whose has trap would run, or a module namespace, which
// a key may make evaluate a deferred module, by a key that is no object,
// whose conversion would call its toString or Symbol.toPrimitive. Any
// other -- a right operand that is no object, which throws, included -- it
// refuses (ok false), and Go makes it after an exit (jitHost).
func (r *Runtime) jitHas(key, obj Value) (has, ok bool) {
	if !obj.IsObject() || key.IsObject() {
		return false, false
	}
	o := obj.Object()
	for p, depth := o, 0; p != nil; p, depth = p.proto, depth+1 {
		if depth > maxChainDepth || p.class == ClassProxy || p.class == ClassModuleNamespace {
			return false, false
		}
	}
	// A primitive's name, converted with nothing called: a number's, a
	// string's or a symbol's as it is, and a boolean's, null's,
	// undefined's or a BigInt's string.
	n, err := r.toPropertyName(key)
	if err != nil {
		return false, false
	}
	k, known := r.keyFor(o, n)
	if !known {
		// A name no atom has, which no property has.
		return false, true
	}
	has, err = r.hasPropErr(o, k)
	return has, err == nil
}

// jitDelete is delete obj[key], as the interpreter makes it (executeAt's
// delete_prop: toPropertyName, keyFor, deleteProp), where making it runs
// nothing and throws nothing (ssa.GoDelete): an object that is no proxy,
// whose deleteProperty trap would run, no module namespace, which a key
// may make evaluate a deferred module, and none whose own properties a
// class synthesizes -- a typed array's elements, a string wrapper's
// characters -- or ties to a binding -- a mapped arguments object's
// parameters -- by a key that is no object, whose conversion would call
// its toString or Symbol.toPrimitive. An ordinary object's own
// configurable property is removed, its shape changed as the interpreter's
// deletion changes it; an array's element becomes a hole, its length
// unchanged; a property absent is gone already. Any other -- a base that
// is no object, which throws or converts, included -- it refuses (ok
// false), and Go makes it after an exit (jitHost). So does a property
// strict code cannot delete, which throws: deleted as sloppy code deletes
// it, a property that cannot be deleted stays, nothing changed, and the
// interpreter's deletion throws after the exit.
func (r *Runtime) jitDelete(obj, key Value, strict bool) (gone, ok bool) {
	if !obj.IsObject() || key.IsObject() {
		return false, false
	}
	o := obj.Object()
	switch o.class {
	case ClassProxy, ClassModuleNamespace, ClassTypedArray, ClassStringWrapper:
		return false, false
	}
	if o.flags&objMappedArguments != 0 {
		return false, false
	}
	// A primitive's name, converted with nothing called, as jitHas's.
	n, err := r.toPropertyName(key)
	if err != nil {
		return false, false
	}
	k, known := r.keyFor(o, n)
	if !known {
		// A name no atom has, which no property has: deleting it succeeds.
		return true, true
	}
	gone, err = r.deleteProp(o, k, false)
	if err != nil || !gone && strict {
		return false, false
	}
	return gone, true
}

// jitAbsentKey reports whether o's property of a name -- a, or, named
// false, a name no atom has -- is absent from o and from every object up
// its chain, each an ordinary object whose properties are its table's,
// none answering the name itself (synthesized): reading it is undefined,
// with nothing run. A table of strings read by keys it does not have --
// Typescript's StringHashTable, this.table[key] -- had left native code at
// each such read; V8's keyed loads answer a name nothing has through its
// chain's validity cell. Dictionary objects, as such tables become, are
// searched as any are (findOwn).
func jitAbsentKey(o *Object, a Atom, named bool) bool {
	for h, depth := o, 0; h != nil; h, depth = h.proto, depth+1 {
		if depth > maxChainDepth || !shapeClass(h.class) || named && (synthesized(h.class, a) || h.findOwn(a) >= 0) {
			return false
		}
	}
	return true
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

// jitReadsArguments reports whether fn uses its arguments object only to
// read it by index, arguments[k] (arguments_index): an object made only on
// demand, never pushed, forwarded to no call (bytecode's
// ForwardsArguments, no apply_arguments), and aliasing no parameter -- a
// sloppy function's with parameters would. Inlined, its reads are the
// call's arguments (ssa.InlineSite's ReadsArguments), as V8 eliminates
// the arguments object; a function that reads arguments had been called
// by Go from native code at each call, its code the tree tier's --
// microbench.js's arguments_test.
func jitReadsArguments(fn *bytecode.Function) bool {
	if !fn.UsesArguments || !fn.ForwardsArguments || fn.MappedArguments && fn.ParamCount > 0 || fn.Leaf == bytecode.LeafForward {
		return false
	}
	return !slices.ContainsFunc(fn.Code, func(in bytecode.Instr) bool { return in.Op == bytecode.OpApplyArguments })
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
	if in.Op == bytecode.OpNew && cl.fn.ForwardsArguments {
		// A.init.apply(this, arguments), and more: each method it forwards
		// to, inlined in it.
		return r.jitConstructs(o) && r.jitForwardsInline(cl, o, jitInlineDepth)
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
	// A constructor whose returns all return its receiver, as a base
	// class's own does, or that has none: the object is the result either
	// way.
	return in.Op != bytecode.OpNew || r.jitConstructs(o) && cl.fn.ReturnsReceiver
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
	if e := r.jit.entryFor(tc.fn); (e == nil || e.callSites == nil) && r.jit.seeding == 0 {
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
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return nil
	}
	m := p.value().Object().getOwnVisible(cl.names[fn.Code[1].A])
	if !m.ok() || m.flags()&propAccessor != 0 || !m.value().IsObject() {
		return nil
	}
	t := m.value().Object()
	if !r.jitNativeTargetOf(t.fn(), cl, false) {
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
// with the construction's arguments (ssa.InlineSite's Forward): of fwd, a
// LeafForward's construction, the method this.k is (jitForwardMethod); of
// another's, one that forwards its arguments (bytecode's
// ForwardsArguments), the function the call's f.apply reads f as
// (jitForwardSiteTarget) -- one that forwards them in turn inlined so too,
// its frame keeping them (ssa.InlineSite's Arguments).
func (fb *jitFeedback) forwardInline(pc int, fwd *jitForwardFrame) (ssa.InlineSite, bool) {
	r := fb.r
	if pc >= len(fb.fn.Code) || fb.fn.Code[pc].Op != bytecode.OpApplyArguments || r.applyFn == nil {
		return ssa.InlineSite{}, false
	}
	var t *Object
	if fwd == fb.fwd {
		t = r.jitForwardMethod(fb.cl, fwd.ctor)
	} else {
		t = r.jitForwardSiteTarget(fb.cl, fwd.ctor, pc)
	}
	if t == nil {
		return ssa.InlineSite{}, false
	}
	tc := t.fn().closure
	kind, forwards := jitLowerInline, tc.fn.ForwardsArguments && tc.fn.Leaf != bytecode.LeafForward
	if forwards {
		kind = jitLowerForward
	}
	p, err := fb.r.jitLower(tc.fn, kind)
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
	callee := &jitFeedback{r: r, fn: tc.fn, cl: tc, root: k}
	if forwards {
		callee.fwdArgs = &jitForwardFrame{ctor: fwd.ctor, pool: fwd.pool, argc: fwd.argc}
	}
	return ssa.InlineSite{
		Program: p, Feedback: callee,
		Callee: uintptr(unsafe.Pointer(t)), Closure: uintptr(unsafe.Pointer(tc)),
		Argc: fwd.argc, Method: true, Params: tc.fn.ParamCount, ThisSlot: this,
		Forward: true, Apply: uintptr(unsafe.Pointer(r.applyFn)), Arguments: forwards, Upvalues: jitUpvalueCells(tc),
	}, true
}

// jitUpvalueCells is the address of cl's captured bindings' cells, which
// an inlined frame of its reads them from (ssa.InlineSite's Upvalues), or
// 0 for none.
func jitUpvalueCells(cl *closure) uintptr {
	if len(cl.upvalues) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(cl.upvalues)))
}

// jitWritesUpvalues reports whether cl's code assigns a binding it
// captured, which an inlined frame of its leaves to Go.
func jitWritesUpvalues(cl *closure) bool {
	return len(cl.upvalues) != 0 && slices.ContainsFunc(cl.fn.Code, func(x bytecode.Instr) bool {
		switch x.Op {
		case bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
			return true
		}
		return false
	})
}

// jitForwardSiteTarget is the function the apply_arguments at pc of cl's
// code, a function that only forwards its arguments (bytecode's
// ForwardsArguments) constructed as o, forwards them to: f of
// f.apply(this, arguments), as the read of f finds it now -- a property of
// this, the construction's receiver, on o's prototype; of anything else, as
// the reads before it find it (jitChainValue), Box2D's A.init -- a
// function the script made, of cl's realm and scope; or nil.
func (r *Runtime) jitForwardSiteTarget(cl *closure, o *Object, pc int) *Object {
	code := cl.fn.Code
	if pc < 4 || pc >= len(code) || code[pc].Op != bytecode.OpApplyArguments || code[pc-1].Op != bytecode.OpPushThis ||
		code[pc-2].Op != bytecode.OpGetPropThis || int(code[pc-2].A) >= len(cl.names) || r.atoms.name(cl.names[code[pc-2].A]) != "apply" ||
		code[pc-3].Op != bytecode.OpGetProp || int(code[pc-3].A) >= len(cl.names) {
		return nil
	}
	var v Value
	if code[pc-4].Op == bytecode.OpPushThis {
		p := o.getOwnVisible(atomPrototype)
		if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
			return nil
		}
		m := p.value().Object().getOwnVisible(cl.names[code[pc-3].A])
		if !m.ok() || m.flags()&propAccessor != 0 {
			return nil
		}
		v = m.value()
	} else {
		prog, err := r.jitLower(cl.fn, jitLowerForward)
		if err != nil {
			return nil
		}
		var ok bool
		if v, ok = r.jitChainValue(cl, prog, pc-3, jitChainReads); !ok {
			return nil
		}
	}
	if !v.IsObject() || !r.jitNativeTargetOf(v.Object().fn(), cl, false) {
		return nil
	}
	return v.Object()
}

// jitHasRun reports whether cl's code has run, as a cache of its filled
// says: a function that forwards its arguments is inlined for what its
// caches learned -- its reads of what it forwards to -- which an inlining
// decided before it ran would leave native code at, every construction,
// until its exits gave the inlining up.
func jitHasRun(cl *closure) bool {
	for i := range cl.ic {
		if cl.ic[i].fills != 0 {
			return true
		}
	}
	return false
}

// jitForwardsInline reports whether a construction of o, cl's function, one
// that only forwards its arguments (bytecode's ForwardsArguments), may be
// inlined: it has run (jitHasRun), assigns no binding it captured, and each
// function it forwards them to (jitForwardSiteTarget) may be inlined in it
// -- with its calls, depth levels on, or, one that forwards them in turn,
// so.
func (r *Runtime) jitForwardsInline(cl *closure, o *Object, depth int) bool {
	if depth <= 0 || jitWritesUpvalues(cl) || !jitHasRun(cl) {
		return false
	}
	forwards := false
	for pc, in := range cl.fn.Code {
		if in.Op != bytecode.OpApplyArguments {
			continue
		}
		forwards = true
		t := r.jitForwardSiteTarget(cl, o, pc)
		if t == nil {
			return false
		}
		tc := t.fn().closure
		switch {
		case tc.fn == cl.fn || jitWritesUpvalues(tc):
			return false
		case tc.fn.ForwardsArguments:
			if tc.fn.Leaf == bytecode.LeafForward || !r.jitForwardsInline(tc, o, depth-1) {
				return false
			}
		case !r.jitInlinable(tc.fn) && !r.jitInlinesCalls(tc, depth-1):
			return false
		}
	}
	return forwards
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
		if fd := o.fn(); fd != nil && fd.closure != cl {
			// A derived class's, constructed with its parent's code
			// (jitDerivedBase).
			x.parent = o.proto
		}
	}
	return x
}

// jitDerivedBase is the class a class o extends, if o's constructor is the
// one a class without one has -- constructor(...args) { super(...args) },
// which hands its arguments on (bytecode.SuperForward) -- o has no fields
// to give its instances, and native code may construct with the class it
// extends (jitConstructs); or nil. new o(...) then does what a
// construction with that class does, its object made as o makes one --
// o's prototype, o's root shape and room, as constructWithTarget makes it
// for super() to hand on -- and its result o's, as V8 skips a default
// derived constructor (FindNonDefaultConstructorOrConstruct). AWFY's
// Havlak constructs a DictIdEntry, a DictEntry's subclass, for each entry
// of its dictionaries: each had been a call of Go.
func (r *Runtime) jitDerivedBase(o *Object) *Object {
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.native != nil || fd.bound || fd.ctorKind != ctorDerived || fd.fieldInit != nil ||
		fd.closure.realm != r.Realm || o.class != ClassFunction || o.findOwn(atomPrototype) < 0 || !jitForwardsSuper(fd.closure.fn) {
		return nil
	}
	if p := o.proto; p != nil && (r.jitConstructs(p) || r.jitConstructsDerived(p)) {
		return p
	}
	return nil
}

// jitConstructsDerived reports whether native code may construct with o, a
// derived class whose constructor is its own -- not a class without one,
// whose construction runs the code of the class it extends (jitDerivedBase)
// -- called natively as a construction (ssa.CallSite's Derived): its
// instance from a pool made for o, as constructWithTarget makes it before
// the body runs, its super() Go's, which runs the parent and the class's
// field initializers.
func (r *Runtime) jitConstructsDerived(o *Object) bool {
	fd := o.fn()
	return fd != nil && fd.closure != nil && fd.native == nil && !fd.bound && fd.ctorKind == ctorDerived && fd.superCtor == o &&
		fd.closure.realm == r.Realm && o.class == ClassFunction && o.findOwn(atomPrototype) >= 0 && !jitForwardsSuper(fd.closure.fn)
}

// jitForwardsSuper reports whether fn is the constructor a derived class
// without one has: super(...args), its this then the result.
func jitForwardsSuper(fn *bytecode.Function) bool {
	c := fn.Code
	return len(c) == 3 && c[0].Op == bytecode.OpSuperCall && c[0].A == 0 && c[0].B == bytecode.SuperForward &&
		c[1].Op == bytecode.OpDrop && c[2].Op == bytecode.OpReturnUndef
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
	if !r.jitNativeTargetOf(o.fn(), f.cl, false) {
		return nil, nil
	}
	return o, o.fn().closure
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

// jitCharCodes is String.prototype.charCodeAt, the realm's own, if a
// method call at the top of the stack calls it with one argument on a
// string, or nil.
func (r *Runtime) jitCharCodes(sp int, in bytecode.Instr) *Object {
	if in.Op != bytecode.OpCallMethod || in.A != 1 || !r.jitCharCodeAt.IsObject() {
		return nil
	}
	callee, recv := r.stack[sp-2], r.stack[sp-3]
	if !callee.IsObject() || callee.Object() != r.jitCharCodeAt.Object() || !recv.IsString() {
		return nil
	}
	return callee.Object()
}

// jitForEaches is Array.prototype.forEach, or another of jitLoops, the
// realm's own, if a method call at the top of the stack calls it on an
// array with one argument, or two, the first a closure of the realm's, and
// that closure and its function object; or nil. One of jitTypedLoops on a
// typed array (jitLoopTyped) is one too, and its prototype the fourth.
func (r *Runtime) jitForEaches(sp int, in bytecode.Instr) (*Object, *closure, *Object, *Object) {
	argc := int(in.A)
	if in.Op != bytecode.OpCallMethod || argc != 1 && argc != 2 {
		return nil, nil, nil, nil
	}
	callee, recv, cb := r.stack[sp-argc-1], r.stack[sp-argc-2], r.stack[sp-argc]
	if !callee.IsObject() || !cb.IsObject() {
		return nil, nil, nil, nil
	}
	var proto *Object
	switch kind := r.jitLoopKind(callee.Object()); {
	case kind == 0:
		return nil, nil, nil, nil
	case kind&abi.LoopTyped != 0:
		if r.jitLoopTyped(recv) == nil {
			return nil, nil, nil, nil
		}
		proto = recv.Object().proto
	case r.plainArray(recv) == nil:
		return nil, nil, nil, nil
	}
	fd := cb.Object().fn()
	if fd == nil || fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm {
		return nil, nil, nil, nil
	}
	return callee.Object(), fd.closure, cb.Object(), proto
}

// jitForEachSeen notes the forEach at pc Go made, of callback cb, cbObj
// its object: native code makes it a loop of its own (jitFeedback's
// ForEach) from the next compile, its callback called natively -- a
// closure with no code compiled for native callers first, as a call's
// callee is (jitCallTarget). One whose callback is another function's, the
// call is Go's, as a call of too many targets is -- and so is a typed
// array's of another type, proto its typed arrays' prototype.
func (r *Runtime) jitForEachSeen(e *jitEntry, fn *bytecode.Function, pc int, o *Object, cb *closure, cbObj *Object, proto *Object) {
	for i := range e.nativeCalls {
		x := &e.nativeCalls[i]
		if int(x.pc) != pc || !x.forEach {
			continue
		}
		if x.cb.fn != cb.fn || x.obj != o || x.proto != proto {
			return
		}
		if x.cbObj != cbObj {
			// A closure made anew at each call of its maker: called by its
			// code (ByCode).
			x.byCode = true
		}
		if ce := r.jit.entryFor(cb.fn); ce == nil || ce.ssa == nil {
			if r.jitNativeCallee(cb) != nil {
				e.inlineReopt = true
			}
		}
		if e.inlinePending && e.inlineReopts < 2*jitInlineReoptimizations {
			// Seen again, Go's still: compiled again for it, as a call
			// decided native is (jitCallSeen).
			e.inlinePending, e.inlineReopt = false, true
		}
		return
	}
	if e.callSites == nil {
		e.callSites = make([]uint8, len(fn.Code))
	}
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, forEach: true, cb: cb, cbObj: cbObj, proto: proto})
	e.callSites[pc] = jitCallNative
	e.inlinePending = true
	if ce := r.jit.entryFor(cb.fn); ce == nil || ce.ssa == nil {
		r.jitNativeCallee(cb)
	}
}

// ForEach is ssa.ForEachFeedback's: the forEach, or another of jitLoops,
// the method call at pc has made on an array (jitForEachSeen), its callback
// called natively as a method call of three arguments -- the element, its
// index, the array -- or, reduce's, four, the accumulator first, by its
// code where closures of it were made anew (ByCode), as a call's target is
// (closureCallSite). One whose callback native callers may not call yet is
// Go's.
func (fb *jitFeedback) ForEach(pc int) (ssa.ForEachSite, bool) {
	e := fb.e
	if e == nil || fb.root != nil || fb.fwd != nil || fb.cl == nil || pc >= len(fb.fn.Code) || jitCallOp(fb.fn.Code[pc].Op) != bytecode.OpCallMethod {
		return ssa.ForEachSite{}, false
	}
	for _, x := range jitTargets(e) {
		kind := fb.r.jitLoopKind(x.obj)
		if int(x.pc) != pc || !x.forEach || x.cb == nil || kind == 0 {
			continue
		}
		var proto uintptr
		if kind&abi.LoopTyped != 0 {
			// A typed array's, of the type its prototype is.
			t := slices.Index(fb.r.typedArrayProtos[:], x.proto)
			if x.proto == nil || t < 0 {
				return ssa.ForEachSite{}, false
			}
			kind |= t << abi.LoopTypedShift
			proto = uintptr(unsafe.Pointer(x.proto))
			k := fb.keep()
			k.holders = append(k.holders, x.proto)
		}
		cb := jitInline{pc: x.pc, cl: x.cb, obj: x.cbObj, byCode: x.byCode}
		argc := uint32(3)
		if kind&abi.LoopKindMask == abi.LoopReduce {
			argc = 4
		}
		site, ok := fb.closureCallSite(pc, cb, bytecode.Instr{Op: bytecode.OpCallMethod, A: argc}, nil)
		if !ok {
			return ssa.ForEachSite{}, false
		}
		fb.loops = true
		return ssa.ForEachSite{Fn: uintptr(unsafe.Pointer(x.obj)), Kind: kind, Proto: proto, Callback: site}, true
	}
	return ssa.ForEachSite{}, false
}

// setLoopSlots notes that e's code for fn holds slots of its loops (ssa's
// forEachCall) n past its frame's operands' first: its native callers make
// its frame that big (jitLink's frame), so that the frames of the callees
// its loops call start past them, as Go's entries do (runSSALoop).
func (e *jitEntry) setLoopSlots(fn *bytecode.Function, n int) {
	e.loopSlots = n
	if l := e.link; l != nil && n > fn.MaxStack {
		l.frame = max(l.frame, uint64(fn.LocalCount+n))
	}
}

// jitLoopDone stands for an exit kind once Go has finished the loop it
// left (jitFinishExit): the frame goes on after the call.
const jitLoopDone = ^uint64(0)

// jitLoopAt is the loop of a built-in's call native code was making in
// ctx, f's context, at its exit at pc, if any (abi.Context's Loop), and
// clears it there: only code that makes such loops (loops, its jitEntry's
// loops) has one, at a method call, which its entries clear.
func jitLoopAt(loops bool, ctx *abi.Context, f *frame, pc int) uint64 {
	loop := jitLoopIn(loops, ctx, f.cl.fn, pc)
	ctx.Loop = 0
	return loop
}

// jitLoopIn is jitLoopAt, the word left as it is: the loop is under way
// still, a level's whose callee is a frame Go makes.
func jitLoopIn(loops bool, ctx *abi.Context, fn *bytecode.Function, pc int) uint64 {
	loop := ctx.Loop
	if loop == 0 || !loops || pc >= len(fn.Code) || jitCallOp(fn.Code[pc].Op) != bytecode.OpCallMethod {
		return 0
	}
	return loop
}

// jitPushLoopFrame pushes, past f's, the frame of the built-in whose call
// f's code makes a loop of (abi.Context's Loop, loop), its callback's
// caller -- as Go's forEach has a frame while its callback runs
// (nativeFrame) -- for the frames Go makes of the loop's callees, which a
// stack trace shows: "at Array.forEach". Its receiver is the loop's array
// where f holds the loop's slots (written, a level Go finishes), or else
// Array.prototype, an array all the same, which the trace names the frame
// by: a level whose call of Go Go makes keeps them in registers, and the
// loop is only ever an array's.
func (r *Runtime) jitPushLoopFrame(f *frame, loop uint64, written bool) {
	kind := int(loop >> 32)
	fe := r.jitLoopFn(kind)
	this := Obj(r.proto.array)
	if t := kind >> abi.LoopTypedShift; kind&abi.LoopTyped != 0 && t < len(r.typedArrayProtos) && r.typedArrayProtos[t] != nil {
		this = Obj(r.typedArrayProtos[t])
	}
	if v := r.stack[f.base+int(loop&0xffffffff)-1]; written && v.IsObject() {
		this = v
	}
	nf := r.pushFrame()
	nf.cl, nf.native, nf.this, nf.newTarget, nf.callee, nf.args = nil, fe.fn().name, this, Undefined, fe, nil
	if len(nf.handlers) != 0 {
		nf.handlers = nf.handlers[:0]
	}
	if len(nf.openUpvalues) != 0 {
		nf.openUpvalues = nf.openUpvalues[:0]
	}
}

// jitLoopLocals is how many of a loop's slots Go goes on from (ssa's
// loopLocals): the array, the callback, this -- reduce's accumulator --
// the index, the length, the array map or filter makes, the element filter
// or find gave the callback, and its result, uninitialized if none yet.
const jitLoopLocals = 8

// jitLoopFinish finishes the loop native code was making of a built-in's
// call in f, at pc, at an exit there, as V8's builtin continuation frames
// finish the builtin: loop is the context's Loop word (abi.Context), which says
// which built-in's and where its slots are past f's operands, and after
// whether the loop's call of its callback for the index there was made --
// its callee left native code, and Go finished it, v and err what it
// returned or threw -- or not; or the loop has its result, an exit after
// the call. Go goes on from that index, the callback's result first used
// as the built-in uses it, or from the next one, to the length read as it
// began, as Go's own does (builtins.go): map and filter into the array
// the loop made, an Array whose elements up to the index it appended. f is
// past its call meanwhile, as the interpreter's frame is during one, which
// a stack trace the callback takes shows. The slots keep nothing after.
func (r *Runtime) jitLoopFinish(f *frame, pc int, loop uint64, after bool, v Value, err error) (Value, error) {
	at := f.base + int(loop&0xffffffff) - 1
	slots := r.stack[at : at+jitLoopLocals]
	arr, cb, this, k, n, out, elem, res := slots[0], slots[1], slots[2], slots[3], slots[4], slots[5], slots[6], slots[7]
	clear(slots)
	if err != nil || r.stopped != nil {
		// A throw ends it, as Go's forEach ends at its callback's.
		return v, err
	}
	fe := r.jitLoopFn(int(loop >> 32))
	typed := int(loop>>32)&abi.LoopTyped != 0
	kind := int(loop>>32) & abi.LoopKindMask
	makes := kind == abi.LoopMap || kind == abi.LoopFilter
	var t *typedArrayData
	if typed {
		t, _ = arr.Object().dataOf().(*typedArrayData)
	}
	if fe == nil || !arr.IsObject() || !k.IsNumber() || !n.IsNumber() || makes && (typed || !out.IsObject()) || typed && t == nil {
		panic("jit: a loop's slots are not a built-in's")
	}
	if !after && !res.IsUninitialized() {
		// An exit after the callback's call: its result the loop's.
		after, v = true, res
	}
	from := int64(k.Number())
	r.jit.loopsFinished++
	f.pc = uint32(pc + 1)
	// The built-in's frame, as Go's has one (nativeFrame), which a stack
	// trace of what the callback throws shows.
	if err := r.nativeFrame(fe, fe.fn(), arr, []Value{cb, this}, Undefined); err != nil {
		return Undefined, err
	}
	defer func() { r.frameDepth-- }()
	a := &arrayLike{o: arr.Object(), n: int64(n.Number())}
	var o *arrayOut
	if makes {
		// The loop's array, of the realm's species, as GoNewArray made it.
		o = &arrayOut{o: out.Object(), plain: true, n: int64(len(out.Object().elemsOf()))}
	}
	// use is what the built-in does with the callback's result res at index
	// i, of element el: whether it is done, and its result then.
	use := func(i int64, el, res Value) (bool, Value, error) {
		switch kind {
		case abi.LoopReduce:
			this = res
		case abi.LoopMap:
			if err := o.push(r, res); err != nil {
				return true, Undefined, err
			}
		case abi.LoopFilter:
			if res.Truthy() {
				if err := o.push(r, el); err != nil {
					return true, Undefined, err
				}
			}
		case abi.LoopSome:
			if res.Truthy() {
				return true, True, nil
			}
		case abi.LoopEvery:
			if !res.Truthy() {
				return true, False, nil
			}
		case abi.LoopFind:
			if res.Truthy() {
				return true, el, nil
			}
		case abi.LoopFindIndex:
			if res.Truthy() {
				return true, Float(float64(i)), nil
			}
		}
		return false, Undefined, nil
	}
	if after {
		if done, x, err := use(from, elem, v); done {
			return x, err
		}
		from++
	}
	// As Go's own: the arguments' list made once.
	var argv [4]Value
	for i := from; i < a.n; i++ {
		var el Value
		if typed {
			// As %TypedArray%.prototype's (builtins_typedarray.go): every
			// index, undefined where the view no longer has it.
			el = t.getElem(int(i))
		} else if kind == abi.LoopFind || kind == abi.LoopFindIndex {
			// A hole too, undefined.
			if el, err = a.get(r, i); err != nil {
				return Undefined, err
			}
		} else {
			var present bool
			if el, present, err = a.at(r, i); err != nil {
				return Undefined, err
			}
			if !present {
				if kind == abi.LoopMap {
					// A hole in the array stays a hole in map's.
					if err := o.pushHole(); err != nil {
						return Undefined, err
					}
				}
				continue
			}
		}
		args, recv := argv[:3], this
		argv[0], argv[1], argv[2] = el, Float(float64(i)), arr
		if kind == abi.LoopReduce {
			args, recv = argv[:4], Undefined
			argv[0], argv[1], argv[2], argv[3] = this, el, Float(float64(i)), arr
		}
		res, err := r.callDirect(cb, recv, args)
		if err != nil {
			return Undefined, err
		}
		if done, x, err := use(i, el, res); done {
			return x, err
		}
	}
	switch kind {
	case abi.LoopReduce:
		return this, nil
	case abi.LoopMap, abi.LoopFilter:
		return o.value(), nil
	case abi.LoopSome:
		return False, nil
	case abi.LoopEvery:
		return True, nil
	case abi.LoopFindIndex:
		return Float(-1), nil
	}
	return Undefined, nil
}

// jitSearchKind is the kind of o among jitSearches, or -1.
func (r *Runtime) jitSearchKind(o *Object) int {
	if o == nil {
		return -1
	}
	return slices.Index(r.jitSearches[:], o)
}

// jitIndexOfs is one of jitSearches, the realm's own, if a method call at
// the top of the stack calls it with one argument or two on a string --
// String.prototype's -- or an array of the realm's, Array.prototype's; or
// nil.
func (r *Runtime) jitIndexOfs(sp int, in bytecode.Instr) *Object {
	argc := int(in.A)
	if jitCallOp(in.Op) != bytecode.OpCallMethod || argc != 1 && argc != 2 {
		return nil
	}
	callee, recv := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() {
		return nil
	}
	switch k := r.jitSearchKind(callee.Object()); {
	case k < 0:
		return nil
	case k < jitSearchArrays && !recv.IsString(), k >= jitSearchArrays && r.plainArray(recv) == nil:
		return nil
	}
	return callee.Object()
}

// IndexOf is ssa.IndexOfFeedback's: the search, of jitSearches, the method
// call at pc has made on a string or an array (jitIndexOfs), and its kind.
func (fb *jitFeedback) IndexOf(pc int) (uintptr, int, bool) {
	e := fb.e
	if e == nil || fb.root != nil || pc >= len(fb.fn.Code) || jitCallOp(fb.fn.Code[pc].Op) != bytecode.OpCallMethod {
		return 0, 0, false
	}
	for _, x := range jitTargets(e) {
		if int(x.pc) != pc || !x.indexOf {
			continue
		}
		if k := fb.r.jitSearchKind(x.obj); k >= 0 {
			return uintptr(unsafe.Pointer(x.obj)), k, true
		}
	}
	return 0, 0, false
}

// jitSearch is the answer of the search of kind, one of jitSearches, on
// recv with args (Go's GoIndexOf): the built-in's own, by its Go function,
// where nothing it does can run script or throw -- a string searched for
// primitives that are no symbol, which convert with nothing called, and
// includes's search no regular expression; an array of the realm's, dense,
// whose prototypes hold no index, which it reads with nothing asked, and
// its position such a primitive. It refuses the rest.
func (r *Runtime) jitSearch(kind int, recv Value, args []Value) (Value, bool) {
	if kind < 0 || kind >= jitSearchKinds || r.jitSearches[kind] == nil {
		return Undefined, false
	}
	if kind < jitSearchArrays {
		if !recv.IsString() {
			return Undefined, false
		}
		for _, a := range args {
			if !jitPlainOperand(a) {
				return Undefined, false
			}
		}
	} else {
		if o := r.plainArray(recv); o == nil || !r.noInheritedIndices(o) || len(args) > 1 && !jitPlainOperand(args[1]) {
			return Undefined, false
		}
	}
	fd := r.jitSearches[kind].fn()
	if fd == nil || fd.native == nil {
		return Undefined, false
	}
	v, err := fd.native(r, recv, args)
	if err != nil {
		return Undefined, false
	}
	return v, true
}

// jitParseInts is the realm's parseInt, if a plain call at the top of the
// stack calls it with one argument, a number; or nil.
func (r *Runtime) jitParseInts(sp int, in bytecode.Instr) *Object {
	if jitCallOp(in.Op) != bytecode.OpCall || in.A != 1 || r.jitParseInt == nil || !jitEncoding.Round {
		return nil
	}
	if callee := r.stack[sp-2]; !callee.IsObject() || callee.Object() != r.jitParseInt || !r.stack[sp-1].IsNumber() {
		return nil
	}
	return r.jitParseInt
}

// jitBigStrings is BigInt.prototype.toString, the realm's own, if a method
// call at the top of the stack, of no argument or one, calls it on a
// BigInt; or nil.
func (r *Runtime) jitBigStrings(sp int, in bytecode.Instr) *Object {
	argc := int(in.A)
	if jitCallOp(in.Op) != bytecode.OpCallMethod || argc > 1 || r.jitBigToString == nil {
		return nil
	}
	if callee := r.stack[sp-argc-1]; !callee.IsObject() || callee.Object() != r.jitBigToString || !r.stack[sp-argc-2].IsBigInt() {
		return nil
	}
	return r.jitBigToString
}

// BigString is ssa.BigStringFeedback's: BigInt.prototype.toString, the
// realm's own, which the method call at pc has made on a BigInt
// (jitBigStrings).
func (fb *jitFeedback) BigString(pc int) (uintptr, bool) {
	e := fb.e
	if e == nil || fb.root != nil || pc >= len(fb.fn.Code) || jitCallOp(fb.fn.Code[pc].Op) != bytecode.OpCallMethod {
		return 0, false
	}
	for _, x := range jitTargets(e) {
		if int(x.pc) == pc && x.bigString && x.obj != nil && x.obj == fb.r.jitBigToString {
			return uintptr(unsafe.Pointer(x.obj)), true
		}
	}
	return 0, false
}

// jitBigString is GoBigString's string of recv, a BigInt, in the radix the
// call gives, if it gives one: BigInt.prototype.toString's, where it
// converts with nothing run and nothing thrown -- no radix, undefined, or
// a number that is an integer from 2 to 36. It refuses the rest.
func jitBigString(recv Value, args []Value) (Value, bool) {
	if !recv.IsBigInt() {
		return Undefined, false
	}
	radix := 10
	if len(args) > 0 && !args[0].IsUndefined() {
		if !args[0].IsNumber() {
			return Undefined, false
		}
		n := args[0].Number()
		if n != math.Trunc(n) || n < 2 || n > 36 {
			return Undefined, false
		}
		radix = int(n)
	}
	return Str(bigString(recv, radix)), true
}

// ParseInt is ssa.ParseIntFeedback's: the realm's parseInt, which the call
// at pc has made of a number (jitParseInts).
func (fb *jitFeedback) ParseInt(pc int) (uintptr, bool) {
	e := fb.e
	if e == nil || fb.root != nil || pc >= len(fb.fn.Code) || jitCallOp(fb.fn.Code[pc].Op) != bytecode.OpCall {
		return 0, false
	}
	for _, x := range jitTargets(e) {
		if int(x.pc) == pc && x.parseInt && x.obj != nil && x.obj == fb.r.jitParseInt {
			return uintptr(unsafe.Pointer(x.obj)), true
		}
	}
	return 0, false
}

// jitSubstrings is String.prototype's substring, slice or substr, the
// realm's own, if a method call at the top of the stack calls it with two
// arguments, numbers, on a string -- or charAt with one -- or nil.
func (r *Runtime) jitSubstrings(sp int, in bytecode.Instr) *Object {
	argc := int(in.A)
	if in.Op != bytecode.OpCallMethod || argc != 1 && argc != 2 {
		return nil
	}
	callee, recv := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() || !recv.IsString() || !r.jitIsSubstring(callee.Object(), argc) {
		return nil
	}
	for _, a := range r.stack[sp-argc : sp] {
		if !a.IsNumber() {
			return nil
		}
	}
	return callee.Object()
}

// jitFromCharCodes is String.fromCharCode, the realm's own, if a method
// call at the top of the stack calls it with one argument, a number, or
// nil.
func (r *Runtime) jitFromCharCodes(sp int, in bytecode.Instr) *Object {
	if in.Op != bytecode.OpCallMethod || in.A != 1 || !r.jitFromCharCode.IsObject() {
		return nil
	}
	callee := r.stack[sp-2]
	if !callee.IsObject() || callee.Object() != r.jitFromCharCode.Object() || !r.stack[sp-1].IsNumber() {
		return nil
	}
	return callee.Object()
}

// jitAllocates is the built-in a construction at the top of the stack
// makes with nothing to run, which native code makes from a pool: `new
// Array()`, the realm's own, or `new Array(n)` of a length native code
// makes (jitArrayRoom), and the room its pool's arrays are given for that
// length; or nil.
func (r *Runtime) jitAllocates(sp int, in bytecode.Instr) (*Object, int) {
	if in.Op != bytecode.OpNew || in.A > 1 {
		return nil, 0
	}
	room := 0
	if in.A == 1 {
		n := r.stack[sp-1]
		if !n.IsNumber() || n.Number() < 0 || n.Number() != math.Trunc(n.Number()) {
			return nil, 0
		}
		if room = jitArrayRoom(n.Number()); room == 0 {
			return nil, 0
		}
	}
	if c := r.stack[sp-1-int(in.A)]; c.IsObject() && c.Object() == r.proto.arrayCtor {
		return c.Object(), room
	}
	return nil, 0
}

// jitArrayRoom is the room a pooled array of `new Array(n)` is given for a
// length n (abi.ObjectPool's Room): the least of the sizes newArrayStorage
// makes an array's elements in its own allocation that n fits, as the
// existing tiers would make it; or 0 past abi.PoolRoomLimit, an n Go
// makes.
func jitArrayRoom(n float64) int {
	for _, room := range [...]int{4, 6, 8, 10, 12, 16} {
		if n <= float64(room) {
			return min(room, abi.PoolRoomLimit)
		}
	}
	return 0
}

// jitNewArrayPool is jitNewPool's for new Array(n): its arrays with room
// for room elements (abi.ObjectPool's Room), each a hole.
func (r *Runtime) jitNewArrayPool(o *Object, room int) *abi.ObjectPool {
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{ctor: o}), Room: uint64(room)}
	r.jitFillPool(pool, o)
	return pool
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
		// it made, a.push, would leave for Go to give it. `new Array(n)`'s
		// has room for pool.Room elements, holes, in its own allocation, as
		// the existing tiers make one (newArrayObject), its length 0 until
		// native code makes it n.
		room := int(pool.Room)
		for i := range n {
			var a *Object
			if room == 0 {
				a = r.newArrayFrom(nil)
			} else {
				a = newArrayObject(r.proto.array, room)
				e := a.elemsOf()
				for j := range e {
					e[j] = elemHole
				}
				a.extOf().elems = e[:0]
			}
			r.ensureShape(a)
			pool.Objects[i] = unsafe.Pointer(a)
		}
		pool.Count, pool.Proto = uint64(n), unsafe.Pointer(r.proto.array)
		return
	}
	if proto, root, props, ok := r.jitConstructLayout(o); ok {
		jitFillObjectPool(pool, proto, root, props)
	}
}

// jitConstructLayout is what an object a construction with o makes starts
// as, as constructWithTarget makes it: o's prototype, its own data
// property, which must hold an object (ok); its constructor's root shape;
// and room for the properties its body adds. A pool's refill looks it up
// once, not for each of its objects.
func (r *Runtime) jitConstructLayout(o *Object) (proto *Object, root *shape, props int, ok bool) {
	p := o.getOwnVisible(atomPrototype)
	fd := o.fn()
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() || fd == nil || fd.closure == nil {
		return nil, nil, 0, false
	}
	root = r.shapes.ctorRoot(fd)
	props = root.roomFor(int(fd.closure.fn.ThisProps))
	return p.value().Object(), root, props, true
}

// jitFillObjectPool fills pool with objects of proto, of shape root, each
// with room for n properties, as newLiteralObject makes them: the layout
// chosen once for the refill, not for each object. Each is an allocation
// of its own, so that keeping one keeps none of the others.
func jitFillObjectPool(pool *abi.ObjectPool, proto *Object, root *shape, n int) {
	switch n {
	case 1:
		jitFillSmallObjects[literalObject1](pool, proto, root)
	case 2:
		jitFillSmallObjects[literalObject2](pool, proto, root)
	case 0, 3:
		// None said, as for a constructor whose body adds nothing: the
		// room newLiteralObject gives it.
		jitFillSmallObjects[literalObject](pool, proto, root)
	case 4:
		jitFillSmallObjects[largeLiteralObject[[4]Value]](pool, proto, root)
	case 5:
		jitFillSmallObjects[largeLiteralObject[[5]Value]](pool, proto, root)
	case 6:
		jitFillSmallObjects[largeLiteralObject[[6]Value]](pool, proto, root)
	case 7, 8:
		jitFillSmallObjects[largeLiteralObject[[8]Value]](pool, proto, root)
	default:
		for i := range int(pool.Size) {
			o := newLiteralObject(proto, ClassObject, n)
			o.shape = root
			pool.Objects[i] = unsafe.Pointer(o)
		}
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(proto)
}

// jitInlineObject is a pointer to one of the layouts newLiteralObject
// makes, an object with room for its properties in its own allocation:
// jitObject gives the object, its properties' room the room.
type jitInlineObject[L any] interface {
	*L
	jitObject() *Object
}

func (lo *literalObject1) jitObject() *Object { lo.vals = lo.inline[:0]; return &lo.Object }
func (lo *literalObject2) jitObject() *Object { lo.vals = lo.inline[:0]; return &lo.Object }
func (lo *literalObject) jitObject() *Object  { lo.vals = lo.inline[:0]; return &lo.Object }
func (lo *largeLiteralObject[A]) jitObject() *Object {
	lo.vals = unsafe.Slice(&lo.inline[0], len(lo.inline))[:0]
	return &lo.Object
}

// jitFillSmallObjects is jitFillObjectPool's for the layout L.
func jitFillSmallObjects[L any, P jitInlineObject[L]](pool *abi.ObjectPool, proto *Object, root *shape) {
	for i := range int(pool.Size) {
		o := P(new(L)).jitObject()
		o.proto, o.shape = proto, root
		o.class, o.flags = ClassObject, objExtensible|objInlineProps
		pool.Objects[i] = unsafe.Pointer(o)
	}
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
	// A closure's (jitFillClosures): its template, the function it is made
	// in and that function's closures' environment.
	tmpl Value
	fn   *bytecode.Function
	env  *Object
}

// jitRefillPool fills pool again, run out, with more objects (ssa's
// GoRefill), as an exit at its site has Go fill it, and reports whether it
// has one now: a construction's function whose prototype is no longer an
// object leaves it empty, and the site to Go.
func (r *Runtime) jitRefillPool(pool *abi.ObjectPool) bool {
	src := (*jitPoolSource)(pool.Source)
	if want := pool.Want; want != 0 && src != nil && src.ctor != nil {
		// A new Array(n) of an n past its arrays' room: filled again,
		// whatever it holds, with arrays of more.
		pool.Want = 0
		room := jitArrayRoom(float64(want))
		if room == 0 || uint64(room) <= pool.Room {
			return false
		}
		pool.Room = uint64(room)
		r.jitFillPool(pool, src.ctor)
		return pool.Count != 0
	}
	if src == nil || pool.Count != 0 {
		return pool.Count != 0
	}
	pool.Size = min(2*max(pool.Size, abi.PoolSize), abi.PoolCapacity)
	if src.ctor != nil {
		r.jitFillPool(pool, src.ctor)
	} else {
		r.jitFillLiteral(&jitLiteral{root: src.root, pool: pool, n: src.n, array: src.array, lit: src.lit, tmpl: src.tmpl, fn: src.fn, env: src.env})
	}
	return pool.Count != 0
}

// jitPoolObject is an object for a construction with o, of the layout
// jitFillPool gives a pool's (jitConstructLayout), or nil if o's prototype is not its own data property holding
// an object.
func (r *Runtime) jitPoolObject(o *Object) *Object {
	proto, root, props, ok := r.jitConstructLayout(o)
	if !ok {
		return nil
	}
	obj := newLiteralObject(proto, ClassObject, props)
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
		} else if p := o.getOwnVisible(atomPrototype); !p.ok() || !p.value().IsObject() || unsafe.Pointer(p.value().Object()) != x.pool.Proto {
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
	// Busy before room is made: the room is made from what the cache
	// holds, e among it, which, dropped, would no longer be its function's
	// -- its link no longer its -- but compiled all the same, its code
	// published in the link, which nothing then held the code for: the
	// collector released it, and native callers called where it had been.
	e.busy++
	defer func() { e.busy-- }()
	if !s.makeRoom(len(fn.Code)*s.perInstr + s.hintRoom(e)) {
		return nil
	}
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
	if len(fb.wanted) == 0 && len(fb.linked) == 0 || r.jit.seeding != 0 {
		return
	}
	r.jit.seeding = jitSeedDepth
	defer func() { r.jit.seeding = 0 }()
	for _, cl := range fb.linked {
		// Called through its link: native from its first compile on, which
		// is now, as for a callee wanted, but with no compile of the
		// caller's. One that does not compile, for good, Go calls: the
		// caller is compiled again for that, a call through a link with no
		// code having Go make its frame (ExitEnter) at every call -- AWFY's
		// Json, its callees' calls so, had left native code 60,000 times.
		if r.jitNativeCallee(cl) == nil && !r.jitLinkable(cl) {
			e.upgradeReopt = true
		}
	}
	if e.upgradeReopts >= jitInlineReoptimizations {
		return
	}
	for _, cl := range fb.wanted {
		if r.jitNativeCallee(cl) != nil {
			e.upgradeReopt = true
		}
	}
}

// setSSA gives e the code the new pipeline compiled for fn from p, which
// native callers call through its link.
func (e *jitEntry) setSSA(s *jitState, fn *bytecode.Function, p *ir.Program, code *jit.SSACode, fb *jitFeedback) {
	e.warming = false
	e.ssa, e.this, e.ssaShapes, e.ssaHolders = code, p.This, fb.shapes, fb.holders
	e.lexThis, e.loops = fb.lexThis, fb.loops
	e.setLoopSlots(fn, fb.loopSlots)
	e.ssaSerial++
	e.ssaProg = p
	e.ssaInlinedAt, e.definesUnfed, e.ssaCells = fb.inlinedAt, fb.definesUnfed, fb.cells
	e.ssaStrings, e.ssaInlined, e.ssaPools = fb.strings, fb.inlined, fb.pools
	s.setLinks(e, fb.links)
	e.ssaKeeps = max(e.ssaKeeps, fb.keeps)
	e.link.setCode(code)
	s.callRefs = max(s.callRefs, uint64(code.CallRefs()))
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
// is never changed once lowered. inlinable is jitInlinable's answer, once
// asked: 1 if the function may be inlined, 2 if not.
//
// bytes is what the lowering holds, its programs' (ir.Program's
// Footprint), which the JIT counts (jitState.lowerBytes); used the epoch
// it was last asked for in, as of the cache's last look (age). The cache
// drops the lowerings of functions it has no entry for to make room
// (makeRoom), the least recently used first, before any code.
type jitLowering struct {
	inlinable uint8
	done      [jitLowerKinds]bool
	progs     [jitLowerKinds]*ir.Program
	errs      [jitLowerKinds]error
	bytes     int
	used      uint64
	// hints are what its code's exits have shown it does that its
	// bytecode does not say (jitcompile.Hints): its programs are lowered
	// with them. treeAdds is how many additions its tree had marked
	// (treeSeen) when they were last taken into hints.
	hints    jitcompile.Hints
	treeAdds int32
	// startOnly is jitcompile.StartOnly's answer for the function, once
	// asked by a call of it: 1 for no, 2 for yes.
	startOnly uint8
}

// jitLowerKind is how a program is lowered: for a function's own code
// (jitcompile.LowerSSA), for a callee inlined (LowerSSAInline), for a
// forwarding constructor inlined (LowerSSAForward), for a function with no
// loop shown to be hot (LowerSSAHot).
type jitLowerKind uint8

const (
	jitLowerSSA jitLowerKind = iota
	jitLowerInline
	jitLowerForward
	jitLowerHot
	jitLowerKinds
)

// lowering is fn's lowering, made if the runtime has none, and found used
// now.
func (s *jitState) lowering(fn *bytecode.Function) *jitLowering {
	key := weak.Make(fn)
	l := s.lowered[key]
	if l == nil {
		if s.lowered == nil {
			s.lowered = map[weak.Pointer[bytecode.Function]]*jitLowering{}
		}
		l = &jitLowering{bytes: jitLoweringFixed}
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
	if t := (*tree)(atomic.LoadPointer(&fn.VMCode)); t != nil {
		// The additions the tree tier has seen add strings, Go's from the
		// first compile (ztree_zzzzseen.go).
		if seen := t.seen.Load(); seen != nil && seen.n.Load() != l.treeAdds {
			l.treeAdds = seen.n.Load()
			before := jitCapBytes(l.hints.StringAdds)
			grown := false
			for i := range seen.adds {
				for w := seen.adds[i].Load(); w != 0; w &= w - 1 {
					pc := int32(i*32 + bits.TrailingZeros32(w))
					if !slices.Contains(l.hints.StringAdds, pc) {
						l.hints.StringAdds, grown = append(l.hints.StringAdds, pc), true
					}
				}
			}
			l.bytes += jitCapBytes(l.hints.StringAdds) - before
			s.lowerBytes += jitCapBytes(l.hints.StringAdds) - before
			if grown {
				s.relower(l)
			}
		}
	}
	if !l.done[kind] {
		switch kind {
		case jitLowerSSA:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAWith(fn, l.hints)
		case jitLowerInline:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAInlineWith(fn, l.hints)
		case jitLowerForward:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAForwardWith(fn, l.hints)
		case jitLowerHot:
			l.progs[kind], l.errs[kind] = jitcompile.LowerSSAHotWith(fn, l.hints)
		}
		l.done[kind] = true
		n := l.progs[kind].Footprint()
		l.bytes += n
		s.lowerBytes += n
	}
	return l.progs[kind], l.errs[kind]
}

// jitStringAddSeen notes an exit at pc, an addition in e's code that met a
// string -- the frame holding its operands as the exit wrote them -- which
// its function's programs are lowered again for (jitcompile.Hints): Go
// makes it (ir.StringAdd), and what it adds is read as it is. The code is
// compiled again at its next invocation, as for an element read learned
// (jitIndexSeen). PDF.js's font parser builds its tokens a character at a
// time, token += c, and read each character as a number, which a string
// left native code at.
func (r *Runtime) jitStringAddSeen(f *frame, e *jitEntry, pc int) {
	p := e.ssaProg
	if p == nil || pc >= len(p.Code) {
		return
	}
	in := p.Code[pc]
	if in.Op != ir.Binary || in.Operator != ir.Add {
		return
	}
	if a, b := r.jitOperand(f, p, in.Left), r.jitOperand(f, p, in.Right); !a.IsString() && !b.IsString() {
		return
	}
	l := r.jit.lowering(f.cl.fn)
	if slices.Contains(l.hints.StringAdds, int32(pc)) {
		return
	}
	before := jitCapBytes(l.hints.StringAdds)
	l.hints.StringAdds = append(l.hints.StringAdds, int32(pc))
	l.bytes += jitCapBytes(l.hints.StringAdds) - before
	r.jit.lowerBytes += jitCapBytes(l.hints.StringAdds) - before
	r.jit.relower(l)
	e.reopt = true
}

// relower drops l's programs, which are lowered again with its hints as
// they are asked for (jitLower): what was compiled from them holds them.
func (s *jitState) relower(l *jitLowering) {
	for k := range l.progs {
		if l.done[k] {
			n := l.progs[k].Footprint()
			l.bytes -= n
			s.lowerBytes -= n
		}
		l.done[k], l.progs[k], l.errs[k] = false, nil, nil
	}
	l.inlinable = 0
}

// jitOperand is the value of an operand of p's instruction in f's frame as
// an exit wrote it: a literal, a local, or one on the frame's stack.
func (r *Runtime) jitOperand(f *frame, p *ir.Program, o ir.Operand) Value {
	switch {
	case o.Slot < 0:
		if o.Literal.Kind == ir.String {
			return Str(emptyString)
		}
		return Undefined
	case o.Slot < p.Locals:
		if o.Slot < len(f.locals) {
			return f.locals[o.Slot]
		}
		return Undefined
	}
	if i := f.base + o.Slot - p.Locals; i >= 0 && i < len(r.stack) {
		return r.stack[i]
	}
	return Undefined
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
		} else if len(fn.Upvalues) == 0 && jitReadsArguments(fn) {
			// arguments[k] read from the call's arguments
			// (ssa.InlineSite's ReadsArguments).
			p, err := r.jitLower(fn, jitLowerForward)
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
// BigInt reports an arithmetic, bitwise or relational operator at pc of
// the root that met BigInts: at a guard that failed (jitBigIntSeen), or in
// the tree tier (treeBigSeen), as V8's lower tier collects BigInt feedback
// for its optimizing compiler.
func (fb *jitFeedback) BigInt(pc int) bool {
	if fb.e != nil && slices.Contains(fb.e.bigints, int32(pc)) {
		return true
	}
	big, _ := jitTreeBig(fb.fn, pc)
	return big
}

// BigIntWide is ssa.BigIntFeedback's: whether the operator at pc met
// BigInts from 0 to 2^64-1, some past the int64 range, which native code
// makes as Uint64s -- at a guard that failed, or in the tree tier.
func (fb *jitFeedback) BigIntWide(pc int) bool {
	if fb.e != nil && slices.Contains(fb.e.bigwide, int32(pc)) {
		return true
	}
	return jitTreeWide(fb.fn, pc)
}

// BigIntFailed is ssa.BigIntFeedback's: whether a guard at pc failed in
// code compiled before (jitDeoptimized).
func (fb *jitFeedback) BigIntFailed(pc int) bool {
	return fb.e != nil && slices.Contains(fb.e.failed, int32(pc))
}

// jitTreeBig reports whether the tree tier's operator at pc of fn met
// BigInts, and whether one on the heap (treeBigSeen).
func jitTreeBig(fn *bytecode.Function, pc int) (big, heap bool) {
	s := jitTreeBigs(fn, pc)
	if s == nil {
		return false, false
	}
	bit := uint32(1) << (pc & 31)
	heap = s.heap[pc>>5].Load()&bit != 0
	return heap || (s.bigs[pc>>5].Load()|s.wide[pc>>5].Load())&bit != 0, heap
}

// jitTreeWide reports whether the tree tier's operator at pc of fn met
// BigInts from 0 to 2^64-1, some past the int64 range (treeBigSeen).
func jitTreeWide(fn *bytecode.Function, pc int) bool {
	s := jitTreeBigs(fn, pc)
	return s != nil && s.wide[pc>>5].Load()&(uint32(1)<<(pc&31)) != 0
}

// jitTreeRecv reports whether the tree tier's method read at pc of fn met
// a BigInt receiver (treeBigSeen).
func jitTreeRecv(fn *bytecode.Function, pc int) bool {
	s := jitTreeBigs(fn, pc)
	return s != nil && s.recvs[pc>>5].Load()&(uint32(1)<<(pc&31)) != 0
}

// jitTreeBigs is the tree tier's marks of fn's BigInt operators, if it
// has any there are for pc.
func jitTreeBigs(fn *bytecode.Function, pc int) *treeBigSeen {
	t := (*tree)(atomic.LoadPointer(&fn.VMCode))
	if t == nil || t == noTree || pc < 0 {
		return nil
	}
	s := t.bigSeen.Load()
	if s == nil || pc>>5 >= len(s.bigs) {
		return nil
	}
	return s
}

// jitBigIntSeen notes that the operator at site, of e's program, whose
// guard failed or which is generic, met BigInts, as V8's feedback learns
// BigInt operands, and reports whether the site speculates on them: an
// operator of BigInts in the int64 range, its result one too, or of
// BigInts from 0 to 2^64-1 (jitEntry's bigwide), which the code compiled
// again makes natively (ssa.BigIntFeedback), not as Go's. One whose
// speculation fails then -- an operand of another type, a result outside
// the range, a BigInt on the heap -- is generic (jitDeoptimized), but for
// one made as an int64's that met BigInts from 0 to 2^64-1, learned once
// as those; so is one that met other BigInts, whose operation Go makes
// from native code (ssa.GoArith), as V8's code calls its BigInt builtins.
func (r *Runtime) jitBigIntSeen(f *frame, e *jitEntry, site int) bool {
	p := e.ssaProg
	if site < 0 || p == nil || site >= len(p.Code) {
		return false
	}
	in := p.Code[site]
	generic := slices.Contains(e.failed, int32(site))
	native := false
	switch {
	case in.Op == ir.Binary && (in.Operator == ir.Add || in.Operator == ir.Sub || in.Operator == ir.Mul),
		(in.Op == ir.Binary || in.Op == ir.Branch) && (in.Operator == ir.Lt || in.Operator == ir.Le || in.Operator == ir.Gt || in.Operator == ir.Ge):
		// Generic, an addition is GoAdd's and a comparison GoCompare's,
		// whatever the site learns.
		native = !generic
		if generic && in.Operator != ir.Sub && in.Operator != ir.Mul {
			return false
		}
	case in.Op == ir.Binary && (in.Operator == ir.Div || in.Operator == ir.Mod || in.Operator == ir.Pow || in.Operator == ir.BitAnd || in.Operator == ir.BitOr ||
		in.Operator == ir.BitXor || in.Operator == ir.Shl || in.Operator == ir.Shr),
		in.Op == ir.Unary && (in.Operator == ir.Neg || in.Operator == ir.BitNot):
		native = !generic
	default:
		return false
	}
	right := in.Right
	if in.Op == ir.Unary {
		// -x and ~x: of x alone.
		right = in.Left
	}
	if in.Left.Slot < 0 || right.Slot < 0 {
		return false
	}
	a, b := r.jitOperand(f, p, in.Left), r.jitOperand(f, p, right)
	if !a.IsBigInt() || !b.IsBigInt() {
		return false
	}
	learned := slices.Contains(e.bigints, int32(site))
	if big, _ := jitTreeBig(f.cl.fn, site); learned || big {
		// Its speculation, learned or the tree tier's, failed: made as one
		// of BigInts from 0 to 2^64-1 where they are, once.
		if !native || slices.Contains(e.bigwide, int32(site)) || jitTreeWide(f.cl.fn, site) || !jitBigUnsigned(a) || !jitBigUnsigned(b) {
			return false
		}
		e.bigwide = append(e.bigwide, int32(site))
		e.reopt = true
		return true
	}
	e.bigints = append(e.bigints, int32(site))
	e.reopt = true
	if !native {
		return false
	}
	if !a.isInt64Big() || !b.isInt64Big() {
		if jitBigUnsigned(a) && jitBigUnsigned(b) {
			e.bigwide = append(e.bigwide, int32(site))
			return true
		}
		return false
	}
	x, y := a.int64Big(), b.int64Big()
	switch in.Operator {
	case ir.Neg:
		return x != math.MinInt64
	case ir.Add:
		if s := x + y; (x^s)&(y^s) < 0 {
			return false
		}
	case ir.Sub:
		if d := x - y; (x^y)&(x^d) < 0 {
			return false
		}
	case ir.Mul:
		hi, lo := bits.Mul64(uint64(x), uint64(y))
		// The signed product's high word: the unsigned one's, less each
		// operand where the other is negative.
		if x < 0 {
			hi -= uint64(y)
		}
		if y < 0 {
			hi -= uint64(x)
		}
		if hi != uint64(int64(lo)>>63) {
			return false
		}
	case ir.Div:
		if y == 0 || x == math.MinInt64 && y == -1 {
			return false
		}
	case ir.Mod:
		if y == 0 {
			return false
		}
	case ir.Pow:
		_, ok := shortArith(bytecode.OpPow, a, b)
		return ok
	}
	return true
}

// BigIntSole is ssa.BigIntRecycleFeedback's: the locals of fn that hold
// their values alone (soleLocals), and whether its code may give BigInts'
// blocks back at all -- not a generator's or an async function's, whose
// frames suspend, as the tree tier's freshPC has it -- on a 64-bit
// machine, whose BigInts have room in their headers for their room.
func (fb *jitFeedback) BigIntSole() (uint64, bool) {
	fn := fb.fn
	if bits.UintSize != 64 || fn.Generator || fn.Async {
		return 0, false
	}
	return soleLocals(fn), true
}

// jitBigRelease gives back what of a, b and the third operand, old, the
// call of Go that made v of them may give back (ssa.CallSite's Release),
// as the tree tier's bigRelease does for its operators.
func (r *Runtime) jitBigRelease(ctx *abi.Context, v, a, b Value) {
	rel := uint8(ctx.GoOp >> abi.GoOpKindShift)
	if rel == 0 {
		return
	}
	flags := 0
	if rel&ssa.ReleaseLeft != 0 {
		flags |= treeFreshA
	}
	if rel&ssa.ReleaseRight != 0 {
		flags |= treeFreshB
	}
	old := Undefined
	if rel&ssa.ReleaseOld != 0 {
		flags |= treeOwnRes
		old = jitGoArg(ctx, 2)
	}
	r.bigRelease(v, a, b, old, flags)
}

// jitBigUnsigned reports whether v is a BigInt from 0 to 2^64-1, which is
// held in its value.
func jitBigUnsigned(v Value) bool {
	if !v.isShortBig() {
		return false
	}
	neg, _ := v.shortBigParts()
	return !neg
}

// jitBigIntExit is jitBigIntSeen at a host exit of e's code at site, for
// the operator there with no native form for BigInts, generic, that met
// two: what it asks first, at any exit, is the instruction's.
func (r *Runtime) jitBigIntExit(f *frame, e *jitEntry, site int) {
	p := e.ssaProg
	if p == nil || site >= len(p.Code) {
		return
	}
	in := p.Code[site]
	right := in.Right
	switch {
	case in.Op == ir.Binary:
		switch in.Operator {
		case ir.Sub, ir.Mul, ir.Div, ir.Mod, ir.Pow, ir.BitAnd, ir.BitOr, ir.BitXor, ir.Shl, ir.Shr:
		default:
			return
		}
	case in.Op == ir.Unary && (in.Operator == ir.Neg || in.Operator == ir.BitNot):
		right = in.Left
	default:
		return
	}
	if in.Left.Slot < 0 || right.Slot < 0 || !r.jitOperand(f, p, in.Left).IsBigInt() || !r.jitOperand(f, p, right).IsBigInt() ||
		!slices.Contains(e.failed, int32(site)) {
		return
	}
	r.jitBigIntSeen(f, e, site)
}

func (fb *jitFeedback) Generic(pc int) bool {
	if fb.e != nil && slices.Contains(fb.e.failed, int32(pc)) {
		return true
	}
	// Arithmetic the tree tier saw meet or make a BigInt on the heap, which
	// Go makes from native code (ssa.GoArith).
	_, heap := jitTreeBig(fb.fn, pc)
	return heap
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
	if i < 0 || int(i) >= len(env.vals) || env.shape.keys[i].key != name {
		i = env.findOwn(name)
	}
	if i < 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i}
	if check {
		// A strict assignment's check: data, which the cell's guard
		// finds is there.
		if env.shape.keys[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		return site, true
	}
	if write {
		// An assignment: a writable data property, which the code
		// checks it still is.
		if p := env.shape.keys[i]; p.isAccessor() || p.flags&propWritable == 0 {
			return ssa.GlobalSite{}, false
		}
		return site, true
	}
	site.Typed = !env.shape.keys[i].isAccessor() && jitTypedArray(env.vals[i])
	if p := env.shape.keys[i]; !p.isAccessor() && p.flags&(propWritable|propConfigurable) == 0 {
		switch {
		case env.vals[i].IsUndefined():
			site.Fixed, site.Constant = true, ir.Value{Kind: ir.Undefined}
		case env.vals[i].IsNumber():
			site.Fixed, site.Constant = true, ir.Float(env.vals[i].Number())
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
	if i < 0 || int(i) >= len(env.vals) || env.shape.keys[i].key != name {
		i = env.findOwn(name)
	}
	holder := env
	shadowable := false
	switch {
	case i >= 0 && env.shape.keys[i].isAccessor():
		// An import, which only a read reads: an assignment to one throws.
		if write || check {
			return ssa.GlobalSite{}, false
		}
		h, key, ok := fb.r.jitImportedBinding(env, name)
		if !ok {
			return ssa.GlobalSite{}, false
		}
		if i = h.findOwn(key); i < 0 || h.shape.keys[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		holder, name = h, key
	case i < 0:
		g := env.proto
		if write || check || g == nil || fb.r.lexShadows(name) || proxyOf(g) != nil {
			return ssa.GlobalSite{}, false
		}
		if i = g.findOwn(name); i < 0 || g.shape.keys[i].isAccessor() {
			return ssa.GlobalSite{}, false
		}
		holder, shadowable = g, true
	}
	p := holder.propAt(i)
	if write && p.flags&propWritable == 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i, Holder: uintptr(unsafe.Pointer(holder)), Shadowable: shadowable}
	site.Typed = !write && !check && jitTypedArray(p.value)
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

// jitTypedArray reports whether v is a typed array (ssa.GlobalSite's
// Typed).
func jitTypedArray(v Value) bool {
	return v.IsObject() && v.Object().class == ClassTypedArray
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
	p := lex.propAt(i)
	if write && p.flags&propWritable == 0 {
		return ssa.GlobalSite{}, false
	}
	site := ssa.GlobalSite{Key: uint32(name), Index: i, Lexical: true, Typed: !write && jitTypedArray(p.value)}
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
	big := (slices.Contains(e.bigSites, uint32(pc)) || jitTreeRecv(fb.fn, pc)) && !slices.Contains(e.strSites, uint32(pc))
	if big {
		// A read that met BigInt receivers and no string: off
		// BigInt.prototype, which the site names (Holders).
		proto = fb.r.proto.bigint
	}
	if key.IsIndex() || key == atomLength || proto == nil {
		return ssa.PropertySite{}, false
	}
	if !big && !slices.Contains(e.strSites, uint32(pc)) &&
		(c.fills != 0 || fb.r.proto.object == nil || fb.r.proto.object.findOwn(key) >= 0) {
		return ssa.PropertySite{}, false
	}
	sh := fb.r.ensureProtoShape(proto)
	i := proto.findOwn(key)
	if sh == nil || sh == noShape || i < 0 || proto.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return ssa.PropertySite{}, false
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(sh))
	site := ssa.PropertySite{Key: uint32(key), Shape: uintptr(unsafe.Pointer(sh)), Index: i}
	if big {
		k.holders = append(k.holders, proto)
		site.Holders[0] = ssa.Holder{Object: uintptr(unsafe.Pointer(proto)), Shape: site.Shape}
	}
	return site, true
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
		if in := fb.fn.Code[pc]; in.Op != bytecode.OpSetProp && int(in.B) < len(fb.cl.ic) && (fb.cl.ic[in.B].shape == nil || fb.cl.ic[in.B].shape == noShape || fb.cl.ic[in.B].cell != nil) {
			if cases := fb.deepCases(pc); len(cases) != 0 {
				k := cases[0]
				site.Shape, site.Index, site.Holders, site.More, site.Cases = k.Shape, k.Index, k.Holders, k.More, cases[1:]
				deep = true
			}
		}
	}
	if ok && fb.e != nil && fb.root == nil {
		if fb.fn.Code[pc].Op == bytecode.OpSetProp {
			site.Cases, site.Adds = fb.writeCases(pc, site)
		} else if site.Shape != 0 && !deep {
			site.Cases = fb.cases(pc, site.Shape)
		}
	} else if ok && fb.root != nil && fb.fwd == nil && site.Shape != 0 && !deep {
		// An inlined callee's site, whose code noted no shapes of its own
		// (jitSeenFeedback): those its cache met before the one it holds.
		site.Cases = fb.seenCases(pc, site)
	}
	if ok && fb.root != nil && fb.fwd == nil && fb.fn.Code[pc].Op == bytecode.OpSetProp {
		// And its additions so.
		site.Adds = fb.seenAdds(pc, site)
	}
	if ok {
		// A receiver of a shape the site's cases do not have deoptimizes,
		// its cache learning the shape as the interpreter makes the
		// operation, and so does any at a site whose cache knows none
		// (ssa.PropertySite): as V8's map checks deoptimize, and its code
		// deoptimizes for insufficient feedback. But where the cache is
		// megamorphic the code looks further, as V8's megamorphic loads and
		// stores do, and leaves for Go only what it does not find.
		site.Megamorphic = fb.megamorphic(pc, site)
	}
	if in := fb.fn.Code[pc]; ok && !site.Megamorphic && in.Op == bytecode.OpGetProp && int(in.B) < len(fb.cl.ic) && fb.cl.ic[in.B].fills == 0 {
		// A read whose cache is empty, which the interpreter may have run
		// all the same, reading an object's small table without filling it
		// (plainOwn), as only the tree tier fills it (ownScan): it searches
		// a receiver's own small table, and deoptimizes where that does not
		// have the property, its cache learning the shape then -- rather
		// than at every receiver, and again at each compile, its code
		// compiled again only on the existing tiers' budget (jitTierUp).
		site.Scan = true
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
			if a := fb.deepAdd(d); a != nil {
				adds = append(adds, a)
			}
		}
	}
	return cases, adds
}

// deepAdd is d, an addition along a prototype chain deeper than a cache
// holds, as ssa.PropertyAdd has it, the code keeping its shapes and
// prototypes; or nil where its next shape wants its table's index built,
// as fb.add leaves to Go.
func (fb *jitFeedback) deepAdd(d jitDeepAdd) *ssa.PropertyAdd {
	if d.next.index == nil && int(d.next.n) > linearScanLimit {
		return nil
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
	return a
}

// seenCases are the cases of the property site at pc, as its cache met
// them before the shape it holds now, compiled for site (propCache.seen):
// a read's found on the receiver or on a prototype a cache holds, a
// write's own properties; up to jitPropertyCases. An inlined callee's
// site is compiled for them, as V8 compiles a site inlined for the maps
// its inline cache met: at any other shape it deoptimizes.
func (fb *jitFeedback) seenCases(pc int, site ssa.PropertySite) []ssa.PropertyCase {
	in := fb.fn.Code[pc]
	if int(in.B) >= len(fb.cl.ic) || fb.cl.ic[in.B].seen == nil || site.Holders[0].Object != 0 && in.Op == bytecode.OpSetProp {
		return nil
	}
	c := &fb.cl.ic[in.B]
	write := in.Op == bytecode.OpSetProp
	var cases []ssa.PropertyCase
	k := fb.keep()
	for _, x := range c.seen.entries[:c.seen.n] {
		if len(cases)+1 >= jitPropertyCases {
			break
		}
		from := uintptr(unsafe.Pointer(x.shape))
		if x.shape == nil || x.shape == noShape || x.getter || x.idx < 0 || x.next != nil || from == site.Shape ||
			c.shape != nil && jitSuperseded(x.shape, c.shape) || write && x.p1 != nil {
			continue
		}
		if slices.ContainsFunc(cases, func(p ssa.PropertyCase) bool { return p.Shape == from }) {
			continue
		}
		pcase := ssa.PropertyCase{Shape: from, Index: x.idx}
		k.shapes = append(k.shapes, remember(x.shape))
		for i, h := range [2]struct {
			p *Object
			s *shape
		}{{x.p1, x.s1}, {x.p2, x.s2}} {
			if h.p == nil {
				break
			}
			pcase.Holders[i] = ssa.Holder{Object: uintptr(unsafe.Pointer(h.p)), Shape: uintptr(unsafe.Pointer(h.s))}
			k.shapes, k.holders = append(k.shapes, remember(h.s)), append(k.holders, h.p)
		}
		cases = append(cases, pcase)
	}
	return cases
}

// seenAdds are the additions the property write at pc, an inlined
// callee's, made as its cache met them besides the one site is compiled for
// (propCache.seen, poly) -- and those along prototype chains deeper than it
// holds (propSeen's deep) -- up to jitPropertyCases: an initializer's that a
// class's subclasses share, as Box2D's static ones are, each class's
// objects of a shape of their own, whose additions had each been an exit.
func (fb *jitFeedback) seenAdds(pc int, site ssa.PropertySite) []*ssa.PropertyAdd {
	in := fb.fn.Code[pc]
	if int(in.B) >= len(fb.cl.ic) {
		return nil
	}
	c := &fb.cl.ic[in.B]
	var adds []*ssa.PropertyAdd
	note := func(x *propCache) {
		if len(adds)+1 >= jitPropertyCases || x.next == nil || x.shape == nil || x.shape == noShape || x.getter {
			return
		}
		from := uintptr(unsafe.Pointer(x.shape))
		if from == site.Shape || site.Add != nil && from == site.Add.From ||
			slices.ContainsFunc(adds, func(a *ssa.PropertyAdd) bool { return a.From == from }) {
			return
		}
		if a := fb.add(x); a != nil {
			adds = append(adds, a)
		}
	}
	if c.seen != nil {
		for i := range c.seen.n {
			note(&c.seen.entries[i])
		}
	}
	if c.poly != nil {
		for i := range c.poly.n {
			note(&c.poly.entries[i])
		}
	}
	if c.seen != nil && int(in.A) < len(fb.cl.names) {
		// The additions along chains deeper than a cache holds -- a
		// subclass's object's, two prototypes below the class whose
		// initializer adds the property -- as the chain is now.
		for _, d := range c.seen.deep {
			from := uintptr(unsafe.Pointer(d.from))
			if len(adds)+1 >= jitPropertyCases || from == site.Shape || site.Add != nil && from == site.Add.From ||
				slices.ContainsFunc(adds, func(a *ssa.PropertyAdd) bool { return a.From == from }) {
				continue
			}
			if da, ok := fb.r.jitDeepAddFrom(d.from, d.next, d.proto, fb.cl.names[in.A]); ok {
				if a := fb.deepAdd(da); a != nil {
					adds = append(adds, a)
				}
			}
		}
	}
	return adds
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
	if i < 0 || i >= sh.n || int(i) >= len(recv.vals) || recv.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
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
	if c.shape == nil || c.shape == noShape || c.shape == dictShape || c.cell != nil || c.getter || c.next != nil || c.idx < 0 {
		// Nothing native code is compiled for: an entry for dictionary
		// objects (dictShape) answers by a lookup in the object's table,
		// and one of a chain's cell (protoCell) by the cell.
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
// captured bindings, the receiver if the code reads it (this, its
// program's This: from the context, where Go put it), then its operands.
func (r *Runtime) jitSlot(f *frame, this bool, i int) *Value {
	n, u := f.cl.fn.LocalCount, len(f.cl.upvalues)
	switch {
	case i < n:
		return &f.locals[i]
	case i < n+u:
		return f.cl.upvalues[i-n].slot
	case this && i == n+u:
		return (*Value)(unsafe.Pointer(&r.jit.ssaCtx.This))
	}
	if this {
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
	if !m.ok() || m.flags()&^propDefault != 0 || !m.value().StrictEquals(r.jitCharCodeAt) {
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
// references it moved, and the primitives it put where a reference was,
// numbered as the code that left has them (this, jitSlot's). The slots
// records read hold their values from entry until the first write, so
// every one is read first; one record, the usual case, reads its value and
// writes it.
func (r *Runtime) jitApplyRecords(f *frame, this bool, ctx *abi.Context) {
	n := int(ctx.Records)
	if n == 0 {
		return
	}
	r.jit.ssaRecords += uint64(n)
	const flags = abi.RecordScalar | abi.RecordMaybe | abi.RecordDirect
	if n == 1 {
		rec := &ctx.Record[0]
		*r.jitSlot(f, this, int(rec.Slot&^flags)) = r.jitRecordValue(f, this, ctx, 0)
		return
	}
	var buf [8]Value
	src := buf[:0]
	for i := range ctx.Record[:n] {
		src = append(src, r.jitRecordValue(f, this, ctx, i))
	}
	for i, rec := range ctx.Record[:n] {
		*r.jitSlot(f, this, int(rec.Slot&^flags)) = src[i]
	}
}

// jitRecordValue is the value a record writes, read from the frame as the
// exit left it.
func (r *Runtime) jitRecordValue(f *frame, this bool, ctx *abi.Context, i int) Value {
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
		return *r.jitSlot(f, this, int(rec.Arg))
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
	// runGoCalls counts those it made in runs begun at a loop that Go
	// makes for good, and runWork the work it did natively before each
	// (jitRunGoProfit); osrStretches the
	// stretches begun at a loop by on-stack replacement, and osrWork the
	// work they did (jitSSAProfit) -- each since Go last began entering the
	// code again (jitRetrySlow).
	runGoCalls, runWork   uint64
	osrStretches, osrWork uint64
	// judged is how many stretches, calls of Go included, there had been
	// when jitSSAProfit last judged the code.
	judged uint64
	// workBase and stretchBase are work and the stretches when Go last
	// began entering the code again (jitRetrySlow): it is judged by what
	// it has done since.
	workBase, stretchBase uint64
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
//
// The stretches of a run begun at a loop by on-stack replacement (osr)
// are judged apart besides. Code judged slow is entered so at a loop that
// has run a full budget in the existing tiers (tryJITAt), the judgement
// being of Go's entries at the start, whose stretches are a short call's.
// But where the run's stretches do too little too -- the loop leaves at
// each turn, for a using declaration's resources, say -- each entry ran a
// turn natively and left the frame for the interpreter until the next
// budget, a loop run in the bytecode interpreter: Go does not enter such
// code at a loop either (loopSlow), and the tree runs the loop.
func (r *Runtime) jitSSAProfit(e *jitEntry, f *frame, ctx *abi.Context, start, edges int, osr bool) {
	st := &e.ssaStats
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
	if osr {
		st.osrStretches++
		st.osrWork += work
		if st.osrStretches%jitSSAProbe == 0 && st.osrWork < st.osrStretches*jitSSAMinWork {
			e.entrySlow, e.loopSlow = true, true
		}
	}
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
	if st.work-st.workBase < (stretches-st.stretchBase)*jitSSAMinWork {
		// Go stops entering it; native callers, which pay neither, go on
		// calling it (NativeCalls).
		e.entrySlow = true
	}
}

// jitRetrySlow counts a call the existing tiers made of code Go does not
// enter (entrySlow), and has Go enter it again after jitNativeRetry calls,
// doubled each time it was found not to pay, up to jitNativeBackoffs times,
// judging it by what it does from then on (jitSSAProfit): code judged on its
// first stretches -- its calls not yet decided, made by Go; its sites
// without feedback -- may pay once it has learned, as a native callee does
// (jitRetryNative). Judged once, for good, AWFY's Richards' runTask had
// every call made by Go after its one call's target was learned. Code
// compiled for native callers alone (ssaCallee) Go does not enter.
func jitRetrySlow(e *jitEntry) {
	if e.ssaCallee || e.ssa == nil || e.slowCalls-e.slowSince < jitNativeRetry<<e.slowBackoff {
		return
	}
	st := &e.ssaStats
	e.entrySlow, e.loopSlow = false, false
	e.slowSince, e.slowBackoff = e.slowCalls, min(e.slowBackoff+1, jitNativeBackoffs)
	st.judged = st.entries + st.goCalls
	st.workBase, st.stretchBase = st.work, st.judged
	st.runGoCalls, st.runWork, st.osrStretches, st.osrWork = 0, 0, 0, 0
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

// jitGoOn has the interpreter run f from pc, sp operands high, where e's
// native code has no entry: after an exit that Go ran the instruction at,
// or after a native call Go finished, as V8's code is entered only at a
// function's start and by on-stack replacement at a loop. The interpreter
// runs only to the next backward jump of the frame's own, the
// interpreter's check made due there, where Go goes on natively, at the
// loop's header, in this run (again, at goOnPC with goOnDepth operands;
// tryJITLoop): an iteration that left as V8's code deoptimizes leaves the
// rest of that iteration to the interpreter, not the rest of the loop.
// Code that does not pay (entrySlow), and code invalidated (jitInvalidate),
// leaves the interpreter the rest of the invocation, as a deoptimized
// invocation is run, entered again at a loop if the existing tiers' budget
// has it compiled again meanwhile (tryJITAt). run says whether f is
// the frame of the innermost run of e's code Go is inside, which is taken
// out of its runs meanwhile (jitLeaveCode); a frame native code called
// natively, which Go finishes (jitFinishExit), is in none -- the caller's
// run, which may be e's own, goes on in its code after the call.
//
// Code was compiled with entries in the middle of loops' bodies: after
// every guard that may leave for Go -- Typescript's 3,148-instruction
// function had 1,666, each loading its live slots and merging with phis,
// two thirds of its 145,000 values, its compile 200 ms -- and then where
// Go had gone on: Gameboy's instruction dispatch, with 46, ran natively
// 40% longer, each a join where what the loop had proved held no longer.
func (r *Runtime) jitGoOn(f *frame, e *jitEntry, pc, sp int, run bool) (v Value, err error, done, again bool) {
	f.pc = uint32(pc)
	n := -1
	if run {
		// Out of its code, the run goes on natively at an entry of the
		// code as it is when the interpreter offers one: a run of it that a
		// back edge with no entry in this one began meanwhile, which
		// deoptimized, is compiled again for it.
		n = r.jitLeaveCode(e)
	}
	defer r.jitBackInCode(e, n)
	if !e.native() {
		v, err, done = r.jitInterpret(f, sp, nil)
		return v, err, done, false
	}
	s := r.jit
	s.interpreted++
	defer func(f *frame) { s.goOn = f }(s.goOn)
	s.goOn = f
	// The interpreter's check, due at the next back edge, which is no
	// budget the existing tiers spent (forcedCheck).
	r.backEdges, s.forcedCheck = 1, true
	v, err = r.executeAt(f, sp, nil)
	if err == errGoOnNatively {
		s.wentOn++
		return Undefined, nil, false, true
	}
	return v, err, true, false
}

// errGoOnNatively ends the interpreter's run of a frame Go goes on in
// natively, at a loop's header (jitGoOn).
var errGoOnNatively = errors.New("jit: go on natively")

// runSSA runs a function compiled by the new pipeline from pc, where the
// frame has depth operands, until it returns or leaves native code for the
// rest of the invocation.
func (r *Runtime) runSSA(f *frame, e *jitEntry, pc, depth int) (Value, error, bool) {
	if e.invalid || !e.ssa.HasEntry(pc) {
		// Invalidated (jitInvalidate): the existing tiers run the frame.
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
	if !e.owned() {
		// The cache dropped it to make room since it was found -- a
		// compile Go made meanwhile -- before its run kept it (ssaRuns):
		// its code is not its function's now, and what it calls through
		// may be gone. The existing tiers run the frame.
		return Undefined, nil, false
	}
	s := r.jit
	top := s.ctxTop
	s.ctxTop = idx + 1
	e.ssaRuns++
	s.runEntries = append(s.runEntries, e)
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
	s.runEntries[len(s.runEntries)-1] = nil
	s.runEntries = s.runEntries[:len(s.runEntries)-1]
	s.ctxTop = top
	if idx < len(s.goRuns) {
		s.goRuns[idx] = jitGoRun{}
	}
	c := s.ssaCtxs[idx]
	clear(c.Keep[:e.ssaKeeps])
	c.This, c.Upvalues, c.Instance = abi.Slot{}, nil, abi.Slot{}
	// The frames made for the native levels of this run's calls of Go
	// (jitLevels), which only its calls share: their closures and
	// receivers had kept a benchmark's graph alive once it ended.
	if lv := &s.levels; len(lv.at) != 0 && idx <= lv.root {
		clear(lv.at)
		lv.at, lv.serial, lv.n = lv.at[:0], 0, 0
	}
	// Past RecordHigh, the pointer words native calls wrote themselves,
	// which Go did not expand (abi.CallRecord's Ref).
	if n := max(c.RecordHigh, s.callRefs); n != 0 {
		clear(c.RecordRef[:n])
		c.RecordHigh = 0
	}
	for i := idx + 1; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
		if c.Closure == nil {
			break
		}
		if n := max(c.RecordHigh, s.callRefs); n != 0 {
			clear(c.RecordRef[:n])
			c.RecordHigh = 0
		}
		clear(c.Keep[:s.ssaKeeps])
		c.Closure, c.Upvalues, c.Cells = nil, nil, nil
		c.This, c.RetValue, c.Instance = abi.Slot{}, abi.Slot{}, abi.Slot{}
	}
}

// runSSALoop is runSSAIn's work, between its run's start and end.
func (r *Runtime) runSSALoop(f *frame, e *jitEntry, pc, depth, idx int, resume uintptr) (Value, error, bool) {
	s := r.jit
	ctx := s.ssaCtxs[idx]
	// Begun at a loop by on-stack replacement, its stretches are judged
	// as such (jitSSAProfit), and its calls of Go as they are made
	// (jitRunGoProfit).
	osr := s.osrRun
	s.osrRun = false
	// resume, when not 0, is where native code goes on after a native
	// call whose callee Go finished, in place of an entry.
	for {
		// What Go has run since the last entry may have changed the frame, how
		// deep calls are, or the scope.
		s.ssaCtx = ctx
		ctx.ReturnTo, ctx.Live, ctx.TailReturn = 0, 0, 0
		r.jitShareContexts(idx, f)
		ctx.Locals = unsafe.Pointer(unsafe.SliceData(f.locals))
		ctx.Stack = unsafe.Pointer(&r.stack[f.base])
		ctx.Upvalues = unsafe.Pointer(unsafe.SliceData(f.cl.upvalues))
		if resume == 0 {
			// Code whose captured locals are in cells of their own makes
			// them at its entry (ssa.GoCells); resumed after a native call,
			// it goes on with those it made, and in the loop it was in
			// (abi.Context's Loop).
			ctx.Cells, ctx.Loop = nil, 0
		}
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
		if e.this || e.lexThis || e.ssaProg != nil && e.ssaProg.Cells {
			// Read by the code, or by the arrows it makes from their pools
			// (mir's closureTake), as a native call's context has it.
			this, bound := f.thisValue()
			if !bound {
				this = uninitialized
			}
			*(*Value)(unsafe.Pointer(&ctx.This)) = this
		}
		if f.thisRef != nil && f.cl.fn.Kind == bytecode.KindDerivedConstructor && f.callee != nil && f.newTarget.IsObject() {
			// A derived construction's: what its super() natively needs,
			// as a native construction's context has it (ssa.CallSite's
			// Super) -- its instance, its new.target, its function.
			*(*Value)(unsafe.Pointer(&ctx.Instance)) = f.thisRef.value
			ctx.NewTarget, ctx.Function = uint64(uintptr(unsafe.Pointer(f.newTarget.Object()))), uint64(uintptr(unsafe.Pointer(f.callee)))
		}
		s.entries++
		s.ssaEntries++
		e.ssaStats.entries++
		start, edges := pc, r.backEdges-s.pollCredit
		r.jitGoRunAt(idx, f, e, pc, edges, osr)
		// The code the stretch runs: an eager deoptimization invalidates
		// it if it is still the function's.
		ran := e.ssa
		var err error
		top, raised := r.stackTop, -1
		if need := f.base + e.loopSlots; e.loopSlots != 0 && need > top && need <= len(r.stack) {
			// The slots of its loops of built-ins' calls, past its operands:
			// the frames of the callees they call start past them.
			r.stackTop, r.stackHigh, raised = need, max(r.stackHigh, need), need
		}
		if resume != 0 {
			err, resume = e.ssa.Resume(resume, s.ssaCtxs[idx+1]), 0
		} else {
			err = e.ssa.Run(pc, ctx)
		}
		if r.stackTop == raised {
			// Back where it was raised, no callee of the code's still
			// live: the frame's own top again. A callee that left is
			// finished from its frame past it (jitUnwindNative).
			r.stackTop = top
		}
		if ctx.Cells != nil {
			// Its cells, as the frame has them, for whatever runs it now.
			jitReopenRunCells(f, ctx)
		}
		if f.thisRef != nil && !f.thisRef.init {
			if this := *(*Value)(unsafe.Pointer(&ctx.This)); this.IsObject() && f.cl.fn.Kind == bytecode.KindDerivedConstructor {
				// A super() native code made bound the context's this,
				// which no exit writes: the frame's binding is it.
				f.this, f.thisRef.value, f.thisRef.init = this, this, true
			}
		}
		if len(s.builders) != 0 {
			// Whatever native code left, Go may now see its strings in a
			// frame: none grows in place from here.
			r.jitFreezeBuilders()
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
			// A level making a native call, its state as its code
			// described it (abi.Context's CallState), which what follows
			// reads.
			jit.ExpandCall(s.ssaCtxs[k])
			k++
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
			r.jitSSAProfit(e, f, ctx, start, edges, osr)
		}
		if c := s.ssaCtxs[idx+1]; c.Live == abi.LiveCall {
			// A native call's callee left native code: Go finishes it
			// (jitUnwindNative), then this code goes on natively where the
			// callee would have returned to, its state where it left it --
			// as V8's lazy deoptimization leaves a caller's optimized frame
			// alone, the collector marking or not: the call's result, which
			// Go writes with its barrier (RetValue), is the only pointer
			// native code stores before it tests the collector's flag
			// again, into a cell whose old value nothing native reads (mir's
			// resumeCall). Not into code compiled again or dropped meanwhile
			// (Size 0, closed) -- what Go ran may have run the function
			// again, a recursive one, and compiled it again there, as V8
			// installs new code while older frames are in the old
			// (jitReoptimize) -- nor after a throw: then as below, from the
			// frame the call wrote, as the code that left describes it
			// (this and loops, taken before Go ran anything), and in the
			// code the function has now, at its next entry.
			s.hosts++
			e.ssaStats.hosts++
			back, base, code := uintptr(c.ReturnTo), c.Base, e.ssa
			this, loops := e.this, e.loops
			exitPC, exitDepth := int(ctx.ExitPC), int(ctx.ExitDepth)
			// Past its call meanwhile, as the interpreter's frame is during
			// one, which a stack trace shows (stackFrame).
			f.pc = uint32(exitPC) + 1
			v, err := r.jitUnwindNative(idx, e, f.cl)
			if err == nil && r.stopped == nil && back != 0 && e.ssa == code && !e.invalid && code.Contains(back) {
				// Native code run meanwhile may have used the context.
				c.Base, c.Live, c.ReturnTo = base, 0, 0
				*(*Value)(unsafe.Pointer(&c.RetValue)) = v
				s.resumed++
				resume, pc = back, exitPC
				continue
			}
			e.ssaStats.records += ctx.Records
			// The receiver its records may name is this run's, not that of
			// one Go ran meanwhile.
			s.ssaCtx = ctx
			r.jitApplyRecords(f, this, ctx)
			jitHandlers(f, e.ssaProg, exitPC)
			if err == nil && f.cl.fn.Code[exitPC].Op == bytecode.OpSuperCall {
				// A super() native code made, its parent finished by Go.
				v = jitSuperBound(f, v)
			}
			if loop := jitLoopAt(loops, ctx, f, exitPC); loop != 0 {
				// The callee was a loop's callback (ssa.ForEachSite): Go goes
				// on with the loop after its index.
				v, err = r.jitLoopFinish(f, exitPC, loop, true, v, err)
			}
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
			r.jitRecompileLearned(f.cl, e)
			if !e.ssa.HasEntry(pc) || !e.native() {
				v, err, done, again := r.jitGoOn(f, e, pc, sp, true)
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
			r.jitApplyRecords(f, e.this, ctx)
			exitPC, exitDepth := int(ctx.ExitPC), int(ctx.ExitDepth)
			jitHandlers(f, e.ssaProg, exitPC)
			// Past its call meanwhile, as a stack trace shows it.
			f.pc = uint32(exitPC) + 1
			v, err := r.jitUnwindNative(idx, e, f.cl)
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
			r.jitRecompileLearned(f.cl, e)
			if !e.ssa.HasEntry(pc) || !e.native() {
				v, err, done, again := r.jitGoOn(f, e, pc, sp, true)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
			continue
		}
		if ctx.ExitKind == abi.ExitReturn {
			v := Value{num: math.Float64frombits(ctx.Ret)}
			if s := jitSource(&ctx.RetFrom); s != nil && s.ref != nil {
				v = *s
			}
			if f.thisRef != nil {
				// A derived constructor's: undefined is the this its
				// super() bound, or, unbound, a ReferenceError, as
				// executeAt's return makes it (derivedResult).
				v, err := r.derivedResult(f, f.cl, v)
				return v, err, true
			}
			return v, nil, true
		}
		e.ssaStats.records += ctx.Records
		r.jitApplyRecords(f, e.this, ctx)
		jitHandlers(f, e.ssaProg, int(ctx.ExitPC))
		if loop := jitLoopAt(e.loops, ctx, f, int(ctx.ExitPC)); loop != 0 {
			// A loop of a built-in's call that left -- a check of its, or
			// its back edge's poll: Go goes on with it from its index, as
			// V8's builtin continuation frames go on in the builtin, then
			// native code after the call where it can.
			s.hosts++
			e.ssaStats.hosts++
			exitPC, exitDepth := int(ctx.ExitPC), int(ctx.ExitDepth)
			f.pc = uint32(exitPC)
			var err error
			if ctx.ExitKind == abi.ExitPoll {
				r.backEdges = s.backEdgeBudget()
				err = r.checkInterruptNow()
			}
			res, err := r.jitLoopFinish(f, exitPC, loop, false, Undefined, err)
			sp, ok := r.jitCallResult(f, exitPC, exitDepth, res, err)
			if !ok || r.stopped != nil {
				sp, err, native := r.jitCatch(f, e, sp, err)
				if !native {
					return r.jitInterpret(f, sp, err)
				}
				pc, depth = int(f.pc), sp-f.base
				continue
			}
			pc, depth = int(f.pc), sp-f.base
			r.jitRecompileLearned(f.cl, e)
			if !e.ssa.HasEntry(pc) || !e.native() {
				v, err, done, again := r.jitGoOn(f, e, pc, sp, true)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
			continue
		}
		switch ctx.ExitKind {
		case abi.ExitDeopt:
			// The frame holds the state at the guard; the interpreter runs
			// the rest of this invocation, and the code, the function's
			// still, is invalidated, as V8's eager deoptimizations
			// invalidate it: compiled again once the existing tiers have
			// spent their budget (jitTierUp), entered at a loop's back edge
			// meanwhile if it is (tryJITLoop).
			s.guards++
			e.ssaStats.guards++
			if e.ssaStats.guardsAt == nil {
				e.ssaStats.guardsAt = map[int32]uint64{}
			}
			e.ssaStats.guardsAt[int32(int64(ctx.ExitSite))]++
			exitPC, sp := int(ctx.ExitPC), f.base+int(ctx.ExitDepth)
			if r.jitPropertyDeopt(f, e, e, int32(int64(ctx.ExitSite)), exitPC, sp) {
				// A shape the code was not compiled for: the interpreter
				// makes the operation, its cache learning the shape.
				s.shapeDeopts++
				if e.ssa == ran {
					r.jitInvalidate(f.cl, e)
				}
				f.pc = uint32(exitPC)
				return r.jitDeoptRun(f, e, sp)
			}
			// An addition speculated to add numbers that met a string is
			// learned as at a host exit (jitStringAddSeen), whatever the
			// compiles left for failed speculations: Go's from the next
			// lowering on, with every addition of its result after it, so
			// that a chain, tag + a + b + c, is learned at once. Its sites
			// had been learned one deoptimization, and one compile, at a
			// time, until none were left: PDF.js's createTableEntry then
			// deoptimized at every invocation.
			r.jitStringAddSeen(f, e, int(ctx.ExitPC))
			// So is an element read that met a string, or an object's
			// property by its name, as at a host exit (jitIndexSeen): a read
			// compiled before it ran, as a typed array's or an array's, had
			// been compiled again a guard at a time -- an array's element
			// as a number, then as it is, then a string's -- each failure
			// leaving the rest of its invocation to the interpreter: PDF.js's
			// font decryption's tokenizer, compiled before it ran, at each
			// font.
			if in := f.cl.fn.Code[ctx.ExitPC]; in.Op == bytecode.OpGetIndex || in.Op == bytecode.OpGetIndexThis || in.Op == bytecode.OpGetLocalIndex {
				if o, k, ok := r.jitIndexOperands(f, in, f.base+int(ctx.ExitDepth)); ok {
					r.jitIndexSeen(e, int(ctx.ExitPC), o, k)
				}
			}
			if !r.jitBigIntSeen(f, e, int(int64(ctx.ExitSite))) {
				jitDeoptimized(e, ctx, start)
			}
			if e.ssa == ran {
				r.jitInvalidate(f.cl, e)
			}
			f.pc = uint32(ctx.ExitPC)
			return r.jitDeoptRun(f, e, f.base+int(ctx.ExitDepth))
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
			if site := int(int64(ctx.ExitSite)); site >= 0 {
				// A generic operator that met BigInts, which Go makes from
				// native code once compiled again (ssa.GoArith).
				r.jitBigIntExit(f, e, site)
			}
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
					r.jitRecompileLearned(f.cl, e)
					if !e.ssa.HasEntry(pc) || !e.native() {
						v, err, done, again := r.jitGoOn(f, e, pc, sp, true)
						if !again {
							return v, err, done
						}
						pc, depth = s.goOnPC, s.goOnDepth
					}
					continue
				}
			}
			r.jitStringAddSeen(f, e, exitPC)
			switch jitCallOp(in.Op) {
			case bytecode.OpYield, bytecode.OpAwait, bytecode.OpYieldStar:
				return r.jitSuspend(f, in, f.base+int(ctx.ExitDepth))
			case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
				r.jitCallSeen(f, e, exitPC, f.base+int(ctx.ExitDepth), jitCallInstr(in))
			case bytecode.OpGetProp, bytecode.OpGetPropThis:
				r.jitPolySeen(f, e, exitPC, f.base+int(ctx.ExitDepth), in)
			case bytecode.OpGetIndex, bytecode.OpGetIndexThis, bytecode.OpGetLocalIndex:
				if o, k, ok := r.jitIndexOperands(f, in, f.base+int(ctx.ExitDepth)); ok {
					r.jitIndexSeen(e, exitPC, o, k)
				}
			case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn:
				r.jitPrivateSeen(f, e, exitPC, f.base+int(ctx.ExitDepth), in)
			case bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpNewRegExp:
				r.jitRefillLiteral(f, e, exitPC)
			case bytecode.OpDefineField:
				jitDefineSeen(e, exitPC)
			case bytecode.OpSetIndex:
				r.jitKeyedWriteSeen(e, exitPC, f.base+int(ctx.ExitDepth))
			}
			if r.jitTailGivesUp(f, exitPC) {
				// Deep in the stack, a call in tail position gives up the
				// frame for the callee's, as the interpreter's tail_call
				// does: run makes it (errTailCall), not the interpreter.
				r.jitRecompileLearned(f.cl, e)
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
			r.jitRecompileLearned(f.cl, e)
			if !e.ssa.HasEntry(pc) || !e.native() {
				// No entry here; or native code does not pay (jitSSAProfit),
				// and the interpreter runs the rest of this invocation.
				v, err, done, again := r.jitGoOn(f, e, pc, sp, true)
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
			// Compiled again, its budget spent, for what it learned (the
			// poll left for it, ssa.GoPoll): entered again at the loop's
			// header, in the new code.
			r.jitRecompileLearned(f.cl, e)
			if !e.ssa.HasEntry(pc) || !e.native() {
				v, err, done, again := r.jitGoOn(f, e, pc, f.base+depth, true)
				if !again {
					return v, err, done
				}
				pc, depth = s.goOnPC, s.goOnDepth
			}
		}
	}
}

// jitShared is what every context native code runs in shares with the one
// it was called from (abi.Context): where the global names are, the
// VM's stack. How deep calls may go they share in one place
// (jitState.bounds).
type jitShared struct {
	global, lexNames, globalLex, strMethods, stackBase unsafe.Pointer
	stackEnd                                           uint64
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
// in, when it is not what they hold: a native call then writes none of it.
// They hold it from the last time it was written (jitSharedContexts), if
// that was from idx or before, as nothing else writes it. How deep the
// calls may go from idx, which the VM's depth at the entry decides, is
// written once for them all (abi.Bounds).
func (r *Runtime) jitShareContexts(idx int, f *frame) {
	s := r.jit
	bounds := abi.Bounds{
		LevelLimit: uint64(min(len(s.ssaCtxs), idx+max(0, r.maxFrames-r.frameDepth))),
		// The level whose frame is tailCallDepth deep, the run's frame at
		// idx.
		TailLevel: uint64(idx + max(0, tailCallDepth-r.frameDepth)),
	}
	if bounds != s.bounds {
		s.bounds = bounds
		s.ssaSharedGen++
	}
	sh := jitShared{
		global: unsafe.Pointer(f.cl.scope()), lexNames: unsafe.Pointer(&r.lexNames),
		stackBase: unsafe.Pointer(unsafe.SliceData(r.stack)), stackEnd: uint64(len(r.stack)),
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
				sh.ccOffset = uint64(i) * uint64(jitEncoding.ValueSize)
			}
		}
	}
	if sh == s.ssaShared && s.ssaSharedFrom <= idx {
		return
	}
	for i := idx; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
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
	if s.charStrings == nil {
		s.charStrings = new([256]Value)
		for b := range s.charStrings {
			if b < 128 {
				s.charStrings[b] = Str(r.asciiCharString(byte(b)))
			} else {
				s.charStrings[b] = Str(r.latin1CharString(byte(b)))
			}
		}
	}
	for i := range chunk {
		chunk[i].Host = unsafe.Pointer(r)
		chunk[i].CharStrings = unsafe.Pointer(s.charStrings)
		chunk[i].SetConsts(&jitEncoding)
		chunk[i].Level, chunk[i].Bounds, chunk[i].Hints = uint64(old+i), &s.bounds, &s.hintTable
		if s.mega == nil {
			s.mega = new(abi.MegaCache)
		}
		chunk[i].Mega = s.mega
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
	for _, c := range s.ssaCtxs[old:] {
		c.StackBase, c.StackEnd, c.StackTop, c.StackHigh = last.StackBase, last.StackEnd, last.StackTop, last.StackHigh
		c.BackEdges, c.Global, c.LexNames, c.GlobalLex = last.BackEdges, last.Global, last.LexNames, last.GlobalLex
		c.StringMethods = last.StringMethods
		c.CharCodeAt = last.CharCodeAt
		c.StringProto, c.StringShape, c.CharCodeAtOffset = last.StringProto, last.StringShape, last.CharCodeAtOffset
	}
	if s.bounds.LevelLimit == uint64(old) {
		// Calls the contexts limited may go as deep as the new ones allow.
		s.bounds.LevelLimit = n
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
	// args, for an inlined callee's that keeps its call's arguments
	// (abi.Context's InlineArgs), says where they are; 0 for none.
	args uint64
	// construct marks a callee a construction called, whose result, if not
	// an object, is its receiver, and newTarget is then the function the
	// construction called, the frame's new.target.
	construct bool
	newTarget Value
	// callee, for an inlined callee's, is its function object
	// (abi.Context's InlineCallee), the frame's, as a stack trace names it
	// by; nil for a native call's.
	callee *Object
	// ctx is the level's context's index.
	ctx int
	// returnTo is where its native caller goes on after the call
	// (abi.Context.ReturnTo), and code the caller's code then: the caller
	// is resumed there once Go has finished this level (jitUnwindNative).
	returnTo uintptr
	code     *jit.SSACode
	// owner, for an inlined callee's level, is the entry whose code
	// inlined it, the code its exit left (jitFinishExit), and ownerCl a
	// closure of its function's.
	owner   *jitEntry
	ownerCl *closure
	// loop is the loop of a built-in's call the level's code was making at
	// its call or exit (abi.Context's Loop), which Go goes on with there;
	// loopFrame says Go made the built-in's frame past the level's
	// (jitPushLoopFrame), its callee's caller.
	loop      uint64
	loopFrame bool
}

// jitUnwindNative finishes the native calls the code running in context
// idx, e's, made, the innermost of which left native code, and the inlined
// callee an exit inside wrote the frame of: it makes the VM's frame of
// each, as runFD would have, from the outermost; then finishes each from
// the innermost -- that one from its exit, as runSSA does, the others from
// after their call, with what the one they called returned or threw -- and
// returns what the outermost returned or threw.
func (r *Runtime) jitUnwindNative(idx int, e *jitEntry, root *closure) (Value, error) {
	s := r.jit
	code := root.fn
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
			l.inline, l.locals, l.thisSlot, l.args = true, int(c.InlineLocals), int(c.InlineThis), c.InlineArgs
			if c.InlineCallee != 0 {
				// The object, which the caller's code keeps alive.
				l.callee = *(**Object)(unsafe.Pointer(&c.InlineCallee))
			}
			l.construct = callerPC < len(caller.Code) && caller.Code[callerPC].Op == bytecode.OpNew
			if l.construct && c.InlineCallee != 0 {
				// The object, which the caller's code keeps alive.
				l.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.InlineCallee)))
			}
			// Its code is that of the nearest level below it not inlined,
			// or e's, which inlined it, and the callees it is inlined in.
			// The closures it inlined: those of the code that ran, the
			// run's, which e may have replaced meanwhile, or a native
			// callee's, which is not compiled again while it is called.
			parent, parentCl, parentPC := e, root, s.ssaCtxs[idx].ExitPC
			inlined := s.goRuns[idx].inlined
			outer := true
			for k := len(s.unwinding) - 1; k >= start; k-- {
				if caller := &s.unwinding[k]; !caller.inline {
					parent, parentCl, parentPC = r.jitEntryOf(caller.cl), caller.cl, caller.pc
					inlined = parent.ssaInlined
					break
				}
				outer = false
			}
			for _, cl := range inlined {
				if uintptr(unsafe.Pointer(cl)) == uintptr(c.InlineClosure) {
					l.cl = cl
				}
			}
			if l.cl == nil {
				panic("jit: an inlined callee's closure is not its caller's")
			}
			l.owner, l.ownerCl = parent, parentCl
			if outer {
				// Counted once, for the call the outermost is inlined at,
				// below.
				left, leftPC = parent, int(parentPC)
			}
		} else {
			l.cl, l.this = (*closure)(c.Closure), *(*Value)(unsafe.Pointer(&c.This))
			l.loop = c.Loop
			// Its function object, where its call says (an accessor's).
			l.callee = r.jitLevelCallee(caller, callerPC, l.this)
			l.construct = callerPC < len(caller.Code) && caller.Code[callerPC].Op == bytecode.OpNew
			if callerPC < len(caller.Code) && caller.Code[callerPC].Op == bytecode.OpSuperCall && c.Function != 0 {
				// A super()'s parent, constructed: its function and its
				// new.target the context's, its this the instance, a
				// derived parent's unbound (ssa.CallSite's Super).
				l.construct, l.callee = true, *(**Object)(unsafe.Pointer(&c.Function))
			}
			if l.construct && c.NewTarget != 0 {
				l.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.NewTarget)))
			}
			l.returnTo = c.ReturnTo
			if k := len(s.unwinding) - 1; k >= start && !s.unwinding[k].inline {
				// The caller, a level of its own: its code now.
				if ce := r.jitEntryOf(s.unwinding[k].cl); ce != nil {
					l.code = ce.ssa
				}
			}
		}
		caller, callerPC = l.cl.fn, int(l.pc)
		s.unwinding = append(s.unwinding, l)
		c.Live, c.ReturnTo = 0, 0
	}
	n := len(s.unwinding) - start
	if left != nil && (n == 0 || !r.jitPoolEmptied(&s.unwinding[len(s.unwinding)-1]) && !r.jitShapeMiss(&s.unwinding[len(s.unwinding)-1])) && !left.reoptPending() {
		// An exit inside an inlined callee -- but one at a construction
		// inlined in it whose pool ran out, which Go fills again, as
		// jitCallSeen does not count at the call itself.
		r.jitInlineLeft(left, leftPC)
	}
	if left != nil && left != e {
		// An exit in what a native callee's code inlined: the code's, as
		// its own exits are, which is invalidated if it learned what to be
		// compiled again for before it goes on (below). Counted for
		// the inlined callee's code alone, a native callee inlining pop()
		// compiled before pop's property reads had run left native code at
		// them at every call, never compiled again for what they learned.
		left.ssaStats.hosts++
	}
	if n != 0 {
		// The innermost left -- but for one that never entered native code
		// (ExitEnter), a call Go makes; the others only return through Go.
		if l := &s.unwinding[len(s.unwinding)-1]; !l.inline && l.kind != abi.ExitEnter && !r.jitPoolEmptied(l) {
			// Its entry by its function: a callee only native callers
			// call has no hint. Not one that left for its pool to be
			// filled again.
			if e := r.jitEntryOf(l.cl); e != nil {
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
		if l.ctx == len(s.ssaCtxs)-1 && s.bounds.LevelLimit == uint64(len(s.ssaCtxs)) && l.kind == abi.ExitHost {
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
	if n != 0 && !s.unwinding[start].inline && idx < len(s.goRuns) && s.goRuns[idx].f != nil {
		if rf := s.goRuns[idx].f; jitLoopIn(e.makesLoops(), s.ssaCtxs[idx], code, int(s.ssaCtxs[idx].ExitPC)) != 0 {
			// The run's own loop's callee: the built-in's frame between
			// them, until the callee is finished.
			r.jitPushLoopFrame(rf, s.ssaCtxs[idx].Loop, false)
			defer func() { r.frameDepth-- }()
		}
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
			if e := r.jitEntryOf(l.cl); e != nil {
				f.cl, f.locals, f.base = l.cl, r.stack[l.base:l.base+fn.LocalCount:l.base+fn.LocalCount], l.base+fn.LocalCount
				saved := s.ssaCtx
				s.ssaCtx = c
				r.jitApplyRecords(f, e.this, c)
				s.ssaCtx = saved
				l.this = *(*Value)(unsafe.Pointer(&c.This))
			}
		}
		f.cl = l.cl
		f.locals = r.stack[l.base : l.base+fn.LocalCount : l.base+fn.LocalCount]
		if l.kind == abi.ExitEnter && !l.inline {
			jitStartLocals(fn, f.locals)
		}
		f.base = l.base + fn.LocalCount
		f.pc = uint32(l.pc)
		if k < n-1 {
			// Past the call it makes, as the interpreter's frame is during
			// one, which a stack trace shows.
			f.pc++
		}
		f.this, f.newTarget, f.callee, f.args = l.this, Undefined, l.callee, nil
		if l.inline && l.args != 0 {
			// Its call's arguments, in its caller's operands, which its
			// apply_arguments hands on (ssa.InlineState's Args).
			n, from := int(l.args>>32)-1, l.base-int(uint32(l.args))
			if n >= 0 && from >= 0 && from+n <= len(r.stack) {
				f.args = slices.Clone(r.stack[from : from+n])
			}
		}
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
		if c := s.ssaCtxs[l.ctx]; !l.inline && c.Cells != nil {
			// Its captured locals' cells, which its closures share: the
			// frame's, open on its slots now, which closes them as it is
			// popped.
			jitReopenCells(f, c)
		}
		f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
		if !l.inline && l.construct && fn.Kind == bytecode.KindDerivedConstructor {
			// A derived construction's, its this bound if its super()
			// bound it.
			jitDerivedFrame(f, s.ssaCtxs[l.ctx])
		}
		if !l.inline {
			// Those in force at its exit, or at its call: an exception
			// from the call unwinds the frame from there.
			if e := r.jitEntryOf(l.cl); e != nil {
				jitHandlers(f, e.ssaProg, int(l.pc))
			}
		}
		s.unwindingFrames = append(s.unwindingFrames, f)
		if k < n-1 && !l.inline && !s.unwinding[start+k+1].inline && l.loop != 0 &&
			jitLoopIn(r.jitEntryOf(l.cl).makesLoops(), s.ssaCtxs[l.ctx], fn, int(l.pc)) != 0 {
			// Its loop's callee, the next level: the built-in's frame
			// between them.
			r.jitPushLoopFrame(f, l.loop, true)
			l.loopFrame = true
		}
	}
	var v Value
	var err error
	for k := n - 1; k >= 0; k-- {
		f, l := s.unwindingFrames[start+k], s.unwinding[start+k]
		if l.loopFrame {
			// Its callee finished: the built-in's frame goes.
			r.frameDepth--
		}
		e := r.jit.hint(f.cl)
		if e == nil {
			e = r.jitFor(f.cl)
		}
		var next *jitNativeLevel
		if k < n-1 {
			next = &s.unwinding[start+k+1]
		}
		if k < n-1 && !l.inline && next.inline && e != nil && e.ssa != nil {
			// The code an exit inside its inlined callees left (above).
			r.jitRecompileLearned(f.cl, e)
		}
		if k == n-1 {
			v, err = r.jitFinishExit(f, e, &l)
		} else if !l.inline && !next.inline && next.returnTo != 0 && err == nil && r.stopped == nil &&
			e != nil && e.ssa != nil && e.ssa == next.code && !e.invalid && e.ssa.Contains(next.returnTo) {
			// Its native code goes on where the call returns to, as runSSA's
			// does after a callee leaves: its context, spills and keeps are
			// as it left them, and its callee's, frame base included -- the
			// collector marking or not, as there. The code must be the one
			// the call was made from: compiled again meanwhile, the
			// function's code may be where the old was, the old's return
			// address in the middle of other instructions.
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
			if err == nil && f.cl.fn.Code[l.pc].Op == bytecode.OpSuperCall {
				// A super() native code made, its parent finished by Go.
				v = jitSuperBound(f, v)
			}
			if loop := jitLoopAt(e.makesLoops(), s.ssaCtxs[l.ctx], f, int(l.pc)); loop != 0 && loop == l.loop {
				// Its callee was a loop's callback: Go goes on with the loop
				// after its index.
				v, err = r.jitLoopFinish(f, int(l.pc), loop, true, v, err)
			}
			sp, ok := r.jitCallResult(f, int(l.pc), int(l.depth), v, err)
			switch {
			case !ok || r.stopped != nil || e == nil || e.ssa == nil:
				v, err, _ = r.jitInterpret(f, sp, err)
			default:
				pc, depth, native := int(f.pc), sp-f.base, true
				if !e.ssa.HasEntry(pc) {
					// No entry after the call: the interpreter goes on to
					// the next loop header, native code from there
					// (jitGoOn), not for the rest of the invocation.
					var again bool
					if v, err, _, again = r.jitGoOn(f, e, pc, sp, false); !again {
						break
					}
					pc, depth = r.jit.goOnPC, r.jit.goOnDepth
					f.pc = uint32(pc)
				}
				if v, err, native = r.runSSA(f, e, pc, depth); !native {
					v, err, _ = r.jitInterpret(f, f.base+depth, nil)
				}
			}
		}
		switch {
		case l.construct && !l.inline && l.cl.fn.Kind == bytecode.KindDerivedConstructor:
			v, err = r.jitDerivedResult(v, err)
		case l.construct && err == nil && !v.IsObject():
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
	if e != nil && e.invalid && f.cl.jitRefused {
		// Its code invalidated, of a function Go does not enter, which only
		// native callers call natively: the invocation spends the code's
		// budget (jitTierUp), as tryJITAt has one Go enters spend it, and
		// as jitInvokedFrameless a frameless call. Uncounted, a visitor
		// called through its link (ssa.CallSite's Generic) that deoptimized
		// once was never compiled again, each call of it Go's from then on.
		r.jitTierUp(f.cl, e, int64(len(f.cl.fn.Code)))
	}
	// So too code that learned what to be compiled again for.
	r.jitLearnedCall(f.cl, e)
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
	jitStartLocals(fn, f.locals)
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this, f.newTarget, f.callee, f.args = this, newTarget, nil, nil
	f.openUpvalues, f.handlers = f.openUpvalues[:0], f.handlers[:0]
	f.thisRef, f.withScopes, f.evalVars, f.native, f.savedSP = nil, nil, nil, "", 0
	derived := construct && fn.Kind == bytecode.KindDerivedConstructor
	if derived {
		jitDerivedFrame(f, c)
	}
	v, err := r.jitEnterCallee(f, s.hint(cl), i)
	switch {
	case derived:
		v, err = r.jitDerivedResult(v, err)
	case construct && err == nil && !v.IsObject():
		v = this
	}
	r.popFrameOf(f, base)
	return v, err
}

// jitDerivedResult is a derived construction's result, from what its
// frame's run gave, as constructWithTarget makes it: an object; undefined
// returned before super(), a ReferenceError; anything else, a TypeError,
// thrown at the construction, which the constructor's own try does not
// catch.
func (r *Runtime) jitDerivedResult(v Value, err error) (Value, error) {
	switch {
	case err == errNoSuper:
		return Undefined, r.throwError(errReference, "%s", errNoSuper.Error())
	case err != nil:
		return Undefined, err
	case !v.IsObject():
		return Undefined, r.throwTypeError(errDerivedReturn)
	}
	return v, nil
}

// jitDerivedFrame gives f, a frame Go makes for a derived constructor's
// level, which a native construction called (ssa.CallSite's Derived), what
// runFD gives a derived construction's frame, from its context: its
// function (abi.Context's Function), new.target, instance, and a binding
// of its this, bound if its super() bound it (This).
func jitDerivedFrame(f *frame, c *abi.Context) {
	if c.Function == 0 || c.NewTarget == 0 {
		return
	}
	f.callee = *(**Object)(unsafe.Pointer(&c.Function))
	f.newTarget = Obj(*(**Object)(unsafe.Pointer(&c.NewTarget)))
	if this := *(*Value)(unsafe.Pointer(&c.This)); !this.IsUninitialized() {
		f.this, f.thisRef = this, &thisBinding{value: this, init: true}
		return
	}
	f.this = *(*Value)(unsafe.Pointer(&c.Instance))
	f.thisRef = &thisBinding{value: f.this}
}

// jitSuperBound binds f's this to what a super() native code made gave
// (ssa.CallSite's Super), its parent finished by Go: its result if an
// object, else the instance, as superCall binds it -- a second super()
// native code does not make (mir's superGuards), and the class has no
// fields for it to give (jitSuperTarget). The binding is the result.
func jitSuperBound(f *frame, v Value) Value {
	if f.thisRef == nil {
		return v
	}
	if !v.IsObject() {
		v = f.thisRef.value
	}
	f.this, f.thisRef.value, f.thisRef.init = v, v, true
	return v
}

// jitSuperTarget is the parent d's super() constructs with, natively
// (ssa.CallSite's Super), and the code it runs: d a derived class of no
// fields -- which super() would give the instance -- making no arrow --
// whose this its binding is, which native code has none of -- its
// prototype p a constructor of the realm's script, a base one of no
// fields, or derived; p a class with no constructor of its own, the code
// of the one it extends, skip, as V8 skips a default derived constructor
// (FindNonDefaultConstructorOrConstruct). nil where there is none.
func (r *Runtime) jitSuperTarget(d *Object) (p, skip *Object, cl *closure) {
	dfd := d.fn()
	if dfd == nil || dfd.closure == nil || dfd.ctorKind != ctorDerived || dfd.superCtor != d || dfd.fieldInit != nil || jitMakesArrow(dfd.closure.fn) {
		return nil, nil, nil
	}
	code := func(o *Object) *closure {
		fd := o.fn()
		if fd == nil || fd.closure == nil || fd.native != nil || fd.bound || fd.closure.realm != r.Realm || o.class != ClassFunction ||
			isGeneratorTemplate(fd.closure.fn) || fd.fieldInit != nil {
			return nil
		}
		switch fd.ctorKind {
		case ctorBase:
			return fd.closure
		case ctorDerived:
			if fd.superCtor == o {
				return fd.closure
			}
		}
		return nil
	}
	if p = d.proto; p == nil {
		return nil, nil, nil
	}
	if cl = code(p); cl == nil {
		return nil, nil, nil
	}
	if cl.fn.Kind == bytecode.KindDerivedConstructor && jitForwardsSuper(cl.fn) {
		if q := p.proto; q != nil {
			if qc := code(q); qc != nil && !(qc.fn.Kind == bytecode.KindDerivedConstructor && jitForwardsSuper(qc.fn)) {
				return p, q, qc
			}
		}
		return nil, nil, nil
	}
	return p, nil, cl
}

// jitMakesArrow reports whether fn makes an arrow, which shares its this
// binding.
func jitMakesArrow(fn *bytecode.Function) bool {
	for _, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Kind == bytecode.KindArrow {
			return true
		}
	}
	return false
}

// jitSuperCall is superCall for a super() Go makes for native code
// (abi.GoCallSuper): a parent whose code is invalid spends its budget, as
// a call Go makes does (jitCallAt, jitInvokedFrameless) -- one the
// existing tiers ran with no frame, a constructor storing its parameters
// (leafCall), had counted nothing, its code never compiled again, native
// super()s of it each Go's from then on.
func (r *Runtime) jitSuperCall(f *frame, args []Value) error {
	var ce *jitEntry
	var cl *closure
	var spends uint32
	if p := r.parentConstructorOf(f); p != nil {
		if ce, cl = r.jitInvalidCallee(Obj(p)); ce != nil {
			spends = ce.tierSpends
		}
	}
	err := r.superCall(f, args)
	r.jitInvokedFrameless(cl, ce, spends)
	return err
}

// jitSuperSeen notes the super() at pc of f's code, e's, that Go made: the
// parent it constructs with, which native code calls from the next compile
// on (ssa.CallSite's Super), as a call Go makes is learned (jitCallSeen).
func (r *Runtime) jitSuperSeen(f *frame, e *jitEntry, pc int) {
	if e == nil || f.callee == nil || pc >= len(f.cl.fn.Code) {
		return
	}
	if e.callSites == nil {
		e.callSites = make([]uint8, len(f.cl.fn.Code))
	}
	if e.callSites[pc] != 0 {
		return
	}
	p, skip, cl := r.jitSuperTarget(f.callee)
	if p == nil || !r.jitNativeOrLinked(cl) {
		e.callSites[pc] = jitCallDone
		return
	}
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), cl: cl, obj: p, parent: skip, active: f.callee})
	e.callSites[pc] = jitCallNative
	// Compiled again for it once it has spent a budget (jitLearnedDue), as
	// for a native call's target learned (jitCallSeen).
	e.inlinePending = true
	if e.inlineReopts < 2*jitInlineReoptimizations {
		e.inlinePending, e.inlineReopt = false, true
	}
}

// jitStartLocals makes the locals of a frame a native call made for fn, its
// code not run (abi.ExitEnter), undefined past its parameters, as fn's
// code's call entry would have, which the call left to it (mir's
// callEntry).
func jitStartLocals(fn *bytecode.Function, locals []Value) {
	for i := min(fn.ParamCount, len(locals)); i < len(locals); i++ {
		locals[i] = Undefined
	}
}

// jitShapeMiss reports whether level l, an inlined callee's, left native
// code at a property site for a shape the code was not compiled for, or at
// a site that knew none, its cache not megamorphic: the code is compiled
// again for what the cache learns there (jitFinishExit), and the exit is
// not the callee's leaving native code, which would stop it being inlined
// (jitInlineLeft) -- a callee inlined before its sites met their shapes
// left once for each.
func (r *Runtime) jitShapeMiss(l *jitNativeLevel) bool {
	if !l.inline || int(l.pc) >= len(l.cl.fn.Code) || !jitPropertyOp(l.cl.fn.Code[l.pc].Op) {
		return false
	}
	return l.kind == abi.ExitDeopt || l.kind == abi.ExitHost && !r.jitSiteMegamorphic(l.cl, r.jit.entryFor(l.cl.fn), int(l.pc))
}

// jitPoolEmptied reports whether level l left native code at a construction
// its code makes from a pool, now empty: it left for the pool to be filled
// again (jitRefillPools).
func (r *Runtime) jitPoolEmptied(l *jitNativeLevel) bool {
	fn := l.cl.fn
	if l.kind != abi.ExitHost || int(l.pc) >= len(fn.Code) || fn.Code[l.pc].Op != bytecode.OpNew {
		return false
	}
	e := r.jitEntryOf(l.cl)
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
// times more each for nothing.
func (r *Runtime) jitUnwound(e *jitEntry) {
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
	e.notNative, e.link.entry, e.link.body, e.nativeRetry = true, 0, 0, 0
	e.nativeBackoff = min(e.nativeBackoff+1, jitNativeBackoffs)
}

// jitLearning reports whether e's code has met, since it was compiled,
// what a compile would take that it was not compiled for: what is to be
// compiled again for (reoptPending), calls its exits learned
// (inlinePending), or a call it has left native
// code at but not yet decided, which the compile would decide if the
// existing tiers have met its target (jitSeedCalls).
func jitLearning(e *jitEntry) bool {
	if e.reoptPending() || e.inlinePending {
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
	if !e.owned() || e.invalid {
		// Dropped by the cache: its code is not its function's link's; or
		// invalidated, which native callers call again once compiled
		// again.
		return
	}
	if e.nativeRetry++; e.nativeRetry >= jitNativeRetry<<(e.nativeBackoff-1) {
		e.notNative, e.link.in, e.nativeOut = false, 0, 0
		e.ssaStats.calleeGoCalls, e.ssaStats.calleeWork = 0, 0
		e.link.setCode(e.ssa)
	}
}

// jitFinishExit finishes a frame whose native code, called natively, left
// for Go as l says, as runSSA would have had it been entered from Go: Go
// runs the instruction, or the interpreter the rest, or the interrupt
// check is made; native code goes on where it can.
func (r *Runtime) jitFinishExit(f *frame, e *jitEntry, l *jitNativeLevel) (Value, error) {
	pc, depth := int(l.pc), int(l.depth)
	f.pc = uint32(pc)
	kind := l.kind
	if l.loop != 0 && !l.inline && kind != abi.ExitEnter {
		if loop := jitLoopAt(e.makesLoops(), r.jit.ssaCtxs[l.ctx], f, pc); loop != 0 {
			// Its loop of a built-in's call left, as at runSSA's (above):
			// Go goes on with it from its index, then the frame after the
			// call.
			var err error
			if kind == abi.ExitPoll {
				r.backEdges = r.jit.backEdgeBudget()
				err = r.checkInterruptNow()
			}
			v, lerr := r.jitLoopFinish(f, pc, loop, false, Undefined, err)
			sp, ok := r.jitCallResult(f, pc, depth, v, lerr)
			if !ok || r.stopped != nil {
				v, err, _ := r.jitInterpret(f, sp, lerr)
				return v, err
			}
			pc, depth = int(f.pc), sp-f.base
			kind = jitLoopDone
		}
	}
	switch kind {
	case jitLoopDone:
	case abi.ExitEnter:
		return r.jitEnterCallee(f, e, l.ctx)
	case abi.ExitHost:
		if p := r.jit.goPending; p.set {
			// A call Go made from this native code (jitGoCallNested), its
			// result or its throw here.
			r.jit.goPending = jitGoPending{}
			if p.pc == pc && p.ctx == l.ctx {
				if p.err == nil && f.thisRef != nil && f.cl.fn.Code[pc].Op == bytecode.OpSuperCall {
					// A super() Go made on a frame of the level's of its
					// own (jitGoCallNested): this one's binding is the
					// this it bound.
					f.this, f.thisRef.value, f.thisRef.init = p.v, p.v, true
				}
				sp, ok := r.jitCallResult(f, pc, depth, p.v, p.err)
				if !ok || r.stopped != nil {
					v, err, _ := r.jitInterpret(f, sp, p.err)
					return v, err
				}
				r.jitRecompileLearned(f.cl, e)
				pc, depth = int(f.pc), sp-f.base
				break
			}
		}
		// What it learns here it learns as runSSA's exits do: a function
		// only native callers run is entered from Go nowhere else.
		in := f.cl.fn.Code[pc]
		if l.inline && l.owner != nil && jitPropertyOp(in.Op) && !r.jitSiteMegamorphic(f.cl, e, pc) {
			// An inlined callee's read or write of a shape the code was not
			// compiled for, or at a site that knew none: Go makes it, as
			// every exit inside an inlined callee leaves its operation to
			// Go, but it is a map check's failure all the same, as runSSA's
			// deoptimizations at the code's own sites are
			// (jitPropertyDeopt): the site's cache learns the shape, and
			// the code that inlined the callee is invalidated, compiled
			// again for it once the existing tiers have spent their budget.
			r.jit.shapeHosts++
			r.jitDeoptSeen(f, e, pc, f.base+depth, in, l.owner.ssaSerial)
			if e != nil {
				r.jit.account(e)
			}
			r.jitInvalidate(l.ownerCl, l.owner)
		}
		if e != nil && e.ssa != nil {
			e.ssaStats.hosts++
			r.jitStringAddSeen(f, e, pc)
			switch jitCallOp(in.Op) {
			case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
				r.jitCallSeen(f, e, pc, f.base+depth, jitCallInstr(in))
			case bytecode.OpGetProp, bytecode.OpGetPropThis:
				r.jitPolySeen(f, e, pc, f.base+depth, in)
			case bytecode.OpGetIndex, bytecode.OpGetIndexThis, bytecode.OpGetLocalIndex:
				if o, k, ok := r.jitIndexOperands(f, in, f.base+depth); ok {
					r.jitIndexSeen(e, pc, o, k)
				}
			case bytecode.OpGetPrivate, bytecode.OpSetPrivate, bytecode.OpPrivateIn:
				r.jitPrivateSeen(f, e, pc, f.base+depth, in)
			case bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpNewRegExp:
				r.jitRefillLiteral(f, e, pc)
			case bytecode.OpDefineField:
				jitDefineSeen(e, pc)
			}
		}
		if l.inline && l.owner != nil {
			switch in.Op {
			case bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpNewRegExp:
				// An inlined callee's literal, whose pool the code that
				// inlined it keeps (jitFeedback's Literal).
				r.jitRefillLiteral(f, l.owner, pc)
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
			if e.inlinePending && e.inlineReopts < 2*jitInlineReoptimizations {
				e.inlinePending, e.inlineReopt = false, true
			}
			r.jitRecompileLearned(f.cl, e)
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
		// The code that deoptimized: the callee's own, or, for an inlined
		// callee's level, the code that inlined it.
		owner, ownerCl := l.owner, l.ownerCl
		if owner == nil {
			owner, ownerCl = e, f.cl
		}
		if l.kind == abi.ExitDeopt && !r.jitPropertyDeopt(f, e, owner, int32(int64(l.site)), pc, f.base+depth) && e != nil && e.ssa != nil {
			// A speculation that failed, learned as runSSA learns one: a
			// function only native callers run had kept failing it, at its
			// entry at every call, never compiled again generic.
			r.jitStringAddSeen(f, e, pc)
			jitDeoptimized(e, &abi.Context{ExitSite: l.site}, 0)
		}
		if l.kind == abi.ExitDeopt {
			// Invalidated, as runSSA's deoptimizations invalidate the code:
			// the interpreter runs the rest of the frame.
			r.jitInvalidate(ownerCl, owner)
		}
		v, err, _ := r.jitInterpret(f, f.base+depth, nil)
		return v, err
	}
	f.pc = uint32(pc)
	if e != nil && e.ssa != nil && !e.ssa.HasEntry(pc) {
		// No entry here, as after a guard's exit: the interpreter goes on
		// to the next loop header, and native code from there, as runSSA
		// has it go on (jitGoOn), not for the rest of the invocation. The
		// frame is no run's of e's code: its caller's run, which may be,
		// goes on after the call.
		v, err, _, again := r.jitGoOn(f, e, pc, f.base+depth, false)
		if !again {
			return v, err
		}
		pc, depth = r.jit.goOnPC, r.jit.goOnDepth
		f.pc = uint32(pc)
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
	if in.Op == bytecode.OpIterStep {
		// A destructuring's step Go made (abi.GoCallFill): its cursor
		// stays, and its value goes on top.
		sp := f.base + depth
		f.pc = uint32(pc + 1)
		if err != nil {
			return sp, false
		}
		r.stack[sp] = v
		return sp + 1, true
	}
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
	switch in.Op {
	case bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp:
		// An accessor's read or write, its getter or setter called
		// (ssa.AccessorSite), or one Go made (abi.GoCallGet): the object
		// goes but for get_prop_this, which keeps it below the value; a
		// write's value goes too, and leaves nothing.
		sp := f.base + depth
		f.pc = uint32(pc + 1)
		switch in.Op {
		case bytecode.OpGetProp:
			sp--
		case bytecode.OpSetProp:
			sp -= 2
		}
		if err != nil {
			return sp, false
		}
		if in.Op == bytecode.OpSetProp {
			return sp, true
		}
		r.stack[sp] = v
		return sp + 1, true
	}
	sp := f.base + depth - int(in.A) - 1
	switch in.Op {
	case bytecode.OpTypeOf:
		// Its operand (abi.GoCallTypeOf).
		sp = f.base + depth - 1
	case bytecode.OpInstanceOf:
		// The object and the constructor (abi.GoCallInstanceOf).
		sp = f.base + depth - 2
	case bytecode.OpSuperCall:
		// Its arguments (abi.GoCallSuper); the this it bound comes.
		sp = f.base + depth - int(in.A)
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
	if f.cl.jitResumeSkip != 0 {
		// One of the resumptions counted at the last look (below).
		f.cl.jitResumeSkip--
		return Undefined, nil, false
	}
	e := r.jit.hint(f.cl)
	if e != nil && e.entrySlow && !e.invalid && !e.reoptPending() {
		// Code judged slow, which Go does not enter until it retries it
		// (jitRetrySlow): the next jitSSAProbe resumptions, counted now,
		// run in the tree without a look at the code. Looked at each time,
		// a generator's every step, of a loop over its values, had cost
		// it a twentieth of what it took.
		e.slowCalls += 1 + jitSSAProbe
		f.cl.jitResumeSkip = jitSSAProbe
		r.jit.slowResumes++
		jitRetrySlow(e)
		return Undefined, nil, false
	}
	if a := r.jitFeedbackCalls(); f.cl.jitCalls < a {
		f.cl.jitCalls++
		if f.cl.jitCalls < a {
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
	return r.tryJITAt(f, pc, p.Maps[pc].Depth, false, false)
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
// The element types native code reads are the VM's: either difference
// overflows, which does not compile, unless they are the same.
const (
	_ = uint8(elemInt8-abi.ElemInt8) + uint8(abi.ElemInt8-elemInt8)
	_ = uint8(elemUint8Clamped-abi.ElemUint8Clamped) + uint8(abi.ElemUint8Clamped-elemUint8Clamped)
	_ = uint8(elemUint16-abi.ElemUint16) + uint8(abi.ElemUint16-elemUint16)
	_ = uint8(elemUint32-abi.ElemUint32) + uint8(abi.ElemUint32-elemUint32)
	_ = uint8(elemFloat16-abi.ElemFloat16) + uint8(abi.ElemFloat16-elemFloat16)
	_ = uint8(elemFloat64-abi.ElemFloat64) + uint8(abi.ElemFloat64-elemFloat64)
)

func jitTypeWord(x any) uint64 {
	return uint64((*[2]uintptr)(unsafe.Pointer(&x))[0])
}
