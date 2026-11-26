//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A loop's step over a register generator, natively (jit_genstep.go's
// for register code).
//
// A for...of's step, or a call of the realm's next, over a generator of
// register code is a native call of the generator's code (ssa.CallSite's
// Gens): the step checks the generator and jumps to its code's generator
// entry (mir's windowGenEntry), which makes the generator's frame -- a
// window of registers past the VM's stack's top, as a native callee's, from
// the generator's values (genFrom on) and its callee and receiver below
// them, the value sent in to the yield's register -- and goes on at the
// entry for the generator's place. A yield there writes the window back to
// the generator's values and returns the value to the step (mir's
// windowYieldExit); a return finishes it. The window is made and written
// back because register code keeps a generator's registers in its values
// only between its runs (regGenRun), its frame a window in the VM's stack
// while it runs, which register code's native code addresses.
//
// Anything else leaves native code as a native callee does, and Go
// finishes the level as regGenRun finishes a resumption (jitRegGenDone):
// the window written back where it suspends, the step's result the value,
// or its done.

// jitRegGenCallable reports whether a loop's step or a call of next may
// resume a generator of cl's natively: register code's synchronous
// generator with no yield*, no call in tail position, no local a closure
// captures -- no capture open on its window's registers, which the
// generator would keep (regMoveUps) -- and no arguments kept, whose mapped
// object aliases the parameters' registers by captures too.
func jitRegGenCallable(cl *closure) bool {
	c := cl.reg
	return c != nil && c.wide && c.suspends && !c.args && c.rc == cl.fn.RegCode() && jitcompile.GeneratorResumable(cl.fn)
}

// regGenNative reports whether a generator of fn, of register code c, is
// one a loop's step may resume natively (jitRegGenCallable): its values
// then keep its whole register file (regCode's genFrom 0), which its native
// code runs on as its registers (mir's windowGenFrame).
func regGenNative(fn *bytecode.Function, c *regCode) bool {
	return !c.args && jitcompile.GeneratorResumable(fn)
}

// jitRegGenShape gives f, built from p of cl's register code c, its
// generator entry where a loop's step may resume its generators natively
// (jitRegGenCallable): the places it resumes them at (jitRegGenResumes),
// its registers the generator's values, which keep them all (genFrom 0,
// a synchronous generator's).
func jitRegGenShape(f *ssa.Func, p *ir.Program, cl *closure, c *regCode) {
	if cl == nil || c == nil || !jitRegGenCallable(cl) || p.Cells || c.genFrom != 0 {
		return
	}
	f.GenResumes = jitRegGenResumes(c.rc, p, f)
	f.GenEntry = len(f.GenResumes) != 0
}

// jitRegGenResumes are the places a step resumes a generator of register
// code rc natively at (ssa.Func's GenResumes), where p, built as f, has
// an entry: past end_params, its first, and, after each yield, the target
// of the jump after it, where a resumption by next goes on (regGenResume),
// the value sent in to the yield's register.
func jitRegGenResumes(rc *bytecode.RegCode, p *ir.Program, f *ssa.Func) []ssa.GenResume {
	has := func(pc int) bool {
		for _, e := range f.Entries {
			if e.PC == pc {
				return true
			}
		}
		return false
	}
	var out []ssa.GenResume
	for rpc, w := range rc.Code {
		var gr ssa.GenResume
		switch w.Op() {
		case bytecode.REndParams:
			gr = ssa.GenResume{PC: p.At(uint32(rpc + 1)), Place: rpc + 1, Sent: -1}
		case bytecode.RYield:
			if rpc+1 >= len(rc.Code) || rc.Code[rpc+1].Op() != bytecode.RJmp {
				continue
			}
			t := rpc + 2 + int(regJumpOff(rc.Code[rpc+1]))
			gr = ssa.GenResume{PC: p.At(uint32(t)), Place: rpc + 1, Sent: int(w.A()), Started: true}
		default:
			continue
		}
		if gr.PC < 0 || !has(gr.PC) {
			continue
		}
		out = append(out, gr)
		if len(out) == jitGenResumesMax {
			break
		}
	}
	return out
}

// regGenSteps is GenSteps by register pc rpc: the generators the step or
// the call of next there has met (jitRegGenSeen), each of the code's
// environment and realm, called through its function's link.
func (f *jitRegFeedback) regGenSteps(rpc int) []ssa.GenStep {
	fb := f.fb
	e := fb.e
	if e == nil || fb.cl == nil {
		return nil
	}
	for _, st := range e.genSteps {
		if int(st.pc) != rpc {
			continue
		}
		var out []ssa.GenStep
		for _, cl := range st.cls {
			if cl.env != fb.cl.env || cl.realm != fb.cl.realm || !jitRegGenCallable(cl) {
				continue
			}
			link := fb.r.jitState().regLinkOf(cl, cl.reg)
			k := fb.keep()
			k.links = append(k.links, link)
			out = append(out, ssa.GenStep{
				Fn: uintptr(unsafe.Pointer(cl.fn)), Env: uintptr(unsafe.Pointer(cl.env)), Realm: uintptr(unsafe.Pointer(cl.realm)),
				Entry: uintptr(unsafe.Pointer(&link.gen)), Count: uintptr(unsafe.Pointer(&link.in)),
			})
		}
		return out
	}
	return nil
}

