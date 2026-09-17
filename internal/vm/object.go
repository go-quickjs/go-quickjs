package vm

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
	// propNamespaceTag marks a namespace's Symbol.toStringTag, which is the one
	// property of one that is not an export.
	propNamespaceTag
)

// propDefault is the attribute set for an ordinary assignment.
const propDefault = propWritable | propEnumerable | propConfigurable

// accessor holds the getter and setter of an accessor property.
type accessor struct {
	getter *Object
	setter *Object
}

// Property is one own property of an object.
//
// An accessor stores its getter and setter in an *accessor hung off the value's
// reference field, so that the common data property costs no more than a value
// plus a word.
type Property struct {
	key   Atom
	flags propFlags
	value Value
}

func (p *Property) isAccessor() bool { return p.flags&propAccessor != 0 }

func (p *Property) getterSetter() *accessor {
	a, _ := p.value.ref.(*accessor)
	return a
}

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
)

// linearScanLimit is the property count below which lookup scans the slice
// instead of consulting a map.
//
// Most objects are small, and for a handful of properties a linear scan over a
// contiguous slice of 32-byte entries beats a map probe: it touches one or two
// cache lines and needs no hashing. The index map is built only once an object
// grows past this.
const linearScanLimit = 8

// Object is a JavaScript object.
type Object struct {
	proto *Object

	// props holds named and symbol-keyed properties in insertion order, which
	// is the order enumeration must report them in.
	props []Property
	// index maps a key to its slot in props, built lazily once the object grows
	// past linearScanLimit.
	index map[Atom]int32

	// elems holds dense array elements. Arrays and other index-keyed objects
	// keep their elements here rather than in props, so that a[i] is a slice
	// index rather than a map lookup.
	elems []Value

	// arrayLen is an array's length once it stops being dense. It cannot be
	// derived from the keys present at that point, because a length may be far
	// beyond the highest index -- `a.length = 4294967295` leaves no keys at all
	// -- and because an array is not required to have its last index present.
	arrayLen uint32

	class Class
	flags objFlags

	// data carries the internal slots of an exotic object: *funcData for a
	// function, *dateData for a Date, and so on. It is nil for an ordinary
	// object.
	data any
}

// newObject returns a bare object with the given prototype and class.
func newObject(proto *Object, class Class) *Object {
	flags := objExtensible
	if class == ClassArray {
		// An array's length is synthesized rather than stored, so its
		// writability is a flag rather than a property attribute, and every
		// array starts with it writable.
		flags |= objArrayLengthWritable
	}
	return &Object{proto: proto, class: class, flags: flags}
}

// Class returns the object's class.
func (o *Object) Class() Class { return o.class }

// Proto returns the object's prototype, which may be nil.
func (o *Object) Proto() *Object { return o.proto }

// IsExtensible reports whether new properties may be added.
func (o *Object) IsExtensible() bool { return o.flags&objExtensible != 0 }

// IsCallable reports whether the object can be called.
func (o *Object) IsCallable() bool {
	if _, ok := o.data.(*funcData); ok {
		return true
	}
	// A proxy is callable exactly when its target is, which is what makes the
	// apply and construct traps reachable.
	if p, ok := o.data.(*proxyData); ok {
		return p.target.IsCallable()
	}
	return false
}

// IsArray reports whether the object is an ordinary array.
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

// findOwn returns the index of an own property in props, or -1.
func (o *Object) findOwn(key Atom) int32 {
	if o.index != nil {
		if i, ok := o.index[key]; ok {
			return i
		}
		return -1
	}
	for i := range o.props {
		if o.props[i].key == key && o.props[i].flags&propDeleted == 0 {
			return int32(i)
		}
	}
	return -1
}

// buildIndex populates the lookup map once an object has enough properties for
// it to pay off.
func (o *Object) buildIndex() {
	o.index = make(map[Atom]int32, len(o.props)*2)
	for i := range o.props {
		if o.props[i].flags&propDeleted == 0 {
			o.index[o.props[i].key] = int32(i)
		}
	}
}

// getOwn returns an own property, excluding dense elements.
//
// It sees private members, so it is the lookup the private accessors use.
// Every path reachable from source goes through getOwnVisible instead.
func (o *Object) getOwn(key Atom) *Property {
	if i := o.findOwn(key); i >= 0 {
		return &o.props[i]
	}
	return nil
}

