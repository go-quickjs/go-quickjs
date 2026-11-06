package vm

// An array's growth. Go's append eases from doubling a slice to adding a
// quarter, and past a few thousand elements what a large array built by
// push or by a[a.length] = x allocates on the way is up to five and a half
// times its size, where half again's is about three. Which is better for
// a given length depends on where it falls among the capacities each passes
// through: an array of 8,192 elements fits one of Go's closely, and half
// again from 1,024 allocates 14% more for it, so array_push_large and
// array_push_args3 ran 10 to 20% slower. Over every length from 16,384 up,
// half again allocates a median of 27% less, and at worst 12% more, and below
// that, where most arrays are, growth is Go's.
//
// V8 grows by half and 16 at any length. Its elements are copied by its
// own collector's rules; here a make and a copy would take the whole write
// barrier while the collector marks, where Go's growth takes the one for a
// new slice, so the room is asked of append.
//
// Its code is generic, instantiated with struct{}, as zzz_zliteral.go's.

// growElemsAt is the capacity from which an array grows by half.
const growElemsAt = 16384

// growElems is els, an array's elements, with room for more, its length as
// it is.
//
//go:noinline
func growElems[_ any](els []Value, more int) []Value {
	if c := cap(els); c >= growElemsAt {
		more = max(more, c+c>>1+16-len(els))
	}
	return append(els[:len(els):len(els)], make([]Value, more)...)[:len(els)]
}

// appendGrown is v appended to o's elements, which have no room: what
// appendElem makes out of line, the rest of it a store and a length.
//
//go:noinline
func appendGrown[_ any](o *Object, v Value) {
	e := o.extOf()
	e.elems = append(growElems[struct{}](e.elems, 1), v)
}
