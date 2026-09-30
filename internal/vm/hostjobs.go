package vm

import "sync"

// Work from other goroutines.
//
// A runtime belongs to one goroutine, but much of what it waits for finishes
// on another: an Atomics.waitAsync that another agent notifies, a host's
// timer, a read, a request. What finishes elsewhere is queued here and run on
// the runtime's goroutine, the next time the runtime runs its jobs.
//
// The runtime holds the queue, whatever loop drives it, so that nothing
// queued is lost: each job either runs, once, on the runtime's goroutine, or
// -- if the runtime closes first -- is cancelled, once. A host's work is
// counted while it is outstanding, which is how a loop knows a program is
// done; an Atomics.waitAsync is not, as node's is not.

// hostJob is one function queued from elsewhere: what runs, and what is called
// instead if the runtime closes before it can -- nil for a job that has
// nothing to say then.
type hostJob struct {
	run    func()
	cancel func()
	work   *HostWork
}

// hostQueue is what other goroutines have finished for the runtime.
type hostQueue struct {
	mu   sync.Mutex
	jobs []hostJob
	// closed is set once the runtime has closed, after which a job is
	// cancelled at once rather than queued.
	closed bool
	// live counts the referenced works that keep the runtime busy.
	live int
	// ready has a value when jobs wait, or when the last live work has
	// ended: a loop waiting on it looks again.
	ready chan struct{}
	// post is the host loop's way to run a function on the runtime's
	// goroutine, when one is attached; it is told to run the queue.
	post func(func())
}

// signal wakes whoever waits on ready. The lock is held.
func (q *hostQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// taken empties the signal once the jobs have been taken. The lock is held.
func (q *hostQueue) taken() {
	select {
	case <-q.ready:
	default:
	}
}

// postFromElsewhere hands the runtime work from another goroutine that keeps
// nothing waiting for it: an Atomics.waitAsync's answer. It is safe to call
// from any goroutine; once the runtime has closed, fn is dropped.
func (r *Runtime) postFromElsewhere(fn func()) {
	r.enqueueHostJob(hostJob{run: fn})
}

// enqueueHostJob queues j, or cancels it at once if the runtime has closed,
// and reports whether it was queued.
func (r *Runtime) enqueueHostJob(j hostJob) bool {
	q := &r.hostJobs
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		if j.cancel != nil {
			j.cancel()
		}
		return false
	}
	q.jobs = append(q.jobs, j)
	if j.work != nil {
		j.work.queued++
	}
	q.signal()
	post := q.post
	q.mu.Unlock()
	if post != nil {
		post(r.runHostJobsForLoop)
	}
	return true
}

// runHostJobs runs what other goroutines have handed over, in the order it
// was handed over, as runHostJob does.
func (r *Runtime) runHostJobs() error {
	for {
		ran, err := r.runHostJob()
		if err != nil || !ran {
			return err
		}
	}
}

// runHostJob runs the next job another goroutine handed over, if there is
// one, and reports whether it ran one. A job is taken off the queue one at a
// time, so that one that panics leaves those behind it queued, for Close to
// cancel; and none runs while the host has stopped the script, which leaves
// them for the next run and returns why.
func (r *Runtime) runHostJob() (bool, error) {
	q := &r.hostJobs
	{
		if r.stopped != nil {
			return false, r.stopped
		}
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.taken()
			q.mu.Unlock()
			return false, nil
		}
		j := q.jobs[0]
		q.jobs[0] = hostJob{}
		q.jobs = q.jobs[1:]
		if w := j.work; w != nil {
			was := w.isLive()
			w.queued--
			q.update(w, was)
		}
		q.mu.Unlock()
		j.run()
		if r.stopped != nil {
			return true, r.stopped
		}
		return true, nil
	}
}

// runHostJobsForLoop is what a host loop is handed to run: the jobs, each
// followed by the microtasks it queued, as any run of them is.
func (r *Runtime) runHostJobsForLoop() {
	if r.frameDepth == 0 {
		_ = r.DrainJobs()
	}
}

// CloseHostJobs closes the queue: every job waiting is cancelled, in order,
// and any posted from now on is cancelled at once. The runtime has closed.
func (r *Runtime) CloseHostJobs() {
	q := &r.hostJobs
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	jobs := q.jobs
	q.jobs = nil
	q.live = 0
	q.post = nil
	q.signal()
	q.mu.Unlock()
	for _, j := range jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
}

