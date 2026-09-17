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
		has, err := rt.hasOwnPropOf(o, key)
		if err != nil {
			return Undefined, err
		}
		return Bool(has), nil
	})

	r.defMethod(p, "isPrototypeOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Anything but an object has no prototype chain to be on, and that is
		// answered before the receiver is coerced -- which is where a nullish
		// one is reported.
		v := arg(args, 0)
		if !v.IsObject() {
			return False, nil
		}
		target, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		// The walk asks each object for its prototype rather than reading the
		// field, so a proxy in the chain runs its trap.
		for o := v.Object(); ; {
			next, err := rt.protoOf(o)
			if err != nil {
				return Undefined, err
			}
			if !next.IsObject() {
				return False, nil
			}
			o = next.Object()
			if o == target {
				return True, nil
			}
		}
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
		// A proxy answers for itself, through the descriptor its trap reports.
		if pp := proxyOf(o); pp != nil {
			desc, err := rt.proxyGetOwnPropertyDescriptor(pp, key)
			if err != nil || !desc.IsObject() {
				return False, err
			}
			v, err := rt.getValueProp(desc, atomEnumerable)
			return Bool(v.Truthy()), err
		}
		yes, err := rt.isEnumerable(o, key)
		return Bool(yes), err
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
		tag, err := rt.classTag(o)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString("[object " + tag + "]")), nil
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
			keys, err := rt.ownKeysOf(so, true)
			if err != nil {
				return Undefined, err
			}
			for _, k := range keys {
				enumerable, err := rt.isEnumerable(so, k)
				if err != nil {
					return Undefined, err
				}
				if !enumerable {
					continue
				}
				v, err := rt.getProp(so, k, src)
				if err != nil {
					return Undefined, err
				}
				if _, err := rt.setProp(target, k, v, Obj(target), true); err != nil {
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
		if p := proxyOf(o); p != nil {
			return rt.proxyGetPrototypeOf(p)
		}
		return protoValue(o), nil
	})

	r.defMethod(ctor, "setPrototypeOf", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		target := arg(args, 0)
		proto := arg(args, 1)
		// The target has to be something a prototype could be set on, which
		// null and undefined are not -- and that is checked before the
		// prototype, so the more obvious mistake is the one reported.
		if target.IsNullish() {
			return Undefined, rt.throwTypeError(
				"Object.setPrototypeOf called on %s", target.Kind())
		}
		if !proto.IsObject() && !proto.IsNull() {
			return Undefined, rt.throwTypeError("the prototype must be an object or null")
		}
		if !target.IsObject() {
			return target, nil
		}
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxySetPrototypeOf(p, proto)
			if err != nil {
				return Undefined, err
			}
			if !ok {
				return Undefined, rt.throwTypeError("cannot set the prototype of this object")
			}
			return target, nil
		}
		if !rt.setProtoOfChecked(target.Object(), proto) {
			return Undefined, rt.throwTypeError("cannot set the prototype of this object")
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
		if p := proxyOf(target.Object()); p != nil {
			ok, err := rt.proxyDefineProperty(p, key, arg(args, 2))
			if err != nil {
				return Undefined, err
			}
			if !ok {
				return Undefined, rt.throwTypeError(
					"the proxy \"defineProperty\" trap returned false for %q",
					rt.atoms.name(key))
			}
			return target, nil
		}
		// A function's name and length are synthesized on demand, so they have
		// to exist before a redefinition can be checked against them.
		rt.materializeFunctionProp(target.Object(), key)
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
		keys, err := rt.ownKeysOf(o, false)
		if err != nil {
			return Undefined, err
		}
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
		if p := proxyOf(o); p != nil {
			return rt.proxyGetOwnPropertyDescriptor(p, key)
		}
		return rt.describeProperty(o, key)
	})

	r.defMethod(ctor, "freeze", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return v, nil
		}
		return v, rt.setIntegrity(v.Object(), true)
	})

	r.defMethod(ctor, "isFrozen", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return True, nil
		}
		ok, err := rt.testIntegrity(v.Object(), true)
		return Bool(ok), err
	})

	r.defMethod(ctor, "preventExtensions", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return v, nil
		}
		if p := proxyOf(v.Object()); p != nil {
			ok, err := rt.proxyPreventExtensions(p)
			if err != nil {
				return Undefined, err
			}
			if !ok {
				return Undefined, rt.throwTypeError("cannot prevent extensions on this object")
			}
			return v, nil
		}
		v.Object().flags &^= objExtensible
		return v, nil
	})

	r.defMethod(ctor, "isExtensible", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return False, nil
		}
		if p := proxyOf(v.Object()); p != nil {
			ok, err := rt.proxyIsExtensible(p)
			return Bool(ok), err
		}
		return Bool(v.Object().IsExtensible()), nil
	})

	r.defMethod(ctor, "is", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Bool(arg(args, 0).SameValue(arg(args, 1))), nil
	})

	r.defMethod(ctor, "fromEntries", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o := newObject(rt.proto.object, ClassObject)
		err := rt.iterate(arg(args, 0), func(entry Value) error {
			// An entry is a pair, and only an object can be one. A string has
			// a [0] and a [1] and would otherwise pass for one silently.
			if !entry.IsObject() {
				return rt.throwTypeError("an entry must be an object, not %s", entry.Kind())
			}
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
//
// It can fail twice over: deciding whether the object is an array asks a proxy
// for its target, which a revoked one no longer has, and Symbol.toStringTag may
// be a getter that throws.
func (r *Runtime) classTag(o *Object) (string, error) {
	builtin, err := r.builtinTag(o)
	if err != nil {
		return "", err
	}
	// A Symbol.toStringTag property overrides the built-in tag, but only when
	// it is a string: anything else is ignored rather than coerced.
	v, err := r.getProp(o, r.atoms.internSymbol(r.wellKnown.toStringTag), Obj(o))
	if err != nil {
		return "", err
	}
	if v.IsString() {
		return v.String().Go(), nil
	}
	return builtin, nil
}

// builtinTag is the tag an object has before Symbol.toStringTag is consulted.
func (r *Runtime) builtinTag(o *Object) (string, error) {
	// A proxy answers for its target: whether something is an array or is
	// callable is a question about behaviour, and a proxy of an array behaves
	// like one.
	isArr, err := r.isArray(Obj(o))
	if err != nil {
		return "", err
	}
	if isArr {
		return "Array", nil
	}
	if o.IsCallable() {
		return "Function", nil
	}
	switch o.class {
	case ClassError:
		return "Error", nil
	case ClassBooleanWrapper:
		return "Boolean", nil
	case ClassNumberWrapper:
		return "Number", nil
	case ClassStringWrapper:
		return "String", nil
	case ClassDate:
		return "Date", nil
	case ClassRegExp:
		return "RegExp", nil
	case ClassArguments:
		return "Arguments", nil
	}
	return "Object", nil
}

// isEnumerable reports whether a key is an enumerable own property.
//
// It can fail, because an object may compute the answer: a proxy runs a trap,
// and a module namespace reads the binding behind the export -- which is a
// ReferenceError while that binding is in its dead zone.
func (r *Runtime) isEnumerable(o *Object, key Atom) (bool, error) {
	// A proxy answers through its getOwnPropertyDescriptor trap; reading the
	// property table would see the proxy object itself, which has none.
	if p := proxyOf(o); p != nil {
		desc, err := r.proxyGetOwnPropertyDescriptor(p, key)
		if err != nil || !desc.IsObject() {
			return false, err
		}
		v, err := r.getValueProp(desc, atomEnumerable)
		return v.Truthy(), err
	}
	if o.class == ClassModuleNamespace {
		d, err := r.namespaceDescriptor(o, key)
		if err != nil || d != nil {
			return d != nil && d.enumerable, err
		}
	}
	if key.IsIndex() {
		if _, ok := o.getElem(key.Index()); ok {
			return true, nil
		}
		// A typed array's elements live in a buffer rather than in the
		// property table, but they are enumerable own properties. So are a
		// String object's characters.
		if o.class == ClassTypedArray {
			if t, ok := o.data.(*typedArrayData); ok && int(key.Index()) < t.count() {
				return true, nil
			}
		}
		if o.class == ClassStringWrapper {
			if str, ok := o.data.(*String); ok && int(key.Index()) < str.Len() {
				return true, nil
			}
		}
	}
	if p := o.getOwnVisible(key); p != nil {
		return p.flags&propEnumerable != 0, nil
	}
	return false, nil
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
	keys, err := r.ownKeysOf(o, false)
	if err != nil {
		return Undefined, err
	}
	proxied := proxyOf(o) != nil
	var out []Value
	for _, k := range keys {
		if proxied {
			// A proxy says whether a key is enumerable through the descriptor
			// its trap reports, not through a property table it does not have.
			desc, err := r.ownDescriptorOf(o, k)
			if err != nil {
				return Undefined, err
			}
			if !desc.IsObject() {
				continue
			}
			e, err := r.getValueProp(desc, atomEnumerable)
			if err != nil {
				return Undefined, err
			}
			if !e.Truthy() {
				continue
			}
		} else {
			enumerable, err := r.isEnumerable(o, k)
			if err != nil {
				return Undefined, err
			}
			if !enumerable {
				continue
			}
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
func (r *Runtime) describeProperty(o *Object, key Atom) (Value, error) {
	r.materializeFunctionProp(o, key)
	pd, err := r.currentDescriptor(o, key)
	if err != nil || pd == nil {
		return Undefined, err
	}
	d := newObject(r.proto.object, ClassObject)
	if pd.isAccessor() {
		get, set := Undefined, Undefined
		if pd.getter != nil {
			get = Obj(pd.getter)
		}
		if pd.setter != nil {
			set = Obj(pd.setter)
		}
		r.setDescField(d, "get", get)
		r.setDescField(d, "set", set)
	} else {
		r.setDescField(d, "value", pd.value)
		r.setDescField(d, "writable", Bool(pd.writable))
	}
	r.setDescField(d, "enumerable", Bool(pd.enumerable))
	r.setDescField(d, "configurable", Bool(pd.configurable))
	return Obj(d), nil
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
	keys, err := r.ownKeysOf(src, true)
	if err != nil {
		return err
	}
	for _, k := range keys {
		enumerable, err := r.isEnumerable(src, k)
		if err != nil {
			return err
		}
		if !enumerable {
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

// ---------------------------------------------------------------------------
// Function
// ---------------------------------------------------------------------------

func (r *Runtime) initFunctionBuiltins() {
	p := r.proto.function

	// The two properties that once let a function walk the call stack. They
	// are still there, and reading or writing either throws: the same function
	// an unmapped arguments object's callee uses, which is what makes them
	// indistinguishable from one another.
	r.defineAccessor(p, r.atoms.intern("caller"),
		r.throwTypeErrorFn, r.throwTypeErrorFn, propConfigurable)
	r.defineAccessor(p, atomArguments,
		r.throwTypeErrorFn, r.throwTypeErrorFn, propConfigurable)

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
			callArgs, err = rt.argumentList(list)
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
		// The bound function's length is what remains of the target's after the
		// arguments already supplied, which is what makes it still describe how
		// many the caller has left to give. Only an own length counts, and only
		// a number: anything else leaves it at zero.
		length := Int(0)
		if rt.hasOwnProp(target, atomLength) {
			targetLen, err := rt.getProp(target, atomLength, this)
			if err != nil {
				return Undefined, err
			}
			if targetLen.IsNumber() {
				n := targetLen.Number()
				switch {
				case math.IsInf(n, 1):
					length = Float(n)
				case math.IsInf(n, -1) || math.IsNaN(n):
				default:
					if left := math.Trunc(n) - float64(len(bound)); left > 0 {
						length = Float(left)
					}
				}
			}
		}
		name := ""
		if n, err := rt.getProp(target, atomName, this); err != nil {
			return Undefined, err
		} else if n.IsString() {
			name = n.String().Go()
		}

		o := newObject(rt.proto.function, ClassFunction)
		o.data = &funcData{
			boundTarget: target,
			boundThis:   arg(args, 0),
			boundArgs:   bound,
			name:        "bound " + name,
			ctorKind:    target.fn().ctorKind,
			// Both are settled here rather than synthesized on demand, because
			// a length of infinity is not something the synthesized form can
			// hold.
			propsMaterialized: true,
		}
		o.setOwnRaw(atomLength, length, propConfigurable)
		o.setOwnRaw(atomName, Str(NewString("bound "+name)), propConfigurable)
		return Obj(o), nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !isCallable(this) {
			return Undefined, rt.throwTypeError("Function.prototype.toString requires a function")
		}
		fd := this.Object().fn()
		// A proxy is callable when its target is, but carries no function data
		// of its own and no source text either. The native form is the answer
		// the specification requires for anything without source.
		if fd == nil {
			return Str(NewString("function () { [native code] }")), nil
		}
		if fd.closure != nil && fd.closure.fn.Text != "" {
			return Str(NewString(fd.closure.fn.Text)), nil
		}
		return Str(NewString("function " + fd.nameOr("") + "() { [native code] }")), nil
	})

	// The method is neither writable nor configurable, which is what lets a
	// script rely on `instanceof` meaning what it says.
	defer func() {
		if pd := p.getOwn(r.atoms.internSymbol(r.wellKnown.hasInstance)); pd != nil {
			pd.flags &^= propWritable | propConfigurable
		}
	}()
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
			// The chain is walked by asking each object for its prototype, so
			// a proxy in it runs its trap rather than being read around.
			for o := v.Object(); ; {
				next, err := rt.protoOf(o)
				if err != nil {
					return Undefined, err
				}
				if !next.IsObject() {
					return False, nil
				}
				o = next.Object()
				if o == target {
					return True, nil
				}
			}
		})

	r.newCtor("Function", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// Compiling a function from a string needs the parser, which would make
		// this package depend on it. The host can supply the capability instead.
		return Undefined, rt.throwTypeError("the Function constructor is disabled")
	})
}

// arrayToSlice reads an array-like into a Go slice.
// argumentList reads an array-like as a list of arguments, which requires it to
// be an object: apply and its relatives refuse a primitive rather than treating
// it as an empty list or reading indices off a wrapper.
func (r *Runtime) argumentList(v Value) ([]Value, error) {
	if !v.IsObject() {
		return nil, r.throwTypeError("an argument list must be an object, not %s", v.Kind())
	}
	return r.arrayToSlice(v)
}

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
		// A length is whatever the object says it is, and may be 2**53-1 with
		// nothing behind it, so the walk has to stay interruptible.
		if err := r.tick(); err != nil {
			return nil, err
		}
		el, err := r.getProp(o, r.atoms.indexAtom(uint32(i)), v)
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

	r.proto.arrayCtor = ctor

	r.defMethod(ctor, "isArray", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		yes, err := rt.isArray(arg(args, 0))
		return Bool(yes), err
	})

	r.defMethod(ctor, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n := int64(len(args))
		// Called on a constructor -- which it is on Array itself, and may be on
		// a subclass -- the result is what that constructor makes.
		var o *Object
		if isConstructor(this) {
			v, err := rt.construct(this, []Value{Float(float64(n))})
			if err != nil {
				return Undefined, err
			}
			if !v.IsObject() {
				return Undefined, rt.throwTypeError("the constructor did not return an object")
			}
			o = v.Object()
		} else {
			o = rt.newArrayOfLength(n)
		}
		for i, v := range args {
			if err := rt.createIndexed(o, int64(i), v); err != nil {
				return Undefined, err
			}
		}
		a := arrayLike{o: o}
		if err := a.setLength(rt, n); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})

	r.initArrayFromAsync(ctor)
	r.defSpecies(ctor)

	r.defMethod(ctor, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		src, mapFn, thisArg := arg(args, 0), arg(args, 1), arg(args, 2)
		mapping := false
		if !mapFn.IsUndefined() {
			if !isCallable(mapFn) {
				return Undefined, rt.throwTypeError("the map function is not callable")
			}
			mapping = true
		}
		// Called on a constructor -- which it is, on Array itself, and may be
		// on a subclass -- the result is what that constructor makes.
		build := func(n int64) (*Object, error) {
			if isConstructor(this) {
				var ctorArgs []Value
				if n >= 0 {
					ctorArgs = []Value{Float(float64(n))}
				}
				v, err := rt.construct(this, ctorArgs)
				if err != nil {
					return nil, err
				}
				if !v.IsObject() {
					return nil, rt.throwTypeError("the constructor did not return an object")
				}
				return v.Object(), nil
			}
			if n < 0 {
				n = 0
			}
			return rt.newArrayOfLength(n), nil
		}

		iterable, err := rt.isIterable(src)
		if err != nil {
			return Undefined, err
		}
		if iterable {
			o, err := build(-1)
			if err != nil {
				return Undefined, err
			}
			a := arrayLike{o: o}
			k := int64(0)
			err = rt.iterate(src, func(v Value) error {
				if mapping {
					mapped, err := rt.call(mapFn, thisArg, []Value{v, Float(float64(k))})
					if err != nil {
						return err
					}
					v = mapped
				}
				if err := rt.createIndexed(o, k, v); err != nil {
					return err
				}
				k++
				return nil
			})
			if err != nil {
				return Undefined, err
			}
			if err := a.setLength(rt, k); err != nil {
				return Undefined, err
			}
			return Obj(o), nil
		}

		from, err := rt.viewArrayLike(src)
		if err != nil {
			return Undefined, err
		}
		o, err := build(from.n)
		if err != nil {
			return Undefined, err
		}
		a := arrayLike{o: o}
		for k := int64(0); k < from.n; k++ {
			v, err := from.get(rt, k)
			if err != nil {
				return Undefined, err
			}
			if mapping {
				mapped, err := rt.call(mapFn, thisArg, []Value{v, Float(float64(k))})
				if err != nil {
					return Undefined, err
				}
				v = mapped
			}
			if err := rt.createIndexed(o, k, v); err != nil {
				return Undefined, err
			}
		}
		if err := a.setLength(rt, from.n); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	})

	// push, pop, shift and unshift work through the view rather than the dense
	// elements, so that they apply to an array-like, honour a frozen array's
	// refusal to be written, and see a length that is a number rather than a
	// count of what is present.
	r.defMethod(p, "push", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n+int64(len(args)) > maxArrayLength {
			return Undefined, rt.throwTypeError("the array would be too long")
		}
		for i, v := range args {
			if err := a.set(rt, a.n+int64(i), v); err != nil {
				return Undefined, err
			}
		}
		n := a.n + int64(len(args))
		if err := a.setLength(rt, n); err != nil {
			return Undefined, err
		}
		return Float(float64(n)), nil
	})

	r.defMethod(p, "pop", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n == 0 {
			// The length is still assigned, which is observable when it was
			// not already zero.
			return Undefined, a.setLength(rt, 0)
		}
		v, err := a.get(rt, a.n-1)
		if err != nil {
			return Undefined, err
		}
		if err := a.remove(rt, a.n-1); err != nil {
			return Undefined, err
		}
		if err := a.setLength(rt, a.n-1); err != nil {
			return Undefined, err
		}
		return v, nil
	})

	r.defMethod(p, "shift", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n == 0 {
			return Undefined, a.setLength(rt, 0)
		}
		first, err := a.get(rt, 0)
		if err != nil {
			return Undefined, err
		}
		for i := int64(1); i < a.n; i++ {
			v, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if err := a.put(rt, i-1, v, present); err != nil {
				return Undefined, err
			}
		}
		if err := a.remove(rt, a.n-1); err != nil {
			return Undefined, err
		}
		if err := a.setLength(rt, a.n-1); err != nil {
			return Undefined, err
		}
		return first, nil
	})

	r.defMethod(p, "unshift", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		add := int64(len(args))
		if a.n+add > maxArrayLength {
			return Undefined, rt.throwTypeError("the array would be too long")
		}
		// Backwards, so that an element is never overwritten before it is read.
		for i := a.n - 1; i >= 0; i-- {
			v, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if err := a.put(rt, i+add, v, present); err != nil {
				return Undefined, err
			}
		}
		for i, v := range args {
			if err := a.set(rt, int64(i), v); err != nil {
				return Undefined, err
			}
		}
		n := a.n + add
		if err := a.setLength(rt, n); err != nil {
			return Undefined, err
		}
		return Float(float64(n)), nil
	})

	r.defMethod(p, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex64(arg(args, 0), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex64(arg(args, 1), a.n, a.n)
		if err != nil {
			return Undefined, err
		}
		count := end - start
		if count < 0 {
			count = 0
		}
		out, err := rt.arraySpeciesCreate(this, count)
		if err != nil {
			return Undefined, err
		}
		for i := start; i < end; i++ {
			v, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				// A hole in the source stays a hole in the result.
				if err := out.pushHole(rt); err != nil {
					return Undefined, err
				}
				continue
			}
			if err := out.push(rt, v); err != nil {
				return Undefined, err
			}
		}
		if err := out.setLength(rt, count); err != nil {
			return Undefined, err
		}
		return out.value(), nil
	})

	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n == 0 {
			// An empty search converts nothing: the starting point cannot
			// matter, so its valueOf is never called.
			return Float(-1), nil
		}
		target := arg(args, 0)
		from, err := rt.relativeIndex64(arg(args, 1), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		for i := from; i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			// indexOf skips holes and compares with ===, so NaN is never found.
			if present && el.StrictEquals(target) {
				return Float(float64(i)), nil
			}
		}
		return Int(-1), nil
	})

	r.defMethod(p, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n == 0 {
			// An empty array answers before the second argument is converted,
			// which a valueOf on it can tell.
			return Int(-1), nil
		}
		target := arg(args, 0)
		from := a.n - 1
		if len(args) > 1 {
			n, err := rt.toInteger(args[1])
			if err != nil {
				return Undefined, err
			}
			if n < 0 {
				n += float64(a.n)
				if n < 0 {
					return Int(-1), nil
				}
			}
			// Anything at or beyond the end means the whole array, which is
			// what the starting point already is.
			if n < float64(from) {
				from = int64(n)
			}
		}
		for i := from; i >= 0; i-- {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if present && el.StrictEquals(target) {
				return Float(float64(i)), nil
			}
		}
		return Int(-1), nil
	})

	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		if a.n == 0 {
			// Nothing to search, and nothing to convert either: the starting
			// point is left alone, so its valueOf is never called.
			return False, nil
		}
		target := arg(args, 0)
		from, err := rt.relativeIndex64(arg(args, 1), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		for i := from; i < a.n; i++ {
			// includes compares with SameValueZero, so NaN is found, and it
			// reads holes as undefined rather than skipping them.
			el, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			if el.SameValueZero(target) {
				return True, nil
			}
		}
		return False, nil
	})

	r.defMethod(p, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
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
		for i := int64(0); i < a.n; i++ {
			if i > 0 {
				sb.WriteString(sep)
			}
			el, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			// null and undefined contribute nothing, unlike String(el).
			if el.IsNullish() {
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
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		fn, err := rt.getProp(o, rt.atoms.intern("join"), this)
		if err != nil {
			return Undefined, err
		}
		if isCallable(fn) {
			return rt.call(fn, this, nil)
		}
		// Whatever this is has no join to call, so it falls back to the
		// ordinary object description -- the real one, tag and all, not a
		// fixed string: the receiver need not be an array.
		tag, err := rt.classTag(o)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString("[object " + tag + "]")), nil
	})

	r.defMethod(p, "concat", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		out, err := rt.arraySpeciesCreate(Obj(a.o), 0)
		if err != nil {
			return Undefined, err
		}
		// The receiver counts as the first argument: it is spread when it is an
		// array and appended whole otherwise, just like the rest.
		items := append([]Value{Obj(a.o)}, args...)
		for _, item := range items {
			spread, err := rt.isConcatSpreadable(item)
			if err != nil {
				return Undefined, err
			}
			if !spread {
				if err := out.push(rt, item); err != nil {
					return Undefined, err
				}
				continue
			}
			src, err := rt.viewArrayLike(item)
			if err != nil {
				return Undefined, err
			}
			if out.n+src.n > maxArrayLength {
				return Undefined, rt.throwTypeError("the result would be too long")
			}
			for i := int64(0); i < src.n; i++ {
				v, present, err := src.at(rt, i)
				if err != nil {
					return Undefined, err
				}
				if !present {
					if err := out.pushHole(rt); err != nil {
						return Undefined, err
					}
					continue
				}
				if err := out.push(rt, v); err != nil {
					return Undefined, err
				}
			}
		}
		n := out.n
		if err := out.setLength(rt, n); err != nil {
			return Undefined, err
		}
		return out.value(), nil
	})

	r.defMethod(p, "reverse", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		for lo, hi := int64(0), a.n-1; lo < hi; lo, hi = lo+1, hi-1 {
			loVal, loHas, err := a.at(rt, lo)
			if err != nil {
				return Undefined, err
			}
			hiVal, hiHas, err := a.at(rt, hi)
			if err != nil {
				return Undefined, err
			}
			// A hole swapped in has to be deleted rather than written, or it
			// would become an element holding undefined.
			if err := a.put(rt, lo, hiVal, hiHas); err != nil {
				return Undefined, err
			}
			if err := a.put(rt, hi, loVal, loHas); err != nil {
				return Undefined, err
			}
		}
		// The coerced receiver is what comes back, so reversing a primitive
		// hands back the wrapper it was reversed through.
		return Obj(a.o), nil
	})

	// The iteration methods share a shape, so they are defined from a table.
	// Every one of these captures the length once before iterating, as the
	// specification requires: an element the callback appends is not visited,
	// and one it removes is skipped. Re-reading the length each step would
	// also let a callback that pushes loop forever.
	r.defIterationMethod(p, "forEach", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				continue
			}
			if _, err := rt.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)}); err != nil {
				return Undefined, err
			}
		}
		return Undefined, nil
	})

	r.defIterationMethod(p, "map", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		out, err := rt.arraySpeciesCreate(Obj(a.o), a.n)
		if err != nil {
			return Undefined, err
		}
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				// A hole in the source stays a hole in the result.
				if err := out.pushHole(rt); err != nil {
					return Undefined, err
				}
				continue
			}
			v, err := rt.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)})
			if err != nil {
				return Undefined, err
			}
			if err := out.push(rt, v); err != nil {
				return Undefined, err
			}
		}
		return out.value(), nil
	})

	r.defIterationMethod(p, "filter", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		out, err := rt.arraySpeciesCreate(Obj(a.o), 0)
		if err != nil {
			return Undefined, err
		}
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				continue
			}
			keep, err := rt.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)})
			if err != nil {
				return Undefined, err
			}
			if keep.Truthy() {
				if err := out.push(rt, el); err != nil {
					return Undefined, err
				}
			}
		}
		return out.value(), nil
	})

	r.defIterationMethod(p, "find", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		v, _, err := rt.findIn(a, cb, thisArg, false)
		return v, err
	})

	r.defIterationMethod(p, "findIndex", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		_, i, err := rt.findIn(a, cb, thisArg, false)
		return Float(float64(i)), err
	})

	r.defIterationMethod(p, "findLast", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		v, _, err := rt.findIn(a, cb, thisArg, true)
		return v, err
	})

	r.defIterationMethod(p, "findLastIndex", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		_, i, err := rt.findIn(a, cb, thisArg, true)
		return Float(float64(i)), err
	})

	r.defIterationMethod(p, "some", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				continue
			}
			res, err := rt.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)})
			if err != nil {
				return Undefined, err
			}
			if res.Truthy() {
				return True, nil
			}
		}
		return False, nil
	})

	r.defIterationMethod(p, "every", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				continue
			}
			res, err := rt.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)})
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
		return rt.reduceArray(this, args, false)
	})

	r.defMethod(p, "sort", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		cmp := arg(args, 0)
		// The comparator is checked before the receiver is even coerced, so a
		// bad one is reported whatever it was going to be applied to.
		if !cmp.IsUndefined() && !isCallable(cmp) {
			return Undefined, rt.throwTypeError("the comparator is not a function")
		}
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		sorted, err := rt.sortIndexed(a, cmp)
		if err != nil {
			return Undefined, err
		}
		// The present elements go back at the front and the rest of the
		// positions are emptied, which is what moves holes to the end and
		// keeps them holes.
		i := int64(0)
		for ; i < int64(len(sorted)); i++ {
			if err := a.set(rt, i, sorted[i]); err != nil {
				return Undefined, err
			}
		}
		for ; i < a.n; i++ {
			if err := a.remove(rt, i); err != nil {
				return Undefined, err
			}
		}
		return Obj(a.o), nil
	})

	// Symbol.iterator is set alongside values, which it has to be the very same
	// function as.
}

