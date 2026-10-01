// Package stdlib gives a runtime the things a program expects to find: a
// console, timers, streams, hashing, a filesystem, the network, and the rest.
//
// None of what reaches outside the process is present unless a host asks for
// it, and each piece is asked for separately, because they are not equally
// dangerous. A console writes where the host says; timers need somewhere to
// run; fs, fetch, serve, sockets and child processes reach the world. A host
// that wants the lot, and has decided that is right for the code it is about to
// run, can say so in one line:
//
//	loop := stdlib.NewLoop(rt)
//	stdlib.Install(rt, stdlib.Config{
//	    Stdout: os.Stdout,
//	    Loop:   loop,
//	    FS:     &stdlib.FS{Root: "."},
//	    Fetch:  &stdlib.Fetch{},
//	})
//	rt.Eval(script)
//	loop.Run(ctx)
//
// What is installed without being asked for is what cannot reach anything: the
// language's own library, the web's streams and text encoders, URL, Blob and
// FormData, hashing and compression, and the node modules that are pure
// computation. Everything else is a field in [Config].
//
// A host that wants only a console and timers installs only those. What is not
// installed cannot be reached: there is no ambient authority anywhere in the
// engine, so a capability that was never handed over does not exist for the
// script.
//
// # Where the work happens
//
// A runtime belongs to one goroutine. Everything here that waits -- a request,
// a file, a program, a socket -- does its waiting on another goroutine and
// hands the result back through [Loop.Post], which runs it on the loop's
// goroutine with the rest of the script. Nothing in this package touches a
// JavaScript value from anywhere else.
package stdlib

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Loop runs the work a runtime has waiting: the microtasks a promise queues,
// the callbacks a timer is due to run, and whatever another goroutine has
// finished and handed back.
//
// A Runtime belongs to one goroutine, so everything the loop runs happens on
// the goroutine that called Run. Work from elsewhere arrives through Post,
// which is how an HTTP request that finished on its own goroutine gets its
// promise settled on this one.
type Loop struct {
	rt *quickjs.Runtime

	timers timerQueue
	nextID int64

	// work carries work from other goroutines to the runtime, which runs it
	// as a task when the loop runs its jobs. It does not keep the runtime
	// busy of itself: pending counts what does.
	work *quickjs.AsyncWork
	// wake is how a loop that is waiting is told to look again. Work that
	// finishes without posting anything -- a socket closing, a request being
	// cancelled -- changes nothing the loop is watching, and a loop waiting on
	// a queue that will stay empty would wait for ever.
	wake chan struct{}

	mu sync.Mutex
	// pending counts the host operations that have started and not finished,
	// and the work that has been posted and not yet run. The loop waits while
	// any remain, which is what keeps it running for a request that has been
	// sent but not answered -- and, once the answer is in the queue, until the
	// queue has been emptied.
	pending int
	// closed marks a loop that will run no more work.
	closed bool
	// failed is an exception that work posted from elsewhere raised and
	// nothing caught, which Run returns as it returns one a timer raises.
	failed error

	// ctx is the loop's lifetime: its runtime's, ended early by Close.
	ctx    context.Context
	cancel context.CancelFunc

	// report is asked about an error a callback raised, before Run returns
	// it; true means it was reported, and Run goes on.
	report func(error) bool
}

// NewLoop returns a loop for a runtime.
func NewLoop(rt *quickjs.Runtime) *Loop {
	return newLoop(rt, rt.Context())
}

// newLoop is NewLoop for a loop whose context ends with ctx as well as with
// its runtime: a worker's, which terminate ends.
func newLoop(rt *quickjs.Runtime, ctx context.Context) *Loop {
	l := &Loop{rt: rt, work: rt.StartAsyncWork(), wake: make(chan struct{}, 1)}
	l.work.Unref()
	l.ctx, l.cancel = context.WithCancel(ctx)
	if ctx != rt.Context() {
		context.AfterFunc(rt.Context(), l.cancel)
	}
	return l
}

// Post hands work to the loop from another goroutine.
//
// The function runs on the loop's goroutine, which is the only one that may
// touch the runtime. Posting to a loop that has stopped does nothing, so a
// request that outlives the loop cannot block on it.
func (l *Loop) Post(fn func()) {
	l.mu.Lock()
	closed := l.closed
	if !closed {
		// Work that has been posted counts as outstanding until it has run.
		// Without that the loop can decide the program is over in the moment
		// between a goroutine posting its answer and finishing, and the answer
		// is never delivered.
		l.pending++
	}
	l.mu.Unlock()
	if closed {
		return
	}
	// The runtime holds what is posted until it runs, whatever the loop is
	// doing; a loop closed by then runs none of it.
	l.work.Post(func(err error) {
		if err == nil && !l.isClosed() {
			fn()
		}
		l.ran()
	})
}

