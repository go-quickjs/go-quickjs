package vm

// The view a typed array method's result goes into.
//
// slice, map, filter and subarray all produce a new view, and all four ask the
// receiver what kind it should be, exactly as the Array methods do. The
// difference is that the answer is checked afterwards as well: a species may
// return any object at all, and a method that then wrote elements into it would
// be writing into something that is not a view.
//
// The content type is checked too. A BigInt64Array's elements are BigInts and a
// Float64Array's are Numbers, and neither can hold the other's, so a species
// that crosses the two is refused rather than left to fail element by element.

// typedArrayCtorFor returns the intrinsic constructor for an element type,
// which is the default a species lookup falls back to.
func (r *Runtime) typedArrayCtorFor(kind elemType) *Object {
	p := r.typedArrayProtos[kind]
	if p == nil {
		return nil
	}
	if c := p.getOwn(atomConstructor); c != nil && c.value.IsObject() {
		return c.value.Object()
	}
	return nil
}

// typedArraySpeciesCreate builds the result a method returns.
//
// args is what the constructor is called with: a single length for slice, map
// and filter, and a buffer, offset and length for subarray.
func (r *Runtime) typedArraySpeciesCreate(this Value, t *typedArrayData,
	args []Value) (Value, *typedArrayData, error) {
	def := r.typedArrayCtorFor(t.kind)
	ctor, err := r.speciesConstructor(this.Object(), def)
	if err != nil {
		return Undefined, nil, err
	}

	res, err := r.construct(ctor, args)
	if err != nil {
		return Undefined, nil, err
	}
	nt, err := r.typedArrayOf(res, "the typed array species")
	if err != nil {
		return Undefined, nil, err
	}
	if nt.storage().detached {
		return Undefined, nil, r.throwTypeError("the typed array species is detached")
	}
	// A length was asked for, so a shorter result is one the method could not
	// fill.
	if len(args) == 1 && args[0].IsNumber() && float64(nt.length) < args[0].Number() {
		return Undefined, nil, r.throwTypeError("the typed array species is too short")
	}
	if elemInfos[nt.kind].big != elemInfos[t.kind].big {
		return Undefined, nil, r.throwTypeError(
			"a BigInt typed array and a Number one cannot stand in for each other")
	}
	return res, nt, nil
}

// newTypedArrayLike builds a view of the same kind as an existing one, through
// whatever species it names.
func (r *Runtime) newTypedArrayLike(this Value, t *typedArrayData, n int) (Value, *typedArrayData, error) {
	return r.typedArraySpeciesCreate(this, t, []Value{Int(n)})
}

// fillTypedArrayLike builds the result of map or filter: a view of the
// receiver's species holding the values collected.
func (r *Runtime) fillTypedArrayLike(this Value, t *typedArrayData, vals []Value) (Value, error) {
	res, nt, err := r.newTypedArrayLike(this, t, len(vals))
	if err != nil {
		return Undefined, err
	}
	for i, v := range vals {
		if i >= nt.length {
			break
		}
		if err := r.setElem(nt, i, v); err != nil {
			return Undefined, err
		}
	}
	return res, nil
}
