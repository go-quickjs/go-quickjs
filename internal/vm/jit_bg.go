//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code compiled in the background (jit_background.go): the
// runtime's goroutine lowers the function's register code and asks its
// feedback ahead (jitRegSnapshotOf); a worker of the process's builds the
// function from the snapshot, optimizes it, runs the backend and places
// the code apart from the code running meanwhile (jitRegJob.run), touching
// nothing else of the runtime's; and the runtime's goroutine installs it at
// a later check (jitDrain), at a sample of the register tier's (jitRegHot),
// taking the code placed into its cache as the compile made on the spot
// would have (jitRegPlaceWith). Meanwhile the function runs in the
// interpreter, or in its code that the compile replaces, which goes on
// running, valid, called by its native callers, as V8's does; a compile of
// a function deoptimized since is thrown away (jitRegJob's deopts). Code
// for a frame waiting at a loop (jitRegLoopCode) is compiled so too, and
// kept beside the function's code at its install (jitRegInstall's osrAt).
// A build that asked what the snapshot was not asked (a miss), or whose
// questions took back what they cannot, is built on the runtime's
// goroutine at the install instead.

// jitRegJob is a register compile in the background.
type jitRegJob struct {
	cl     *closure
	c      *regCode
	key    jitKey
	realm  *Realm
	callee bool
	// deopts is the function's deoptimizations when it was prepared: a
	// compile its function deoptimized since is thrown away, what that
	// taught asked for anew. asks is its marks to be compiled again for
	// what it learned (jitEntry's reoptAsks) when its feedback was asked,
	// ahead or at the install: one marked since was built without what it
	// learned, and is compiled again for it (jitRegInstall).
	deopts uint16
	asks   uint32
	// frameAt is the loop's header a frame waits at for the compile
	// (jitRegFrameAt), whose entry escape analysis keeps. osrAt, where not
	// 0, is that header for code kept for a loop (jitRegLoopCode), to be
	// kept beside osrBeside, the function's code it was asked beside,
	// if that is still the function's at the install (jitRegInstall).
	frameAt   int
	osrAt     int
	osrBeside *jitRegEntry
	// miss has the worker leave the build as a miss (QJS_JIT_STRESS's
	// miss), built on the runtime's goroutine at the install.
	miss bool
	// unstarted marks a job no worker is given (jitTestConfig's
	// bgUnstarted, QJS_JIT_STRESS's takeback).
	unstarted bool
	// asked counts the times its function asked for it again, each a
	// budget after the last (jitRegHot), which take it back at
	// jitTakeBackAsks if no worker has begun it (jitRegTakeBack).
	asked int
	// waited is the work its function did in the interpreter since it
	// was handed over, its samples' (jitRegHot), which the workers' queue
	// is served by (jitWorkerPool's next); seen is waited at the last tick
	// the runtime looked (jitRegDropStale), -1 before it first did.
	waited atomic.Int64
	seen   int64
	// waiting marks a job done whose install waits for the native calls
	// under way of the code it replaces (jitRegInstallWaits): kept, and
	// installed at a check after they are done, it is no longer one of
	// the compiles the background has room for (jobsFull).
	waiting bool
	// e is the entry being made, its program p, rf its feedback, which a
	// build on the runtime's goroutine asks, snap that feedback asked ahead,
	// which the worker's build asks.
	e    *jitRegEntry
	p    *ir.Program
	rf   *jitRegFeedback
	snap *jitRegSnapshot
	// links are the links rf's keep held as it was handed over, which,
	// with those its snapshot's questions took back, are all its worker
	// puts back (holdsLink, replay).
	links []*jitLink
	// arena is the runtime's arena, which the worker places the code in,
	// apart from the code running meanwhile (jit.NewSSACodeApart), placed
	// the code placed there.
	arena  *jit.Arena
	placed *jit.SSACode
	// shaped is what the placed code describes of itself, made by the
	// worker with it (jitRegShapeOf), which its install takes.
	shaped *jitRegShaped
	// w is the compile's workspace, taken by its worker (workRecovering),
	// f the function built in it, enc and opts the encoding and the
	// backend's switches as they were when it was prepared. backHook is
	// the test's hook of the workspace's give-back (jitTestConfig's
	// workspaceBack), replayHook its hook of the replay (bgReplayed).
	w          *jit.Workspace
	backHook   func(*jitRegJob)
	replayHook func(*jitRegJob)
	f          *ssa.Func
	enc        abi.Encoding
	opts       mir.Options
	// looped are the register pcs of the loops' headers the build has
	// entries at (jitRegShape).
	looped map[uint32]bool
	// mc is the backend's code, or err its refusal; buildErr the build's
	// refusal; missed marks a build that asked what snap was not asked;
	// panicked a refusal the worker made of a panic, allocErr the panic
	// where it was a failed check of the register allocation's.
	mc       *mir.Code
	err      error
	buildErr error
	missed   bool
	panicked bool
	allocErr *mir.AllocationError
	// took is what the worker's build and compile took (jitRegEntry's
	// took).
	took time.Duration
	// state hands the job between its worker and the runtime's goroutine
	// (jitJobQueued ...), done is closed once the worker is finished with
	// it, checks counts the checks it has waited (jitDrain).
	state  atomic.Int32
	done   chan struct{}
	checks int
}

