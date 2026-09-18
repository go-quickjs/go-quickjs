package vm

import (
	"math"
	"math/bits"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// The remainder of the standard library.
//
// These are split out from builtins.go only for size. Each is an ordinary
// method; the ones with behaviour worth explaining carry a comment where the
// surprise is.

func (r *Runtime) initExtraBuiltins() {
	r.initArrayExtras2()
	r.initObjectExtras()
	r.initStringExtras()
	r.initNumberExtras()
	r.initMathExtras()
	r.initURIFunctions()
	r.initPromiseExtras()
}

// ---------------------------------------------------------------------------
// Array
// ---------------------------------------------------------------------------

func (r *Runtime) initArrayExtras2() {
	p := r.proto.array

	r.defMethod(p, "splice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex64(arg(args, 0), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		// With no second argument splice removes everything from start; with
		// one it removes that many. The difference is observable, so the
		// argument count is checked rather than the value.
		var removeCount int64
		var inserted []Value
		switch {
		case len(args) == 0:
			removeCount = 0
		case len(args) == 1:
			removeCount = a.n - start
		default:
			c, err := rt.toInteger(args[1])
			if err != nil {
				return Undefined, err
			}
			removeCount = int64(clampFloatIndex(c, int(a.n-start)))
			inserted = args[2:]
		}
		if a.n+int64(len(inserted))-removeCount > maxArrayLength {
			return Undefined, rt.throwTypeError("the result would be too long")
		}

		out, err := rt.arraySpeciesCreate(this, removeCount)
		if err != nil {
			return Undefined, err
		}
		for k := int64(0); k < removeCount; k++ {
			v, present, err := a.at(rt, start+k)
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
		if err := out.setLength(rt, removeCount); err != nil {
			return Undefined, err
		}

		// The tail moves by the difference between what went and what came,
		// left or right depending on the sign, so that the elements it passes
		// over are never overwritten before they are read.
		add := int64(len(inserted))
		switch {
		case add < removeCount:
			for k := start; k < a.n-removeCount; k++ {
				v, present, err := a.at(rt, k+removeCount)
				if err != nil {
					return Undefined, err
				}
				if err := a.put(rt, k+add, v, present); err != nil {
					return Undefined, err
				}
			}
			for k := a.n; k > a.n-removeCount+add; k-- {
				if err := a.remove(rt, k-1); err != nil {
					return Undefined, err
				}
			}
		case add > removeCount:
			for k := a.n - removeCount; k > start; k-- {
				v, present, err := a.at(rt, k+removeCount-1)
				if err != nil {
					return Undefined, err
				}
				if err := a.put(rt, k+add-1, v, present); err != nil {
					return Undefined, err
				}
			}
		}
		for i, v := range inserted {
			if err := a.set(rt, start+int64(i), v); err != nil {
				return Undefined, err
			}
		}
		if err := a.setLength(rt, a.n-removeCount+add); err != nil {
			return Undefined, err
		}
		return out.value(), nil
	})

	r.defMethod(p, "copyWithin", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		target, err := rt.relativeIndex64(arg(args, 0), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex64(arg(args, 1), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex64(arg(args, 2), a.n, a.n)
		if err != nil {
			return Undefined, err
		}
		count := end - start
		if n := a.n - target; n < count {
			count = n
		}
		if count <= 0 {
			return Obj(a.o), nil
		}
		// Copying backwards when the ranges overlap the other way is what stops
		// an element being overwritten before it has been read.
		step := int64(1)
		if start < target && target < start+count {
			start += count - 1
			target += count - 1
			step = -1
		}
		for ; count > 0; count-- {
			v, present, err := a.at(rt, start)
			if err != nil {
				return Undefined, err
			}
			if err := a.put(rt, target, v, present); err != nil {
				return Undefined, err
			}
			start += step
			target += step
		}
		// The object, not the receiver: called on a primitive, what was copied
		// within is the wrapper.
		return Obj(a.o), nil
	})

	r.defMethod(p, "reduceRight", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.reduceArray(this, args, true)
	})

	r.defIterationMethod(p, "flatMap", func(rt *Runtime, a *arrayLike, cb Value, thisArg Value) (Value, error) {
		out, err := rt.arraySpeciesCreate(Obj(a.o), 0)
		if err != nil {
			return Undefined, err
		}
		// The arguments are the same three every time, so the list they go in
		// is made once rather than per element: a callee may not keep it, any
		// more than it may keep the interpreter's own stack.
		var argv [3]Value
		for i := int64(0); i < a.n; i++ {
			el, present, err := a.at(rt, i)
			if err != nil {
				return Undefined, err
			}
			if !present {
				continue
			}
			argv[0], argv[1], argv[2] = el, Float(float64(i)), Obj(a.o)
			v, err := rt.call(cb, thisArg, argv[:])
			if err != nil {
				return Undefined, err
			}
			// flatMap flattens exactly one level, never more.
			if v.IsObject() && v.Object().IsArray() {
				inner, err := rt.viewArrayLike(v)
				if err != nil {
					return Undefined, err
				}
				for j := int64(0); j < inner.n; j++ {
					iv, present, err := inner.at(rt, j)
					if err != nil {
						return Undefined, err
					}
					if present {
						if err := out.push(rt, iv); err != nil {
							return Undefined, err
						}
					}
				}
				continue
			}
			if err := out.push(rt, v); err != nil {
				return Undefined, err
			}
		}
		return out.value(), nil
	})

	// The iteration-protocol methods.
	// Array.prototype[Symbol.unscopables] lists the methods added after `with`
	// existed, so that `with (arr) { values }` still means whatever `values`
	// meant outside. It is what lets the language keep adding array methods
	// without breaking code that happens to use the same name.
	unscopables := newObject(nil, ClassObject)
	for _, name := range []string{
		"at", "copyWithin", "entries", "fill", "find", "findIndex", "findLast",
		"findLastIndex", "flat", "flatMap", "includes", "keys", "toReversed",
		"toSorted", "toSpliced", "values",
	} {
		unscopables.setOwnRaw(r.atoms.intern(name), True, propDefault)
	}
	p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.unscopables), Obj(unscopables),
		propConfigurable)

	// Array.prototype[Symbol.iterator] is not merely equivalent to values, it is
	// the same function object, which a script can check and which the
	// iteration fast paths rely on to decide that an array still iterates the
	// way they assume.
	values := r.defMethod(p, "values", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.newArrayIterator(this)
	})
	r.arrayValuesFn = values
	p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator), Obj(values),
		propWritable|propConfigurable)
	r.defMethod(p, "keys", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.newArrayIteratorKind(this, iterKeys)
	})
	r.defMethod(p, "entries", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.newArrayIteratorKind(this, iterEntries)
	})

	// toLocaleString asks each element how it would like to be written, which
	// is the only difference from join -- and the whole point, since what a
	// number or a date looks like is a per-element question.
	r.defMethod(p, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.arrayToLocaleString(this, args)
	})

	// The change-by-copy methods, which return a new array rather than
	// mutating the receiver.
	// The change-by-copy methods all read through the view and write a fresh
	// plain array -- deliberately plain, since the point of them is to leave
	// the receiver alone, and a species that did something else would defeat
	// that.
	r.defMethod(p, "toReversed", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		out, err := rt.arrayCreate(a.n)
		if err != nil {
			return Undefined, err
		}
		for i := int64(0); i < a.n; i++ {
			v, err := a.get(rt, a.n-1-i)
			if err != nil {
				return Undefined, err
			}
			out.elems = append(out.elems, v)
		}
		return Obj(out), nil
	})

	r.defMethod(p, "toSorted", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if cmp := arg(args, 0); !cmp.IsUndefined() && !isCallable(cmp) {
			return Undefined, rt.throwTypeError("the comparator is not a function")
		}
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		out, err := rt.arrayCreate(a.n)
		if err != nil {
			return Undefined, err
		}
		for i := int64(0); i < a.n; i++ {
			v, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			out.elems = append(out.elems, v)
		}
		fn, err := rt.getValueProp(Obj(out), rt.atoms.intern("sort"))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.call(fn, Obj(out), args); err != nil {
			return Undefined, err
		}
		return Obj(out), nil
	})

	r.defMethod(p, "toSpliced", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex64(arg(args, 0), a.n, 0)
		if err != nil {
			return Undefined, err
		}
		var removeCount int64
		var inserted []Value
		switch {
		case len(args) == 0:
			removeCount = 0
		case len(args) == 1:
			removeCount = a.n - start
		default:
			c, err := rt.toInteger(args[1])
			if err != nil {
				return Undefined, err
			}
			removeCount = int64(clampFloatIndex(c, int(a.n-start)))
			inserted = args[2:]
		}
		n := a.n + int64(len(inserted)) - removeCount
		if n > maxArrayLength {
			return Undefined, rt.throwTypeError("the result would be too long")
		}
		out, err := rt.arrayCreate(n)
		if err != nil {
			return Undefined, err
		}
		for i := int64(0); i < start; i++ {
			v, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			out.elems = append(out.elems, v)
		}
		out.elems = append(out.elems, inserted...)
		for i := start + removeCount; i < a.n; i++ {
			v, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			out.elems = append(out.elems, v)
		}
		return Obj(out), nil
	})

	r.defMethod(p, "with", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.viewArrayLike(this)
		if err != nil {
			return Undefined, err
		}
		i, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if i < 0 {
			i += float64(a.n)
		}
		if !(i >= 0) || i >= float64(a.n) {
			return Undefined, rt.throwRangeError("invalid index")
		}
		out, err := rt.arrayCreate(a.n)
		if err != nil {
			return Undefined, err
		}
		for k := int64(0); k < a.n; k++ {
			if k == int64(i) {
				out.elems = append(out.elems, arg(args, 1))
				continue
			}
			v, err := a.get(rt, k)
			if err != nil {
				return Undefined, err
			}
			out.elems = append(out.elems, v)
		}
		return Obj(out), nil
	})
}

