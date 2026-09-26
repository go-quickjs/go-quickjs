package vm

import (
	"math"
	"time"
)

// SharedArrayBuffer and Atomics.
//
// A Runtime is one agent: nothing else runs on its memory while it does, so a
// shared buffer is an ordinary buffer marked as shared, and every Atomics
// operation is atomic by construction. What sharing changes is what may be
// done to the buffer -- it cannot be detached or transferred, and a growable
// one only grows -- and which methods accept it: an ArrayBuffer method refuses
// a shared buffer and a SharedArrayBuffer method refuses one that is not.
//
// Atomics.wait blocks the agent, and with no other agent to notify it, only
// its timeout or the host's context ends the wait.

// sharedBufferOf recovers a shared buffer from a receiver.
func (r *Runtime) sharedBufferOf(this Value, name string) (*arrayBufferData, error) {
	if this.IsObject() && this.Object().class == ClassArrayBuffer {
		if b, ok := this.Object().data.(*arrayBufferData); ok && b.shared {
			return b, nil
		}
	}
	return nil, r.throwTypeError("%s called on an incompatible receiver", name)
}

func (r *Runtime) initSharedArrayBufferBuiltins() {
	proto := newObject(r.proto.object, ClassObject)

	ctor := r.newCtor("SharedArrayBuffer", 1, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("SharedArrayBuffer"); err != nil {
			return Undefined, err
		}
		n, err := rt.toIndex(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// A maxByteLength in the options makes the buffer growable up to it.
		max := int64(-1)
		if opts := arg(args, 1); opts.IsObject() {
			mv, err := rt.getProp(opts.Object(), rt.atoms.intern("maxByteLength"), opts)
			if err != nil {
				return Undefined, err
			}
			if !mv.IsUndefined() {
				if max, err = rt.toIndex(mv); err != nil {
					return Undefined, err
				}
				if n > max {
					return Undefined, rt.throwRangeError("the SharedArrayBuffer length exceeds its maxByteLength")
				}
			}
		}
		p, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		if n > maxBufferLength {
			return Undefined, rt.throwRangeError("the SharedArrayBuffer length is too large")
		}
		if max > maxBufferLength {
			return Undefined, rt.throwRangeError("the SharedArrayBuffer maxByteLength is too large")
		}
		o := newObject(p, ClassArrayBuffer)
		b := &arrayBufferData{bytes: make([]byte, n), shared: true}
		if max >= 0 {
			b.resizable, b.maxByteLength = true, max
		}
		o.data = b
		return Obj(o), nil
	})
	r.defSpecies(ctor)
	r.sharedArrayBufferCtor = ctor

	r.defGetter(proto, "byteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.sharedBufferOf(this, "SharedArrayBuffer.prototype.byteLength")
		if err != nil {
			return Undefined, err
		}
		return Int(len(b.bytes)), nil
	})
	r.defGetter(proto, "growable", func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.sharedBufferOf(this, "SharedArrayBuffer.prototype.growable")
		if err != nil {
			return Undefined, err
		}
		return Bool(b.resizable), nil
	})
	r.defGetter(proto, "maxByteLength", func(rt *Runtime, this Value, args []Value) (Value, error) {
		b, err := rt.sharedBufferOf(this, "SharedArrayBuffer.prototype.maxByteLength")
		if err != nil {
			return Undefined, err
		}
		if b.resizable {
			return Float(float64(b.maxByteLength)), nil
		}
		return Int(len(b.bytes)), nil
	})

	// grow only grows: another agent may be reading the memory a shrink
	// would take away, so a shared buffer never gives any back.
	r.defMethod(proto, "grow", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const name = "SharedArrayBuffer.prototype.grow"
		b, err := rt.sharedBufferOf(this, name)
		if err != nil {
			return Undefined, err
		}
		if !b.resizable {
			return Undefined, rt.throwTypeError("%s requires a growable SharedArrayBuffer", name)
		}
		n, err := rt.toIndex(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if n > b.maxByteLength {
			return Undefined, rt.throwRangeError("the new length exceeds the SharedArrayBuffer's maxByteLength")
		}
		if n < int64(len(b.bytes)) {
			return Undefined, rt.throwRangeError("a SharedArrayBuffer cannot shrink")
		}
		b.resize(int(n))
		return Undefined, nil
	})

	r.defMethod(proto, "slice", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		const name = "SharedArrayBuffer.prototype.slice"
		b, err := rt.sharedBufferOf(this, name)
		if err != nil {
			return Undefined, err
		}
		n := len(b.bytes)
		start, err := rt.relativeIndex(arg(args, 0), n, 0)
		if err != nil {
			return Undefined, err
		}
		end, err := rt.relativeIndex(arg(args, 1), n, n)
		if err != nil {
			return Undefined, err
		}
		count := max(end-start, 0)
		ctor, err := rt.speciesConstructor(this.Object(), rt.sharedArrayBufferCtor)
		if err != nil {
			return Undefined, err
		}
		res, err := rt.construct(ctor, []Value{Int(count)})
		if err != nil {
			return Undefined, err
		}
		if !res.IsObject() || res.Object().class != ClassArrayBuffer {
			return Undefined, rt.throwTypeError("the species did not return a SharedArrayBuffer")
		}
		out, ok := res.Object().data.(*arrayBufferData)
		if !ok || !out.shared {
			return Undefined, rt.throwTypeError("the species did not return a SharedArrayBuffer")
		}
		if res.Object() == this.Object() {
			return Undefined, rt.throwTypeError("the species returned the buffer being sliced")
		}
		if len(out.bytes) < count {
			return Undefined, rt.throwTypeError("the species returned too small a buffer")
		}
		// A shared buffer never shrinks, so the range is still there.
		copy(out.bytes, b.bytes[start:start+count])
		return res, nil
	})

	r.defToStringTag(proto, "SharedArrayBuffer")
}