// jitRegShape sets f, register code's function built from p, for its
// native code -- the register frame, its call entry -- and reports the
// register pcs of the loops' headers it has entries at.
func jitRegShape(f *ssa.Func, p *ir.Program, cl *closure, c *regCode) map[uint32]bool {
	// The register frame: its window's registers are its locals, in place
	// at the context's Locals; its captured bindings past them, in their
	// cells; its scratch slots past those, which are only values.
	f.FrameLocals = p.Regs
	f.Scratch = p.Locals - jitRegScratchBase(p)
	f.ThisSlot = -1
	if p.This {
		// A derived constructor's this binding, past its captured bindings,
		// which native code finds in the context (abi.Context's This).
		f.ThisSlot = p.UpvalueBase + p.Upvalues
	}
	f.Params, f.StartOnly = p.ParamBase+p.Params, false
	// Its bindings undefined at its call entry, as the interpreter's call
	// makes them, not its temporaries: each a pair of stores at every
	// call, ten of them a call of fib's.
	f.InitEnd = f.Params + c.rc.Inits
	f.Cells = p.Cells
	// Native callers call it (jitRegFeedback's NativeCalls), its window
	// made past theirs: its call entry makes its frame (mir's callEntry).
	f.CallEntry = true
	// And another function's call in tail position, its frame given up,
	// jumps to its tail entry (mir's windowTailJump).
	f.TailEntry = true
	// A generator's code a loop's step may resume natively: its generator
	// entry (mir's windowGenEntry).
	jitRegGenShape(f, p, cl, c)
	looped := map[uint32]bool{}
	var headers []bool
	for _, en := range f.Entries {
		if en.OSR {
			looped[p.Origin[en.PC]] = true
			continue
		}
		// An entry at a loop's header the build did not mark OSR's, which
		// on-stack replacement enters all the same: a header Go has gone on
		// at, whose entry the code keeps, its objects made there
		// (jitRegFeedback's Resumed, jitRegFramesLeave); and in a
		// generator's code, whose yields suspend it natively where a step
		// resumed it, every header: the build marks none of its entries
		// OSR's, none the escape analysis may drop (ssa.Func's GenResumes).
		if headers == nil {
			headers = make([]bool, len(p.Code)+1)
			for pc, in := range p.Code {
				if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc {
					headers[in.Target] = true
				}
			}
		}
		if en.PC > 0 && headers[en.PC] {
			looped[p.Origin[en.PC]] = true
		}
	}
	return looped
}

// jitRegScratchBase is the first of p's expansions' scratch slots, past its
// captured bindings' and a derived constructor's this slot (ir.Program's
// This).
func jitRegScratchBase(p *ir.Program) int {
	n := p.UpvalueBase + p.Upvalues
	if p.This {
		n++
	}
	return n
}

