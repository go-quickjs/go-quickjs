package vm

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// A typed array's indices are not properties.
//
// An element of a typed array lives in a buffer, and the language gives the
// object a set of internal methods that say so: an index is present only while
// it is in range, cannot be deleted, cannot be turned into an accessor or made
// read-only, and a write past the end is dropped rather than added. All of that
// falls out of one rule -- the object's storage is the buffer and nothing else
// -- and it is what makes a typed array a fixed-length window rather than an
// array that happens to hold numbers.
//
// The awkward part is deciding what counts as an index. It is not "a string of
// digits": the specification asks whether the key is a *canonical numeric index
// string*, meaning ToString(ToNumber(key)) is the key again. So "1.5", "NaN",
// "1e21" and "-0" are all numeric keys -- none of them valid -- while "01" and
// " 1" are ordinary property names. The difference is observable: writing to
// a["-0"] does nothing, while writing to a["01"] adds a property.

// numericIndex is what a key means to a typed array.
type numericIndex struct {
	// numeric is set when the key is a canonical numeric index string, which
	// is what makes it the buffer's business rather than the property table's.
	numeric bool
	// valid is set when it also names an element that is there.
	valid bool
	// integral is set when the key is a non-negative integer, which an
	// element could have even though none is there at the moment.
	integral bool
	i        int
}

// typedElemIndex is the element a number key names in a typed array, for the
// reads and writes that need no property key. A typed array answers every
// integer index from its buffer and never from its prototype, so the number
// goes straight to the element. Anything else -- a fraction, a negative
// number, one past what an int holds -- takes the long way.
func typedElemIndex(o *Object, key float64) (*typedArrayData, int, bool) {
	if o.class != ClassTypedArray {
		return nil, 0, false
	}
	i := uint32(key)
	if float64(i) != key || i > math.MaxInt32 {
		return nil, 0, false
	}
	t, ok := o.dataOf().(*typedArrayData)
	return t, int(i), ok
}

// typedWrite stores the number v in element i of a typed array of a fixed
// length that is inside its buffer, straight into the buffer, or reports
// false where setElem has to: a view that tracks its buffer's length, one
// whose buffer has been detached or has shrunk past its end, an index past
// its own end, a value that is not a number, a BigInt or Float16 element, an
// immutable buffer. It asks the object's data for the view, which is the
// class check too. A number
// converts without running code, so nothing can change the view between the
// check and the store.
func typedWrite(o *Object, i uint32, v Value) bool {
	return typedStore(Obj(o), float64(i), v)
}

// typedStore is typedWrite of key, a number, into obj, any value: false for
// what is no typed array, or a key that is no index, its checks its own as
// typedRead's are.
func typedStore(obj Value, key float64, v Value) bool {
	if !obj.IsObject() {
		return false
	}
	o := obj.object()
	if o.class != ClassTypedArray {
		return false
	}
	i := uint32(key)
	if float64(i) != key {
		return false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || uint(i) >= uint(t.fixedLength) || !v.IsNumber() {
		return false
	}
	st := t.st
	if st == nil {
		if st = t.fixedStorage(); st == nil {
			return false
		}
	}
	if t.end > len(st.bytes) || st.immutable {
		return false
	}
	// The offset is an int, shifted as one: below fixedLength, the index
	// fits, and a uint32 shifted would wrap past 4 GiB.
	b, n, j := st.bytes[t.byteOffset:t.end], v.num, int(i)
	switch t.kind {
	case elemInt8, elemUint8:
		b[j] = byte(toInt32Wrap(n))
	case elemUint8Clamped:
		b[j] = clampUint8(n)
	case elemInt16, elemUint16:
		binary.LittleEndian.PutUint16(b[j<<1:], uint16(toInt32Wrap(n)))
	case elemInt32, elemUint32:
		binary.LittleEndian.PutUint32(b[j<<2:], uint32(toInt32Wrap(n)))
	case elemFloat32:
		binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(n)))
	case elemFloat64:
		binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(n))
	default:
		return false
	}
	return true
}