func (r *Runtime) initAtomicsBuiltins() {
	a := newObject(r.proto.object, ClassObject)
	r.defValue(r.global, "Atomics", Obj(a))
	r.defToStringTag(a, "Atomics")

	// The read-modify-write operations share one shape: validate, convert
	// the value, check the view again, and swap in what the operation makes
	// of the old bits and the new.
	rmw := func(name string, op func(old, v uint64) uint64) {
		r.defMethod(a, name, 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
			t, at, err := rt.atomicAccess(arg(args, 0), arg(args, 1), false, true, false)
			if err != nil {
				return Undefined, err
			}
			_, v, err := rt.atomicOperand(t, arg(args, 2))
			if err != nil {
				return Undefined, err
			}
			if err := rt.revalidateAtomic(t, at); err != nil {
				return Undefined, err
			}
			old := t.getElem(t.atomicIndex(at))
			size := t.info().size
			b := t.storage().bytes[at : at+size]
			writeUint(b, op(readUint(b, true), v), true)
			return old, nil
		})
	}
	rmw("add", func(old, v uint64) uint64 { return old + v })
	rmw("sub", func(old, v uint64) uint64 { return old - v })
	rmw("and", func(old, v uint64) uint64 { return old & v })
	rmw("or", func(old, v uint64) uint64 { return old | v })
	rmw("xor", func(old, v uint64) uint64 { return old ^ v })
	rmw("exchange", func(_, v uint64) uint64 { return v })

	r.defMethod(a, "compareExchange", 4, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, at, err := rt.atomicAccess(arg(args, 0), arg(args, 1), false, true, false)
		if err != nil {
			return Undefined, err
		}
		_, expected, err := rt.atomicOperand(t, arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		_, replacement, err := rt.atomicOperand(t, arg(args, 3))
		if err != nil {
			return Undefined, err
		}
		if err := rt.revalidateAtomic(t, at); err != nil {
			return Undefined, err
		}
		old := t.getElem(t.atomicIndex(at))
		size := t.info().size
		b := t.storage().bytes[at : at+size]
		// The comparison is of the element's bytes, so the expected value is
		// first made what the element would hold: 256 matches a Uint8 zero.
		if readUint(b, true) == expected&sizeMask(size) {
			writeUint(b, replacement, true)
		}
		return old, nil
	})

	r.defMethod(a, "load", 2, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, at, err := rt.atomicAccess(arg(args, 0), arg(args, 1), false, false, false)
		if err != nil {
			return Undefined, err
		}
		if err := rt.revalidateAtomic(t, at); err != nil {
			return Undefined, err
		}
		return t.getElem(t.atomicIndex(at)), nil
	})

	r.defMethod(a, "store", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, at, err := rt.atomicAccess(arg(args, 0), arg(args, 1), false, true, false)
		if err != nil {
			return Undefined, err
		}
		v, bits, err := rt.atomicOperand(t, arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		if err := rt.revalidateAtomic(t, at); err != nil {
			return Undefined, err
		}
		size := t.info().size
		writeUint(t.storage().bytes[at:at+size], bits, true)
		// store answers with the value as converted, not as stored: 300 into
		// a Uint8Array returns 300.
		return v, nil
	})

	r.defMethod(a, "isLockFree", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Bool(n == 1 || n == 2 || n == 4 || n == 8), nil
	})

	r.defMethod(a, "wait", 4, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, at, err := rt.atomicAccess(arg(args, 0), arg(args, 1), true, false, true)
		if err != nil {
			return Undefined, err
		}
		_, v, err := rt.atomicWaitOperand(t, arg(args, 2))
		if err != nil {
			return Undefined, err
		}
		q, err := rt.toNumber(arg(args, 3))
		if err != nil {
			return Undefined, err
		}
		timeout := math.Inf(1)
		if !math.IsNaN(q) {
			timeout = math.Max(q, 0)
		}
		size := t.info().size
		if readUint(t.storage().bytes[at:at+size], true) != v&sizeMask(size) {
			return Str(NewString("not-equal")), nil
		}
		// Nothing else runs on this agent's memory, so nothing can notify
		// it: the wait ends when its time is up, or when the host cancels.
		if err := rt.atomicSleep(timeout); err != nil {
			return Undefined, err
		}
		return Str(NewString("timed-out")), nil
	})

	r.defMethod(a, "notify", 3, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, _, err := rt.atomicAccess(arg(args, 0), arg(args, 1), true, false, false)
		if err != nil {
			return Undefined, err
		}
		if c := arg(args, 2); !c.IsUndefined() {
			if _, err := rt.toInteger(c); err != nil {
				return Undefined, err
			}
		}
		// With one agent, whoever waited has stopped waiting by the time
		// anything can notify it; and a buffer that is not shared cannot be
		// waited on at all.
		_ = t
		return Int(0), nil
	})

	r.defMethod(a, "pause", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The count is a hint of how long to spin, and has to be an integer
		// if it is there at all; with one agent there is nothing to spin for.
		if n := arg(args, 0); !n.IsUndefined() {
			f := n.Number()
			if !n.IsNumber() || math.IsInf(f, 0) || f != math.Trunc(f) {
				return Undefined, rt.throwTypeError("Atomics.pause requires an integral count")
			}
		}
		return Undefined, nil
	})
}