func clampInt(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}

// ---------------------------------------------------------------------------
// Object
// ---------------------------------------------------------------------------

func (r *Runtime) initObjectExtras() {
	ctorVal, err := r.getProp(r.global, r.atoms.intern("Object"), Obj(r.global))
	if err != nil || !ctorVal.IsObject() {
		return
	}
	ctor := ctorVal.Object()

	r.defMethod(ctor, "hasOwn", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		key, err := rt.toPropertyKey(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		return Bool(rt.hasOwnProp(o, key)), nil
	})

	r.defMethod(ctor, "getOwnPropertySymbols", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		keys, err := rt.ownKeysOf(o, true)
		if err != nil {
			return Undefined, err
		}
		var out []Value
		for _, k := range keys {
			if sym := rt.atoms.symbol(k); sym != nil {
				out = append(out, Sym(sym))
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})

	r.defMethod(ctor, "getOwnPropertyDescriptors", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		keys, err := rt.ownKeysOf(o, true)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		for _, k := range keys {
			d, err := rt.ownDescriptorOf(o, k)
			if err != nil {
				return Undefined, err
			}
			if d.IsUndefined() {
				continue
			}
			out.setOwnRaw(k, d, propDefault)
		}
		return Obj(out), nil
	})

	r.defMethod(ctor, "seal", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return v, nil
		}
		return v, rt.setIntegrity(v.Object(), false)
	})

	r.defMethod(ctor, "isSealed", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return True, nil
		}
		ok, err := rt.testIntegrity(v.Object(), false)
		return Bool(ok), err
	})

	r.defMethod(ctor, "groupBy", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		cb := arg(args, 1)
		if !isCallable(cb) {
			return Undefined, rt.throwTypeError("Object.groupBy requires a function")
		}
		// The result has a null prototype, so a group named "toString" does not
		// collide with an inherited method.
		out := newObject(nil, ClassObject)
		i := 0
		// One list for the callback's arguments, refilled per element.
		var argv [2]Value
		err := rt.iterate(arg(args, 0), func(v Value) error {
			argv[0], argv[1] = v, Int(i)
			keyVal, err := rt.call(cb, Undefined, argv[:])
			i++
			if err != nil {
				return err
			}
			key, err := rt.toPropertyKey(keyVal)
			if err != nil {
				return err
			}
			group := out.getOwn(key)
			if group == nil {
				arr := rt.newArrayFrom([]Value{v})
				out.setOwnRaw(key, Obj(arr), propDefault)
				return nil
			}
			if group.value.IsObject() {
				g := group.value.Object()
				g.elems = append(g.elems, v)
			}
			return nil
		})
		if err != nil {
			return Undefined, err
		}
		return Obj(out), nil
	})

	// The legacy accessor helpers, which predate Object.defineProperty.
	p := r.proto.object
	r.defMethod(p, "__defineGetter__", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		// The function is checked before the key is converted, so a key whose
		// toString counts its calls sees none.
		fn := arg(args, 1)
		if !isCallable(fn) {
			return Undefined, rt.throwTypeError("__defineGetter__ requires a function")
		}
		key, err := rt.toPropertyKey(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// Through the object's own define, so that a proxy sees it and a
		// refusal is reported rather than ignored.
		return Undefined, rt.defineAccessorOrThrow(o, key, fn, Undefined)
	})
	r.defMethod(p, "__defineSetter__", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		// The function is checked before the key is converted, so a key whose
		// toString counts its calls sees none.
		fn := arg(args, 1)
		if !isCallable(fn) {
			return Undefined, rt.throwTypeError("__defineSetter__ requires a function")
		}
		key, err := rt.toPropertyKey(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Undefined, rt.defineAccessorOrThrow(o, key, Undefined, fn)
	})
	r.defMethod(p, "__lookupGetter__", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.lookupAccessor(this, arg(args, 0), true)
	})
	r.defMethod(p, "__lookupSetter__", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.lookupAccessor(this, arg(args, 0), false)
	})

	// __proto__ as an accessor, which is how it is specified.
	getProto := r.newNativeFunc("get __proto__", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.toObject(this)
		if err != nil {
			return Undefined, err
		}
		if p := proxyOf(o); p != nil {
			return rt.proxyGetPrototypeOf(p)
		}
		return protoValue(o), nil
	})
	setProto := r.newNativeFunc("set __proto__", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if this.IsNullish() {
			return Undefined, rt.throwTypeError("cannot set __proto__ of %s", rt.describe(this))
		}
		proto := arg(args, 0)
		// Anything that is not an object or null is simply ignored, which is
		// the one place assigning a prototype is not an error.
		if !proto.IsObject() && !proto.IsNull() {
			return Undefined, nil
		}
		if !this.IsObject() {
			return Undefined, nil
		}
		if p := proxyOf(this.Object()); p != nil {
			ok, err := rt.proxySetPrototypeOf(p, proto)
			if err != nil {
				return Undefined, err
			}
			if !ok {
				return Undefined, rt.throwTypeError("cannot set the prototype of this object")
			}
			return Undefined, nil
		}
		// The chain is checked first: a cycle would make every property lookup
		// walk it forever.
		if !rt.setProtoOfChecked(this.Object(), proto) {
			return Undefined, rt.throwTypeError("cannot set the prototype of this object")
		}
		return Undefined, nil
	})
	r.defineAccessor(p, atomProto, getProto, setProto, propConfigurable)
}

