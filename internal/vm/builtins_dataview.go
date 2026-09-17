package vm

import (
	"encoding/binary"
	"math"
	"math/big"
)

// DataView.
//
// A DataView reads and writes a buffer at an arbitrary byte offset in either
// endianness, where a typed array is fixed to one element type, native-endian
// and naturally aligned. That is what makes it the right tool for binary
// formats: a wire protocol chooses its own layout, and rarely the one the
// machine prefers.
//
// The endianness parameter defaults to big-endian, which is the opposite of
// every typed array and of every machine this runs on, but is what the
// specification says.

// dataViewData is a DataView's state.
type dataViewData struct {
	buffer     *Object
	byteOffset int
	byteLength int
}

func (d *dataViewData) storage() *arrayBufferData {
	b, _ := d.buffer.data.(*arrayBufferData)
	return b
}

// dataViewOf recovers a view from a receiver.
//
// Whether the buffer is still there is a separate question, asked later: the
// index and the value are converted first, and a conversion can detach it, so
// checking here would answer about the wrong moment -- and would report a
// detached buffer where an out-of-range index should have been reported.
func (r *Runtime) dataViewOf(this Value, name string) (*dataViewData, error) {
	if !this.IsObject() || this.Object().class != ClassDataView {
		return nil, r.throwTypeError("%s called on an incompatible receiver", name)
	}
	d, ok := this.Object().data.(*dataViewData)
	if !ok {
		return nil, r.throwTypeError("%s called on an uninitialized DataView", name)
	}
	return d, nil
}

// dataViewLive recovers a view and requires its buffer to still be there, which
// the size getters need.
func (r *Runtime) dataViewLive(this Value, name string) (*dataViewData, error) {
	d, err := r.dataViewOf(this, name)
	if err != nil {
		return nil, err
	}
	if d.storage().detached {
		return nil, r.throwTypeError("the underlying ArrayBuffer has been detached")
	}
	return d, nil
}

func (r *Runtime) initDataViewBuiltins() {
	proto := newObject(r.proto.object, ClassObject)

	r.newCtor("DataView", 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if !rt.Constructing() {
			return Undefined, rt.throwTypeError("DataView requires new")
		}
		bufArg := arg(args, 0)
		if !bufArg.IsObject() || bufArg.Object().class != ClassArrayBuffer {
			return Undefined, rt.throwTypeError("DataView requires an ArrayBuffer")
		}
		buf := bufArg.Object()
		storage := buf.data.(*arrayBufferData)

		off, err := rt.toIndex(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		// The length argument is read before the detach check below, because
		// its ToIndex conversion may itself detach the buffer.
		length := int64(-1)
		if lv := arg(args, 2); !lv.IsUndefined() {
			n, err := rt.toIndex(lv)
			if err != nil {
				return Undefined, err
			}
			length = n
		}
		if storage.detached {
			return Undefined, rt.throwTypeError("the ArrayBuffer has been detached")
		}
		total := int64(len(storage.bytes))
		if off > total {
			return Undefined, rt.throwRangeError("the offset is outside the buffer")
		}
		if length < 0 {
			length = total - off
		} else if off+length > total {
			return Undefined, rt.throwRangeError("the view extends past the end of the buffer")
		}

		// Reading the prototype can run a getter, and that getter can detach
		// the buffer the view was about to describe -- so the check is made
		// again afterwards.
		viewProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		if storage.detached {
			return Undefined, rt.throwTypeError("the ArrayBuffer has been detached")
		}
		o := newObject(viewProto, ClassDataView)
		o.data = &dataViewData{buffer: buf, byteOffset: int(off), byteLength: int(length)}
		return Obj(o), nil
	})

	r.defGetter(proto, "buffer", func(rt *Runtime, this Value, args []Value) (Value, error) {
		// buffer is readable on a detached view, unlike everything else, since
		// it is how a script observes the detachment.
		if !this.IsObject() || this.Object().class != ClassDataView {
			return Undefined, rt.throwTypeError("DataView.prototype.buffer called on an incompatible receiver")
		}
		d, ok := this.Object().data.(*dataViewData)
		if !ok {
			return Undefined, rt.throwTypeError("DataView.prototype.buffer called on an uninitialized DataView")
		}
		return Obj(d.buffer), nil
	})

	r.defGetter(proto, "byteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dataViewLive(this, "DataView.prototype.byteLength")
		if err != nil {
			return Undefined, err
		}
		return Int(d.byteLength), nil
	})

	r.defGetter(proto, "byteOffset", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dataViewLive(this, "DataView.prototype.byteOffset")
		if err != nil {
			return Undefined, err
		}
		return Int(d.byteOffset), nil
	})

	for _, spec := range dataViewTypes {
		// The declared length stops at the first optional parameter, so every
		// getter reports one and every setter two however many bytes it moves
		// and whether or not it takes an endianness flag.
		r.defMethod(proto, "get"+spec.name, 1, r.dataViewGetter(spec))
		r.defMethod(proto, "set"+spec.name, 2, r.dataViewSetter(spec))
	}

	r.defToStringTag(proto, "DataView")
}