// iterationFn is the body of an array method that takes a callback.
type iterationFn func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error)

// defIterationMethod defines an array method that takes a callback and an
// optional `this` argument, which is the shape most of them share.
func (r *Runtime) defIterationMethod(p *Object, name string, body iterationFn) {
	r.defMethod(p, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The receiver is coerced and its length read before the callback is
		// checked, which is the order the specification observes: a getter on
		// length runs even when the callback turns out to be missing.
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		cb := arg(args, 0)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("%s requires a function", name)
		}
		return body(rt, a, cb, arg(args, 1))
	})
}

// findIn backs find, findIndex, findLast and findLastIndex, which differ only
// in the direction they walk and what they report.
//
// All four visit holes, unlike filter and forEach, reporting them as undefined:
// they are looking for a position, and a hole is a position.
func (r *Runtime) findIn(a *arrayLike, cb, thisArg Value, backwards bool) (Value, int64, error) {
	for k := int64(0); k < a.n; k++ {
		i := k
		if backwards {
			i = a.n - 1 - k
		}
		el, err := a.get(r, i)
		if err != nil {
			return Undefined, -1, err
		}
		ok, err := r.call(cb, thisArg, []Value{el, Float(float64(i)), Obj(a.o)})
		if err != nil {
			return Undefined, -1, err
		}
		if ok.Truthy() {
			return el, i, nil
		}
	}
	return Undefined, -1, nil
}