// regGenNextSite gives site, the call of Go of call_method_w at register pc
// rpc, of no argument or one, the generators it learned it resumes with the
// realm's next (jitRegGenCallSeen): made natively, its result objects from
// a pool.
func (f *jitRegFeedback) regGenNextSite(rpc int, site *ssa.CallSite) {
	if !site.Method || site.New || site.Argc > 1 {
		return
	}
	gens := f.regGenSteps(rpc)
	if len(gens) == 0 {
		return
	}
	fb := f.fb
	pool := fb.r.jitGenResultPool(fb.e, rpc)
	k := fb.keep()
	k.pools = append(k.pools, pool)
	site.Gens, site.GenNext, site.Pool = gens, fb.GenNext(), uintptr(unsafe.Pointer(pool))
}

// jitRegGenStepSeen notes a loop's step at register pc rpc of f's code e,
// which Go made from native code, of cursor: one over a generator a step
// may resume natively.
func (r *Runtime) jitRegGenStepSeen(f *frame, e *jitRegEntry, rpc int, cursor Value) {
	st := cursorOf(cursor)
	if st == nil || st.done || st.arr != nil || st.forIn || !st.iter.IsObject() || r.genNextFn == nil ||
		st.next.ref != unsafe.Pointer(r.genNextFn) {
		return
	}
	r.jitRegGenSeen(f, e, rpc, st.iter.Object())
}

// jitRegGenCallSeen notes a method call at register pc rpc of f's code e,
// which Go made from native code, of callee on this with argc arguments:
// the realm's generators' next, of no argument or one, on a generator a
// call may resume natively.
func (r *Runtime) jitRegGenCallSeen(f *frame, e *jitRegEntry, rpc int, this, callee Value, argc int) {
	if argc > 1 || r.genNextFn == nil || callee.ref != unsafe.Pointer(r.genNextFn) || !this.IsObject() {
		return
	}
	r.jitRegGenSeen(f, e, rpc, this.Object())
}

// jitRegGenSeen is jitRegGenStepSeen's and jitRegGenCallSeen's: it, a
// generator of register code a native step may resume, of f's environment
// and realm, learned at rpc, which the code resumes natively once compiled
// again (jitRegReopt).
func (r *Runtime) jitRegGenSeen(f *frame, e *jitRegEntry, rpc int, it *Object) {
	cl := f.cl
	if it.class != ClassGenerator || cl == nil || e == nil || e.invalid {
		return
	}
	g := genOf(it)
	if !g.reg || g.async || g.cl == nil || g.cl.env != cl.env || g.cl.realm != cl.realm || g.cl.realm != nil && g.cl.realm != r.Realm ||
		!jitRegGenCallable(g.cl) {
		return
	}
	l := r.jit.regLearnedOf(r.jitRegKeyOf(cl))
	for i := range l.genSteps {
		gs := &l.genSteps[i]
		if int(gs.pc) != rpc {
			continue
		}
		for _, c := range gs.cls {
			if c.fn == g.cl.fn {
				return
			}
		}
		if len(gs.cls) >= jitGenTargets {
			return
		}
		gs.cls = append(gs.cls, g.cl)
		r.jitRegGenLearned(cl, e)
		return
	}
	l.genSteps = append(l.genSteps, jitGenStep{pc: int32(rpc), cls: []*closure{g.cl}})
	r.jitRegGenLearned(cl, e)
}

// jitRegGenLearned asks for e, cl's code, to be compiled again for a
// generator's step or making it learned (jitRegReopt), marked so that the
// loops it was judged slow for, whose steps had each been a call of Go, are
// judged afresh once it is (jitRegCached).
func (r *Runtime) jitRegGenLearned(cl *closure, e *jitRegEntry) {
	if e.life != nil {
		e.life.genLearned = true
	}
	r.jitRegReopt(cl, e)
}

