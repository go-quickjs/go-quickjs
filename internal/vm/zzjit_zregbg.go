//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code compiled in the background, as the stack form's is
// (zzjit_background.go): the runtime's goroutine lowers the function's
// register code and asks its feedback ahead (jitRegSnapshotOf); a worker
// of the process's builds the function from the snapshot, optimizes it
// and runs the backend (jitRegJob.run), touching nothing of the
// runtime's; and the runtime's goroutine installs it at a later check
// (jitDrain), at a sample of the register tier's (jitRegHot), placing its
// code as the compile made on the spot would have. Meanwhile the function
// runs in the interpreter, or in its code that the compile replaces, which
// goes on running, valid, called by its native callers, as V8's does; a
// compile of a function deoptimized since is thrown away, as the stack
// form's is (jitReoptimized). A build that asked what the snapshot was not asked (a
// miss), or whose questions took back what they cannot, is built on the
// runtime's goroutine at the install instead.

// jitRegJob is a register compile in the background.
type jitRegJob struct {
	cl     *closure
	c      *regCode
	key    jitKey
	realm  *Realm
	callee bool
	// deopts is the function's deoptimizations when it was prepared: a
	// compile its function deoptimized since is thrown away, as the stack
	// form's is (jitReoptimized), what that taught asked for anew.
	deopts uint16
	// frameAt is the loop's header a frame waits at for the compile
	// (jitRegFrameAt), whose entry escape analysis keeps.
	frameAt int
	// miss has the worker leave the build as a miss (QJS_JIT_STRESS's
	// miss), built on the runtime's goroutine at the install.
	miss bool
	// e is the entry being made, its program p, rf its feedback, which a
	// build on the runtime's goroutine asks, snap that feedback asked ahead,
	// which the worker's build asks.
	e    *jitRegEntry
	p    *ir.Program
	rf   *jitRegFeedback
	snap *jitRegSnapshot
	// w is the compile's workspace, f the function built in it, enc and
	// opts the encoding and the backend's switches as they were when it
	// was prepared.
	w    *jit.Workspace
	f    *ssa.Func
	enc  abi.Encoding
	opts mir.Options
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
		// A derived constructor's this binding, past its captured
		// bindings, which native code finds in the context (abi.Context's
		// This), as the stack form's this slot.
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
		// on-stack replacement enters all the same, as the stack form's
		// enters any entry at a loop's header (tryJITAt): a header Go has
		// gone on at, whose entry the code keeps, its objects made there
		// (jitRegFeedback's Resumed, jitRegFramesLeave); and in a
		// generator's code, whose yields suspend it natively where a step
		// resumed it, every header: the build marks none of its entries
		// OSR's, none the escape analysis may drop (ssa.Func's
		// GenResumes).
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
	if s.regPendingJobs == nil {
		s.regPendingJobs = map[jitKey]*jitRegJob{}
	}
	s.regPendingJobs[j.key] = j
	s.regJobs = append(s.regJobs, j)
	if cc := j.cl.closureCode; cc != nil {
		// Its calls go to the JIT, which installs it once done
		// (jitRegEnter).
		cc.jitEnters = true
	}
	s.bgSubmitted++
	jitWorkers.submit(j)
	if r.jitStress.background && r.jitStress.checks == 0 {
		r.jitDrain()
	}
}

// run is j's work on a worker, as jitJob's: the build from the snapshot,
// the optimization and the backend, a panic of any a refusal.
func (j *jitRegJob) run() {
	defer close(j.done)
	if !j.state.CompareAndSwap(jitJobQueued, jitJobWorking) {
		jit.PutWorkspace(j.w)
		return
	}
	j.workRecovering()
	if !j.state.CompareAndSwap(jitJobWorking, jitJobDone) {
		jit.PutWorkspace(j.w)
	}
}

// workRecovering is work, a panic its refusal and a snapshot's miss its
// mark (missed).
func (j *jitRegJob) workRecovering() {
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
	if j.miss {
		// Left as a miss (QJS_JIT_STRESS's miss).
		j.missed = true
		return
	}
	f, err := ssa.BuildIn(&j.w.SSA, j.p, j.snap)
	if err != nil {
		j.buildErr = err
		return
	}
	f.FrameAt = j.frameAt
	j.f = f
	j.looped = jitRegShape(f, j.p, j.cl, j.c)
	ssa.Optimize(f)
	j.mc, j.err = mir.CompileWith(&j.w.MIR, f, j.enc, j.opts)
}

