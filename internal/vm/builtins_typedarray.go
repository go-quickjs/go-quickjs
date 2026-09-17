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
	elemFloat16
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
	elemFloat16:      {"Float16Array", 2, false},
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

// count is how many elements are actually there.
//
// It is not t.length: the buffer can go away underneath a view at any point --
// a valueOf called while a method is running is enough -- and every read and
// write has to be measured against what is there now rather than against what
// was there when the view was made. Answering zero is what turns a detached
// buffer into an out-of-range access instead of a crash.
// searchStart converts a forward search's starting point, reporting whether
// there is any element left to look at.
//
// The length is read before the conversion, which is what makes an empty array
// answer without running a valueOf at all.
func (r *Runtime) searchStart(v Value, length int) (int, bool, error) {
	if length == 0 {
		return 0, false, nil
	}
	if v.IsUndefined() {
		return 0, true, nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return 0, false, err
	}
	switch {
	case math.IsInf(n, 1):
		return 0, false, nil
	case math.IsInf(n, -1):
		return 0, true, nil
	case n < 0:
		n += float64(length)
		if n < 0 {
			return 0, true, nil
		}
	case n >= float64(length):
		return 0, false, nil
	}
	return int(n), true, nil
}

func (t *typedArrayData) count() int {
	b := t.storage()
	if b == nil || b.detached {
		return 0
	}
	n := (len(b.bytes) - t.byteOffset) / t.info().size
	if n > t.length {
		n = t.length
	}
	if n < 0 {
		return 0
	}
	return n
}