// dataViewType describes one of the element types a DataView understands.
type dataViewType struct {
	name string
	size int
	kind elemType
	// argc is 2 for the types that take an endianness flag and 1 for the
	// single-byte ones, where the byte order cannot be observed.
	//
	// It is not the declared length: that stops at the first optional
	// parameter, so every getter reports one and every setter two.
	argc int
}

var dataViewTypes = [...]dataViewType{
	{"Int8", 1, elemInt8, 1},
	{"Uint8", 1, elemUint8, 1},
	{"Int16", 2, elemInt16, 2},
	{"Uint16", 2, elemUint16, 2},
	{"Int32", 4, elemInt32, 2},
	{"Uint32", 4, elemUint32, 2},
	{"Float16", 2, elemFloat16, 2},
	{"Float32", 4, elemFloat32, 2},
	{"Float64", 8, elemFloat64, 2},
	{"BigInt64", 8, elemBigInt64, 2},
	{"BigUint64", 8, elemBigUint64, 2},
}

// resolve turns an index argument into a byte range within the view, or an
// error if it does not fit.
//
// The index is converted before the endianness flag, and both before the
// detached check, because each conversion can run user code that detaches the
// buffer.
func (r *Runtime) resolveViewIndex(d *dataViewData, idxArg Value, size int, name string) (int, error) {
	i, err := r.toIndex(idxArg)
	if err != nil {
		return 0, err
	}
	if d.storage().detached {
		return 0, r.throwTypeError("the underlying ArrayBuffer has been detached")
	}
	if i+int64(size) > int64(d.byteLength) {
		return 0, r.throwRangeError("%s reads past the end of the view", name)
	}
	return d.byteOffset + int(i), nil
}

func (r *Runtime) dataViewGetter(spec dataViewType) NativeFunc {
	name := "DataView.prototype.get" + spec.name
	return func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dataViewOf(this, name)
		if err != nil {
			return Undefined, err
		}
		little := viewEndianness(args, spec)
		off, err := rt.resolveViewIndex(d, arg(args, 0), spec.size, name)
		if err != nil {
			return Undefined, err
		}
		return decodeView(d.storage().bytes[off:off+spec.size], spec, little), nil
	}
}

func (r *Runtime) dataViewSetter(spec dataViewType) NativeFunc {
	name := "DataView.prototype.set" + spec.name
	return func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.dataViewOf(this, name)
		if err != nil {
			return Undefined, err
		}
		// The order here is the specification's: index, then value, then
		// endianness, each of which may run user code.
		i, err := rt.toIndex(arg(args, 0))
		if err != nil {
			return Undefined, err
		}

		var (
			num float64
			big *BigInt
		)
		v := arg(args, 1)
		if spec.kind == elemBigInt64 || spec.kind == elemBigUint64 {
			bv, err := rt.toBigIntOperand(v)
			if err != nil {
				return Undefined, err
			}
			big = bv
		} else {
			num, err = rt.toNumber(v)
			if err != nil {
				return Undefined, err
			}
		}

		little := false
		if spec.argc == 2 {
			little = arg(args, 2).Truthy()
		}

		if d.storage().detached {
			return Undefined, rt.throwTypeError("the underlying ArrayBuffer has been detached")
		}
		if i+int64(spec.size) > int64(d.byteLength) {
			return Undefined, rt.throwRangeError("%s writes past the end of the view", name)
		}
		off := d.byteOffset + int(i)
		encodeView(d.storage().bytes[off:off+spec.size], spec, little, num, big)
		return Undefined, nil
	}
}

