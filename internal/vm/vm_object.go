package vm

import (
	"math"
	"slices"
	"unsafe"
)

// Class identifies an object's exotic behaviour. Most objects are ClassObject
// and behave ordinarily; the rest have internal slots or overridden property
// semantics that the interpreter must know about.
type Class uint8

const (
	ClassObject Class = iota
	ClassArray
	ClassFunction
	ClassError
	ClassArguments
	ClassBooleanWrapper
	ClassNumberWrapper
	ClassStringWrapper
	ClassSymbolWrapper
	ClassBigIntWrapper
	ClassDate
	ClassRegExp
	ClassMap
	ClassSet
	ClassWeakMap
	ClassWeakSet
	ClassIteratorHelper
	ClassIteratorWrap
	ClassWeakRef
	ClassFinalizationRegistry
	ClassPromise
	ClassGenerator
	ClassAsyncGenerator
	ClassArrayBuffer
	ClassDataView
	ClassTypedArray
	ClassProxy
	ClassIterator
	ClassMathObject
	// ClassModuleNamespace is the object `import * as ns` produces, whose
	// properties are a module's exports and nothing else.
	ClassModuleNamespace
	ClassJSONObject
)

// propFlags holds a property's attributes.
type propFlags uint8

const (
	propWritable propFlags = 1 << iota
	propEnumerable
	propConfigurable
	// propAccessor marks a property whose value slot holds an *accessor rather
	// than a data value.
	propAccessor
	// propDeleted marks a slot vacated by delete. Slots are tombstoned rather
	// than removed so that the index map stays valid and enumeration order is
	// preserved for the properties that remain.
	propDeleted
	// propPrivate marks a private class member. Private members are stored as
	// ordinary properties under a key that source cannot spell, but they are
	// not properties in the language's sense: they are invisible to
	// enumeration, to Object.getOwnPropertyNames, to JSON, and to `in`. Only
	// the private accessors reach them.
	propPrivate
	// propNamespaceExport marks a module namespace's export. It is stored as an
	// accessor, because an export is live, but it reports as a data property:
	// what a module exports is a value, not a way of computing one.
	propNamespaceExport
	// propUninit marks a binding still in its temporal dead zone. Only a
	// module's top-level lexical bindings need it: everything else in a dead
	// zone is a frame slot, where the uninitialized value alone says so.
	propUninit
)

// propDefault is the attribute set for an ordinary assignment.
const propDefault = propWritable | propEnumerable | propConfigurable

// accessor holds the getter and setter of an accessor property.
type accessor struct {
	getter *Object
	setter *Object
}

// Property is one own property of an object, as a value: what is added to
// an object, or read from one. An object keeps its properties' keys and
// attributes in its shape (propKey) and only their values itself
// (Object.vals).
//
// An accessor stores its getter and setter in an *accessor hung off the value's
// reference field, so that the common data property costs no more than a
// value.
type Property struct {
	key   Atom
	flags propFlags
	value Value
}

func (p *Property) isAccessor() bool { return p.flags&propAccessor != 0 }

func (p *Property) getterSetter() *accessor {
	return p.value.accessorPair()
}

// propKey is a property's key and attributes, which its object's shape keeps
// for it (shape.keys). Its layout is shapeEdge's.
type propKey struct {
	key   Atom
	flags propFlags
}

func (k propKey) isAccessor() bool { return k.flags&propAccessor != 0 }

// ownProp is an object's own property, as getOwn finds it: its value is the
// object's, at i, and its key and attributes the object's shape's. An i of
// -1 is none.
type ownProp struct {
	o *Object
	i int32
}

func (p ownProp) ok() bool                { return p.i >= 0 }
func (p ownProp) value() Value            { return p.o.vals[p.i] }
func (p ownProp) flags() propFlags        { return p.o.shape.keys[p.i].flags }
func (p ownProp) isAccessor() bool        { return p.flags()&propAccessor != 0 }
func (p ownProp) getterSetter() *accessor { return p.value().accessorPair() }

// setValue changes the property's value, which its layout does not settle.
func (p ownProp) setValue(v Value) { p.o.vals[p.i] = v }

// setFlags changes the property's attributes, and so its object's layout.
func (p ownProp) setFlags(flags propFlags) { p.o.setFlagsAt(p.i, flags) }

// objFlags holds per-object booleans.
type objFlags uint8

const (
	objExtensible objFlags = 1 << iota
	// objArrayLengthWritable tracks whether an array's length is writable,
	// which Object.defineProperty can turn off.
	objArrayLengthWritable
	// objHasSparseElements marks an array that has fallen back to storing
	// elements as ordinary properties.
	objHasSparseElements
	// objMappedArguments marks a sloppy-mode arguments object whose indices
	// alias the parameters they were passed to, so that writing one is visible
	// through the other.
	objMappedArguments
	// objEvalVars marks the object holding the bindings a direct eval declared
	// in a frame. It sits on the scope chain like a `with` object, but it is
	// not one a script can reach: a call through it has no receiver.
	objEvalVars
	// objImmutableProto marks an object whose prototype can never be changed,
	// however extensible it is. Object.prototype is one: the root of every
	// ordinary chain cannot be given a parent without making the chain cyclic
	// for the whole realm at once.
	objImmutableProto
	// objHTMLDDA is Annex B's [[IsHTMLDDA]] slot, which the web's document.all
	// has: typeof says "undefined", ToBoolean says false, and == says it
	// equals null and undefined. Everything else sees an ordinary object.
	objHTMLDDA
	// objInlineProps marks an object whose properties are still in the room
	// its own allocation carries for its first few. Grown past it, they move
	// to a slice of their own, and the room is cleared as they leave: it is
	// part of the object, and what it still held would stay reachable for as
	// long as the object is -- a node's old links, and through them every
	// node a linked structure has let go of since.
	objInlineProps
)

// linearScanLimit is the property count below which lookup scans the keys
// instead of consulting a map.
//
// Most objects are small, and for a handful of properties a linear scan over a
// contiguous slice of 8-byte keys beats a map probe: it touches one cache line
// and needs no hashing. The index map is built only once an object grows past
// this.
const linearScanLimit = 8