// defineAccessorOrThrow defines one half of an accessor through the object's own
// machinery, which for a proxy is its trap.
func (r *Runtime) defineAccessorOrThrow(o *Object, key Atom, getter, setter Value) error {
	d := &propDesc{
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	}
	if isCallable(getter) {
		d.getter, d.hasGet = getter.Object(), true
	}
	if isCallable(setter) {
		d.setter, d.hasSet = setter.Object(), true
	}
	ok, err := r.defineProperty(o, key, d)
	if err != nil {
		return err
	}
	if !ok {
		return r.throwTypeError("cannot redefine property %q", r.atoms.name(key))
	}
	return nil
}

// lookupAccessor walks the prototype chain for one half of an accessor, asking
// each object what it has rather than reading its table -- so a proxy along the
// way answers through its traps.
func (r *Runtime) lookupAccessor(this, key Value, wantGetter bool) (Value, error) {
	o, err := r.toObject(this)
	if err != nil {
		return Undefined, err
	}
	k, err := r.toPropertyKey(key)
	if err != nil {
		return Undefined, err
	}
	for cur := o; cur != nil; {
		d, err := r.ownPropDesc(cur, k)
		if err != nil {
			return Undefined, err
		}
		if d != nil {
			if !d.isAccessor() {
				return Undefined, nil
			}
			if wantGetter && d.getter != nil {
				return Obj(d.getter), nil
			}
			if !wantGetter && d.setter != nil {
				return Obj(d.setter), nil
			}
			return Undefined, nil
		}
		next, err := r.protoOf(cur)
		if err != nil {
			return Undefined, err
		}
		if !next.IsObject() {
			return Undefined, nil
		}
		cur = next.Object()
	}
	return Undefined, nil
}

