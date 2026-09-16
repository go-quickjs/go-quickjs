package vm

import (
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

func nan() float64         { return math.NaN() }
func inf(sign int) float64 { return math.Inf(sign) }

// ---------------------------------------------------------------------------
// Object
// ---------------------------------------------------------------------------

func (r *Runtime) initObjectBuiltins() {
	p := r.proto.object

	r.defMethod(p, "hasOwnProperty", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		key, err := rt.toPropertyKey(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		return Bool(rt.hasOwnProp(o, key)), nil
	})

	r.defMethod(p, "isPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() || !this.IsObject() {
			return False, nil
		}
		target := this.Object()
		for o := v.Object().proto; o != nil; o = o.proto {
			if o == target {
				return True, nil
			}
		}
		return False, nil
	})

	r.defMethod(p, "propertyIsEnumerable", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		key, err := rt.toPropertyKey(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		if key.IsIndex() {
			if _, ok := o.getElem(key.Index()); ok {
				return True, nil
			}
		}
		if prop := o.getOwnVisible(key); prop != nil {
			return Bool(prop.flags&propEnumerable != 0), nil
		}
		return False, nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		switch {
		case this.IsUndefined():
			return Str(NewString("[object Undefined]")), nil
		case this.IsNull():
			return Str(NewString("[object Null]")), nil
		}
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString("[object " + rt.classTag(o) + "]")), nil
	})

	r.defMethod(p, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		fn, err := rt.getValueProp(this, atomToString)
		if err != nil {
			return Undefined, err
		}
		return rt.call(fn, this, nil)
	})

	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})

	ctor := r.newCtor("Object", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if v.IsNullish() {
			return Obj(newObject(rt.proto.object, ClassObject)), nil
		}
		o, err := rt.toObject(v)
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})

	r.defMethod(ctor, "keys", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.objectKeysLike(arg(args, 0), keysOnly)
	})
	r.defMethod(ctor, "values", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.objectKeysLike(arg(args, 0), valuesOnly)
	})
	r.defMethod(ctor, "entries", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.objectKeysLike(arg(args, 0), keysAndValues)
	})

	r.defMethod(ctor, "assign", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		for _, src := range args[min(1, len(args)):] {
			if src.IsNullish() {
				continue
			}
			so, err := rt.toObject(src)
			if err != nil {
				return Undefined, err
			}
			for _, k := range so.ownKeys(true, rt.atoms) {
				if !rt.isEnumerable(so, k) {
					continue
				}
				v, err := rt.getProp(so, k, src)
				if err != nil {
					return Undefined, err
				}
				if err := rt.setProp(target, k, v, Obj(target), true); err != nil {
					return Undefined, err
				}
			}
		}
		return Obj(target), nil
	})

	r.defMethod(ctor, "create", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		protoArg := arg(args, 0)
		var proto *Object
		switch {
		case protoArg.IsObject():
			proto = protoArg.Object()
		case protoArg.IsNull():
			proto = nil
		default:
			return Undefined, rt.throwTypeError("Object.create requires an object or null")
		}
		o := newObject(proto, ClassObject)
		if props := arg(args, 1); !props.IsUndefined() {
			if err := rt.defineProperties(o, props); err != nil {
				return Undefined, err
			}
		}
		return Obj(o), nil
	})

	r.defMethod(ctor, "getPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if o.proto == nil {
			return Null, nil
		}
		return Obj(o.proto), nil
	})

	r.defMethod(ctor, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return target, nil
		}
		switch pv := arg(args, 1); {
		case pv.IsObject():
			target.Object().proto = pv.Object()
		case pv.IsNull():
			target.Object().proto = nil
		default:
			return Undefined, rt.throwTypeError("the prototype must be an object or null")
		}
		return target, nil
	})

	r.defMethod(ctor, "defineProperty", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Object.defineProperty requires an object")
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if err := rt.definePropertyFromDescriptor(target.Object(), key, arg(args, 2)); err != nil {
			return Undefined, err
		}
		return target, nil
	})

	r.defMethod(ctor, "defineProperties", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		if !target.IsObject() {
			return Undefined, rt.throwTypeError("Object.defineProperties requires an object")
		}
		if err := rt.defineProperties(target.Object(), arg(args, 1)); err != nil {
			return Undefined, err
		}
		return target, nil
	})

	r.defMethod(ctor, "getOwnPropertyNames", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		keys := o.ownKeys(false, rt.atoms)
		out := make([]Value, len(keys))
		for i, k := range keys {
			out[i] = Str(NewString(rt.atoms.name(k)))
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(ctor, "getOwnPropertyDescriptor", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return rt.describeProperty(o, key), nil
	})

	r.defMethod(ctor, "freeze", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return v, nil
		}
		o := v.Object()
		o.flags &^= objExtensible
		for i := range o.props {
			o.props[i].flags &^= propWritable | propConfigurable
		}
		// Dense elements cannot express attributes, so freezing moves them into
		// the property table where they can be marked read-only.
		for i, el := range o.elems {
			if !isHole(el) {
				o.setOwnRaw(internIndex(uint32(i)), el, propEnumerable)
			}
		}
		if len(o.elems) > 0 {
			o.elems = nil
			o.flags |= objHasSparseElements
		}
		return v, nil
	})

	r.defMethod(ctor, "isFrozen", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return True, nil
		}
		o := v.Object()
		if o.IsExtensible() {
			return False, nil
		}
		for i := range o.props {
			if o.props[i].flags&propDeleted != 0 {
				continue
			}
			if o.props[i].flags&(propWritable|propConfigurable) != 0 {
				return False, nil
			}
		}
		for _, el := range o.elems {
			if !isHole(el) {
				return False, nil
			}
		}
		return True, nil
	})

	r.defMethod(ctor, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if v := arg(args, 0); v.IsObject() {
			v.Object().flags &^= objExtensible
		}
		return arg(args, 0), nil
	})

	r.defMethod(ctor, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsObject() && v.Object().IsExtensible()), nil
	})

	r.defMethod(ctor, "is", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Bool(arg(args, 0).SameValue(arg(args, 1))), nil
	})

	r.defMethod(ctor, "fromEntries", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(rt.proto.object, ClassObject)
		err := rt.iterate(arg(args, 0), func(entry Value) error {
			k, err := rt.getIndexed(entry, Int(0))
			if err != nil {
				return err
			}
			v, err := rt.getIndexed(entry, Int(1))
			if err != nil {
				return err
			}
			key, err := rt.toPropertyKey(k)
			if err != nil {
				return err
			}
			return rt.defineOwnProp(o, key, v, propDefault)
		})
		if err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})
}