// ran records that posted work has been taken off the queue and run.
func (l *Loop) ran() {
	l.Done()
}

// Begin records that a host operation has started, so that Run waits for it.
// Every Begin must be matched by a Done, whatever the operation's outcome.
func (l *Loop) Begin() {
	l.mu.Lock()
	l.pending++
	l.mu.Unlock()
}

// Done records that a host operation has finished.
func (l *Loop) Done() {
	l.mu.Lock()
	if l.pending > 0 {
		l.pending--
	}
	empty := l.pending == 0
	l.mu.Unlock()
	if empty {
		// The last thing the loop was waiting for has finished, and it may
		// have nothing else to do. Whatever it is waiting on, it is told to
		// look again rather than waiting for work that is not coming.
		l.nudge()
	}
}

// nudge wakes a loop that is waiting, if it is.
func (l *Loop) nudge() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Pending reports whether any host operation is outstanding.
func (l *Loop) Pending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending > 0
}

// Close stops the loop from accepting further work. Work already posted is
// dropped, and a request that finishes afterwards has nowhere to deliver its
// result, which is what makes it safe to abandon a runtime. A Run or RunUntil
// returns once the callback it is running, if any, does.
func (l *Loop) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	l.nudge()
}

// Context is cancelled when the loop is closed, or its runtime is. What the
// standard library started for the script stops then -- a request is
// abandoned, a program killed, a socket and a server closed, a worker
// terminated -- and a host's own functions should start their work with it
// too, rather than finishing it for a runtime that is gone. Run's context is
// not this: a Run that ends may be followed by another. It is safe to call
// from any goroutine.
func (l *Loop) Context() context.Context { return l.ctx }

// isClosed reports whether Close has been called.
func (l *Loop) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// loopContext is a loop's context, or the background for none.
func loopContext(l *Loop) context.Context {
	if l == nil {
		return context.Background()
	}
	return l.ctx
}

