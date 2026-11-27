package lsra

import "unsafe"

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
		a.buf = make([]T, 0, max(2*cap(a.buf), n, firstLen[T]()))
	}
	i := len(a.buf)
	a.buf = a.buf[:i+n]
	return a.buf[i : i : i+n]
}

// reset empties the arena for the next allocation.
func (a *arena[T]) reset() { a.buf = a.buf[:0] }

// appendA appends xs to s, moving it within the arena where it is full.
// The elements go one at a time, but for a long list's move: most lists
// are short, and an append of a slice is a call of the runtime's copy.
func appendA[T any](a *arena[T], s []T, xs ...T) []T {
	if len(s)+len(xs) > cap(s) {
		ns := a.take(max(2*cap(s), len(s)+len(xs), 4))
		s = append(ns, s...)
	}
	for _, x := range xs {
		s = append(s, x)
	}
	return s
}

// chunks hands out values of a type one at a time from chunks it keeps,
// which an allocation empties for the next: their addresses stay put.
type chunks[T any] struct {
	bufs [][]T
	i, n int
}

// fill returns a value as an earlier allocation left it, for a caller
// that sets the whole of it.
func (c *chunks[T]) fill() *T {
	if c.i < len(c.bufs) && c.n == len(c.bufs[c.i]) {
		c.i, c.n = c.i+1, 0
	}
	if c.i == len(c.bufs) {
		// The first chunk is a few kilobytes, each after it twice the one
		// before up to 256 values: a small function's compile, in a
		// workspace new to it, makes little.
		n := firstLen[T]()
		if c.i > 0 {
			n = min(2*len(c.bufs[c.i-1]), max(n, 256))
		}
		c.bufs = append(c.bufs, make([]T, n))
	}
	p := &c.bufs[c.i][c.n]
	c.n++
	return p
}

// rest returns the values of the chunk fill takes them from next as an
// earlier allocation left them, all taken, for a caller that sets each it
// uses in order and gives the rest back (unuse) before any other fill.
func (c *chunks[T]) rest() []T {
	c.fill()
	c.n--
	r := c.bufs[c.i][c.n:]
	c.n = len(c.bufs[c.i])
	return r
}

// unuse gives back the last k values rest returned.
func (c *chunks[T]) unuse(k int) { c.n -= k }

// reset empties the chunks for the next allocation.
func (c *chunks[T]) reset() { c.i, c.n = 0, 0 }

// firstLen is the length of an arena's first buffer, and of the first of
// chunks', of T: about two kilobytes' worth, at least 8.
func firstLen[T any]() int {
	var z T
	return max(8, 2048/max(int(unsafe.Sizeof(z)), 1))
}
