package pool

import "sync"

// Allocator hands out T from a short free list of its own, in front of a
// Pool. What the list holds survives collections, which empty a Pool, so
// that the working state a job grows -- arena chunks, a graph's arrays --
// is kept for the next job rather than made again after each collection;
// the list is capped, so that it keeps no more than a few jobs' worth, and
// what does not fit goes to the Pool. It is safe for concurrent use: the
// list is behind a mutex, which a job takes once to begin and once to end.
type Allocator[T any] struct {
	mu   sync.Mutex
	free []T
	max  int
	pool *Pool[T]
}

// NewAllocator is an allocator keeping up to max T in its list, which
// makes a T with make and resets one with reset as it is put back.
func NewAllocator[T any](max int, make func() T, reset func(T)) *Allocator[T] {
	return &Allocator[T]{max: max, pool: New(make, reset)}
}

// Get is a T from the list, or else from the pool, or a new one.
func (a *Allocator[T]) Get() T {
	a.mu.Lock()
	if n := len(a.free); n > 0 {
		x := a.free[n-1]
		var zero T
		a.free[n-1] = zero
		a.free = a.free[:n-1]
		a.mu.Unlock()
		return x
	}
	a.mu.Unlock()
	return a.pool.Get()
}

// Put resets x and keeps it in the list, or, where the list is full, in
// the pool.
func (a *Allocator[T]) Put(x T) {
	if a.pool.reset != nil {
		a.pool.reset(x)
	}
	a.mu.Lock()
	if len(a.free) < a.max {
		a.free = append(a.free, x)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	a.pool.p.Put(x)
}