// reduceArray backs reduce and reduceRight.
func (r *Runtime) reduceArray(this Value, args []Value, backwards bool) (Value, error) {
	name := "reduce"
	if backwards {
		name = "reduceRight"
	}
	a, err := r.viewArrayLike(this)
	if err != nil {
		return Undefined, err
	}
	cb := arg(args, 0)
	if !isCallable(cb) {
		return Undefined, r.throwTypeError("%s requires a function", name)
	}

	k := int64(0)
	var acc Value
	seeded := len(args) > 1
	if seeded {
		acc = args[1]
	}
	// Without an initial value the first present element seeds the
	// accumulator, and a list with none at all is an error rather than
	// undefined -- the one case where reduce refuses to guess.
	for !seeded && k < a.n {
		i := k
		if backwards {
			i = a.n - 1 - k
		}
		el, present, err := a.at(r, i)
		if err != nil {
			return Undefined, err
		}
		k++
		if present {
			acc, seeded = el, true
		}
	}
	if !seeded {
		return Undefined, r.throwTypeError("%s of an empty array with no initial value", name)
	}

	for ; k < a.n; k++ {
		i := k
		if backwards {
			i = a.n - 1 - k
		}
		el, present, err := a.at(r, i)
		if err != nil {
			return Undefined, err
		}
		if !present {
			continue
		}
		acc, err = r.call(cb, Undefined, []Value{acc, el, Float(float64(i)), Obj(a.o)})
		if err != nil {
			return Undefined, err
		}
	}
	return acc, nil
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
	// The error is discarded: this form is for closing during an abrupt
	// completion, where the completion already in flight is the one that
	// matters.
	_ = r.closeIteratorErr(iter)
}

