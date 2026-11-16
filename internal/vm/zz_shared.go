package vm

import (
	"encoding/binary"
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

func truthy(v Value) bool {
	return math.Float64bits(v.num) == trueBits || math.Float64bits(v.num) != falseBits && v.Truthy()
}

// elemAt is an element in an array's dense storage, as get_index reads one
// first thing, or a typed array's in its buffer, or false.
//
// The key is not asked whether it is a number: every other value's num is
// a NaN, which the round trip through uint32 never gives back, so the test
// that the key is an index is that test too. Each test leaves on failure,
// rather than joining the next with &&, which Go compiles to a flag kept
// in a register and tested again; and the element's bits are read as an
// integer, not as a float moved through the stack to be compared.
//
//go:noinline
func elemAt(obj, key Value) (Value, bool) {
	if !obj.IsObject() {
		return Undefined, false
	}
	o := obj.object()
	i, e := uint32(key.num), o.elemsOf()
	if float64(i) != key.num || uint(i) >= uint(len(e)) || o.flags&objMappedArguments != 0 {
		if o.class != ClassTypedArray || float64(i) != key.num {
			return Undefined, false
		}
		// A typed array's element, read from the storage its view keeps
		// once typedRead has filled it in, as typedRead reads it: written
		// here, not called, as a call would give elemAt, which every
		// array's read makes, a check of the stack's room.
		t, ok := o.dataOf().(*typedArrayData)
		if !ok || uint(i) >= uint(t.fixedLength) {
			return Undefined, false
		}
		st := t.st
		if st == nil || t.end > len(st.bytes) {
			return Undefined, false
		}
		b, j := st.bytes[t.byteOffset:t.end], int(i)
		switch t.kind {
		case elemInt8:
			return Int(int(int8(b[j]))), true
		case elemUint8, elemUint8Clamped:
			return Int(int(b[j])), true
		case elemInt16:
			return Int(int(int16(binary.LittleEndian.Uint16(b[j<<1:])))), true
		case elemUint16:
			return Int(int(binary.LittleEndian.Uint16(b[j<<1:]))), true
		case elemInt32:
			return Int32(int32(binary.LittleEndian.Uint32(b[j<<2:]))), true
		case elemUint32:
			return Uint32(binary.LittleEndian.Uint32(b[j<<2:])), true
		case elemFloat32:
			return Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:])))), true
		case elemFloat64:
			return Float(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:]))), true
		}
		return Undefined, false
	}
	p := &e[i]
	if holeAt(p) {
		return Undefined, false
	}
	return *p, true
}

// setElem stores v in an array's dense storage, as set_index does first
// thing, or a number in a typed array's buffer, or reports false. The key is a number when it is an index, as in
// elemAt.
//
//go:noinline
func setElem(o, k, v Value) bool {
	if !o.IsObject() {
		return false
	}
	a := o.object()
	i, e := uint32(k.num), a.elemsOf()
	if float64(i) != k.num || uint(i) >= uint(len(e)) || a.flags&(objMappedArguments|objSharedElems) != 0 {
		if a.class != ClassTypedArray || float64(i) != k.num || !v.IsNumber() {
			return false
		}
		// A number stored in a typed array's buffer, as typedStore stores
		// it, through the storage its view keeps once typedStore has filled
		// it in, and written here for the reason elemAt's read is. A
		// Uint8Clamped element, rounded by a call, is typedStore's.
		t, ok := a.dataOf().(*typedArrayData)
		if !ok || uint(i) >= uint(t.fixedLength) {
			return false
		}
		st := t.st
		if st == nil || t.end > len(st.bytes) || st.immutable {
			return false
		}
		// An integer element takes a number that is an int32, as toInt32
		// does first; another is typedStore's, as toInt32's slow path is a
		// call.
		b, n, j := st.bytes[t.byteOffset:t.end], v.num, int(i)
		x := int32(n)
		switch t.kind {
		case elemInt8, elemUint8:
			if float64(x) != n {
				return false
			}
			b[j] = byte(x)
		case elemInt16, elemUint16:
			if float64(x) != n {
				return false
			}
			binary.LittleEndian.PutUint16(b[j<<1:], uint16(x))
		case elemInt32, elemUint32:
			if float64(x) != n {
				return false
			}
			binary.LittleEndian.PutUint32(b[j<<2:], uint32(x))
		case elemFloat32:
			binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(n)))
		case elemFloat64:
			binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(n))
		default:
			return false
		}
		return true
	}
	p := &e[i]
	if holeAt(p) {
		return false
	}
	*p = v
	return true
}

