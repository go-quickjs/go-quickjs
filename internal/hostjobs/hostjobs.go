// Package hostjobs is how work another goroutine finishes for a runtime --
// the settling of an Atomics.waitAsync, a timer a host keeps -- reaches the
// runtime's goroutine: through the host's event loop when one is attached,
// and otherwise the next time the runtime runs its jobs.
//
// The quickjs package fills the functions in; the runtime is a
// *quickjs.Runtime, passed as any so that this package need not import it.
package hostjobs

var (
	// Attach makes work for rt go to post, which runs a function on rt's
	// goroutine: an event loop's Post.
	Attach func(rt any, post func(func()))
	// Post runs fn on rt's goroutine, from any goroutine.
	Post func(rt any, fn func())
	// Ready has a value when work is waiting for rt to run its jobs, for a
	// host with no loop attached.
	Ready func(rt any) <-chan struct{}
)
