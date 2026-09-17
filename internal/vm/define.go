package vm

import "math"

// Defining a property.
//
// Object.defineProperty is where an object's shape is actually decided, and it
// is not a plain assignment: a property that was declared non-configurable
// cannot be redefined, a non-extensible object cannot gain one, and an array's
// length has rules of its own. Those checks are what a script relies on when it
// freezes an object and hands it out, so getting them wrong quietly undoes
// every use of Object.freeze.

// propDesc is a property descriptor as written, with a flag per field saying
// whether it was present at all.
//
// The distinction matters: {writable: false} makes a property non-writable,
// while {} leaves whatever was there alone.
type propDesc struct {
	value  Value
	getter *Object
	setter *Object

	writable     bool
	enumerable   bool
	configurable bool

	hasValue        bool
	hasGet          bool
	hasSet          bool
	hasWritable     bool
	hasEnumerable   bool
	hasConfigurable bool
}

func (d *propDesc) isAccessor() bool { return d.hasGet || d.hasSet }
func (d *propDesc) isData() bool     { return d.hasValue || d.hasWritable }

// generic reports a descriptor that says nothing about the kind of property,
// only about its attributes.
func (d *propDesc) generic() bool { return !d.isAccessor() && !d.isData() }

// toDescriptor reads a descriptor object into its parsed form.
func (r *Runtime) toDescriptor(desc Value) (*propDesc, error) {
	if !desc.IsObject() {
		return nil, r.throwTypeError("a property descriptor must be an object")
	}
	o := desc.Object()
	d := &propDesc{value: Undefined}

	read := func(name Atom) (Value, bool, error) {
		has, err := r.hasPropErr(o, name)
		if err != nil || !has {
			return Undefined, false, err
		}
		v, err := r.getProp(o, name, desc)
		return v, true, err
	}

	v, ok, err := read(atomEnumerable)
	if err != nil {
		return nil, err
	}
	d.enumerable, d.hasEnumerable = v.Truthy(), ok

	v, ok, err = read(atomConfigurable)
	if err != nil {
		return nil, err
	}
	d.configurable, d.hasConfigurable = v.Truthy(), ok

	v, ok, err = read(atomValue)
	if err != nil {
		return nil, err
	}
	d.value, d.hasValue = v, ok

	v, ok, err = read(atomWritable)
	if err != nil {
		return nil, err
	}
	d.writable, d.hasWritable = v.Truthy(), ok

	v, ok, err = read(atomGet)
	if err != nil {
		return nil, err
	}
	if ok {
		if !isCallable(v) && !v.IsUndefined() {
			return nil, r.throwTypeError("a getter must be a function")
		}
		d.hasGet = true
		if v.IsObject() {
			d.getter = v.Object()
		}
	}

	v, ok, err = read(atomSet)
	if err != nil {
		return nil, err
	}
	if ok {
		if !isCallable(v) && !v.IsUndefined() {
			return nil, r.throwTypeError("a setter must be a function")
		}
		d.hasSet = true
		if v.IsObject() {
			d.setter = v.Object()
		}
	}

	if d.isAccessor() && d.isData() {
		return nil, r.throwTypeError(
			"a property descriptor cannot be both an accessor and a data descriptor")
	}
	return d, nil
}

// definePropertyFromDescriptor implements Object.defineProperty.
func (r *Runtime) definePropertyFromDescriptor(target *Object, key Atom, desc Value) error {
	d, err := r.toDescriptor(desc)
	if err != nil {
		return err
	}
	ok, err := r.defineProperty(target, key, d)
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot redefine property %q", r.atoms.name(key))
	}
	return nil
}

