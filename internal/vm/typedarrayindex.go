package vm

import (
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
	i     int
}

// typedArrayIndex classifies a key against a typed array.
func (r *Runtime) typedArrayIndex(o *Object, key Atom) numericIndex {
	t, ok := o.data.(*typedArrayData)
	if !ok {
		return numericIndex{}
	}
	if key.IsIndex() {
		i := int(key.Index())
		return numericIndex{numeric: true, valid: !t.storage().detached && i < t.length, i: i}
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
	i := int(f)
	inRange := f == math.Trunc(f) && !math.Signbit(f) && f >= 0 &&
		f < float64(t.length) && !t.storage().detached
	return numericIndex{numeric: true, valid: inRange, i: i}
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
	if !ix.valid {
		return false, r.throwTypeError("cannot define a property outside a typed array")
	}
	switch {
	case d.isAccessor():
		return false, r.throwTypeError("a typed array element cannot be an accessor")
	case d.hasConfigurable && !d.configurable:
		return false, r.throwTypeError("a typed array element is always configurable")
	case d.hasEnumerable && !d.enumerable:
		return false, r.throwTypeError("a typed array element is always enumerable")
	case d.hasWritable && !d.writable:
		return false, r.throwTypeError("a typed array element is always writable")
	}
	if !d.hasValue {
		return true, nil
	}
	t := o.data.(*typedArrayData)
	if err := r.setElem(t, ix.i, d.value); err != nil {
		return false, err
	}
	return true, nil
}