// atomicAccess is ValidateAtomicAccessOnIntegerTypedArray: it checks that the
// view is an integer typed array -- one Atomics.wait can wait on, when
// waitable is set -- that a write has a mutable buffer to go to, that the
// buffer is shared when Atomics.wait needs it to be, and that the index is in
// range, and returns the index's byte offset in the buffer.
func (r *Runtime) atomicAccess(ta, index Value, waitable, write, shared bool) (*typedArrayData, int, error) {
	var t *typedArrayData
	var err error
	if write {
		t, err = r.typedArrayWritable(ta, "Atomics")
	} else {
		t, err = r.typedArrayOf(ta, "Atomics")
	}
	if err != nil {
		return nil, 0, err
	}
	switch t.kind {
	case elemInt32, elemBigInt64:
	case elemInt8, elemUint8, elemInt16, elemUint16, elemUint32, elemBigUint64:
		if waitable {
			return nil, 0, r.throwTypeError("Atomics.wait and Atomics.notify require an Int32Array or a BigInt64Array")
		}
	default:
		return nil, 0, r.throwTypeError("Atomics requires an integer typed array")
	}
	if shared && !t.storage().shared {
		return nil, 0, r.throwTypeError("Atomics.wait requires a view of a SharedArrayBuffer")
	}
	length := t.count()
	i, err := r.toIndex(index)
	if err != nil {
		return nil, 0, err
	}
	if i >= int64(length) {
		return nil, 0, r.throwRangeError("the index is outside the typed array")
	}
	return t, t.byteOffset + int(i)*t.info().size, nil
}

// revalidateAtomic checks the view again once the operands are converted,
// since a conversion can detach or shrink the buffer out from under it.
func (r *Runtime) revalidateAtomic(t *typedArrayData, at int) error {
	if err := r.requireInBounds(t); err != nil {
		return err
	}
	if at >= t.byteOffset+t.count()*t.info().size {
		return r.throwRangeError("the index is outside the typed array")
	}
	return nil
}

// atomicIndex turns a byte offset back into an element index.
func (t *typedArrayData) atomicIndex(at int) int {
	return (at - t.byteOffset) / t.info().size
}

// atomicOperand converts an operand the way the Atomics operations do: to a
// BigInt for a BigInt view, and otherwise to an integer -- not a Number
// wrapped to the element's range, which the write does -- and returns it both
// as a value and as the bits to store.
func (r *Runtime) atomicOperand(t *typedArrayData, v Value) (Value, uint64, error) {
	if t.info().big {
		b, err := r.toBigIntOperand(v)
		if err != nil {
			return Undefined, 0, err
		}
		return Big(b), bigLowUint64(b), nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return Undefined, 0, err
	}
	// Adding zero turns -0 into +0, which is what ToIntegerOrInfinity gives.
	n += 0
	return Float(n), uint64(uint32(toInt32Wrap(n))), nil
}

// atomicWaitOperand converts the value Atomics.wait compares with: ToInt32
// for an Int32Array and ToBigInt64 for a BigInt64Array.
func (r *Runtime) atomicWaitOperand(t *typedArrayData, v Value) (Value, uint64, error) {
	if t.info().big {
		return r.atomicOperand(t, v)
	}
	n, err := r.toNumber(v)
	if err != nil {
		return Undefined, 0, err
	}
	i := toInt32Wrap(n)
	return Int(int(i)), uint64(uint32(i)), nil
}

func sizeMask(size int) uint64 {
	if size >= 8 {
		return math.MaxUint64
	}
	return 1<<(8*size) - 1
}

// atomicSleep blocks for a number of milliseconds, or for ever, returning
// early only when the host's context is done.
func (r *Runtime) atomicSleep(ms float64) error {
	var timer <-chan time.Time
	if !math.IsInf(ms, 1) {
		if ms <= 0 {
			return nil
		}
		t := time.NewTimer(time.Duration(ms * float64(time.Millisecond)))
		defer t.Stop()
		timer = t.C
	}
	var done <-chan struct{}
	if r.ctx != nil {
		done = r.ctx.Done()
	}
	select {
	case <-timer:
		return nil
	case <-done:
		return r.ctx.Err()
	}
}