// jitRegSubmit hands j, its feedback asked ahead, to the process's
// workers.
func (r *Runtime) jitRegSubmit(j *jitRegJob) {
	s := r.jit
	j.done = make(chan struct{})
	s.regJobs = append(s.regJobs, j)
	if j.osrAt != 0 {
		// Code for a loop, which the function's code it is asked beside
		// waits for: its frames waiting at the loop go on in the
		// interpreter meanwhile (jitRegLoopCode).
		j.osrBeside.osrJob = j
	} else {
		if s.regPendingJobs == nil {
			s.regPendingJobs = map[jitKey]*jitRegJob{}
		}
		s.regPendingJobs[j.key] = j
		if cc := j.cl.closureCode; cc != nil {
			// Its calls go to the JIT, which installs it once done
			// (jitRegEnter).
			cc.jitEnters = true
		}
	}
	s.bgSubmitted++
	j.seen = -1
	// Under QJS_JIT_STRESS's takeback, no worker is given a compile its
	// function waits for in the interpreter, which it takes back
	// (jitRegTakeBack); one of code that runs meanwhile, compiled again,
	// is the workers'.
	old := s.regCache[j.key]
	waits := j.osrAt == 0 && (old == nil || old.invalid || old.code == nil)
	if j.unstarted = r.jitTest.bgUnstarted || r.jitStress.background && r.jitStress.takeBack && waits; !j.unstarted {
		jitWorkers.submit(j)
	}
	if r.jitTest.bgDone {
		<-j.done
	}
	if r.jitScheduled() && r.jitStress.checks == 0 {
		r.jitDrain()
	}
}

// waitedWork is the work j's function did waiting for it (jitTask).
func (j *jitRegJob) waitedWork() int64 { return j.waited.Load() }

// jitRegWaited counts work, the work of a sample of cl's function, toward
// its compile in the background, if one is queued (jitRegJob's waited).
func (r *Runtime) jitRegWaited(cl *closure, work int64) {
	if j := r.jit.regPendingJobs[r.jitRegKeyOf(cl)]; j != nil {
		j.waited.Add(work)
	}
	if j := r.jit.regAheadRoots[r.jitRegKeyOf(cl)]; j != nil {
		// Or its lowering ahead (jitRegLowerAhead).
		j.waited.Add(work)
	}
}

// jitRegDropStale drops the runtime's compiles in the background that no
// worker has begun and whose functions did no work since the last tick
// it looked (flush), an epoch of the program's running or more, as
// HotSpot's compile queue drops a method no longer invoked: what the
// workers would compile is what runs. Each asks for its compile again once
// it has spent a budget, as before it was compiled. Not under
// QJS_JIT_STRESS's background, whose installs come at the checks it says,
// nor code for a loop (jitRegLoopCode), which its function's code waits
// for.
func (r *Runtime) jitRegDropStale() {
	s := r.jit
	if r.jitScheduled() || s.draining {
		// Nor in an install, whose drain goes through the jobs.
		return
	}
	for i := 0; i < len(s.regJobs); {
		j := s.regJobs[i]
		w := j.waited.Load()
		if j.osrAt != 0 || j.seen < 0 || w != j.seen || !j.state.CompareAndSwap(jitJobQueued, jitJobAbandoned) {
			j.seen = w
			i++
			continue
		}
		s.regJobs = slices.Delete(s.regJobs, i, i+1)
		if s.regPendingJobs[j.key] == j {
			delete(s.regPendingJobs, j.key)
		}
		if cc := j.cl.closureCode; cc != nil {
			cc.jitEnters = cc.jitReg != nil
		}
		if j.snap != nil && !j.key.gone() {
			// Dropped, no worker having begun it: derived here for what its
			// asking appends to the function's learning (finish).
			j.snap.s.finish()
		}
		j.snap = nil
		if j.unstarted {
			j.letGo()
			close(j.done)
		}
		s.bgDropped++
	}
}

// run is j's work on a worker: the build from the snapshot, the
// optimization and the backend, a panic of any a refusal. A job its
// runtime abandoned meanwhile gives its workspace back; one it took back
// (jitRegTakeBack) is the runtime's.
func (j *jitRegJob) run() {
	if !j.state.CompareAndSwap(jitJobQueued, jitJobWorking) {
		if j.state.Load() == jitJobAbandoned {
			j.letGo()
			close(j.done)
		}
		return
	}
	defer close(j.done)
	j.workRecovering()
	if !j.state.CompareAndSwap(jitJobWorking, jitJobDone) {
		j.letGo()
	}
}