// Object is a JavaScript object.
type Object struct {
	proto *Object

	// vals holds the values of the named and symbol-keyed properties in
	// insertion order, which is the order enumeration must report them in.
	vals []Value
	// shape is the table's layout: its keys and their attributes, at the
	// values' positions; an identity a property cache can remember, unless it
	// is loose; and the table's lookup index. See vm_shape.go. An object with
	// properties always has one. Whatever changes the table's keys, their
	// order or their attributes keeps it right: appendProp and layoutChanged.
	shape *shape

	// ext holds what only some objects have -- an array's elements, an
	// exotic object's internal slots, the WeakMaps it is a key of -- or is
	// nil: an ordinary object, most of all, has none, and is
	// the smaller for it. An array and a function have it in their own
	// allocation; an object that comes to need it later gets one of its
	// own (extOf).
	ext *objectExt

	class Class
	flags objFlags
	// mark is the memory meter's, which it sets on an object it has counted;
	// it fills what would be padding.
	mark uint16

	// arrayLen is an array's length once it stops being dense. It cannot be
	// derived from the keys present at that point, because a length may be far
	// beyond the highest index -- `a.length = 4294967295` leaves no keys at all
	// -- and because an array is not required to have its last index present.
	// It fills the rest of what would be padding.
	arrayLen uint32
}

// objectExt is the part of an object only some objects have (Object's ext).
//
// An object whose kind always needs it -- an array, a function, a Date --
// has it in its own allocation, right after the Object (embedExt), and
// pays for it no more than it did when these were the Object's own fields.
// Any other object that comes to need it has a keyedExt of its own
// (extOf).
//
// An array's is always its own, for as long as the array lives: native code
// reads an array's elements at a fixed offset from it (abi.Encoding's
// ObjectElems).
type objectExt struct {
	// elems holds dense array elements. Arrays and other index-keyed objects
	// keep their elements here rather than in the property table, so that
	// a[i] is a slice index rather than a map lookup.
	elems []Value

	// data carries the internal slots of an exotic object: *funcData for a
	// function, *dateData for a Date, and so on. It is nil for an ordinary
	// object. An array has none, and keeps its weakMapRefs here instead
	// (setWeakMapRefs).
	data any
}

// keyedExt is an ext in an allocation of its own, which has room for the
// WeakMaps the object is a key of. Being a key is rare enough that an
// embedded ext has no such room: an object with one that becomes a key
// moves to a keyedExt, its fields copied, unless it is an array
// (setWeakMapRefs).
type keyedExt struct {
	objectExt

	// weakMapRefs holds the object's values in the WeakMaps it is a key of,
	// made when it first becomes one (see weakmap.go).
	weakMapRefs *weakMapRefs
}

// embedExt gives o the ext its allocation carries, which must be right
// after it: that is how weakMapRefsOf tells one from a keyedExt.
func (o *Object) embedExt(e *objectExt) {
	if uintptr(unsafe.Pointer(e)) != uintptr(unsafe.Pointer(o))+unsafe.Sizeof(Object{}) {
		panic("vm: an embedded ext not right after its object")
	}
	o.ext = e
}

// extEmbedded reports whether o's ext is the one its allocation carries.
// Nothing else can be at that address: it is within o's allocation, where
// only an embedded ext is put.
func (o *Object) extEmbedded() bool {
	return uintptr(unsafe.Pointer(o.ext)) == uintptr(unsafe.Pointer(o))+unsafe.Sizeof(Object{})
}

// extOf is o's ext, made if it has none.
func (o *Object) extOf() *objectExt {
	if o.ext == nil {
		o.ext = &new(keyedExt).objectExt
	}
	return o.ext
}

// elemsOf is o's dense elements: none without an ext.
func (o *Object) elemsOf() []Value {
	if e := o.ext; e != nil {
		return e.elems
	}
	return nil
}

// dataOf is o's internal slots: nil without an ext. An array's is its
// weakMapRefs, if it has been a key.
func (o *Object) dataOf() any {
	if e := o.ext; e != nil {
		return e.data
	}
	return nil
}

// weakMapRefsOf is o's values in the WeakMaps it is a key of: nil if it has
// never been one.
func (o *Object) weakMapRefsOf() *weakMapRefs {
	switch {
	case o.ext == nil:
		return nil
	case o.class == ClassArray:
		refs, _ := o.ext.data.(*weakMapRefs)
		return refs
	case o.extEmbedded():
		return nil
	}
	return (*keyedExt)(unsafe.Pointer(o.ext)).weakMapRefs
}

// setWeakMapRefs gives o its values in the WeakMaps it is a key of. An
// array keeps them as its data; any other object in a keyedExt, to which
// the fields of an embedded ext move -- which is why nothing may keep the
// ext of an object that is not an array across what can make it a key.
func (o *Object) setWeakMapRefs(refs *weakMapRefs) {
	switch {
	case o.ext == nil:
		o.extOf()
	case o.class == ClassArray:
		o.ext.data = refs
		return
	case o.extEmbedded():
		k := &keyedExt{objectExt: *o.ext}
		o.ext = &k.objectExt
	}
	(*keyedExt)(unsafe.Pointer(o.ext)).weakMapRefs = refs
}

// maxHoleRun is the longest run of holes an array writes into its dense
// elements; a longer one makes it sparse.
const maxHoleRun = 1024

// newObject returns a bare object with the given prototype and class.
func newObject(proto *Object, class Class) *Object {
	flags := objExtensible
	if class == ClassArray {
		// An array's length is synthesized rather than stored, so its
		// writability is a flag rather than a property attribute, and every
		// array starts with it writable.
		flags |= objArrayLengthWritable
	}
	if class == ClassObject {
		return &Object{proto: proto, class: class, flags: flags}
	}
	x := &extObject{Object: Object{proto: proto, class: class, flags: flags}}
	x.embedExt(&x.side)
	return &x.Object
}

// extObject is an object of a kind that always has an ext, with it in the
// same allocation.
type extObject struct {
	Object
	side objectExt
}

// literalObject is an ordinary object with room for its first few properties
// in the same allocation.
//
// An object literal says how many properties it is about to be given, and a
// parsed JSON object is the same shape: the table would otherwise be a second
// allocation for every one of them. The room is wasted on an object with fewer
// properties than this, which is the trade -- a few words against a trip to the
// allocator.
type literalObject struct {
	Object
	inline [3]Value
}

// literalObject1 and literalObject2 are literalObject for an object that says
// it will have one property or two. Each is the next size class down: a pair's
// two fields, the commonest of all, fit in 96 bytes rather than 112.
type literalObject1 struct {
	Object
	inline [1]Value
}

type literalObject2 struct {
	Object
	inline [2]Value
}

// largeLiteralObject is literalObject for an object that says it will have
// four to eight properties, each in the size class its count needs; seven
// takes the room for eight, a size class of its own being no smaller.
type largeLiteralObject[A [4]Value | [5]Value | [6]Value | [8]Value] struct {
	Object
	inline A
}

