package quickjs

import (
	"context"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// watchStop has the engine notice what stops it -- the context it runs
// under, the channel AbortOn gave it -- at its next back edge or call once
// that stops it, rather than at its next counted check, which a loop of a
// few long turns reaches seconds late (vm.OffsetBackEdges). It watches anew
// only when what stops the engine changed, and nothing once the runtime is
// closed.
func (r *Runtime) watchStop() {
	if r.closed {
		return
	}
	ctx := r.rt.Context()
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	if r.unwatch != nil && done == r.watchDone && r.abort == r.watchAbort {
		return
	}
	if r.unwatch != nil {
		r.unwatch()
	}
	r.watchDone, r.watchAbort = done, r.abort
	r.unwatch = watchEngine(r.rt, ctx, r.abort)
}

// watchEngine makes rt notice, at its next back edge or call, that ctx has
// ended or abort closed -- either may be nil -- until the function it
// returns is called.
//
// Once one has, a goroutine stores zero in the engine's counters, which the
// next back edge or call finds run out (vm.OffsetBackEdges). The store
// races with the engine's decrements on purpose: a word stored without
// synchronization is read whole (the Go memory model), and only ever ints
// are stored; the stop itself is recorded by the engine, on its own
// goroutine. A decrement, or native code's budget stored back from its
// register, can store over the zero, so the goroutine stores it again, at
// growing intervals, until the engine runs no script. A build with the race
// detector, which would report the race, keeps to the counted checks.
func watchEngine(rt *vm.Runtime, ctx context.Context, abort <-chan struct{}) (unwatch func()) {
	var done <-chan struct{}
	if ctx != nil {
		done = ctx.Done()
	}
	if raceBuild || done == nil && abort == nil {
		return func() {}
	}
	quit := make(chan struct{})
	poke := func() { pokeEngine(rt, quit) }
	if abort == nil {
		stop := context.AfterFunc(ctx, poke)
		return func() {
			close(quit)
			stop()
		}
	}
	go func() {
		select {
		case <-done:
		case <-abort:
		case <-quit:
			return
		}
		poke()
	}()
	return func() { close(quit) }
}

// pokeEngine runs rt's counters out until rt runs no script or quit closes.
func pokeEngine(rt *vm.Runtime, quit <-chan struct{}) {
	p := unsafe.Pointer(rt)
	field := func(off uintptr) *int { return (*int)(unsafe.Add(p, off)) }
	var t *time.Timer
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for wait := time.Millisecond; ; wait = min(2*wait, 16*time.Millisecond) {
		*field(vm.OffsetInterruptCounter), *field(vm.OffsetBackEdges) = 0, 0
		if *field(vm.OffsetFrameDepth) == 0 && *field(vm.OffsetHostCalls) == 0 {
			return
		}
		if t == nil {
			t = time.NewTimer(wait)
		} else {
			t.Reset(wait)
		}
		select {
		case <-quit:
			return
		case <-t.C:
		}
	}
}
