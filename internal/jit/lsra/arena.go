package lsra

// arena hands out slices of one buffer, which an allocation empties for the
// next: the many small lists an allocation makes -- ranges, members, hints
// -- cost no allocation of their own once the buffer has grown to a
// function's size. A slice it gives has its capacity cut at its end, so
// that an append past it moves it (appendA) rather than writing over
// another's.
type arena[T any] struct{ buf []T }

// take returns an empty slice of capacity n.
func (a *arena[T]) take(n int) []T {
	if len(a.buf)+n > cap(a.buf) {
		// The slices given out keep the old buffer; the next allocation
		// begins in this one.
		a.buf = make([]T, 0, max(2*cap(a.buf), n, 256))
	}
	i := len(a.buf)
	a.buf = a.buf[:i+n]
	return a.buf[i : i : i+n]
}

// reset empties the arena for the next allocation.
func (a *arena[T]) reset() { a.buf = a.buf[:0] }

// appendA appends xs to s, moving it within the arena where it is full.
func appendA[T any](a *arena[T], s []T, xs ...T) []T {
	if len(s)+len(xs) > cap(s) {
		ns := a.take(max(2*cap(s), len(s)+len(xs), 4))
		s = append(ns, s...)
	}
	return append(s, xs...)
}

// chunks hands out values of a type one at a time from chunks it keeps,
// which an allocation empties for the next: their addresses stay put.
type chunks[T any] struct {
	bufs [][]T
	i, n int
}

// new returns a zeroed value.
func (c *chunks[T]) new() *T {
	if c.i < len(c.bufs) && c.n == len(c.bufs[c.i]) {
		c.i, c.n = c.i+1, 0
	}
	if c.i == len(c.bufs) {
		c.bufs = append(c.bufs, make([]T, 256))
	}
	p := &c.bufs[c.i][c.n]
	c.n++
	var zero T
	*p = zero
	return p
}

// reset empties the chunks for the next allocation.
func (c *chunks[T]) reset() { c.i, c.n = 0, 0 }