// typedRead is element key of obj, a typed array of a fixed length that is
// inside its buffer, read straight from the buffer as typedStore stores
// there, or false where getElem has to -- a view that tracks its buffer's
// length, one whose buffer has been detached or has shrunk past its end, a
// key that is no index below its length, a BigInt or Float16 element --
// or for any other value. Its every check is its own, so that the tree
// tier's nodes, which ask it where elemAt has found no element, make one
// call for it: the inliner gives a node of the tree builder's none of the
// calls it would inline elsewhere.
// Through typedElemIndex and getElem, a read took two calls, two type
// assertions and the buffer's storage looked up anew each time: Mandreel's
// heap, and the Game Boy's memory, are read so at every turn.
func typedRead(obj Value, key float64) (Value, bool) {
	if !obj.IsObject() {
		return Undefined, false
	}
	o := obj.object()
	if o.class != ClassTypedArray {
		return Undefined, false
	}
	t, ok := o.dataOf().(*typedArrayData)
	if !ok {
		return Undefined, false
	}
	i := uint32(key)
	if float64(i) != key || uint(i) >= uint(t.fixedLength) {
		return Undefined, false
	}
	st := t.st
	if st == nil {
		if st = t.fixedStorage(); st == nil {
			return Undefined, false
		}
	}
	if t.end > len(st.bytes) {
		return Undefined, false
	}
	// Below fixedLength, the index fits an int, shifted as one.
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

// fixedStorage fills in st and end for a view of a fixed length, and gives
// st, or nil for a view that tracks its buffer's length. It is out of line,
// as what typedWrite does once per view.
//
//go:noinline
func (t *typedArrayData) fixedStorage() *arrayBufferData {
	st, _ := t.buffer.dataOf().(*arrayBufferData)
	if st == nil || t.tracking {
		return nil
	}
	t.st, t.end = st, t.byteOffset+t.fixedLength*elemInfos[t.kind].size
	return st
}

// typedArrayIndex classifies a key against a typed array.
func (r *Runtime) typedArrayIndex(o *Object, key Atom) numericIndex {
	t, ok := o.dataOf().(*typedArrayData)
	if !ok {
		return numericIndex{}
	}
	if key.IsIndex() {
		i := int(key.Index())
		return numericIndex{numeric: true, valid: i < t.count(), integral: true, i: i}
	}
	if r.atoms.IsSymbol(key) {
		return numericIndex{}
	}
	name := r.atoms.name(key)
	f, ok := canonicalNumericIndex(name)
	if !ok {
		return numericIndex{}
	}
	// A numeric key that is not a non-negative integer in range names no
	// element, but it is still the buffer's business: the write is dropped
	// rather than becoming a property.
	integral := f == math.Trunc(f) && !math.Signbit(f) && f >= 0 && f <= maxTypedArrayLength
	if !integral {
		return numericIndex{numeric: true}
	}
	i := int(f)
	return numericIndex{numeric: true, valid: i < t.count(), integral: true, i: i}
}

// canPreventExtensions reports whether an ordinary object, not a proxy, can be
// made non-extensible. Only a typed array whose length can change cannot -- one
// tracking its buffer's length, or any view over a resizable buffer -- since it
// could gain elements later, and so refuses to promise it will not. A growable
// shared buffer only grows, so a view of a fixed length over one keeps it.
func canPreventExtensions(o *Object) bool {
	t, ok := o.dataOf().(*typedArrayData)
	if !ok || o.class != ClassTypedArray {
		return true
	}
	b := t.storage()
	return !t.tracking && !(b.resizable && !b.shared)
}

// typedArrayImmutable reports whether a typed array views an immutable buffer.
func typedArrayImmutable(o *Object) bool {
	t, ok := o.dataOf().(*typedArrayData)
	return ok && t.storage().immutable
}

// canonicalNumericIndex reports whether a name is a canonical numeric index
// string, meaning it is what printing its own numeric value produces.
func canonicalNumericIndex(name string) (float64, bool) {
	if name == "-0" {
		return math.Copysign(0, -1), true
	}
	if name == "" {
		return 0, false
	}
	// A cheap rejection first: no canonical numeric string starts with
	// anything else, and almost every property name fails here.
	switch c := name[0]; {
	case c >= '0' && c <= '9', c == '-', c == 'N', c == 'I':
	default:
		return 0, false
	}
	f, err := strconv.ParseFloat(name, 64)
	if err != nil {
		return 0, false
	}
	if jsnum.FormatFloat(f) != name {
		return 0, false
	}
	return f, true
}

// typedArrayDefine implements [[DefineOwnProperty]] for a numeric key.
//
// The attributes a typed array's elements have are fixed, so a descriptor that
// asks for different ones is refused rather than quietly ignored: there is no
// way to store the answer, and pretending otherwise would let a script believe
// it had frozen an element.
func (r *Runtime) typedArrayDefine(o *Object, ix numericIndex, d *propDesc) (bool, error) {
	// Every refusal here is reported rather than thrown: an element is a slot
	// in a buffer, and a descriptor that does not describe one is simply not
	// applicable. Object.defineProperty turns the refusal into a TypeError and
	// Reflect.defineProperty answers false, as they do for any other.
	if !ix.valid {
		// An index outside the array names nothing, and nothing is where a
		// property cannot be added.
		return false, nil
	}
	t := o.dataOf().(*typedArrayData)
	if t.storage().immutable {
		// An element of an immutable buffer is a non-writable,
		// non-configurable property, so a define succeeds only by asking for
		// nothing it does not already have -- the value included, compared as
		// it is rather than converted.
		switch {
		case d.isAccessor(),
			d.hasConfigurable && d.configurable,
			d.hasEnumerable && !d.enumerable,
			d.hasWritable && d.writable,
			d.hasValue && !d.value.SameValue(t.getElem(ix.i)):
			return false, nil
		}
		return true, nil
	}
	switch {
	case d.isAccessor(),
		d.hasConfigurable && !d.configurable,
		d.hasEnumerable && !d.enumerable,
		d.hasWritable && !d.writable:
		// An element is a writable, enumerable, configurable data property,
		// and it cannot be made into anything else.
		return false, nil
	}
	if !d.hasValue {
		return true, nil
	}
	if err := r.setElem(t, ix.i, d.value); err != nil {
		return false, err
	}
	return true, nil
}