// closeIteratorErr closes an iterator and reports what went wrong.
//
// Closing during an ordinary completion -- a helper's own return(), or take
// reaching its limit -- has nothing else in flight, so a failure there is the
// result rather than something to swallow.
func (r *Runtime) closeIteratorErr(iter Value) error {
	ret, err := r.getValueProp(iter, atomReturn)
	if err != nil {
		return err
	}
	if !isCallable(ret) {
		return nil
	}
	_, err = r.call(ret, iter, nil)
	return err
}

// arrayIterKind says what an array iterator yields.
type arrayIterKind uint8

const (
	iterValues arrayIterKind = iota
	iterKeys
	iterEntries
)

// newArrayIterator builds an iterator over an array's elements.
func (r *Runtime) newArrayIterator(target Value) (Value, error) {
	return r.newArrayIteratorKind(target, iterValues)
}

// newArrayIteratorKind builds an iterator over an array's indices, elements or
// both.
//
// It reads length and each element as it goes rather than taking a snapshot,
// because an array that grows or shrinks during iteration is meant to be seen
// doing so. Reading through the property table rather than the dense elements
// is what makes a frozen or sparse array iterate at all: freezing moves the
// elements out of the dense slice.
func (r *Runtime) newArrayIteratorKind(target Value, kind arrayIterKind) (Value, error) {
	o, err := r.toObject(target)
	if err != nil {
		return Undefined, err
	}
	iter := newObject(r.proto.arrayIter, ClassIterator)
	iter.data = &arrayIterData{a: arrayLike{o: o}, kind: kind}
	return Obj(iter), nil
}