// defineProperty applies a descriptor, reporting whether it was allowed.
//
// The caller decides what a refusal means: Object.defineProperty throws,
// Reflect.defineProperty returns false.
func (r *Runtime) defineProperty(o *Object, key Atom, d *propDesc) (bool, error) {
	// A proxy decides for itself what defining a property means.
	if p := proxyOf(o); p != nil {
		return r.proxyDefineProperty(p, key, r.descriptorObject(d))
	}
	if key == atomLength && o.class == ClassArray {
		return r.defineArrayLength(o, d)
	}
	// A numeric key on a typed array names a slot in its buffer, not a
	// property, and the attributes of those are fixed.
	if o.class == ClassTypedArray {
		if ix := r.typedArrayIndex(o, key); ix.numeric {
			return r.typedArrayDefine(o, ix, d)
		}
	}
	// A function's name and length are synthesized, so they have to exist
	// before a redefinition can be checked against them.
	r.materializeFunctionProp(o, key)

	existing := r.currentDescriptor(o, key)
	if existing == nil {
		// A new property needs room for it.
		if !o.IsExtensible() {
			return false, nil
		}
		r.installProperty(o, key, d, nil)
		if o.class == ClassArray && key.IsIndex() {
			r.growArrayLength(o, key.Index())
		}
		return true, nil
	}

	if !r.validateRedefine(existing, d) {
		return false, nil
	}
	r.installProperty(o, key, d, existing)
	return true, nil
}

// validateRedefine reports whether an existing property may be redefined.
//
// A configurable property may become anything. A non-configurable one is
// frozen in almost every respect: the single exception is that a writable data
// property may have its value changed and its writability turned off, which is
// what makes a one-way transition to read-only possible.
func (r *Runtime) validateRedefine(cur *propDesc, d *propDesc) bool {
	if d.generic() && !d.hasEnumerable && !d.hasConfigurable {
		// An empty descriptor asks for nothing and is always allowed.
		return true
	}
	if cur.configurable {
		return true
	}

	if d.hasConfigurable && d.configurable {
		return false
	}
	if d.hasEnumerable && d.enumerable != cur.enumerable {
		return false
	}
	if d.generic() {
		return true
	}
	// The kind of property cannot change.
	if cur.isAccessor() != d.isAccessor() {
		return false
	}
	if cur.isAccessor() {
		if d.hasGet && d.getter != cur.getter {
			return false
		}
		if d.hasSet && d.setter != cur.setter {
			return false
		}
		return true
	}
	if !cur.writable {
		if d.hasWritable && d.writable {
			return false
		}
		if d.hasValue && !d.value.SameValue(cur.value) {
			return false
		}
	}
	return true
}

// currentDescriptor reads an own property into descriptor form, or nil if there
// is none.
func (r *Runtime) currentDescriptor(o *Object, key Atom) *propDesc {
	if o.class == ClassModuleNamespace {
		// An export is stored as an accessor, because it is live, but it is a
		// data property: what a module exports is a value, not a way of
		// computing one.
		return r.namespaceDescriptor(o, key)
	}
	if o.class == ClassTypedArray {
		ix := r.typedArrayIndex(o, key)
		if ix.numeric {
			if !ix.valid {
				return nil
			}
			t := o.data.(*typedArrayData)
			return &propDesc{
				value: t.getElem(ix.i), hasValue: true,
				writable: true, hasWritable: true,
				enumerable: true, hasEnumerable: true,
				configurable: true, hasConfigurable: true,
			}
		}
	}
	if o.class == ClassStringWrapper {
		// A String object's characters are own properties, read-only and not
		// configurable: the object is a box round an immutable string, and its
		// length says the same.
		if s, ok := o.data.(*String); ok {
			if key.IsIndex() && int(key.Index()) < s.Len() {
				i := int(key.Index())
				return &propDesc{
					value: Str(s.Substring(i, i+1)), hasValue: true,
					writable: false, hasWritable: true,
					enumerable: true, hasEnumerable: true,
					configurable: false, hasConfigurable: true,
				}
			}
			if key == atomLength && o.getOwn(atomLength) == nil {
				return &propDesc{
					value: Int(s.Len()), hasValue: true,
					writable: false, hasWritable: true,
					enumerable: false, hasEnumerable: true,
					configurable: false, hasConfigurable: true,
				}
			}
		}
	}
	if o.class == ClassArray && key == atomLength {
		// An array's length is synthesized rather than stored, but it is an own
		// property like any other and has to describe itself as one. It is
		// never enumerable and never configurable; only its writability can
		// change, which freezing and defineProperty do.
		return &propDesc{
			value: Uint32(o.arrayLength()), hasValue: true,
			writable: o.flags&objArrayLengthWritable != 0, hasWritable: true,
			enumerable: false, hasEnumerable: true,
			configurable: false, hasConfigurable: true,
		}
	}
	if key.IsIndex() {
		if v, ok := o.getElem(key.Index()); ok {
			// A dense element is a plain data property with every attribute on.
			return &propDesc{
				value: v, hasValue: true,
				writable: true, hasWritable: true,
				enumerable: true, hasEnumerable: true,
				configurable: true, hasConfigurable: true,
			}
		}
	}
	p := o.getOwnVisible(key)
	if p == nil {
		return nil
	}
	d := &propDesc{
		enumerable: p.flags&propEnumerable != 0, hasEnumerable: true,
		configurable: p.flags&propConfigurable != 0, hasConfigurable: true,
	}
	if p.isAccessor() {
		a := p.getterSetter()
		d.hasGet, d.hasSet = true, true
		if a != nil {
			d.getter, d.setter = a.getter, a.setter
		}
		return d
	}
	d.value, d.hasValue = p.value, true
	if u := mappedArgument(o, key); u != nil {
		d.value = u.get()
	}
	d.writable, d.hasWritable = p.flags&propWritable != 0, true
	return d
}