// viewEndianness reads the littleEndian flag, which the single-byte accessors
// do not take.
func viewEndianness(args []Value, spec dataViewType) bool {
	if spec.argc != 2 {
		return false
	}
	return arg(args, 1).Truthy()
}

// decodeView reads one value out of a byte range.
func decodeView(b []byte, spec dataViewType, little bool) Value {
	u := readUint(b, little)
	switch spec.kind {
	case elemInt8:
		return Int(int(int8(u)))
	case elemUint8:
		return Int(int(uint8(u)))
	case elemInt16:
		return Int(int(int16(u)))
	case elemUint16:
		return Int(int(uint16(u)))
	case elemInt32:
		return Int(int(int32(u)))
	case elemUint32:
		return Float(float64(uint32(u)))
	case elemFloat16:
		return Float(float16frombits(uint16(u)))
	case elemFloat32:
		return Float(float64(math.Float32frombits(uint32(u))))
	case elemFloat64:
		return Float(math.Float64frombits(u))
	case elemBigInt64:
		return Big(NewBigInt(int64(u)))
	case elemBigUint64:
		bi := &BigInt{}
		bi.V.SetUint64(u)
		return Big(bi)
	}
	return Undefined
}

// encodeView writes one value into a byte range.
func encodeView(b []byte, spec dataViewType, little bool, num float64, big *BigInt) {
	var u uint64
	switch spec.kind {
	case elemInt8, elemUint8:
		u = uint64(uint8(toInt32Wrap(num)))
	case elemInt16, elemUint16:
		u = uint64(uint16(toInt32Wrap(num)))
	case elemInt32, elemUint32:
		u = uint64(uint32(toInt32Wrap(num)))
	case elemFloat16:
		u = uint64(float16bits(num))
	case elemFloat32:
		u = uint64(math.Float32bits(float32(num)))
	case elemFloat64:
		u = math.Float64bits(num)
	case elemBigInt64, elemBigUint64:
		u = bigLowUint64(big)
	}
	writeUint(b, u, little)
}

// readUint assembles the bytes of a range into an integer of the range's width.
func readUint(b []byte, little bool) uint64 {
	switch len(b) {
	case 1:
		return uint64(b[0])
	case 2:
		if little {
			return uint64(binary.LittleEndian.Uint16(b))
		}
		return uint64(binary.BigEndian.Uint16(b))
	case 4:
		if little {
			return uint64(binary.LittleEndian.Uint32(b))
		}
		return uint64(binary.BigEndian.Uint32(b))
	default:
		if little {
			return binary.LittleEndian.Uint64(b)
		}
		return binary.BigEndian.Uint64(b)
	}
}

func writeUint(b []byte, u uint64, little bool) {
	switch len(b) {
	case 1:
		b[0] = byte(u)
	case 2:
		if little {
			binary.LittleEndian.PutUint16(b, uint16(u))
		} else {
			binary.BigEndian.PutUint16(b, uint16(u))
		}
	case 4:
		if little {
			binary.LittleEndian.PutUint32(b, uint32(u))
		} else {
			binary.BigEndian.PutUint32(b, uint32(u))
		}
	default:
		if little {
			binary.LittleEndian.PutUint64(b, u)
		} else {
			binary.BigEndian.PutUint64(b, u)
		}
	}
}

// bigLowUint64 returns a BigInt's low 64 bits in two's complement.
//
// big.Int.Uint64 is only meaningful for a value that fits unsigned, and for a
// negative one returns the magnitude rather than the wrapped representation, so
// -1n would store as 1 instead of all ones. The specification's ToBigInt64 and
// ToBigUint64 are both a modulo by 2**64, which is what this does.
func bigLowUint64(b *BigInt) uint64 {
	var m big.Int
	m.And(&b.V, mask64)
	return m.Uint64()
}

// mask64 is 2**64 - 1, held once rather than rebuilt per write.
var mask64 = new(big.Int).SetUint64(^uint64(0))