// holeAt is isHole of the element at p, its bits read straight into an
// integer register.
func holeAt(p *Value) bool { return *(*uint64)(unsafe.Pointer(&p.num)) == holeBits }

func isBitwise(op bytecode.Op) bool {
	switch op {
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		return true
	}
	return false
}

// toInt32 is ToInt32 of a number, without the call where it is already an
// int32, which in code that uses bitwise operators it almost always is.
func toInt32(f float64) int32 {
	if i := int32(f); float64(i) == f {
		return i
	}
	return jsnum.ToInt32(f)
}

// callRest is callDirect for a callee that is the object o, whose funcData
// is fd or nil, once the register interpreter's call has found it is not
// one regPlan has given its register code: callDirect's other cases. A compiled
// function's comes first, which is callDirect's third: the two before it,
// a Math function and Function.prototype.call, are built-ins, so no callee
// is of both, and a pure body's call, the commonest here, asks nothing
// first that cannot be its answer.
func (r *Runtime) callRest(o *Object, fd *funcData, this Value, args []Value) (Value, error) {
	if fd != nil && fd.native == nil && !fd.bound &&
		fd.closure != nil && fd.closure.realm == r.Realm {
		if fn := fd.closure.fn; fn.DirectCall {
			if err := r.tick(); err != nil {
				return Undefined, err
			}
			if leaf := fn.Leaf; leaf == bytecode.LeafPure {
				if pureFirst(fd, fn) {
					if v, ok := r.pureCall(fd, this, args); ok {
						return v, nil
					}
				}
			} else if leaf != bytecode.LeafNone && this.IsObject() {
				if v, ok := r.leafCall(fd.closure, this.Object(), args); ok {
					return v, nil
				}
			}
			newTarget := Undefined
			if fd.arrow {
				this, newTarget = fd.extra.lexThis, fd.extra.newTarget()
			}
			v, err := r.runFD(fd.closure, this, args, newTarget, o, fd)
			if err == errNoSuper {
				err = r.throwError(errReference, "%s", errNoSuper.Error())
			}
			if !fd.planned {
				planCall(fd)
			}
			return v, err
		}
	}
	if fd != nil && fd.mathOp != 0 {
		if v, ok := mathCall(fd.mathOp, args); ok {
			return v, nil
		}
	}
	if o == r.callFn {
		return r.callThrough(o, this, args)
	}
	if fd != nil && fd.native != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) {
		if op := fd.elemOp; op >= elemNativeFirst {
			if r.meter == nil && leafChecks[op](r, this, args) {
				if v, err := fd.native(r, this, args); err == nil {
					return v, nil
				}
			}
		} else if op != 0 {
			if v, ok := r.pureElemOp(op, this, args, true); ok {
				return v, nil
			}
		}
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
			return Undefined, err
		}
		v, err := fd.native(r, this, args)
		r.frameDepth--
		if r.meter != nil && err == nil {
			err = r.chargeResult(v)
		}
		return v, err
	}
	return r.call(Obj(o), this, args)
}

// makeClosure builds a closure from a nested function template
// (newClosure), and plans its calls (planCall) where its function's
// register code is built: what planCall asks of it is settled when it is
// made.
func (r *Runtime) makeClosure(f *frame, c Value) *Object {
	o := r.newClosure(f, c)
	if fd := o.fn(); fd != nil && atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
		planCall(fd)
	}
	return o
}