// jitRegSuspend suspends f, a generator's or an async function's frame
// running c with its window at wb, at register pc rpc, a yield, a yield*,
// an await or the end of the parameters its native code left at, as the
// interpreter's yield and regSuspend do: the pc past the instruction kept
// for the resumption, which enters native code there (regGenRun), and the
// value handed out. It returns the suspension, the runtime's own, which
// the frame's run ends in as the interpreter's would, or nil where it does
// not suspend: an await its job goes on past in place (regAwaitsInPlace),
// which regWhole runs before this is asked, and any other instruction.
// Left to the interpreter, each suspension was a run ended there, the
// frame handed to runReg only to suspend: generator_busy's every yield.
func (r *Runtime) jitRegSuspend(f *frame, c *regCode, wb, rpc int) *suspendSignal {
	if c == nil || rpc < 0 || rpc >= len(c.rc.Code) {
		return nil
	}
	w := regAt(c.code, rpc)
	op := w.Op()
	switch op {
	case bytecode.RYield:
		sig := &r.treeSig
		*sig = suspendSignal{value: r.stack[wb+int(w.B())]}
		f.savedSP = rpc + 1
		return sig
	case bytecode.RAwait:
		if r.regAwaitsInPlace(f, w, wb) {
			// Gone on from in place: regWhole runs it (jitRegRun).
			return nil
		}
	case bytecode.RYieldStar, bytecode.REndParams:
	default:
		return nil
	}
	sig := &r.treeSig
	*sig = suspendSignal{
		value:    Undefined,
		await:    op == bytecode.RAwait,
		delegate: op == bytecode.RYieldStar,
		raw:      op == bytecode.RYieldStar && !c.fn.Async,
	}
	if op != bytecode.REndParams {
		sig.value = r.stack[wb+int(w.B())]
	}
	f.savedSP = rpc + 1
	return sig
}

// jitRegGenLevel marks l, a level of register code, the resumption of a
// generator a loop's step or a call of next made natively (jitGenLevel):
// its generator, and the step's cursor, nil for a call of next's. Its
// registers, the generator's values while native code runs it (mir's
// windowGenFrame), are given the window the code made room for at its
// base, the level's, which Go reads and writes as any level's: copied
// there the first time Go finds the level, its context's Locals the
// window's from then on -- where native code going on reads them, and
// from which a yield writes them back (mir's windowYieldExit) -- and an
// exit's description written there (jit.RebaseExitLocals).
func (r *Runtime) jitRegGenLevel(l *jitRegLevel, c *abi.Context) {
	g := jitGenLevel(c, l.cl, abi.ExitHost)
	if g == nil || !g.reg {
		return
	}
	l.gen, l.cursor = g, jitGenCursor(c)
	n := int(l.c.numRegs)
	if l.c.genFrom != 0 || len(g.vals) < n || n == 0 {
		return
	}
	vals := unsafe.Pointer(&g.vals[0])
	if c.Locals != vals {
		return
	}
	w := r.stack[l.wb : l.wb+n : l.wb+n]
	copy(w, g.vals[:n])
	c.Locals = unsafe.Pointer(&w[0])
	jit.RebaseExitLocals(c, vals, c.Locals)
}

// jitRegGenFill fills f, the frame of l, a generator's level, as regGenRun
// fills a resumption's.
func (r *Runtime) jitRegGenFill(f *frame, l *jitRegLevel) {
	g := l.gen
	f.this, f.callee = g.this, g.callee
	f.args = nil
	f.handlers, f.openUpvalues = nil, nil
	g.frameRare(f)
	f.savedSP = 0
	f.regKind = 0
}

// jitRegGenDone ends f, the frame of l, a generator's level, which returned
// v or ended in err, as regGenRun and regGenResume end a resumption:
// suspended, its window written back to the generator's values and its
// place saved, the step's result the value; finished, the generator done
// and the step's cursor too -- the step's result the uninitialized value,
// its done, or the throw. A call of next's result is its object, { value,
// done }, which the context's generator, 1, tells its native caller
// (abi.Context's GenSent). The frame is popped after, with its next's below
// it (jitRegPopLevel).
func (r *Runtime) jitRegGenDone(f *frame, l *jitRegLevel, v Value, err error) (Value, error) {
	g, c := l.gen, l.c
	defer func() {
		f.savedSP = 0
		f.args = nil
		f.detachSuspended()
	}()
	if s, ok := err.(*suspendSignal); ok {
		g.pc = uint32(f.savedSP)
		g.saveWith(f.withScopes)
		n, from := int(c.numRegs), int(c.genFrom)
		copy(g.vals[:n-from], r.stack[l.wb+from:l.wb+n])
		g.delegating, g.state, g.started = s.delegate, genSuspendedYield, true
		// A yield's: the generator has no yield* (jitRegGenCallable),
		// whose suspension hands out a result object.
		v = s.value
		s.value = Undefined
		if l.cursor == nil {
			r.jit.ssaCtxs[l.ctx].GenObject = 1
			v = Obj(r.iterResult(v, false))
		}
		return v, nil
	}
	g.state, g.saved = genCompleted, nil
	switch {
	case err != nil:
		if l.cursor != nil {
			l.cursor.done = true
		}
		return Undefined, err
	case l.cursor == nil:
		r.jit.ssaCtxs[l.ctx].GenObject = 1
		return Obj(r.iterResult(v, true)), nil
	}
	l.cursor.done = true
	return uninitialized, nil
}