// arrayIterData is where an array iterator is in its array.
type arrayIterData struct {
	a    arrayLike
	kind arrayIterKind
	i    int64
	done bool
}

// initArrayIteratorProto fills in %ArrayIteratorPrototype%.
//
// The next method lives here rather than on each iterator, so that every array
// iterator has the same one -- which a script can check, and which is what
// makes the prototype worth having.
func (r *Runtime) initArrayIteratorProto() {
	p := r.proto.arrayIter
	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, ok := arrayIterOf(this)
		if !ok {
			return Undefined, rt.throwTypeError(
				"Array Iterator.prototype.next called on an incompatible receiver")
		}
		if d.done {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		// A typed array whose buffer was detached mid-iteration is a TypeError
		// rather than a silent end: its length reads as zero, which would
		// otherwise look like exhaustion.
		if d.a.o.class == ClassTypedArray {
			if _, err := rt.typedArrayOf(Obj(d.a.o), "Array Iterator.prototype.next"); err != nil {
				d.done = true
				return Undefined, err
			}
		}
		n, err := rt.lengthOf(d.a.o)
		if err != nil {
			d.done = true
			return Undefined, err
		}
		if d.i >= n {
			d.done = true
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		idx := d.i
		d.i++
		if d.kind == iterKeys {
			return Obj(rt.iterResult(Float(float64(idx)), false)), nil
		}
		v, err := d.a.get(rt, idx)
		if err != nil {
			d.done = true
			return Undefined, err
		}
		if d.kind == iterEntries {
			v = Obj(rt.newArrayFrom([]Value{Float(float64(idx)), v}))
		}
		return Obj(rt.iterResult(v, false)), nil
	})
	p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.toStringTag),
		Str(NewString("Array Iterator")), propConfigurable)
}

