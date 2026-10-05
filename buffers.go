package quickjs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"
	"unsafe"
)

// BufferMode says whether an ArrayBuffer made from Go memory holds a copy of
// it or the memory itself. NewArrayBuffer and NewTypedArray take one, so that
// every call says which it is.
type BufferMode int

const (
	// CopyMemory gives the buffer bytes of its own, a copy of the slice's:
	// the host may change the slice, or let it go, as it likes, and what the
	// script writes stays the script's.
	CopyMemory BufferMode = iota

	// ShareMemory makes the buffer the slice's memory itself, which the host
	// and the script then both see -- what one writes, the other reads --
	// with nothing copied, however large it is.
	//
	// The host touches the slice only on the runtime's goroutine, or while
	// no script is running, as it does any of the runtime's values; a slice
	// that grows past its capacity leaves the buffer on the old memory. The
	// script may detach the buffer, by transferring it, which leaves the
	// slice as it is; but it cannot send it to a worker by transferring it --
	// that is a DataCloneError -- since that would share the host's memory
	// with another goroutine. Cloned, it is copied.
	ShareMemory

	// ShareMemoryReadOnly shares the memory as an immutable ArrayBuffer: the
	// script reads it, and cannot write it, detach it or transfer it. The
	// host must not change it either while the script can see it, which is
	// what an immutable buffer promises.
	ShareMemoryReadOnly
)

// NewArrayBuffer returns an ArrayBuffer of b's bytes, copied or shared as
// mode says.
func (r *Runtime) NewArrayBuffer(b []byte, mode BufferMode) Value {
	if r.closed {
		return Value{}
	}
	shared := mode == ShareMemory || mode == ShareMemoryReadOnly
	if !shared {
		b = bytes.Clone(b)
	}
	return Value{v: r.rt.NewArrayBufferOf(b, shared, mode == ShareMemoryReadOnly), rt: r.rt}
}

// TypedArrayElement is the element type of a slice NewTypedArray makes a
// typed array of: int8 an Int8Array, uint8 (byte) a Uint8Array, int16,
// uint16, int32 and uint32 their arrays, int64 a BigInt64Array, uint64 a
// BigUint64Array, and float32 and float64 a Float32Array and a Float64Array
// -- or a type of its own whose underlying type is one of them.
type TypedArrayElement interface {
	~int8 | ~uint8 | ~int16 | ~uint16 | ~int32 | ~uint32 | ~int64 | ~uint64 | ~float32 | ~float64
}

// errSharedBigEndian is why a typed array cannot share wide elements on a
// big-endian machine.
var errSharedBigEndian = errors.New("quickjs: a typed array cannot share the memory of elements wider than a byte on a big-endian machine, as it reads them little-endian")

// littleEndian is whether the machine stores a word's low byte first, which
// is how a typed array reads its elements.
var littleEndian = binary.NativeEndian.Uint16([]byte{1, 0}) == 1

// NewTypedArray returns the typed array of s's element type -- a Float64Array
// for a []float64 -- over s's elements, copied or shared as mode says.
//
// A typed array reads its elements little-endian. On a big-endian machine a
// copy is put in that order, and memory of elements wider than a byte cannot
// be shared: that is an error.
func NewTypedArray[T TypedArrayElement](r *Runtime, s []T, mode BufferMode) (Value, error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	var zero T
	size := int(unsafe.Sizeof(zero))
	var b []byte
	if len(s) > 0 {
		b = unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), len(s)*size)
	}
	shared := mode == ShareMemory || mode == ShareMemoryReadOnly
	switch {
	case shared && size > 1 && !littleEndian:
		return Value{}, errSharedBigEndian
	case !shared:
		b = bytes.Clone(b)
		if size > 1 && !littleEndian {
			for i := 0; i < len(b); i += size {
				slices.Reverse(b[i : i+size])
			}
		}
	}
	buf := r.rt.NewArrayBufferOf(b, shared, mode == ShareMemoryReadOnly)
	v, err := r.rt.NewTypedArrayOf(typedArrayName(reflect.TypeFor[T]().Kind()), buf)
	if err != nil {
		return Value{}, err
	}
	return Value{v: v, rt: r.rt}, nil
}

// typedArrayName is the typed array of an element kind.
func typedArrayName(k reflect.Kind) string {
	switch k {
	case reflect.Int8:
		return "Int8Array"
	case reflect.Uint8:
		return "Uint8Array"
	case reflect.Int16:
		return "Int16Array"
	case reflect.Uint16:
		return "Uint16Array"
	case reflect.Int32:
		return "Int32Array"
	case reflect.Uint32:
		return "Uint32Array"
	case reflect.Int64:
		return "BigInt64Array"
	case reflect.Uint64:
		return "BigUint64Array"
	case reflect.Float32:
		return "Float32Array"
	}
	return "Float64Array"
}