// ---------------------------------------------------------------------------
// String
// ---------------------------------------------------------------------------

func (r *Runtime) initStringExtras() {
	p := r.proto.str

	thisStr := func(rt *Runtime, this Value) (*String, error) {
		if this.IsNullish() {
			return nil, rt.throwTypeError("String.prototype method called on %s", this.Kind())
		}
		return rt.toString(this)
	}

	r.defMethod(p, "substr", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex(arg(args, 0), s.Len(), 0)
		if err != nil {
			return Undefined, err
		}
		// substr takes a length, not an end index, which is what distinguishes
		// it from the substring it is so easily confused with.
		length := s.Len() - start
		if lv := arg(args, 1); !lv.IsUndefined() {
			n, err := rt.toInteger(lv)
			if err != nil {
				return Undefined, err
			}
			length = clampInt(int(n), 0, s.Len()-start)
		}
		return Str(s.Substring(start, start+length)), nil
	})

	// trimLeft and trimRight are the older names, kept as aliases.
	for _, pair := range [][2]string{{"trimLeft", "trimStart"}, {"trimRight", "trimEnd"}} {
		alias, target := pair[0], pair[1]
		if v, err := r.getProp(p, r.atoms.intern(target), Obj(p)); err == nil && isCallable(v) {
			p.setOwnRaw(r.atoms.intern(alias), v, propWritable|propConfigurable)
		}
	}

	r.defMethod(p, "normalize", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		form := "NFC"
		if f := arg(args, 0); !f.IsUndefined() {
			fs, err := rt.toString(f)
			if err != nil {
				return Undefined, err
			}
			form = fs.Go()
			switch form {
			case "NFC", "NFD", "NFKC", "NFKD":
			default:
				return Undefined, rt.throwRangeError("invalid normalization form")
			}
		}
		return Str(NewString(normalizeString(s.Go(), form))), nil
	})

	r.defMethod(p, "localeCompare", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		o, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// This is Intl.Collator by another name: it sorts the way the
		// language sorts, and takes the same locale and options.
		var rest []Value
		if len(args) > 1 {
			rest = args[1:]
		}
		c, err := rt.collatorFor(rest)
		if err != nil {
			return Undefined, err
		}
		return Int(c.compare(s.Go(), o.Go())), nil
	})

	// The locale-sensitive pair, which differ from the others in the handful
	// of languages that have rules of their own about it.
	r.defMethod(p, "toLocaleUpperCase", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		language, err := rt.caseLanguage(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(localeUpper(s.Go(), language))), nil
	})
	r.defMethod(p, "toLocaleLowerCase", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := thisStr(rt, this)
		if err != nil {
			return Undefined, err
		}
		language, err := rt.caseLanguage(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(localeLower(s.Go(), language))), nil
	})
}

