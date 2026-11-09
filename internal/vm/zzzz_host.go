package vm

import (
	"math/bits"
	"reflect"
	"unsafe"
)

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
	// HostJITEnabled reports whether the runtime runs native code it
	// compiles (WithJIT), or is nil in a build without a JIT, which never
	// does.
	HostJITEnabled func(r *Runtime) bool
	// HostDefineLazyGlobal defines the global name, made by build the first
	// time it is read.
	HostDefineLazyGlobal func(r *Runtime, name string, build func(*Runtime) *Object)
	// HostNodeQuirks reports whether the runtime follows Node where it
	// departs from the standards (Config.NodeQuirks).
	HostNodeQuirks func(r *Runtime) bool
	// HostErrorClass returns a constructor named name, not a global, that
	// makes errors as the NativeError constructors do -- a message, a cause
	// and a stack -- inheriting from Error, and its prototype, which carries
	// the name and an empty message.
	HostErrorClass func(r *Runtime, name string) (ctor, proto *Object)
	// HostNewError returns an error of prototype proto with message msg and
	// the stack where it is made, as one the runtime throws.
	HostNewError func(r *Runtime, proto *Object, msg string) *Object
	// HostSetSlot attaches data to any object, under a key script cannot
	// reach, and HostSlot returns what v carries there, or nil.
	HostSetSlot func(r *Runtime, o *Object, data any)
	HostSlot    func(r *Runtime, v Value) any
	// HostDefineOwn defines an own data property of the attributes given,
	// and HostDefineAccessor an own accessor, either half of which may be
	// nil.
	HostDefineOwn      func(o *Object, key Atom, v Value, writable, enumerable, configurable bool)
	HostDefineAccessor func(r *Runtime, o *Object, key Atom, get, set *Object, enumerable, configurable bool)
	// HostToStringTag returns the atom of Symbol.toStringTag.
	HostToStringTag func(r *Runtime) Atom
	// HostPreventExtensions makes an ordinary object non-extensible.
	HostPreventExtensions func(o *Object)
	// HostCheckInterrupt reports why the host has stopped the runtime, if it
	// has, looking now rather than at the next periodic check.
	HostCheckInterrupt func(r *Runtime) error
	// HostBufferSource returns the bytes a buffer source -- an ArrayBuffer
	// or a typed array over any buffer -- is looking at, and whether v is
	// one; a detached buffer, or a view of one, has no bytes.
	HostBufferSource func(v Value) ([]byte, bool)
	// HostCaptureStack gives o, an object that is not an error, an own stack
	// property: the stack where it is made, as an error's stack reads.
	HostCaptureStack func(r *Runtime, o *Object)
	// HostSetPrototype sets an ordinary object's prototype.
	HostSetPrototype func(o, proto *Object)
	// HostHasProperty is the in operator: whether o has key, own or
	// inherited, asking a proxy.
	HostHasProperty func(r *Runtime, o *Object, key Atom) (bool, error)
	// HostStringFromUnits returns the string of UTF-16 code units u.
	HostStringFromUnits func(u []uint16) *String
	// HostToBigUint64 converts v to a BigInt as HostToBigInt64 does and
	// returns its value modulo 2^64, and whether it was in u64's range.
	HostToBigUint64 func(r *Runtime, v Value) (uint64, bool, error)

	// HostMemoryLimited reports whether the runtime has a memory limit
	// (Config.MemoryLimit), which what the host holds for it is charged to.
	HostMemoryLimited func(r *Runtime) bool
	// HostReserveMemory counts n more bytes the host is about to hold for
	// the runtime, reporting false, and counting nothing, if they would take
	// it past its limit even once its heap has been measured again.
	HostReserveMemory func(r *Runtime, n int64) bool
	// HostChargeMemory counts n bytes the host holds for the runtime,
	// returning the error that stops it if they take it past its limit.
	HostChargeMemory func(r *Runtime, n int64) error
	// HostMemoryExceeded stops the runtime as one past its limit is, and
	// returns the error it stops with.
	HostMemoryExceeded func(r *Runtime) error
)

// HostSized is host data that says how much memory it holds, for a
// runtime's memory limit: the walk that measures the runtime's heap asks
// the data of an object HostObject or HostSetSlot made rather than looking
// into it. The bytes of a buffer HostArrayBuffer or HostSharedArrayBuffer
// made are counted by their capacity, so a host gives it no more than its
// length. MemoryFootprint returns the bytes held and kept alive; has
// reports whether the walk has counted p already, marking it counted, so
// that what several objects share is counted once.
type HostSized interface {
	MemoryFootprint(has func(p unsafe.Pointer) bool) int64
}

// meterData visits an object's data d: it counts what host data that is
// HostSized holds, and walks anything else.
var meterData func(m *memoryMeter, d any)

