package vm

import "sync"

// Work from other goroutines.
//
// A runtime belongs to one goroutine, but some of what it waits for finishes
// on another: an Atomics.waitAsync that another agent notifies, or whose time
// runs out. What finishes elsewhere is queued here and run on the runtime's
// goroutine -- by the host's event loop, when one is attached, and otherwise
// the next time the runtime runs its jobs, which a host can wait for.
//
// Nothing queued here keeps a host's loop running while it is only awaited,
// as node's does not for a waitAsync: a program that ends before the answer
// arrives does not wait for it.

// hostQueue is what other goroutines have finished for the runtime.
type hostQueue struct {
	mu   sync.Mutex
	jobs []func()
	// ready has a value while jobs is not empty and no loop is attached.
	ready chan struct{}
	// post is the host loop's way to run a function on the runtime's
	// goroutine, when there is one.
	post func(func())
}

// postFromElsewhere hands the runtime work from another goroutine. It is safe
// to call from any goroutine.
func (r *Runtime) postFromElsewhere(fn func()) {
	q := &r.hostJobs
	q.mu.Lock()
	post := q.post
	if post == nil {
		q.jobs = append(q.jobs, fn)
		// Signalled with the lock held, so that it cannot come after the
		// jobs were taken, and stay with nothing to run.
		select {
		case q.ready <- struct{}{}:
		default:
		}
	}
	q.mu.Unlock()
	if post != nil {
		post(fn)
	}
}

// taken empties the signal once the jobs have been taken. The lock is held.
func (q *hostQueue) taken() {
	select {
	case <-q.ready:
	default:
	}
}

// runHostJobs runs what other goroutines have handed over.
func (r *Runtime) runHostJobs() {
	q := &r.hostJobs
	q.mu.Lock()
	jobs := q.jobs
	q.jobs = nil
	q.taken()
	q.mu.Unlock()
	for _, fn := range jobs {
		fn()
	}
}

// PostFromElsewhere runs fn on the runtime's goroutine, from any goroutine:
// through the host's loop if one is attached, and otherwise the next time the
// runtime runs its jobs.
func (r *Runtime) PostFromElsewhere(fn func()) { r.postFromElsewhere(fn) }

// AttachHostLoop makes work from other goroutines go to post, a host loop's
// way to run a function on the runtime's goroutine. Work already waiting is
// handed over too, and HostJobsReady no longer signals it.
func (r *Runtime) AttachHostLoop(post func(func())) {
	q := &r.hostJobs
	q.mu.Lock()
	q.post = post
	jobs := q.jobs
	q.jobs = nil
	q.taken()
	q.mu.Unlock()
	for _, fn := range jobs {
		post(fn)
	}
}

// HostJobsReady has a value when work from another goroutine is waiting for
// the runtime to run its jobs, for a host that has no loop attached.
func (r *Runtime) HostJobsReady() <-chan struct{} { return r.hostJobs.ready }