// ---------------------------------------------------------------------------
// Number
// ---------------------------------------------------------------------------

func (r *Runtime) initNumberExtras() {
	p := r.proto.number

	r.defMethod(p, "toExponential", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		// The digit count is coerced before the value is looked at, so a
		// valueOf that throws is seen even for a NaN.
		digits, hasDigits := -1, false
		if d := arg(args, 0); !d.IsUndefined() {
			v, err := rt.toInteger(d)
			if err != nil {
				return Undefined, err
			}
			digits, hasDigits = int(v), true
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Str(NewString(jsnum.FormatFloat(n))), nil
		}
		if hasDigits && (digits < 0 || digits > 100) {
			return Undefined, rt.throwRangeError("toExponential() argument must be between 0 and 100")
		}
		if !hasDigits {
			// No count given: as few digits as round-trip, which is what the
			// shortest representation is.
			return Str(NewString(fixExponent(strconv.FormatFloat(n, 'e', -1, 64)))), nil
		}
		return Str(NewString(formatExponential(n, digits))), nil
	})

	r.defMethod(p, "toPrecision", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		d := arg(args, 0)
		if d.IsUndefined() {
			return Str(NewString(jsnum.FormatFloat(n))), nil
		}
		// The precision is coerced before the value is looked at, so a valueOf
		// that throws is seen even for a NaN.
		v, err := rt.toInteger(d)
		if err != nil {
			return Undefined, err
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return Str(NewString(jsnum.FormatFloat(n))), nil
		}
		if v < 1 || v > 100 {
			return Undefined, rt.throwRangeError("toPrecision() argument must be between 1 and 100")
		}
		return Str(NewString(formatPrecision(n, int(v)))), nil
	})

	// toLocaleString is Intl.NumberFormat under another name, which is what
	// ECMA-402 says it is.
	r.defMethod(p, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.thisNumber(this)
		if err != nil {
			return Undefined, err
		}
		text, err := rt.formatNumberFor(args, n)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(text)), nil
	})
}

