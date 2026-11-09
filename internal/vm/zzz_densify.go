package vm

import (
	"math"
	"slices"
)

// A sparse array filled in.
//
//	a = []
//	for (i = 4095; i >= 0; i--) a[i] = i
//
//	b = new Array(100000)
//	for (i = 0; i < b.length; i++) b[i] = i
//
// a[4095] is too far past a's length for the holes to be written in
// (setElem's run of 1024), so it went to the property table, the array
// sparse (noteArrayIndex), and so did every index down to 1024, where the
// holes below were written and dense storage took the rest. b's length was
// too long for its holes (setArrayLength). Nothing made either dense again:
// every read of a[1024..4095] was a search of the table, 80 ns where an
// element is 4, and every write to b, sparse, took the slow path, 120 ns
// where a dense array's is 40, after b was full. So as a sparse array's
// dense storage grows, once its elements, dense and in the table, are at
// least half its length and the table's indices are all plain elements --
// data, writable, enumerable and configurable -- they move back into dense
// storage, the rest of its length holes, and the array is dense again
// (objHasSparseElements cleared), as V8 makes a dictionary's elements fast
// again once they are dense enough (ShouldConvertToFastElements). The idea
// is from a port of QuickJS that measured Octane's Crypto by it
// (github.com/bellard/quickjs/issues/602), whose arrays here never go
// sparse: a write past the end writes a run of up to 1024 holes in.
//
// An array the table holds an index of with other attributes stays sparse,
// and so does one that is a prototype, or whose layout has a side: its
// layout is what caches and its objects know it by. One not dense enough,
// or not so made, is looked at again once its dense storage has doubled
// (densifyAt), so that growing it element by element looks at its table a
// logarithmic number of times.
//
// Its function is generic, instantiated with struct{} alone: Go lays a
// generic function's instantiations out after every function and closure of
// the package (see zzz_direct.go). setElem asks it as a sparse array's dense
// storage grows, within the padding that rounds it to 32 bytes.

// densify makes o, an object whose dense storage setElem has just grown
// past a run of holes or while it was sparse, dense again, where it is a
// sparse array that may be (see above).
//
//go:noinline
func densify[_ any](o *Object) {
	if o.flags&(objHasSparseElements|objExtensible|objArrayLengthWritable|objProtoMet|objSharedElems|objInlineProps) !=
		objHasSparseElements|objExtensible|objArrayLengthWritable || o.class != ClassArray || o.shape == nil || o.shape.side != nil {
		return
	}
	ext := o.arrayExt()
	// A sparse array's length is its recorded one or its dense storage's,
	// whichever is longer (arrayLength).
	n := uint64(len(ext.elems))
	length := max(uint64(ext.arrayLen), n)
	if n < uint64(ext.densifyAt) || (n+uint64(o.valn))*2 < length {
		return
	}
	keys, vals := o.keys(), o.vals()
	moved, kept := uint64(0), false
	for _, k := range keys {
		switch {
		case k.flags&propDeleted != 0 || !k.key.IsIndex():
		case k.flags == propDefault && uint64(k.key.Index()) < length:
			moved++
		default:
			kept = true
		}
	}
	if kept || (n+moved)*2 < length || length >= atomIndexTag || length > math.MaxInt {
		// Not now: looked at again once its dense storage has doubled.
		ext.densifyAt = uint32(min(2*n, math.MaxUint32))
		return
	}
	e := slices.Grow(ext.elems, int(length-n))[:length]
	for i := n; i < length; i++ {
		e[i] = elemHole
	}
	if moved > 0 {
		rest := make([]Property, 0, len(keys)-int(moved))
		for i, k := range keys {
			switch {
			case k.flags&propDeleted != 0:
			case k.key.IsIndex():
				e[k.key.Index()] = vals[i]
			default:
				rest = append(rest, Property{key: k.key, flags: k.flags, value: vals[i]})
			}
		}
		proto := o.shape.proto
		clear(vals)
		o.setVals(make([]Value, 0, len(rest)))
		o.shape = rootFor(ClassArray, proto)
		for _, p := range rest {
			o.appendProp(p)
		}
	}
	ext.elems, ext.densifyAt = e, 0
	o.flags &^= objHasSparseElements
	if verifyShapes {
		checkShape(o)
	}
}