// classTag returns the tag Object.prototype.toString reports for an object.
func (r *Runtime) classTag(o *Object) string {
	// A Symbol.toStringTag property overrides the built-in tag.
	if v, err := r.getProp(o, r.atoms.internSymbol(r.wellKnown.toStringTag), Obj(o)); err == nil {
		if v.IsString() {
			return v.String().Go()
		}
	}
	switch o.class {
	case ClassArray:
		return "Array"
	case ClassFunction:
		return "Function"
	case ClassError:
		return "Error"
	case ClassBooleanWrapper:
		return "Boolean"
	case ClassNumberWrapper:
		return "Number"
	case ClassStringWrapper:
		return "String"
	case ClassDate:
		return "Date"
	case ClassRegExp:
		return "RegExp"
	case ClassArguments:
		return "Arguments"
	}
	return "Object"
}

// isEnumerable reports whether a key is an enumerable own property.
func (r *Runtime) isEnumerable(o *Object, key Atom) bool {
	if key.IsIndex() {
		if _, ok := o.getElem(key.Index()); ok {
			return true
		}
	}
	if p := o.getOwnVisible(key); p != nil {
		return p.flags&propEnumerable != 0
	}
	return false
}

// keysMode selects what Object.keys, values and entries produce.
type keysMode uint8

const (
	keysOnly keysMode = iota
	valuesOnly
	keysAndValues
)

func (r *Runtime) objectKeysLike(v Value, mode keysMode) (Value, error) {
	o, err := r.toObject(v)
	if err != nil {
		return Undefined, err
	}
	keys := o.ownKeys(false, r.atoms)
	if p := proxyOf(o); p != nil {
		var err error
		if keys, err = r.proxyOwnKeys(p); err != nil {
			return Undefined, err
		}
	}
	var out []Value
	for _, k := range keys {
		if p := proxyOf(o); p == nil && !r.isEnumerable(o, k) {
			continue
		}
		switch mode {
		case keysOnly:
			out = append(out, Str(NewString(r.atoms.name(k))))
		case valuesOnly:
			val, err := r.getProp(o, k, v)
			if err != nil {
				return Undefined, err
			}
			out = append(out, val)
		default:
			val, err := r.getProp(o, k, v)
			if err != nil {
				return Undefined, err
			}
			pair := r.newArrayFrom([]Value{Str(NewString(r.atoms.name(k))), val})
			out = append(out, Obj(pair))
		}
	}
	return Obj(r.newArrayFrom(out)), nil
}

// describeProperty builds a property descriptor object.
func (r *Runtime) describeProperty(o *Object, key Atom) Value {
	if key.IsIndex() {
		if v, ok := o.getElem(key.Index()); ok {
			d := newObject(r.proto.object, ClassObject)
			r.setDescField(d, "value", v)
			r.setDescField(d, "writable", True)
			r.setDescField(d, "enumerable", True)
			r.setDescField(d, "configurable", True)
			return Obj(d)
		}
	}
	p := o.getOwnVisible(key)
	if p == nil {
		return Undefined
	}
	d := newObject(r.proto.object, ClassObject)
	if p.isAccessor() {
		a := p.getterSetter()
		get, set := Undefined, Undefined
		if a != nil && a.getter != nil {
			get = Obj(a.getter)
		}
		if a != nil && a.setter != nil {
			set = Obj(a.setter)
		}
		r.setDescField(d, "get", get)
		r.setDescField(d, "set", set)
	} else {
		r.setDescField(d, "value", p.value)
		r.setDescField(d, "writable", Bool(p.flags&propWritable != 0))
	}
	r.setDescField(d, "enumerable", Bool(p.flags&propEnumerable != 0))
	r.setDescField(d, "configurable", Bool(p.flags&propConfigurable != 0))
	return Obj(d)
}

func (r *Runtime) setDescField(d *Object, name string, v Value) {
	d.setOwnRaw(r.atoms.intern(name), v, propDefault)
}

// defineProperties applies a map of descriptors.
func (r *Runtime) defineProperties(target *Object, props Value) error {
	src, err := r.toObject(props)
	if err != nil {
		return err
	}
	for _, k := range src.ownKeys(true, r.atoms) {
		if !r.isEnumerable(src, k) {
			continue
		}
		desc, err := r.getProp(src, k, props)
		if err != nil {
			return err
		}
		if err := r.definePropertyFromDescriptor(target, k, desc); err != nil {
			return err
		}
	}
	return nil
}

