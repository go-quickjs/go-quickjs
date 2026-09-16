package vm

import (
	"encoding/binary"
	"math"
)

// ArrayBuffer, typed arrays and DataView.
//
// An ArrayBuffer is a flat byte slice. A typed array is a view over one with an
// element type, an offset and a length; every element access converts between
// the element type and a JavaScript number, which is where the wrapping and
// clamping rules live.
//
// The views share the buffer's bytes rather than copying, so writing through
// one view is visible through another over the same region. That aliasing is
// the reason typed arrays exist.

// arrayBufferData is an ArrayBuffer's storage.
type arrayBufferData struct {
	bytes []byte
	// detached marks a buffer whose storage has been transferred away. Every
	// access through a view then throws, which is what makes transfer safe.
	detached bool
}

// elemType identifies a typed array's element type.
type elemType uint8

const (
	elemInt8 elemType = iota
	elemUint8
	elemUint8Clamped
	elemInt16
	elemUint16
	elemInt32
	elemUint32
	elemFloat32
	elemFloat64
	elemBigInt64
	elemBigUint64
)

// elemInfo describes one element type.
type elemInfo struct {
	name string
	size int
	// big marks the two types whose elements are BigInts rather than numbers.
	big bool
}

var elemInfos = [...]elemInfo{
	elemInt8:         {"Int8Array", 1, false},
	elemUint8:        {"Uint8Array", 1, false},
	elemUint8Clamped: {"Uint8ClampedArray", 1, false},
	elemInt16:        {"Int16Array", 2, false},
	elemUint16:       {"Uint16Array", 2, false},
	elemInt32:        {"Int32Array", 4, false},
	elemUint32:       {"Uint32Array", 4, false},
	elemFloat32:      {"Float32Array", 4, false},
	elemFloat64:      {"Float64Array", 8, false},
	elemBigInt64:     {"BigInt64Array", 8, true},
	elemBigUint64:    {"BigUint64Array", 8, true},
}

// typedArrayData is a view over a buffer.
type typedArrayData struct {
	buffer *Object
	kind   elemType
	// offset and length are in elements, not bytes.
	byteOffset int
	length     int
}

func (t *typedArrayData) info() elemInfo { return elemInfos[t.kind] }

func (t *typedArrayData) storage() *arrayBufferData {
	b, _ := t.buffer.data.(*arrayBufferData)
	return b
}

// typedArrayOf recovers a view from a receiver.
func (r *Runtime) typedArrayOf(this Value, name string) (*typedArrayData, error) {
	if !this.IsObject() || this.Object().class != ClassTypedArray {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	t, ok := this.Object().data.(*typedArrayData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized typed array", name)
	}
	if t.storage().detached {
		return nil, r.throwTypeError("the underlying ArrayBuffer has been detached")
	}
	return t, nil
}

// getElem reads one element as a JavaScript value.
func (t *typedArrayData) getElem(i int) Value {
	if i < 0 || i >= t.length {
		return Undefined
	}
	b := t.storage().bytes
	off := t.byteOffset + i*t.info().size
	switch t.kind {
	case elemInt8:
		return Int(int(int8(b[off])))
	case elemUint8, elemUint8Clamped:
		return Int(int(b[off]))
	case elemInt16:
		return Int(int(int16(binary.LittleEndian.Uint16(b[off:]))))
	case elemUint16:
		return Int(int(binary.LittleEndian.Uint16(b[off:])))
	case elemInt32:
		return Int32(int32(binary.LittleEndian.Uint32(b[off:])))
	case elemUint32:
		return Uint32(binary.LittleEndian.Uint32(b[off:]))
	case elemFloat32:
		return Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))))
	case elemFloat64:
		return Float(math.Float64frombits(binary.LittleEndian.Uint64(b[off:])))
	case elemBigInt64:
		return Big(NewBigInt(int64(binary.LittleEndian.Uint64(b[off:]))))
	case elemBigUint64:
		v := binary.LittleEndian.Uint64(b[off:])
		bi := &BigInt{}
		bi.V.SetUint64(v)
		return Big(bi)
	}
	return Undefined
}

