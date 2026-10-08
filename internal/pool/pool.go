// Package pool keeps reusable working state between jobs: Pool, a typed
// sync.Pool whose Put runs a reset first, and Allocator, a short free list
// in front of one.
//
// Both are safe for concurrent use, which the engine needs of anything
// that keeps scratch for a function's tree: the tree is built by whichever
// goroutine first calls the function, and a compiled program, its trees
// with it, may be shared by runtimes on several goroutines. A collection
// may empty a Pool, and the next job then makes its scratch again, at the
// size it needs; what an Allocator's list holds stays.
package pool

import "sync"

// Pool is a sync.Pool of T, made by new and reset by reset, if not nil,
// as it is put back.
type Pool[T any] struct {
	p     sync.Pool
	reset func(T)
}

// New is a pool that makes a T with make and resets one with reset.
func New[T any](make func() T, reset func(T)) *Pool[T] {
	return &Pool[T]{p: sync.Pool{New: func() any { return make() }}, reset: reset}
}

// Get is a T from the pool, or a new one.
func (p *Pool[T]) Get() T { return p.p.Get().(T) }

// Put resets x and keeps it for a later Get.
func (p *Pool[T]) Put(x T) {
	if p.reset != nil {
		p.reset(x)
	}
	p.p.Put(x)
}
