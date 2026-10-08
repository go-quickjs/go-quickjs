// Package arena hands out small slices of T from big chunks, for working
// state that lives as long as one job and is thrown away together.
//
// An Arena is a bump allocator: Make cuts the next n slots from the
// current chunk, starting a new one, half as big again as the last, when
// it does not fit. Rewind keeps the chunks, so that the next job cuts its
// slices from the same memory and allocates nothing once the chunks are as
// big as its jobs need. Rewind clears what was handed out, so that the
// chunks keep nothing the last job pointed to alive, and so that Make
// hands out zeroed slots; Forget, for an arena of plain values, does not.
//
// An Arena is not safe for concurrent use.
package arena

// Arena is a bump allocator of T. Its zero value is ready to use.
type Arena[T any] struct {
	chunks [][]T
	// cur is the chunk being cut from, and off how much of it is cut.
	cur, off int
}

// firstChunk is the length of an arena's first chunk.
const firstChunk = 64

// Make is n zeroed slots, with a capacity of n: appending to it copies it
// out of the arena. A request bigger than a chunk is a slice of its own,
// which the arena does not keep.
func (a *Arena[T]) Make(n int) []T {
	if n <= 0 {
		return nil
	}
	for a.cur < len(a.chunks) {
		if c := a.chunks[a.cur]; a.off+n <= len(c) {
			a.off += n
			return c[a.off-n : a.off : a.off]
		}
		if a.cur+1 == len(a.chunks) {
			break
		}
		a.cur, a.off = a.cur+1, 0
	}
	// No chunk kept has room: a new one, unless n is more than one holds.
	l := a.nextLen()
	if n > l {
		return make([]T, n)
	}
	a.chunks = append(a.chunks, make([]T, l))
	a.cur, a.off = len(a.chunks)-1, n
	return a.chunks[a.cur][:n:n]
}

// nextLen is the length of the next chunk the arena would make.
func (a *Arena[T]) nextLen() int {
	if len(a.chunks) == 0 {
		return firstChunk
	}
	return len(a.chunks[len(a.chunks)-1]) * 3 / 2
}

// Append is s with v added, as the built-in append, but growing s, where it
// is full, into a slice from the arena twice as big.
func (a *Arena[T]) Append(s []T, v T) []T {
	if len(s) == cap(s) {
		t := a.Make(2*len(s) + 2)[:len(s)]
		copy(t, s)
		s = t
	}
	return append(s, v)
}

// Rewind takes back everything the arena handed out, clearing it, and
// keeps the chunks for what is made next. Nothing made before may be used
// after.
func (a *Arena[T]) Rewind() {
	for i := 0; i < a.cur && i < len(a.chunks); i++ {
		clear(a.chunks[i])
	}
	if a.cur < len(a.chunks) {
		clear(a.chunks[a.cur][:a.off])
	}
	a.cur, a.off = 0, 0
}

// Forget takes back everything the arena handed out, as Rewind does, but
// without clearing it: for a T with no pointers, whose old values keep
// nothing alive, and whose users do not need Make's slots zeroed. Until the
// next Rewind, Make hands out slots as they were left.
func (a *Arena[T]) Forget() { a.cur, a.off = 0, 0 }

// Cap is how many slots the arena's chunks hold.
func (a *Arena[T]) Cap() int {
	n := 0
	for _, c := range a.chunks {
		n += len(c)
	}
	return n
}