// setElem writes one element, applying the conversion its type requires.
func (r *Runtime) setElem(t *typedArrayData, i int, v Value) error {
	if i < 0 || i >= t.length {
		// Writing out of range is silently ignored, which is what makes a
		// typed array not grow.
		return nil
	}
	b := t.storage().bytes
	off := t.byteOffset + i*t.info().size

	if t.info().big {
		if !v.IsBigInt() {
			return r.throwTypeError("a BigInt typed array requires a BigInt value")
		}
		binary.LittleEndian.PutUint64(b[off:], bigLowUint64(v.BigInt()))
		return nil
	}

	n, err := r.toNumber(v)
	if err != nil {
		return err
	}
	switch t.kind {
	case elemInt8:
		b[off] = byte(int8(toInt32Wrap(n)))
	case elemUint8:
		b[off] = byte(toInt32Wrap(n))
	case elemUint8Clamped:
		// The clamped type saturates and rounds half to even, unlike every
		// other type, which wraps.
		b[off] = clampUint8(n)
	case elemInt16:
		binary.LittleEndian.PutUint16(b[off:], uint16(toInt32Wrap(n)))
	case elemUint16:
		binary.LittleEndian.PutUint16(b[off:], uint16(toInt32Wrap(n)))
	case elemInt32, elemUint32:
		binary.LittleEndian.PutUint32(b[off:], uint32(toInt32Wrap(n)))
	case elemFloat32:
		binary.LittleEndian.PutUint32(b[off:], math.Float32bits(float32(n)))
	case elemFloat64:
		binary.LittleEndian.PutUint64(b[off:], math.Float64bits(n))
	}
	return nil
}

// toInt32Wrap converts a number to a 32-bit value with the wrapping the
// integer element types use.
func toInt32Wrap(n float64) int32 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0
	}
	return int32(uint32(int64(math.Mod(math.Trunc(n), 4294967296))))
}

// clampUint8 implements the Uint8ClampedArray conversion: saturate to the
// range and round half to even.
func clampUint8(n float64) byte {
	switch {
	case math.IsNaN(n) || n <= 0:
		return 0
	case n >= 255:
		return 255
	}
	f := math.Floor(n)
	if n-f > 0.5 {
		return byte(f + 1)
	}
	if n-f < 0.5 {
		return byte(f)
	}
	// Exactly half rounds to the even neighbour.
	if int64(f)%2 == 0 {
		return byte(f)
	}
	return byte(f + 1)
}

func (r *Runtime) initArrayBufferBuiltins() {
	abProto := newObject(r.proto.object, ClassObject)
	r.arrayBufferProto = abProto

	ctor := r.newCtor("ArrayBuffer", 1, abProto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("ArrayBuffer"); err != nil {
			return Undefined, err
		}
		n, err := rt.toIndex(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if n > 1<<31 {
			return Undefined, rt.throwRangeError("the ArrayBuffer length is too large")
		}
		o := newObject(abProto, ClassArrayBuffer)
		o.data = &arrayBufferData{bytes: make([]byte, n)}
		return Obj(o), nil
	})
	r.defSpecies(ctor)

	r.defMethod(ctor, "isView", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsObject() {
			return False, nil
		}
		c := v.Object().class
		return Bool(c == ClassTypedArray || c == ClassDataView), nil
	})

	r.defGetter(abProto, "byteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.bufferOf(this, "ArrayBuffer.prototype.byteLength")
		if err != nil {
			return Undefined, err
		}
		if b.detached {
			return Int(0), nil
		}
		return Int(len(b.bytes)), nil
	})

	r.defMethod(abProto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.bufferOf(this, "ArrayBuffer.prototype.slice")
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex(arg(args, 0), len(b.bytes), 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), len(b.bytes), len(b.bytes))
		if err != nil {
			return Undefined, err
		}
		if start > end {
			start = end
		}
		o := newObject(abProto, ClassArrayBuffer)
		// slice copies, unlike a typed array view, which aliases.
		o.data = &arrayBufferData{bytes: append([]byte(nil), b.bytes[start:end]...)}
		return Obj(o), nil
	})

	r.defToStringTag(abProto, "ArrayBuffer")
}