// definePropertyFromDescriptor applies one descriptor object.
func (r *Runtime) definePropertyFromDescriptor(target *Object, key Atom, desc Value) error {
	if !desc.IsObject() {
		return r.throwTypeError("a property descriptor must be an object")
	}
	d := desc.Object()

	read := func(name Atom) (Value, bool, error) {
		if !r.hasProp(d, name) {
			return Undefined, false, nil
		}
		v, err := r.getProp(d, name, desc)
		return v, true, err
	}

	getter, hasGet, err := read(atomGet)
	if err != nil {
		return err
	}
	setter, hasSet, err := read(atomSet)
	if err != nil {
		return err
	}
	value, hasValue, err := read(atomValue)
	if err != nil {
		return err
	}
	writable, hasWritable, err := read(atomWritable)
	if err != nil {
		return err
	}
	enumerable, hasEnumerable, err := read(atomEnumerable)
	if err != nil {
		return err
	}
	configurable, hasConfigurable, err := read(atomConfigurable)
	if err != nil {
		return err
	}

	if (hasGet || hasSet) && (hasValue || hasWritable) {
		return r.throwTypeError("a property descriptor cannot be both an accessor and a data descriptor")
	}

	var flags propFlags
	if hasEnumerable && enumerable.Truthy() {
		flags |= propEnumerable
	}
	if hasConfigurable && configurable.Truthy() {
		flags |= propConfigurable
	}

	if hasGet || hasSet {
		var g, s *Object
		if hasGet && getter.IsObject() {
			g = getter.Object()
		}
		if hasSet && setter.IsObject() {
			s = setter.Object()
		}
		r.defineAccessor(target, key, g, s, flags)
		return nil
	}
	if hasWritable && writable.Truthy() {
		flags |= propWritable
	}
	return r.defineOwnProp(target, key, value, flags)
}

// ---------------------------------------------------------------------------
// Function
// ---------------------------------------------------------------------------

func (r *Runtime) initFunctionBuiltins() {
	p := r.proto.function

	r.defMethod(p, "call", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		rest := args
		if len(rest) > 0 {
			rest = rest[1:]
		}
		return rt.call(this, arg(args, 0), rest)
	})

	r.defMethod(p, "apply", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		list := arg(args, 1)
		var callArgs []Value
		if !list.IsNullish() {
			var err error
			callArgs, err = rt.arrayToSlice(list)
			if err != nil {
				return Undefined, err
			}
		}
		return rt.call(this, arg(args, 0), callArgs)
	})

	r.defMethod(p, "bind", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !isCallable(this) {
			return Undefined, rt.throwTypeError("Function.prototype.bind requires a function")
		}
		target := this.Object()
		var bound []Value
		if len(args) > 1 {
			bound = append(bound, args[1:]...)
		}
		o := newObject(rt.proto.function, ClassFunction)
		o.data = &funcData{
			boundTarget: target,
			boundThis:   arg(args, 0),
			boundArgs:   bound,
			name:        "bound " + target.fn().nameOr(""),
			ctorKind:    target.fn().ctorKind,
		}
		return Obj(o), nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !isCallable(this) {
			return Undefined, rt.throwTypeError("Function.prototype.toString requires a function")
		}
		fd := this.Object().fn()
		if fd.closure != nil && fd.closure.fn.Text != "" {
			return Str(NewString(fd.closure.fn.Text)), nil
		}
		return Str(NewString("function " + fd.nameOr("") + "() { [native code] }")), nil
	})

	r.defSymbolMethod(p, r.wellKnown.hasInstance, "[Symbol.hasInstance]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			// The default implementation is the ordinary prototype-chain walk,
			// expressed without recursing back through instanceOf.
			v := arg(args, 0)
			if !isCallable(this) || !v.IsObject() {
				return False, nil
			}
			protoVal, err := rt.getProp(this.Object(), atomPrototype, this)
			if err != nil {
				return Undefined, err
			}
			if !protoVal.IsObject() {
				return Undefined, rt.throwTypeError("the prototype is not an object")
			}
			target := protoVal.Object()
			for o := v.Object().proto; o != nil; o = o.proto {
				if o == target {
					return True, nil
				}
			}
			return False, nil
		})

	r.newCtor("Function", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Compiling a function from a string needs the parser, which would make
		// this package depend on it. The host can supply the capability instead.
		return Undefined, rt.throwTypeError("the Function constructor is disabled")
	})
}