// installProperty writes the descriptor, filling in from the existing property
// whatever the new one leaves unsaid.
func (r *Runtime) installProperty(o *Object, key Atom, d *propDesc, cur *propDesc) {
	// A descriptor that says nothing about the value defines one holding
	// undefined, so the starting point is undefined rather than the zero Value
	// -- which is the number zero.
	merged := propDesc{value: Undefined}
	if cur != nil {
		merged = *cur
	}
	if d.hasEnumerable {
		merged.enumerable, merged.hasEnumerable = d.enumerable, true
	}
	if d.hasConfigurable {
		merged.configurable, merged.hasConfigurable = d.configurable, true
	}

	switch {
	case d.isAccessor():
		// Changing kind discards whatever the other kind held.
		if cur == nil || !cur.isAccessor() {
			merged.getter, merged.setter = nil, nil
			merged.value, merged.hasValue, merged.hasWritable = Undefined, false, false
			merged.writable = false
		}
		if d.hasGet {
			merged.getter = d.getter
		}
		if d.hasSet {
			merged.setter = d.setter
		}
		merged.hasGet, merged.hasSet = true, true
	case d.isData():
		if cur != nil && cur.isAccessor() {
			merged.hasGet, merged.hasSet = false, false
			merged.getter, merged.setter = nil, nil
			merged.value, merged.writable = Undefined, false
		}
		if d.hasValue {
			merged.value = d.value
		}
		if d.hasWritable {
			merged.writable = d.writable
		}
		merged.hasValue, merged.hasWritable = true, true
	case cur == nil:
		// A generic descriptor on a new property creates a data property
		// holding undefined.
		merged.value, merged.hasValue = Undefined, true
		merged.hasWritable = true
	}

	flags := propFlags(0)
	if merged.enumerable {
		flags |= propEnumerable
	}
	if merged.configurable {
		flags |= propConfigurable
	}

	if merged.hasGet || merged.hasSet {
		// Vacate any dense slot, which cannot hold an accessor.
		if key.IsIndex() && int(key.Index()) < len(o.elems) {
			o.markSparse()
			o.elems[key.Index()] = elemHole
		}
		o.setOwnRaw(key, Value{ref: &accessor{getter: merged.getter, setter: merged.setter}},
			flags|propAccessor)
		return
	}
	if merged.writable {
		flags |= propWritable
	}
	if err := r.defineOwnProp(o, key, merged.value, flags); err != nil {
		// defineOwnProp only fails on paths this function does not take.
		_ = err
	}
}