func arrayIterOf(v Value) (*arrayIterData, bool) {
	if !v.IsObject() {
		return nil, false
	}
	d, ok := v.Object().data.(*arrayIterData)
	return d, ok
}

// ---------------------------------------------------------------------------
// Number, Boolean, Symbol
// ---------------------------------------------------------------------------

func (r *Runtime) initNumberBuiltins() {
	p := r.proto.number

	ctor := r.newCtor("Number", 1, p, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n := float64(0)
		if len(args) > 0 {
			// Number is the one place a BigInt converts to a Number without
			// complaint: it is what a program asking for the conversion
			// explicitly has written, as opposed to one mixing the two kinds
			// by accident.
			prim, err := rt.toPrimitive(args[0], hintNumber)
			if err != nil {
				return Undefined, err
			}
			if prim.IsBigInt() {
				n = bigIntToFloat(prim.BigInt())
			} else {
				v, err := rt.toNumber(prim)
				if err != nil {
					return Undefined, err
				}
				n = v
			}
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
			// Symbol refuses to construct, so that every symbol is a
			// primitive. It is still a constructor, which is why a class may
			// extend it -- the refusal happens when the subclass is built.
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
	r.defMethod(p, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.thisSymbol(this)
		if err != nil {
			return Undefined, err
		}
		return Sym(s), nil
	})
	r.defToStringTag(p, "Symbol")

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

	// A symbol wrapper coerces to the symbol it wraps rather than throwing the
	// way an implicit conversion of a symbol does, which is what makes
	// `Object(sym) == sym` true. The hint is ignored: there is nothing else it
	// could produce.
	r.defSymbolMethod(p, r.wellKnown.toPrimitive, "[Symbol.toPrimitive]", 1,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			s, err := rt.thisSymbol(this)
			if err != nil {
				return Undefined, err
			}
			return Sym(s), nil
		})
	// The method is not writable, which is how a script can tell it apart from
	// one a program installed.
	if pd := p.getOwn(r.atoms.internSymbol(r.wellKnown.toPrimitive)); pd != nil {
		pd.flags &^= propWritable
	}
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

		// AggregateError takes the list of errors before the message, so it has
		// one more parameter than the rest.
		arity := 1
		if kind == errAggregate {
			arity = 2
		}
		ctor := r.newCtor(name, arity, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
			o := newObject(rt.proto.nativeErrors[kind], ClassError)
			// AggregateError takes the list of causes first, so its message is
			// the second argument rather than the first.
			msgArg, optsArg := arg(args, 0), arg(args, 1)
			if kind == errAggregate {
				msgArg, optsArg = arg(args, 1), arg(args, 2)
			}
			msg := ""
			if m := msgArg; !m.IsUndefined() {
				s, err := rt.toString(m)
				if err != nil {
					return Undefined, err
				}
				msg = s.Go()
				o.setOwnRaw(atomMessage, Str(s), propWritable|propConfigurable)
			}
			// The cause option, when present, is attached as an own property.
			if opts := optsArg; opts.IsObject() {
				causeKey := rt.atoms.intern("cause")
				if rt.hasProp(opts.Object(), causeKey) {
					cause, err := rt.getProp(opts.Object(), causeKey, opts)
					if err != nil {
						return Undefined, err
					}
					o.setOwnRaw(causeKey, cause, propWritable|propConfigurable)
				}
			}
			// The list of errors is drained last, after the message and the
			// options have been read: the iterator may run user code, and it
			// runs after they do.
			if kind == errAggregate {
				var errs []Value
				if err := rt.iterate(arg(args, 0), func(v Value) error {
					errs = append(errs, v)
					return nil
				}); err != nil {
					return Undefined, err
				}
				o.setOwnRaw(rt.atoms.intern("errors"), Obj(rt.newArrayFrom(errs)),
					propWritable|propConfigurable)
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
	r.defToStringTag(m, "Math")
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
		// The half-precision counterpart, which is how a script sees what a
		// Float16Array would store without allocating one.
		"f16round": func(f float64) float64 { return float16frombits(float16bits(f)) },
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
		nums, err := rt.coerceAll(args)
		if err != nil {
			return Undefined, err
		}
		// An infinity wins over a NaN here, unlike everywhere else: the length
		// of a vector with an infinite side is infinite whatever the other
		// sides are, including unknown.
		sawNaN, largest := false, 0.0
		for _, n := range nums {
			switch {
			case math.IsInf(n, 0):
				return Float(inf(1)), nil
			case math.IsNaN(n):
				sawNaN = true
			default:
				if a := math.Abs(n); a > largest {
					largest = a
				}
			}
		}
		if sawNaN {
			return Float(nan()), nil
		}
		if largest == 0 {
			return Float(0), nil
		}
		// The squares are scaled by the largest term, so that a vector of large
		// or small components neither overflows nor underflows on the way.
		sum := 0.0
		for _, n := range nums {
			q := n / largest
			sum += q * q
		}
		return Float(largest * math.Sqrt(sum)), nil
	})

	r.defMethod(m, "random", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Float(rt.random()), nil
	})
}