// exactDecimal returns the exact decimal digits of a finite number and the
// exponent of the first of them: x is 0.d1d2... shifted so that x = d1.d2... ×
// 10**exp.
//
// It is exact because a double's decimal expansion is finite -- around 750
// digits at worst -- and asking for more than that many leaves only zeros to
// trim. The exact digits are what the rounding rules are written against: a
// value a hair under a half must round down even though the shortest
// representation of it reads as a half.
func exactDecimal(x float64) (digits string, exp int) {
	s := strconv.FormatFloat(x, 'e', 1100, 64)
	i := strings.IndexByte(s, 'e')
	mant := strings.Replace(s[:i], ".", "", 1)
	e, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "0", 0
	}
	mant = strings.TrimRight(mant, "0")
	if mant == "" {
		return "0", 0
	}
	return mant, e
}

// roundSignificant rounds a digit string to n significant digits, taking the
// larger value on a tie -- which is what the specification asks for, and not
// what the round-to-even that formats a float would do.
//
// It reports whether the rounding carried past the leading digit, which moves
// the exponent: 999 to three digits is 100 one decade up.
func roundSignificant(digits string, n int) (string, bool) {
	if len(digits) <= n {
		return digits + strings.Repeat("0", n-len(digits)), false
	}
	keep := []byte(digits[:n])
	if digits[n] < '5' {
		return string(keep), false
	}
	for i := n - 1; i >= 0; i-- {
		if keep[i] != '9' {
			keep[i]++
			return string(keep), false
		}
		keep[i] = '0'
	}
	return "1" + string(keep[:n-1]), true
}

// significantDigits renders a number as n significant digits and the exponent
// of the first of them.
func significantDigits(x float64, n int) (digits string, exp int) {
	if x == 0 {
		return strings.Repeat("0", n), 0
	}
	d, e := exactDecimal(x)
	rounded, carried := roundSignificant(d, n)
	if carried {
		e++
	}
	return rounded, e
}

// formatExponential renders a number as one digit, f fractional digits and an
// exponent.
func formatExponential(n float64, f int) string {
	sign := ""
	if n < 0 {
		// Negative zero has no sign here: the test is on the value, not on the
		// bit, so (-0).toExponential(0) is "0e+0".
		sign, n = "-", -n
	}
	digits, e := significantDigits(n, f+1)
	mant := digits[:1]
	if f > 0 {
		mant += "." + digits[1:]
	}
	return sign + mant + "e" + exponentSign(e) + strconv.Itoa(abs(e))
}

// formatPrecision renders a number with exactly p significant digits.
//
// Go's %g drops the trailing zeros of the mantissa, where JavaScript keeps
// them: (100).toPrecision(2) is "1.0e+2", not "1e+2". So the digits are taken
// from the exponential form and the decimal point is placed by hand.
func formatPrecision(n float64, p int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits, e := significantDigits(n, p)

	switch {
	case e < -6 || e >= p:
		// Too far from the decimal point to write out, so the exponent says
		// where it went.
		mant := digits[:1]
		if p > 1 {
			mant += "." + digits[1:]
		}
		return sign + mant + "e" + exponentSign(e) + strconv.Itoa(abs(e))
	case e == p-1:
		return sign + digits
	case e >= 0:
		return sign + digits[:e+1] + "." + digits[e+1:]
	default:
		return sign + "0." + strings.Repeat("0", -(e+1)) + digits
	}
}

