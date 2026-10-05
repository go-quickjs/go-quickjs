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

// AsyncContext is the runtime's async context: a value the host sets, which
// the engine carries from where work was queued to where it runs. A promise
// reaction -- a then, an await -- runs in the context it was registered in,
// a job in the one it was queued in, a FinalizationRegistry's callback in
// the one the registry was made in, and what is posted to an AsyncWork in
// the one the work was started in. It is undefined until a host sets it.
//
// It is what AsyncLocalStorage is built on, as node's is since node 24 and
// as the TC39 AsyncContext proposal has it: the value is opaque to the
// engine, and a host's AsyncLocalStorage keeps in it, say, a map from each
// storage to its store, copied whenever one is set. A host that runs
// callbacks of its own -- a timer, a read that finished -- takes the context
// when it starts the work and sets it around the callback.
func (r *Runtime) AsyncContext() Value {
	if r.closed || r.rt == nil {
		return Value{}
	}
	return Value{v: r.rt.AsyncContext(), rt: r.rt}
}

// SetAsyncContext makes v the async context and returns the one before, for
// the host to put back when what it runs in v is done. A call into the
// runtime from Go -- Eval, RunJobs -- leaves the context as it found it.
func (r *Runtime) SetAsyncContext(v Value) (prev Value) {
	if r.closed || r.rt == nil {
		return Value{}
	}
	p := r.rt.SetAsyncContext(r.vmValue(v))
	return Value{v: p, rt: r.rt}
}

// Go runs fn on a goroutine of its own and returns a promise for its result,
// which is what a Go function hands script for work that finishes later: a
// query, a request, a computation. When fn returns, its result is converted
// as Set converts a value and the promise resolved with it, or rejected with
// its error as RejectError rejects, on the runtime's goroutine when the
// runtime next runs its jobs; the work keeps the runtime busy until then,
// as an AsyncWork does. fn is given the runtime's Context, which Close
// cancels; a result that arrives after Close is dropped.
//
//	rt.Set("fetchUser", func(id int) quickjs.Value {
//	    return rt.Go(func(ctx context.Context) (any, error) {
//	        return db.LoadUser(ctx, id)
//	    })
//	})
//
// fn runs on its own goroutine, so it must not touch the runtime or its
// values: what it needs from script it takes as Go values before it starts.
// A panic in fn is a panic of its goroutine.
func (r *Runtime) Go(fn func(ctx context.Context) (any, error)) Value {
	p := r.NewPromise()
	if r.closed || r.rt == nil {
		return p.Value()
	}
	w := r.StartAsyncWork()
	ctx := r.Context()
	go func() {
		v, err := fn(ctx)
		w.Complete(func(cerr error) {
			switch {
			case cerr != nil:
			case err != nil:
				p.RejectError(err)
			default:
				if err := p.Resolve(v); err != nil {
					p.RejectError(err)
				}
			}
		})
	}()
	return p.Value()
}

// ErrNeverSettles is what Await returns for a promise that is still pending
// when the runtime has nothing left that could settle it: no job, and no
// AsyncWork that is not done.
var ErrNeverSettles = errors.New("quickjs: the promise is pending and nothing is left to settle it")

// Await waits for a promise to settle, running the runtime's jobs and the
// functions posted to its AsyncWorks meanwhile, as an event loop would, and
// returns what it was fulfilled with, or what it was rejected with as an
// error, as a call that threw returns one. A value that is not a promise is
// awaited as script's await takes it: a thenable is followed, and anything
// else is its own result.
//
// It is for the host, between calls: called while script is running -- by a
// Go function the script called -- it returns an error rather than run the
// runtime's jobs inside the script. It stops when ctx does, and returns
// ErrNeverSettles when the runtime has nothing left that could settle the
// promise. A promise it waits for counts as handled, as one a then is
// attached to does: a rejection it returns is not also reported to
// OnUnhandledRejection.
func (v Value) Await(ctx context.Context) (Value, error) {
	if v.rt == nil {
		return Value{}, ErrClosed
	}
	host, _ := v.rt.Host.(*Runtime)
	if host == nil || host.closed || host.rt == nil {
		return Value{}, ErrClosed
	}
	if host.rt.Running() {
		return Value{}, errors.New("quickjs: Await called while a script is running")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pv := v.v
	if _, _, _, ok := host.rt.PromiseResult(pv); !ok {
		// What await does with a value: resolve a promise with it.
		p := host.NewPromise()
		p.p.Resolve(pv)
		pv = p.p.Value()
	}
	for {
		res, settled, rejected, _ := host.rt.PromiseResult(pv)
		if settled {
			if rejected {
				return Value{}, host.wrapError(host.rt.ThrowValue(res))
			}
			return Value{v: res, rt: host.rt}, nil
		}
		if !host.Busy() {
			return Value{}, ErrNeverSettles
		}
		select {
		case <-host.Wake():
			if err := host.RunJobsContext(ctx); err != nil {
				return Value{}, err
			}
		case <-ctx.Done():
			return Value{}, ctx.Err()
		}
		if host.closed {
			return Value{}, ErrClosed
		}
	}
}