// jitRegResumed counts a resumption Go makes of a generator's or an async
// function's register code c, cl's, which has no native code, as an
// invocation: its length spent of its budget (jitRegHot). Code refused for
// host operations alone is asked again once its resumptions have done
// jitHotWork of it, the count kept on the closure's template
// (jitRegColdWork): looked up in the cache at each resumption, an async
// chain's functions -- each await a resumption -- spent a twentieth of
// their time on it. A generator of no loop has no back edge to be sampled
// at, and its resumptions, made by Go from native code whose polls rearm
// the interrupt budget, spend none of it. Every resumption counted at
// jitHotWork, an async function whose loop awaits at every turn -- each
// turn a resumption, too few back edges in all for a sample -- was never
// compiled.
func (r *Runtime) jitRegResumed(cl *closure, c *regCode) {
	cc := cl.closureCode
	if !r.jitEnabled || cc == nil || cc.jitRegRefused {
		return
	}
	work := max(len(c.rc.Code), 1)
	if !cc.jitRegHostOnly {
		e := r.jitState().regCache[r.jitRegKeyOf(cl)]
		if e == nil || !e.hostOnly {
			r.jitRegHot(cl, c, int64(work))
			return
		}
		cc.jitRegHostOnly, cc.jitRegColdWork = true, 0
	}
	cold := int(cc.jitRegColdWork) + min(work, jitHotWork)
	if cold < jitHotWork {
		cc.jitRegColdWork = uint16(cold)
		return
	}
	// Asked again, as the cache holds it now: refused so still, with the
	// work its resumptions did.
	cc.jitRegHostOnly, cc.jitRegColdWork = false, 0
	if e := r.jitState().regCache[r.jitRegKeyOf(cl)]; e != nil && e.hostOnly {
		work = cold
	}
	r.jitRegHot(cl, c, int64(work))
}

// regGenCaught catches, for f's native code, a throw injected into f, a
// suspended frame of register code c, at its suspension (f.savedSP, which
// gen_entry throws at), where a handler of the frame's takes it: the value
// thrown given to the handler's register, as regCatch gives it, and the
// register pc the frame resumes at, the handler's, where its native code
// has an entry (ir.Program's handlers) -- f.savedSP cleared. Where no
// handler takes it, it returns 0, the throw gen_entry's. Left to gen_entry,
// a rejected await in a try statement had the interpreter catch it and run
// the rest of the frame, its later awaits gone on from in place: an async
// function's loop awaiting a promise that may reject, natively in its
// first run alone.
func (r *Runtime) regGenCaught(f *frame, c *regCode) int {
	at := -f.savedSP - 1
	h := c.rc.HandlerAt(at)
	if h == nil {
		return 0
	}
	wb := f.base - int(c.locals)
	r.stack[wb+int(h.Reg)] = r.stack[wb+int(regAt(c.code, at).A())]
	f.savedSP = 0
	return int(h.To)
}

// Awaits that go on in place, natively (ssa.OpAwaitReady, GoAwait).
//
// An async function resumed by its await's job, no job queued, goes on in
// place at an await of a settled value, as the interpreter's await does
// (regSuspend): the job the await would queue would be the queue's next,
// resuming the frame with what the value settled to at once. Native code
// asks the same at the await -- the frame's kind, which Go gives each run
// of an async function's code (abi.Context's AwaitJob), and the queue,
// whose next job's index and length it reads where the runtime keeps them
// (JobHead, JobCount) -- and goes on with the value where it is no object,
// and with what Go finds it settled to where it is (jitAwaitSettled); it
// leaves at the await otherwise, where Go suspends the frame.

// jitAwaitJob is the context's AwaitJob for a run of f, an async
// function's frame: 1 where its await's job resumed it.
func jitAwaitJob(f *frame) uint64 {
	if f.regKind == regFrameAwaitJob {
		return 1
	}
	return 0
}

// jitJobCount is where r keeps its job queue's length, the word of its
// slice's header after the pointer (abi.Context's JobCount).
func jitJobCount(r *Runtime) *int {
	return (*int)(unsafe.Add(unsafe.Pointer(&r.microtasks), unsafe.Sizeof(uintptr(0))))
}

// jitAwaitSettled is GoAwait: what an await of the object its operand is
// goes on with in place, as the interpreter's await finds it (regSettled),
// to the call's keep cell, or false where the frame is to suspend.
func (r *Runtime) jitAwaitSettled(ctx *abi.Context) bool {
	v, ok := r.regSettled(jitGoArg(ctx, 0))
	if ok {
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	}
	return ok
}