func exponentSign(e int) string {
	if e < 0 {
		return "-"
	}
	return "+"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// fixExponent rewrites Go's exponent form into JavaScript's, which uses no
// zero padding: 1e+05 becomes 1e+5.
func fixExponent(s string) string {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return s
	}
	mant, exp := s[:i], s[i+1:]
	sign := ""
	if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
		sign, exp = string(exp[0]), exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if exp == "" {
		exp = "0"
	}
	if sign == "" {
		sign = "+"
	}
	return mant + "e" + sign + exp
}

// ---------------------------------------------------------------------------
// Math
// ---------------------------------------------------------------------------

func (r *Runtime) initMathExtras() {
	mVal, err := r.getProp(r.global, r.atoms.intern("Math"), Obj(r.global))
	if err != nil || !mVal.IsObject() {
		return
	}
	m := mVal.Object()

	r.defMethod(m, "clz32", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.toUint32(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Int(bits.LeadingZeros32(n)), nil
	})

	r.defMethod(m, "imul", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a, err := rt.toInt32(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		b, err := rt.toInt32(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		// The product wraps to 32 bits, which is the whole point of imul.
		return Int32(a * b), nil
	})
}

// ---------------------------------------------------------------------------
// URI handling
// ---------------------------------------------------------------------------

func (r *Runtime) initURIFunctions() {
	// The four URI functions differ only in which characters they leave alone.
	const (
		uriReserved  = ";/?:@&=+$,#"
		uriUnescaped = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	)

	encode := func(name, keep string) {
		r.defMethod(r.global, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			out, ok := encodeURIWith(s.Go(), keep)
			if !ok {
				return Undefined, rt.throwError(errURI, "URI malformed")
			}
			return Str(NewString(out)), nil
		})
	}
	encode("encodeURI", uriUnescaped+uriReserved)
	encode("encodeURIComponent", uriUnescaped)

	decode := func(name, keep string) {
		r.defMethod(r.global, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			out, ok := decodeURIWith(s.Go(), keep)
			if !ok {
				return Undefined, rt.throwError(errURI, "URI malformed")
			}
			return Str(NewString(out)), nil
		})
	}
	decode("decodeURI", uriReserved)
	decode("decodeURIComponent", "")

	r.defMethod(r.global, "queueMicrotask", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		fn := arg(args, 0)
		if !isCallable(fn) {
			return Undefined, rt.throwTypeError("queueMicrotask requires a function")
		}
		rt.enqueueJob(func() {
			// A microtask that throws has nowhere to report, so the error is
			// dropped rather than propagated into an unrelated turn.
			_, _ = rt.call(fn, Undefined, nil)
		})
		return Undefined, nil
	})
}

// encodeURIWith percent-encodes every character outside keep.
//
// A lone surrogate has no UTF-8 encoding, so it is rejected, which is what
// makes encodeURIComponent("\uD800") throw.
func encodeURIWith(s, keep string) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if strings.IndexByte(keep, c) >= 0 {
				sb.WriteByte(c)
			} else {
				writePercent(&sb, c)
			}
			i++
			continue
		}
		if _, isSurrogate := wtf8.DecodeSurrogateAt(s, i); isSurrogate {
			return "", false
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		for j := 0; j < size; j++ {
			writePercent(&sb, s[i+j])
		}
		i += size
	}
	return sb.String(), true
}

func writePercent(sb *strings.Builder, c byte) {
	const hex = "0123456789ABCDEF"
	sb.WriteByte('%')
	sb.WriteByte(hex[c>>4])
	sb.WriteByte(hex[c&0xF])
}