func (r *Runtime) bufferOf(this Value, name string) (*arrayBufferData, error) {
	if !this.IsObject() || this.Object().class != ClassArrayBuffer {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	b, ok := this.Object().data.(*arrayBufferData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized ArrayBuffer", name)
	}
	return b, nil
}

func (r *Runtime) initTypedArrayBuiltins() {
	// The shared prototype holds every method; each concrete constructor's
	// prototype inherits from it, which is how %TypedArray% works.
	base := newObject(r.proto.object, ClassObject)
	r.typedArrayProto = base
	r.defineTypedArrayMethods(base)

	// %TypedArray% itself is abstract: it exists to hold the shared methods and
	// to be the prototype of the nine concrete constructors, never to be
	// called.
	abstract := newObject(r.proto.function, ClassFunction)
	abstract.data = &funcData{
		name: "TypedArray", length: 0, ctorKind: ctorBase,
		native: func(rt *Runtime, this Value, args []Value) (Value, error) {
			return Undefined, rt.throwTypeError("TypedArray is abstract and cannot be constructed")
		},
	}
	abstract.setOwnRaw(atomPrototype, Obj(base), 0)
	base.setOwnRaw(atomConstructor, Obj(abstract), propWritable|propConfigurable)
	r.typedArrayCtor = abstract
	r.defSpecies(abstract)
	r.initTypedArrayStatics(abstract)

	for kind := elemInt8; int(kind) < len(elemInfos); kind++ {
		info := elemInfos[kind]
		proto := newObject(base, ClassObject)
		k := kind
		r.typedArrayProtos[kind] = proto

		ctor := r.newCtor(info.name, 3, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if err := rt.requireNew(info.name); err != nil {
				return Undefined, err
			}
			return rt.constructTypedArray(k, proto, args)
		})
		// The concrete constructors inherit the statics from %TypedArray%.
		ctor.proto = abstract
		r.defConst(ctor, "BYTES_PER_ELEMENT", Int(info.size))
		r.defConst(proto, "BYTES_PER_ELEMENT", Int(info.size))

		if kind == elemUint8 {
			// The base64 and hex conversions live only on Uint8Array: they are
			// about bytes, and a view of wider elements would leave the caller
			// reasoning about byte order.
			r.uint8Proto = proto
			r.initBase64Builtins(ctor, proto)
		}
	}
}