// getOwnVisible returns an own property that source can observe, which excludes
// private class members.
func (o *Object) getOwnVisible(key Atom) *Property {
	p := o.getOwn(key)
	if p != nil && p.flags&propPrivate != 0 {
		return nil
	}
	return p
}

// setOwnRaw installs a property, replacing any existing one, without consulting
// the prototype chain or any setter.
func (o *Object) setOwnRaw(key Atom, value Value, flags propFlags) {
	if i := o.findOwn(key); i >= 0 {
		p := &o.props[i]
		p.value, p.flags = value, flags
		return
	}
	o.appendProp(Property{key: key, flags: flags, value: value})
}

func (o *Object) appendProp(p Property) {
	o.props = append(o.props, p)
	switch {
	case o.index != nil:
		o.index[p.key] = int32(len(o.props) - 1)
	case len(o.props) > linearScanLimit:
		o.buildIndex()
	}
}

// deleteOwn removes an own property, reporting whether it existed and was
// configurable.
func (o *Object) deleteOwn(key Atom) bool {
	i := o.findOwn(key)
	if i < 0 {
		return true // deleting an absent property succeeds
	}
	if o.props[i].flags&propConfigurable == 0 {
		return false
	}
	// Tombstone rather than splice, so that the remaining indices stay valid.
	o.props[i] = Property{key: atomEmpty, flags: propDeleted}
	if o.index != nil {
		delete(o.index, key)
	}
	return true
}