func newLargeLiteralObject[A [4]Value | [5]Value | [6]Value | [8]Value](o Object) *Object {
	lo := &largeLiteralObject[A]{Object: o}
	lo.vals = unsafe.Slice(&lo.inline[0], len(lo.inline))[:0]
	return &lo.Object
}

// newLiteralObject returns an object that is about to be given n properties.
// One that says none -- a constructor that fills its object in elsewhere --
// gets the room for three.
func newLiteralObject(proto *Object, class Class, n int) *Object {
	o := Object{proto: proto, class: class, flags: objExtensible | objInlineProps}
	switch n {
	case 1:
		lo := &literalObject1{Object: o}
		lo.vals = lo.inline[:0]
		return &lo.Object
	case 2:
		lo := &literalObject2{Object: o}
		lo.vals = lo.inline[:0]
		return &lo.Object
	case 4:
		return newLargeLiteralObject[[4]Value](o)
	case 5:
		return newLargeLiteralObject[[5]Value](o)
	case 6:
		return newLargeLiteralObject[[6]Value](o)
	case 7, 8:
		return newLargeLiteralObject[[8]Value](o)
	}
	if n > len(literalObject{}.inline) {
		o := &Object{proto: proto, class: class, flags: objExtensible}
		o.vals = make([]Value, 0, n)
		return o
	}
	lo := &literalObject{Object: o}
	lo.vals = lo.inline[:0]
	return &lo.Object
}

// errorObject carries the Go wrapper used to propagate this error through the
// interpreter. An Error and its wrapper have the same lifetime, so keeping
// them together avoids a second allocation each time the Error is thrown.
type errorObject struct {
	Object
	side   objectExt
	inline [3]Value
	thrown Thrown
}

func newErrorObject(proto *Object) *Object {
	eo := &errorObject{
		Object: Object{proto: proto, class: ClassError, flags: objExtensible | objInlineProps},
	}
	eo.vals = eo.inline[:0]
	eo.embedExt(&eo.side)
	eo.side.data = &eo.thrown
	return &eo.Object
}

// arrayObject is an array with room for its first few elements in the same
// allocation, which is what an array literal and a small result array need.
//
// Four is what the literals a program writes mostly hold; newArrayStorage
// has larger rooms, up to sixteen, and a longer one gets storage of its
// own, sized exactly.
type arrayObject struct {
	Object
	side   objectExt
	inline [4]Value
}

// newArrayObject returns an array about to be given n elements.
func newArrayObject(proto *Object, n int) *Object {
	o, elems := newArrayStorage(n)
	if o == nil {
		o = newObject(proto, ClassArray)
		o.ext.elems = make([]Value, n)
		return o
	}
	o.proto, o.class, o.flags = proto, ClassArray, objExtensible|objArrayLengthWritable
	o.ext.elems = elems[:n]
	return o
}

// newArrayStorage makes an Object with room for n elements in the same
// allocation, and returns the room, or nil for an n too large for one. Each
// room is sized so that the whole fills one of Go's size classes on a
// 64-bit platform: an array literal of up to sixteen elements, or a small
// result array, is one allocation rather than two, in about the space two
// took.
func newArrayStorage(n int) (*Object, []Value) {
	switch {
	case n <= len(arrayObject{}.inline):
		ao := new(arrayObject)
		ao.embedExt(&ao.side)
		return &ao.Object, ao.inline[:]
	case n <= 6:
		x := new(struct {
			Object
			side   objectExt
			inline [6]Value
		})
		x.embedExt(&x.side)
		return &x.Object, x.inline[:]
	case n <= 8:
		x := new(struct {
			Object
			side   objectExt
			inline [8]Value
		})
		x.embedExt(&x.side)
		return &x.Object, x.inline[:]
	case n <= 10:
		x := new(struct {
			Object
			side   objectExt
			inline [10]Value
		})
		x.embedExt(&x.side)
		return &x.Object, x.inline[:]
	case n <= 12:
		x := new(struct {
			Object
			side   objectExt
			inline [12]Value
		})
		x.embedExt(&x.side)
		return &x.Object, x.inline[:]
	case n <= 16:
		x := new(struct {
			Object
			side   objectExt
			inline [16]Value
		})
		x.embedExt(&x.side)
		return &x.Object, x.inline[:]
	}
	return nil, nil
}

// matchResultObject is the array a regular expression's match gives, with
// room in the same allocation for its index, input, groups and indices, for
// its first few elements, and for the string of what matched: a program that
// matches in a loop makes one each time round, and the matched text is the
// one element every match has.
type matchResultObject struct {
	Object
	side   objectExt
	fields [4]Value
	inline [4]Value
	str    String
}

// matchResult1Object is matchResultObject for the commonest match, of a
// pattern with no groups and no d flag: one element, and index, input and
// groups.
type matchResult1Object struct {
	Object
	side   objectExt
	fields [3]Value
	inline [1]Value
	str    String
}

// newMatchResult returns a match's array, about to be given n elements, and
// room for the string of its first. indices says whether it will have the
// d flag's indices too.
func newMatchResult(proto *Object, n int, indices bool) (*Object, *String) {
	o := Object{
		proto: proto, class: ClassArray,
		flags: objExtensible | objArrayLengthWritable | objInlineProps,
	}
	if n == 1 && !indices {
		mo := &matchResult1Object{Object: o}
		mo.vals = mo.fields[:0]
		mo.embedExt(&mo.side)
		mo.side.elems = mo.inline[:]
		return &mo.Object, &mo.str
	}
	mo := &matchResultObject{Object: o}
	mo.vals = mo.fields[:0]
	mo.embedExt(&mo.side)
	if n <= len(mo.inline) {
		mo.side.elems = mo.inline[:n]
	} else {
		mo.side.elems = make([]Value, n)
	}
	return &mo.Object, &mo.str
}

// funcObject is an object that is also a function.
//
// The two parts are allocated together: a callable object always needs both,
// and a closure created in a loop is common enough that the second allocation
// showed up in the profile.
type funcObject struct {
	Object
	side objectExt
	fn   funcData
}

// newFuncObject creates a callable object and returns its function data, which
// the caller fills in.
func newFuncObject(proto *Object, class Class) (*Object, *funcData) {
	fo := &funcObject{Object: Object{proto: proto, class: class, flags: objExtensible}}
	fo.embedExt(&fo.side)
	fo.side.data = &fo.fn
	return &fo.Object, &fo.fn
}

