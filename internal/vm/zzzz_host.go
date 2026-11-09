package vm

// What a package defining built-in objects of its own through the runtime
// needs -- the WebAssembly namespace's classes, for one: constructors whose
// instances carry Go state, its ArrayBuffers over memory the package owns,
// BigInt conversions and a global made when first read.
//
// The functions are package variables assigned in init, so that adding them
// moves no code before them (see AGENTS.md on the package's layout).

// hostBuffer is the state of an ArrayBuffer over memory a host package owns:
// script cannot detach or transfer it, and the host resizes it.
type hostBuffer struct {
	// owner keeps the memory the buffer's bytes are in alive.
	owner any
	// resize, for a resizable buffer, grows the memory to n bytes, calling
	// HostSetArrayBufferBytes; it returns the error to throw if it cannot.
	resize func(n int) error
}

var (
	// HostConstructor returns a constructor named name, of length length,
	// whose prototype property is proto, calling fn. It is not made a
	// global.
	HostConstructor func(r *Runtime, name string, length int, proto *Object, fn NativeFunc) *Object
	// HostObject returns an ordinary extensible object of prototype proto
	// carrying data.
	HostObject func(proto *Object, data any) *Object
	// HostData returns the data an object made by HostObject carries, or
	// nil for any other value.
	HostData func(v Value) any
	// HostProtoFromNewTarget returns the prototype a constructor being
	// called with new should give its object: new.target's prototype
	// property if it is an object, fallback otherwise.
	HostProtoFromNewTarget func(r *Runtime, fallback *Object) (*Object, error)
	// HostToBigInt64 converts v to a BigInt, as BigInt arithmetic converts
	// its operands, throwing a TypeError for a Number, and returns its value
	// modulo 2^64 as an int64.
	HostToBigInt64 func(r *Runtime, v Value) (int64, error)
	// HostBigInt64 returns the BigInt of v.
	HostBigInt64 func(v int64) Value
	// HostArrayBuffer returns an ArrayBuffer over b, memory owner keeps,
	// which script cannot detach or transfer. A resizable one, of
	// maxByteLength max, is resized by resize.
	HostArrayBuffer func(r *Runtime, b []byte, owner any, resizable bool, max int64, resize func(n int) error) Value
	// HostDetachArrayBuffer detaches a buffer HostArrayBuffer made.
	HostDetachArrayBuffer func(r *Runtime, v Value)
	// HostSetArrayBufferBytes gives a buffer HostArrayBuffer made new
	// bytes, as its memory grew.
	HostSetArrayBufferBytes func(r *Runtime, v Value, b []byte)
	// HostSharedArrayBuffer returns a SharedArrayBuffer over the first
	// length bytes of mem, memory owner keeps that never moves; growable up
	// to max bytes by grow if max >= 0.
	HostSharedArrayBuffer func(r *Runtime, mem []byte, length int, owner any, max int64, grow func(n int) error) Value
	// HostDefineLazyGlobal defines the global name, made by build the first
	// time it is read.
	HostDefineLazyGlobal func(r *Runtime, name string, build func(*Runtime) *Object)
)

func init() {
	HostConstructor = func(r *Runtime, name string, length int, proto *Object, fn NativeFunc) *Object {
		c, fd := r.newSlabFuncObject(r.proto.function, ClassFunction)
		*fd = funcData{native: fn, name: name, length: int32(length), ctorKind: ctorBase, realm: r.Realm}
		c.setOwnRaw(atomPrototype, Obj(proto), 0)
		proto.setOwnRaw(atomConstructor, Obj(c), propWritable|propConfigurable)
		return c
	}
	HostObject = func(proto *Object, data any) *Object {
		o := newObjectExt(proto, ClassObject, objExtensible)
		o.setData(data)
		return o
	}
	HostData = func(v Value) any {
		if !v.IsObject() {
			return nil
		}
		return v.Object().dataOf()
	}
	HostProtoFromNewTarget = func(r *Runtime, fallback *Object) (*Object, error) {
		return r.protoFromNewTargetErr(fallback)
	}
	HostToBigInt64 = func(r *Runtime, v Value) (int64, error) {
		b, err := r.toBigIntOperand(v)
		if err != nil {
			return 0, err
		}
		return int64(bigLowUint64(b)), nil
	}
	HostBigInt64 = func(v int64) Value { return Big(NewBigInt(v)) }
	HostArrayBuffer = func(r *Runtime, b []byte, owner any, resizable bool, max int64, resize func(n int) error) Value {
		o := newObject(r.arrayBufferProto, ClassArrayBuffer)
		d := &arrayBufferData{bytes: b, external: true, host: &hostBuffer{owner: owner, resize: resize}}
		if resizable {
			d.resizable, d.maxByteLength = true, max
		}
		o.setData(d)
		return Obj(o)
	}
	HostDetachArrayBuffer = func(r *Runtime, v Value) {
		if d, ok := HostData(v).(*arrayBufferData); ok {
			d.detached, d.bytes = true, nil
			r.bufferChanged()
		}
	}
	HostSetArrayBufferBytes = func(r *Runtime, v Value, b []byte) {
		if d, ok := HostData(v).(*arrayBufferData); ok {
			d.bytes = b
			r.bufferChanged()
		}
	}
	HostSharedArrayBuffer = func(r *Runtime, mem []byte, length int, owner any, max int64, grow func(n int) error) Value {
		m := &SharedMemory{mem: mem, max: max, pinned: true}
		m.length.Store(int64(length))
		o := newObject(r.proto.sharedArrayBuffer, ClassArrayBuffer)
		d := &arrayBufferData{bytes: m.bytes(), shared: true, block: m, external: true,
			host: &hostBuffer{owner: owner, resize: grow}}
		if max >= 0 {
			d.resizable, d.maxByteLength = true, max
		}
		o.setData(d)
		return Obj(o)
	}
	HostDefineLazyGlobal = func(r *Runtime, name string, build func(*Runtime) *Object) {
		r.defineLazyGlobal(name, build)
	}
}