// constructTypedArray builds a view from the several argument shapes the
// constructor accepts.
func (r *Runtime) constructTypedArray(kind elemType, proto *Object, args []Value) (Value, error) {
	info := elemInfos[kind]
	o := newObject(proto, ClassTypedArray)

	first := arg(args, 0)
	switch {
	case first.IsObject() && first.Object().class == ClassArrayBuffer:
		// A view over an existing buffer, which aliases rather than copies.
		buf := first.Object()
		b := buf.data.(*arrayBufferData)
		off, err := r.toIndex(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if int(off)%info.size != 0 {
			return Undefined, r.throwRangeError("the byte offset must be a multiple of %d", info.size)
		}
		if int(off) > len(b.bytes) {
			return Undefined, r.throwRangeError("the byte offset is out of range")
		}
		length := (len(b.bytes) - int(off)) / info.size
		if lv := arg(args, 2); !lv.IsUndefined() {
			n, err := r.toIndex(lv)
			if err != nil {
				return Undefined, err
			}
			if int(off)+int(n)*info.size > len(b.bytes) {
				return Undefined, r.throwRangeError("the view extends past the end of the buffer")
			}
			length = int(n)
		}
		o.data = &typedArrayData{buffer: buf, kind: kind, byteOffset: int(off), length: length}
		return Obj(o), nil

	case first.IsObject():
		// From an array-like or an iterable, which copies.
		items, err := r.arrayToSlice(first)
		if err != nil {
			return Undefined, err
		}
		t := r.allocTypedArray(o, kind, len(items))
		for i, v := range items {
			if err := r.setElem(t, i, v); err != nil {
				return Undefined, err
			}
		}
		return Obj(o), nil

	default:
		n, err := r.toIndex(first)
		if err != nil {
			return Undefined, err
		}
		if n > 1<<28 {
			return Undefined, r.throwRangeError("the typed array length is too large")
		}
		r.allocTypedArray(o, kind, int(n))
		return Obj(o), nil
	}
}

// allocTypedArray gives a view its own freshly allocated buffer.
func (r *Runtime) allocTypedArray(o *Object, kind elemType, length int) *typedArrayData {
	buf := newObject(r.arrayBufferProto, ClassArrayBuffer)
	buf.data = &arrayBufferData{bytes: make([]byte, length*elemInfos[kind].size)}
	t := &typedArrayData{buffer: buf, kind: kind, length: length}
	o.data = t
	return t
}

func (r *Runtime) defineTypedArrayMethods(p *Object) {
	r.defGetter(p, "length", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "length")
		if err != nil {
			return Undefined, err
		}
		return Int(t.length), nil
	})
	r.defGetter(p, "byteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "byteLength")
		if err != nil {
			return Undefined, err
		}
		return Int(t.length * t.info().size), nil
	})
	r.defGetter(p, "byteOffset", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "byteOffset")
		if err != nil {
			return Undefined, err
		}
		return Int(t.byteOffset), nil
	})
	r.defGetter(p, "buffer", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "buffer")
		if err != nil {
			return Undefined, err
		}
		return Obj(t.buffer), nil
	})

	r.defMethod(p, "set", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.set")
		if err != nil {
			return Undefined, err
		}
		off, err := rt.toIndex(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		items, err := rt.arrayToSlice(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if int(off)+len(items) > t.length {
			return Undefined, rt.throwRangeError("the source is too long for this typed array")
		}
		for i, v := range items {
			if err := rt.setElem(t, int(off)+i, v); err != nil {
				return Undefined, err
			}
		}
		return Undefined, nil
	})

	r.defMethod(p, "subarray", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.subarray")
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex(arg(args, 0), t.length, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), t.length, t.length)
		if err != nil {
			return Undefined, err
		}
		if start > end {
			start = end
		}
		// subarray shares the buffer, unlike slice, which copies.
		o := newObject(this.Object().proto, ClassTypedArray)
		o.data = &typedArrayData{
			buffer:     t.buffer,
			kind:       t.kind,
			byteOffset: t.byteOffset + start*t.info().size,
			length:     end - start,
		}
		return Obj(o), nil
	})

	r.defMethod(p, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.slice")
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex(arg(args, 0), t.length, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), t.length, t.length)
		if err != nil {
			return Undefined, err
		}
		if start > end {
			start = end
		}
		o := newObject(this.Object().proto, ClassTypedArray)
		nt := rt.allocTypedArray(o, t.kind, end-start)
		for i := 0; i < end-start; i++ {
			if err := rt.setElem(nt, i, t.getElem(start+i)); err != nil {
				return Undefined, err
			}
		}
		return Obj(o), nil
	})

	r.defMethod(p, "fill", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.fill")
		if err != nil {
			return Undefined, err
		}
		start, err := rt.relativeIndex(arg(args, 1), t.length, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 2), t.length, t.length)
		if err != nil {
			return Undefined, err
		}
		for i := start; i < end; i++ {
			if err := rt.setElem(t, i, arg(args, 0)); err != nil {
				return Undefined, err
			}
		}
		return this, nil
	})

	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.indexOf")
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		for i := 0; i < t.length; i++ {
			if t.getElem(i).StrictEquals(target) {
				return Int(i), nil
			}
		}
		return Int(-1), nil
	})

	r.defMethod(p, "includes", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.includes")
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		for i := 0; i < t.length; i++ {
			if t.getElem(i).SameValueZero(target) {
				return True, nil
			}
		}
		return False, nil
	})

	r.defMethod(p, "join", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.join")
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
		out := emptyString
		for i := 0; i < t.length; i++ {
			if i > 0 {
				out = out.Concat(NewString(sep))
			}
			s, err := rt.toString(t.getElem(i))
			if err != nil {
				return Undefined, err
			}
			out = out.Concat(s)
		}
		return Str(out), nil
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		fn, err := rt.getValueProp(this, rt.atoms.intern("join"))
		if err != nil {
			return Undefined, err
		}
		return rt.call(fn, this, nil)
	})

	// The callback-taking methods.
	// Every method that walks the elements is served the same way: the
	// elements are read into a plain array and the ordinary Array method
	// reused, which keeps one implementation of each rather than eleven.
	//
	// The ones that build a new collection then convert the result back, so
	// that a Uint8Array's map yields a Uint8Array rather than a plain array.
	for _, name := range []string{
		"forEach", "map", "filter", "find", "findIndex", "findLast",
		"findLastIndex", "some", "every", "reduce", "reduceRight",
	} {
		method := name
		r.defMethod(p, method, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.typedArrayOf(this, "TypedArray.prototype."+method)
			if err != nil {
				return Undefined, err
			}
			cb := arg(args, 0)
			if !isCallable(cb) {
				return Undefined, rt.throwTypeError("%s requires a function", method)
			}
			// The elements are read into a plain array and the ordinary array
			// method reused, which keeps one implementation of each.
			vals := make([]Value, t.length)
			for i := range vals {
				vals[i] = t.getElem(i)
			}
			arr := Obj(rt.newArrayFrom(vals))
			fn, err := rt.getValueProp(arr, rt.atoms.intern(method))
			if err != nil {
				return Undefined, err
			}
			out, err := rt.call(fn, arr, args)
			if err != nil {
				return Undefined, err
			}
			switch method {
			case "map", "filter":
				return rt.typedArrayFromValues(t.kind, out)
			}
			return out, nil
		})
	}

	// The methods that take no callback, delegated the same way. Those that
	// mutate copy the result back; those that build a new collection convert
	// it to a view of the same kind.
	type delegated struct {
		name    string
		length  int
		mutates bool
		rebuild bool
	}
	for _, m := range []delegated{
		{"at", 1, false, false},
		{"indexOf", 1, false, false},
		{"lastIndexOf", 1, false, false},
		{"includes", 1, false, false},
		{"join", 1, false, false},
		{"reverse", 0, true, false},
		{"sort", 1, true, false},
		{"copyWithin", 2, true, false},
		{"toReversed", 0, false, true},
		{"toSorted", 1, false, true},
		{"with", 2, false, true},
		{"entries", 0, false, false},
		{"keys", 0, false, false},
		{"values", 0, false, false},
	} {
		d := m
		r.defMethod(p, d.name, d.length, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.typedArrayOf(this, "TypedArray.prototype."+d.name)
			if err != nil {
				return Undefined, err
			}
			vals := make([]Value, t.length)
			for i := range vals {
				vals[i] = t.getElem(i)
			}
			arr := rt.newArrayFrom(vals)
			fn, err := rt.getValueProp(Obj(arr), rt.atoms.intern(d.name))
			if err != nil {
				return Undefined, err
			}
			callArgs := args
			if (d.name == "sort" || d.name == "toSorted") && !isCallable(arg(args, 0)) {
				// A typed array sorts numerically by default, where an ordinary
				// array sorts by string: [10, 9] is [9, 10] here and [10, 9]
				// there.
				callArgs = []Value{Obj(rt.newNativeFunc("", 2,
					func(rt *Runtime, _ Value, a []Value) (Value, error) {
						x, err := rt.toNumber(arg(a, 0))
						if err != nil {
							return Undefined, err
						}
						y, err := rt.toNumber(arg(a, 1))
						if err != nil {
							return Undefined, err
						}
						switch {
						case x < y:
							return Int(-1), nil
						case x > y:
							return Int(1), nil
						}
						return Int(0), nil
					}))}
			}
			out, err := rt.call(fn, Obj(arr), callArgs)
			if err != nil {
				return Undefined, err
			}
			switch {
			case d.mutates:
				// The plain array was reordered in place; the view has to be
				// written back element by element, through the element type's
				// own conversion.
				for i := 0; i < t.length && i < len(arr.elems); i++ {
					if err := rt.setElem(t, i, arr.elems[i]); err != nil {
						return Undefined, err
					}
				}
				return this, nil
			case d.rebuild:
				return rt.typedArrayFromValues(t.kind, out)
			}
			return out, nil
		})
	}

	// The tag names the concrete type, so Object.prototype.toString reports
	// [object Uint8Array] rather than [object Object]. It is a getter on the
	// shared prototype because the nine types share it.
	tag := r.newNativeFunc("get [Symbol.toStringTag]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			if !this.IsObject() || this.Object().class != ClassTypedArray {
				return Undefined, nil
			}
			t, ok := this.Object().data.(*typedArrayData)
			if !ok {
				return Undefined, nil
			}
			return Str(NewString(t.info().name)), nil
		})
	r.defineAccessor(p, r.atoms.internSymbol(r.wellKnown.toStringTag), tag, nil, propConfigurable)

	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.typedArrayOf(this, "TypedArray.prototype[Symbol.iterator]")
			if err != nil {
				return Undefined, err
			}
			vals := make([]Value, t.length)
			for i := range vals {
				vals[i] = t.getElem(i)
			}
			return rt.newArrayIterator(Obj(rt.newArrayFrom(vals)))
		})
}