// setProtoOfChecked assigns a prototype, refusing a cycle.
//
// Without the check, `Object.setPrototypeOf(a, b)` where b already inherits
// from a makes a ring that every property lookup walks forever -- a hang the
// interpreter's interrupt check cannot reach, since the loop is in Go.
func (r *Runtime) setProtoOfChecked(o *Object, proto Value) bool {
	if !o.IsExtensible() {
		return proto.SameValue(protoValue(o))
	}
	if proto.IsObject() {
		for p := proto.Object(); p != nil; p = p.proto {
			if p == o {
				return false
			}
			// A proxy's prototype is whatever its trap says, which may not be
			// walkable here; the cycle it could form is its own problem.
			if proxyOf(p) != nil {
				break
			}
		}
	}
	return setProtoOf(o, proto)
}

// growArrayLength extends an array's length to cover a newly defined index.
func (r *Runtime) growArrayLength(o *Object, i uint32) {
	if int64(i)+1 > int64(o.arrayLength()) {
		o.setArrayLength(i + 1)
	}
}

// defineArrayLength applies a descriptor to an array's length.
//
// Shortening deletes the elements beyond the new length, and stops at the first
// one that refuses to go -- leaving the length just above it, so that the array
// never claims to be shorter than what it still holds.
func (r *Runtime) defineArrayLength(o *Object, d *propDesc) (bool, error) {
	writable := o.flags&objArrayLengthWritable != 0
	if d.hasConfigurable && d.configurable {
		return false, nil
	}
	if d.hasEnumerable && d.enumerable {
		return false, nil
	}
	if d.isAccessor() {
		return false, nil
	}
	if !d.hasValue {
		if d.hasWritable && !d.writable {
			o.flags &^= objArrayLengthWritable
		}
		return true, nil
	}

	n, err := r.toNumber(d.value)
	if err != nil {
		return false, err
	}
	want, err := r.toIndexLength(d.value)
	if err != nil {
		return false, err
	}
	if float64(want) != n || math.IsNaN(n) {
		return false, r.throwRangeError("invalid array length")
	}
	if !writable && uint32(want) != o.arrayLength() {
		return false, nil
	}
	reached := shrinkArray(o, uint32(want))
	o.setArrayLength(reached)
	// The writability change applies even when the shortening stopped short,
	// which is what stops a second attempt from getting any further.
	if d.hasWritable && !d.writable {
		o.flags &^= objArrayLengthWritable
	}
	return reached == uint32(want), nil
}

// shrinkArray deletes an array's elements at or above a new length, highest
// first, and reports the length it actually reached.
//
// It stops at the first element that refuses to go, leaving the length just
// above it: an array never claims to be shorter than what it still holds. Only
// an index redefined as non-configurable can refuse, so the dense elements --
// which are configurable by construction -- always go.
func shrinkArray(o *Object, newLen uint32) uint32 {
	if newLen >= o.arrayLength() {
		return newLen
	}
	stop := newLen
	var keys []Atom
	for i := range o.props {
		p := &o.props[i]
		if p.flags&propDeleted != 0 || !p.key.IsIndex() {
			continue
		}
		if p.key.Index() >= newLen {
			keys = append(keys, p.key)
		}
	}
	sortAtomsByIndex(keys)
	for j := len(keys) - 1; j >= 0; j-- {
		p := o.getOwn(keys[j])
		if p == nil {
			continue
		}
		if p.flags&propConfigurable == 0 {
			stop = keys[j].Index() + 1
			break
		}
		o.deleteOwn(keys[j])
	}
	return stop
}

// toIndexLength converts a value to an array length, rejecting anything that
// is not one.
func (r *Runtime) toIndexLength(v Value) (uint32, error) {
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) || n < 0 || n > 4294967295 || n != math.Trunc(n) {
		return 0, r.throwRangeError("invalid array length")
	}
	return uint32(n), nil
}

// descriptorObject renders a descriptor as the object a trap is handed.
func (r *Runtime) descriptorObject(d *propDesc) Value {
	o := newObject(r.proto.object, ClassObject)
	set := func(name string, v Value) {
		o.setOwnRaw(r.atoms.intern(name), v, propDefault)
	}
	if d.hasValue {
		set("value", d.value)
	}
	if d.hasWritable {
		set("writable", Bool(d.writable))
	}
	if d.hasGet {
		set("get", objOrUndefined(d.getter))
	}
	if d.hasSet {
		set("set", objOrUndefined(d.setter))
	}
	if d.hasEnumerable {
		set("enumerable", Bool(d.enumerable))
	}
	if d.hasConfigurable {
		set("configurable", Bool(d.configurable))
	}
	return Obj(o)
}