// coerceAll converts every argument to a number, in the order they were
// written.
//
// Math.max and its relatives convert the whole list before they look at any of
// it, so an argument whose valueOf has a side effect has it even when an
// earlier argument has already settled the answer.
func (r *Runtime) coerceAll(args []Value) ([]float64, error) {
	nums := make([]float64, len(args))
	for i, a := range args {
		n, err := r.toNumber(a)
		if err != nil {
			return nil, err
		}
		nums[i] = n
	}
	return nums, nil
}

// mathExtremum implements Math.max and Math.min, which return NaN if any
// argument is NaN and treat -0 as less than +0.
func (r *Runtime) mathExtremum(args []Value, wantMax bool) (Value, error) {
	nums, err := r.coerceAll(args)
	if err != nil {
		return Undefined, err
	}
	best := inf(-1)
	if !wantMax {
		best = inf(1)
	}
	for _, n := range nums {
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
//
// The sign of a zero result is the sign of the argument, so Math.round(-0.2) is
// -0 as well -- visible through 1/x, which is where a program notices.
func jsRound(f float64) float64 {
	switch {
	case math.IsNaN(f), math.IsInf(f, 0), f == 0:
		return f
	case f > 0 && f < 0.5:
		return 0
	case f < 0 && f >= -0.5:
		return math.Copysign(0, -1)
	case math.Abs(f) >= 1<<52:
		// Past this every double is already a whole number, and adding a half
		// to one would round rather than carry.
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
	// Number.parseInt and Number.parseFloat are the global functions
	// themselves rather than copies, which is what makes Number.parseInt ===
	// parseInt. They are installed once the globals exist.
	defer func() {
		ctor, ok := r.globalObj("Number")
		if !ok {
			return
		}
		for _, name := range []string{"parseInt", "parseFloat"} {
			key := r.atoms.intern(name)
			if p := r.global.getOwn(key); p != nil {
				ctor.setOwnRaw(key, p.value, propWritable|propConfigurable)
			}
		}
	}()

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

	r.defMethod(p, "fill", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The length comes from the property, not from the dense storage:
		// freezing an array moves its elements out of that storage, and a
		// frozen array is exactly where fill has to report that it cannot
		// write.
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		v := arg(args, 0)
		start, err := rt.relativeIndex(arg(args, 1), int(a.n), 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 2), int(a.n), int(a.n))
		if err != nil {
			return Undefined, err
		}
		for i := int64(start); i < int64(end) && i < a.n; i++ {
			// Through the property protocol, so that a read-only element is
			// the TypeError it should be rather than a silent write.
			if err := a.set(rt, i, v); err != nil {
				return Undefined, err
			}
		}
		// The object, not the receiver: called on a primitive, what was filled
		// is the wrapper.
		return Obj(a.o), nil
	})

	r.defMethod(p, "flat", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		depth := 1.0
		if d := arg(args, 0); !d.IsUndefined() {
			if depth, err = rt.toInteger(d); err != nil {
				return Undefined, err
			}
		}
		out, err := rt.arraySpeciesCreate(this, 0)
		if err != nil {
			return Undefined, err
		}
		if err := rt.flatten(a, int(min(depth, maxArrayLength)), out); err != nil {
			return Undefined, err
		}
		return out.value(), nil
	})
}

