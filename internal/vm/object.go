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

	class Class
	flags objFlags

	// data carries the internal slots of an exotic object: *funcData for a
	// function, *dateData for a Date, and so on. It is nil for an ordinary
	// object.
	data any
}

// newObject returns a bare object with the given prototype and class.
func newObject(proto *Object, class Class) *Object {
	return &Object{proto: proto, class: class, flags: objExtensible}
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
func (o *Object) IsArray() bool { return o.class == ClassArray }

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
	keys := make([]Atom, 0, len(o.elems)+len(o.props))

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
	return v, true
}

// setElem stores a dense element, growing the slice when the write is at or
// just past the end.
//
// A write far past the end would leave a large run of holes, so the object
// falls back to storing the element as an ordinary property instead. The
// threshold bounds the memory a single sparse write can commit.
func (o *Object) setElem(i uint32, v Value) bool {
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
	if o.flags&objHasSparseElements != 0 {
		for i := range o.props {
			p := &o.props[i]
			if p.flags&propDeleted == 0 && p.key.IsIndex() && p.key.Index() >= n {
				n = p.key.Index() + 1
			}
		}
	}
	return n
}

// setArrayLength truncates or extends an array.
func (o *Object) setArrayLength(n uint32) {
	switch {
	case int(n) < len(o.elems):
		// Truncating releases the discarded values for collection.
		clear(o.elems[n:])
		o.elems = o.elems[:n]
	case int(n) > len(o.elems):
		// Extending an array only makes it longer; the new slots are holes.
		const maxHoleRun = 1024
		if int(n)-len(o.elems) <= maxHoleRun {
			for len(o.elems) < int(n) {
				o.elems = append(o.elems, elemHole)
			}
		} else {
			o.flags |= objHasSparseElements
			o.setOwnRaw(atomLength, Uint32(n), propWritable)
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