// arrayToSlice reads an array-like into a Go slice.
func (r *Runtime) arrayToSlice(v Value) ([]Value, error) {
	o, err := r.toObject(v)
	if err != nil {
		return nil, err
	}
	lenVal, err := r.getProp(o, atomLength, v)
	if err != nil {
		return nil, err
	}
	n, err := r.toLength(lenVal)
	if err != nil {
		return nil, err
	}
	out := make([]Value, 0, min(int(n), 1024))
	for i := int64(0); i < n; i++ {
		el, err := r.getProp(o, internIndex(uint32(i)), v)
		if err != nil {
			return nil, err
		}
		out = append(out, el)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Array
// ---------------------------------------------------------------------------

func (r *Runtime) initArrayBuiltins() {
	p := r.proto.array

	ctor := r.newCtor("Array", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// A single numeric argument gives the length rather than one element.
		if len(args) == 1 && args[0].IsNumber() {
			n, err := rt.toArrayLength(args[0])
			if err != nil {
				return Undefined, err
			}
			a := rt.newArrayFrom(nil)
			a.setArrayLength(n)
			return Obj(a), nil
		}
		return Obj(rt.newArrayFrom(args)), nil
	})

	r.defMethod(ctor, "isArray", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsObject() && v.Object().IsArray()), nil
	})

	r.defMethod(ctor, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Obj(rt.newArrayFrom(args)), nil
	})

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		src := arg(args, 0)
		mapFn := arg(args, 1)
		var out []Value
		add := func(v Value) error {
			if isCallable(mapFn) {
				mapped, err := rt.call(mapFn, Undefined, []Value{v, Int(len(out))})
				if err != nil {
					return err
				}
				v = mapped
			}
			out = append(out, v)
			return nil
		}
		// An iterable is consumed through its iterator; anything else is
		// treated as array-like.
		if ok, err := rt.isIterable(src); err != nil {
			return Undefined, err
		} else if ok {
			if err := rt.iterate(src, add); err != nil {
				return Undefined, err
			}
			return Obj(rt.newArrayFrom(out)), nil
		}
		items, err := rt.arrayToSlice(src)
		if err != nil {
			return Undefined, err
		}
		for _, it := range items {
			if err := add(it); err != nil {
				return Undefined, err
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(p, "push", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		o.elems = append(o.elems, args...)
		return Int(len(o.elems)), nil
	})

	r.defMethod(p, "pop", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		if len(o.elems) == 0 {
			return Undefined, nil
		}
		v := o.elems[len(o.elems)-1]
		o.elems = o.elems[:len(o.elems)-1]
		if isHole(v) {
			return Undefined, nil
		}
		return v, nil
	})

	r.defMethod(p, "shift", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		if len(o.elems) == 0 {
			return Undefined, nil
		}
		v := o.elems[0]
		o.elems = append(o.elems[:0], o.elems[1:]...)
		if isHole(v) {
			return Undefined, nil
		}
		return v, nil
	})

	r.defMethod(p, "unshift", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		o.elems = append(append(make([]Value, 0, len(o.elems)+len(args)), args...), o.elems...)
		return Int(len(o.elems)), nil
	})

	r.defMethod(p, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		n := len(o.elems)
		start, err := rt.relativeIndex(arg(args, 0), n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), n, n)
		if err != nil {
			return Undefined, err
		}
		if start >= end {
			return Obj(rt.newArrayFrom(nil)), nil
		}
		return Obj(rt.newArrayFrom(o.elems[start:end])), nil
	})

	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		for i, el := range o.elems {
			if !isHole(el) && el.StrictEquals(target) {
				return Int(i), nil
			}
		}
		return Int(-1), nil
	})

	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		for _, el := range o.elems {
			// includes uses SameValueZero, so NaN is found.
			if el.SameValueZero(target) {
				return True, nil
			}
		}
		return False, nil
	})

	r.defMethod(p, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		sep := ","
		if s := arg(args, 0); !s.IsUndefined() {
			ss, err := rt.toString(s)
			if err != nil {
				return Undefined, err
			}
			sep = ss.Go()
		}
		var sb strings.Builder
		for i, el := range o.elems {
			if i > 0 {
				sb.WriteString(sep)
			}
			// null and undefined contribute nothing, unlike String(el).
			if el.IsNullish() || isHole(el) {
				continue
			}
			s, err := rt.toString(el)
			if err != nil {
				return Undefined, err
			}
			sb.WriteString(s.Go())
		}
		return Str(NewString(sb.String())), nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		fn, err := rt.getValueProp(this, rt.atoms.intern("join"))
		if err != nil {
			return Undefined, err
		}
		if isCallable(fn) {
			return rt.call(fn, this, nil)
		}
		return Str(NewString("[object Array]")), nil
	})

	r.defMethod(p, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		out := append([]Value(nil), o.elems...)
		for _, a := range args {
			// An array argument is spread; anything else is appended whole.
			if a.IsObject() && a.Object().IsArray() {
				out = append(out, a.Object().elems...)
				continue
			}
			out = append(out, a)
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(p, "reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		for i, j := 0, len(o.elems)-1; i < j; i, j = i+1, j-1 {
			o.elems[i], o.elems[j] = o.elems[j], o.elems[i]
		}
		return this, nil
	})

	// The iteration methods share a shape, so they are defined from a table.
	// Every one of these captures the length once before iterating, as the
	// specification requires: an element the callback appends is not visited,
	// and one it removes is skipped. Re-reading the length each step would
	// also let a callback that pushes loop forever.
	r.defIterationMethod(p, "forEach", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		for i := 0; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				continue
			}
			if _, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)}); err != nil {
				return Undefined, err
			}
		}
		return Undefined, nil
	})

	r.defIterationMethod(p, "map", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		out := make([]Value, n)
		for i := 0; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				// A hole in the source stays a hole in the result.
				out[i] = elemHole
				continue
			}
			v, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			out[i] = v
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defIterationMethod(p, "filter", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		var out []Value
		for i := 0; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				continue
			}
			keep, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			if keep.Truthy() {
				out = append(out, el)
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defIterationMethod(p, "find", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		for i := 0; i < n; i++ {
			// find visits holes, unlike filter and forEach, reporting them as
			// undefined.
			el, _ := elemAt(o, i)
			ok, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			if ok.Truthy() {
				return el, nil
			}
		}
		return Undefined, nil
	})

	r.defIterationMethod(p, "findIndex", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		for i := 0; i < n; i++ {
			el, _ := elemAt(o, i)
			ok, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			if ok.Truthy() {
				return Int(i), nil
			}
		}
		return Int(-1), nil
	})

	r.defIterationMethod(p, "some", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		for i := 0; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				continue
			}
			res, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			if res.Truthy() {
				return True, nil
			}
		}
		return False, nil
	})

	r.defIterationMethod(p, "every", func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error) {
		n := len(o.elems)
		for i := 0; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				continue
			}
			res, err := rt.call(cb, thisArg, []Value{el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
			if !res.Truthy() {
				return False, nil
			}
		}
		return True, nil
	})

	r.defMethod(p, "reduce", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("reduce requires a function")
		}
		n := len(o.elems)
		i := 0
		var acc Value
		if len(args) > 1 {
			acc = args[1]
		} else {
			// Without an initial value the first present element seeds the
			// accumulator, and an array with none is an error.
			for i < n {
				if el, ok := elemAt(o, i); ok {
					acc = el
					i++
					break
				}
				i++
			}
			if i > n || acc.IsUndefined() && i == 0 {
				return Undefined, rt.throwTypeError("reduce of an empty array with no initial value")
			}
			if i == 0 {
				return Undefined, rt.throwTypeError("reduce of an empty array with no initial value")
			}
		}
		for ; i < n; i++ {
			el, ok := elemAt(o, i)
			if !ok {
				continue
			}
			acc, err = rt.call(cb, Undefined, []Value{acc, el, Int(i), Obj(o)})
			if err != nil {
				return Undefined, err
			}
		}
		return acc, nil
	})

	r.defMethod(p, "sort", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		cmp := arg(args, 0)
		var sortErr error
		// The default comparison is by the string form, which is why
		// [10, 9].sort() gives [10, 9].
		sort.SliceStable(o.elems, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			a, b := o.elems[i], o.elems[j]
			switch {
			case isHole(a) || a.IsUndefined():
				return false
			case isHole(b) || b.IsUndefined():
				return true
			}
			if isCallable(cmp) {
				res, err := rt.call(cmp, Undefined, []Value{a, b})
				if err != nil {
					sortErr = err
					return false
				}
				n, err := rt.toNumber(res)
				if err != nil {
					sortErr = err
					return false
				}
				return n < 0
			}
			sa, err := rt.toString(a)
			if err != nil {
				sortErr = err
				return false
			}
			sb, err := rt.toString(b)
			if err != nil {
				sortErr = err
				return false
			}
			return sa.Compare(sb) < 0
		})
		if sortErr != nil {
			return Undefined, sortErr
		}
		return this, nil
	})

	// Arrays are iterable.
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return rt.newArrayIterator(this)
		})
}