func objOrUndefined(o *Object) Value {
	if o == nil {
		return Undefined
	}
	return Obj(o)
}

// setIntegrity implements Object.seal and Object.freeze.
//
// Both go through the object's own machinery rather than its property table: a
// proxy has none, and an object that refuses to make a property permanent has
// to say so rather than be quietly left alone. Sealing makes every property
// non-configurable; freezing also makes the data ones read-only.
func (r *Runtime) setIntegrity(o *Object, freeze bool) error {
	// A function's name and length are synthesized on demand, and about to be
	// constrained, so they have to exist first.
	r.materializeFunctionProp(o, atomLength)
	ok, err := r.preventExtensionsOf(o)
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot make this object non-extensible")
	}
	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return err
	}
	for _, k := range keys {
		d := &propDesc{configurable: false, hasConfigurable: true}
		if freeze {
			cur, err := r.ownPropDesc(o, k)
			if err != nil {
				return err
			}
			if cur == nil {
				continue
			}
			if cur.isData() {
				d.writable, d.hasWritable = false, true
			}
		}
		applied, err := r.defineProperty(o, k, d)
		if err != nil {
			return err
		}
		if !applied {
			return r.throwTypeError("cannot make property %q permanent", r.atoms.name(k))
		}
	}
	return nil
}

// testIntegrity implements Object.isSealed and Object.isFrozen.
func (r *Runtime) testIntegrity(o *Object, frozen bool) (bool, error) {
	ext, err := r.isExtensibleOf(o)
	if err != nil {
		return false, err
	}
	if ext {
		return false, nil
	}
	keys, err := r.ownKeysOf(o, true)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		d, err := r.ownPropDesc(o, k)
		if err != nil {
			return false, err
		}
		if d == nil {
			continue
		}
		if d.configurable {
			return false, nil
		}
		if frozen && d.isData() && d.writable {
			return false, nil
		}
	}
	return true, nil
}

// preventExtensionsOf makes an object non-extensible, asking a proxy's trap
// when the object is one.
func (r *Runtime) preventExtensionsOf(o *Object) (bool, error) {
	if p := proxyOf(o); p != nil {
		return r.proxyPreventExtensions(p)
	}
	// Dense elements cannot express attributes, so an object about to have
	// them constrained moves them into the property table first.
	if len(o.elems) > 0 {
		for i, el := range o.elems {
			if !isHole(el) {
				o.setOwnRaw(internIndex(uint32(i)), el, propDefault)
			}
		}
		o.markSparse()
		o.elems = nil
	}
	o.flags &^= objExtensible
	return true, nil
}

// createDataProperty defines a property the caller promised to create, so a
// define the object refuses is an error rather than something to ignore.
func (r *Runtime) createDataProperty(o *Object, key Atom, val Value, flags propFlags) error {
	ok, err := r.defineProperty(o, key, &propDesc{
		value: val, hasValue: true,
		writable: flags&propWritable != 0, hasWritable: true,
		enumerable: flags&propEnumerable != 0, hasEnumerable: true,
		configurable: flags&propConfigurable != 0, hasConfigurable: true,
	})
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot define property %q", r.atoms.name(key))
	}
	return nil
}

// defineHalfAccessorChecked is defineHalfAccessor through the checked define,
// so that a key the object will not part with is reported.
func (r *Runtime) defineHalfAccessorChecked(o *Object, key Atom, fn *Object, getter bool,
	flags propFlags) error {
	d := &propDesc{
		enumerable: flags&propEnumerable != 0, hasEnumerable: true,
		configurable: flags&propConfigurable != 0, hasConfigurable: true,
	}
	if getter {
		d.getter, d.hasGet = fn, true
	} else {
		d.setter, d.hasSet = fn, true
	}
	ok, err := r.defineProperty(o, key, d)
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot define property %q", r.atoms.name(key))
	}
	return nil
}
