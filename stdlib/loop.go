// Package stdlib gives a runtime the things a program expects to find: a
// console, timers, fetch, a filesystem, and the rest.
//
// None of it is present unless a host asks for it, and each piece is asked for
// separately, because they are not equally dangerous. A console writes where
// the host says; timers need somewhere to run; fs and fetch reach outside the
// process altogether. A host that wants the lot, and has decided that is right
// for the code it is about to run, can say so in one line:
//
//	loop := stdlib.NewLoop(rt)
//	stdlib.Install(rt, loop, stdlib.Config{
//	    Stdout: os.Stdout,
//	    FS:     &stdlib.FS{Root: "."},
//	    Fetch:  &stdlib.Fetch{},
//	})
//	rt.Eval(script)
//	loop.Run(ctx)
//
// A host that wants only a console and timers installs only those. What is not
// installed cannot be reached: there is no ambient authority anywhere in the
// engine, so a capability that was never handed over does not exist for the
// script.
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

	// tasks carries work from other goroutines. It is buffered so that a
	// finishing request does not block on a loop that is busy.
	tasks chan func()
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
}

// NewLoop returns a loop for a runtime.
func NewLoop(rt *quickjs.Runtime) *Loop {
	return &Loop{rt: rt, tasks: make(chan func(), 64), wake: make(chan struct{}, 1)}
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
	select {
	case l.tasks <- fn:
	default:
		// The queue is full, which means the loop is far behind. Growing it is
		// better than dropping the work or blocking the goroutine that has it.
		go func() {
			defer func() { recover() }()
			l.tasks <- fn
		}()
	}
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
// result, which is what makes it safe to abandon a runtime.
func (l *Loop) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.nudge()
}

// Run works until there is nothing left to do.
//
// That means: no microtask queued, no timer armed, and no host operation
// outstanding. A program whose last act is to start a request keeps the loop
// running until the answer arrives and its reactions have run.
//
// Run returns the first error a callback raises, which is what an uncaught
// exception in a timer or a reaction becomes. Cancelling ctx stops the loop and
// returns its error.
func (l *Loop) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.rt.RunJobs(); err != nil {
			return err
		}
		// A timer that is due runs before the loop waits for anything: the
		// queue may be long, and each callback may queue microtasks of its own.
		now := time.Now()
		if t := l.timers.due(now); t != nil {
			if err := l.fire(t, now); err != nil {
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
		} else if !l.Pending() {
			// No timers, no host work, nothing queued: the program is over.
			return nil
		}

		select {
		case fn := <-l.tasks:
			fn()
			l.ran()
		case <-l.wake:
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RunUntil works until the given promise settles, which is what a host running
// one asynchronous thing wants rather than a loop that outlives it.
func (l *Loop) RunUntil(ctx context.Context, done <-chan struct{}) error {
	for {
		select {
		case <-done:
			return l.rt.RunJobs()
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.rt.RunJobs(); err != nil {
			return err
		}
		now := time.Now()
		if t := l.timers.due(now); t != nil {
			if err := l.fire(t, now); err != nil {
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
		case fn := <-l.tasks:
			fn()
			l.ran()
		case <-l.wake:
		case <-wait:
		case <-done:
			return l.rt.RunJobs()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fire runs a timer's callback and re-arms it if it repeats.
func (l *Loop) fire(t *timer, now time.Time) error {
	if t.repeat > 0 {
		t.at = now.Add(t.repeat)
		l.timers.add(t)
	} else {
		delete(l.timers.byID, t.id)
	}
	_, err := t.fn.Call(t.args...)
	if err != nil {
		return err
	}
	return l.rt.RunJobs()
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