// iterationFn is the body of an array method that takes a callback.
type iterationFn func(rt *Runtime, o *Object, cb Value, thisArg Value) (Value, error)

// defIterationMethod defines an array method that takes a callback and an
// optional `this` argument, which is the shape most of them share.
func (r *Runtime) defIterationMethod(p *Object, name string, body iterationFn) {
	r.defMethod(p, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("%s requires a function", name)
		}
		return body(rt, o, cb, arg(args, 1))
	})
}

// relativeIndex resolves an index argument that may be negative, as slice and
// its relatives accept.
func (r *Runtime) relativeIndex(v Value, length, def int) (int, error) {
	if v.IsUndefined() {
		return def, nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		n += float64(length)
	}
	switch {
	case n < 0:
		return 0, nil
	case n > float64(length):
		return length, nil
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Iteration protocol
// ---------------------------------------------------------------------------

// isIterable reports whether a value has a Symbol.iterator method.
func (r *Runtime) isIterable(v Value) (bool, error) {
	if v.IsNullish() {
		return false, nil
	}
	m, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return false, err
	}
	return isCallable(m), nil
}

// iterate drives the iteration protocol, calling fn for each value.
func (r *Runtime) iterate(v Value, fn func(Value) error) error {
	method, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.iterator))
	if err != nil {
		return err
	}
	if !isCallable(method) {
		return r.throwTypeError("%s is not iterable", r.describe(v))
	}
	iter, err := r.call(method, v, nil)
	if err != nil {
		return err
	}
	next, err := r.getValueProp(iter, atomNext)
	if err != nil {
		return err
	}
	for {
		res, err := r.call(next, iter, nil)
		if err != nil {
			return err
		}
		if !res.IsObject() {
			return r.throwTypeError("an iterator result must be an object")
		}
		done, err := r.getValueProp(res, atomDone)
		if err != nil {
			return err
		}
		if done.Truthy() {
			return nil
		}
		val, err := r.getValueProp(res, atomValue)
		if err != nil {
			return err
		}
		if err := fn(val); err != nil {
			// An error from the body closes the iterator, giving it a chance
			// to release resources.
			r.closeIterator(iter)
			return err
		}
	}
}

// closeIterator calls an iterator's return method, ignoring any error since the
// original failure is what matters.
func (r *Runtime) closeIterator(iter Value) {
	ret, err := r.getValueProp(iter, atomReturn)
	if err != nil || !isCallable(ret) {
		return
	}
	_, _ = r.call(ret, iter, nil)
}

// newArrayIterator builds an iterator over an array's elements.
func (r *Runtime) newArrayIterator(target Value) (Value, error) {
	o, err := r.toObject(target)
	if err != nil {
		return Undefined, err
	}
	i := 0
	iter := newObject(r.proto.arrayIter, ClassIterator)
	r.defMethod(iter, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		res := newObject(rt.proto.object, ClassObject)
		if i >= len(o.elems) {
			res.setOwnRaw(atomValue, Undefined, propDefault)
			res.setOwnRaw(atomDone, True, propDefault)
			return Obj(res), nil
		}
		v := o.elems[i]
		if isHole(v) {
			v = Undefined
		}
		i++
		res.setOwnRaw(atomValue, v, propDefault)
		res.setOwnRaw(atomDone, False, propDefault)
		return Obj(res), nil
	})
	// An iterator is itself iterable, which is what makes `for (x of iter)`
	// work on one directly.
	r.defSymbolMethod(iter, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return this, nil
		})
	return Obj(iter), nil
}

// ---------------------------------------------------------------------------
// Number, Boolean, Symbol
// ---------------------------------------------------------------------------

