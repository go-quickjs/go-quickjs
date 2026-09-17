package vm

// The array a method's result goes into.
//
// slice, map, filter, concat and splice all build a new array, and all five ask
// the receiver what kind of array that should be: a subclass of Array gets back
// an instance of itself rather than a plain array. That is what Symbol.species
// is for, and it means the result cannot simply be built in dense storage --
// the species may be any constructor at all, and its elements have to be
// created through the property protocol so that its own behaviour is honoured.
//
// The plain case is still the common one, so arrayOut keeps both: a fast path
// that appends to the dense slice when the result really is an ordinary Array,
// and the general one otherwise.

// maxArrayLengthValue is the largest length an Array may have. It is one more
// than the largest array index, which is the value length may hold without any
// index being able to name it.
const maxArrayLengthValue = 1<<32 - 1

// arrayCreate builds a new Array of a given length.
//
// A length beyond what a length may hold is a RangeError rather than an
// enormous allocation: a method asked to produce one has to say so before it
// starts copying, not after it has exhausted memory trying.
func (r *Runtime) arrayCreate(n int64) (*Object, error) {
	if n < 0 || n > maxArrayLengthValue {
		return nil, r.throwError(errRange, "invalid array length")
	}
	o := r.newArrayOfLength(n)
	return o, nil
}

// arrayOut is the result array a method fills.
type arrayOut struct {
	o *Object
	// plain marks a result that is an ordinary Array with nothing between it
	// and its dense storage, so that elements can be appended directly.
	plain bool
	// n is how many elements have been placed, which the general path needs
	// because it addresses by index rather than appending.
	n int64
}

// arraySpeciesCreate makes the array a method's result goes into.
func (r *Runtime) arraySpeciesCreate(orig Value, n int64) (*arrayOut, error) {
	if !orig.IsObject() || !orig.Object().IsArray() {
		o, err := r.arrayCreate(n)
		if err != nil {
			return nil, err
		}
		return &arrayOut{o: o, plain: true}, nil
	}

	c, err := r.getValueProp(orig, atomConstructor)
	if err != nil {
		return nil, err
	}
	if c.IsObject() {
		sp, err := r.getValueProp(c, r.atoms.internSymbol(r.wellKnown.species))
		if err != nil {
			return nil, err
		}
		// Null means "a plain array", which is how a subclass opts out of
		// having its methods return instances of itself.
		if sp.IsNull() {
			c = Undefined
		} else {
			c = sp
		}
	}
	if c.IsUndefined() || (c.IsObject() && c.Object() == r.proto.arrayCtor) {
		o, err := r.arrayCreate(n)
		if err != nil {
			return nil, err
		}
		return &arrayOut{o: o, plain: true}, nil
	}
	if !isConstructor(c) {
		return nil, r.throwTypeError("the array species is not a constructor")
	}
	res, err := r.construct(c, []Value{Float(float64(n))})
	if err != nil {
		return nil, err
	}
	o, err := r.toObject(res)
	if err != nil {
		return nil, err
	}
	return &arrayOut{o: o}, nil
}

// push appends one element.
func (a *arrayOut) push(r *Runtime, v Value) error {
	if a.plain {
		a.o.elems = append(a.o.elems, v)
		a.n++
		return nil
	}
	i := a.n
	a.n++
	return r.defineOwnProp(a.o, r.indexKey(i), v, propDefault)
}

// pushHole leaves a gap, which slice and map do where the source had one: a
// hole is not the same as an undefined element, and copying it as one would be
// visible to `in` and to every method that skips holes.
func (a *arrayOut) pushHole(r *Runtime) error {
	if a.plain {
		a.o.elems = append(a.o.elems, elemHole)
		a.n++
		return nil
	}
	a.n++
	return nil
}

// setLength fixes the result's length, which matters when holes at the end mean
// no property was ever created for the last index.
func (a *arrayOut) setLength(r *Runtime, n int64) error {
	if a.plain {
		if int64(len(a.o.elems)) != n {
			a.o.setArrayLength(uint32(n))
		}
		return nil
	}
	return r.setValueProp(Obj(a.o), atomLength, Float(float64(n)), true)
}

// value is the finished array.
func (a *arrayOut) value() Value { return Obj(a.o) }

// isConcatSpreadable decides whether concat flattens an argument or appends it
// whole.
//
// An array is spread, and anything at all may say it should be by defining
// Symbol.isConcatSpreadable -- which is how an array-like joins in, and how an
// array opts out.
func (r *Runtime) isConcatSpreadable(v Value) (bool, error) {
	if !v.IsObject() {
		return false, nil
	}
	flag, err := r.getValueProp(v, r.atoms.internSymbol(r.wellKnown.isConcatSpreadable))
	if err != nil {
		return false, err
	}
	if !flag.IsUndefined() {
		return flag.Truthy(), nil
	}
	return v.Object().IsArray(), nil
}