// funcSlabSize is how many function objects are allocated at a time for the
// built-ins.
//
// A realm has hundreds of them and they all live as long as it does, so they
// are cut from a slab rather than allocated one by one. Nothing else may be
// allocated this way: an object that outlives its neighbours would keep the
// whole slab alive.
const funcSlabSize = 64

// newSlabFuncObject creates a callable object from the runtime's slab, while
// there is one: a function made after the realm is built -- a module's export
// getter, the continuation of an await -- is allocated on its own, since it may
// not outlive its neighbours holding them all alive.
func (r *Runtime) newSlabFuncObject(proto *Object, class Class) (*Object, *funcData) {
	if len(r.funcSlab) == 0 {
		if !r.building {
			return newFuncObject(proto, class)
		}
		r.funcSlab = make([]funcObject, funcSlabSize)
	}
	fo := &r.funcSlab[0]
	r.funcSlab = r.funcSlab[1:]
	fo.Object = Object{proto: proto, class: class, flags: objExtensible}
	fo.embedExt(&fo.side)
	fo.side.data = &fo.fn
	return &fo.Object, &fo.fn
}

// scriptFuncObject is a function with a body of its own, which needs a closure
// that says what it captured. Small variants below carry the capture slots in
// the same allocation without making every function pay for the largest one.
type scriptFuncObject struct {
	funcObject
	cl closure
}

type scriptFuncObject1 struct {
	scriptFuncObject
	captured [1]*upvalue
}

type scriptFuncObject2 struct {
	scriptFuncObject
	captured [2]*upvalue
}

type scriptFuncObject3 struct {
	scriptFuncObject
	captured [3]*upvalue
}

type scriptFuncObject4 struct {
	scriptFuncObject
	captured [4]*upvalue
}

// arrowFuncObject is scriptFuncObject for an arrow, with the surroundings it
// captures, which every arrow has, in the same allocation.
type arrowFuncObject struct {
	scriptFuncObject
	extra funcExtra
}

type arrowFuncObject1 struct {
	arrowFuncObject
	captured [1]*upvalue
}

type arrowFuncObject2 struct {
	arrowFuncObject
	captured [2]*upvalue
}

// newScriptFuncObject creates a callable object together with the closure and
// the upvalue slots that go with it, and, for an arrow, its funcExtra.
func newScriptFuncObject(proto *Object, class Class, upvalues int, arrow bool) (*Object, *funcData, *closure, []*upvalue, *funcExtra) {
	var fo *scriptFuncObject
	var captured []*upvalue
	var extra *funcExtra
	switch {
	case arrow && upvalues == 0:
		x := &arrowFuncObject{}
		fo, extra = &x.scriptFuncObject, &x.extra
	case arrow && upvalues == 1:
		x := &arrowFuncObject1{}
		fo, extra, captured = &x.scriptFuncObject, &x.extra, x.captured[:]
	case arrow && upvalues == 2:
		x := &arrowFuncObject2{}
		fo, extra, captured = &x.scriptFuncObject, &x.extra, x.captured[:]
	case arrow:
		extra = &funcExtra{}
	}
	if fo != nil {
		upvalues = -1
	}
	switch upvalues {
	case -1:
	case 0:
		fo = &scriptFuncObject{}
	case 1:
		x := &scriptFuncObject1{}
		fo, captured = &x.scriptFuncObject, x.captured[:]
	case 2:
		x := &scriptFuncObject2{}
		fo, captured = &x.scriptFuncObject, x.captured[:]
	case 3:
		x := &scriptFuncObject3{}
		fo, captured = &x.scriptFuncObject, x.captured[:]
	case 4:
		x := &scriptFuncObject4{}
		fo, captured = &x.scriptFuncObject, x.captured[:]
	default:
		fo = &scriptFuncObject{}
		captured = make([]*upvalue, upvalues)
	}
	fo.Object = Object{proto: proto, class: class, flags: objExtensible}
	fo.embedExt(&fo.side)
	fo.side.data = &fo.fn
	return &fo.Object, &fo.fn, &fo.cl, captured, extra
}

// Class returns the object's class.
func (o *Object) Class() Class { return o.class }

// Proto returns the object's prototype, which may be nil.
func (o *Object) Proto() *Object { return o.proto }

// IsExtensible reports whether new properties may be added.
func (o *Object) IsExtensible() bool { return o.flags&objExtensible != 0 }

// MarkHTMLDDA gives the object Annex B's [[IsHTMLDDA]] slot. It is for an
// object being made, before any script has seen it: typeof an object does not
// change over its life.
func (o *Object) MarkHTMLDDA() { o.flags |= objHTMLDDA }

// IsCallable reports whether the object can be called.
func (o *Object) IsCallable() bool {
	if _, ok := o.dataOf().(*funcData); ok {
		return true
	}
	// A proxy is callable exactly when its target is, which is what makes the
	// apply and construct traps reachable.
	if p, ok := o.dataOf().(*proxyData); ok {
		return p.callable
	}
	return false
}

// IsArray reports whether the object is an ordinary array.
// isArray answers the same question as IsArray but reports a revoked proxy,
// which has no target left to ask and so cannot be classified at all.
func (r *Runtime) isArray(v Value) (bool, error) {
	if !v.IsObject() {
		return false, nil
	}
	o := v.Object()
	for {
		if o.class == ClassArray {
			return true, nil
		}
		p := proxyOf(o)
		if p == nil {
			return false, nil
		}
		if p.revoked {
			return false, r.throwTypeError("cannot perform an operation on a revoked proxy")
		}
		o = p.target
	}
}

func (o *Object) IsArray() bool {
	// A proxy is an array exactly when its target is, all the way down: what
	// Array.isArray and JSON.stringify both need to decide is whether the thing
	// behaves like an array, not what kind of object it literally is.
	for {
		if o.class == ClassArray {
			return true
		}
		p := proxyOf(o)
		if p == nil {
			return false
		}
		o = p.target
	}
}

// ---------------------------------------------------------------------------
// Own property access
// ---------------------------------------------------------------------------

// propIndex is the lookup table of an object with many properties.
type propIndex struct {
	slots map[Atom]int32
	// recent caches lookups in slots, by the key's low bits. The same few keys
	// are asked for over and over -- the globals a loop reads and writes, the
	// methods of a prototype, the setters an assignment looks for up the chain
	// -- and hashing each time is most of what finding them costs.
	//
	// An entry is a key and where it is, or -1 for a key the object does not
	// have. Where a key is is checked against the table on every use, so a
	// delete or a move makes an entry miss rather than lie; and a key being
	// added overwrites its entry, so that it is never found missing.
	recent [16]recentKey
	// indexKeys is set once a key that is an array index has been added,
	// and stays set: that the table has none is then certain when it is
	// clear.
	indexKeys bool
}