// letGo gives back what j holds that no install took: its workspace,
// unrewound, for its next taker to rewind (jit.ReturnWorkspace), and the
// code its worker placed.
func (j *jitRegJob) letGo() {
	if j.w != nil {
		jit.ReturnWorkspace(j.w)
		j.w = nil
	}
	j.closePlaced()
}

// letGoApart is letGo at the install, on the runtime's goroutine, once
// nothing built in the workspace is used -- the function, the backend's
// code, whose bytes on arm64 are the workspace's until placed: the code
// placed and the entry made of it, or thrown away. The workspace goes back
// to the pool unrewound (jit.ReturnWorkspace), and a worker taking it for
// its next compile rewinds it (jit.GetWorkspaceOnWorker): the rewind, a
// clear of what the compile built, had been more than half of an
// install's time, 37 of 72 us a compile of Octane's Typescript, and
// handed to a worker as a task of its own, a task queued and often a
// worker started, 3.6 of the 14.5 us left.
func (j *jitRegJob) letGoApart() {
	if w := j.w; w != nil {
		if h := j.backHook; h != nil {
			h(j)
		}
		j.w = nil
		jit.ReturnWorkspace(w)
	}
	j.closePlaced()
}

// closePlaced lets go of the code j's worker placed, which no install
// took.
func (j *jitRegJob) closePlaced() {
	if j.placed != nil {
		j.placed.Close()
		j.placed = nil
	}
}

// workRecovering is work, a panic its refusal and a snapshot's miss its
// mark (missed).
func (j *jitRegJob) workRecovering() {
	from := time.Now()
	defer func() { j.took = time.Since(from) }()
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(jitSnapMiss); ok {
				j.missed, j.f = true, nil
				return
			}
			j.mc, j.err, j.panicked = nil, fmt.Errorf("jit: the compiler panicked: %v", v), true
			j.allocErr, _ = v.(*mir.AllocationError)
			jitWorkerPanics.Add(1)
		}
	}()
	if j.w == nil {
		// Taken here, on the worker or on the runtime's goroutine that
		// took the compile back (jitRegTakeBack), not as it was prepared.
		j.w = jit.GetWorkspaceOnWorker()
	}
	if j.miss {
		// Left as a miss (QJS_JIT_STRESS's miss).
		j.missed = true
		return
	}
	// The answers derived from what the snapshot gathered, here, off the
	// runtime's goroutine (jit_snapderive.go).
	j.snap.s.derive()
	f, err := ssa.BuildIn(&j.w.SSA, j.p, j.snap)
	if err != nil {
		j.buildErr = err
		return
	}
	f.FrameAt = j.frameAt
	j.f = f
	// What the build's questions appended to the feedback's lists, taken
	// back, put back here as the build from the feedback would have left
	// them: the compile's own feedback, the root's, its keep's and its
	// callees', which the runtime's goroutine does not read until the
	// install (holdsLink reads the links it held at the hand-over). At
	// the install, it had been about 12 of the 40 us it took on Octane's
	// Typescript.
	j.snap.s.replay()
	if h := j.replayHook; h != nil {
		h(j)
	}
	// And its keep's links each once, here, as the code's install holds
	// them (regHoldLinks).
	j.rf.fb.keep().linksOnly()
	j.looped = jitRegShape(f, j.p, j.cl, j.c)
	ssa.Optimize(f)
	j.mc, j.err = mir.CompileWith(&j.w.MIR, f, j.enc, j.opts)
	if j.err == nil && j.arena != nil {
		// Placed here, apart from the code the runtime's goroutine runs
		// meanwhile: its install no more than a store of the code. Placed
		// there, each compile had sealed its pages, a few system calls, on
		// the program's time. Where it cannot be placed, the install
		// places it, as on the spot.
		j.placed, _ = jit.NewSSACodeApart(j.arena, j.mc)
		if j.placed != nil {
			// And its shape made of it here, which its install takes.
			j.shaped = jitRegShapeOf(j.c, j.p, f, j.mc, j.placed, j.looped)
		}
	}
}