// typedArrayDataOf recovers a view from a receiver without insisting that its
// buffer is still there, which the size getters need: a detached view is empty
// rather than an error.
func (r *Runtime) typedArrayDataOf(this Value, name string) (*typedArrayData, error) {
	if !this.IsObject() || this.Object().class != ClassTypedArray {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	t, ok := this.Object().data.(*typedArrayData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized typed array", name)
	}
	return t, nil
}

// typedArrayOf recovers a view from a receiver.
func (r *Runtime) typedArrayOf(this Value, name string) (*typedArrayData, error) {
	t, err := r.typedArraySlot(this, name)
	if err != nil {
		return nil, err
	}
	if t.storage().detached {
		return nil, r.throwTypeError("the underlying ArrayBuffer has been detached")
	}
	return t, nil
}

// typedArraySlot is typedArrayOf without the detachment check, for the few
// methods that convert an argument first and only then look at the buffer.
func (r *Runtime) typedArraySlot(this Value, name string) (*typedArrayData, error) {
	if !this.IsObject() || this.Object().class != ClassTypedArray {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	t, ok := this.Object().data.(*typedArrayData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized typed array", name)
	}
	return t, nil
}

// getElem reads one element as a JavaScript value.
func (t *typedArrayData) getElem(i int) Value {
	if i < 0 || i >= t.count() {
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
	case elemFloat16:
		return Float(float16frombits(binary.LittleEndian.Uint16(b[off:])))
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
	// The conversion comes first and happens whether or not the write lands:
	// a valueOf can detach the buffer, and the specification orders the
	// coercion before the bounds check precisely so that it still runs.
	var (
		n  float64
		bv *BigInt
	)
	if t.info().big {
		// ToBigInt, not a type check: a string or a boolean converts, and only
		// a Number is refused -- mixing the two kinds is almost always a
		// mistake rather than a request to convert.
		var err error
		if bv, err = r.toBigIntOperand(v); err != nil {
			return err
		}
	} else {
		var err error
		if n, err = r.toNumber(v); err != nil {
			return err
		}
	}

	// Re-measured after the conversion, because the conversion may have taken
	// the buffer away.
	if i < 0 || i >= t.count() {
		// Writing out of range is silently ignored, which is what makes a
		// typed array not grow.
		return nil
	}
	b := t.storage().bytes
	off := t.byteOffset + i*t.info().size

	if t.info().big {
		binary.LittleEndian.PutUint64(b[off:], bigLowUint64(bv))
		return nil
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
	case elemFloat16:
		binary.LittleEndian.PutUint16(b[off:], float16bits(n))
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
		// The object exists before its storage does, so a prototype getter
		// that throws is reported rather than an allocation failure.
		proto, err := rt.protoFromNewTargetErr(abProto)
		if err != nil {
			return Undefined, err
		}
		if n > 1<<31 {
			return Undefined, rt.throwRangeError("the ArrayBuffer length is too large")
		}
		o := newObject(proto, ClassArrayBuffer)
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

	// detached is how a script tells a transferred buffer from a live one
	// without provoking a TypeError by touching it.
	r.defGetter(abProto, "detached", func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.bufferOf(this, "ArrayBuffer.prototype.detached")
		if err != nil {
			return Undefined, err
		}
		return Bool(b.detached), nil
	})

	// transfer hands the storage to a new buffer and detaches this one, which
	// is what makes passing a large buffer around cost nothing: there is only
	// ever one owner, so nothing has to be copied and nothing can be read
	// through a stale view.
	transfer := func(rt *Runtime, this Value, args []Value, name string) (Value, error) {
		b, err := rt.bufferOf(this, name)
		if err != nil {
			return Undefined, err
		}
		if b.detached {
			return Undefined, rt.throwTypeError("the ArrayBuffer has already been detached")
		}
		n := int64(len(b.bytes))
		if lv := arg(args, 0); !lv.IsUndefined() {
			if n, err = rt.toIndex(lv); err != nil {
				return Undefined, err
			}
			if n > 1<<31 {
				return Undefined, rt.throwRangeError("the ArrayBuffer length is too large")
			}
		}
		// A longer target is zero-filled; a shorter one drops the tail.
		out := make([]byte, n)
		copy(out, b.bytes)
		b.detached, b.bytes = true, nil

		o := newObject(abProto, ClassArrayBuffer)
		o.data = &arrayBufferData{bytes: out}
		return Obj(o), nil
	}
	r.defMethod(abProto, "transfer", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return transfer(rt, this, args, "ArrayBuffer.prototype.transfer")
	})
	r.defMethod(abProto, "transferToFixedLength", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			// The two differ only for a resizable buffer, which this engine
			// does not have, so they are the same operation here.
			return transfer(rt, this, args, "ArrayBuffer.prototype.transferToFixedLength")
		})

	r.arrayBufferCtor = ctor

	r.defMethod(abProto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.bufferOf(this, "ArrayBuffer.prototype.slice")
		if err != nil {
			return Undefined, err
		}
		if b.detached {
			return Undefined, rt.throwTypeError("the ArrayBuffer has been detached")
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
		// A subclass decides what its slice returns, which is what the species
		// protocol is for. Everything it hands back is checked, because the
		// copy is written into it.
		ctor, err := rt.speciesConstructor(this.Object(), rt.arrayBufferCtor)
		if err != nil {
			return Undefined, err
		}
		res, err := rt.construct(ctor, []Value{Int(end - start)})
		if err != nil {
			return Undefined, err
		}
		if !res.IsObject() || res.Object().class != ClassArrayBuffer {
			return Undefined, rt.throwTypeError("the species did not return an ArrayBuffer")
		}
		out, ok := res.Object().data.(*arrayBufferData)
		if !ok || out.detached {
			return Undefined, rt.throwTypeError("the species returned a detached ArrayBuffer")
		}
		if res.Object() == this.Object() {
			return Undefined, rt.throwTypeError("the species returned the buffer being sliced")
		}
		if len(out.bytes) < end-start {
			return Undefined, rt.throwTypeError("the species returned too small a buffer")
		}
		// Running the constructor may have detached the source, in which case
		// there is nothing left to copy.
		if !b.detached {
			copy(out.bytes, b.bytes[start:end])
		}
		return res, nil
	})

	r.defToStringTag(abProto, "ArrayBuffer")
}

// DetachArrayBuffer releases a buffer's storage, as a transfer does.
//
// Every view over it then throws, which is what makes handing a buffer to
// another owner safe: the original holder cannot keep reading through a view it
// made earlier.
func (r *Runtime) DetachArrayBuffer(v Value) error {
	if !v.IsObject() || v.Object().class != ClassArrayBuffer {
		return r.throwTypeError("an ArrayBuffer is required")
	}
	b, ok := v.Object().data.(*arrayBufferData)
	if !ok {
		return r.throwTypeError("the ArrayBuffer is uninitialized")
	}
	b.detached = true
	b.bytes = nil
	return nil
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
			// A subclass's instances get its prototype, which new.target
			// names -- unless it says something that is not an object, where
			// the intrinsic one stands in.
			p := proto
			if nt := rt.newTarget(); nt.IsObject() {
				custom, err := rt.getValueProp(nt, atomPrototype)
				if err != nil {
					return Undefined, err
				}
				if custom.IsObject() {
					p = custom.Object()
				}
			}
			return rt.constructTypedArray(k, p, args)
		})
		r.typedArrayCtors[kind] = ctor
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
		var explicit int64 = -1
		if lv := arg(args, 2); !lv.IsUndefined() {
			n, err := r.toIndex(lv)
			if err != nil {
				return Undefined, err
			}
			explicit = int64(n)
		}
		// Both arguments are coerced before the buffer is looked at: either
		// coercion can run a valueOf that detaches it, and a detached buffer is
		// what the view would have been over.
		if b.detached {
			return Undefined, r.throwTypeError("the buffer is detached")
		}
		if int(off) > len(b.bytes) {
			return Undefined, r.throwRangeError("the byte offset is out of range")
		}
		length := (len(b.bytes) - int(off)) / info.size
		if explicit >= 0 {
			if int64(off)+explicit*int64(info.size) > int64(len(b.bytes)) {
				return Undefined, r.throwRangeError("the view extends past the end of the buffer")
			}
			length = int(explicit)
		}
		o.data = &typedArrayData{buffer: buf, kind: kind, byteOffset: int(off), length: length}
		return Obj(o), nil

	case first.IsObject() && first.Object().class == ClassTypedArray:
		// From another view, element by element, so the conversion between the
		// two element types happens once per value rather than by
		// reinterpreting the bytes.
		src, err := r.typedArrayOf(first, "the TypedArray constructor")
		if err != nil {
			return Undefined, err
		}
		t := r.allocTypedArray(o, kind, src.length)
		for i := 0; i < src.length; i++ {
			if err := r.setElem(t, i, src.getElem(i)); err != nil {
				return Undefined, err
			}
		}
		return Obj(o), nil

	case first.IsObject():
		// From an iterable when it has one, and from the array-like protocol
		// otherwise -- the same order Array.from uses.
		var items []Value
		method, err := r.getValueProp(first, r.atoms.internSymbol(r.wellKnown.iterator))
		if err != nil {
			return Undefined, err
		}
		if isCallable(method) {
			if err := r.iterate(first, func(v Value) error {
				// The iterable is not asked how long it is, so the refusal has
				// to come from the count: collecting more than could ever be
				// allocated is how a four-billion-element source takes the
				// process down instead of throwing.
				if int64(len(items)) >= maxTypedArrayLength {
					return r.throwRangeError("the typed array length is too large")
				}
				items = append(items, v)
				return nil
			}); err != nil {
				return Undefined, err
			}
		} else {
			// The array-like path allocates from the reported length before it
			// reads anything, which is what stops {length: 2**32} from being
			// walked -- and what makes the refusal a RangeError rather than an
			// exhausted machine.
			src, err := r.viewArrayLike(first)
			if err != nil {
				return Undefined, err
			}
			t, err := r.allocTypedArrayChecked(o, kind, src.n)
			if err != nil {
				return Undefined, err
			}
			for i := int64(0); i < src.n; i++ {
				v, err := src.get(r, i)
				if err != nil {
					return Undefined, err
				}
				if err := r.setElem(t, int(i), v); err != nil {
					return Undefined, err
				}
			}
			return Obj(o), nil
		}
		t, err := r.allocTypedArrayChecked(o, kind, int64(len(items)))
		if err != nil {
			return Undefined, err
		}
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
		if _, err := r.allocTypedArrayChecked(o, kind, int64(n)); err != nil {
			return Undefined, err
		}
		return Obj(o), nil
	}
}