// hostSlotSymbol is the key of the data HostSetSlot attaches.
var hostSlotSymbol = NewSymbol("host slot", true)

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
	HostNodeQuirks = func(r *Runtime) bool { return r.nodeQuirks }
	HostErrorClass = func(r *Runtime, name string) (*Object, *Object) {
		proto := newObject(r.proto.nativeErrors[errError], ClassObject)
		ctor := HostConstructor(r, name, 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
			p, err := rt.protoFromNewTargetErr(proto)
			if err != nil {
				return Undefined, err
			}
			o := newErrorObject(p)
			// The trace leaves out the constructor, and a subclass's
			// constructors up to new.target's.
			var until *Object
			if f := rt.topFrame(); f != nil && f.newTarget.IsObject() && f.newTarget.Object() != f.callee {
				until = f.newTarget.Object()
			}
			rt.attachStack(o, rt.captureTrace(until, true))
			if m := arg(args, 0); !m.IsUndefined() {
				s, err := rt.toString(m)
				if err != nil {
					return Undefined, err
				}
				o.setOwnRaw(atomMessage, Str(s), propWritable|propConfigurable)
			}
			if opts := arg(args, 1); opts.IsObject() {
				causeKey := rt.atoms.intern("cause")
				has, err := rt.hasPropErr(opts.Object(), causeKey)
				if err != nil {
					return Undefined, err
				}
				if has {
					cause, err := rt.getProp(opts.Object(), causeKey, opts)
					if err != nil {
						return Undefined, err
					}
					o.setOwnRaw(causeKey, cause, propWritable|propConfigurable)
				}
			}
			return Obj(o), nil
		})
		proto.setOwnRaw(atomName, Str(NewString(name)), propWritable|propConfigurable)
		proto.setOwnRaw(atomMessage, Str(emptyString), propWritable|propConfigurable)
		ctor.setProto(r.proto.errorCtors[errError])
		return ctor, proto
	}
	HostNewError = func(r *Runtime, proto *Object, msg string) *Object {
		o := newErrorObject(proto)
		r.attachStack(o, r.captureTrace(nil, false))
		o.setOwnRaw(atomMessage, Str(NewString(msg)), propWritable|propConfigurable)
		return o
	}
	HostSetSlot = func(r *Runtime, o *Object, data any) {
		holder := newObjectExt(nil, ClassObject, objExtensible)
		holder.setData(data)
		o.setOwnRaw(r.atoms.internSymbol(hostSlotSymbol), Obj(holder), propPrivate)
	}
	HostSlot = func(r *Runtime, v Value) any {
		if !v.IsObject() {
			return nil
		}
		p := v.Object().getOwn(r.atoms.internSymbol(hostSlotSymbol))
		if p.ok() && p.flags()&propPrivate != 0 && p.value().IsObject() {
			return p.value().Object().dataOf()
		}
		return nil
	}
	HostDefineOwn = func(o *Object, key Atom, v Value, writable, enumerable, configurable bool) {
		var f propFlags
		if writable {
			f |= propWritable
		}
		if enumerable {
			f |= propEnumerable
		}
		if configurable {
			f |= propConfigurable
		}
		o.setOwnRaw(key, v, f)
	}
	HostDefineAccessor = func(r *Runtime, o *Object, key Atom, get, set *Object, enumerable, configurable bool) {
		var f propFlags
		if enumerable {
			f |= propEnumerable
		}
		if configurable {
			f |= propConfigurable
		}
		r.defineAccessor(o, key, get, set, f)
	}
	HostToStringTag = func(r *Runtime) Atom { return r.atoms.internSymbol(r.wellKnown.toStringTag) }
	HostPreventExtensions = func(o *Object) { o.flags &^= objExtensible }
	HostCheckInterrupt = func(r *Runtime) error { return r.checkInterruptNow() }
	HostBufferSource = func(v Value) ([]byte, bool) {
		if !v.IsObject() {
			return nil, false
		}
		switch d := v.Object().dataOf().(type) {
		case *arrayBufferData:
			if d.shared || v.Object().class != ClassArrayBuffer {
				return nil, false
			}
			if d.detached {
				return nil, true
			}
			return d.bytes, true
		case *typedArrayData:
			if v.Object().class != ClassTypedArray {
				return nil, false
			}
			storage := d.storage()
			if storage == nil || storage.detached || d.outOfBounds() {
				return nil, true
			}
			start := d.byteOffset
			end := start + d.count()*d.info().size
			if start > len(storage.bytes) || end > len(storage.bytes) {
				return nil, true
			}
			return storage.bytes[start:end], true
		}
		return nil, false
	}
	HostCaptureStack = func(r *Runtime, o *Object) {
		e := newErrorObject(r.proto.nativeErrors[errError])
		r.attachStack(e, r.captureTrace(nil, true))
		v, err := r.getProp(e, atomStack, Obj(e))
		if err != nil {
			v = Undefined
		}
		o.setOwnRaw(atomStack, v, propWritable|propConfigurable)
	}
	HostSetPrototype = func(o, proto *Object) { o.setProto(proto) }
	HostHasProperty = func(r *Runtime, o *Object, key Atom) (bool, error) { return r.hasPropErr(o, key) }
	HostStringFromUnits = func(u []uint16) *String { return fromUnits(u) }
	HostToBigUint64 = func(r *Runtime, v Value) (uint64, bool, error) {
		b, err := r.toBigIntOperand(v)
		if err != nil {
			return 0, false, err
		}
		if b.IsZero() {
			return 0, true, nil
		}
		return bigLowUint64(b), !b.negative() && b.digits()*bits.UintSize <= 64, nil
	}
	HostMemoryLimited = func(r *Runtime) bool { return r.meter != nil }
	HostReserveMemory = func(r *Runtime, n int64) bool {
		m := r.meter
		if m == nil || n <= 0 {
			return true
		}
		if m.live+n > m.limit {
			// What was counted may since have been let go of.
			m.allocsAt = m.allocated()
			m.live = max(0, m.walk(r)-m.baseline)
			if m.live+n > m.limit {
				return false
			}
		}
		m.live += n
		return true
	}
	HostChargeMemory = func(r *Runtime, n int64) error {
		if r.meter == nil || n <= 0 {
			return nil
		}
		if n > int64(^uint(0)>>1) {
			return r.stop(ErrMemoryLimit)
		}
		return r.reserveMemory(int(n))
	}
	HostMemoryExceeded = func(r *Runtime) error { return r.stop(ErrMemoryLimit) }
	meterData = func(m *memoryMeter, d any) {
		if s, ok := d.(HostSized); ok {
			m.total += s.MemoryFootprint(m.has)
			return
		}
		m.values = append(m.values, reflect.ValueOf(d))
	}
}