// flatten appends the elements of nested arrays up to the given depth.
//
// It reads through the view rather than the dense elements, so that it works on
// an array-like and sees an element a getter produces; a hole contributes
// nothing at any depth.
func (r *Runtime) flatten(a *arrayLike, depth int, out *arrayOut) error {
	for i := int64(0); i < a.n; i++ {
		if err := r.tick(); err != nil {
			return err
		}
		v, present, err := a.at(r, i)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if depth > 0 && v.IsObject() && v.Object().IsArray() {
			inner, err := r.viewArrayLike(v)
			if err != nil {
				return err
			}
			if err := r.flatten(inner, depth-1, out); err != nil {
				return err
			}
			continue
		}
		if err := out.push(r, v); err != nil {
			return err
		}
	}
	return nil
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
	return o.getElem(uint32(i))
}

// clipRange re-clamps a range against an array's current length.
//
// Every index a built-in computes goes through ToInteger, which may call a
// valueOf that mutates the array the indices were derived from:
//
//	var a = [1, 2, 3];
//	a.fill(0, {valueOf() { a.length = 0; return 0; }});
//
// By the time the range is used it may name elements that no longer exist, so
// it is clipped immediately before indexing rather than only when computed.
func clipRange(o *Object, start, end int) (int, int) {
	n := len(o.elems)
	if start > n {
		start = n
	}
	if end > n {
		end = n
	}
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return start, end
}

// sortIndexed collects an array-like's present elements and sorts them.
//
// The elements are taken out, sorted and put back rather than moved in place,
// because a comparator is arbitrary code: it may shorten the array, replace its
// elements or throw, and none of that may be allowed to corrupt what is being
// sorted or to reach outside it.
//
// Holes are left out entirely -- they are not values to compare, and they end
// up at the end because the caller puts nothing there -- while undefined sorts
// after everything, without the comparator being asked.
func (r *Runtime) sortIndexed(a *arrayLike, cmp Value) ([]Value, error) {
	items := make([]Value, 0, min(int(a.n), 1024))
	for i := int64(0); i < a.n; i++ {
		v, present, err := a.at(r, i)
		if err != nil {
			return nil, err
		}
		if present {
			items = append(items, v)
		}
	}

	var sortErr error
	sort.SliceStable(items, func(i, j int) bool {
		if sortErr != nil {
			return false
		}
		less, err := r.compareForSort(items[i], items[j], cmp)
		if err != nil {
			sortErr = err
			return false
		}
		return less
	})
	if sortErr != nil {
		return nil, sortErr
	}
	return items, nil
}

// compareForSort reports whether x sorts before y.
func (r *Runtime) compareForSort(x, y, cmp Value) (bool, error) {
	// undefined sorts after everything, and the comparator is not consulted
	// about it -- which is what lets a comparator assume its arguments are
	// values it put there.
	switch {
	case x.IsUndefined():
		return false, nil
	case y.IsUndefined():
		return true, nil
	}
	if isCallable(cmp) {
		res, err := r.call(cmp, Undefined, []Value{x, y})
		if err != nil {
			return false, err
		}
		n, err := r.toNumber(res)
		if err != nil {
			return false, err
		}
		// A comparator returning NaN says nothing, which a stable sort turns
		// into "leave them as they are".
		return n < 0, nil
	}
	// The default comparison is by the string form, which is why [10, 9].sort()
	// gives [10, 9].
	sx, err := r.toString(x)
	if err != nil {
		return false, err
	}
	sy, err := r.toString(y)
	if err != nil {
		return false, err
	}
	return sx.Compare(sy) < 0, nil
}

// createIndexed defines one element of a result the caller promised to produce,
// so a define the object refuses is an error rather than something to ignore.
func (r *Runtime) createIndexed(o *Object, i int64, v Value) error {
	ok, err := r.defineProperty(o, r.indexKey(i), &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot create index %d of the result", i)
	}
	return nil
}