type recentKey struct {
	key Atom
	idx int32
}

func newPropIndex(n int) *propIndex {
	return &propIndex{slots: make(map[Atom]int32, n)}
}

// add records a key's slot.
func (x *propIndex) add(key Atom, i int32) {
	x.slots[key] = i
	x.recent[key&15] = recentKey{key, i}
	if key.IsIndex() {
		x.indexKeys = true
	}
}

// noIndexKeys reports whether the object's table certainly has no key that is
// an array index short enough for an atom to carry, which a large table's
// index knows and a small one is scanned for.
func (o *Object) noIndexKeys() bool {
	if x := o.shapeIndex(); x != nil {
		return !x.indexKeys
	}
	for _, k := range o.keys() {
		if k.key.IsIndex() {
			return false
		}
	}
	return true
}

// keys is the keys and attributes of the object's properties, by position:
// its shape's, as many as it has values.
func (o *Object) keys() []propKey {
	if len(o.vals) == 0 {
		return nil
	}
	return o.shape.keys[:len(o.vals)]
}

// propAt is the property at position i of the table.
func (o *Object) propAt(i int32) Property {
	k := o.shape.keys[i]
	return Property{key: k.key, flags: k.flags, value: o.vals[i]}
}

// setFlagsAt changes the attributes of the property at position i, and so
// the object's layout.
func (o *Object) setFlagsAt(i int32, flags propFlags) {
	if o.shape.keys[i].flags == flags {
		return
	}
	if t := o.shape.reconfigured(i, flags); t != nil {
		o.shape = t
		if t.index == nil && len(o.vals) > linearScanLimit {
			o.buildIndex()
		}
		if verifyShapes {
			checkShape(o)
		}
		return
	}
	o.layoutChanged()
	o.shape.keys[i].flags = flags
}

// findOwn returns the position of an own property in the table, or -1.
func (o *Object) findOwn(key Atom) int32 {
	if x := o.shapeIndex(); x != nil {
		keys := o.keys()
		c := &x.recent[key&15]
		if c.key == key {
			if c.idx < 0 {
				return -1
			}
			if int(c.idx) < len(keys) {
				if k := keys[c.idx]; k.key == key && k.flags&propDeleted == 0 {
					return c.idx
				}
			}
		}
		i, ok := x.slots[key]
		if !ok {
			i = -1
		}
		// What the index says is remembered, whatever this table holds there:
		// a borrowed index is shared with the layout's objects.
		c.key, c.idx = key, i
		if i >= 0 && (int(i) >= len(keys) || keys[i].key != key || keys[i].flags&propDeleted != 0) {
			// A key the object deleted, which the index it borrows has.
			return -1
		}
		return i
	}
	for i, k := range o.keys() {
		if k.key == key && k.flags&propDeleted == 0 {
			return int32(i)
		}
	}
	return -1
}

// buildIndex populates the lookup map once an object has enough properties for
// it to pay off. The index is the shape's.
func (o *Object) buildIndex() {
	x := newPropIndex(len(o.vals) * 2)
	for i, k := range o.keys() {
		if k.flags&propDeleted == 0 {
			x.add(k.key, int32(i))
		}
	}
	o.shape.index = x
	o.shape.borrowed = false
}

// getOwn returns an own property, excluding dense elements.
//
// It sees private members, so it is the lookup the private accessors use.
// Every path reachable from source goes through getOwnVisible instead.
func (o *Object) getOwn(key Atom) ownProp {
	return ownProp{o, o.findOwn(key)}
}

// getOwnVisible returns an own property that source can observe, which excludes
// private class members.
func (o *Object) getOwnVisible(key Atom) ownProp {
	p := o.getOwn(key)
	if p.ok() && p.flags()&propPrivate != 0 {
		p.i = -1
	}
	return p
}

// reserveProps makes room for n properties in all, so that an object built
// all at once grows its table once rather than at every doubling -- whether
// or not it has a table yet: a constructor is given its prototype before its
// methods, and an object spread into one is given all its keys. A table in
// the object's own allocation too small for them leaves it, as
// leaveInlineProps does.
func (o *Object) reserveProps(n int) {
	if cap(o.vals) >= n {
		return
	}
	vals := make([]Value, len(o.vals), n)
	copy(vals, o.vals)
	if o.flags&objInlineProps != 0 {
		o.shape.noteGrowth(n)
		clear(o.vals[:cap(o.vals)])
		o.flags &^= objInlineProps
	}
	o.vals = vals
}

// setOwnRaw installs a property, replacing any existing one, without consulting
// the prototype chain or any setter.
func (o *Object) setOwnRaw(key Atom, value Value, flags propFlags) {
	if i := o.findOwn(key); i >= 0 {
		o.setFlagsAt(i, flags)
		o.vals[i] = value
		return
	}
	if o.class == ClassArray && key.IsIndex() {
		// An element in the property table is what the sparse mark means. It is
		// set here, where such an element is created, so that nothing looks for
		// this index in dense storage afterwards and finds a second answer.
		o.markSparse()
	}
	o.appendProp(Property{key: key, flags: flags, value: value})
}

// prependProps inserts properties ahead of every existing one.
//
// Only a function's synthesized length and name need this, and only once per
// function, so the shift and the index rebuild are not worth avoiding. Only a
// function's table may be shifted: a global name's site keeps the slot its
// name was found at for good, which holds that name or a tombstone only
// while no table that is an environment is ever shifted (see propCache).
func (o *Object) prependProps(ps []Property) {
	if o.fn() == nil {
		panic("prependProps on an object that is no function")
	}
	if len(ps) == 0 {
		return
	}
	for i := len(ps) - 1; i >= 0; i-- {
		o.insertProp(0, ps[i])
	}
}

// leaveInlineProps moves the properties out of the room the object carries
// for them, with room past them for more of them, and clears the room.
func (o *Object) leaveInlineProps(more int) {
	// Its constructor's next objects are given the room this one needed.
	o.shape.noteGrowth(len(o.vals) + more)
	old := o.vals[:cap(o.vals)]
	vals := make([]Value, len(o.vals), 2*cap(o.vals)+more)
	copy(vals, o.vals)
	o.vals = vals
	clear(old)
	o.flags &^= objInlineProps
}

