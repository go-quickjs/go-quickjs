//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// Compiling in the background, as V8's concurrent recompilation does
// (internal/jit/README.md, "Tier-up and on-stack replacement"). A compile is a jitRegJob
// (jit_bg.go) in three steps: the runtime's goroutine lowers the
// function's register code and asks its feedback ahead
// (jitRegSnapshotOf); a worker of the process's builds, optimizes it, runs
// the backend and places the code apart (jitRegJob.run), touching nothing
// else of the runtime's; and the runtime's goroutine installs it at a later
// check (jitDrain, jitRegInstall), taking the code into its cache as the
// compile made on the spot would have. Meanwhile the function runs in the
// interpreter, or in its code, still valid, that the compile replaces.
//
// The runtime asks it with JITBackground, or QJS_JIT_STRESS's background
// does, which a test that sets its own stress (jitStress) leaves out, as
// it leaves out the budgets and deoptimizations; otherwise it waits for each
// compile, and its tiering is reproducible, which the tests counting
// compiles, the differential fuzzer and the placements need.

// jitJobsMax is how many compiles a runtime has in the background at once,
// as V8 queues at most eight of an isolate's: past it, a function due to be
// compiled waits in the interpreter, and is asked again at its next
// invocation.
const jitJobsMax = 4

// A background compile's state (jitRegJob), which hands it between its worker and the runtime's
// goroutine: queued for a worker, being worked, done, abandoned by its
// runtime, whose worker then gives its workspace back (jitRegJob.run), or
// taken back by its runtime before any worker began it (jitRegTakeBack).
const (
	jitJobQueued int32 = iota
	jitJobWorking
	jitJobDone
	jitJobAbandoned
	jitJobTaken
)

// jitInBackground reports whether r compiles fn in the background: a
// runtime that asks it, for a function's own code -- top-level code, which
// runs once, compiled at once -- and not inside another compile, nor while
// a host times the compiles (mir.Timing) or repeats them
// (JITCompileRepeat), which they do on the compiling goroutine, nor while a
// test looks at each compile's SSA (jitTestConfig's ssaOptimized). A
// compile there that
// finds the runtime with as many in the background as it may (jobsFull)
// waits, asked again at a later check, rather than be made on the spot.
func (r *Runtime) jitInBackground(fn *bytecode.Function) bool {
	return (r.jitBackground || r.jitStress.background) && !fn.TopLevel && r.jit != nil && r.jit.compiling == 0 && !r.jit.onSpot &&
		mir.Timing == nil && r.jitCompileRepeat() == 0 && r.jitTest.ssaOptimized == nil
}

// jobsFull reports whether the runtime has as many compiles in the
// background as it may, not counting those done that wait for their
// install (jitRegInstallWaits).
func (s *jitState) jobsFull() bool {
	n := 0
	for _, j := range s.regJobs {
		if !j.waiting {
			n++
		}
	}
	return n >= jitJobsMax
}

// inFlight reports whether the runtime has compiles or lowerings in the
// background, which a check installs (jitDrain).
func (s *jitState) inFlight() bool {
	return len(s.regJobs) != 0
}

// jitDrain installs the compiles in the background that are done, in the
// order they were handed over, at a check of the runtime's goroutine: a
// function's entry, a loop's back edge (jitRegDrainJobs). An install
// may find it is to wait (a native call of the code it replaces, under
// way), and is asked again at the next check. Under QJS_JIT_STRESS's
// background=K, each compile is installed at the Kth check after it was
// handed over, waited for if need be, and no other: when code is installed
// does not depend on how fast the workers are.
func (r *Runtime) jitDrain() {
	s := r.jit
	if s == nil || s.draining || !s.inFlight() {
		return
	}
	// An install compiles callees, and may hand over compiles of its own,
	// which this drain takes in turn, not one within it.
	s.draining = true
	defer func() { s.draining = false }()
	stress, k := r.jitScheduled(), int(r.jitStress.checks)
	r.jitRegDrainJobs(stress, k)
}

// abandonJobs lets the runtime's compiles in the background go, as it
// releases its JIT: their entries no longer wait for them, and each
// workspace goes back to the process's pool, by the worker if it still
// has it.
func (s *jitState) abandonJobs() {
	s.jitRegAbandonJobs()
}

// jitWorkers are the process's compile workers, shared by its runtimes:
// at most a quarter of GOMAXPROCS, at least one, each started when a
// compile is handed over and none is free, each ending once none is
// queued, so that an idle process runs none. A worker takes the queued
// compile whose function did most work in the interpreter since it was
// handed over, as HotSpot's compile queue serves the method invoked most
// since it was queued: one no longer run waits behind those that are, and
// its runtime drops it once it has gone unused for an epoch
// (jitRegDropStale).
var jitWorkers jitWorkerPool

type jitWorkerPool struct {
	mu      sync.Mutex
	queue   []jitTask
	running int
}

// jitTask is what a worker runs: a compile's work (jitRegJob), and the
// work its function has done waiting for it (jitRegJob's waited), which
// orders the queue.
type jitTask interface {
	run()
	waitedWork() int64
}

// jitWorkerLimit is how many workers compile at once.
func jitWorkerLimit() int { return max(1, runtime.GOMAXPROCS(0)/4) }

// submit queues j, starting a worker if fewer than the limit run.
func (p *jitWorkerPool) submit(j jitTask) {
	p.mu.Lock()
	p.queue = append(p.queue, j)
	start := p.running < jitWorkerLimit()
	if start {
		p.running++
	}
	p.mu.Unlock()
	if start {
		go p.work()
	}
}

// work runs the queued compiles, until none is left: of those queued, the
// one whose function did most work waiting (next).
func (p *jitWorkerPool) work() {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.running--
			p.mu.Unlock()
			return
		}
		j := p.next()
		p.mu.Unlock()
		j.run()
	}
}

// next takes the compile to run next, p.mu held: the one whose function did
// most work waiting for it, the first queued of those that did as much.
func (p *jitWorkerPool) next() jitTask {
	best, most := 0, p.queue[0].waitedWork()
	for i := 1; i < len(p.queue); i++ {
		if w := p.queue[i].waitedWork(); w > most {
			best, most = i, w
		}
	}
	j := p.queue[best]
	copy(p.queue[best:], p.queue[best+1:])
	p.queue[len(p.queue)-1] = nil
	p.queue = p.queue[:len(p.queue)-1]
	return j
}

// jitWorkerPanics counts the panics the process's workers made refusals
// of, whatever runtime's: one whose runtime never installs it is counted
// too, which the tests look for (TestMain).
var jitWorkerPanics atomic.Int64