// maxTypedArrayLength bounds how many elements a view may have.
//
// The specification allows up to 2**53-1, which no machine can hold. The point
// of the bound is that an over-large request is refused before anything is
// allocated, so a script asking for one gets a RangeError rather than taking
// the process down with it.
const maxTypedArrayLength = 1 << 28

// allocTypedArrayChecked allocates a view's buffer, refusing a length that
// could not be held.
func (r *Runtime) allocTypedArrayChecked(o *Object, kind elemType, n int64) (*typedArrayData, error) {
	if n < 0 || n > maxTypedArrayLength ||
		n*int64(elemInfos[kind].size) > maxTypedArrayLength*8 {
		return nil, r.throwRangeError("the typed array length is too large")
	}
	return r.allocTypedArray(o, kind, int(n)), nil
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
	// The three size getters answer zero for a detached view rather than
	// throwing. A view over a buffer that has gone is empty, not broken, and a
	// script asking how long it is deserves that answer.
	r.defGetter(p, "length", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayDataOf(this, "length")
		if err != nil {
			return Undefined, err
		}
		return Int(t.count()), nil
	})
	r.defGetter(p, "byteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayDataOf(this, "byteLength")
		if err != nil {
			return Undefined, err
		}
		return Int(t.count() * t.info().size), nil
	})
	r.defGetter(p, "byteOffset", func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayDataOf(this, "byteOffset")
		if err != nil {
			return Undefined, err
		}
		if t.count() == 0 && t.storage().detached {
			return Int(0), nil
		}
		return Int(t.byteOffset), nil
	})
	r.defGetter(p, "buffer", func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The buffer is reported whether or not it is still attached: it is
		// the object the view was made over, and asking which one that was is
		// how a caller finds out that it has gone.
		t, err := rt.typedArraySlot(this, "buffer")
		if err != nil {
			return Undefined, err
		}
		return Obj(t.buffer), nil
	})

	r.defMethod(p, "set", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The receiver only has to be a typed array to begin with: converting
		// the offset runs user code, which may detach the buffer, and that is
		// checked afterwards rather than before.
		t, err := rt.typedArraySlot(this, "TypedArray.prototype.set")
		if err != nil {
			return Undefined, err
		}
		off, err := rt.toIndex(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if t.storage().detached {
			return Undefined, rt.throwTypeError("the underlying ArrayBuffer has been detached")
		}
		src := arg(args, 0)
		if src.IsObject() && src.Object().class == ClassTypedArray {
			// The source is looked at only now, for the same reason: coercing
			// the offset may have detached it.
			if _, err := rt.typedArrayOf(src, "TypedArray.prototype.set"); err != nil {
				return Undefined, err
			}
			// A typed array source may share the buffer, so every element is
			// read before any is written: interleaving would let an early
			// write change a later read.
			items, err := rt.arrayToSlice(src)
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
		}

		a, err := rt.viewArrayLike(src)
		if err != nil {
			return Undefined, err
		}
		if off+a.n > int64(t.length) {
			return Undefined, rt.throwRangeError("the source is too long for this typed array")
		}
		// Each element is read and written before the next is read, so a getter
		// on the source sees the writes that preceded it -- and a getter that
		// throws leaves the earlier ones in place.
		for i := int64(0); i < a.n; i++ {
			v, err := a.get(rt, i)
			if err != nil {
				return Undefined, err
			}
			if err := rt.setElem(t, int(off+i), v); err != nil {
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
		// subarray shares the buffer, unlike slice, which copies. It is built
		// through the species from that buffer rather than assembled here, so
		// that a subclass gets an instance of itself over the same bytes.
		res, _, err := rt.typedArraySpeciesCreate(this, t, []Value{
			Obj(t.buffer),
			Int(t.byteOffset + start*t.info().size),
			Int(end - start),
		})
		return res, err
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
		res, nt, err := rt.newTypedArrayLike(this, t, end-start)
		if err != nil {
			return Undefined, err
		}
		if end == start {
			// Nothing to copy, so the source is never looked at again -- which
			// is why detaching it while the species ran is not an error here.
			return res, nil
		}
		// Building the result ran user code, which may have detached the
		// source out from under the copy -- and the result has to be able to
		// hold what the source holds, which a BigInt array and a Number one
		// cannot do for each other.
		if t.storage().detached {
			return Undefined, rt.throwTypeError("the underlying ArrayBuffer has been detached")
		}
		if elemInfos[nt.kind].big != elemInfos[t.kind].big {
			return Undefined, rt.throwTypeError(
				"a BigInt typed array and a Number one cannot stand in for each other")
		}
		for i := 0; i < end-start && i < nt.length; i++ {
			if err := rt.setElem(nt, i, t.getElem(start+i)); err != nil {
				return Undefined, err
			}
		}
		return res, nil
	})

	r.defMethod(p, "fill", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.fill")
		if err != nil {
			return Undefined, err
		}
		// The value is converted once, before the range is worked out: a
		// valueOf that counts its calls must see exactly one, however many
		// elements are filled.
		v, err := rt.toElementValue(t, arg(args, 0))
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
		// Any of those coercions can run a valueOf that detaches the buffer,
		// so the view is checked again before anything is written.
		if t.storage().detached {
			return Undefined, rt.throwTypeError("the underlying ArrayBuffer has been detached")
		}
		for i := start; i < end; i++ {
			if err := rt.setElem(t, i, v); err != nil {
				return Undefined, err
			}
		}
		return this, nil
	})

	// The searches read their elements after the starting point has been
	// converted, which a valueOf that detaches the buffer can tell: the length
	// was settled first, but every read past the detachment is undefined.
	//
	// searchStart is where a forward search begins, or reports that there is
	// nowhere to start from.
	r.defMethod(p, "indexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.indexOf")
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		from, ok, err := rt.searchStart(arg(args, 1), t.length)
		if err != nil || !ok {
			return Int(-1), err
		}
		// indexOf asks whether each index is there before reading it, and a
		// detached buffer leaves none of them: it reports nothing found rather
		// than finding undefined everywhere, which is where it parts company
		// with includes.
		avail := t.count()
		for i := from; i < t.length && i < avail; i++ {
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
		from, ok, err := rt.searchStart(arg(args, 1), t.length)
		if err != nil || !ok {
			return False, err
		}
		for i := from; i < t.length; i++ {
			if t.getElem(i).SameValueZero(target) {
				return True, nil
			}
		}
		return False, nil
	})

	r.defMethod(p, "copyWithin", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.copyWithin")
		if err != nil {
			return Undefined, err
		}
		to, err := rt.relativeIndex(arg(args, 0), t.length, 0)
		if err != nil {
			return Undefined, err
		}
		from, err := rt.relativeIndex(arg(args, 1), t.length, 0)
		if err != nil {
			return Undefined, err
		}
		final, err := rt.relativeIndex(arg(args, 2), t.length, t.length)
		if err != nil {
			return Undefined, err
		}
		count := final - from
		if n := t.length - to; n < count {
			count = n
		}
		if count > 0 {
			// Any of those conversions can detach the buffer, and a view over
			// one that has gone has nothing to copy within. There is nothing
			// to complain about when there was nothing to copy.
			if t.storage().detached {
				return Undefined, rt.throwTypeError(
					"the underlying ArrayBuffer has been detached")
			}
			size := t.info().size
			b := t.storage().bytes
			dst := t.byteOffset + to*size
			src := t.byteOffset + from*size
			copy(b[dst:dst+count*size], b[src:src+count*size])
		}
		return this, nil
	})

	r.defMethod(p, "lastIndexOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.typedArrayOf(this, "TypedArray.prototype.lastIndexOf")
		if err != nil {
			return Undefined, err
		}
		target := arg(args, 0)
		from := t.length - 1
		if t.length == 0 {
			return Int(-1), nil
		}
		if len(args) > 1 {
			n, err := rt.toInteger(args[1])
			if err != nil {
				return Undefined, err
			}
			switch {
			case math.IsInf(n, -1):
				return Int(-1), nil
			case n < 0:
				n += float64(t.length)
				if n < 0 {
					return Int(-1), nil
				}
				from = int(n)
			case n < float64(from):
				from = int(n)
			}
		}
		if avail := t.count(); from >= avail {
			from = avail - 1
		}
		for i := from; i >= 0; i-- {
			if t.getElem(i).StrictEquals(target) {
				return Int(i), nil
			}
		}
		return Int(-1), nil
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

	r.defMethod(p, "toLocaleString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.typedArrayOf(this, "TypedArray.prototype.toLocaleString"); err != nil {
			return Undefined, err
		}
		return rt.arrayToLocaleString(this)
	})

	r.defMethod(p, "toString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		fn, err := rt.getValueProp(this, rt.atoms.intern("join"))
		if err != nil {
			return Undefined, err
		}
		return rt.call(fn, this, nil)
	})

	// The callback-taking methods.
	// The methods that take a callback are written against the view directly
	// rather than delegated to the Array versions. The callback receives the
	// view as its third argument and, with no thisArg, as nothing else -- a
	// temporary array standing in for it would be observable, and is what
	// several hundred tests check.
	//
	// The length is read once, as everywhere else: an element the callback
	// writes is seen, but the view cannot grow underneath the loop.
	type callbackMethod struct {
		name   string
		length int
	}
	for _, m := range []callbackMethod{
		{"forEach", 1}, {"map", 1}, {"filter", 1}, {"find", 1}, {"findIndex", 1},
		{"findLast", 1}, {"findLastIndex", 1}, {"some", 1}, {"every", 1},
	} {
		method := m.name
		r.defMethod(p, method, m.length, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.typedArrayOf(this, "TypedArray.prototype."+method)
			if err != nil {
				return Undefined, err
			}
			cb := arg(args, 0)
			if !isCallable(cb) {
				return Undefined, rt.throwTypeError("%s requires a function", method)
			}
			thisArg := arg(args, 1)
			n := t.length

			var kept []Value
			out := make([]Value, 0, n)
			backwards := method == "findLast" || method == "findLastIndex"
			for k := 0; k < n; k++ {
				i := k
				if backwards {
					i = n - 1 - k
				}
				el := t.getElem(i)
				res, err := rt.call(cb, thisArg, []Value{el, Int(i), this})
				if err != nil {
					return Undefined, err
				}
				switch method {
				case "map":
					out = append(out, res)
				case "filter":
					if res.Truthy() {
						kept = append(kept, el)
					}
				case "find", "findLast":
					if res.Truthy() {
						return el, nil
					}
				case "findIndex", "findLastIndex":
					if res.Truthy() {
						return Int(i), nil
					}
				case "some":
					if res.Truthy() {
						return True, nil
					}
				case "every":
					if !res.Truthy() {
						return False, nil
					}
				}
			}
			switch method {
			case "map":
				return rt.fillTypedArrayLike(this, t, out)
			case "filter":
				return rt.fillTypedArrayLike(this, t, kept)
			case "some":
				return False, nil
			case "every":
				return True, nil
			case "findIndex", "findLastIndex":
				return Int(-1), nil
			}
			return Undefined, nil
		})
	}

	for _, backwards := range []bool{false, true} {
		reversed := backwards
		name := "reduce"
		if reversed {
			name = "reduceRight"
		}
		r.defMethod(p, name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, err := rt.typedArrayOf(this, "TypedArray.prototype."+name)
			if err != nil {
				return Undefined, err
			}
			cb := arg(args, 0)
			if !isCallable(cb) {
				return Undefined, rt.throwTypeError("%s requires a function", name)
			}
			n := t.length
			k := 0
			var acc Value
			seeded := len(args) > 1
			if seeded {
				acc = args[1]
			} else {
				if n == 0 {
					return Undefined, rt.throwTypeError(
						"%s of an empty typed array with no initial value", name)
				}
				i := 0
				if reversed {
					i = n - 1
				}
				acc, k = t.getElem(i), 1
			}
			for ; k < n; k++ {
				i := k
				if reversed {
					i = n - 1 - k
				}
				acc, err = rt.call(cb, Undefined, []Value{acc, t.getElem(i), Int(i), this})
				if err != nil {
					return Undefined, err
				}
			}
			return acc, nil
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
		{"reverse", 0, true, false},
		{"sort", 1, true, false},
		{"toReversed", 0, false, true},
		{"toSorted", 1, false, true},
		{"with", 2, false, true},
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
						return Int(compareNumeric(arg(a, 0), arg(a, 1))), nil
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
				// A fresh view of the same kind holding the reordered values,
				// which is what toSorted and toReversed produce.
				var vals []Value
				if out.IsObject() {
					vals = out.Object().elems
				}
				o := newObject(rt.typedArrayProtoFor(t.kind), ClassTypedArray)
				nt := rt.allocTypedArray(o, t.kind, len(vals))
				for i, el := range vals {
					if err := rt.setElem(nt, i, el); err != nil {
						return Undefined, err
					}
				}
				return Obj(o), nil
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

	// entries, keys and values read the view as they go rather than copying
	// it, so that a typed array written to while it is iterated is seen
	// changing -- which is the whole point of a view over a buffer.
	for _, m := range []struct {
		name string
		kind arrayIterKind
	}{
		{"entries", iterEntries},
		{"keys", iterKeys},
		{"values", iterValues},
	} {
		kind := m.kind
		name := m.name
		r.defMethod(p, name, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if _, err := rt.typedArrayOf(this, "TypedArray.prototype."+name); err != nil {
				return Undefined, err
			}
			return rt.newArrayIteratorKind(this, kind)
		})
	}

	// Symbol.iterator is the values method itself rather than another function
	// that does the same thing, which a script can tell by comparing them.
	values, err := r.getProp(p, r.atoms.intern("values"), Obj(p))
	if err == nil && values.IsObject() {
		p.setOwnRaw(r.atoms.internSymbol(r.wellKnown.iterator), values,
			propWritable|propConfigurable)
	}
}