// insertProp puts a property at a given position rather than at the end, which
// a property created later than it would have been needs: where a key sits in
// the table is where an ownKeys walk reports it. As with prependProps, only a
// function's table may be shifted.
func (o *Object) insertProp(at int, p Property) {
	if o.fn() == nil {
		panic("insertProp on an object that is no function")
	}
	if at >= len(o.vals) {
		o.appendProp(p)
		return
	}
	o.layoutChanged()
	if o.flags&objInlineProps != 0 && len(o.vals) == cap(o.vals) {
		o.leaveInlineProps(1)
	}
	s := o.shape
	o.vals = slices.Insert(o.vals, at, p.value)
	s.keys = slices.Insert(s.keys[:s.n], at, propKey{p.key, p.flags})
	s.n = int32(len(o.vals))
	if o.shapeIndex() != nil || len(o.vals) > linearScanLimit {
		o.buildIndex()
	}
}

func (o *Object) appendProp(p Property) {
	if o.flags&objInlineProps != 0 && len(o.vals) == cap(o.vals) {
		o.leaveInlineProps(1)
	}
	o.vals = append(o.vals, p.value)
	o.shapeAdded(propKey{p.key, p.flags})
}

// deleteOwn removes an own property, reporting whether it existed and was
// configurable.
func (o *Object) deleteOwn(key Atom) bool {
	i := o.findOwn(key)
	if i < 0 {
		return true // deleting an absent property succeeds
	}
	if o.shape.keys[i].flags&propConfigurable == 0 {
		return false
	}
	if s := o.shape; !s.unique && !s.loose && s.parent != nil && i == s.n-1 {
		// The key last added to a shared layout: the object goes back to
		// the layout it had before, as V8 rolls a map back along its
		// transition, rather than to one of its own.
		o.vals[i] = Value{}
		o.vals = o.vals[:i]
		o.shape = s.parent
		if verifyShapes {
			checkShape(o)
		}
		return true
	}
	o.tombstone(i)
	return true
}

// tombstone deletes the property at position i of the table, leaving a
// tombstone rather than splicing it out, so that the remaining positions stay
// valid.
func (o *Object) tombstone(i int32) {
	key := o.shape.keys[i].key
	o.layoutChanged()
	o.shape.keys[i] = propKey{key: atomEmpty, flags: propDeleted}
	o.vals[i] = Value{}
	if x := o.shapeIndex(); x != nil && !o.shape.borrowed {
		delete(x.slots, key)
	}
}

// ownKeys returns the object's own property keys in specification order:
// integer indices in ascending numeric order, then string keys in insertion
// order, then symbol keys in insertion order.
func (o *Object) ownKeys(includeSymbols bool, atoms *atomTable) []Atom {
	keys := make([]Atom, 0, len(o.elemsOf())+len(o.vals)+2)

	// A typed array's elements are backed by a buffer rather than by the
	// property table, but they are own properties and must be listed.
	if o.class == ClassTypedArray {
		if t, ok := o.dataOf().(*typedArrayData); ok {
			for i := 0; i < t.count(); i++ {
				keys = append(keys, atoms.indexAtom(uint32(i)))
			}
		}
	}

	// A String object's characters are own properties too, indexed and
	// enumerable, which is what makes Object.keys(new String("ab")) list them.
	// Its length is one as well, synthesized on demand like a function's.
	if o.class == ClassStringWrapper {
		if s, ok := o.dataOf().(*String); ok {
			for i := 0; i < s.Len(); i++ {
				keys = append(keys, atoms.indexAtom(uint32(i)))
			}
		}
	}

	// Dense elements are already in ascending index order.
	dense := len(keys)
	for i, v := range o.elemsOf() {
		if v.ref == nil && v.IsUninitialized() {
			// A hole in a dense array is not an own property.
			continue
		}
		keys = append(keys, atoms.indexAtom(uint32(i)))
	}

	// Index-valued keys stored in the property table must be merged in
	// ascending order ahead of the string keys. The largest indices are
	// spelled out rather than carried in the atom, and they belong with the
	// rest.
	var indexKeys []indexedAtom
	for _, p := range o.keys() {
		if p.flags&propDeleted != 0 {
			continue
		}
		if ix, ok := atoms.arrayIndex(p.key); ok {
			indexKeys = append(indexKeys, indexedAtom{p.key, ix})
		}
	}
	if len(indexKeys) > 0 {
		// They go among the dense elements, not after them: an element
		// given attributes or an accessor moves to the table, and is still
		// where its index puts it.
		sortIndexedAtoms(indexKeys)
		elems := append([]Atom(nil), keys[dense:]...)
		keys = keys[:dense]
		i := 0
		for _, k := range indexKeys {
			for i < len(elems) && elems[i].Index() < k.index {
				keys = append(keys, elems[i])
				i++
			}
			keys = append(keys, k.atom)
		}
		keys = append(keys, elems[i:]...)
	}

	// An array's length is synthesized rather than stored, but it is an own
	// property, and it was the first one the array had -- so it comes after the
	// indices, which always come first, and before every other string key. A
	// string object's length is in the same position, after any index a script
	// added past the end of the string.
	if o.class == ClassArray {
		keys = append(keys, atomLength)
	}
	if o.class == ClassStringWrapper && !o.getOwn(atomLength).ok() {
		if _, ok := o.dataOf().(*String); ok {
			keys = append(keys, atomLength)
		}
	}

	// A function's length and name are synthesized on demand, but they are own
	// properties and must be listed -- before its other string keys, in that
	// order, as they would have been had the function been built with them, and
	// after the indices, which always come first however late they were added.
	if o.class == ClassFunction && o.fn() != nil && !o.fn().propsMaterialized {
		if !o.getOwn(atomLength).ok() {
			keys = append(keys, atomLength)
		}
		if !o.getOwn(atomName).ok() {
			keys = append(keys, atomName)
		}
	}

	for _, p := range o.keys() {
		if p.flags&(propDeleted|propPrivate) != 0 {
			continue
		}
		if _, isIndex := atoms.arrayIndex(p.key); isIndex {
			continue
		}
		if atoms.IsSymbol(p.key) {
			continue
		}
		keys = append(keys, p.key)
	}

	if includeSymbols {
		for _, p := range o.keys() {
			if p.flags&(propDeleted|propPrivate) != 0 || p.key.IsIndex() {
				continue
			}
			if atoms.IsSymbol(p.key) {
				keys = append(keys, p.key)
			}
		}
	}
	return keys
}

// indexedAtom pairs a key with the index it names, so that keys spelled out as
// names sort with the ones carried in an atom.
type indexedAtom struct {
	atom  Atom
	index uint32
}

