package vm

import "math"

// The array-like view.
//
// Nearly every Array.prototype method is generic: it works on anything with a
// length and numeric keys. That is what lets
// Array.prototype.map.call(arguments, f) work, what makes the methods usable on
// a NodeList or a jQuery object, and what most of test262 exercises.
//
// Reading the dense element slice directly skips all of it. A plain object with
// a length looks empty; a hole does not fall through to the prototype, so
// Array.prototype[1] = "x" is invisible to join; and an index redefined as an
// accessor is never called.
//
// So reads go through the property protocol, with a fast path for the case that
// dominates: a dense array whose element really is there. A dense element
// shadows anything on the prototype and cannot be an accessor -- defining one
// vacates the slot -- so taking it directly is not observable.

// maxArrayLength is the largest length an array-like may report. It is one less
// than 2**53, the point past which consecutive integers stop being
// representable, so an index beyond it could not be distinguished from its
// neighbour.
const maxArrayLength = 1<<53 - 1

// arrayLike is a receiver an Array method operates on.
type arrayLike struct {
	o *Object
	// n is the length read once at the start, as the specification requires:
	// an element the callback appends is not visited, and one it removes is
	// skipped. Re-reading it each step would also let a callback that pushes
	// loop forever.
	n int64
}

// viewArrayLike coerces a receiver and reads its length.
func (r *Runtime) viewArrayLike(this Value) (*arrayLike, error) {
	o, err := r.toObject(this)
	if err != nil {
		return nil, err
	}
	n, err := r.lengthOf(o)
	if err != nil {
		return nil, err
	}
	return &arrayLike{o: o, n: n}, nil
}

// lengthOf reads an object's length as the specification's ToLength does:
// truncated towards zero and clamped to the representable range, so a negative
// or absent length is simply zero rather than an error.
func (r *Runtime) lengthOf(o *Object) (int64, error) {
	if o.class == ClassArray {
		return int64(o.arrayLength()), nil
	}
	v, err := r.getProp(o, atomLength, Obj(o))
	if err != nil {
		return 0, err
	}
	f, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	switch {
	case math.IsNaN(f) || f <= 0:
		return 0, nil
	case f > maxArrayLength:
		return maxArrayLength, nil
	}
	return int64(f), nil
}

// indexKey returns the property key for an integer index.
//
// Beyond 2**32-1 an index is no longer an array index and becomes an ordinary
// string key, which only an array-like with a length that large can reach.
func (r *Runtime) indexKey(i int64) Atom {
	if i >= 0 && i < 1<<32-1 {
		return r.atoms.indexAtom(uint32(i))
	}
	return r.atoms.intern(formatIndex(i))
}

func formatIndex(i int64) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// has reports whether the index is present, which is what distinguishes a hole
// from an element holding undefined.
func (a *arrayLike) has(r *Runtime, i int64) (bool, error) {
	if _, fast := a.dense(i); fast {
		return true, nil
	}
	return r.hasPropErr(a.o, r.indexKey(i))
}

// at reads the index, reporting whether it was present at all.
func (a *arrayLike) at(r *Runtime, i int64) (Value, bool, error) {
	if v, fast := a.dense(i); fast {
		return v, true, nil
	}
	// An array-like may report a length of 2**53-1 while holding nothing, and
	// walking it is then a loop the interpreter never sees. The check belongs
	// on the slow path only: the dense case above is bounded by the array's
	// actual storage.
	if err := r.tick(); err != nil {
		return Undefined, false, err
	}
	present, err := a.has(r, i)
	if err != nil || !present {
		return Undefined, false, err
	}
	v, err := r.getProp(a.o, r.indexKey(i), Obj(a.o))
	return v, true, err
}

// get reads the index, treating a missing one as undefined. Methods that do not
// distinguish holes use this and avoid the extra HasProperty call.
func (a *arrayLike) get(r *Runtime, i int64) (Value, error) {
	if v, fast := a.dense(i); fast {
		return v, nil
	}
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	return r.getProp(a.o, r.indexKey(i), Obj(a.o))
}

// set writes the index.
func (a *arrayLike) set(r *Runtime, i int64, v Value) error {
	_, err := r.setProp(a.o, r.indexKey(i), v, Obj(a.o), true)
	return err
}

// remove deletes the index, which is how a method leaves a hole behind.
func (a *arrayLike) remove(r *Runtime, i int64) error {
	_, err := r.deleteProp(a.o, r.indexKey(i), true)
	return err
}

// put writes a value, or deletes the index when the source position was a hole.
func (a *arrayLike) put(r *Runtime, i int64, v Value, present bool) error {
	if !present {
		return a.remove(r, i)
	}
	return a.set(r, i, v)
}

// setLength writes the length, which a method that shortens or grows the
// receiver must do explicitly on anything that is not a real array.
func (a *arrayLike) setLength(r *Runtime, n int64) error {
	_, err := r.setProp(a.o, atomLength, Float(float64(n)), Obj(a.o), true)
	return err
}

// dense returns the element at i when it can be read without consulting the
// property protocol.
//
// The conditions are that the receiver is a real array, the index is inside its
// dense storage, and the slot is not a hole. A hole has to fall through to the
// prototype chain, and an index that was redefined with attributes or as an
// accessor has already been moved out of dense storage, so anything still here
// is a plain data property that shadows whatever the prototype holds.
func (a *arrayLike) dense(i int64) (Value, bool) {
	if a.o.class != ClassArray || i < 0 || i >= int64(len(a.o.elems)) {
		return Undefined, false
	}
	v := a.o.elems[i]
	if isHole(v) {
		return Undefined, false
	}
	return v, true
}

// newArrayOfLength builds an array to hold a method's result.
func (r *Runtime) newArrayOfLength(n int64) *Object {
	o := newObject(r.proto.array, ClassArray)
	// Only a modest length is preallocated. An array-like may report a length
	// of 2**53-1 while holding nothing, and reserving for that would exhaust
	// memory before the first element is read; append grows the rest.
	if n > 0 && n <= 1<<16 {
		o.elems = make([]Value, 0, n)
	}
	return o
}

// relativeIndex64 resolves an index argument that may be negative, as slice and
// its relatives accept, against a length that may exceed an int.
func (r *Runtime) relativeIndex64(v Value, length, def int64) (int64, error) {
	if v.IsUndefined() {
		return def, nil
	}
	n, err := r.toInteger(v)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		n += float64(length)
	}
	switch {
	case n < 0:
		return 0, nil
	case n > float64(length):
		return length, nil
	}
	return int64(n), nil
}
