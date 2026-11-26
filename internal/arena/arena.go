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
	// jobs is what an arena that keeps big requests keeps chunks for
	// (trim).
	jobs jobs
}

// jobs are the most slots the chunks a job cut from held (its reach) of
// the jobs that ended in the collector's cycle the last job ended in
// (cycle), this, and of those that ended in the cycle before it, last.
// Not generic, as what decides a trim is not: an Arena's
// methods are compiled for each T, and kept to the size they had, for the
// code laid out after them.
type jobs struct {
	cycle      uint64
	this, last int
}

// KeepBig has the arena keep a request bigger than its next chunk as a
// chunk of its own, for the next job to cut from, rather than hand it out
// and forget it: for jobs that make a few big tables each time, as a
// compile does, at the cost of keeping memory as big as the biggest of
// the recent jobs' (RewindAt).
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
// after. It is RewindAt in the collector's cycle the last job ended in.
func (a *Arena[T]) Rewind() { a.RewindAt(a.jobs.cycle) }

// RewindAt is Rewind at the end of a job that ended in the collector's
// cycle cycle, which the caller counts: an arena that keeps big requests
// (KeepBig) keeps chunks for as much as the jobs that ended in this cycle
// and the one before it, this one among them, reached, and lets the rest
// go (trim).
func (a *Arena[T]) RewindAt(cycle uint64) {
	if a.keepBig {
		a.trim(cycle)
	}
	for i := 0; i < a.cur && i < len(a.chunks); i++ {
		clear(a.chunks[i][:a.used[i]])
	}
	if a.cur < len(a.chunks) {
		clear(a.chunks[a.cur][:a.off])
	}
	a.cur, a.off = 0, 0
}

// trim notes the job that ends, in cycle, and drops the chunks past what
// the most demanding of the jobs of this cycle and the one before needed
// (jobs' cut), as sync.Pool keeps what was put in it for a cycle more: a
// compile of one big function keeps its tables until a cycle has passed
// with none as big, where they had been kept as long as the runtime --
// RegExp's run blocks, of hundreds of captured bindings, left a hundred
// megabytes live, which the collector then marked at every cycle. Given
// back as the big job ended, when it reached past the sixteen jobs before
// it, they were made again by the next job as big, which came often: a
// compile repeated, or the next of a program's functions of that size --
// 54 of the 94 MB a run of -jitcompile's repeats allocated on Typescript;
// kept for the sixteen jobs after it, a program's next big compile still
// made them again, some 30 MB a run of Typescript. What the jobs of two
// cycles needed, the heap held while they ran. What it drops Rewind does
// not clear: nothing points to it.
func (a *Arena[T]) trim(cycle uint64) {
	// The chunks' lengths, which do not depend on T.
	chunks := *(*[][]struct{})(unsafe.Pointer(&a.chunks))
	if i := cut[struct{}](&a.jobs, chunks, a.cur, a.off != 0 || a.cur != 0, cycle); i >= 0 {
		clear(a.chunks[i:])
		a.chunks = a.chunks[:i]
		if len(a.used) > i {
			a.used = a.used[:i]
		}
	}
}

// cut notes, in j, a job that ended in cycle and cut from the chunks
// before chunks[cur], and from it if cutCur, and returns how many of
// chunks to keep, or -1 for all: as many as hold the most any job of this
// cycle or the one before, this one included, reached. A job's reach is
// how many slots the chunks it cut from hold, the rest that requests that
// did not fit left behind included: what the same job needs again.
// Counted as the slots it used, the chunks kept could not hold a job like
// the recent ones again, its last requests making new chunks after every
// trim.
//
// It is generic only to be compiled where it is called, with the Arena's
// methods, and not in this package, ahead of every package that uses it:
// code of the package's own moved all of theirs, package vm's included.
func cut[_ any](j *jobs, chunks [][]struct{}, cur int, cutCur bool, cycle uint64) int {
	reach := 0
	for i := 0; i < cur && i < len(chunks); i++ {
		reach += len(chunks[i])
	}
	if cur < len(chunks) && cutCur {
		reach += len(chunks[cur])
	}
	if cycle != j.cycle {
		j.last = 0
		if cycle == j.cycle+1 {
			j.last = j.this
		}
		j.this, j.cycle = 0, cycle
	}
	j.this = max(j.this, reach)
	keep := max(j.this, j.last)
	total := 0
	for i, c := range chunks {
		if total >= keep {
			return i
		}
		total += len(c)
	}
	return -1
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