// typedArrayFromValues builds a view of the given kind holding the values of an
// array.
func (r *Runtime) typedArrayFromValues(kind elemType, v Value) (Value, error) {
	var vals []Value
	if v.IsObject() {
		vals = v.Object().elems
	}
	o := newObject(r.typedArrayProtoFor(kind), ClassTypedArray)
	t := r.allocTypedArray(o, kind, len(vals))
	for i, el := range vals {
		if err := r.setElem(t, i, el); err != nil {
			return Undefined, err
		}
	}
	return Obj(o), nil
}

// typedArrayProtoFor returns the prototype a view of the given element type
// should have.
func (r *Runtime) typedArrayProtoFor(kind elemType) *Object {
	if p := r.typedArrayProtos[kind]; p != nil {
		return p
	}
	return r.typedArrayProto
}

// initTypedArrayStatics defines from and of on %TypedArray%, which the concrete
// constructors inherit.
func (r *Runtime) initTypedArrayStatics(abstract *Object) {
	r.defMethod(abstract, "of", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		kind, err := rt.typedArrayKindOf(this, "of")
		if err != nil {
			return Undefined, err
		}
		return rt.typedArrayFromValues(kind, Obj(rt.newArrayFrom(args)))
	})

	r.defMethod(abstract, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		kind, err := rt.typedArrayKindOf(this, "from")
		if err != nil {
			return Undefined, err
		}
		// The source is collected with Array.from, so an iterable, an
		// array-like and the mapping function all behave identically here.
		from, err := rt.getValueProp(rt.global.getOwn(rt.atoms.intern("Array")).value,
			rt.atoms.intern("from"))
		if err != nil {
			return Undefined, err
		}
		arr, err := rt.call(from, Undefined, args)
		if err != nil {
			return Undefined, err
		}
		return rt.typedArrayFromValues(kind, arr)
	})
}

// typedArrayKindOf recovers the element type a static was reached through.
func (r *Runtime) typedArrayKindOf(this Value, name string) (elemType, error) {
	if this.IsObject() {
		for kind := elemInt8; int(kind) < len(elemInfos); kind++ {
			if p := r.typedArrayProtos[kind]; p != nil {
				if c := p.getOwn(atomConstructor); c != nil && c.value.SameValue(this) {
					return kind, nil
				}
			}
		}
	}
	return 0, r.throwTypeError("%%TypedArray%%.%s requires a typed array constructor", name)
}