// Run works until there is nothing left to do.
//
// That means: no microtask queued, no timer armed, and no host operation
// outstanding. A program whose last act is to start a request keeps the loop
// running until the answer arrives and its reactions have run.
//
// Run returns the first error a callback raises, which is what an uncaught
// exception in a timer or a reaction becomes. Cancelling ctx stops the loop --
// and the callback running, should one never return -- and returns its error.
func (l *Loop) Run(ctx context.Context) error {
	// The callbacks run under ctx, so that one that never returns is
	// stopped by it too, not only the waiting between them.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if l.isClosed() {
			// What the loop was waiting for will not be delivered.
			return nil
		}
		// What other goroutines finished runs here, a task at a time, each
		// followed by the microtasks it queued.
		if err := l.rt.RunJobsContext(ctx); l.fatal(err) {
			return err
		}
		if err := l.takeFailure(); l.fatal(err) {
			return err
		}
		// A timer that is due runs before the loop waits for anything: the
		// queue may be long, and each callback may queue microtasks of its own.
		now := time.Now()
		if t := l.timers.due(now); t != nil {
			if err := l.fire(ctx, t, now); l.fatal(err) {
				return err
			}
			continue
		}
		if l.rt.HasPendingJobs() {
			continue
		}

		// Nothing is ready, so the loop waits for whichever comes first: work
		// from another goroutine, the next timer, or the context ending.
		var wait <-chan time.Time
		if next, ok := l.timers.next(); ok {
			timer := time.NewTimer(max(0, time.Until(next)))
			defer timer.Stop()
			wait = timer.C
		} else if !l.Pending() && !l.rt.Busy() {
			// No timers, no host work, nothing queued: the program is over.
			return nil
		}

		select {
		case <-l.rt.Wake():
		case <-l.wake:
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fatal reports whether an error a callback raised ends Run: any does,
// unless the loop's report takes it, which a web worker's does with an
// exception -- the standard reports it, and the worker runs on.
func (l *Loop) fatal(err error) bool {
	return err != nil && (l.report == nil || !l.report(err))
}

// RunUntil works until the given promise settles, which is what a host running
// one asynchronous thing wants rather than a loop that outlives it.
func (l *Loop) RunUntil(ctx context.Context, done <-chan struct{}) error {
	for {
		select {
		case <-done:
			return l.rt.RunJobsContext(ctx)
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if l.isClosed() {
			return nil
		}
		if err := l.rt.RunJobsContext(ctx); err != nil {
			return err
		}
		if err := l.takeFailure(); err != nil {
			return err
		}
		now := time.Now()
		if t := l.timers.due(now); t != nil {
			if err := l.fire(ctx, t, now); err != nil {
				return err
			}
			continue
		}
		var wait <-chan time.Time
		if next, ok := l.timers.next(); ok {
			timer := time.NewTimer(max(0, time.Until(next)))
			defer timer.Stop()
			wait = timer.C
		}
		select {
		case <-l.rt.Wake():
		case <-l.wake:
		case <-wait:
		case <-done:
			return l.rt.RunJobsContext(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fail records an exception that work posted to the loop raised and nothing
// caught -- a MessagePort's listener's -- for Run to return.
func (l *Loop) fail(err error) {
	l.mu.Lock()
	if l.failed == nil {
		l.failed = err
	}
	l.mu.Unlock()
}

// takeFailure returns the exception fail recorded, if there is one.
func (l *Loop) takeFailure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.failed
	l.failed = nil
	return err
}

// fire runs a timer's callback, and the microtasks it queued, under ctx, and
// re-arms it if it repeats.
func (l *Loop) fire(ctx context.Context, t *timer, now time.Time) error {
	if t.repeat > 0 {
		t.at = now.Add(t.repeat)
		l.timers.add(t)
	} else {
		delete(l.timers.byID, t.id)
	}
	_, err := t.fn.CallContext(ctx, t.args...)
	if err != nil {
		return err
	}
	return l.rt.RunJobsContext(ctx)
}

// ErrLoopClosed reports work handed to a loop that has stopped.
var ErrLoopClosed = errors.New("stdlib: the event loop has stopped")

// ---------------------------------------------------------------------------
// The timer queue
// ---------------------------------------------------------------------------

// timer is one armed callback.
type timer struct {
	id     int64
	at     time.Time
	repeat time.Duration
	fn     quickjs.Value
	args   []any
	index  int
	dead   bool
}

// timerQueue keeps the armed timers in the order they are due.
//
// A cancelled timer is marked rather than removed, because a script cancels by
// an identifier it was given and the queue is a heap: marking costs nothing and
// the entry falls out when its turn comes.
type timerQueue struct {
	entries []*timer
	byID    map[int64]*timer
}

func (q *timerQueue) add(t *timer) {
	if q.byID == nil {
		q.byID = make(map[int64]*timer)
	}
	q.byID[t.id] = t
	heap.Push((*timerHeap)(q), t)
}

func (q *timerQueue) cancel(id int64) {
	if t, ok := q.byID[id]; ok {
		t.dead = true
		delete(q.byID, id)
	}
}

// due returns the timer whose time has come, or nil.
func (q *timerQueue) due(now time.Time) *timer {
	for len(q.entries) > 0 {
		t := q.entries[0]
		if t.dead {
			heap.Pop((*timerHeap)(q))
			continue
		}
		if t.at.After(now) {
			return nil
		}
		heap.Pop((*timerHeap)(q))
		return t
	}
	return nil
}

// next returns when the earliest live timer is due.
func (q *timerQueue) next() (time.Time, bool) {
	for len(q.entries) > 0 {
		if q.entries[0].dead {
			heap.Pop((*timerHeap)(q))
			continue
		}
		return q.entries[0].at, true
	}
	return time.Time{}, false
}

// timerHeap is the heap interface over the queue, kept apart so that the
// queue's own methods read as what they do rather than as heap mechanics.
type timerHeap timerQueue

func (h *timerHeap) Len() int { return len(h.entries) }
func (h *timerHeap) Less(i, j int) bool {
	return h.entries[i].at.Before(h.entries[j].at)
}
func (h *timerHeap) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.entries[i].index, h.entries[j].index = i, j
}
func (h *timerHeap) Push(x any) {
	t := x.(*timer)
	t.index = len(h.entries)
	h.entries = append(h.entries, t)
}
func (h *timerHeap) Pop() any {
	old := h.entries
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	h.entries = old[:n-1]
	return t
}