func sortIndexedAtoms(a []indexedAtom) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j].index > v.index {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

// ---------------------------------------------------------------------------
// Dense elements
// ---------------------------------------------------------------------------

// elemHole is the marker stored in a dense element slice for an array hole.
// It is the uninitialized value, which cannot otherwise appear in user data.
var elemHole = uninitialized

// isHole reports whether a dense element slot is empty. It compares the
// whole of the value's bits with the hole's: the other uninitialized values
// -- the markers of a Map's or a WeakMap's deleted entries -- are never
// elements, and one comparison keeps elemAt and setElem, which ask it of
// every element a tree reads or writes, small enough for Go to inline.
func isHole(v Value) bool { return math.Float64bits(v.num) == holeBits }

// holeBits is elemHole's bits: an uninitialized value of payload 0.
const holeBits = tagBase | uint64(KindUninitialized)

// getElem returns a dense element and whether the slot is present.
func (o *Object) getElem(i uint32) (Value, bool) {
	if uint(i) >= uint(len(o.elemsOf())) {
		return Undefined, false
	}
	v := o.elemsOf()[i]
	if isHole(v) {
		return Undefined, false
	}
	if o.flags&objMappedArguments != 0 {
		if u := o.argumentBinding(i); u != nil {
			// The parameter is what the index names; the stored value is only
			// there so that the index exists as a property.
			return u.get(), true
		}
	}
	return v, true
}

// argumentBinding returns the parameter a mapped arguments object's index
// aliases, or nil when the index does not alias one.
func (o *Object) argumentBinding(i uint32) *upvalue {
	d, _ := o.dataOf().(*argumentsData)
	if d == nil || uint(i) >= uint(len(d.mapped)) {
		return nil
	}
	return d.mapped[i]
}

// unmapArgument breaks the alias between an index and its parameter, which
// deleting the property or redefining it as anything but a writable data
// property does.
func (o *Object) unmapArgument(i uint32) {
	if o.flags&objMappedArguments == 0 {
		return
	}
	if d, _ := o.dataOf().(*argumentsData); d != nil && uint(i) < uint(len(d.mapped)) {
		d.mapped[i] = nil
	}
}

// setElem stores a dense element, growing the slice when the write is at or
// just past the end.
//
// A write far past the end would leave a large run of holes, so the object
// falls back to storing the element as an ordinary property instead. The
// threshold bounds the memory a single sparse write can commit.
func (o *Object) setElem(i uint32, v Value) bool {
	if o.flags&objMappedArguments != 0 && uint(i) < uint(len(o.elemsOf())) {
		if u := o.argumentBinding(i); u != nil {
			// The value goes to the parameter, and to the slot as well so that
			// the index stays present.
			u.set(v)
			o.extOf().elems[i] = v
			return true
		}
	}
	switch {
	case uint(i) < uint(len(o.elemsOf())):
		o.extOf().elems[i] = v
		return true
	case uint(i) == uint(len(o.elemsOf())):
		o.extOf().elems = append(o.elemsOf(), v)
		return true
	}

	const maxHoleRun = 1024
	// Unsigned, as the index is: an int is 32 bits on some platforms.
	if uint(i)-uint(len(o.elemsOf())) > maxHoleRun {
		return false
	}
	for uint(len(o.elemsOf())) < uint(i) {
		o.extOf().elems = append(o.elemsOf(), elemHole)
	}
	o.extOf().elems = append(o.elemsOf(), v)
	return true
}

// arrayLength returns an array's length, which is the dense element count plus
// any higher index properties stored sparsely.
func (o *Object) arrayLength() uint32 {
	n := uint32(len(o.elemsOf()))
	if o.flags&objHasSparseElements != 0 && o.arrayLen > n {
		return o.arrayLen
	}
	return n
}

// markSparse moves an array out of dense storage, recording the length it had.
//
// The length has to be remembered rather than derived from the keys present,
// because they no longer tell you: `a.length = 4294967295` leaves an array with
// no keys at all and a length of four billion, and `delete a[a.length - 1]`
// leaves one whose highest key is below its length.
//
// It must be called before the dense elements are moved or dropped.
func (o *Object) markSparse() {
	if o.flags&objHasSparseElements == 0 {
		o.arrayLen = uint32(len(o.elemsOf()))
		o.flags |= objHasSparseElements
	}
}

// noteArrayIndex extends an array's length to cover an index stored outside its
// dense elements.
func (o *Object) noteArrayIndex(i uint32) {
	o.markSparse()
	if i+1 > o.arrayLen {
		o.arrayLen = i + 1
	}
}

// setArrayLength truncates or extends an array.
func (o *Object) setArrayLength(n uint32) {
	switch {
	case uint(n) < uint(len(o.elemsOf())):
		// Truncating releases the discarded values for collection. One
		// element, as a loop shortening an array one at a time takes off,
		// is a store rather than a call of the runtime's bulk clear.
		if uint(n)+1 == uint(len(o.elemsOf())) {
			o.extOf().elems[n] = Value{}
		} else {
			clear(o.elemsOf()[n:])
		}
		o.extOf().elems = o.elemsOf()[:n]
		if o.flags&objHasSparseElements != 0 {
			o.arrayLen = n
		}
	case uint(n) > uint(len(o.elemsOf())):
		// Extending an array only makes it longer; the new slots are holes.
		// Beyond a short run they are not materialized at all, since a length
		// of four billion is a number rather than four billion holes -- unless
		// the array keeps at least as many elements as holes, as V8 keeps an
		// array's elements fast while they are at least half used: a vector
		// that doubles its storage by its length, as AWFY's Vector does past
		// a thousand elements, had become sparse at each doubling, and every
		// element written to it after a property.
		if holes := uint(n) - uint(len(o.elemsOf())); o.flags&objHasSparseElements == 0 && holes <= max(maxHoleRun, uint(len(o.elemsOf()))) {
			// Grown once to the length, the holes written in: appended one
			// at a time, new Array(4) -- AWFY's Storage makes its trees of
			// them -- had grown its storage three times.
			old := len(o.elemsOf())
			e := slices.Grow(o.elemsOf(), int(holes))[:old+int(holes)]
			for i := old; i < len(e); i++ {
				e[i] = elemHole
			}
			o.extOf().elems = e
		} else {
			o.markSparse()
			o.arrayLen = n
		}
	default:
		if o.flags&objHasSparseElements != 0 {
			o.arrayLen = n
		}
	}
	if o.flags&objHasSparseElements != 0 {
		// Drop any sparse index properties beyond the new length.
		for i, p := range o.keys() {
			if key := p.key; p.flags&propDeleted == 0 && key.IsIndex() && key.Index() >= n {
				o.tombstone(int32(i))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Function objects
// ---------------------------------------------------------------------------

// funcData holds the internal slots that make an object callable.
type funcData struct {
	// native is set for a function implemented in Go.
	native NativeFunc
	// closure is set for a function compiled from JavaScript source.
	closure *closure

	// name and length back the eponymous properties, which are created lazily
	// because most functions are never asked for them.
	name string

	// realm is the realm a built-in belongs to, which it runs in whoever
	// calls it. A compiled function's is its closure's; a bound function has
	// its target's.
	realm *Realm
	// homeObject is the object a method was defined on, which `super` resolves
	// against.
	homeObject *Object
	// ctorShape is the empty shape the objects the function constructs start
	// from, made when it first constructs one; see siteRoot.
	ctorShape *shape
	// length is the function's length, as its own length property, before
	// one is made, says (propsMaterialized). It is an int32 beside the
	// flags, in what their padding had been: a parameter list, or a
	// built-in's declared length, is never longer.
	length int32
	// ctorKind says how the function responds to `new`. It sits with the
	// flags below, which share its word.
	ctorKind ctorKind
	// propsMaterialized records that name and length have been turned into real
	// properties, after which the synthesized reads must stop -- otherwise
	// deleting one would have no effect.
	propsMaterialized bool
	// protoPending records that an ordinary function still owes the .prototype
	// object it is defined to have. It is built by the first thing that asks,
	// which for most functions is nothing at all.
	protoPending bool

	// mathOp marks a Math function a call applies itself, without a frame,
	// to numbers: one of one number by its place in unaryMath, or max and
	// min of two (mathMax, mathMin); see mathCall.
	mathOp uint8
	// elemOp marks a built-in a call applies without a frame, as it calls
	// nothing and cannot throw: Array.prototype.push or pop, a Map's or a
	// Set's lookup or addition, Math.random; see pureElemOp.
	elemOp uint8
	// iterNext marks a built-in iterator's next -- a Map or Set iterator's,
	// or a generator's -- which a loop over the iterator steps itself,
	// without the result object; see stepIter.
	iterNext uint8

	// arrow marks a function with no bindings of its own. An arrow does not
	// get this, new.target, super or arguments from its call: it uses the ones
	// in scope where it was written, which is the whole reason to reach for
	// one instead of an ordinary function.
	arrow bool
	// bound marks a function produced by Function.prototype.bind.
	bound bool
	// treePlanned records that planTreeCall has decided about the function.
	treePlanned bool
	// treeRest marks a function whose calls callRest makes on its tree
	// (callTreeWith), which callTree does not: one that ends in a tail
	// call, or an arrow; see planTreeCall.
	treeRest bool

	// extra is what only some functions have -- an arrow's surroundings, a
	// bound function's target, the scopes of one made inside a `with` body
	// or beside a direct eval -- or nil. Every built-in and most closures have
	// none, and are the smaller for it.
	extra *funcExtra
	// treeCall is the tree a call of the function goes straight to through
	// callTree, or nil; see planTreeCall.
	treeCall *tree

	// superCtor is the class constructor whose prototype super() reads.
	//
	// It is the function rather than the parent because the link is live:
	// Object.setPrototypeOf on a class changes what super() calls. An arrow
	// written inside a derived constructor inherits it, since its super() is
	// the constructor's.
	superCtor *Object
	// fieldInit is a class's instance initializer: the function that installs
	// the private methods and runs the field initializers on a new instance.
	//
	// It is a function of its own rather than a prefix of the constructor
	// because of when it runs. A base class runs it before the body; a derived
	// one runs it inside super(), wherever in the body that call is and however
	// deep in an arrow it was written, and only for the call that binds `this`.
	fieldInit *Object
}

// funcExtra is the part of a function's data that only some functions
// have; see funcData.extra.
type funcExtra struct {
	// lexThis and lexNewTarget hold what an arrow captured from the frame
	// that created it.
	lexThis      Value
	lexNewTarget Value
	// lexThisRef is the binding an arrow captured when it was created inside a
	// derived constructor, where `this` is not a value until super() runs.
	lexThisRef *thisBinding
	// rare is what few functions have: the scopes of a function made in a
	// with body or after a direct eval, and what a bound function calls.
	// It is apart, so that an arrow, which has a funcExtra in its own
	// allocation, does not carry it: an arrow capturing a binding of its
	// maker's had been 464 bytes, in Go's 480-byte size class, and DeltaBlue
	// and Havlak make one for each forEach and each loop over a set.
	rare *funcRare
}

// funcRare is the part of a funcExtra that only a few functions have.
type funcRare struct {
	// lexEvalVars is where a direct eval had put the bindings it declared in
	// the function this one was created inside, which this one can still see.
	lexEvalVars *Object
	// lexWith are the `with` objects in scope where the function was created.
	// Names in its body resolve against them, so they outlive the frame that
	// pushed them.
	lexWith []*Object

	// boundTarget, boundThis and boundArgs are set for a function produced by
	// Function.prototype.bind.
	boundTarget *Object
	boundThis   Value
	boundArgs   []Value
}

// with is lexWith, for a function that has any.
func (x *funcExtra) with() []*Object {
	if x == nil || x.rare == nil {
		return nil
	}
	return x.rare.lexWith
}

// evalVars is lexEvalVars, for a function that has them.
func (x *funcExtra) evalVars() *Object {
	if x == nil || x.rare == nil {
		return nil
	}
	return x.rare.lexEvalVars
}

// scopesOf is the funcRare of a function made where the with objects and a
// direct eval's bindings are these, or nil where there are none.
func scopesOf(with []*Object, evalVars *Object) *funcRare {
	if with == nil && evalVars == nil {
		return nil
	}
	return &funcRare{lexWith: with, lexEvalVars: evalVars}
}

// boundExtra is a bound function's funcExtra and funcRare, made in one
// allocation.
type boundExtra struct {
	funcExtra
	r funcRare
}

type ctorKind uint8

const (
	// ctorNone marks a function that cannot be constructed: an arrow, a method,
	// an accessor or most native functions.
	ctorNone ctorKind = iota
	ctorBase
	ctorDerived
)

// NativeFunc is a function implemented in Go.
//
// It returns a value and an error. Returning an error is how a native
// implementation throws: the interpreter converts it into a JavaScript
// exception, preserving a *Thrown unchanged so that a thrown JavaScript value
// survives a trip through Go code.
type NativeFunc func(rt *Runtime, this Value, args []Value) (Value, error)

// fn returns the object's function data, or nil if it is not callable.
func (o *Object) fn() *funcData {
	f, _ := o.dataOf().(*funcData)
	return f
}