// ownKeys returns the object's own property keys in specification order:
// integer indices in ascending numeric order, then string keys in insertion
// order, then symbol keys in insertion order.
func (o *Object) ownKeys(includeSymbols bool, atoms *atomTable) []Atom {
	keys := make([]Atom, 0, len(o.elems)+len(o.props)+2)

	// A typed array's elements are backed by a buffer rather than by the
	// property table, but they are own properties and must be listed.
	if o.class == ClassTypedArray {
		if t, ok := o.data.(*typedArrayData); ok {
			for i := 0; i < t.count(); i++ {
				keys = append(keys, internIndex(uint32(i)))
			}
		}
	}

	// A String object's characters are own properties too, indexed and
	// enumerable, which is what makes Object.keys(new String("ab")) list them.
	// Its length is one as well, synthesized on demand like a function's.
	if o.class == ClassStringWrapper {
		if s, ok := o.data.(*String); ok {
			for i := 0; i < s.Len(); i++ {
				keys = append(keys, internIndex(uint32(i)))
			}
			if o.getOwn(atomLength) == nil {
				keys = append(keys, atomLength)
			}
		}
	}

	// A function's length and name are synthesized on demand, but they are own
	// properties and must be listed -- first, in that order, as they would have
	// been had the function been built with them.
	if o.class == ClassFunction && o.fn() != nil && !o.fn().propsMaterialized {
		if o.getOwn(atomLength) == nil {
			keys = append(keys, atomLength)
		}
		if o.getOwn(atomName) == nil {
			keys = append(keys, atomName)
		}
	}

	// Dense elements are already in ascending index order.
	for i, v := range o.elems {
		if v.ref == nil && v.IsUninitialized() {
			// A hole in a dense array is not an own property.
			continue
		}
		keys = append(keys, internIndex(uint32(i)))
	}

	// Index-valued keys stored in props must be merged in ascending order
	// ahead of the string keys.
	var indexKeys []Atom
	for i := range o.props {
		p := &o.props[i]
		if p.flags&propDeleted != 0 {
			continue
		}
		if p.key.IsIndex() {
			indexKeys = append(indexKeys, p.key)
		}
	}
	if len(indexKeys) > 0 {
		sortAtomsByIndex(indexKeys)
		keys = append(keys, indexKeys...)
	}

	// An array's length is synthesized rather than stored, but it is an own
	// property, and it was the first one the array had -- so it comes after the
	// indices, which always come first, and before every other string key.
	if o.class == ClassArray {
		keys = append(keys, atomLength)
	}

	for i := range o.props {
		p := &o.props[i]
		if p.flags&(propDeleted|propPrivate) != 0 || p.key.IsIndex() {
			continue
		}
		if atoms.IsSymbol(p.key) {
			continue
		}
		keys = append(keys, p.key)
	}

	if includeSymbols {
		for i := range o.props {
			p := &o.props[i]
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

// sortAtomsByIndex sorts index atoms ascending. Insertion sort is used because
// the slice is almost always tiny; an object with many index properties keeps
// them in elems instead.
func sortAtomsByIndex(a []Atom) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j].Index() > v.Index() {
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

// isHole reports whether a dense element slot is empty.
func isHole(v Value) bool { return v.IsUninitialized() }

// getElem returns a dense element and whether the slot is present.
func (o *Object) getElem(i uint32) (Value, bool) {
	if int(i) >= len(o.elems) {
		return Undefined, false
	}
	v := o.elems[i]
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
	d, _ := o.data.(*argumentsData)
	if d == nil || int(i) >= len(d.mapped) {
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
	if d, _ := o.data.(*argumentsData); d != nil && int(i) < len(d.mapped) {
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
	if o.flags&objMappedArguments != 0 && int(i) < len(o.elems) {
		if u := o.argumentBinding(i); u != nil {
			// The value goes to the parameter, and to the slot as well so that
			// the index stays present.
			u.set(v)
			o.elems[i] = v
			return true
		}
	}
	switch {
	case int(i) < len(o.elems):
		o.elems[i] = v
		return true
	case int(i) == len(o.elems):
		o.elems = append(o.elems, v)
		return true
	}

	const maxHoleRun = 1024
	if int(i)-len(o.elems) > maxHoleRun {
		return false
	}
	for len(o.elems) < int(i) {
		o.elems = append(o.elems, elemHole)
	}
	o.elems = append(o.elems, v)
	return true
}

// arrayLength returns an array's length, which is the dense element count plus
// any higher index properties stored sparsely.
func (o *Object) arrayLength() uint32 {
	n := uint32(len(o.elems))
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
		o.arrayLen = uint32(len(o.elems))
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
	case int(n) < len(o.elems):
		// Truncating releases the discarded values for collection.
		clear(o.elems[n:])
		o.elems = o.elems[:n]
		if o.flags&objHasSparseElements != 0 {
			o.arrayLen = n
		}
	case int(n) > len(o.elems):
		// Extending an array only makes it longer; the new slots are holes.
		// Beyond a short run they are not materialized at all, since a length
		// of four billion is a number rather than four billion holes.
		const maxHoleRun = 1024
		if o.flags&objHasSparseElements == 0 && int(n)-len(o.elems) <= maxHoleRun {
			for len(o.elems) < int(n) {
				o.elems = append(o.elems, elemHole)
			}
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
		for i := range o.props {
			p := &o.props[i]
			if p.flags&propDeleted == 0 && p.key.IsIndex() && p.key.Index() >= n {
				o.props[i] = Property{key: atomEmpty, flags: propDeleted}
				if o.index != nil {
					delete(o.index, p.key)
				}
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
	name   string
	length int

	// ctorKind says how the function responds to `new`.
	ctorKind ctorKind
	// homeObject is the object a method was defined on, which `super` resolves
	// against.
	homeObject *Object
	// propsMaterialized records that name and length have been turned into real
	// properties, after which the synthesized reads must stop -- otherwise
	// deleting one would have no effect.
	propsMaterialized bool

	// arrow marks a function with no bindings of its own. An arrow does not
	// get this, new.target, super or arguments from its call: it uses the ones
	// in scope where it was written, which is the whole reason to reach for
	// one instead of an ordinary function.
	arrow bool
	// lexThis, lexNewTarget and lexArgs hold what an arrow captured from the
	// frame that created it.
	lexThis Value
	// lexThisRef is the binding an arrow captured when it was created inside a
	// derived constructor, where `this` is not a value until super() runs.
	lexThisRef *thisBinding
	// lexWith are the `with` objects in scope where the function was created.
	// Names in its body resolve against them, so they outlive the frame that
	// pushed them.
	lexWith      []*Object
	lexNewTarget Value
	lexArgs      []Value

	// boundTarget, boundThis and boundArgs are set for a function produced by
	// Function.prototype.bind.
	boundTarget *Object
	boundThis   Value
	boundArgs   []Value
	// parentCtor is the constructor a derived class's super() invokes.
	parentCtor *Object
	// fields holds a class's instance field initializers, run by the
	// constructor before the body.
	fields []classField
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

// classField is one instance field initializer of a class.
type classField struct {
	key Atom
	// computed keys are evaluated once when the class is defined, so the atom
	// above is already resolved by the time the constructor runs.
	init *closure
	// value is used for a field with a constant initializer, avoiding a call.
	value    Value
	hasValue bool
	private  bool
}

// fn returns the object's function data, or nil if it is not callable.
func (o *Object) fn() *funcData {
	f, _ := o.data.(*funcData)
	return f
}