// newTypedArrayOf builds a view of the given kind holding the given values.
func (r *Runtime) newTypedArrayOf(kind elemType, vals []Value) (Value, error) {
	o := newObject(r.typedArrayProtoFor(kind), ClassTypedArray)
	t := r.allocTypedArray(o, kind, len(vals))
	for i, el := range vals {
		if err := r.setElem(t, i, el); err != nil {
			return Undefined, err
		}
	}
	return Obj(o), nil
}

// typedArrayFromValues builds a view of the given kind holding the values of an
// array.
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
		return rt.typedArrayFromValues(this, args)
	})

	r.defMethod(abstract, "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !isConstructor(this) {
			return Undefined, rt.throwTypeError(
				"%%TypedArray%%.from requires a constructor receiver")
		}
		mapFn := arg(args, 1)
		if !mapFn.IsUndefined() && !isCallable(mapFn) {
			return Undefined, rt.throwTypeError("the map function is not callable")
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
		var vals []Value
		if arr.IsObject() {
			vals = arr.Object().elems
		}
		return rt.typedArrayFromValues(this, vals)
	})
}

// typedArrayFromValues builds a view of the given length with the constructor
// it was asked for, and fills it.
//
// The constructor is the receiver, so a subclass gets one of its own -- and
// whatever it returns has to be a typed array long enough to hold them.
func (r *Runtime) typedArrayFromValues(ctor Value, vals []Value) (Value, error) {
	if !isConstructor(ctor) {
		return Undefined, r.throwTypeError("a typed array constructor is required")
	}
	res, err := r.construct(ctor, []Value{Int(len(vals))})
	if err != nil {
		return Undefined, err
	}
	t, err := r.typedArrayOf(res, "the result")
	if err != nil {
		return Undefined, err
	}
	if t.length < len(vals) {
		return Undefined, r.throwTypeError("the result is too short")
	}
	for i, el := range vals {
		if err := r.setElem(t, i, el); err != nil {
			return Undefined, err
		}
	}
	return res, nil
}

// typedArrayKindOf recovers the element type a static was reached through.
func (r *Runtime) typedArrayKindOf(this Value, name string) (elemType, error) {
	if this.IsObject() {
		for kind := elemInt8; int(kind) < len(elemInfos); kind++ {
			if c := r.typedArrayCtors[kind]; c != nil && c == this.Object() {
				return kind, nil
			}
		}
	}
	return 0, r.throwTypeError("%%TypedArray%%.%s requires a typed array constructor", name)
}

// compareNumeric orders two elements of a typed array.
//
// A BigInt array's elements are BigInts, which have no finite float64 form, so
// they are compared as integers rather than converted.
func compareNumeric(x, y Value) int {
	if x.IsBigInt() && y.IsBigInt() {
		return x.BigInt().Cmp(y.BigInt())
	}
	a, b := x.Number(), y.Number()
	switch {
	case a != a:
		// NaN sorts last, and two NaNs are equal for this purpose.
		if b != b {
			return 0
		}
		return 1
	case b != b:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