// jitTakeBackAsks is how many times a function asks for its compile again,
// each after a budget more spent in the interpreter (jitRegHot), before a
// compile no worker has begun is taken back (jitRegTakeBack). A compile
// takes about as long as the interpreter spends on two or three of its
// function's budgets, so that the wait is bounded by what the compile on
// the spot costs: no worker free -- every core busy, a host running a
// runtime on each -- costs no more than about twice what compiling on the
// runtime's goroutine would.
const jitTakeBackAsks = 2

// jitRegTakeBack compiles j, a compile in the background that no worker has
// begun, on the runtime's goroutine, and installs it at once, as the
// compile on the spot would be: it reports whether it took it. A worker
// that comes to it after leaves it (run). Not under QJS_JIT_STRESS's
// background, whose installs come at the checks it says, nor for code kept
// for a loop (jitRegLoopCode), whose frame goes on in the interpreter.
func (r *Runtime) jitRegTakeBack(j *jitRegJob) bool {
	if j.osrAt != 0 || r.jitScheduled() || !j.state.CompareAndSwap(jitJobQueued, jitJobTaken) {
		return false
	}
	s := r.jit
	s.bgTakenBack++
	s.compileBegin()
	if clock := r.jitCompileClock(); clock != nil && !s.timingGoroutine {
		from := clock()
		j.workRecovering()
		s.goroutineTimes.TakenBack += clock() - from
		s.goroutineTimes.TakeBacks++
	} else {
		j.workRecovering()
	}
	s.compileEnd()
	// Done, which a worker coming to it after leaves as it is.
	j.state.Store(jitJobDone)
	close(j.done)
	r.jitDrain()
	return true
}

// takePlaced is the code j's worker placed for j.mc, which j no longer
// holds from then on, or nil for none.
func (j *jitRegJob) takePlaced() *jit.SSACode {
	p := j.placed
	j.placed = nil
	return p
}

// jitRegDrainJobs installs the register compiles in the background that are
// done (jitDrain): under QJS_JIT_STRESS's background=K, each at the Kth
// check after it was handed over.
func (r *Runtime) jitRegDrainJobs(stress bool, k int) {
	s := r.jit
	for i := 0; i < len(s.regJobs); {
		j := s.regJobs[i]
		if stress {
			if j.checks++; j.checks < k {
				i++
				continue
			}
			<-j.done
		} else if j.state.Load() != jitJobDone {
			i++
			continue
		}
		if r.jitRegInstallWaits(j) {
			j.waiting = true
			i++
			continue
		}
		s.regJobs = slices.Delete(s.regJobs, i, i+1)
		if s.regPendingJobs[j.key] == j {
			delete(s.regPendingJobs, j.key)
		}
		prev := r.Realm
		r.Realm = j.realm
		if clock := r.jitCompileClock(); clock != nil && !s.timingGoroutine {
			// The runtime goroutine's part of it (JITGoroutineTimes), the
			// compiles the install makes among it.
			s.timingGoroutine = true
			from, hooks := clock(), s.hookTime
			r.jitRegInstall(j)
			s.goroutineTimes.Installed += clock() - from - (s.hookTime - hooks)
			s.goroutineTimes.Installs++
			s.timingGoroutine = false
		} else {
			r.jitRegInstall(j)
		}
		r.Realm = prev
	}
}

// jitRegInstallWaits reports whether j, a compile in the background that is
// done, is to wait for its install: the code it replaces, which the cache
// holds still, is run by a native call under way, which finds its code in
// the cache (jitRegLevelAt) -- the function neither gone nor deoptimized
// since, which would throw it away. A compile on the spot is not made
// meanwhile (jitRegForAs); one in the background had been thrown away and
// asked again, built a second time and as often thrown away again: Are We
// Fast Yet's DeltaBlue, whose functions are mostly under way in recursive
// native calls, threw away 77 of its 201 compiles, a third longer in the
// background than on the spot.
func (r *Runtime) jitRegInstallWaits(j *jitRegJob) bool {
	s := r.jit
	if j.osrAt != 0 || j.key.gone() || j.e.life.deopts != j.deopts {
		return false
	}
	old := s.regCache[j.key]
	return old != nil && s.regCalled(old)
}

