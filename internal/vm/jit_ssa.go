//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"encoding/binary"
	"errors"
	"iter"
	"math"
	"math/bits"
	"runtime"
	"runtime/metrics"
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
	ObjectProps:      int32(unsafe.Offsetof(Object{}.valp)),
	ObjectPropsLen:   int32(unsafe.Offsetof(Object{}.valn)),
	ObjectPropsCap:   int32(unsafe.Offsetof(Object{}.valc)),
	ShapeProto:       int32(unsafe.Offsetof(shape{}.proto)),
	WriteBarrier:     jit.WriteBarrier(),
	CallGo:           jit.CallGo(),
	BigFast:          uint64(jitBigFastAddr()),
	LeafFast:         uint64(jitLeafFastAddr()),
	GoFast:           uint64(jitGoFastAddr()),
	StoreRef:         jit.StoreRef(),
	Round:            runtime.GOARCH == "arm64" || cpu.X86.HasSSE41,
	ShapeKeys:        int32(unsafe.Offsetof(shape{}.keys)),
	ShapeNext:        int32(unsafe.Offsetof(shape{}.next)),
	ShapeNextKey:     int32(unsafe.Offsetof(shape{}.nextKey)),
	ShapeNextFlags:   int32(unsafe.Offsetof(shape{}.nextFlags)),
	ShapeIndex:       int32(unsafe.Offsetof(shape{}.index)),
	LinearScan:       linearScanLimit,
	PropDefault:      uint8(propDefault),
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
	ObjectExt:          int32(unsafe.Sizeof(Object{})),
	ObjectArrayLen:     int32(unsafe.Sizeof(Object{}) + unsafe.Offsetof(arrayExt{}.arrayLen)),
	ObjectElems:        int32(unsafe.Sizeof(Object{}) + unsafe.Offsetof(objectExt{}.elems)),
	ExtElems:           int32(unsafe.Offsetof(objectExt{}.elems)),
	ObjectData:         int32(unsafe.Offsetof(objectExt{}.data)),
	IterState:          int32(unsafe.Offsetof(iterObject{}.st)),
	ClassCursor:        uint8(ClassCursor),
	IterDone:           int32(unsafe.Offsetof(iterState{}.done)),
	IterArr:            int32(unsafe.Offsetof(iterState{}.arr)),
	IterIdx:            int32(unsafe.Offsetof(iterState{}.idx)),
	IterForIn:          int32(unsafe.Offsetof(iterState{}.forIn)),
	ForInState:         int32(unsafe.Offsetof(forInObject{}.in) - unsafe.Offsetof(iterObject{}.st)),
	ForInKeys:          int32(unsafe.Offsetof(forInState{}.keys)),
	ForInObj:           int32(unsafe.Offsetof(forInState{}.obj)),
	ForInShape:         int32(unsafe.Offsetof(forInState{}.shape)),
	ForInIndexKeys:     int32(unsafe.Offsetof(forInState{}.indexKeys)),
	ShapeUnique:        int32(unsafe.Offsetof(shape{}.unique)),
	ShapeSide:          int32(unsafe.Offsetof(shape{}.side)),
	SideForIn:          int32(unsafe.Offsetof(shapeSide{}.forIn)),
	EnumKeys:           int32(unsafe.Offsetof(forInKeys{}.keys)),
	EnumAtoms:          int32(unsafe.Offsetof(forInKeys{}.atoms)),
	ForInAtoms:         int32(unsafe.Offsetof(forInState{}.keyAtoms)),
	HostForInLast:      int32(unsafe.Offsetof(Runtime{}.forInLast)),
	HostRealm:          int32(unsafe.Offsetof(Runtime{}.Realm)),
	ForInChains:        int32(unsafe.Offsetof(Realm{}.forInChains)),
	ChainSize:          int32(unsafe.Sizeof(forInChain{})),
	ChainProto:         int32(unsafe.Offsetof(forInChain{}.proto)),
	ChainCell:          int32(unsafe.Offsetof(forInChain{}.cell)),
	CellValid:          int32(unsafe.Offsetof(protoCell{}.valid)),
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
	GenObject:          int32(unsafe.Offsetof(generatorObject{}.g)),
	ClassGenerator:     uint8(ClassGenerator),
	GenClosure:         int32(unsafe.Offsetof(generator{}.cl)),
	GenThis:            int32(unsafe.Offsetof(generator{}.this)),
	GenVals:            int32(unsafe.Offsetof(generator{}.vals)),
	GenPC:              int32(unsafe.Offsetof(generator{}.pc)),
	GenRare:            int32(unsafe.Offsetof(generator{}.rare)),
	GenSaved:           int32(unsafe.Offsetof(generator{}.saved)),
	GenState:           int32(unsafe.Offsetof(generator{}.state)),
	GenStarted:         int32(unsafe.Offsetof(generator{}.started)),
	GenDelegating:      int32(unsafe.Offsetof(generator{}.delegating)),
	GenSuspendedStart:  uint8(genSuspendedStart),
	GenSuspendedYield:  uint8(genSuspendedYield),
	GenExecuting:       uint8(genExecuting),
	GenCompleted:       uint8(genCompleted),
	GenRoom:            int32(unsafe.Offsetof(generator{}.room)),
	GenRoomCursor:      int32(unsafe.Offsetof(generatorLoop{}.cur)),
	GenIterFrom:        int32(unsafe.Offsetof(genIterationCache{}.from)),
	GenIterCell:        int32(unsafe.Offsetof(genIterationCache{}.cell)),
	GenIterNextObj:     int32(unsafe.Offsetof(genIterationCache{}.next) + unsafe.Offsetof(ownProp{}.o)),
	GenIterNextIdx:     int32(unsafe.Offsetof(genIterationCache{}.next) + unsafe.Offsetof(ownProp{}.i)),
	GenIterIterObj:     int32(unsafe.Offsetof(genIterationCache{}.iter) + unsafe.Offsetof(ownProp{}.o)),
	GenIterIterIdx:     int32(unsafe.Offsetof(genIterationCache{}.iter) + unsafe.Offsetof(ownProp{}.i)),
	FuncData:           jitTypeWord((*funcData)(nil)),
	FuncClosure:        int32(unsafe.Offsetof(funcData{}.closure)),
	ClosureFn:          int32(unsafe.Offsetof(closure{}.fn)),
	ClosureUpvalues:    int32(unsafe.Offsetof(closure{}.upvalues)),
	CodeEnv:            int32(unsafe.Offsetof(closureCode{}.env)),
	ClosureRealm:       int32(unsafe.Offsetof(closure{}.realm)),
	ClosureCode:        int32(unsafe.Offsetof(closure{}.closureCode)),
	ClosureHint:        int32(unsafe.Offsetof(closureCode{}.jitHint)),
	Mega:               true,
	FuncExtra:          int32(unsafe.Offsetof(funcData{}.extra)),
	ExtraLexThis:       int32(unsafe.Offsetof(funcExtra{}.lexThis)),
	ExtraLexThisRef:    int32(unsafe.Offsetof(funcExtra{}.lexThisRef)),
	ThisBindingValue:   int32(unsafe.Offsetof(thisBinding{}.value)),
	ThisBindingInit:    int32(unsafe.Offsetof(thisBinding{}.init)),
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
	LinkGen:            int32(unsafe.Offsetof(jitLink{}.gen)),
	ClassArray:         uint8(ClassArray),
	ClassTypedArray:    uint8(ClassTypedArray),
	TypedKind:          int32(unsafe.Offsetof(typedArrayData{}.kind)),
	TypedElems:         int32(unsafe.Offsetof(typedArrayData{}.data)),
	TypedLength:        int32(unsafe.Offsetof(typedArrayData{}.length)),
	TypedEpoch:         int32(unsafe.Offsetof(typedArrayData{}.epoch)),
	FlagSparse:         uint8(objHasSparseElements),
	FlagExt:            uint8(objHasExt),
	FlagData:           uint8(objHasData),
	FlagPoly:           uint8(objPolyProto >> 8),
	FlagHTMLDDA:        uint8(objHTMLDDA),
	FlagShared:         uint8(objSharedElems),
	FlagExtensible:     uint8(objExtensible),
	LengthKey:          uint32(atomLength),
	FlagLengthWritable: uint8(objArrayLengthWritable),
}

// jitMaxDeopts is how many eager deoptimizations a function's code makes,
// each invalidating it (jitRegInvalidate), before the function is compiled
// no more and runs in the interpreter for good, as V8 gives up optimizing
// a function that keeps deoptimizing. Each compile meanwhile waits for the
// interpreter's budget, and for its caches' feedback to have settled
// (jitRegWarmUp): a function whose feedback has settled deoptimizes no more,
// and one meeting new shapes in phases -- a compiler's walking a syntax
// tree of new kinds of node -- deoptimizes about once a phase.
const jitMaxDeopts = 64

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
	// that met what the code cannot use (jitRegDeoptSeen); missed are the
	// receivers' shapes the site deoptimized for, each with the compile of
	// the code that did (jitEntry's ssaSerial).
	mega   bool
	missed []jitMissed
	// prim marks a read or a write that met a receiver that is a
	// primitive, but not undefined or null, whose read or write throws:
	// Go reads or writes such a receiver's property on the operation's
	// branch of its own (ssa.PropertySite's Primitives), rather than at an
	// exit.
	prim bool
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
	return r.jitDeepAddFrom(before, o.shape, o.proto(), key)
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
	for p := proto; p != nil; p = p.proto() {
		if len(a.up) == jitAddDepth || r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) || !passesWrite(p, key) {
			return jitDeepAdd{}, false
		}
		a.up, a.shapes = append(a.up, p), append(a.shapes, p.shape)
	}
	return a, true
}