// PostFromElsewhere runs fn on the runtime's goroutine, from any goroutine,
// the next time the runtime runs its jobs.
func (r *Runtime) PostFromElsewhere(fn func()) { r.postFromElsewhere(fn) }

// AttachHostLoop has post told to run the queue whenever a job arrives, post
// being a host loop's way to run a function on the runtime's goroutine. The
// jobs stay in the runtime's queue until they run, whatever the loop does.
func (r *Runtime) AttachHostLoop(post func(func())) {
	q := &r.hostJobs
	q.mu.Lock()
	q.post = post
	waiting := len(q.jobs) > 0 && !q.closed
	q.mu.Unlock()
	if waiting {
		post(r.runHostJobsForLoop)
	}
}

// HostJobsReady has a value when work from another goroutine is waiting for
// the runtime to run its jobs, or when the last piece of work that kept it
// busy has ended.
func (r *Runtime) HostJobsReady() <-chan struct{} { return r.hostJobs.ready }

// HostBusy reports whether a referenced piece of work is outstanding or a job
// from elsewhere is waiting.
func (r *Runtime) HostBusy() bool {
	q := &r.hostJobs
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.closed && (q.live > 0 || len(q.jobs) > 0)
}

// HostWork is a piece of a host's work that finishes on other goroutines, and
// keeps the runtime busy until it has: started on the runtime's goroutine,
// posted to and ended from any.
type HostWork struct {
	r *Runtime
	// ref is whether the work keeps the runtime busy, done whether it has
	// ended, and queued how many of its jobs wait. The queue's lock guards
	// them.
	ref, done bool
	queued    int
}

// isLive reports whether w keeps the runtime busy. The lock is held.
func (w *HostWork) isLive() bool { return w.ref && (!w.done || w.queued > 0) }

// update counts w in or out of the live works after a change to it, having
// been live or not before. The lock is held.
func (q *hostQueue) update(w *HostWork, was bool) {
	switch now := w.isLive(); {
	case was && !now:
		q.live--
		if q.live == 0 {
			q.signal()
		}
	case !was && now:
		q.live++
	}
}

// StartHostWork begins a piece of work, which keeps the runtime busy until it
// is done and its jobs have run.
func (r *Runtime) StartHostWork() *HostWork {
	w := &HostWork{r: r, ref: true}
	q := &r.hostJobs
	q.mu.Lock()
	if !q.closed {
		q.live++
	}
	q.mu.Unlock()
	return w
}

// PostOutcome is what became of a job posted to a piece of work.
type PostOutcome int

const (
	// Queued jobs run, or are cancelled if the runtime closes first.
	Queued PostOutcome = iota
	// Cancelled jobs were posted after the runtime closed: cancel has run.
	Cancelled
	// Refused jobs were posted after the work was done, and neither
	// function has run.
	Refused
)

// Post queues run for the runtime's goroutine, with cancel to call instead if
// the runtime closes before it runs. It is safe from any goroutine.
func (w *HostWork) Post(run, cancel func()) PostOutcome {
	q := &w.r.hostJobs
	q.mu.Lock()
	if w.done && !q.closed {
		q.mu.Unlock()
		return Refused
	}
	q.mu.Unlock()
	if w.r.enqueueHostJob(hostJob{run: run, cancel: cancel, work: w}) {
		return Queued
	}
	return Cancelled
}

// Done ends the work: nothing more is posted to it, and it keeps the runtime
// busy only until the jobs already posted have run. A second Done does
// nothing. It is safe from any goroutine.
func (w *HostWork) Done() {
	q := &w.r.hostJobs
	q.mu.Lock()
	if !w.done {
		was := w.isLive()
		w.done = true
		if !q.closed {
			q.update(w, was)
		}
	}
	q.mu.Unlock()
}

// SetRef sets whether the work keeps the runtime busy. It is safe from any
// goroutine.
func (w *HostWork) SetRef(ref bool) {
	q := &w.r.hostJobs
	q.mu.Lock()
	if w.ref != ref {
		was := w.isLive()
		w.ref = ref
		if !q.closed {
			q.update(w, was)
		}
	}
	q.mu.Unlock()
}