func (r *Runtime) initNumberBuiltins() {
	p := r.proto.number

	ctor := r.newCtor("Number", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n := float64(0)
		if len(args) > 0 {
			v, err := rt.toNumber(args[0])
			if err != nil {
				return Undefined, err
			}
			n = v
		}
		if !rt.Constructing() {
			return Float(n), nil
		}
		o := newObject(rt.proto.number, ClassNumberWrapper)
		o.data = n
		return Obj(o), nil
	})

	r.defConst(ctor, "MAX_SAFE_INTEGER", Float(maxSafeInteger))
	r.defConst(ctor, "MIN_SAFE_INTEGER", Float(-maxSafeInteger))
	r.defConst(ctor, "MAX_VALUE", Float(math.MaxFloat64))
	r.defConst(ctor, "MIN_VALUE", Float(5e-324))
	r.defConst(ctor, "EPSILON", Float(2.220446049250313e-16))
	r.defConst(ctor, "POSITIVE_INFINITY", Float(inf(1)))
	r.defConst(ctor, "NEGATIVE_INFINITY", Float(inf(-1)))
	r.defConst(ctor, "NaN", Float(nan()))

	r.defMethod(ctor, "isInteger", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsNumber() && isFiniteInteger(v.Number())), nil
	})
	r.defMethod(ctor, "isSafeInteger", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsNumber() && isFiniteInteger(v.Number()) &&
			math.Abs(v.Number()) <= maxSafeInteger), nil
	})
	r.defMethod(ctor, "isFinite", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Unlike the global isFinite, this does not coerce its argument.
		v := arg(args, 0)
		return Bool(v.IsNumber() && !math.IsNaN(v.Number()) && !math.IsInf(v.Number(), 0)), nil
	})
	r.defMethod(ctor, "isNaN", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsNumber() && math.IsNaN(v.Number())), nil
	})
	r.defMethod(ctor, "parseFloat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.globalParseFloat(args)
	})
	r.defMethod(ctor, "parseInt", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.globalParseInt(args)
	})

	r.defMethod(p, "toString", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		radix := 10
		if rv := arg(args, 0); !rv.IsUndefined() {
			ri, err := rt.toInteger(rv)
			if err != nil {
				return Undefined, err
			}
			if ri < 2 || ri > 36 {
				return Undefined, rt.throwRangeError("the radix must be between 2 and 36")
			}
			radix = int(ri)
		}
		return Str(NewString(jsnum.FormatRadix(n, radix))), nil
	})

	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		return Float(n), nil
	})

	r.defMethod(p, "toFixed", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		digits, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if digits < 0 || digits > 100 {
			return Undefined, rt.throwRangeError("toFixed() digits must be between 0 and 100")
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Str(NewString(jsnum.FormatFloat(n))), nil
		}
		return Str(NewString(formatFixed(n, int(digits)))), nil
	})
}

// thisNumber unwraps the receiver of a Number method.
func (r *Runtime) thisNumber(this Value) (float64, error) {
	if this.IsNumber() {
		return this.Number(), nil
	}
	if this.IsObject() && this.Object().class == ClassNumberWrapper {
		if n, ok := this.Object().data.(float64); ok {
			return n, nil
		}
	}
	return 0, r.throwTypeError("this is not a number")
}

func isFiniteInteger(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f)
}

func (r *Runtime) initBooleanBuiltins() {
	p := r.proto.boolean
	r.newCtor("Boolean", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b := arg(args, 0).Truthy()
		if !rt.Constructing() {
			return Bool(b), nil
		}
		// Called with new, the result is a wrapper object whose valueOf gives
		// the primitive back.
		o := newObject(rt.proto.boolean, ClassBooleanWrapper)
		o.data = b
		return Obj(o), nil
	})
	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.thisBool(this)
		if err != nil {
			return Undefined, err
		}
		if b {
			return Str(NewString("true")), nil
		}
		return Str(NewString("false")), nil
	})
	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.thisBool(this)
		if err != nil {
			return Undefined, err
		}
		return Bool(b), nil
	})
}

func (r *Runtime) thisBool(this Value) (bool, error) {
	if this.IsBool() {
		return this.BoolValue(), nil
	}
	if this.IsObject() && this.Object().class == ClassBooleanWrapper {
		if b, ok := this.Object().data.(bool); ok {
			return b, nil
		}
	}
	return false, r.throwTypeError("this is not a boolean")
}

func (r *Runtime) initSymbolBuiltins() {
	p := r.proto.symbol

	ctor := r.newCtor("Symbol", 0, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if rt.Constructing() {
			// Symbol is deliberately not constructible, so that every symbol
			// is a primitive.
			return Undefined, rt.throwTypeError("Symbol is not a constructor")
		}
		d := arg(args, 0)
		if d.IsUndefined() {
			return Sym(NewSymbol("", false)), nil
		}
		s, err := rt.toString(d)
		if err != nil {
			return Undefined, err
		}
		return Sym(NewSymbol(s.Go(), true)), nil
	})
	// Symbol is not a constructor: `new Symbol()` is a TypeError.
	ctor.fn().ctorKind = ctorNone

	wk := map[string]*Symbol{
		"iterator": r.wellKnown.iterator, "asyncIterator": r.wellKnown.asyncIterator,
		"hasInstance": r.wellKnown.hasInstance, "toPrimitive": r.wellKnown.toPrimitive,
		"toStringTag": r.wellKnown.toStringTag, "species": r.wellKnown.species,
		"isConcatSpreadable": r.wellKnown.isConcatSpreadable,
		"unscopables":        r.wellKnown.unscopables, "match": r.wellKnown.match,
		"matchAll": r.wellKnown.matchAll, "replace": r.wellKnown.replace,
		"search": r.wellKnown.search, "split": r.wellKnown.split,
	}
	for name, sym := range wk {
		r.defConst(ctor, name, Sym(sym))
	}

	r.defMethod(ctor, "for", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		key := s.Go()
		if sym, ok := rt.symbolRegistry[key]; ok {
			return Sym(sym), nil
		}
		sym := &Symbol{Description: key, HasDescription: true, Registered: true}
		rt.symbolRegistry[key] = sym
		return Sym(sym), nil
	})

	r.defMethod(ctor, "keyFor", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsSymbol() {
			return Undefined, rt.throwTypeError("Symbol.keyFor requires a symbol")
		}
		if s := v.Symbol(); s.Registered {
			return Str(NewString(s.Description)), nil
		}
		return Undefined, nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.thisSymbol(this)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(s.String())), nil
	})
	r.defGetter(p, "description", func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.thisSymbol(this)
		if err != nil {
			return Undefined, err
		}
		if !s.HasDescription {
			return Undefined, nil
		}
		return Str(NewString(s.Description)), nil
	})
}