// jitRegAbandonJobs lets the runtime's register compiles in the
// background go, as it releases its JIT (abandonJobs).
func (s *jitState) jitRegAbandonJobs() {
	for _, j := range s.regJobs {
		if cc := j.cl.closureCode; cc != nil {
			cc.jitEnters = cc.jitReg != nil
		}
		if b := j.osrBeside; b != nil && b.osrJob == j {
			b.osrJob = nil
		}
		for {
			st := j.state.Load()
			if st == jitJobDone {
				j.letGo()
				break
			}
			if j.state.CompareAndSwap(st, jitJobAbandoned) {
				if j.unstarted {
					// No worker will see it.
					j.letGo()
					close(j.done)
				}
				break
			}
		}
	}
	s.regJobs, s.regPendingJobs = nil, nil
	// The lowerings ahead, which hold nothing of the runtime's: their
	// workers make them for none.
	s.regAhead, s.regAheadRoots = nil, nil
}

// jitRegInstall is j's install on the runtime's goroutine: the code
// placed, as the compile on the spot places it (jitRegPlace), with the
// feedback's lists as the build from the feedback would have left them,
// which its worker put back (workRecovering); a build that missed is made
// here, from the feedback as it is now. The function's closure takes
// the code, as jitRegHot gives it, where it has none meanwhile.
func (r *Runtime) jitRegInstall(j *jitRegJob) {
	s := r.jit
	// Ended with no more than the budget held, whatever came of it: a look
	// meanwhile leaves it to the end (keepWithin), where the code placed is
	// settled (below), and anything else kept within.
	s.settleAfter++
	defer func() {
		s.settleAfter--
		s.keepWithin()
	}()
	// The links the job's feedback asked for are let go of by none until
	// its code holds them (regUnlink).
	defer s.regBuilding(&j.rf)()
	defer func() {
		if cc := j.cl.closureCode; cc != nil {
			// Its calls enter it if it has code, installed or not.
			cc.jitEnters = cc.jitReg != nil
		}
		// What the install took not: the workspace, and the code placed
		// where the install threw it away or built it again.
		j.letGoApart()
		// Nor the snapshot, its copies and its answers, done with: a job
		// a runtime's state keeps past its install keeps no more of it.
		j.snap = nil
	}()
	e := j.e
	e.took += j.took
	if j.snap != nil && !j.key.gone() {
		// What its derivations deferred of the function's learning, written
		// whatever comes of the compile, as the asking had written it
		// when the compile was prepared (jitSnapFinishers).
		j.snap.s.finish()
	}
	old := s.regCache[j.key]
	if j.osrAt != 0 {
		if b := j.osrBeside; b.osrJob == j {
			b.osrJob = nil
		}
		if j.key.gone() || e.life.deopts != j.deopts || old != j.osrBeside || old.invalid || old.code == nil || old.dropped {
			// Gone; deoptimized since; or the code it was asked beside
			// compiled again, invalid or let go of: frames in the
			// function's loops are the new code's to learn of.
			s.bgDiscarded++
			return
		}
	} else if called := old != nil && s.regCalled(old); j.key.gone() || e.life.deopts != j.deopts || called {
		// Gone; or deoptimized since, compiled for what it has learned
		// once a budget is spent (jitRegInvalidate); or run by a native
		// call under way, which finds its code in the cache (jitRegLevelAt),
		// asked again for what it learned once that is due.
		s.bgDiscarded++
		if called && old.code != nil && !old.invalid {
			r.jitRegReopt(j.cl, old)
		}
		return
	}
	if j.allocErr != nil {
		// A failed check of the allocation's (mir.CheckAllocations, which
		// tests set) ends the run as it does a compile on the spot.
		panic(j.allocErr)
	}
	switch {
	case j.panicked:
		s.backendPanics++
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, j.err)
	case j.missed:
		// Built here, as on the spot, from feedback asked anew: the job's
		// was the snapshot's, whose questions' effects -- the closures the
		// code inlines, the objects and shapes it keeps -- the snapshot
		// took back, to replay those the build asks; asked again, an
		// instruction's Inline and NativeCalls answer from its cache
		// (jitRegFeedback's askedOf) without them, and code built from it
		// had inlined callees its keep did not name, which an exit inside
		// one could not find (jitRegInlined).
		s.regSnapMisses++
		s.compileBegin()
		limit := j.rf.fb.keepLimit
		j.rf, j.asks = r.jitRegFeedbackFor(j.cl, j.p), e.life.reoptAsks
		j.rf.fb.keepLimit = limit
		f, err := ssa.BuildIn(&j.w.SSA, j.p, j.rf)
		if err != nil {
			e.refusal = fmt.Sprintf("%v: %v", jitRefused, err)
		} else {
			f.FrameAt = j.frameAt
			j.looped = jitRegShape(f, j.p, j.cl, j.c)
			ssa.Optimize(f)
			j.f = f
			j.mc, j.err = mir.CompileWith(&j.w.MIR, f, j.enc, j.opts)
		}
		s.compileEnd()
	case j.buildErr != nil:
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, j.buildErr)
	}
	if e.refusal == "" && errors.Is(j.err, mir.ErrKeeps) {
		// As on the spot (jitRegCompile): built again here, its keep cells
		// limited to the context's.
		s.compileBegin()
		j.rf, j.f, j.looped, j.mc, j.err = r.jitRegBuild(j.w, j.cl, j.c, j.p, abi.MaxKeeps, j.frameAt)
		j.asks = e.life.reoptAsks
		s.compileEnd()
	}
	// Marked to be compiled again for what it learned since its feedback
	// was asked: what the old code met while this was compiled, which the
	// code installed was not built for.
	learned := e.life.reoptAsks != j.asks
	if e.refusal == "" {
		if j.err != nil {
			e.refusal = fmt.Sprintf("%v: %v", jitRefused, j.err)
		} else if !r.jitRegPlaceWith(e, j.c, j.rf, j.f, j.mc, j.looped, s.jitRegReplaced(j.key, j.osrAt), j.takePlaced(), j.shaped) {
			// No room: asked again at a later sample.
			s.bgDiscarded++
			return
		} else if h := r.jitTierHook(); h != nil {
			r.jitTierPlaced(h, j.cl, j.c, e)
		}
	}
	if j.osrAt != 0 {
		if e.code != nil {
			// Kept beside the function's code, which a frame waiting at
			// the loop goes on in at its next check (jitRegLoopCode); a
			// refusal keeps nothing, as on the spot.
			s.regKeepOSR(old, e)
		}
		return
	}
	if e.code == nil && old != nil && old.code != nil && !old.invalid {
		// A refusal replaces no valid code (jitRegCompileFor), asked
		// again for nothing it learned before its feedback was asked.
		old.life.reopt, old.life.learning = learned, false
		return
	}
	r.jitRegCached(j.key, e)
	if learned && e.code != nil {
		// Compiled again once it has spent a budget, as code that learned
		// is (jitRegLearnedDue): React's createElement, compiled again in
		// the background, had met "div" at its read of a.defaultProps
		// meanwhile, learned for good (jitPolySite's prim), and its new
		// code, built without it, left native code at every such read --
		// ssr's Render 348,000 times, the rest of each invocation run in
		// the interpreter -- learning nothing more.
		e.life.reopt = true
	}
	// No more than the budget held after, as after a compile on the spot
	// (jitRegCompileFor): what the code describes, and what its callees
	// learned of it as callers joined them, count past the room it asked.
	defer s.settle()
	if cc := j.cl.closureCode; cc != nil && j.cl.reg == j.c {
		switch {
		case e.code == nil:
			cc.jitRegRefused = cc.jitRegRefused || !e.hostOnly && !j.callee
			s.regForgetTopLevel(e)
		case !cc.jitRegRefused:
			// The function's code from here, whatever it had: runs of
			// that go on in this at their next exit (jitRegGoOnAt).
			cc.setJITReg(e)
		}
	}
}
