package quickjs

import (
	"context"
	"errors"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// ErrWorkDone is what a function posted to an AsyncWork is called with when
// the work was already done: nothing more may be posted to it.
var ErrWorkDone = errors.New("quickjs: the async work is done")

// AsyncWork is a piece of a host's work that finishes on other goroutines: a
// timer, a read, a request, or a source of many results such as a socket or a
// message port. It is Node-API's async work -- and its thread-safe function,
// for many results -- and Deno's op.
//
// Work is started on the runtime's goroutine, which is when it starts to keep
// the runtime busy (see Busy); results are posted to it from any goroutine,
// and it is ended, from any goroutine, with Done. Every function posted is
// called exactly once:
//
//   - with nil, on the runtime's goroutine, while the runtime is open: when
//     the host next runs its jobs (RunJobs, RunJobsContext, or the end of an
//     Eval), in the order the functions were posted;
//   - with ErrClosed once the runtime has closed: by Close, on the goroutine
//     that closed it, or at once, on the posting goroutine, if it was closed
//     already. It must not touch the runtime then; it is where the work
//     releases what it holds;
//   - with ErrWorkDone, at once, on the posting goroutine, if the work was
//     already done.
//
// Nothing else drops a posted function: not a loop that stops, not a script
// that is interrupted, and not another posted function that panics, which
// closes the runtime, so that those after it are called with ErrClosed.
type AsyncWork struct {
	w *vm.HostWork
}

// StartAsyncWork begins a piece of work, which keeps the runtime busy until
// it is done and what was posted to it has run. It is called on the runtime's
// goroutine, as every Runtime method is.
func (r *Runtime) StartAsyncWork() *AsyncWork {
	if r.closed || r.rt == nil {
		return &AsyncWork{}
	}
	return &AsyncWork{w: r.rt.StartHostWork()}
}

// Post hands fn to the runtime, from any goroutine: fn is called exactly once,
// as AsyncWork describes.
func (w *AsyncWork) Post(fn func(err error)) {
	if w.w == nil {
		fn(ErrClosed)
		return
	}
	switch w.w.Post(func() { fn(nil) }, func() { fn(ErrClosed) }) {
	case vm.Refused:
		fn(ErrWorkDone)
	}
}

// Done ends the work: nothing more is posted to it, and it keeps the runtime
// busy only until what was posted has run. A second Done does nothing. It is
// safe from any goroutine.
func (w *AsyncWork) Done() {
	if w.w != nil {
		w.w.Done()
	}
}

// Complete posts fn and ends the work, for work with one result.
func (w *AsyncWork) Complete(fn func(err error)) {
	w.Post(fn)
	w.Done()
}

// Unref stops the work keeping the runtime busy, for work a program should
// not wait for -- as unref() does for a timer in node. What is posted to it
// still runs. It is safe from any goroutine.
func (w *AsyncWork) Unref() {
	if w.w != nil {
		w.w.SetRef(false)
	}
}

// Ref makes unreferenced work keep the runtime busy again. It is safe from
// any goroutine.
func (w *AsyncWork) Ref() {
	if w.w != nil {
		w.w.SetRef(true)
	}
}

// closedChan is a channel that is always ready.
var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Wake has a value when the runtime has something to run -- a function posted
// to an AsyncWork, or a job -- or when the last work that kept it busy has
// ended, so that a loop waiting on it looks at Busy again:
//
//	for rt.Busy() {
//	    select {
//	    case <-rt.Wake():
//	        if err := rt.RunJobsContext(ctx); err != nil {
//	            return err
//	        }
//	    case <-ctx.Done():
//	        return ctx.Err()
//	    }
//	}
//
// It is safe to receive from on any goroutine.
func (r *Runtime) Wake() <-chan struct{} {
	if r.closed || r.rt == nil || r.rt.HasPendingJobs() {
		return closedChan
	}
	return r.rt.HostJobsReady()
}

// Busy reports whether the runtime has anything left: work not yet ended and
// not unreferenced, a posted function waiting, or a job. A closed runtime has
// nothing.
func (r *Runtime) Busy() bool {
	if r.closed || r.rt == nil {
		return false
	}
	return r.rt.HostBusy() || r.rt.HasPendingJobs()
}

// RunJobsContext is RunJobs under ctx: the functions posted to AsyncWorks,
// then the jobs, stop when ctx does, as a script EvalContext runs does.
func (r *Runtime) RunJobsContext(ctx context.Context) error {
	if r.closed || r.rt == nil {
		return ErrClosed
	}
	rt := r.rt
	prev := rt.Context()
	rt.SetContext(ctx)
	defer func() {
		if !r.closed {
			rt.SetContext(prev)
		}
	}()
	return r.RunJobs()
}

// AbortOn stops whatever the runtime runs once done is closed -- a script, a
// callback, a wait -- as a cancelled context stops an Eval, and every call
// after it. It is called on the runtime's goroutine; done may be closed from
// any, which is how a parent ends a worker.
func (r *Runtime) AbortOn(done <-chan struct{}) {
	if !r.closed && r.rt != nil {
		r.rt.SetAbort(done)
	}
}