// jitRegDrainJobs installs the register compiles in the background that
// are done, as jitDrain installs the stack form's: under QJS_JIT_STRESS's
// background=K, each at the Kth check after it was handed over.
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
		s.regJobs = slices.Delete(s.regJobs, i, i+1)
		if s.regPendingJobs[j.key] == j {
			delete(s.regPendingJobs, j.key)
		}
		prev := r.Realm
		r.Realm = j.realm
		r.jitRegInstall(j)
		r.Realm = prev
	}
}

// jitRegAbandonJobs lets the runtime's register compiles in the
// background go, as it releases its JIT (abandonJobs).
func (s *jitState) jitRegAbandonJobs() {
	for _, j := range s.regJobs {
		if cc := j.cl.closureCode; cc != nil {
			cc.jitEnters = cc.jitReg != nil
		}
		for {
			st := j.state.Load()
			if st == jitJobDone {
				jit.PutWorkspace(j.w)
				j.w = nil
				break
			}
			if j.state.CompareAndSwap(st, jitJobAbandoned) {
				break
			}
		}
	}
	s.regJobs, s.regPendingJobs = nil, nil
}

// jitRegInstall is j's install on the runtime's goroutine: what the
// snapshot's questions appended to the feedback's lists put back, as the
// build from the feedback would have left them, and the code placed, as
// the compile on the spot places it (jitRegPlace); a build that missed is
// made here, from the feedback as it is now. The function's closure takes
// the code, as jitRegHot gives it, where it has none meanwhile.
func (r *Runtime) jitRegInstall(j *jitRegJob) {
	s := r.jit
	defer func() {
		if cc := j.cl.closureCode; cc != nil {
			// Its calls enter it if it has code, installed or not.
			cc.jitEnters = cc.jitReg != nil
		}
		if j.w != nil {
			jit.PutWorkspace(j.w)
			j.w = nil
		}
	}()
	e := j.e
	old := s.regCache[j.key]
	called := old != nil && s.regCalled(old)
	if j.key.gone() || e.life.deopts != j.deopts || called {
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
		// Built here, as on the spot, from feedback asked anew, as the
		// stack form's missed compile is (compileSSAFinish): the job's
		// was the snapshot's, whose questions' effects -- the closures
		// the code inlines, the objects and shapes it keeps -- the
		// snapshot took back, to replay those the build asks; asked again,
		// an instruction's Inline and NativeCalls answer from its cache
		// (jitRegFeedback's askedOf) without them, and code built from it
		// had inlined callees its keep did not name, which an exit inside
		// one could not find (jitRegInlined).
		s.regSnapMisses++
		s.compileBegin()
		limit := j.rf.fb.keepLimit
		j.rf = r.jitRegFeedbackFor(j.cl, j.p)
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
	default:
		j.snap.s.replay()
	}
	if e.refusal == "" && errors.Is(j.err, mir.ErrKeeps) {
		// As on the spot (jitRegCompile): built again here, its keep cells
		// limited to the context's.
		s.compileBegin()
		j.rf, j.f, j.looped, j.mc, j.err = r.jitRegBuild(j.w, j.cl, j.c, j.p, abi.MaxKeeps, j.frameAt)
		s.compileEnd()
	}
	if e.refusal == "" {
		if j.err != nil {
			e.refusal = fmt.Sprintf("%v: %v", jitRefused, j.err)
		} else if !r.jitRegPlace(e, j.c, j.rf, j.f, j.mc, j.looped, s.jitRegReplaced(j.key, 0)) {
			// No room: asked again at a later sample.
			s.bgDiscarded++
			return
		}
	}
	if e.code == nil && old != nil && old.code != nil && !old.invalid {
		// A refusal replaces no valid code (jitRegCompileFor), asked
		// again for nothing it has learned so far.
		old.life.reopt, old.life.learning = false, false
		return
	}
	r.jitRegCached(j.key, e)
	if cc := j.cl.closureCode; cc != nil && j.cl.reg == j.c {
		switch {
		case e.code == nil:
			cc.jitRegRefused = cc.jitRegRefused || !e.hostOnly && !j.callee
		case !cc.jitRegRefused:
			// The function's code from here, whatever it had: runs of
			// that go on in this at their next exit (jitRegGoOnAt).
			cc.setJITReg(e)
		}
	}
}