func (r *Runtime) thisSymbol(this Value) (*Symbol, error) {
	if this.IsSymbol() {
		return this.Symbol(), nil
	}
	if this.IsObject() && this.Object().class == ClassSymbolWrapper {
		if s, ok := this.Object().data.(*Symbol); ok {
			return s, nil
		}
	}
	return nil, r.throwTypeError("this is not a symbol")
}

// ---------------------------------------------------------------------------
// Error
// ---------------------------------------------------------------------------

func (r *Runtime) initErrorBuiltins() {
	for k := errorKind(0); k < errorKindCount; k++ {
		kind := k
		proto := r.proto.nativeErrors[k]
		name := errorKindNames[k]

		proto.setOwnRaw(atomName, Str(NewString(name)), propWritable|propConfigurable)
		proto.setOwnRaw(atomMessage, Str(emptyString), propWritable|propConfigurable)

		ctor := r.newCtor(name, 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
			o := newObject(rt.proto.nativeErrors[kind], ClassError)
			msg := ""
			if m := arg(args, 0); !m.IsUndefined() {
				s, err := rt.toString(m)
				if err != nil {
					return Undefined, err
				}
				msg = s.Go()
				o.setOwnRaw(atomMessage, Str(s), propWritable|propConfigurable)
			}
			// The cause option, when present, is attached as an own property.
			if opts := arg(args, 1); opts.IsObject() {
				causeKey := rt.atoms.intern("cause")
				if rt.hasProp(opts.Object(), causeKey) {
					cause, err := rt.getProp(opts.Object(), causeKey, opts)
					if err != nil {
						return Undefined, err
					}
					o.setOwnRaw(causeKey, cause, propWritable|propConfigurable)
				}
			}
			o.setOwnRaw(atomStack, Str(NewString(rt.formatStack(msg, kind))),
				propWritable|propConfigurable)
			return Obj(o), nil
		})
		r.proto.errorCtors[k] = ctor

		// The native error constructors inherit from Error itself.
		if k != errError {
			ctor.proto = r.proto.errorCtors[errError]
		}
	}

	r.defMethod(r.proto.err, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !this.IsObject() {
			return Undefined, rt.throwTypeError("Error.prototype.toString requires an object")
		}
		o := this.Object()
		nameVal, err := rt.getProp(o, atomName, this)
		if err != nil {
			return Undefined, err
		}
		msgVal, err := rt.getProp(o, atomMessage, this)
		if err != nil {
			return Undefined, err
		}
		name := "Error"
		if !nameVal.IsUndefined() {
			s, err := rt.toString(nameVal)
			if err != nil {
				return Undefined, err
			}
			name = s.Go()
		}
		msg := ""
		if !msgVal.IsUndefined() {
			s, err := rt.toString(msgVal)
			if err != nil {
				return Undefined, err
			}
			msg = s.Go()
		}
		switch {
		case msg == "":
			return Str(NewString(name)), nil
		case name == "":
			return Str(NewString(msg)), nil
		}
		return Str(NewString(name + ": " + msg)), nil
	})
}

// ---------------------------------------------------------------------------
// Math
// ---------------------------------------------------------------------------

func (r *Runtime) initMathBuiltins() {
	m := newObject(r.proto.object, ClassMathObject)
	r.defValue(r.global, "Math", Obj(m))

	r.defConst(m, "PI", Float(math.Pi))
	r.defConst(m, "E", Float(math.E))
	r.defConst(m, "LN2", Float(math.Ln2))
	r.defConst(m, "LN10", Float(math.Log(10)))
	r.defConst(m, "LOG2E", Float(1/math.Ln2))
	r.defConst(m, "LOG10E", Float(1/math.Log(10)))
	r.defConst(m, "SQRT2", Float(math.Sqrt2))
	r.defConst(m, "SQRT1_2", Float(math.Sqrt(0.5)))

	// The single-argument functions differ only in which Go function they call.
	unary := map[string]func(float64) float64{
		"abs": math.Abs, "floor": math.Floor, "ceil": math.Ceil,
		"sqrt": math.Sqrt, "cbrt": math.Cbrt, "sin": math.Sin, "cos": math.Cos,
		"tan": math.Tan, "asin": math.Asin, "acos": math.Acos, "atan": math.Atan,
		"sinh": math.Sinh, "cosh": math.Cosh, "tanh": math.Tanh,
		"asinh": math.Asinh, "acosh": math.Acosh, "atanh": math.Atanh,
		"log": math.Log, "log2": math.Log2, "log10": math.Log10,
		"log1p": math.Log1p, "exp": math.Exp, "expm1": math.Expm1,
		"trunc": math.Trunc,
		// JavaScript rounds half away from zero for positive values but half
		// up overall, which is neither math.Round nor math.Floor.
		"round":  jsRound,
		"sign":   jsSign,
		"fround": func(f float64) float64 { return float64(float32(f)) },
	}
	for name, fn := range unary {
		f := fn
		r.defMethod(m, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			n, err := rt.toNumber(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			return Float(f(n)), nil
		})
	}

	r.defMethod(m, "pow", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toNumber(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Float(jsPow(a, b)), nil
	})

	r.defMethod(m, "atan2", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toNumber(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Float(math.Atan2(a, b)), nil
	})

	r.defMethod(m, "max", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.mathExtremum(args, true)
	})
	r.defMethod(m, "min", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.mathExtremum(args, false)
	})

	r.defMethod(m, "hypot", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		sum := 0.0
		for _, a := range args {
			n, err := rt.toNumber(a)
			if err != nil {
				return Undefined, err
			}
			sum += n * n
		}
		return Float(math.Sqrt(sum)), nil
	})

	r.defMethod(m, "random", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Float(rt.random()), nil
	})
}