// jitPolyWriteSeenOp is what jitRegWriteSeen learns in e: the write at
// pc, its site's cache cache, of key where named, to o, of shape
// before then.
func (r *Runtime) jitPolyWriteSeenOp(e *jitEntry, pc int, cache *propCache, key Atom, named bool, o *Object, before *shape) {
	i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
	if i < 0 {
		e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
		i = len(e.poly) - 1
	}
	p := &e.poly[i]
	if o != nil && named {
		// A setter on the object's prototypes, as for a read's getter: of
		// the shape the object had, which the setter may have changed.
		if a, ok := r.jitAccessorCaseOf(o, before, key, true); ok {
			if jitAddAccessorCase(p, a) {
				e.polyReopt = true
			}
			return
		}
	}
	if len(p.cases)+len(p.adds) >= jitPropertyCases {
		return
	}
	c := *cache
	if c.shape == nil || c.shape == noShape || c.getter || c.next == setterNext || c.idx < 0 || c.next == nil && c.p1 != nil {
		// No cache: an addition along a chain deeper than one holds, as
		// the object took it.
		if o == nil || !named {
			return
		}
		a, ok := r.jitDeepAddOf(o, before, key)
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

// jitPropertyCases is how many shapes a read is compiled for, as V8's
// polymorphic inline caches keep: one more leaves for Go.
const jitPropertyCases = 4

// jitPropOp is a named property's operation as what the JIT learns at its
// exits sees it, of either form's code: its key, its receiver, its site's
// cache (nil where it has none), and whether it is a write or a method's
// read (get_prop_this's, get_method's).
type jitPropOp struct {
	key           Atom
	recv          Value
	c             *propCache
	write, method bool
}

// plain reports whether op is a plain read: get_prop's, not a method's.
func (op *jitPropOp) plain() bool { return !op.write && !op.method }

// jitPolySeenOp is what jitRegReadSeen and jitRegHostSeen learn in e: the
// read op at pc that native code left to Go.
func (r *Runtime) jitPolySeenOp(e *jitEntry, pc int, op jitPropOp) {
	recv := op.recv
	if recv.IsString() && op.method && !slices.Contains(e.strSites, uint32(pc)) {
		// A string's method: read off String.prototype by the code
		// compiled again, as for a shape the read had not met.
		e.strSites = append(e.strSites, uint32(pc))
		e.polyReopt = true
		return
	}
	if recv.IsBigInt() && op.method && !slices.Contains(e.bigSites, uint32(pc)) {
		// A BigInt's: off BigInt.prototype, as a string's.
		e.bigSites = append(e.bigSites, uint32(pc))
		e.polyReopt = true
		return
	}
	if r.jitPrimitiveSeen(e, pc, &op) {
		// A number's or a boolean's: off Number.prototype or
		// Boolean.prototype, as a string's.
		e.polyReopt = true
		return
	}
	if !recv.IsObject() {
		if !recv.IsObject() && !recv.IsNullish() && op.plain() {
			// A string's, a number's, a boolean's: Go reads it from
			// native code once the code is compiled again (prim) -- an
			// element type, React's createElement's a.defaultProps of
			// "div" -- where it had left native code at each.
			if p := jitPolySiteAt(e, pc); !p.prim {
				p.prim = true
				e.polyReopt = true
			}
		}
		return
	}
	if op.method && op.c != nil && !slices.Contains(e.strSites, uint32(pc)) {
		if c := op.c; (c.shape == nil || c.shape == noShape) && c.fills == 0 && r.proto.str != nil && r.proto.str.findOwn(op.key) >= 0 {
			// An object's method, at a read the code took for a string's
			// (jitFeedback's StringProperty), its cache knowing nothing: a
			// function run once leaves no read in the interpreter's cache. The
			// cache takes the receiver, as the interpreter's read would
			// have, and the code is compiled again for its shape; it had
			// left there at every entry, and was judged slow for it.
			r.fillPropCache(c, recv.Object(), op.key, false)
			if c.shape != nil && c.shape != noShape {
				e.polyReopt = true
				return
			}
		} else if c.shape != nil && c.shape != noShape && c.shape == recv.Object().shape && r.proto.str != nil && r.proto.str.findOwn(op.key) >= 0 {
			// So too where the interpreter filled the cache between the
			// compile and this exit, with the receiver's shape: the code
			// still takes the read for a string's, and had left at every
			// read -- [i, 1].at(1), the interpreter running the rest of
			// each iteration -- the exit learning nothing.
			e.polyReopt = true
			return
		}
	}
	if op.plain() {
		// A getter on the receiver's prototypes, which the code calls once
		// compiled again for the receiver's shape too (jitFeedback's
		// Accessor).
		if a, ok := r.jitAccessorCaseOf(recv.Object(), recv.Object().shape, op.key, false); ok {
			if jitAddAccessorCase(jitPolySiteAt(e, pc), a) {
				e.polyReopt = true
			}
			return
		}
	}
	r.jit.megaFill(recv.Object(), op.key)
	absent := r.jitMegaFillProto(recv.Object(), op.key)
	var c propCache
	r.fillPropCache(&c, recv.Object(), op.key, false)
	if c.shape == nil {
		// One a cache cannot hold, found past its two prototypes.
		if d, ok := r.jitDeepReadOf(recv.Object(), op.key); ok && jitAddDeepRead(jitPolySiteAt(e, pc), d) {
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
	if c.idx < int32(holder.valn) && !holder.vals()[c.idx].IsNumber() {
		jitNumberMissed(e, pc)
	}
	// A shape it was not compiled for: at a megamorphic site, the
	// megamorphic cache, filled above, has it natively from now on.
}

// jitPrimitiveSeen notes in e the method read in at pc, of f's function,
// as one that met recv, a number or a boolean, and reports whether it did:
// one whose key the prototype the realm fixed for recv's kind,
// Number.prototype or Boolean.prototype, has as a plain data property,
// which the code compiled again reads off that prototype, as a string's
// off String.prototype (jitFeedback's StringProperty). A site that has met
// another primitive already is read as StringProperty takes it, the other
// kinds' receivers left to Go; a method the prototype does not have, Go
// reads.
func (r *Runtime) jitPrimitiveSeen(e *jitEntry, pc int, op *jitPropOp) bool {
	var sites *[]uint32
	var proto *Object
	switch recv := op.recv; {
	case recv.IsNumber():
		sites, proto = &e.numSites, r.proto.number
	case recv.IsBool():
		sites, proto = &e.boolSites, r.proto.boolean
	default:
		return false
	}
	if !op.method || proto == nil {
		return false
	}
	at := uint32(pc)
	if slices.Contains(e.strSites, at) || slices.Contains(e.bigSites, at) || slices.Contains(e.numSites, at) || slices.Contains(e.boolSites, at) {
		return false
	}
	key := op.key
	i := proto.findOwn(key)
	if key.IsIndex() || i < 0 || proto.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return false
	}
	*sites = append(*sites, at)
	return true
}

// jitPolyPrim reports whether e's read at pc met a primitive receiver
// whose read does not throw (jitPolySite's prim).
func jitPolyPrim(e *jitEntry, pc int) bool {
	for i := range e.poly {
		if p := &e.poly[i]; int(p.pc) == pc {
			return p.prim
		}
	}
	return false
}

// jitDeoptSeenOp is what jitRegDeoptSeen learns in e: the operation op at
// pc, which deoptimized in the code of compile serial, before the
// interpreter makes it.
func (r *Runtime) jitDeoptSeenOp(e *jitEntry, pc int, op jitPropOp, serial uint32) {
	if e == nil {
		// Nothing to note in: the cache learns what it can.
		e = &jitEntry{jitLearned: &jitLearned{}}
	}
	key, write, recv := op.key, op.write, op.recv
	if recv.IsBigInt() && op.method && !slices.Contains(e.bigSites, uint32(pc)) {
		// A BigInt's method: read off BigInt.prototype by the code compiled
		// again, the site no polymorphic one (jitFeedback's StringProperty).
		e.bigSites = append(e.bigSites, uint32(pc))
		return
	}
	if r.jitPrimitiveSeen(e, pc, &op) {
		// A number's or a boolean's: off Number.prototype or
		// Boolean.prototype, so.
		return
	}
	p := jitPolySiteAt(e, pc)
	if recv.IsString() && op.method && !slices.Contains(e.strSites, uint32(pc)) {
		// A string's method: read off String.prototype by the code
		// compiled again.
		e.strSites = append(e.strSites, uint32(pc))
		return
	}
	if !recv.IsObject() || op.c == nil {
		p.mega = true
		if !recv.IsObject() && !recv.IsNullish() && (op.plain() || write && !r.jitStress.primLeave) {
			// A primitive's read or write, Go's from native code in the
			// code compiled again (jitPolySite's prim): one compile, not
			// one to be megamorphic and another for the primitive.
			p.prim = true
		}
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
	c := op.c
	if write || c.fills >= maxCacheFills {
		// A write's cache learns as the interpreter writes -- an own
		// property, an addition, or, for one it cannot hold, a fill all the
		// same, toward giving up (setPropCached); a cache that has given
		// up is megamorphic.
		return
	}
	// The site's cache learns the receiver's shape here, as a cached read
	// would teach it (ownScan, cachedGet): the interpreter,
	// which makes the read after, reads an object's small table without
	// it (plainOwn).
	if c.holder(o) == nil {
		r.fillPropCache(c, o, key, op.plain())
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
	if c.idx < int32(holder.valn) && !holder.vals()[c.idx].IsNumber() {
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
	if s.mega == nil || sh == nil || sh.loose || o.class != ClassObject && o.class != ClassFunction || int(o.valn) <= abi.MaxScan {
		return
	}
	i := o.findOwn(key)
	if i < 0 || int(i) >= len(sh.keys) || int(i) >= int(o.valn) {
		return
	}
	flags := sh.keys[i].flags
	if flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return
	}
	idx := uint32(i)
	if flags&propWritable != 0 && !sh.pristine {
		// Not a pristine object's, whose writes are left to Go, which
		// tells it it is no longer (shape's pristine).
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
	for p := o.proto(); p != nil; p = p.proto() {
		if n == abi.MegaDepth || r.ensureProtoShape(p) == nil || p.cacheShape() == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return false
		}
		chain[n] = abi.MegaLink{Object: unsafe.Pointer(p), Shape: unsafe.Pointer(p.shape)}
		n++
		i := p.findOwn(key)
		if i < 0 {
			continue
		}
		if i >= p.shape.n || int(i) >= int(p.valn) || p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
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

// jitMegaFillAdd has the megamorphic cache name the addition of key, a
// plain data property, that a store has just made to o, of shape from
// before it, along from's shared transition (abi.MegaEntry's Next): where
// none of o's prototypes has the key -- the chain all of them, up to
// abi.MegaDepth, each of a class with shapes that does not answer the key,
// as an absent read's entry has them (jitMegaFillProto) -- so that native
// code adds it to the next object of that shape, extensible and with room,
// as V8's megamorphic stores take a transition the stub cache holds. A
// write whose site met more shapes than it was compiled for -- o.y = j on
// objects Object.create made of a prototype each call of a function makes
// anew -- had had each addition made by Go.
func (r *Runtime) jitMegaFillAdd(o *Object, from *shape, key Atom) {
	s := r.jit
	next := o.shape
	if s == nil || s.mega == nil || from == nil || from.loose || from.unique || from.tree == nil || next == nil || next.loose || next.unique ||
		next.parent != from || next.n != from.n+1 || int(o.valn) != int(next.n) || o.class != ClassObject || o == r.global ||
		key.IsIndex() || next.keys[from.n] != (propKey{key: key, flags: propDefault}) {
		return
	}
	chain := new(abi.MegaChain)
	n := 0
	for p := o.proto(); p != nil; p = p.proto() {
		if n == abi.MegaDepth || r.ensureProtoShape(p) == nil || p.cacheShape() == nil || !shapeClass(p.class) || synthesized(p.class, key) ||
			p.findOwn(key) >= 0 {
			return
		}
		chain[n] = abi.MegaLink{Object: unsafe.Pointer(p), Shape: unsafe.Pointer(p.shape)}
		n++
	}
	remember(from)
	remember(next)
	for _, l := range chain[:n] {
		remember((*shape)(l.Shape))
	}
	s.megaPut(abi.MegaEntry{Shape: unsafe.Pointer(from), Chain: chain, Key: uint32(key), Index: abi.MegaWritable | abi.MegaAbsent, Next: unsafe.Pointer(next)})
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
	if e.reg {
		// A register instruction's: its every instruction built generic.
		e.failed = append(e.failed, int32(pc))
	}
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
// jitPropertyCases, and reports whether it did. A shape another came from
// by adding properties -- an object's whose methods give it fields after
// its construction, as it moves on from its constructor's shape -- may be
// stale, no object of it left, or live, objects of it never given those
// properties; V8's inline caches keep both maps, deprecating only one whose
// field generalized. So it keeps its case while there is room, as a case
// costs only its check; where they are full, a case superseded -- by c, by
// now, the shape the site's cache holds, or by another case -- gives c its
// place, and c, superseded itself, takes none: the stale shapes had taken
// the cases, and the code left native code at every read of the live ones,
// compiled again and again for the same stale ones until its compiles ran
// out. Dropped where there was room, a shape that lived on deoptimized
// each of a function's reads of its objects in turn, one compile each --
// Typescript's Binder.bindSymbol, of symbols some of which are never given
// a type, nine times.
func jitAddReadCase(p *jitPolySite, c propCache, now *shape) bool {
	if slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape }) {
		return false
	}
	if len(p.cases) < jitPropertyCases {
		p.cases = append(p.cases, c)
		return true
	}
	stale := func(s *shape) bool {
		return jitSuperseded(s, now) || slices.ContainsFunc(p.cases, func(k propCache) bool { return jitSuperseded(s, k.shape) })
	}
	if stale(c.shape) {
		return false
	}
	i := slices.IndexFunc(p.cases, func(k propCache) bool { return jitSuperseded(k.shape, c.shape) || stale(k.shape) })
	if i < 0 {
		return false
	}
	p.cases = append(slices.Delete(p.cases, i, i+1), c)
	return true
}

// jitSuperseded reports whether t came from s by adding properties: s is
// one of t's ancestors along the shapes' transitions. A nil t has none, and
// neither has a unique one, whose parent field is a prototype's root
// (protoRoot).
func jitSuperseded(s, t *shape) bool {
	for a := t; a != nil && !a.unique; {
		if a = a.parent; a == s {
			return a != nil
		}
	}
	return false
}

// jitCacheSeen is a cache's earlier entries (propPoly.seen), nil until
// the site has met objects of a second shape.
type jitCacheSeen = *propSeen

// seenOf is c's earlier entries (propPoly.seen), or nil.
func (c *propCache) seenOf() *propSeen {
	if c.poly == nil {
		return nil
	}
	return c.poly.seen
}

// setSeen makes s c's earlier entries, its poly made for them if it has
// none.
func (c *propCache) setSeen(s *propSeen) {
	if c.poly == nil {
		c.poly = &propPoly{}
	}
	c.poly.seen = s
}

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
// -- or false. A typed array, whose reads no cache holds, its key no
// numeric one, which names its elements, is read so at any depth: its
// methods, on its type's prototype and %TypedArray%.prototype.
func (r *Runtime) jitDeepReadOf(o *Object, key Atom) (jitDeepRead, bool) {
	typed := o.class == ClassTypedArray && r.jitTypedNamed(o, key)
	if key.IsIndex() || o.shape == nil || o.shape == noShape || !shapeClass(o.class) && !typed || synthesized(o.class, key) || o.findOwn(key) >= 0 {
		return jitDeepRead{}, false
	}
	d := jitDeepRead{shape: o.shape}
	for p := o.proto(); p != nil; p = p.proto() {
		if len(d.up) == jitAddDepth || r.ensureProtoShape(p) == nil || !shapeClass(p.class) || synthesized(p.class, key) {
			return jitDeepRead{}, false
		}
		d.up, d.shapes = append(d.up, p), append(d.shapes, p.shape)
		if i := p.findOwn(key); i >= 0 {
			if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 || len(d.up) <= 2 && !typed {
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

// jitTypedNamed reports whether key names, on o, a typed array, a
// property its prototypes answer: no canonical numeric string, which
// names an element or nothing (typedArrayIndex), nor a symbol of its own.
func (r *Runtime) jitTypedNamed(o *Object, key Atom) bool {
	if key.IsIndex() || r.typedArrayIndex(o, key).numeric {
		return false
	}
	_, ok := o.dataOf().(*typedArrayData)
	return ok
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
	s := c.seenOf()
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
		c.setSeen(s)
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
// the site to make it natively from the first (jitRegSeenFeedback). Before,
// native code learned such additions one exit and one compile at a time:
// a constructor's at each of its sites in turn, for each of its receivers'
// shapes.
func jitNoteDeepAdd(c *propCache, from, next *shape, proto *Object) {
	s := c.seenOf()
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
		c.setSeen(s)
	}
	s.deep = append(s.deep, jitDeepSeen{from: remember(from), next: remember(next), proto: proto})
}

// jitNoteSeen keeps old, what a cache remembered before it remembers n,
// among the entries it has seen, if it was of another shape: replacing
// the one of its shape, or the oldest when they are full.
func jitNoteSeen(p *propPoly, old, n propCache) *propPoly {
	if old.shape == nil || old.shape == noShape || old.shape == n.shape {
		return p
	}
	old.poly = nil
	if p == nil {
		p = &propPoly{}
	}
	s := p.seen
	if s == nil {
		s = &propSeen{}
		p.seen = s
	}
	for i := range s.n {
		if s.entries[i].shape == old.shape {
			s.entries[i] = old
			return p
		}
	}
	if int(s.n) < len(s.entries) {
		s.entries[s.n] = old
		s.n++
		return p
	}
	copy(s.entries[:], s.entries[1:])
	s.entries[len(s.entries)-1] = old
	return p
}

// jitSeenSite is what jitRegSeenFeedback learns in e: the site at pc, its
// cache, a write's or a read's, of key where named.
func (r *Runtime) jitSeenSite(e *jitEntry, pc int, cache *propCache, write bool, key Atom, named bool) {
	if cache.poly == nil {
		return
	}
	seen, now := cache.seenOf(), cache.shape
	// What the site remembers besides its entry (poly), which the
	// interpreter's reads and writes fill, and what it met before that
	// (seen), in that order. A chain's cell is no entry the
	// code is compiled for (megamorphic). Taken where they are: copied
	// into a list of their own, they had been most of what a register
	// compile's asks of its inlined callees allocated.
	if seen != nil {
		for _, c := range seen.entries[:seen.n] {
			jitSeenCase(e, pc, c, write, now)
		}
	}
	if p := cache.poly; p != nil {
		for _, c := range p.entries[:p.n] {
			if c.cell == nil {
				jitSeenCase(e, pc, c, write, now)
			}
		}
	}
	if seen == nil {
		return
	}
	if !write {
		// The reads a cache could not hold, past its two prototypes.
		for _, d := range seen.reads {
			jitAddDeepRead(jitPolySiteAt(e, pc), d)
		}
		return
	}
	if len(seen.deep) == 0 || !named {
		return
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
		if a, ok := r.jitDeepAddFrom(d.from, d.next, d.proto, key); ok {
			p.adds = append(p.adds, a)
		}
	}
}

// jitSeenCase is jitSeenSite's of c, a case the site at pc met, a write's
// or a read's, whose cache holds now.
func jitSeenCase(e *jitEntry, pc int, c propCache, write bool, now *shape) {
	if c.shape == nil || c.shape == noShape || c.getter || c.idx < 0 || write && (c.next == setterNext || c.next == nil && c.p1 != nil) {
		return
	}
	i := slices.IndexFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc })
	if i < 0 {
		e.poly = append(e.poly, jitPolySite{pc: uint32(pc)})
		i = len(e.poly) - 1
	}
	p := &e.poly[i]
	if !write {
		// What the site's objects were before they took the shape its
		// cache remembers now (now) is a case while there is room.
		jitAddReadCase(p, c, now)
		return
	}
	if len(p.cases)+len(p.adds) >= jitPropertyCases || slices.ContainsFunc(p.cases, func(k propCache) bool { return k.shape == c.shape }) {
		return
	}
	p.cases = append(p.cases, c)
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
	// for n elements and none yet -- but for shared's, a literal of
	// constants whose arrays are made whole, sharing them
	// (sharedLiteralArray).
	array  bool
	shared bool
	n      int
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

// jitLiteralRoom bounds the room learned from one array result. Larger
// buffers inherit the interpreter's confirmed length (jitLiteralArrayRoom).
const jitLiteralRoom = 64

// jitObjectLiteral is the object literal at pc of cl's code, of either
// form, made in e, its site's cache cl's at site, room for n fields: native
// code makes its objects from a pool of the shape its site's objects start
// with (shapeTree.siteRoot).
func (r *Runtime) jitObjectLiteral(e *jitEntry, cl *closure, pc int, site uint32, n int) *jitLiteral {
	keySite[struct{}](r, cl, pc, site)
	root := r.shapes.siteRoot(&cl.ic[site], r.proto.object)
	if root == nil {
		return nil
	}
	e.literals = append(e.literals, jitLiteral{pc: int32(pc), root: root, pool: new(abi.ObjectPool), n: n, fn: cl.fn})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(&jitPoolSource{root: root, n: l.n})
	r.jitFillLiteral(l)
	return l
}

// jitArrayLiteral is the array literal at pc of cl's code, of either form,
// of n elements, made in e: native code makes its arrays from a pool of
// arrays with room for them, none yet.
func (r *Runtime) jitArrayLiteral(e *jitEntry, cl *closure, pc, n int) *jitLiteral {
	e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), array: true, n: n, fn: cl.fn})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(jitLiteralArraySource(r, cl, l.pool, pc, n))
	r.jitFillLiteral(l)
	return l
}

// jitRegExpLiteral is the regular expression literal at pc of cl's code,
// of either form, of pattern lit, made in e: native code takes fresh
// RegExps of the pattern, each with its lastIndex 0, from a pool -- what
// the literal makes at each evaluation. A pattern that does not compile
// has Go throw.
func (r *Runtime) jitRegExpLiteral(e *jitEntry, cl *closure, pc int, lit *bytecode.Constant) *jitLiteral {
	if _, err := r.newRegExpLiteral(lit); err != nil {
		return nil
	}
	e.literals = append(e.literals, jitLiteral{pc: int32(pc), pool: new(abi.ObjectPool), lit: lit, fn: cl.fn})
	l := &e.literals[len(e.literals)-1]
	l.pool.Source = unsafe.Pointer(&jitPoolSource{lit: lit})
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
	n := l.n
	src := (*jitPoolSource)(pool.Source)
	if src != nil && src.sharedOf != nil {
		// A literal of constants: its arrays as the interpreter makes them,
		// each whole (sharedShare).
		for i := range int(pool.Size) {
			ic := &src.sharedOf.ic[src.sharedPC]
			a := sharedShare(r, ic, ic.p1)
			r.ensureShape(a)
			pool.Objects[i] = unsafe.Pointer(a)
		}
		pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.array)
		return
	}
	if src != nil {
		if s := src.sample; s != nil && len(s.elemsOf()) > n {
			// The last array the pool gave grew past the literal's
			// elements -- a spread's, an append's -- as the next will:
			// they are made with room for as many, as V8's allocation
			// sites size what their literals come to hold, so that native
			// code spreads into them.
			src.room = max(src.room, min(len(s.elemsOf()), jitLiteralRoom))
		}
		n = jitLiteralArrayRoom[struct{}](pool, n)
	}
	for i := range int(pool.Size) {
		a := newArrayObject(r.proto.array, n)
		a.extOf().elems = a.elemsOf()[:0]
		r.ensureShape(a)
		pool.Objects[i] = unsafe.Pointer(a)
	}
	if src != nil {
		// The pool gives its first last.
		src.sample = (*Object)(pool.Objects[0])
	}
	pool.Count, pool.Proto = pool.Size, unsafe.Pointer(r.proto.array)
}

// TypeofCells is ssa.TypeofFeedback's: the table of the runtime's strings
// typeof answers (jitState's typeofCells), made the first time a compile
// asks for it, as typeofString makes each, which the runtime keeps.
func (fb *jitFeedback) TypeofCells() (uintptr, bool) {
	if fb.r == nil || fb.r.jit == nil {
		return 0, false
	}
	s := fb.r.jit
	if s.typeofCells == nil {
		t := new([8]Value)
		for i := range t {
			t[i] = Str(typeofOfStr[struct{}](fb.r, i))
		}
		s.typeofCells = t
	}
	return uintptr(unsafe.Pointer(s.typeofCells)), true
}

// jitFillClosures fills pool with Size functions of the template tmpl as
// the VM makes them in a frame of fn's, its closures' environment env:
// their captured bindings' cells nil, and an arrow's this, new.target and
// home object those of no frame, which native code writes as it takes one
// (mir's closureTake) -- the cells and the this; an arrow that reads the
// others its pool is not made for (jitFeedback's Closure).
func (r *Runtime) jitFillClosures(pool *abi.ObjectPool, tmpl Value, fn *bytecode.Function, env *Object) {
	f := &frame{cl: &closure{fn: fn, closureCode: &closureCode{env: env}, upvalues: make([]*upvalue, len(fn.Upvalues))}, this: Undefined, newTarget: Undefined}
	cells := make([]*upvalue, jitCaptured(fn))
	for i := range int(pool.Size) {
		pool.Objects[i] = unsafe.Pointer(r.newClosureIn(f, tmpl, cells))
	}
	pool.Count, pool.Proto = pool.Size, nil
}

// reoptPending reports whether e's code is to be compiled again, for any
// of the reasons jitRegReoptimize takes: what it learned at its exits to
// make natively -- a speculation that failed, a shape, a call to inline, a
// callee that inlines more -- or that it leaves too often called natively.
// Code whose guard failed is invalidated (jitRegInvalidate) and compiled
// again on the interpreter's budget (jitRegWarmUp); code that learned
// runs on, and is compiled again once it has spent a budget itself
// (jitRegLearnedDue).
func (e *jitEntry) reoptPending() bool {
	return e.reopt || e.inlineReopt || e.polyReopt
}

// jitLearnedMark is jitFeedbackMark of cl's caches with what e's code has
// learned at its exits counted in: a change of either delays the compile
// for it (jitRegLearnedDue), as V8's NotifyICChanged does.
func jitLearnedMark(cl *closure, e *jitEntry) uint64 {
	return jitFeedbackMark(cl) + uint64(jitLearnedCount(e))*0x9e3779b97f4a7c15
}

// jitLearnedCount is how much e's exits have learned, which grows as they
// learn more.
func jitLearnedCount(e *jitEntry) int {
	return len(e.failed) + len(e.failedEntries) + len(e.resumed) + len(e.bigints) + len(e.bigwide) + len(e.poly) + len(e.typedReads) + len(e.stringReads) +
		len(e.keyedReads) + len(e.keyedWrites) + len(e.strSites) + len(e.numSites) + len(e.boolSites) + len(e.nativeCalls) + len(e.inlines) + len(e.notInline) +
		len(e.literals)
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
		if seen := c.seenOf(); seen != nil {
			for k := range seen.entries[:seen.n] {
				entry(&seen.entries[k])
			}
			m += uint64(len(seen.reads))<<32 + uint64(len(seen.deep))<<40
		}
	}
	return m
}

// JITCompileRepeat, if not 0, has every compile of the SSA backend that
// succeeds run that many times more, its build, optimization and backend,
// what each makes thrown away: for timing the compiler (mir.Timing) over
// the compiles a program makes, each as the program makes it, with
// clocks that read too coarsely for one compile. Their time and the
// collector's cycles they run are left out of the JIT cache's clocks
// (jitRepeatCycles), so that the program's code ages as without them. Set
// it before any runtime compiles; for one runtime at a time.
var JITCompileRepeat int

// jitGCCycles is how many cycles the collector has completed.
func jitGCCycles() uint64 {
	s := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// jitAllocated is what the heap has handed out so far, in bytes and
// objects, for the compiles JITCompileRepeat repeats.
func jitAllocated() (bytes, objects uint64) {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}, {Name: "/gc/heap/allocs:objects"}}
	metrics.Read(s)
	return s[0].Value.Uint64(), s[1].Value.Uint64()
}

// jitRefused is a compile's refusal of a program it does not compile:
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
	// selfCalls are the PCs of the function's calls of itself, once live
	// feedback or a snapshot asks (selfKnown).
	selfCalls []int32
	selfKnown bool
	r         *Runtime
	fn        *bytecode.Function
	cl        *closure
	shapes    []*shape
	// holders are the prototypes the code compares receivers' with
	// (ssa.Holder), held by address, which the entry keeps alive.
	holders []*Object
	cells   []*Value
	// root is the function's feedback for an inlined callee's, which keeps
	// what the callee's sites hold for the code; nil for the function's
	// own. strings marks an inlined callee that calls charCodeAt. links
	// are those of the functions the code calls natively (jitLink).
	root  *jitFeedback
	links []*jitLink
	// inlined are the closures of the callees the code inlines, or that
	// its compile asked to inline, inlinedN how many it inlines (ssa.Func's
	// Inlined); pools the object pools its constructions take from.
	inlined []*closure
	// regSeen, the root's, are the register callees the compile asked to
	// inline whose sites' caches it has taken (jitRegSeenInlined), and
	// regCallees their feedback (jitRegCalleeOf).
	regSeen    []*closure
	regCallees []*jitRegFeedback
	pools      []*abi.ObjectPool
	// made are the templates of the closures the code may not make, the
	// loops' callbacks inlined where their callers make them (ForEachSite's
	// Made), which an exit's frame names in their places (jitRegMaterialize);
	// callbacks and unmade how many the code inlines, and does not make
	// (ssa.Func's Callbacks and Unmade).
	made              []*closure
	callbacks, unmade int
	// called is how many closures the code makes that it only calls and
	// does not make (ssa.Func's Called).
	called int
	// tables are the entries of the function tables typed code calls
	// through by index (regTableEntries), which native code reads.
	tables [][]abi.TableEntry
	// linked are the callees it calls through their links before they
	// have code, compiled after it, which native code then calls, the
	// caller's code as it is.
	linked []*closure
	// keeps is how many keep cells the code uses (ssa.Func.Keeps), and
	// keepLimit how many the build may number (ssa.KeepFeedback), or 0.
	keepLimit int
	// e is the entry being compiled again, whose failed speculations
	// (jitEntry.failed) are built generic; nil for a first compile.
	e *jitEntry
	// prog is the function lowered for inlining, its stack depths what
	// Intrinsic reads, made on first use.
	prog *ir.Program
	// virtualRoots are VirtualRoot's answers, by pool, held until the
	// compile keeps those escape analysis took (keepVirtuals).
	virtualRoots map[uintptr]jitVirtualRoot
}

// mathIntrinsic is the intrinsic a call of o, of n arguments, a plain
// call's where plain says, makes, of either form's code: the realm's Math
// functions of numbers, each made natively, as V8's code makes them:
// Math.pow where exact, and by Go called from there otherwise; Math.max
// and Math.min of any number of them; a rounding, the sign, fround, sqrt,
// abs, imul and clz32 by instructions of their own; every other by Go
// called from there, the VM's very function.
func (fb *jitFeedback) mathIntrinsic(o *Object, n int, plain bool) (ssa.Intrinsic, bool) {
	k := ssa.Intrinsic{Callee: uintptr(unsafe.Pointer(o)), Plain: plain}
	fd := o.fn()
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
	case fd.mathOp == mathImul && n == 2:
		k.Op = ssa.OpMulI32
	case fd.mathOp == mathClz32 && n == 1:
		k.Op = ssa.OpClz32
	case fd.mathOp != 0 && int(fd.mathOp) < len(unaryMath) && n == 1:
		k.Op = jitUnaryMath(unaryMathName(fd.mathOp))
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
// with, named name: an instruction of its own -- the roundings SSE4.1's
// or, without it, SSE2's (mir's roundSD) -- or a call of Go
// (ssa.OpMathCall).
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
	return ssa.OpMathCall
}

// instanceOfSite is InstanceOf's site of an instanceof of the constructor
// ctor, the value the code reads before it, as either form's code finds it.
func (fb *jitFeedback) instanceOfSite(ctor *Object) (ssa.InstanceOfSite, bool) {
	r := fb.r
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
	if hv := h.vals()[hc.idx]; !hv.IsObject() || hv.Object() != r.hasInstanceFn {
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

// jitInline is a call jitRegCallSeen has seen call a function it may
// inline: the call's PC, the function, and how often native code has left
// the inlined callee for Go there.
type jitInline struct {
	pc    int32
	cl    *closure
	obj   *Object
	exits uint32
	// pool, for a construction made natively, is where its objects come
	// from (abi.ObjectPool); via marks a call of Function.prototype.call
	// that calls cl's function (jitRegViaSeen).
	pool *abi.ObjectPool
	via  bool
	// forEach marks a call of Array.prototype.forEach, or another of
	// jitLoops, on an array (jitRegForEachSeen), cb the closure of the callback
	// it was given and cbObj its function object.
	forEach bool
	cb      *closure
	cbObj   *Object
	// cbLeft marks a loop whose callback, inlined in it, left native code
	// too often (jitInlineLeft): called natively since.
	cbLeft bool
	// proto is a typed array's loop's typed arrays' prototype
	// (jitLoopTyped), nil for an array's.
	proto *Object
	// byCode marks a function the call met two objects of, closures its
	// maker makes anew at each call: called by its code (jitRegSameCode).
	byCode bool
	// parent, for a construction with a class whose constructor is the
	// one a class without one has (jitRegDerivedBase), is the class it
	// extends, whose constructor cl is: the call checks obj's prototype is
	// still it (ssa.CallSite's Parent).
	parent *Object
	// active, for a super() native code makes, is the class whose
	// constructor's super() it is, whose prototype obj is: the parent,
	// which cl's code -- obj's, or, obj a class with no constructor of its
	// own, parent's -- constructs with (jitRegSuperTarget).
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

// What jitRegCallSeen knows of a call (jitEntry.callSites), past its count:
// that it waits for its callee, too long to inline, to be compiled
// (jitRegCallSeen), that it is made natively, that it is neither that nor
// inlined, or that it is inlined.
const (
	jitCallWaiting = 252
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

// jitTarget is a call's target, at pc: the function object, and, for one
// called by its code (jitInline's byCode), its closure.
type jitTarget struct {
	pc     int32
	byCode bool
	obj    *Object
	cl     *closure
}

// jitTargetsOf is what e's calls are decided to make, natively or inlined,
// as a compile from now would be given them (jitEntry's ssaTargets).
func jitTargetsOf(e *jitEntry) []jitTarget {
	if e == nil || len(e.nativeCalls)+len(e.inlines) == 0 {
		return nil
	}
	t := make([]jitTarget, 0, len(e.nativeCalls)+len(e.inlines))
	for _, xs := range [2][]jitInline{e.nativeCalls, e.inlines} {
		for _, x := range xs {
			t = append(t, jitTarget{pc: x.pc, byCode: x.byCode, obj: x.obj, cl: x.cl})
		}
	}
	return t
}

// jitNativeCallers is how many callers' entries an entry tells when it
// inlines more (jitEntry.nativeCallers).
const jitNativeCallers = 8

func init() {
	jit.HostCall = jitHostCall
	jitStackHighLowered = jitForgetHighSeen
}

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
	if r.gcYield.on && r.gcMarking() {
		r.gcYieldNow()
	}
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoLeaf && r.jitTest.goCallWork == nil {
		// A leaf's call, its function checked by the code (jitLeafCall).
		return r.jitLeafCall(ctx)
	}
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoCall && r.jitTest.goCallWork == nil {
		// The most frequent, asked first; one that runs nothing made
		// without a frame.
		return r.jitMaybePlain(ctx) && r.jitGoCallPlain(ctx) || r.jitGoCall(ctx)
	}
	if w := r.jitTest.goCallWork; w != nil {
		w()
	}
	switch ssa.GoOp(ctx.GoOp & 0xff) {
	case ssa.GoHost, ssa.GoArgument, ssa.GoCells, ssa.GoClosure, ssa.GoClose:
		if r.jitRegIsFrameOp(ctx) {
			// Register code's, whose frame is its window, not the stack
			// form's frame these read (jit_frame.go).
			return r.jitRegFrameOp(ctx)
		}
	}
	switch ssa.GoOp(ctx.GoOp & 0xff) {
	case ssa.GoLeaf:
		return r.jitLeafCall(ctx)
	case ssa.GoAdd:
		return r.jitGoAdd(ctx)
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
		return r.jitGoAppend(ctx)
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
		if n == 1 {
			// A template's part converted (to_string), as the interpreter
			// converts it: a string is itself, which a join had copied.
			p := parts[0]
			if p.IsNumber() {
				p = Str(r.numberString(p.num))
			}
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = p
			return true
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
	case ssa.GoIterResult:
		// A call of next's result object (mir's genNextCall), of the value
		// and the done; its pool, the code's, filled again if it ran out.
		if pool := *(**abi.ObjectPool)(unsafe.Pointer(&ctx.GoArgs[2].Ref)); pool != nil && pool.Count == 0 {
			r.jitRefillPool(pool)
		}
		// A boolean's word, which a NaN's comparison would not tell.
		done := ctx.GoArgs[1].Num == math.Float64bits(True.num)
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(r.iterResult(jitGoArg(ctx, 0), done))
		return true
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
		// captured ones: a native callee's, which its context stands for
		// (jitCalleeFrame).
		f := r.jitCalleeFrame(ctx)
		if f == nil {
			return false
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
			if i >= len(f.cl.consts) || !jitCalleeMakes(f, f.cl.consts[i]) {
				return false
			}
			v = Obj(r.makeClosure(f, f.cl.consts[i]))
		} else if len(f.openUpvalues) != 0 {
			r.closeUpvaluesFrom(f, i)
		}
		r.jit.calleeOpen[ctx.Level] = f.openUpvalues
		ctx.Open = uint64(len(f.openUpvalues))
		if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
			// A close's writes none, so that one made with every cell
			// taken (ssa's goFrame) shares one.
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		}
		return true
	case ssa.GoNewArray:
		form := int(ctx.GoOp >> abi.GoOpIndexShift)
		if form&ssa.NewArraySort != 0 {
			v, ok := r.jitSortCopy(form, jitGoArg(ctx, 0), jitGoArg(ctx, 1))
			if ok {
				*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
			}
			return ok
		}
		if form&ssa.NewArrayTyped != 0 {
			v, ok := r.jitNewTypedResult(form, jitGoArg(ctx, 0), jitGoArg(ctx, 1))
			if ok {
				*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
			}
			return ok
		}
		// map's or filter's array, of no length, room for the array's in
		// map's: where the array's species is Array's (plainSpecies), as
		// their ArraySpeciesCreate would make it.
		o := r.plainArray(jitGoArg(ctx, 0))
		n := jitGoArg(ctx, 1)
		if o == nil || !n.IsNumber() || !r.plainSpecies(o) {
			return false
		}
		room := int64(0)
		if form&ssa.NewArrayRoom != 0 {
			room = int64(n.Number())
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Obj(r.newArrayOfLength(room))
		return true
	case ssa.GoElemCell:
		o := *(**Object)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		return o != nil && jitElemAbsent(o, math.Float64frombits(ctx.GoArgs[1].Num))
	case ssa.GoStringAbsent:
		s := *(**String)(unsafe.Pointer(&ctx.GoArgs[0].Ref))
		return s != nil && r.jitStringAbsent(s, math.Float64frombits(ctx.GoArgs[1].Num))
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
	case ssa.GoHost:
		return false
	case ssa.GoArgument:
		v, ok := r.jitArgument(ctx)
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
		v := jitGoArg(ctx, 2)
		if o == nil || !jitStoreElem(o, math.Float64frombits(ctx.GoArgs[1].Num), v) {
			return false
		}
		if v.IsBigInt() {
			r.jitBigStoreSeen(ctx, o, *(**Value)(unsafe.Pointer(&ctx.GoArgs[3].Ref)))
		}
		return true
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
	case ssa.GoTypedSeen:
		return r.jitTypedSeen(ctx)
	case ssa.GoTypedView:
		return r.jitTypedView(ctx)
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
		if idx := int(ctx.Level); ctx.Live == 0 && idx < len(s.regRuns) && s.regRuns[idx].f != nil && s.ssaCtxs[idx] == ctx {
			if e := s.regRuns[idx].e; e != nil && e.life != nil && e.life.reopt && !e.invalid {
				// Register code that learned what to be compiled again for
				// (jitRegReopt), at a call of Go its loop goes on after: the
				// loop leaves at its header (abi.ExitPoll), where its work is
				// counted toward the code's budget and the code compiled again
				// once that is due, the loop going on in the new code
				// (jitRegLeft), as V8's back edges spend its interrupt
				// budget. Never leaving, a loop that runs a whole invocation
				// natively had made each construction it learned by Go to the
				// end.
				return false
			}
		}
		old := r.backEdges
		r.backEdges = s.backEdgeBudget()
		s.pollCredit += r.backEdges - old
		s.budgets++
		return r.checkInterruptPoll() == nil
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

// jitGoRun is what a run of native code in a context has pinned
// (jitState's goRuns).
type jitGoRun struct {
	pinned bool
	// pinnedAt is where its pins start in goPinned: its own code, then
	// that of the native callees whose calls of Go it is suspended in
	// (jitRegGoCallNested), all released when its native code leaves.
	pinnedAt int
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

// jitGoPending is a call Go made from native code and left native code
// after (jitGoCall): at its exit, Go puts its result, or throws. ctx is the
// level of the code that made it: a run's own, or a native callee's
// (jitRegGoCallNested), which Go finishes at its exit (jitRegLeft).
type jitGoPending struct {
	set bool
	pc  int
	ctx int
	v   Value
	err error
	// whole marks an instruction Go ran whole (jitRegWhole), which goes on
	// at register pc next, v written to register reg, if not -1.
	whole     bool
	next, reg int
	// step marks v and err as what a loop's step's iterator's next, which
	// native code called (regStepSites), returned or threw: the step's
	// answer is made of them (jitRegStepCalled) once the frame is written.
	step bool
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
	if code != nil && (len(s.lazy) != 0 || slices.Contains(s.goPinned, code)) {
		if len(s.lazy) != 0 {
			s.lazyHeld++
		}
		s.retired = append(s.retired, jitRetired{code, keep})
		return nil
	}
	return code.Close()
}

// jitGoCall makes, for native code, the call at its exit PC, from the
// operands it passed (ssa.GoCall), as an exit there would have Go make it:
// the operands past the VM's stack's top, which the call
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
	if jitGoInline(ctx) {
		// Inside an inlined callee, which has no frame for what Go would
		// run, nor its PC for what it would learn: the exit makes its
		// frame, and the interpreter the call.
		return false
	}
	if ctx.Live == abi.LiveCall {
		if len(s.regRuns) != 0 {
			if root := r.jitRegRootOf(idx); root >= 0 {
				// A native callee's of register code (jit_call.go):
				// an operation that runs nothing made without the levels'
				// frames (jitRegPlainWhole).
				if cl := (*closure)(ctx.Closure); cl != nil && r.jitRegPlainWhole(ctx, cl, cl.reg) {
					return true
				}
				return r.jitRegGoCallNested(ctx, idx, root)
			}
		}
		return false
	}
	if ctx.Live == 0 && idx < len(s.regRuns) && s.regRuns[idx].f != nil && s.ssaCtxs[idx] == ctx {
		// On a register frame (jit_native.go).
		return r.jitRegGoCall(ctx, idx)
	}
	return false
}

// jitCalleeFrame is a frame for the native callee running in ctx, which
// has none of the VM's, to make a closure on or close what closures
// captured (ssa.GoClosure, GoClose): its closure, its locals where its
// context has them, its receiver, and the upvalues its closures have
// captured of those locals, which its context keeps (calleeOpen) -- none
// once its context's Open says so, as a native call clears it. Its code
// closes them as it returns, and Go gives them to the frame it makes of
// the callee if it finishes it (jitRegUnwind). Or nil.
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
// context's, as V8's code allocates a function's context. For a native
// callee, whose frame Go does not keep, each is a cell of its own,
// closed, holding its parameter's argument or undefined, as the frame's
// slot is at the call: what a closure captures, and native code reads and
// assigns, from then on, and what Go puts in the frame it makes if the
// callee leaves native code (jitRegReopenCells).
func (r *Runtime) jitMakeCells(ctx *abi.Context) bool {
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

// jitCellsClosure is GoClosure and GoClose for code whose captured locals
// are in cells of their own (abi.Context's Cells), on f, a native callee's
// stand-in (jitCalleeFrame): a closure, its constant i, capturing the
// cells; or a scope's end, from local i on, whose captured locals take
// fresh cells holding what they hold -- the closures that captured the old
// ones keep them, as closing an upvalue leaves them, and a loop's next
// iteration's capture new ones.
func (r *Runtime) jitCellsClosure(ctx *abi.Context, f *frame, i int) bool {
	fn := f.cl.fn
	cells := jitCellsOf(ctx, fn)
	v := Undefined
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
		if i >= len(f.cl.consts) || !jitCalleeMakes(f, f.cl.consts[i]) {
			return false
		}
		o := r.newClosureIn(f, f.cl.consts[i], cells)
		if fd := o.fn(); fd != nil && atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
			planCall(fd)
		}
		v = Obj(o)
	} else {
		k := 0
		for slot, l := range fn.Locals {
			if !l.Captured {
				continue
			}
			if slot >= i {
				u := &upvalue{closed: *cells[k].slot}
				u.slot = &u.closed
				cells[k] = u
			}
			k++
		}
	}
	if ssa.GoOp(ctx.GoOp&0xff) == ssa.GoClosure {
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	}
	return true
}

// jitNeedsMaker reports whether the function template c makes is an arrow
// that reads, or whose arrows read, its maker's new.target or super, or
// has a direct eval, which may read either (bytecode.Function's
// ReadsMaker), which a native callee's context does not have
// (jitCalleeFrame).
func jitNeedsMaker(c Value) bool {
	t := c.template()
	return t != nil && t.fn.ReadsMaker
}

// jitCalleeMakes reports whether Go may make the closure c on f, the
// stand-in for a native callee's frame (jitCalleeFrame), which has no
// function object, new.target or this binding of its own: not an arrow
// that reads its maker's new.target or super (jitNeedsMaker), nor one a
// derived constructor makes before its super() binds its this. The arrow
// shares that binding, which super() initializes later; once it has, the
// binding is fixed -- a second super() throws -- and the arrow captures
// its value, as one made after super() in a frame of the VM's reads it
// from the binding it shares. Refused, the operation leaves native code,
// and the frame Go makes of the callee, its this unbound, makes it.
func jitCalleeMakes(f *frame, c Value) bool {
	if jitNeedsMaker(c) {
		return false
	}
	t := c.template()
	return t == nil || t.fn.Kind != bytecode.KindArrow || f.cl.fn.Kind != bytecode.KindDerivedConstructor || !f.this.IsUninitialized()
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
		// leaves for Go. A BigInt typed array's BigInt, its low 64 bits.
		if v.IsBigInt() {
			return jitStoreBigElem(o, i, v)
		}
		return typedWrite(o, i, v)
	}
	if uint(i) < uint(len(o.elemsOf())) && !isHole(o.elemsOf()[i]) {
		o.ownElems()
		o.extOf().elems[i] = v
		return true
	}
	const want = objExtensible | objArrayLengthWritable
	if o.class != ClassArray || o.flags&(want|objHasSparseElements) != want || !o.noIndexKeys() {
		return false
	}
	for p := o.proto(); p != nil; p = p.proto() {
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
// not (ssa.GoTypedRead): a Float16 element, or one past what the view's
// copy of its length has where another agent has grown its shared
// buffer. An element that is not a number -- a BigInt, an index the view
// does not have -- it leaves for an exit.
func jitTypedRead(o *Object, n float64) (float64, bool) {
	t, i, ok := typedElemIndex(o, n)
	if !ok {
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
	for p := o; p != nil; p = p.proto() {
		if p.class != ClassObject && p.class != ClassArray || p.flags&(objHasSparseElements|objMappedArguments) != 0 || !p.noIndexKeys() {
			return false
		}
		if p != o && len(p.elemsOf()) != 0 {
			return false
		}
	}
	return true
}

// jitStringAbsent reports whether s[n] is undefined with nothing to ask
// (ssa.GoStringAbsent): n an index at or past s's end, which the string
// has no character at, and neither String.prototype -- the realm's, a
// wrapper of the empty string -- nor anything up its chain, each an
// ordinary object or array, has an element or an index of its own, as
// jitElemAbsent asks of an array's chain. As V8 loads an index past a
// string's end in its code once its feedback has met one, under its
// NoElementsProtector: a tokenizer's read one past its text's end,
// PDF.js's Type1 header's, had left native code at each.
func (r *Runtime) jitStringAbsent(s *String, n float64) bool {
	i := uint32(n)
	if float64(i) != n || i == math.MaxUint32 || int64(i) < int64(s.length) {
		return false
	}
	p := r.proto.str
	if p == nil || p.flags&(objHasSparseElements|objMappedArguments) != 0 || len(p.elemsOf()) != 0 || !p.noIndexKeys() {
		return false
	}
	return p.proto() == nil || jitElemAbsent(p.proto(), n)
}

// jitCloseRetired closes the retired code no call of Go is suspended in.
func (r *Runtime) jitCloseRetired() {
	s := r.jit
	if len(s.lazy) != 0 {
		// Native levels whose frames a call of Go put off, whose code is
		// not pinned (jitLazyLevels): until they go on, none is closed.
		return
	}
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

// jitConcatAdd is native code's + (ssa.GoAdd) of two strings, or of a
// string and a number of an integer's string, as add makes it -- the
// concatenation, without the conversions and checks add makes first for
// anything else -- and false for any other, or a string too long, which
// add's path then has.
func (r *Runtime) jitConcatAdd(a, b Value) (Value, bool) {
	switch {
	case a.IsString() && b.IsString():
		sa, sb := a.String(), b.String()
		if sa.length+sb.length > maxStringLength-64 {
			return Undefined, false
		}
		// What concat does where no meter counts.
		return Str(sa.Concat(sb)), true
	case a.IsString() && b.IsNumber():
		if s, ok := r.concatInt(a.String(), b.num, false); ok {
			return Str(s), true
		}
	case b.IsString() && a.IsNumber():
		if s, ok := r.concatInt(b.String(), a.num, true); ok {
			return Str(s), true
		}
	}
	return Undefined, false
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

// ArrayProtos is ssa.AppendFeedback's: the realm's Array.prototype and
// Object.prototype, as a push the code makes natively checks them, where
// neither has an index in its table (jitStoreElem's walk up the chain,
// which native code's checks of their shapes stand for).
func (fb *jitFeedback) ArrayProtos() ([2]ssa.Holder, bool) {
	ap, op := fb.r.proto.array, fb.r.proto.object
	if ap.shape == nil || op.shape == nil || ap.shape.loose || op.shape.loose || ap.proto() != op || op.proto() != nil ||
		!ap.noIndexKeys() || !op.noIndexKeys() {
		return [2]ssa.Holder{}, false
	}
	k := fb.keep()
	k.shapes = append(k.shapes, remember(ap.shape), remember(op.shape))
	k.holders = append(k.holders, ap, op)
	return [2]ssa.Holder{{Object: uintptr(unsafe.Pointer(ap)), Shape: uintptr(unsafe.Pointer(ap.shape))},
		{Object: uintptr(unsafe.Pointer(op)), Shape: uintptr(unsafe.Pointer(op.shape))}}, true
}

// HasCache is ssa.HasCacheFeedback's: the realm's hasCache, made if it has
// none, which it keeps for as long as it lives, and so for as long as any
// code of its does.
func (fb *jitFeedback) HasCache() uintptr {
	if fb.r.has == nil {
		fb.r.has = new(hasCache)
	}
	return uintptr(unsafe.Pointer(fb.r.has))
}

// KeyAtoms is ssa.KeyAtomsFeedback's: the runtime's keyAtoms, and its
// intStrings, made if it has none, both of which it keeps for as long as it
// lives, and so for as long as any code of its does.
func (fb *jitFeedback) KeyAtoms() (uintptr, uintptr) {
	r := fb.r
	if r.intStrings == nil {
		r.intStrings = new([1024]*String)
	}
	return uintptr(unsafe.Pointer(&r.keyAtoms)), uintptr(unsafe.Pointer(r.intStrings))
}

// keyAtoms' and intStrings' layouts are what native code reads and writes
// (abi.KeyAtomSize).
var _ [0]struct{} = [unsafe.Sizeof(keyAtom{}) - abi.KeyAtomSize]struct{}{}
var _ [0]struct{} = [abi.KeyAtomSize - unsafe.Sizeof(keyAtom{})]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(keyAtom{}.s) - abi.KeyAtomString]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(keyAtom{}.atom) - abi.KeyAtomAtom]struct{}{}
var _ [0]struct{} = [abi.KeyAtomAtom - unsafe.Offsetof(keyAtom{}.atom)]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(keyAtom{}.none) - abi.KeyAtomNone]struct{}{}
var _ [0]struct{} = [abi.KeyAtomNone - unsafe.Offsetof(keyAtom{}.none)]struct{}{}
var _ [0]struct{} = [len(Runtime{}.keyAtoms) - 1<<abi.KeyAtomsBits]struct{}{}
var _ [0]struct{} = [1<<abi.KeyAtomsBits - len(Runtime{}.keyAtoms)]struct{}{}
var _ [0]struct{} = [keySlotMul - abi.KeyAtomsMul]struct{}{}
var _ [0]struct{} = [abi.KeyAtomsMul - keySlotMul]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(keyAtom{}.next) - abi.KeyAtomNext]struct{}{}
var _ [0]struct{} = [abi.KeyAtomNext - unsafe.Offsetof(keyAtom{}.next)]struct{}{}
var _ [0]struct{} = [keyAddrShift - abi.KeyAtomsAddrShift]struct{}{}
var _ [0]struct{} = [abi.KeyAtomsAddrShift - keyAddrShift]struct{}{}
var _ [0]struct{} = [keyWaysLog - abi.KeyAtomsWaysLog]struct{}{}
var _ [0]struct{} = [abi.KeyAtomsWaysLog - keyWaysLog]struct{}{}
var _ [0]struct{} = [keyAtomsBits - abi.KeyAtomsBits]struct{}{}
var _ [0]struct{} = [abi.KeyAtomsBits - keyAtomsBits]struct{}{}
var _ [0]struct{} = [atomIndexTag - abi.AtomIndexTag]struct{}{}
var _ [0]struct{} = [abi.AtomIndexTag - atomIndexTag]struct{}{}
var _ [0]struct{} = [len(*Runtime{}.intStrings) - abi.IntStrings]struct{}{}
var _ [0]struct{} = [abi.IntStrings - len(*Runtime{}.intStrings)]struct{}{}

// hasCache's layout is what native code reads (abi.HasEntrySize).
var _ [0]struct{} = [unsafe.Sizeof(hasEntry{}) - abi.HasEntrySize]struct{}{}
var _ [0]struct{} = [abi.HasEntrySize - unsafe.Sizeof(hasEntry{})]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(hasEntry{}.key) - abi.HasEntryKey]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(hasEntry{}.shape) - abi.HasEntryShape]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(hasEntry{}.proto) - abi.HasEntryProto]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(hasEntry{}.cell) - abi.HasEntryCell]struct{}{}
var _ [0]struct{} = [unsafe.Offsetof(hasEntry{}.has) - abi.HasEntryHas]struct{}{}
var _ [0]struct{} = [hasCacheBits - abi.HasCacheBits]struct{}{}
var _ [0]struct{} = [abi.HasCacheBits - hasCacheBits]struct{}{}
var _ [0]struct{} = [hasCacheMul - abi.HasCacheMul]struct{}{}
var _ [0]struct{} = [abi.HasCacheMul - hasCacheMul]struct{}{}
var _ [0]struct{} = [hasCacheWaysLog - abi.HasCacheWaysLog]struct{}{}
var _ [0]struct{} = [abi.HasCacheWaysLog - hasCacheWaysLog]struct{}{}

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

// jitInstr is an operation a call of Go makes (jitGoCallInstr): an
// opcode of the shared vocabulary and its operands, as op.go documents
// them for the opcode.
type jitInstr struct {
	Op bytecode.Op
	A  uint32
	B  uint32
}

// jitGoCallInstr is the call a call of Go makes (abi.Context's GoOp, which
// native code writes): its operation, and how many operands it takes from
// the stack.
func jitGoCallInstr(op uint64) (jitInstr, int) {
	argc := uint32(op>>abi.GoOpArgcShift) & 0xff
	in := jitInstr{Op: bytecode.OpCall, A: argc}
	n := int(argc) + 1
	switch op >> abi.GoOpKindShift & 0xff {
	case abi.GoCallMethod:
		in.Op = bytecode.OpCallMethod
		n++
	case abi.GoCallNew:
		in.Op = bytecode.OpNew
	case abi.GoCallStep:
		// A loop's step, its cursor the one operand.
		return jitInstr{Op: bytecode.OpIterNextOrJump}, 1
	case abi.GoCallFill:
		// A destructuring's step, its cursor the one operand.
		return jitInstr{Op: bytecode.OpIterStep}, 1
	case abi.GoCallForOf:
		// A cursor made, of the iterable, the one operand.
		return jitInstr{Op: bytecode.OpForOfStart}, 1
	case abi.GoCallForIn:
		// A for...in's cursor made, of the object, the one operand.
		return jitInstr{Op: bytecode.OpForInStart}, 1
	case abi.GoCallRest:
		// A destructuring's rest element, of the cursor, the one operand.
		return jitInstr{Op: bytecode.OpIterRest}, 1
	case abi.GoCallGet:
		// A property's read, of the object, which the instruction at the
		// call's PC says (jitRegPropertyMade).
		return jitInstr{Op: bytecode.OpGetProp, A: uint32(op >> abi.GoOpIndexShift)}, 1
	case abi.GoCallSet:
		// A property's write, of the object and the value.
		return jitInstr{Op: bytecode.OpSetProp, A: uint32(op >> abi.GoOpIndexShift)}, 2
	case abi.GoCallTypeOf:
		return jitInstr{Op: bytecode.OpTypeOf}, 1
	case abi.GoCallInstanceOf:
		return jitInstr{Op: bytecode.OpInstanceOf}, 2
	case abi.GoCallSuper:
		// The frame's this slot, then the arguments.
		return jitInstr{Op: bytecode.OpSuperCall, A: argc}, int(argc) + 1
	case abi.GoCallForward:
		// f.apply(x, arguments), of f, apply and x (ssa.CallSite's
		// Forward): the frame's arguments are f's.
		return jitInstr{Op: bytecode.OpApplyArguments}, 3
	case abi.GoCallOp:
		// An operation's opcode and its A, of its operands: the
		// instruction at the call's PC.
		return jitInstr{Op: bytecode.Op(op >> abi.GoOpIndexShift), A: uint32(op >> (abi.GoOpIndexShift + 8))}, int(argc)
	}
	return in, n
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
	for i := range e.nativeCalls {
		// A loop's callback inlined in it (jitFeedback's callbackInline),
		// called natively once it has left too often.
		if x := &e.nativeCalls[i]; int(x.pc) == pc && x.forEach && !x.cbLeft {
			if x.exits++; x.exits >= jitInlineExits {
				x.cbLeft, e.inlineReopt = true, true
			}
		}
	}
	for i := range e.inlines {
		if x := &e.inlines[i]; int(x.pc) == pc && !slices.Contains(e.notInline, x.pc) {
			if x.exits++; x.exits >= jitInlineExits {
				e.notInline = append(e.notInline, x.pc)
				if e.callSites != nil {
					e.callSites[pc] = jitCallDone
					if x.obj != nil && x.cl.reg != nil {
						// A construction's with its pool. A callee inlined
						// by its closure alone, as register code's inlined
						// levels are learned, has no object to be called
						// natively by: its call is the generic one, which
						// regCallTargets had met as a native call of no
						// object and crashed on.
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

// jitCallee is a function object a call calls, and its closure.
type jitCallee struct {
	o  *Object
	cl *closure
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

// jitCallableClosure is o and its closure, if a call in cl's code may make
// it natively or inline it, as jitRegCalled has a callee; or nil.
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

// jitChainReads is how many reads a chain from a global follows: the global
// and the properties after it, Flog.RayTracer.Vector's three, and the five
// of Flog.RayTracer.Vector.prototype.subtract, a method's, with one to
// spare.
const jitChainReads = 6

// jitGlobalValue is the value of the global name, as a read of it in cl's
// code finds it now: the global object's data property of the name -- or
// the script's lexical binding that shadows it, in the realm's global
// scope -- looked up, not through the read's cache, which native code
// that reads it never fills.
func (r *Runtime) jitGlobalValue(cl *closure, name Atom) (Value, bool) {
	h := cl.scope()
	if h != nil && r.globalLex.valn != 0 && r.lexShadows(name) {
		if !r.isGlobalScope(h) {
			return Undefined, false
		}
		h = r.globalLex
	}
	if h == nil {
		return Undefined, false
	}
	prop := h.getOwnVisible(name)
	if !prop.ok() || prop.flags()&propAccessor != 0 || prop.value().IsUninitialized() {
		return Undefined, false
	}
	return prop.value(), true
}

// jitKeyedClass reports whether o's elements by a number Go reads from
// native code where an array's are not (ssa.GoIndex, jitKeyedRead): an
// ordinary object's, and an arguments object's that aliases no parameter
// -- EarleyBoyer's sc_list reads `var a = arguments` so, a[i], which had
// left native code at each read.
func jitKeyedClass(o *Object) bool {
	return o.class == ClassObject || o.class == ClassArguments && o.flags&objMappedArguments == 0
}

// jitKeyedRead is o[k] where reading it runs nothing (ssa.GoIndex): an
// object's data property by a string key, which the realm's shared
// property cache finds -- no getter, no proxy, no class that answers keys
// itself -- or a dense array's element, or a typed array's, by an
// integer, or an index an ordinary object's table holds or nothing up its
// chain does (jitTableRead); and false for any other, which Go reads
// after an exit.
func (r *Runtime) jitKeyedRead(o, k Value) (Value, bool) {
	if !o.IsObject() {
		return Undefined, false
	}
	obj := o.Object()
	if k.IsNumber() {
		i := uint32(k.Number())
		if float64(i) == k.Number() && uint(i) < uint(len(obj.elemsOf())) && obj.flags&objMappedArguments == 0 {
			if v := obj.elemsOf()[i]; !isHole(v) {
				return v, true
			}
		}
		if t, i, ok := typedElemIndex(obj, k.num); ok {
			return t.getElem(i), true
		}
		if float64(i) != k.Number() || i >= atomIndexTag || obj.class == ClassTypedArray {
			return Undefined, false
		}
		// An index the elements do not hold: a property of an ordinary
		// object's table, as an object used as a map by numbers keeps its
		// keys -- Typescript's caches of type comparisons, by
		// (a.typeID << 16) | b.typeID, dictionaries, which had left native
		// code at every read, the interpreter finishing the invocation --
		// or of none, undefined.
		return r.jitTableRead(obj, Atom(atomIndexTag|i))
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
	// One the shared cache does not keep: a dictionary's own.
	return r.jitTableRead(obj, a)
}

// jitTableRead is o's property of key a where its chain's tables say what
// it is with nothing run: the plain data property of the nearest object
// whose table, or, for an index, whose elements, have it, or undefined
// where none does -- each object ordinary, its properties its table's and
// none answering the key itself (shapeClass, synthesized), and for an
// index an ordinary object or an array, which have no element they do not
// store (noneIndexed). The global object, whose bindings Go keeps beside
// its table, is not read so. It reports false for any other.
func (r *Runtime) jitTableRead(o *Object, a Atom) (Value, bool) {
	for h, depth := o, 0; h != nil; h, depth = h.proto(), depth+1 {
		if depth > maxChainDepth || !shapeClass(h.class) || h == r.global || r.ensureShape(h) == nil {
			return Undefined, false
		}
		if a.IsIndex() {
			if h.class != ClassObject && h.class != ClassArray {
				return Undefined, false
			}
			if i := a.Index(); uint(i) < uint(len(h.elemsOf())) && !isHole(h.elemsOf()[i]) {
				return h.elemsOf()[i], true
			}
		} else if synthesized(h.class, a) {
			return Undefined, false
		}
		if j := h.findOwn(a); j >= 0 {
			if h.shape.keys[j].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
				return Undefined, false
			}
			return h.vals()[j], true
		}
	}
	return Undefined, true
}

// jitHas is key in obj, as the interpreter answers it, where answering it
// runs nothing and throws nothing (ssa.GoHas): an object none of whose
// chain is a proxy, whose has trap would run, or a module namespace, which
// a key may make evaluate a deferred module, by a key that is no object,
// whose conversion would call its toString or Symbol.toPrimitive. Any
// other -- a right operand that is no object, which throws, included -- it
// refuses (ok false), and Go makes it after an exit.
func (r *Runtime) jitHas(key, obj Value) (has, ok bool) {
	if !obj.IsObject() || key.IsObject() {
		return false, false
	}
	o := obj.Object()
	if o.flags&objPolyProto != 0 {
		// An object that keeps its prototype in its table, whose layout
		// says none, matches no entry native code's probe of hasCache
		// reads (mir's keyedHas reads the layout's prototype): asked again
		// by a key hasCache has, it is given a layout of its own holding
		// its prototype, as the interpreter's in gives it (hasCached), which
		// the entry hasNote then makes for it matches. Without this a loop
		// asking of such an object called here at every turn.
		if has, ok := hasCached(r, key, o); ok {
			return has, true
		}
	}
	if has, ok := hasQuick(r, key, o); ok {
		// Plain objects and arrays, which hold what they have.
		return has, true
	}
	for p, depth := o, 0; p != nil; p, depth = p.proto(), depth+1 {
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

// jitDelete is delete obj[key], as the interpreter makes it (regDelete:
// toPropertyName, keyFor, deleteProp), where making it runs
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
// false), and Go makes it after an exit. So does a property
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
	for h, depth := o, 0; h != nil; h, depth = h.proto(), depth+1 {
		if depth > maxChainDepth || !shapeClass(h.class) || named && (synthesized(h.class, a) || h.findOwn(a) >= 0) {
			return false
		}
	}
	return true
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

// jitGlobalEpochs is the most changes of the global object's bindings
// (shapeTree's globalEpoch) after which a global's read is still compiled
// to check the count alone: a program that keeps deleting globals or
// declaring lexical bindings has each such read check the binding itself,
// rather than leave its code at every change.
const jitGlobalEpochs = 64

// jitLoopLocals is how many of a loop's slots Go goes on from (ssa's
// loopLocals): the array, the callback, this -- reduce's accumulator --
// the index, the length, the array map or filter makes, the element filter
// or find gave the callback, and its result, uninitialized if none yet.
const jitLoopLocals = 8

// jitSortLocals is how many slots a sort's loop has (ssa's sortLocals):
// the array and the comparator, which Go sorts again from, and the merge
// sort's own, which it clears.
const jitSortLocals = 9

// jitLoopFinishAt finishes the loop whose slots begin at at in the VM's
// stack, f's pc past its call fpc meanwhile: past the register frame's
// window and scratch slots (jitRegLoopFinish).
func (r *Runtime) jitLoopFinishAt(f *frame, at int, fpc uint32, loop uint64, after bool, v Value, err error) (Value, error) {
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
	if kind == abi.LoopSort {
		// A sort's exit, anywhere in it: Go sorts the array again from the
		// first, as the built-in sorts it. Nothing the code did shows --
		// its comparator does nothing a call made again could tell apart
		// (jitRegSortComparator), and the array is as it was until the copy
		// the code sorted is written back, which it is in one call of Go
		// after the last comparison (ssa's sortCall).
		if fe == nil || !arr.IsObject() {
			panic("jit: a sort's slots are not a built-in's")
		}
		clear(r.stack[at+jitLoopLocals : at+jitSortLocals])
		r.jit.loopsFinished++
		r.jit.sortsRestarted++
		f.pc = fpc
		if err := r.nativeFrame(fe, fe.fn(), arr, []Value{cb}, Undefined); err != nil {
			return Undefined, err
		}
		defer func() { r.frameDepth-- }()
		return fe.fn().native(r, arr, []Value{cb})
	}
	makes := kind == abi.LoopMap || kind == abi.LoopFilter
	var t *typedArrayData
	if typed {
		t, _ = arr.Object().dataOf().(*typedArrayData)
	}
	if fe == nil || !arr.IsObject() || !k.IsNumber() || !n.IsNumber() || makes && !out.IsObject() || typed && t == nil {
		panic("jit: a loop's slots are not a built-in's")
	}
	if !after && !res.IsUninitialized() {
		// An exit after the callback's call: its result the loop's.
		after, v = true, res
	}
	from := int64(k.Number())
	r.jit.loopsFinished++
	f.pc = fpc
	// The built-in's frame, as Go's has one (nativeFrame), which a stack
	// trace of what the callback throws shows.
	if err := r.nativeFrame(fe, fe.fn(), arr, []Value{cb, this}, Undefined); err != nil {
		return Undefined, err
	}
	defer func() { r.frameDepth-- }()
	a := &arrayLike{o: arr.Object(), n: int64(n.Number())}
	var o *arrayOut
	var mapped *typedArrayData
	switch {
	case typed && kind == abi.LoopMap:
		// The typed array of the length GoNewArray made, its elements
		// stored at their indices, as %TypedArray%.prototype.map stores
		// them.
		mapped, _ = out.Object().dataOf().(*typedArrayData)
		if mapped == nil {
			panic("jit: a typed array's map has no typed array")
		}
	case makes:
		// The loop's array, of the realm's species, as GoNewArray made it:
		// a typed array's filter's, the elements it kept.
		o = &arrayOut{o: out.Object(), plain: true, n: int64(len(out.Object().elemsOf()))}
	}
	// use is what the built-in does with the callback's result res at index
	// i, of element el: whether it is done, and its result then.
	use := func(i int64, el, res Value) (bool, Value, error) {
		switch kind {
		case abi.LoopReduce:
			this = res
		case abi.LoopMap:
			if mapped != nil {
				// A write past the end of a result that has since shrunk is
				// dropped, as any such write is.
				if err := r.setElem(mapped, int(i), res); err != nil {
					return true, Undefined, err
				}
				break
			}
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
		switch {
		case mapped != nil:
			return out, nil
		case typed:
			// The typed array of the elements kept, of the species, as
			// the method makes it after its loop.
			kept := o.o.elemsOf()
			return r.fillTypedArrayLike(arr, t, kept[:min(int(o.n), len(kept))])
		}
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

// jitNewTypedResult is GoNewArray's for a typed array's map or filter
// (ssa.NewArrayTyped): map's result, a typed array of recv's type and the
// length n, as TypedArraySpeciesCreate makes it; filter's Array of no
// length, which its loop appends the elements it keeps to; and, with
// ssa.NewArrayFill, filter's result made of that Array, kept, as the
// method makes it after its loop. Each only where recv is a typed array a
// loop is made over (jitLoopTyped) whose species is its type's own
// constructor, as it was made (jitPlainTypedSpecies), so that making it
// runs nothing; anything else, and anything that would throw, is refused,
// for Go to make it after an exit.
func (r *Runtime) jitNewTypedResult(form int, recv, n Value) (Value, bool) {
	t := r.jitLoopTyped(recv)
	if t == nil || !r.jitPlainTypedSpecies(recv.Object(), t) {
		return Undefined, false
	}
	// The species its type's own constructor, which makes a view of a
	// buffer of its own, as one of the type is made here: with no frame,
	// which would be written over a native callee's level's, still to be
	// made (jitRegGoCallNested).
	made := func(length int) (*Object, *typedArrayData, bool) {
		o := newObject(r.typedArrayProtoFor(t.kind), ClassTypedArray)
		nt, err := r.allocTypedArrayChecked(o, t.kind, int64(length))
		return o, nt, err == nil
	}
	switch {
	case form&ssa.NewArrayFill != 0:
		kept := r.plainArray(n)
		if kept == nil {
			return Undefined, false
		}
		els := kept.elemsOf()
		big := t.info().big
		for _, v := range els {
			if big && !v.IsBigInt() || !big && !v.IsNumber() {
				return Undefined, false
			}
		}
		o, nt, ok := made(len(els))
		if !ok {
			return Undefined, false
		}
		for i, v := range els {
			// Numbers, or a BigInt type's BigInts, which convert running
			// nothing.
			if err := r.setElem(nt, i, v); err != nil {
				return Undefined, false
			}
		}
		return Obj(o), true
	case form&ssa.NewArrayRoom != 0:
		if !n.IsNumber() || n.Number() != float64(t.count()) {
			return Undefined, false
		}
		o, _, ok := made(t.count())
		if !ok {
			return Undefined, false
		}
		return Obj(o), true
	}
	return Obj(r.newArrayOfLength(0)), true
}

// jitPlainTypedSpecies reports whether TypedArraySpeciesCreate of o, a
// typed array of t, makes one of its type's own constructor without
// running anything: o has no constructor of its own, its prototype's is
// the type's constructor, a data property, and that constructor's
// Symbol.species is %TypedArray%'s accessor as it was made -- as
// plainSpecies asks of an array.
func (r *Runtime) jitPlainTypedSpecies(o *Object, t *typedArrayData) bool {
	if o.findOwn(atomConstructor) >= 0 || o.proto() == nil {
		return false
	}
	ctor := r.typedArrayCtorFor(t.kind)
	if c := o.proto().getOwn(atomConstructor); !c.ok() || c.flags()&propAccessor != 0 || !c.value().IsObject() || c.value().Object() != ctor {
		return false
	}
	species := r.atoms.internSymbol(r.wellKnown.species)
	if ctor.findOwn(species) >= 0 || ctor.proto() != r.typedArrayCtor || r.jitTypedSpecies == nil {
		return false
	}
	sp := r.typedArrayCtor.getOwn(species)
	if !sp.ok() || sp.flags()&propAccessor == 0 {
		return false
	}
	a := sp.value().accessorPair()
	return a != nil && a.getter == r.jitTypedSpecies
}

// jitSortCopy is GoNewArray's for a sort native code makes (ssa's
// sortCall, ssa.NewArraySort): of recv, a typed array of a number type a
// loop is made over (jitLoopTyped), in bounds, its buffer not immutable --
// one the built-in would throw for is Go's to throw for -- or an Array
// whose every element is a number it has (jitSortArray), the Float64Array
// of twice its length whose first half holds its elements, as numbers --
// and, with ssa.NewArrayFill, x that Float64Array, whose first half is
// written back to recv's elements, in order, the result recv. Anything
// else is refused, before anything has run, for Go to make the call.
func (r *Runtime) jitSortCopy(form int, recv, x Value) (Value, bool) {
	var t *typedArrayData
	// An array's elements are read once, here, not through the array in the
	// loops below: Go 1.27 hoists a.extAt(), an addition to a, out of the
	// loops and their branches, and with a nil -- a typed array's -- that
	// left 0x20 in a pointer's slot, live across the calls, which a
	// collection scanning the stack throws for.
	var elems []Value
	n := 0
	if form&ssa.NewArrayTyped != 0 {
		if t = r.jitLoopTyped(recv); t == nil || t.info().big || t.outOfBounds() || t.storage().immutable {
			return Undefined, false
		}
		n = t.count()
	} else {
		a := r.jitSortArray(recv)
		if a == nil {
			return Undefined, false
		}
		a.ownElems()
		elems = a.elemsOf()
		n = len(elems)
	}
	if form&ssa.NewArrayFill == 0 {
		o := newObject(r.typedArrayProtoFor(elemFloat64), ClassTypedArray)
		s, err := r.allocTypedArrayChecked(o, elemFloat64, 2*int64(n))
		if err != nil {
			return Undefined, false
		}
		b := s.st.bytes[s.byteOffset:s.end]
		for i := range n {
			var v Value
			if t != nil {
				v = t.getElem(i)
			} else {
				v = elems[i]
			}
			binary.LittleEndian.PutUint64(b[i<<3:], math.Float64bits(v.num))
		}
		return Obj(o), true
	}
	if !x.IsObject() {
		return Undefined, false
	}
	s, ok := x.Object().dataOf().(*typedArrayData)
	if !ok || s.kind != elemFloat64 || s.count() != 2*n || s.st == nil {
		// Refused before anything is written: the exit has Go sort the
		// array as it was.
		return Undefined, false
	}
	b := s.st.bytes[s.byteOffset:s.end]
	for i := range n {
		v := Float(math.Float64frombits(binary.LittleEndian.Uint64(b[i<<3:])))
		if t != nil {
			// A number of the array's own, converted back to its type
			// exactly, which no conversion of a number fails.
			if err := r.setElem(t, i, v); err != nil {
				panic("jit: a sorted element not stored back")
			}
		} else {
			elems[i] = v
		}
	}
	return recv, true
}

// jitSortArray is recv if it is an Array a sort native code makes may
// sort (jitSortCopy): of the realm's, extensible, dense (plainArray: its
// length its elements'), whose prototypes have no element
// (noInheritedIndices), its every element a number -- no hole, nothing
// undefined -- so that reading and writing them back is
// what the property protocol would make of them, calling nothing; or nil.
func (r *Runtime) jitSortArray(recv Value) *Object {
	o := r.plainArray(recv)
	if o == nil || o.flags&objExtensible == 0 || !r.noInheritedIndices(o) {
		return nil
	}
	for _, v := range o.elemsOf() {
		if !v.IsNumber() {
			return nil
		}
	}
	return o
}

// jitSearchKind is the kind of o among jitSearches, or -1.
func (r *Runtime) jitSearchKind(o *Object) int {
	if o == nil {
		return -1
	}
	return slices.Index(r.jitSearches[:], o)
}

// LeafKind is ssa.LeafKindFeedback's: which of the leaves native code
// makes itself where it can the leaf at fn is -- a function Leaf found,
// which the code checks it calls -- by its frameless application (elemOp):
// String.prototype's codePointAt and at, Number's isNaN, isFinite,
// isInteger and isSafeInteger.
func (fb *jitFeedback) LeafKind(fn uintptr) ssa.LeafKind {
	fd := (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&fn))).fn()
	if fd == nil {
		return ssa.LeafOther
	}
	switch fd.elemOp {
	case elemStrCodePointAt:
		return ssa.LeafCodePointAt
	case elemStrAt:
		return ssa.LeafAt
	case elemNumIsNaN:
		return ssa.LeafNumberIsNaN
	case elemNumIsFinite:
		return ssa.LeafNumberIsFinite
	case elemNumIsInteger:
		return ssa.LeafNumberIsInteger
	case elemNumIsSafeInteger:
		return ssa.LeafNumberIsSafeInteger
	}
	return ssa.LeafOther
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

// jitBigString is GoBigString's string of recv, a BigInt, in the radix the
// call gives, if it gives one: BigInt.prototype.toString's, where it
// converts with nothing run and nothing thrown -- no radix, undefined, or
// a number that is an integer from 2 to 36. It refuses the rest.
func jitBigString(recv Value, args []Value) (Value, bool) {
	if !recv.IsBigInt() || bigLong(recv) {
		// A long one's digits are written by a conversion the host can
		// stop, which throws: the call's.
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

// jitArrayRoom is the room a pooled array of `new Array(n)` is given for a
// length n (abi.ObjectPool's Room): the least of the sizes newArrayStorage
// makes an array's elements in its own allocation that n fits, as the
// interpreter would make it; past them, the least power of two n fits
// (abi.PoolSmallRoom), its elements a slice of their own, as the
// interpreter makes them; or 0 past abi.PoolRoomLimit, the longest n the Array
// constructor makes dense, an n Go makes.
func jitArrayRoom(n float64) int {
	for _, room := range [...]int{4, 6, 8, 10, 12, abi.PoolSmallRoom} {
		if n <= float64(room) {
			return room
		}
	}
	if n > abi.PoolRoomLimit {
		return 0
	}
	return 1 << bits.Len(uint(n)-1)
}

// jitArrayFillBytes bounds the elements' storage one filling of a pool of
// arrays of more than abi.PoolSmallRoom makes (jitFillPool): a fill of
// up to abi.PoolCapacity arrays of 32 elements, one of 4,096 or more.
var jitArrayFillBytes = abi.PoolCapacity * 32 * int(valueSize)

// jitNewArrayPool is jitNewPool's for new Array(n): its arrays with room
// for room elements (abi.ObjectPool's Room), each a hole.
func (r *Runtime) jitNewArrayPool(o *Object, room int) *abi.ObjectPool {
	pool := &abi.ObjectPool{Source: unsafe.Pointer(&jitPoolSource{ctor: o}), Room: uint64(room)}
	r.jitFillPool(pool, o)
	return pool
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
		// the interpreter makes one (newArrayObject), its length 0 until
		// native code makes it n.
		room := int(pool.Room)
		if room > abi.PoolSmallRoom {
			// As many as jitArrayFillBytes holds, at least one, which is
			// what the pool counts (jitPoolBytes).
			n = max(1, min(n, jitArrayFillBytes/(room*int(valueSize))))
			pool.Size = uint64(n)
		}
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
	proto = p.value().Object()
	root = r.shapes.ctorRoot(fd, proto)
	if root == nil && polyable[struct{}](proto) {
		// Its object is made with proto in its table (newOrdinary), which
		// no pool's is.
		return nil, nil, 0, false
	}
	props = root.roomFor(int(fd.closure.fn.ThisProps))
	return proto, root, props, true
}

// jitFillObjectPool fills pool with objects of proto, of shape root, each
// with room for n properties, as newLiteralObject makes them: the layout
// chosen once for the refill, not for each object. Each is an allocation
// of its own, so that keeping one keeps none of the others.
func jitFillObjectPool(pool *abi.ObjectPool, proto *Object, root *shape, n int) {
	// The layout says the prototype: a constructor's or a literal's root,
	// or, where there is none, the class's for proto.
	root = root.orRoot(ClassObject, proto)
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
	case 7:
		jitFillSmallObjects[largeLiteralObject[[7]Value]](pool, proto, root)
	case 8:
		jitFillSmallObjects[largeLiteralObject[[8]Value]](pool, proto, root)
	default:
		for i := range int(pool.Size) {
			pool.Objects[i] = unsafe.Pointer(newShapedLiteral(root, ClassObject, n))
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

func (lo *literalObject1) jitObject() *Object {
	lo.setRoom(&lo.inline[0], len(lo.inline))
	return &lo.Object
}
func (lo *literalObject2) jitObject() *Object {
	lo.setRoom(&lo.inline[0], len(lo.inline))
	return &lo.Object
}
func (lo *literalObject) jitObject() *Object {
	lo.setRoom(&lo.inline[0], len(lo.inline))
	return &lo.Object
}
func (lo *largeLiteralObject[A]) jitObject() *Object {
	lo.setRoom(&lo.inline[0], len(lo.inline))
	return &lo.Object
}

// jitFillSmallObjects is jitFillObjectPool's for the layout L.
func jitFillSmallObjects[L any, P jitInlineObject[L]](pool *abi.ObjectPool, proto *Object, root *shape) {
	for i := range int(pool.Size) {
		o := P(new(L)).jitObject()
		o.shape = root
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
	ctor *Object
	root *shape
	n    int
	// array marks an array literal's pool; results, a call of next's pool
	// of result objects (jitFillResults); sharedPC, with sharedOf, register
	// code's new_array_k of constants whose arrays share them, made whole,
	// sharedPC its site's index in sharedOf's caches (jitRegSharedLiteral).
	// The three share a word.
	array    bool
	results  bool
	sharedPC int32
	lit      *bytecode.Constant
	// A closure's (jitFillClosures): its template, the function it is made
	// in and that function's closures' environment.
	tmpl Value
	fn   *bytecode.Function
	env  *Object
	// room is how many elements an array literal's arrays are made with
	// room for, past its own, learned of sample, the last array of the
	// pool's last filling (jitFillLiteral).
	room   int
	sample *Object
	// gen is the generator function a call's pool of generators is of
	// (jitFillGens).
	gen *Object
	// sharedOf is the closure of sharedPC's literal.
	sharedOf *closure
}

// jitRefillPool fills pool again, run out, with more objects (ssa's
// GoRefill), as an exit at its site has Go fill it, and reports whether it
// has one now: a construction's function whose prototype is no longer an
// object leaves it empty, and the site to Go.
func (r *Runtime) jitRefillPool(pool *abi.ObjectPool) bool {
	if r.gcYield.on && r.gcMarking() {
		r.gcYieldNow()
	}
	src := (*jitPoolSource)(pool.Source)
	if want := pool.Want; want != 0 && src != nil && src.ctor != nil {
		// A new Array(n) of an n past its arrays' room, or one less than
		// half of a room past abi.PoolSmallRoom: filled again, whatever it
		// holds, with arrays of n's room. One of a room past
		// abi.PoolSmallRoom, to or from, is filled with one array at first,
		// and more as it runs out, as a new pool is: a site whose lengths
		// change their room at each call is given an array a call, not a
		// pool's filling.
		pool.Want = 0
		room := jitArrayRoom(float64(want - 1))
		if room == 0 || uint64(room) == pool.Room {
			return false
		}
		if room > abi.PoolSmallRoom || pool.Room > abi.PoolSmallRoom {
			pool.Size = 1
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
	} else if src.results {
		r.jitFillResults(pool)
	} else if src.gen != nil {
		r.jitFillGens(pool)
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
	return newShapedLiteral(root.orRoot(ClassObject, proto), ClassObject, props)
}

// jitRefillPoolsOf refills the pools of the construction at register pc pc
// of e's code that constructs with o: one run out, made larger, and one
// made for another prototype than o's. It reports whether one had run out.
func (r *Runtime) jitRefillPoolsOf(e *jitEntry, pc int, o *Object) (refilled bool) {
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

// jitInliningFrequency is, in hundredths, how many calls of a callee too
// long to inline each invocation of the caller deciding its call must have
// made for it to be compiled for that caller at once (jitRegCalledOften):
// V8's min_inlining_frequency as V8 runs, 0.05 (ssa.MinInliningFrequency).
const jitInliningFrequency = 5

// jitLowering is a function's programs for the new pipeline, each lowered
// once, as its kind says (jitLowerKind), and kept by the runtime: a
// compile asks for its function's and for each callee's it inlines, and a
// function is compiled again as it learns, and inlined into each of its
// callers -- lowering them again had been some 8% of a compile. A program
// is never changed once lowered.
//
// bytes is what the lowering holds, its programs' (ir.Program's
// Footprint), which the JIT counts (jitState.lowerBytes); used the epoch
// it was last asked for in, as of the cache's last look (age). The cache
// drops the lowerings of functions it has no entry for to make room
// (makeRoom), the least recently used first, before any code.
type jitLowering struct {
	done  [jitLowerKinds]bool
	progs [jitLowerKinds]*ir.Program
	errs  [jitLowerKinds]error
	bytes int
	used  uint64
	// hints are what its code's exits have shown it does that its
	// bytecode does not say (jitcompile.Hints): its programs are lowered
	// with them. treeAdds is how many additions its tree had marked
	// (treeSeen) when they were last taken into hints.
	hints jitcompile.Hints
	// serial counts its relowerings (relower): a lowering made in the
	// background from hints a later relowering replaced is stale.
	serial uint32
	// busy counts the compiles of the function's register code under way,
	// which keep its programs (regLoweringSpare).
	busy uint8
	// typedCells are the cells of its element reads and writes of numbers,
	// by PC, that learn the element types of their typed arrays
	// (jit_typedkind.go).
	typedCells map[int32]*Value
}

// jitLowerKind is how a program is lowered: for a function's own code
// (jitcompile.LowerReg), for a callee inlined (LowerRegInline), for a
// forwarding constructor inlined (LowerRegForwardWith), for a function
// with no loop shown to be hot (LowerRegHot).
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

// relower drops l's programs, which are lowered again with its hints as
// they are asked for (jitRegLower): what was compiled from them holds them.
func (s *jitState) relower(l *jitLowering) {
	for k := range l.progs {
		if l.done[k] {
			n := l.progs[k].Footprint()
			l.bytes -= n
			s.lowerBytes -= n
		}
		l.done[k], l.progs[k], l.errs[k] = false, nil, nil
	}
	l.serial++
}

// jitBigIntNote is jitRegBigIntSeen's learning: in, the instruction whose
// guard failed or which is generic, at key, its register pc, in e, its
// operands read by operand, treeBig and treeWide what the interpreter saw
// of it (bigSeenMarks).
func jitBigIntNote(e *jitEntry, key int32, in ir.Instruction, operand func(ir.Operand) Value, treeBig, treeWide bool) bool {
	generic := slices.Contains(e.failed, key)
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
	a, b := operand(in.Left), operand(right)
	if !a.IsBigInt() || !b.IsBigInt() {
		return false
	}
	learned := slices.Contains(e.bigints, key)
	if learned || treeBig {
		// Its speculation, learned or the interpreter's, failed: made as one
		// of BigInts from 0 to 2^64-1 where they are, once.
		if !native || slices.Contains(e.bigwide, key) || treeWide || !jitBigUnsigned(a) || !jitBigUnsigned(b) {
			return false
		}
		e.bigwide = append(e.bigwide, key)
		e.reopt = true
		return true
	}
	e.bigints = append(e.bigints, key)
	e.reopt = true
	if !native {
		return false
	}
	if !a.isInt64Big() || !b.isInt64Big() {
		if jitBigUnsigned(a) && jitBigUnsigned(b) {
			e.bigwide = append(e.bigwide, key)
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

// jitBigRelease gives back what of a, b and the third operand, old, the
// call of Go that made v of them may give back (ssa.CallSite's Release),
// through bigRelease.
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

// globalSite is Global's site of a read of the global name, a write or a
// strict assignment's check, its cache site's index, or -1 for none:
// register code's (jitRegFeedback).
func (fb *jitFeedback) globalSite(name Atom, siteIdx int, write, check bool) (ssa.GlobalSite, bool) {
	env := fb.cl.scope()
	if env == nil {
		return ssa.GlobalSite{}, false
	}
	if !fb.r.isGlobalScope(env) {
		return fb.moduleGlobal(name, env, siteIdx, write, check)
	}
	if fb.r.lexShadows(name) {
		return fb.lexGlobal(name, env, write)
	}
	i := int32(-1)
	if siteIdx >= 0 && siteIdx < len(fb.cl.ic) {
		i = fb.cl.ic[siteIdx].idx
	}
	if i < 0 || int(i) >= int(env.valn) || env.shape.keys[i].key != name {
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
	if s, p := env.shape, env.shape.keys[i]; env.flags&objGlobalBindings != 0 && s.unique && !s.loose && s.tree == fb.r.shapes &&
		s.tree.globalEpoch < jitGlobalEpochs && !p.isAccessor() && p.flags&(propUninit|propPrivate|propDeleted) == 0 {
		// Plain, initialized data at its position, unshadowed: so while no
		// binding of the global object has changed but by appends, and no
		// script-level lexical binding has been declared (globalEpoch),
		// which the read checks rather than each of those.
		site.Epoch, site.Scope = uint32(s.tree.globalEpoch)+1, uintptr(unsafe.Pointer(env))
	}
	site.Typed = !env.shape.keys[i].isAccessor() && jitTypedArray(env.vals()[i])
	site.TypedKind = jitTypedKind(site.Typed, env.vals()[i])
	if p := env.shape.keys[i]; !p.isAccessor() && p.flags&(propWritable|propConfigurable) == 0 {
		switch {
		case env.vals()[i].IsUndefined():
			site.Fixed, site.Constant = true, ir.Value{Kind: ir.Undefined}
		case env.vals()[i].IsNumber():
			site.Fixed, site.Constant = true, ir.Float(env.vals()[i].Number())
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
func (fb *jitFeedback) moduleGlobal(name Atom, env *Object, siteIdx int, write, check bool) (ssa.GlobalSite, bool) {
	i := int32(-1)
	if siteIdx >= 0 && siteIdx < len(fb.cl.ic) {
		i = fb.cl.ic[siteIdx].idx
	}
	if i < 0 || int(i) >= int(env.valn) || env.shape.keys[i].key != name {
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
		g := env.proto()
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
	site.TypedKind = jitTypedKind(site.Typed, p.value)
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

// jitTypedKind is the element type plus one of v, a typed array if typed
// (ssa.GlobalSite's TypedKind), or 0.
func jitTypedKind(typed bool, v Value) uint8 {
	if !typed {
		return 0
	}
	if t, ok := v.Object().dataOf().(*typedArrayData); ok {
		return uint8(t.kind) + 1
	}
	return 0
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
	site.TypedKind = jitTypedKind(site.Typed, p.value)
	if !write && p.flags&propWritable == 0 && p.value.IsNumber() {
		site.Fixed, site.Constant = true, ir.Float(p.value.Number())
	}
	return site, true
}

// stringPropertyOf is StringProperty's: the method read at pc, its cache
// c, of key; big says the interpreter saw it meet a BigInt.
func (fb *jitFeedback) stringPropertyOf(pc int, c *propCache, key Atom, big bool) (ssa.PropertySite, bool) {
	e := fb.e
	if c.shape != nil && c.shape != noShape || slices.ContainsFunc(e.poly, func(p jitPolySite) bool { return int(p.pc) == pc }) {
		return ssa.PropertySite{}, false
	}
	proto, kind := fb.r.proto.str, ssa.PrimitiveString
	str := slices.Contains(e.strSites, uint32(pc))
	switch {
	case str:
	case slices.Contains(e.bigSites, uint32(pc)) || big:
		// A read that met BigInt receivers and no string: off
		// BigInt.prototype, which the site names (Holders).
		proto, kind = fb.r.proto.bigint, ssa.PrimitiveBigInt
	case slices.Contains(e.numSites, uint32(pc)):
		// Numbers: off Number.prototype so.
		proto, kind = fb.r.proto.number, ssa.PrimitiveNumber
	case slices.Contains(e.boolSites, uint32(pc)):
		proto, kind = fb.r.proto.boolean, ssa.PrimitiveBoolean
	}
	if key.IsIndex() || key == atomLength || proto == nil {
		return ssa.PropertySite{}, false
	}
	if kind == ssa.PrimitiveString && !str &&
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
	site := ssa.PropertySite{Key: uint32(key), Shape: uintptr(unsafe.Pointer(sh)), Index: i, Primitive: kind}
	if kind != ssa.PrimitiveString {
		k.holders = append(k.holders, proto)
		site.Holders[0] = ssa.Holder{Object: uintptr(unsafe.Pointer(proto)), Shape: site.Shape}
	}
	return site, true
}

// writeCases are what the write at pc met at objects of shapes other than
// site's (jitPolyWriteSeenOp): an own property it wrote, or a transition it
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

// cases are the shapes other than first the read at pc met (jitPolySeenOp),
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

// deepCasesOf is the cases of the read at pc, its cache c (nil for none),
// whose property is past the two prototypes a cache holds: those the code
// met at its exits, and those the cache's reads met.
func (fb *jitFeedback) deepCasesOf(pc int, c *propCache) []ssa.PropertyCase {
	var cases []ssa.PropertyCase
	if fb.e != nil && fb.root == nil {
		cases = fb.cases(pc, 0)
	}
	if c != nil && c.seenOf() != nil {
		reads := c.seenOf().reads
		for i := range reads {
			d := &reads[i]
			if len(cases) < jitPropertyCases && !slices.ContainsFunc(cases, func(k ssa.PropertyCase) bool { return k.Shape == uintptr(unsafe.Pointer(d.shape)) }) {
				cases = append(cases, fb.deepCase(d))
			}
		}
	}
	return cases
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
	if c.cell != nil {
		// An addition past a chain longer than the entry holds, which it
		// checks by the chain's cell (fillChainStore): compiled as the
		// additions along such chains are, for each prototype as it is.
		if !c.cell.valid {
			return nil
		}
		if d, ok := fb.r.jitDeepAddFrom(c.shape, next, c.p1, next.key); ok {
			return fb.deepAdd(d)
		}
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

// jitSSAStats counts what one function's new-pipeline code does. Each
// entry starts a native stretch, which ends in a return, an exit to Go or
// a failed guard (ended counts those two), or a poll; work estimates, in
// slot IR instructions, what the stretches did natively.
type jitSSAStats struct {
	entries, hosts, guards, polls uint64
	ended, work                   uint64
	// goCalls counts the calls native code had Go make (jitGoCall).
	goCalls uint64
	// calleeGoCalls counts those it made as a native callee
	// (jitRegGoCallNested), and calleeWork the work it did natively before
	// each, as jitRegStretchWork counts a stretch's (jitRegCalleeProfit),
	// since its calls in (jitLink's in) were last counted from 0.
	calleeGoCalls, calleeWork uint64
	// runGoCalls counts those it made in runs begun at a loop that Go makes
	// for good, and runWork the work it did natively before each
	// (jitRegRunGoProfit); osrStretches the stretches begun at a loop by
	// on-stack replacement, and osrWork the work they did
	// (jitRegStretchWork) -- each since Go last began entering the code
	// again (jitRegRetrySlow).
	runGoCalls, runWork   uint64
	osrStretches, osrWork uint64
	// judged is how many stretches, calls of Go included, there had been
	// when jitRegStretchWork last judged the code.
	judged uint64
	// workBase and stretchBase are work and the stretches when Go last
	// began entering the code again (jitRegRetrySlow): it is judged by what
	// it has done since.
	workBase, stretchBase uint64
}

// jitSSAProbe is how many native stretches a function runs between looks
// at whether its native code pays (jitRegStretchWork), and jitSSAMinWork
// the work a stretch must do, on average, for it to: an entry, and the exit
// or return that ends the stretch, cost what the interpreter takes for
// about that many instructions (the V8 suite's medians of five, measured
// with an earlier interpreter: 10 left Richards' task methods, called from
// the interpreter and calling out again, native at a loss; 20 was best
// overall, 1,374.8 ms against 1,413.6).
const (
	jitSSAProbe   = 64
	jitSSAMinWork = 20
)

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
	// The level past the deepest the VM's frames allow, which the contexts
	// made so far may limit further: they grow (jitGrowContexts) to it.
	s.levelBound = uint64(idx + max(0, r.maxFrames-r.frameDepth))
	bounds := abi.Bounds{
		LevelLimit: min(uint64(len(s.ssaCtxs)), s.levelBound),
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
		stackBase: unsafe.Pointer(unsafe.SliceData(r.stack)), stackEnd: r.jitStackEnd(),
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
		c.BackEdges, c.GlobalEpoch, c.TypedEpoch = &r.backEdges, &r.shapes.globalEpoch, &r.typedEpoch
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

// jitContexts is how many contexts are made first: one for each run of
// native code (jitRegStretch), each native call another (abi.Context.Level);
// and as many more as there are each time they run out, as a stack grows, as
// deep as the VM's frames may go (jitContextsMost). A context is some 17 KB:
// the first jitContexts are of the JIT's budget (jitBudget), which every
// runtime that runs native code holds; the rest are the native levels'
// stack, as V8's optimized frames are on the machine stack and not in its
// code space, which a recursion takes as it deepens and gives back once it
// has returned (jitShrinkContexts). With at most 96 of them, AWFY's Havlak,
// whose depth-first search recurses deeper, ran its deeper levels in the
// interpreter, and EarleyBoyer's unifier had finished most of its calls in
// Go with no more than 24.
const jitContexts = 4

// jitContextsMost is how many contexts the runtime's native levels may
// take: one for each frame the VM allows (Config.MaxCallDepth), as each
// level is a frame of the VM's to the depth limit (abi.Bounds'
// LevelLimit), and one past the deepest, which a run Go begins there
// needs (jitRegStretch). A call past the limit is Go's, which throws the
// interpreter's RangeError.
func (r *Runtime) jitContextsMost() int { return r.maxFrames + 2 }

// jitGrowTo is s with room for n elements, its length as it is.
func jitGrowTo[T any](s []T, n int) []T {
	if cap(s) >= n {
		return s
	}
	return append(make([]T, 0, n), s...)
}

// jitGrowContexts adds a chunk of contexts, linked to the last --
// jitContexts, or as many as there are -- if there are fewer than the
// VM's frames need (jitContextsMost), and reports whether it did: the
// first chunk if the JIT's budget has the room for it, the others
// whatever the budget holds, as a deeper recursion needs them. The others
// stay where they are, and native code may be running in them, its
// callers' ReturnTo and Prev pointing at them: each one whose calls the
// contexts limited may go as deep as the new ones allow, and each new one
// holds what they share (jitShareContexts).
func (r *Runtime) jitGrowContexts() bool {
	s := r.jit
	old := len(s.ssaCtxs)
	add := min(max(old, jitContexts), r.jitContextsMost()-old)
	if add <= 0 {
		return false
	}
	if old < jitContexts {
		s.budget = r.jitBudget()
		if !s.makeRoom(add * (jitContextShare + s.spillShare())) {
			return false
		}
	}
	// Each context's places in what is kept by context, made here in the
	// room made for them: grown as runs reach them, they took room no one
	// had made.
	s.ssaCtxs = jitGrowTo(s.ssaCtxs, old+add)
	s.goRuns = jitGrowTo(s.goRuns, old+add)
	s.calleeOpen = jitGrowTo(s.calleeOpen, old+add)
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
		chunk[i].SetConsts(&s.enc)
		chunk[i].Level, chunk[i].Bounds, chunk[i].Hints = uint64(old+i), &s.bounds, &s.hintTable
		if s.mega == nil {
			s.mega = new(abi.MegaCache)
		}
		chunk[i].Mega = s.mega
		chunk[i].GrowSpill(s.ssaSpills)
		s.ssaCtxs = append(s.ssaCtxs, &chunk[i])
	}
	for i := max(old, 1); i < len(s.ssaCtxs); i++ {
		c, prev := s.ssaCtxs[i], s.ssaCtxs[i-1]
		prev.Next, c.Prev = unsafe.Pointer(c), unsafe.Pointer(prev)
	}
	s.ctxMost = max(s.ctxMost, len(s.ssaCtxs))
	if old == 0 {
		return true
	}
	n := uint64(len(s.ssaCtxs))
	last := s.ssaCtxs[old-1]
	for _, c := range s.ssaCtxs[old:] {
		c.StackBase, c.StackEnd, c.StackTop, c.StackHigh = last.StackBase, last.StackEnd, last.StackTop, last.StackHigh
		c.BackEdges, c.Global, c.LexNames, c.GlobalLex = last.BackEdges, last.Global, last.LexNames, last.GlobalLex
		c.GlobalEpoch, c.TypedEpoch = last.GlobalEpoch, last.TypedEpoch
		c.StringMethods = last.StringMethods
		c.CharCodeAt = last.CharCodeAt
		c.StringProto, c.StringShape, c.CharCodeAtOffset = last.StringProto, last.StringShape, last.CharCodeAtOffset
	}
	if s.bounds.LevelLimit == uint64(old) && s.levelBound > uint64(old) {
		// Calls the contexts limited may go as deep as the new ones allow,
		// to the VM's frames' limit.
		s.bounds.LevelLimit = min(n, s.levelBound)
		s.ssaSharedGen++
	}
	return true
}

// jitShrinkContexts gives back the chunks of contexts (jitGrowContexts)
// past every one native code runs in now and every one it has run in
// since the last look, a flush's tick before (flush), as Go shrinks a
// goroutine's stack that has used less than it holds: a recursion that
// went deep once keeps none of its contexts, one that recurses as deep
// again within a tick keeps them all. One runs now below ctxTop, which
// a run Go begins, and a call of Go from native code, moves past every
// level it is under; or is a native level past it a call has made
// (abi.Context's Live), which Go has yet to finish or read. One has run
// since if its frame's locals were set (Locals), which a run's entry and
// every native call's callee (mir's callEntry) set, and which the look
// clears in every context past those running, for the next to see. What
// stays is a chunk's end, one past the deepest kept, for a run begun
// there; none of what native code runs in moves.
func (s *jitState) jitShrinkContexts() {
	n := len(s.ssaCtxs)
	if n <= jitContexts {
		return
	}
	keep := s.ctxTop + 1
	for i := s.ctxTop; i < n; i++ {
		c := s.ssaCtxs[i]
		if c.Live != 0 {
			keep = max(keep, i+2)
			continue
		}
		if c.Locals != nil {
			keep = max(keep, i+2)
			c.Locals = nil
		}
	}
	// The chunks end at jitContexts doubled, and at the last's end.
	end := jitContexts
	for end < keep {
		end *= 2
	}
	if end >= n {
		return
	}
	for i := end; i < n; i++ {
		s.ssaCtxs[i] = nil
	}
	s.ssaCtxs[end-1].Next = nil
	s.ssaCtxs = append([]*abi.Context(nil), s.ssaCtxs[:end]...)
	s.goRuns = jitShrinkTo(s.goRuns, end, s.ctxTop == 0)
	s.regRuns = jitShrinkTo(s.regRuns, end, s.ctxTop == 0)
	s.calleeOpen = jitShrinkTo(s.calleeOpen, end, s.ctxTop == 0)
	if s.bounds.LevelLimit > uint64(end) {
		s.bounds.LevelLimit = uint64(end)
		s.ssaSharedGen++
	}
	if s.ssaSharedFrom > end {
		s.ssaSharedFrom = end
	}
	if c := s.ssaCtx; c != nil && c.Level >= uint64(end) {
		// The context last entered, a run's that has ended: let go with
		// its chunk.
		s.ssaCtx = s.ssaCtxs[0]
	}
	s.ctxShrunk++
}

// jitShrinkTo is s with no more than n elements, what it held past them
// -- a run's by context, gone with the contexts (jitShrinkContexts) --
// cleared, in an array of its own if move says no run is under way: a run
// keeps a pointer to its own element across the calls of Go it makes.
func jitShrinkTo[T any](s []T, n int, move bool) []T {
	if len(s) > n {
		clear(s[n:])
		s = s[:n]
	}
	if !move || cap(s) <= n {
		return s
	}
	return append(make([]T, 0, n), s...)
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
// fields for it to give (jitRegSuperTarget). The binding is the result.
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

// jitBindThis binds the this of f, a derived constructor's frame Go
// entered its code with, as a super() native code made bound it (its
// context's This), where Go has not: as the interpreter's super() binds
// the frame's binding, which a closure Go makes on f, an arrow, shares.
// Native code made such a super() only where no arrow is made before it
// (jitRegArrowBeforeSuper): none shares the binding unbound.
func jitBindThis(f *frame, ctx *abi.Context) {
	if f.thisRef == nil || f.thisRef.init || f.cl.fn.Kind != bytecode.KindDerivedConstructor {
		return
	}
	if this := *(*Value)(unsafe.Pointer(&ctx.This)); !this.IsUninitialized() {
		f.this, f.thisRef.value, f.thisRef.init = this, this, true
	}
}

// jitSuperCall is superCall for a super() Go makes for native code
// (abi.GoCallSuper).
func (r *Runtime) jitSuperCall(f *frame, args []Value) error {
	return r.superCall(f, args)
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
// every native call outside it (jitRegUnwind), which costs more than Go
// making the call.
const (
	jitUnwindProbe = 64
	jitUnwindShare = 4
)

// jitLearning reports whether e's code has met, since it was compiled,
// what a compile would take that it was not compiled for: what is to be
// compiled again for (reoptPending), or a call it has left native code at
// but not yet decided, which the compile would decide if the interpreter
// has met its target.
func jitLearning(e *jitEntry) bool {
	if e.reoptPending() {
		return true
	}
	for _, n := range e.callSites {
		if n != 0 && n < jitCallWaiting {
			return true
		}
	}
	return false
}

// jitUnwindReopts is how many times code that leaves too often when called
// natively is compiled again for it before native callers stop calling it
// (jitRegUnwound).
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

// jitArgument is ssa.GoArgument's answer, for the code ctx runs, a native
// callee's, whose call wrote its arguments to its context: arguments[k] -- the first operand k, the second the local
// the arguments object is kept in once made -- or, of one operand, that
// local, arguments.length; and whether it answers. While the object is
// not made, an index of the arguments is the argument, as the arguments
// object reads it -- a mapped object's too, which aliases parameters the
// function never assigns (compile's argumentsOwn), or, where it or a closure
// assigns them, the parameter the index aliases, which the code passes past
// the two operands (ir.Program's ArgumentParams) -- and their count is how
// many there are. The rest it leaves to the code's exit, where Go makes it.
func (r *Runtime) jitArgument(ctx *abi.Context) (Value, bool) {
	// A native callee's, whose call wrote them to its context
	// (jitFrameArgs).
	cl := (*closure)(ctx.Closure)
	if ctx.Live != abi.LiveCall || cl == nil || !jitFrameArgs(cl.fn) {
		return Undefined, false
	}
	args, fn := jitContextArgs(ctx), cl.fn
	if ctx.GoOp>>abi.GoOpArgcShift&0xff == 1 {
		if jitGoArg(ctx, 0).IsObject() {
			return Undefined, false
		}
		return Int(len(args)), true
	}
	k := jitGoArg(ctx, 0)
	if jitGoArg(ctx, 1).IsObject() || !k.IsNumber() {
		return Undefined, false
	}
	if i := uint32(k.num); float64(i) == k.num && uint(i) < uint(len(args)) {
		if n := int(ctx.GoOp>>abi.GoOpArgcShift&0xff) - 2; uint(i) < uint(n) && mapsArgument(fn, int(i)) {
			// The parameter the mapped object would alias, as the code
			// has it now (ir.Program's ArgumentParams): one it, or a
			// closure, assigned.
			return jitGoArg(ctx, 2+int(i)), true
		}
		return args[i], true
	}
	return Undefined, false
}

// jitFrameArgs reports whether fn's code reads its call's arguments past
// its parameters, from the frame of the VM's: a rest parameter's, the
// arguments object's, a super() of them (bytecode.Function's
// SuperForwards). A native
// call of it writes them all to its callee's context (ssa.CallSite's
// FrameArgs, abi.Context's Args), where Go finds them for the frames it
// makes of the callee's level, as V8's call pushes all of a call's
// arguments, its callee reading them past its parameters.
func jitFrameArgs(fn *bytecode.Function) bool {
	return fn.HasRest || fn.UsesArguments || fn.MappedArguments || fn.Kind == bytecode.KindDerivedConstructor && fn.SuperForwards
}

// jitFrameCallee reports whether fn's frame must have its function object
// (bytecode.Function's FrameCallee, which the compiler records): its code
// reads it -- a super reference's home
// object, push_callee's, a mapped arguments object's callee -- or an arrow
// it makes takes the home object or new.target from it. A native call of
// it writes the object to its callee's context (ssa.CallSite's Function,
// abi.Context's Function), where Go finds it for the frames it makes of the
// callee's level, as V8's frame has its function.
func jitFrameCallee(fn *bytecode.Function) bool { return fn.FrameCallee }

// jitContextArgs is the arguments a native call wrote to its callee's
// context, c (abi.Context's Args), in place: valid while the callee's level
// runs, its next call writing the next context's.
func jitContextArgs(c *abi.Context) []Value {
	n := min(int(c.Argc), len(c.Args))
	return unsafe.Slice((*Value)(unsafe.Pointer(&c.Args[0])), n)
}

// jitTypeWord is the type word an interface holding x has (abi.Encoding's
// MapIterData): what native code compares an object's data with to know
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
	_ = uint8(elemBigInt64-abi.ElemBigInt64) + uint8(abi.ElemBigInt64-elemBigInt64)
	_ = uint8(elemBigUint64-abi.ElemBigUint64) + uint8(abi.ElemBigUint64-elemBigUint64)
)

func jitTypeWord(x any) uint64 {
	return uint64((*[2]uintptr)(unsafe.Pointer(&x))[0])
}

// jitForgetHighSeen is jitStackHighLowered: Go has lowered the VM stack's
// high water, and each context forgets what it last found it at
// (abi.Context's HighSeen), which a call entry trusts never to be above
// it; their next call entries raise it again where their frames reach
// past it.
func jitForgetHighSeen(r *Runtime) {
	if r.jit == nil {
		return
	}
	for _, c := range r.jit.ssaCtxs {
		c.HighSeen = 0
	}
}

// A cursor's released byte is the one past its done byte, where native
// code's close writes it (abi.IterReleased).
var _ [unsafe.Offsetof(iterState{}.released) - unsafe.Offsetof(iterState{}.done) - abi.IterReleased]struct{}
var _ [abi.IterReleased - (unsafe.Offsetof(iterState{}.released) - unsafe.Offsetof(iterState{}.done))]struct{}
