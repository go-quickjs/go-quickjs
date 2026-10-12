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

import "unsafe"

// Arena is a bump allocator of T. Its zero value is ready to use.
type Arena[T any] struct {
	chunks [][]T
	// cur is the chunk being cut from, and off how much of it is cut.
	cur, off int
	// used is how much of each chunk before cur was cut: what Rewind
	// clears, not the rest a request that did not fit left.
	used []int
	// keepBig makes a request bigger than a chunk a chunk of its own that
	// the arena keeps (KeepBig).
	keepBig bool
	// recent are how many slots each of the last jobs used, the latest at
	// next, and jobs how many there have been, up to recentJobs: what an
	// arena that keeps big requests keeps chunks for (trim).
	recent [recentJobs]int
	next   int
	jobs   int
}

// recentJobs is how many jobs an arena that keeps big requests keeps
// chunks for.
const recentJobs = 16

// KeepBig has the arena keep a request bigger than its next chunk as a
// chunk of its own, for the next job to cut from, rather than hand it out
// and forget it: for jobs that make a few big tables each time, as a
// compile does, at the cost of keeping memory as big as the biggest of
// the recent jobs' (Rewind).
func (a *Arena[T]) KeepBig() { a.keepBig = true }

// firstChunk is the length of an arena's first chunk.
const firstChunk = 64

// Make is n zeroed slots, with a capacity of n: appending to it copies it
// out of the arena. A request bigger than a chunk is a slice of its own,
// which the arena does not keep, unless it keeps big ones (KeepBig).
func (a *Arena[T]) Make(n int) []T {
	if n <= 0 {
		return nil
	}
	for a.cur < len(a.chunks) {
		if c := a.chunks[a.cur]; a.off+n <= len(c) {
			a.off += n
			return c[a.off-n : a.off : a.off]
		}
		a.leave()
		if a.cur+1 == len(a.chunks) {
			break
		}
		a.cur, a.off = a.cur+1, 0
	}
	// No chunk kept has room: a new one, unless n is more than one holds.
	l := a.nextLen()
	if n > l {
		if !a.keepBig {
			return make([]T, n)
		}
		l = n
	}
	a.chunks = append(a.chunks, make([]T, l))
	a.cur, a.off = len(a.chunks)-1, n
	return a.chunks[a.cur][:n:n]
}

// leave notes how much of the current chunk was cut, as Make moves past it.
func (a *Arena[T]) leave() {
	for len(a.used) <= a.cur {
		a.used = append(a.used, 0)
	}
	a.used[a.cur] = a.off
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
// after. An arena that keeps big requests (KeepBig) keeps chunks for as
// much as the recent jobs before this one used, and lets the rest go
// (trim).
func (a *Arena[T]) Rewind() {
	used := 0
	for i := 0; i < a.cur && i < len(a.chunks); i++ {
		clear(a.chunks[i][:a.used[i]])
		used += a.used[i]
	}
	if a.cur < len(a.chunks) {
		clear(a.chunks[a.cur][:a.off])
		used += a.off
	}
	a.cur, a.off = 0, 0
	if a.keepBig {
		a.trim(used)
	}
}

// trim notes a job that used n slots, and, if it used more than any of the
// recent jobs before it, drops the chunks past those they needed: a
// compile of one big function gives its tables back as it ends, where
// they had been kept as long as the runtime -- RegExp's run blocks, of
// hundreds of captured bindings, left a hundred megabytes live, which the
// collector then marked at every cycle. Jobs as big that come often keep
// their chunks: one among the recent ones needed as much.
func (a *Arena[T]) trim(n int) {
	keep, known := 0, a.jobs > 0
	for _, m := range a.recent[:min(a.jobs, recentJobs)] {
		keep = max(keep, m)
	}
	a.recent[a.next] = n
	a.next = (a.next + 1) % recentJobs
	a.jobs = min(a.jobs+1, recentJobs)
	if !known || n <= keep {
		return
	}
	total := 0
	for i, c := range a.chunks {
		if total >= keep {
			clear(a.chunks[i:])
			a.chunks = a.chunks[:i]
			break
		}
		total += len(c)
	}
	if len(a.used) > len(a.chunks) {
		a.used = a.used[:len(a.chunks)]
	}
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

// Bytes is the memory the arena's chunks hold.
func (a *Arena[T]) Bytes() int {
	var t T
	return a.Cap()*int(unsafe.Sizeof(t)) + cap(a.chunks)*int(unsafe.Sizeof(a.chunks)) + cap(a.used)*int(unsafe.Sizeof(0))
}