// decodeURIWith reverses the encoding, leaving the characters in keep encoded.
//
// Only what the escapes spell is decoded. Every other character is copied
// through untouched, whatever it is -- a lone surrogate in the input is not a
// malformed URI, it is a character decodeURI has nothing to say about, and only
// the bytes an escape sequence produces have to be well-formed UTF-8.
func decodeURIWith(s, keep string) (string, bool) {
	var buf []byte
	readEscape := func(i int) (byte, bool) {
		if i+2 >= len(s) || s[i] != '%' {
			return 0, false
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return 0, false
		}
		return byte(v), true
	}
	for i := 0; i < len(s); {
		if s[i] != '%' {
			buf = append(buf, s[i])
			i++
			continue
		}
		b, ok := readEscape(i)
		if !ok {
			return "", false
		}
		i += 3
		if b < utf8.RuneSelf {
			// A reserved character stays in its encoded form for decodeURI,
			// which is what keeps a decoded URI still parseable.
			if strings.IndexByte(keep, b) >= 0 {
				buf = append(buf, '%', s[i-2], s[i-1])
				continue
			}
			buf = append(buf, b)
			continue
		}
		// A byte outside ASCII opens a sequence, whose length its top bits
		// give. The continuation bytes must be escapes too: a literal one
		// would be a character of its own rather than part of this.
		n := 0
		switch {
		case b&0xE0 == 0xC0:
			n = 2
		case b&0xF0 == 0xE0:
			n = 3
		case b&0xF8 == 0xF0:
			n = 4
		default:
			return "", false
		}
		seq := []byte{b}
		for j := 1; j < n; j++ {
			c, ok := readEscape(i)
			if !ok || c&0xC0 != 0x80 {
				return "", false
			}
			seq = append(seq, c)
			i += 3
		}
		// The sequence has to be the shortest form of a real code point, which
		// rules out an overlong encoding and a surrogate half: what the escapes
		// spell is a character, not a code unit.
		rn, size := utf8.DecodeRune(seq)
		if rn == utf8.RuneError && size <= 1 {
			return "", false
		}
		buf = utf8.AppendRune(buf, rn)
	}
	return string(buf), true
}

// ---------------------------------------------------------------------------
// Promise
// ---------------------------------------------------------------------------

func (r *Runtime) initPromiseExtras() {
	ctorVal, err := r.getProp(r.global, r.atoms.intern("Promise"), Obj(r.global))
	if err != nil || !ctorVal.IsObject() {
		return
	}
	ctor := ctorVal.Object()

	r.defMethod(ctor, "withResolvers", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The promise is built with the constructor the method was reached
		// through, so a subclass's withResolvers hands back one of its own.
		cap, err := rt.newPromiseCapability(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		out.setOwnRaw(rt.atoms.intern("promise"), Obj(cap.promise), propDefault)
		out.setOwnRaw(rt.atoms.intern("resolve"), cap.resolve, propDefault)
		out.setOwnRaw(rt.atoms.intern("reject"), cap.reject, propDefault)
		return Obj(out), nil
	})
}

// arrayToLocaleString joins an array-like by asking each element for its own
// locale form.
//
// A hole or an element that is null or undefined contributes nothing, which is
// what makes the result of [1, , 2] two separators and two numbers.
func (r *Runtime) arrayToLocaleString(this Value, locale []Value) (Value, error) {
	a, err := r.viewArrayLike(this)
	if err != nil {
		return Undefined, err
	}
	return r.arrayLikeToLocaleString(this, a.n, locale)
}

// arrayLikeToLocaleString is arrayToLocaleString over a length settled by the
// caller, which is what a typed array needs: its length is the view's own and
// not a property a script could have defined over it.
func (r *Runtime) arrayLikeToLocaleString(this Value, n int64, locale []Value) (Value, error) {
	o, err := r.toObject(this)
	if err != nil {
		return Undefined, err
	}
	a := &arrayLike{o: o, n: n}
	var sb strings.Builder
	for i := int64(0); i < a.n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		v, err := a.get(r, i)
		if err != nil {
			return Undefined, err
		}
		if v.IsNullish() {
			continue
		}
		fn, err := r.getValueProp(v, r.atoms.intern("toLocaleString"))
		if err != nil {
			return Undefined, err
		}
		if !isCallable(fn) {
			return Undefined, r.throwTypeError("toLocaleString is not a function")
		}
		// The locales and the options are handed on, since each element is
		// asked to write itself the same way as the last. Both are passed
		// whether or not they were given, which a script can see.
		res, err := r.call(fn, v, []Value{arg(locale, 0), arg(locale, 1)})
		if err != nil {
			return Undefined, err
		}
		s, err := r.toString(res)
		if err != nil {
			return Undefined, err
		}
		sb.WriteString(s.Go())
	}
	return Str(NewString(sb.String())), nil
}