// mathExtremum implements Math.max and Math.min, which return NaN if any
// argument is NaN and treat -0 as less than +0.
func (r *Runtime) mathExtremum(args []Value, wantMax bool) (Value, error) {
	best := inf(-1)
	if !wantMax {
		best = inf(1)
	}
	for _, a := range args {
		n, err := r.toNumber(a)
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(n) {
			return Float(nan()), nil
		}
		switch {
		case wantMax && (n > best || (n == 0 && best == 0 && !math.Signbit(n))):
			best = n
		case !wantMax && (n < best || (n == 0 && best == 0 && math.Signbit(n))):
			best = n
		}
	}
	return Float(best), nil
}

// jsRound rounds half toward positive infinity, which differs from math.Round
// for negative halves: Math.round(-0.5) is -0, not -1.
func jsRound(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	return math.Floor(f + 0.5)
}

func jsSign(f float64) float64 {
	switch {
	case math.IsNaN(f):
		return f
	case f > 0:
		return 1
	case f < 0:
		return -1
	}
	// Preserves -0 and +0.
	return f
}

// ---------------------------------------------------------------------------
// Global functions
// ---------------------------------------------------------------------------

func (r *Runtime) initGlobalFunctions() {
	r.defMethod(r.global, "parseInt", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.globalParseInt(args)
	})
	r.defMethod(r.global, "parseFloat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.globalParseFloat(args)
	})
	r.defMethod(r.global, "isNaN", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(math.IsNaN(n)), nil
	})
	r.defMethod(r.global, "isFinite", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(!math.IsNaN(n) && !math.IsInf(n, 0)), nil
	})
}

func (r *Runtime) globalParseInt(args []Value) (Value, error) {
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	radix := 0
	if rv := arg(args, 1); !rv.IsUndefined() {
		n, err := r.toInt32(rv)
		if err != nil {
			return Undefined, err
		}
		radix = int(n)
	}
	return Float(jsnum.ParseIntPrefix(s.Go(), radix)), nil
}

func (r *Runtime) globalParseFloat(args []Value) (Value, error) {
	s, err := r.toString(arg(args, 0))
	if err != nil {
		return Undefined, err
	}
	return Float(jsnum.ParseFloatPrefix(s.Go())), nil
}

// wtf8 is used by the string built-ins.
var _ = wtf8.Count

// random returns a pseudo-random number in [0, 1).
//
// The generator is per-runtime and seeded from the process-wide source, so that
// two runtimes in the same process do not produce identical sequences while
// each stays deterministic with respect to its own calls.
func (r *Runtime) random() float64 {
	if r.rng == nil {
		r.rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return r.rng.Float64()
}

// initArrayExtras defines the array methods that take a relative index or are
// otherwise recent additions.
func (r *Runtime) initArrayExtras() {
	p := r.proto.array

	r.defMethod(p, "at", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// A negative index counts back from the end.
		if i < 0 {
			i += float64(len(o.elems))
		}
		if i < 0 || i >= float64(len(o.elems)) {
			return Undefined, nil
		}
		v := o.elems[int(i)]
		if isHole(v) {
			return Undefined, nil
		}
		return v, nil
	})

	r.defMethod(p, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		for i := len(o.elems) - 1; i >= 0; i-- {
			if !isHole(o.elems[i]) && o.elems[i].StrictEquals(target) {
				return Int(i), nil
			}
		}
		return Int(-1), nil
	})

	r.defMethod(p, "fill", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		start, err := rt.relativeIndex(arg(args, 1), len(o.elems), 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 2), len(o.elems), len(o.elems))
		if err != nil {
			return Undefined, err
		}
		for i := start; i < end; i++ {
			o.elems[i] = v
		}
		return this, nil
	})

	r.defMethod(p, "flat", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		depth := 1.0
		if d := arg(args, 0); !d.IsUndefined() {
			depth, err = rt.toInteger(d)
			if err != nil {
				return Undefined, err
			}
		}
		out := rt.flatten(o.elems, int(depth))
		return Obj(rt.newArrayFrom(out)), nil
	})
}

// flatten appends the elements of nested arrays up to the given depth.
func (r *Runtime) flatten(elems []Value, depth int) []Value {
	var out []Value
	for _, el := range elems {
		if isHole(el) {
			continue
		}
		if depth > 0 && el.IsObject() && el.Object().IsArray() {
			out = append(out, r.flatten(el.Object().elems, depth-1)...)
			continue
		}
		out = append(out, el)
	}
	return out
}

// elemAt reads a dense element defensively, reporting false for an index that
// is out of range or holds a hole.
//
// A callback may shrink the array while an iteration method is running, so an
// index that was valid when the length was captured may not be by the time it
// is reached.
func elemAt(o *Object, i int) (Value, bool) {
	if i < 0 || i >= len(o.elems) {
		return Undefined, false
	}
	v := o.elems[i]
	if isHole(v) {
		return Undefined, false
	}
	return v, true
}
