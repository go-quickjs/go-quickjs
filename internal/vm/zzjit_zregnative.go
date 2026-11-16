//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Native code made from register code (docs/register-interpreter.md,
// Stage 3, 3a-3): a function's register code lowered (compile.LowerReg),
// built with its feedback (jitRegFeedback), optimized and compiled by the
// same backend as the stack form's (jitRegFor), and run on the register
// frame's window in place (jitRegRun), its exits going on in the register
// interpreter's frame: a host exit runs the register instruction whole by
// the interpreter's slow paths (regWhole) and native code goes on at the
// next entry; a deoptimization, and an exit after which the code has no
// entry, have runReg run the rest of the invocation. Nothing in the
// register tier enters it yet: only a test's entry does (RegNativeCall).

// jitRegEntry is a function's code compiled from its register code, in
// the JIT's cache (jitState's regCache), or the reason none is.
type jitRegEntry struct {
	p    *ir.Program
	code *jit.SSACode
	// keep is what the code reads by address -- the shapes, holders and
	// constants' cells its feedback answered with -- kept alive with it.
	keep *jitFeedback
	// keeps is how many keep cells the code uses; scratch how many scratch
	// slots its program has (ssa.Func's Scratch), past the window in
	// memory; and frame how many slots past those its runs take: the homes
	// of the objects it does not make and its loops' slots (ssa.Func's
	// HomesEnd, LoopsEnd).
	keeps, scratch, frame int
	// loops are its loops' headers that it has entries at, by register
	// pc, sorted: where a frame in the register interpreter goes on in it
	// by on-stack replacement (loopEntry).
	loops []uint32
	// invalid marks code a guard of which failed: its runs finish in the
	// interpreter, and the next asks for it compile it again from what the
	// interpreter learned meanwhile. runs counts the runs under way, which
	// keep it from being closed.
	invalid bool
	runs    int
	// mark is what its function's caches held when it was compiled
	// (jitFeedbackMark): changed, the code is compiled again at its next
	// sample (jitRegHot).
	mark uint64
	// icWait counts down the samples left until the code is compiled
	// again for its caches' changed feedback (jitRegHot), 0 for none.
	icWait int
	// stale marks code that learned what to be compiled again for
	// (jitRegReopt): its runs go on in it, and the next ask compiles it
	// again.
	stale bool
	// hostOnly marks a refusal of host operations alone (jitRegCompile),
	// which a later ask lowers again.
	hostOnly bool
	// levels counts the native calls under way of the code that Go runs
	// beside (jitRegLevelAt): the cache neither drops it nor compiles it
	// again while any is.
	levels  int
	bytes   int
	refusal string
	// retPC and retUndef are where the interpreter ends a frame the code
	// returned from by on-stack replacement, retReg the register retPC's
	// ret returns, and goesOn whether it can (findReturns).
	retPC, retReg, retUndef int
	goesOn                  bool
	// serial numbers the compile (jitState's regSerial): a property site
	// that deoptimizes for a shape again in code compiled since it first
	// did is megamorphic (jitDeoptSeenOp).
	serial uint32
	// unresumed are the register pcs after calls the code has no entry
	// after, Go not having gone on there (ssa.Func's Unresumed): where Go
	// goes on at one, the code is compiled again with the entry
	// (jitRegResumeSeen).
	unresumed []int
}

// loopEntry is the entry, an instruction of the program, of the loop
// whose header is register pc rpc, if the code has one there: the OSR
// entry map, each loop header's register pc to the build's entry at the
// first instruction of its lowering, its live registers loaded from the
// window (ir.Program's Live).
func (e *jitRegEntry) loopEntry(rpc int) (int, bool) {
	if _, ok := slices.BinarySearch(e.loops, uint32(rpc)); !ok {
		return 0, false
	}
	return e.p.At(uint32(rpc)), true
}

// jitRegFor is cl's register code c compiled, from the cache or anew; nil
// where the JIT has no memory for it now. An entry with no code holds the
// refusal.
func (r *Runtime) jitRegFor(cl *closure, c *regCode) *jitRegEntry {
	return r.jitRegForAs(cl, c, false)
}

// jitRegForAs is jitRegFor, for native callers where callee says
// (jitRegNativeCallee): a function of host operations alone, which a
// compile for Go to enter refuses but where its calls are native ones
// (jitRegCompile), is compiled for them, as the stack form compiles one for
// native callers (LowerSSAInline). A refusal for that is not kept: asked
// again, the function is lowered again.
func (r *Runtime) jitRegForAs(cl *closure, c *regCode, callee bool) *jitRegEntry {
	s := r.jitState()
	if s.unavailable {
		return nil
	}
	key := r.jitKeyOf(cl.fn)
	if s.regPendingJobs[key] != nil {
		// Being compiled in the background (jitRegSubmit): installed at
		// a later check.
		return nil
	}
	if e := s.regCache[key]; e != nil {
		if !e.invalid && !e.hostOnly && !e.stale {
			return e
		}
		if e.levels != 0 {
			// Run by native calls under way, which find their code in the
			// cache (jitRegLevelAt): compiled again once they are done.
			return nil
		}
		s.dropReg(key, e)
	}
	e := r.jitRegCompile(cl, c, callee)
	if e == nil {
		return nil
	}
	r.jitRegCached(key, e)
	return e
}

// jitRegCached keeps e, the code of the function whose key is key, in the
// cache, its link the code's.
func (r *Runtime) jitRegCached(key jitKey, e *jitRegEntry) {
	s := r.jit
	if s.regCache == nil {
		s.regCache = map[jitKey]*jitRegEntry{}
	}
	s.regCache[key] = e
	s.regLinkUpdate(key, e)
}

// jitRegCompile is jitRegFor's compile (step A6 of 3a-3): LowerReg, the
// SSA build with jitRegFeedback, Optimize, the backend, the code placed in
// the JIT's arena. A refusal is an entry with no code; nil is no room, or a
// compile handed to the background (jitRegSubmit), installed at a later
// check (jitRegInstall).
func (r *Runtime) jitRegCompile(cl *closure, c *regCode, callee bool) *jitRegEntry {
	s := r.jit
	if !jitCompiles(cl.fn, r.jitBudget()) {
		// Too large for the JIT's memory, as the stack form's compile
		// finds it (jitCompiles): no room, at every ask.
		return nil
	}
	background := r.jitInBackground(cl.fn)
	if background && s.jobsFull() {
		// As many compiles in the background as there may be: asked again
		// at a later sample, as the stack form's compile waits.
		return nil
	}
	e := &jitRegEntry{}
	p, err := jitcompile.LowerReg(c.rc, cl.fn)
	if err != nil && jitcompile.HostOnly(err) {
		// Host operations alone, no loop, no element's work: compiled for
		// its native callers (LowerRegInline), or, hot, where a call it
		// makes is native (LowerRegHot), as the stack form compiles one
		// (jitNativeCallee, jitHotEntry); refused for now otherwise.
		switch {
		case callee:
			p, err = jitcompile.LowerRegInline(c.rc, cl.fn)
		case r.jitRegCallsNatively(cl, c):
			p, err = jitcompile.LowerRegHot(c.rc, cl.fn)
		default:
			e.hostOnly = true
		}
	}
	if err == nil {
		err = p.Validate()
	}
	if err != nil {
		e.refusal = err.Error()
		return e
	}
	e.p = p
	s.regSerial++
	e.serial = s.regSerial
	if jitParityLog.on.Load() {
		r.jitParityNote(cl)
	}
	r.jitRegSeenFeedback(cl, c)
	e.mark = jitFeedbackMark(cl)
	rf := r.jitRegFeedbackFor(cl, p)
	if background {
		// Asked ahead here, built on a worker (zzjit_zregbg.go).
		snap := jitRegSnapshotOf(rf, p)
		if !snap.s.bad {
			j := &jitRegJob{cl: cl, c: c, key: r.jitKeyOf(cl.fn), realm: r.Realm, callee: callee, e: e, p: p, rf: rf, snap: snap,
				w: jit.GetWorkspace(), enc: jitEncoding, opts: mir.Settings()}
			r.jitRegSubmit(j)
			return nil
		}
		// What the questions took back cannot be put back: built here,
		// from the feedback as it is now, its lists as the asking left
		// them.
	}
	w := jit.GetWorkspace()
	defer jit.PutWorkspace(w)
	s.compileBegin()
	defer s.compileEnd()
	f, err := ssa.BuildIn(&w.SSA, p, rf)
	if err != nil {
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, err)
		return e
	}
	looped := jitRegShape(f, p)
	ssa.Optimize(f)
	mc, err := mir.CompileWith(&w.MIR, f, jitEncoding, mir.Settings())
	if err != nil {
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, err)
		return e
	}
	if !r.jitRegPlace(e, c, rf, f, mc, looped) {
		return nil
	}
	return e
}

// jitRegPlace places mc, e's code, the backend's of f, built from c's
// register code lowered with its feedback rf, in the JIT's arena, and
// fills e from what the build found; it reports false where the JIT has
// no room for it now.
func (r *Runtime) jitRegPlace(e *jitRegEntry, c *regCode, rf *jitRegFeedback, f *ssa.Func, mc *mir.Code, looped map[uint32]bool) bool {
	s := r.jit
	p := e.p
	need := jit.CodeFootprint(mc)
	s.budget = r.jitBudget()
	if s.regBytes()+need > s.budget/2 || !s.makeRoom(need) {
		// Half the budget at most, the stack form's code the rest, while
		// only tests run register code natively.
		return false
	}
	code, err := jit.NewSSACode(s.arena, mc)
	if err != nil {
		if errors.Is(err, jit.ErrUnavailable) {
			s.unavailable = true
		}
		return false
	}
	s.inlined += uint64(len(f.Inlined))
	e.code, e.bytes = code, need
	e.scratch = f.Scratch
	e.keeps = f.Keeps
	s.ssaKeeps = max(s.ssaKeeps, f.Keeps)
	if f.HomesEnd != 0 {
		e.frame = f.HomesEnd - p.Locals
	}
	if f.Loops {
		e.frame = max(e.frame, f.LoopsEnd-p.Locals)
	}
	for rpc := range looped {
		if code.HasEntry(p.At(rpc)) {
			e.loops = append(e.loops, rpc)
		}
	}
	slices.Sort(e.loops)
	e.findReturns(c.rc)
	e.unresumed = slices.Clone(f.Unresumed)
	// The pools' roots and prototypes escape analysis took, which the
	// objects it does not make are made with at an exit.
	rf.fb.keepVirtuals(f.VirtualAsked)
	// What the code reads by address, its feedback's keep; the closure
	// and the function the feedback asked are not.
	e.keep = rf.fb.keep()
	e.keep.cl, e.keep.fn, e.keep.prog = nil, nil, nil
	if JITCodeHook != nil {
		// -jitcode's record of the code, as the stack form's compiles
		// make it (jitInstall).
		JITCodeHook(jitFunctionOf(c.fn), code.Text(), mc)
	}
	return true
}

// regBytes is what the register code in the cache takes.
func (s *jitState) regBytes() int {
	n := 0
	for key, e := range s.regCache {
		if key.gone() && e.runs == 0 {
			s.dropReg(key, e)
			continue
		}
		n += e.bytes
	}
	for key := range s.regLearned {
		if key.gone() {
			delete(s.regLearned, key)
		}
	}
	for key := range s.regLinks {
		if key.gone() {
			delete(s.regLinks, key)
		}
	}
	return n
}

// dropReg closes e's code, unless a run is in it, and takes it out of the
// cache.
func (s *jitState) dropReg(key jitKey, e *jitRegEntry) {
	if s.regCache[key] == e {
		delete(s.regCache, key)
		s.regLinkUpdate(key, nil)
	}
	if e.code != nil && e.runs == 0 && e.levels == 0 {
		_ = s.closeSSA(e.code, e.keep)
		e.code = nil
	}
}

// dropRegCode drops every entry of the register code's cache, as the
// runtime releases its JIT.
func (s *jitState) dropRegCode() {
	for key, e := range s.regCache {
		s.dropReg(key, e)
	}
}

// jitRegSlot is the value slot i of p names in f's register frame, its
// window at wb: a register, a captured binding in its cell, or a slot past
// the window, where mir puts them -- a scratch slot first, then what is
// past the program's Locals, an object's home, a loop's slot.
func (r *Runtime) jitRegSlot(f *frame, p *ir.Program, wb, i int) *Value {
	scratch := p.Locals - (p.UpvalueBase + p.Upvalues)
	switch {
	case i < p.Regs:
		return &r.stack[wb+i]
	case i >= p.UpvalueBase && i < p.UpvalueBase+p.Upvalues:
		return f.cl.upvalues[i-p.UpvalueBase].slot
	case i >= p.Locals:
		return &r.stack[wb+p.Regs+scratch+i-p.Locals]
	}
	return &r.stack[wb+p.Regs+i-(p.Locals-scratch)]
}

// jitRegApplyRecords writes the slots ctx's records name into f's register
// frame, every value read before any is written, as jitApplyRecords does
// for the stack form's: an exit's own records native code applies itself
// as it leaves, so these are a native call's caller's state.
func (r *Runtime) jitRegApplyRecords(f *frame, p *ir.Program, wb int, ctx *abi.Context) {
	n := int(ctx.Records)
	if n == 0 {
		return
	}
	r.jit.ssaRecords += uint64(n)
	const flags = abi.RecordScalar | abi.RecordMaybe | abi.RecordDirect
	var buf [8]Value
	src := buf[:0]
	if n > len(buf) {
		src = make([]Value, 0, n)
	}
	src = src[:n]
	for i := range n {
		switch rec := &ctx.Record[i]; {
		case rec.Slot&abi.RecordDirect != 0:
			slot := abi.Slot{Num: rec.Word, Ref: ctx.RecordRef[i]}
			src[i] = *(*Value)(unsafe.Pointer(&slot))
		case rec.Slot&abi.RecordScalar != 0:
			src[i] = Value{num: math.Float64frombits(rec.Word)}
		case rec.Slot&abi.RecordMaybe != 0:
			src[i] = Value{num: math.Float64frombits(rec.Word)}
			if s := jitSource(&rec.Arg); s != nil && s.ref != nil {
				src[i] = *s
			}
		default:
			src[i] = *r.jitRegSlot(f, p, wb, int(rec.Arg))
		}
	}
	for i, rec := range ctx.Record[:n] {
		*r.jitRegSlot(f, p, wb, int(rec.Slot&^flags)) = src[i]
	}
	ctx.Records = 0
}

// jitRegMaterialize makes, in f's register frame, the objects e's code did
// not make (ssa's escape.go), as jitMaterializeObjects does in the stack
// form's: a register that holds the word of one gets the object, made of
// its home past the window, one for each home, the homes cleared after.
func (r *Runtime) jitRegMaterialize(f *frame, e *jitRegEntry, wb int) {
	r.jitRegMaterializeSpans(f, e, wb, nil)
}

// jitRegMaterializeSpans is jitRegMaterialize, the windows of the callees
// e's code inlined that an exit inside them wrote too -- spans, each its
// first slot and its end in the VM's stack -- whose registers name the
// same objects as the frame's: one made for each home, which every slot
// naming it, the frame's and the callees', is given.
func (r *Runtime) jitRegMaterializeSpans(f *frame, e *jitRegEntry, wb int, spans [][2]int) {
	if e.frame == 0 {
		return
	}
	p := e.p
	end := min(len(r.stack), wb+p.Regs+e.scratch+e.frame)
	named := namesVirtual(r.stack[wb:end])
	for _, sp := range spans {
		named = named || sp[0] < sp[1] && sp[1] <= len(r.stack) && namesVirtual(r.stack[sp[0]:sp[1]])
	}
	if !named {
		return
	}
	type made struct {
		home, fields int
		o            *Object
	}
	var objs []made
	at := func(slot int) *Value { return r.jitRegSlot(f, p, wb, slot) }
	var get func(w uint64) *Object
	fix := func(v *Value) {
		if isVirtualWord(v) {
			*v = Obj(get(math.Float64bits(v.num)))
		}
	}
	get = func(w uint64) *Object {
		home, fields, _ := abi.IsVirtualWord(w)
		for _, m := range objs {
			if m.home == home {
				return m.o
			}
		}
		sw := math.Float64bits(at(home).num)
		o := newShapedLiteral(*(**shape)(unsafe.Pointer(&sw)), ClassObject, fields)
		objs = append(objs, made{home, fields, o})
		for k := range fields {
			v := *at(home + abi.VirtualFields + k)
			fix(&v)
			o.appendVal(v)
		}
		return o
	}
	for i := wb; i < end; i++ {
		fix(&r.stack[i])
	}
	for _, sp := range spans {
		for i := sp[0]; i < min(sp[1], len(r.stack)); i++ {
			fix(&r.stack[i])
		}
	}
	for _, m := range objs {
		for k := range abi.VirtualFields + m.fields {
			*at(m.home + k) = Undefined
		}
	}
	r.jit.materialized += uint64(len(objs))
}

// jitRegRun runs f, running c with its window at wb, in e's code from
// register pc rpc, an entry's, until it returns, and goes on in the
// register interpreter where the code leaves for good (step A1 of 3a-3).
func (r *Runtime) jitRegRun(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) (Value, error) {
	pc, v, done, err := r.jitRegGoOn(f, c, e, wb, rpc)
	switch {
	case err != nil:
		return Undefined, err
	case done:
		return v, nil
	}
	return r.runReg(f, c, pc)
}

// jitRegGoOn runs f, running c with its window at wb, in e's code from
// register pc rpc, an entry's, until it returns -- done, with what it
// returns -- or throws what no handler of the frame's takes, err, or
// leaves the frame to the register interpreter, which goes on with it at
// register pc pc: after a deoptimization, at a register pc the code has no
// entry at, or where the code is not to run. A frame the interpreter runs
// goes on so in its own loop (jitRegLoop); a call's, in runReg.
func (r *Runtime) jitRegGoOn(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) (pc int, v Value, done bool, err error) {
	return r.jitRegGoOnAt(f, c, e, wb, rpc, -1, 0)
}

// jitRegGoOnAt is jitRegGoOn, but where resume is not 0 begun by going on
// in e's code where a native call it made returns to, in context at, the
// frame's level of a native call Go finished the callee of (jitRegUnwind),
// the call's result in the next context's RetValue.
func (r *Runtime) jitRegGoOnAt(f *frame, c *regCode, e *jitRegEntry, wb, rpc, at int, resume uintptr) (pc int, v Value, done bool, err error) {
	s := r.jit
	for {
		ipc := 0
		if resume == 0 && e.stale && !e.invalid {
			// Stale for what it learned (jitRegReopt), the run having left
			// it: compiled again now and gone on in that code, as the stack
			// form's run's code is compiled again at its exit
			// (jitRecompileInRun) -- an inlining given up, a call learned
			// -- rather than gone on in code that leaves at the same place
			// again until its budget is spent.
			if ne := r.jitRegFor(f.cl, c); ne != nil && ne.code != nil && !ne.invalid {
				e = ne
			}
		}
		if resume == 0 {
			ipc = e.p.At(uint32(rpc))
			if e.invalid || e.code == nil || ipc < 0 || !e.code.HasEntry(ipc) || wb+e.p.Regs+e.scratch+e.frame > len(r.stack) {
				// No entry here, or code a guard of which failed: the
				// interpreter runs the rest of the invocation.
				r.jitRegResumeSeen(f, e, rpc)
				s.interpreted++
				return rpc, Undefined, false, nil
			}
		}
		ctx, ret, ok := r.jitRegStretch(f, c, e, wb, ipc, at, resume)
		at, resume = -1, 0
		if !ok {
			s.interpreted++
			return rpc, Undefined, false, nil
		}
		next, pc, v, done, err, native := r.jitRegLeft(f, c, e, wb, ctx, ret)
		if !native {
			return pc, v, done, err
		}
		rpc = next
	}
}

// jitRegLeft is what Go makes of the way e's code, run on f's window at wb,
// left in ctx, ret what a return returned: native code goes on at register
// pc next, native; or the frame is done, with v; or it threw err, which no
// handler of the frame's takes; or the interpreter goes on with it at
// register pc pc.
func (r *Runtime) jitRegLeft(f *frame, c *regCode, e *jitRegEntry, wb int, ctx *abi.Context, ret Value) (next, pc int, v Value, done bool, err error, native bool) {
	s := r.jit
	exitPC := int(ctx.ExitPC)
	if ctx.Loop != 0 && ctx.ExitKind != abi.ExitReturn && exitPC < len(c.rc.Code) && regAt(c.code, exitPC).Op() == bytecode.RCallMW {
		// Inside a loop of a built-in's call: Go goes on with it.
		return r.jitRegLoopLeft(f, c, e, wb, ctx)
	}
	switch ctx.ExitKind {
	case abi.ExitReturn:
		return 0, 0, ret, true, nil, false
	case abi.ExitDeopt:
		// A guard failed before its instruction committed anything: the
		// frame holds the state at its start, which the interpreter runs
		// the rest of the invocation from, and the code is compiled
		// again at the next ask, from what the interpreter learns.
		s.guards++
		r.jitRegMaterialize(f, e, wb)
		learned := false
		if int64(ctx.ExitSite) < 0 {
			// An entry's speculation, at a loop's header or the start: the
			// entry's, as the stack form's code learns it (EntryGeneric).
			r.jitRegEntrySeen(f, exitPC)
		} else if learned = r.jitRegBigIntSeen(f, e, wb, int(int64(ctx.ExitSite))); !learned {
			// An operator of BigInts it learned is not generic: the code
			// compiled again makes it of them (jitBigIntNote).
			r.jitRegDeoptSeen(f, c, e, wb, exitPC)
		}
		if learned {
			r.jitRegInvalidateLearned(f.cl, e)
		} else {
			r.jitRegInvalidate(f.cl, e)
		}
		s.interpreted++
		return 0, exitPC, Undefined, false, nil, false
	case abi.ExitHost:
		// Go runs the register instruction whole, then native code goes
		// on at the next entry.
		s.hosts++
		r.jitRegMaterialize(f, e, wb)
		f.pc = regSrc(c, exitPC) + 1
		var ran bool
		if p := s.regPending; p.set && p.pc == exitPC && p.ctx == int(ctx.Level) {
			// A call Go made from native code, which left after it
			// (jitRegGoCall), or a native call whose callee Go finished
			// (jitRegUnwind): what it answered goes to the frame, as the
			// call's register instruction would have it.
			s.regPending = jitGoPending{}
			if p.whole {
				next, ran, err = r.regFinishWhole(wb, p)
			} else {
				next, ran, err = r.regFinishCall(f, c, wb, exitPC, p.v, p.err)
			}
		} else {
			s.noteRegHost(c, exitPC)
			seen := r.jitRegHostSeen(f, c, wb, exitPC)
			r.jitRegPrivateSeen(f, c, e, wb, exitPC)
			if r.jitRegBigIntExit(f, e, wb, int(int64(ctx.ExitSite))) {
				// A generic operator that met BigInts, which the code
				// compiled again calls Go for (ssa.GoArith).
				r.jitRegReopt(f.cl, e)
			}
			var callee Value
			if w := regAt(c.code, exitPC); w.Op() == bytecode.RNewW && wb+int(w.A()) < len(r.stack) {
				callee = r.stack[wb+int(w.A())]
			}
			next, ran, err = r.regWhole(f, c, wb, exitPC)
			if seen.write && err == nil && ran {
				r.jitRegWriteSeen(f, c, e, exitPC, seen)
			}
			if err == nil && ran {
				// A call native code did not make, which it may once
				// compiled again.
				r.jitRegCallSeen(f, c, e, exitPC, callee)
			}
			if l := s.regLearned[r.jitKeyOf(f.cl.fn)]; l != nil && l.polyReopt {
				// The site met what the code was not compiled for and
				// its cache cannot hold: compiled again for it, as the
				// stack form's code is (jitRecompileInRun), its code
				// invalid from here.
				l.polyReopt = false
				r.jitRegInvalidate(f.cl, e)
			}
		}
		if !ran {
			s.interpreted++
			return 0, exitPC, Undefined, false, nil, false
		}
		if err != nil {
			return 0, 0, Undefined, false, err, false
		}
		if r.stopped != nil {
			return 0, 0, Undefined, false, r.stopped, false
		}
		return next, 0, Undefined, false, nil, true
	case abi.ExitPoll:
		// The interpreter's back-edge check, at a loop's header.
		s.budgets++
		r.backEdges = s.backEdgeBudget()
		if err := r.checkInterruptNow(); err != nil {
			return 0, 0, Undefined, false, err, false
		}
		if r.jitStressDeopt(s) || e.stale {
			// Code stale for what it learned (jitRegReopt) is due to be
			// compiled again once a budget has been spent since, as the
			// stack form's (jitRecompileInRun, jitLearnedDue): here, at the
			// budget's end, the loop goes on in the interpreter, whose
			// next sample at its header compiles it anew (jitRegLoop).
			s.interpreted++
			return 0, exitPC, Undefined, false, nil, false
		}
		return exitPC, 0, Undefined, false, nil, true
	}
	// No other way out of register code's native code yet: what it left,
	// the interpreter goes on with from its start.
	r.jitRegMaterialize(f, e, wb)
	s.interpreted++
	return 0, exitPC, Undefined, false, nil, false
}

// jitRegStretch runs e's code once, from its entry at instruction ipc, on
// f's window at wb, in the next context -- or, resume not 0, going on where
// a native call it made returns to (jitRegGoOnAt), in context at -- and
// returns that context with the exit's kind, register pc and the frame
// written -- an exit that left its frame to Go (abi.ExitTable) applied here
// -- and, for a return, the value returned, read before what the run kept
// is let go. A native call whose callee left native code Go finishes
// (jitRegUnwind), and the code goes on after it where it can, as V8's lazy
// deoptimization leaves the caller's frame alone; where it cannot -- code
// a guard of which failed meanwhile, a throw -- the call's state is
// written to the frame, its exit a host exit at the call, which Go
// finishes with the callee's answer (regPending). ok is false where it did
// not run: no context, or the code closed.
func (r *Runtime) jitRegStretch(f *frame, c *regCode, e *jitRegEntry, wb, ipc, at int, resume uintptr) (ctx *abi.Context, ret Value, ok bool) {
	s := r.jit
	idx := s.ctxTop
	if at >= 0 {
		idx = at
	}
	if idx >= len(s.ssaCtxs)-1 && !r.jitGrowContexts() {
		return nil, Undefined, false
	}
	top := s.ctxTop
	s.ctxTop = idx + 1
	e.runs++
	ctx = s.ssaCtxs[idx]
	s.ssaCtx = ctx
	ctx.ReturnTo, ctx.Live, ctx.TailReturn = 0, 0, 0
	r.jitShareContexts(idx, f)
	p := e.p
	ctx.Locals = unsafe.Pointer(&r.stack[wb])
	ctx.Stack = unsafe.Add(ctx.Locals, (p.Regs+e.scratch)*int(jitEncoding.ValueSize))
	ctx.Upvalues = unsafe.Pointer(unsafe.SliceData(f.cl.upvalues))
	if resume == 0 {
		ctx.Cells, ctx.Loop = nil, 0
	}
	ctx.Open = uint64(len(f.openUpvalues))
	// No frame for Go's calls from the code to read: the stack form's
	// frame is what they read (jitGoRun), and they leave what needs one
	// to an exit.
	if idx >= len(s.goRuns) {
		s.goRuns = append(s.goRuns, make([]jitGoRun, idx+1-len(s.goRuns))...)
	}
	s.goRuns[idx] = jitGoRun{}
	// The register frame its calls of Go make theirs on (jitRegGoCall).
	if idx >= len(s.regRuns) {
		s.regRuns = append(s.regRuns, make([]jitRegRun, idx+1-len(s.regRuns))...)
	}
	s.regRuns[idx] = jitRegRun{f: f, c: c, e: e, wb: wb}
	s.regPending = jitGoPending{}
	s.entries++
	s.ssaEntries++
	saved, raised := r.stackTop, -1
	if need := wb + p.Regs + e.scratch + e.frame; need > saved && need <= len(r.stack) {
		// The scratch slots, homes and loops' slots past the window,
		// which the stack's top covers while the code runs.
		r.stackTop, r.stackHigh, raised = need, max(r.stackHigh, need), need
	}
	var err error
	if resume != 0 {
		err = e.code.Resume(resume, s.ssaCtxs[idx+1])
	} else {
		err = e.code.Run(ipc, ctx)
	}
	for err == nil && s.ssaCtxs[idx+1].Live == abi.LiveCall {
		// A native call's callee left native code: Go finishes it, and
		// every native call between, then the code goes on where the
		// call returns to, its state as it left it, the call's result in
		// the callee's context (RetValue), written with Go's barrier -- the
		// one pointer native code stores before it tests the collector's
		// flag again, into a keep cell nothing native reads the old value
		// of (mir's resumeCall) -- the collector marking or not. Not into
		// code that is invalid now, or closed: what Go ran may have run
		// the function again and found its code wanting.
		callee := s.ssaCtxs[idx+1]
		back, base := callee.ReturnTo, callee.Base
		rpc, _ := jit.CallPC(ctx)
		f.pc = regSrc(c, int(rpc)) + 1
		v, uerr := r.jitRegUnwind(idx)
		if uerr == nil && r.stopped == nil && back != 0 && !e.invalid && e.code != nil && e.code.Contains(back) {
			callee.Base, callee.Live, callee.ReturnTo = base, 0, 0
			*(*Value)(unsafe.Pointer(&callee.RetValue)) = v
			s.resumed++
			s.regPending = jitGoPending{}
			err = e.code.Resume(back, callee)
			continue
		}
		// The call's state written to the frame below, the call's exit a
		// host exit at it, which Go finishes with what the callee answered
		// (jitRegLeft).
		jit.ExpandCall(ctx)
		s.regPending = jitGoPending{set: true, pc: int(rpc), ctx: idx, v: v, err: uerr}
		break
	}
	if r.stackTop == raised {
		r.stackTop = saved
	}
	if err == nil && ctx.Cells != nil {
		// Its cells, as the frame has them, for whatever runs it now.
		r.jitRegReopenCells(f, e, wb, ctx, true)
	}
	if len(s.builders) != 0 {
		r.jitFreezeBuilders()
	}
	if s.goRuns[idx].pinned {
		r.jitGoUnpin(idx)
	}
	if err == nil && ctx.ExitKind == abi.ExitTable {
		s.ssaRecords += uint64(s.exitScratch.Apply(ctx))
	}
	if err == nil {
		r.jitRegApplyRecords(f, p, wb, ctx)
		if ctx.ExitKind == abi.ExitReturn {
			ret = Value{num: math.Float64frombits(ctx.Ret)}
			if src := jitSource(&ctx.RetFrom); src != nil && src.ref != nil {
				ret = *src
			}
		}
	}
	if err == nil && ctx.ExitKind == abi.ExitHost {
		// An exit inside a callee the code inlined at the call it left at:
		// Go finishes the callee, and the call goes on with what it
		// answered, as a native callee's (regPending).
		rpc := int(ctx.ExitPC)
		f.pc = regSrc(c, rpc) + 1
		if v, uerr, ok := r.jitRegInlined(idx, f.cl, e, rpc, wb); ok {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: uerr}
		}
	}
	// What the run kept, as jitRunDone clears a stack form's run's, and
	// what its native calls' callees ran with in the contexts past it --
	// closure, keeps, records, result -- each a frame gone once it
	// returned, up to the first context no call ran in.
	clear(ctx.Keep[:e.keeps])
	ctx.This, ctx.Upvalues, ctx.Instance = abi.Slot{}, nil, abi.Slot{}
	if n := max(ctx.RecordHigh, s.callRefs); n != 0 {
		clear(ctx.RecordRef[:n])
		ctx.RecordHigh = 0
	}
	for i := idx + 1; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
		if c.Closure == nil {
			break
		}
		if n := max(c.RecordHigh, s.callRefs); n != 0 {
			clear(c.RecordRef[:n])
			c.RecordHigh = 0
		}
		clear(c.Keep[:s.ssaKeeps])
		c.Closure, c.Upvalues, c.Cells = nil, nil, nil
		c.This, c.RetValue, c.Instance = abi.Slot{}, abi.Slot{}, abi.Slot{}
	}
	s.goRuns[idx] = jitGoRun{}
	s.regRuns[idx] = jitRegRun{}
	s.ctxTop = top
	e.runs--
	if e.code == nil || err != nil {
		return nil, Undefined, false
	}
	return ctx, ret, true
}

// jitRegRun is a register frame native code runs on, in a context
// (jitState's regRuns): f, running c, its window at wb, in e's code.
type jitRegRun struct {
	f  *frame
	c  *regCode
	e  *jitRegEntry
	wb int
}

// jitRegGoCall is a call of Go (ssa.GoCall) native code made from register
// code makes at a typeof, which runs nothing, an instanceof, or a call,
// call_w's, call_method_w's or new_w's, on the
// register frame of the run in context idx, as the interpreter's slow path
// makes it (regCallSlow, regConstruct), not leaving native code: the
// function, the receiver and the arguments the code passed (abi.Context's
// GoArgs) put in a window of their own past the VM's stack's top -- not in
// the frame's, whose registers the code keeps values in as it found them
// -- the frame's pc past the call meanwhile, as regCallW sets it, which a
// stack trace shows. Its result goes to the call's keep cell. A call that
// threw, or that the runtime stopped in, or after which the code was found
// invalid, Go finishes at the exit native code then takes (regPending,
// regFinishCall), the call not made again. A call in tail position, which
// the interpreter makes in constant space, is no call of Go
// (jitRegFeedback's NativeCalls).
func (r *Runtime) jitRegGoCall(ctx *abi.Context, idx int) bool {
	return r.jitRegGoCallOn(ctx, idx, r.jit.regRuns[idx])
}

// jitRegGoCallOn is jitRegGoCall on run's frame, the run's own or a level's
// of its native calls (jitRegGoCallNested).
func (r *Runtime) jitRegGoCallOn(ctx *abi.Context, idx int, run jitRegRun) bool {
	s := r.jit
	f, c := run.f, run.c
	rpc := int(ctx.ExitPC)
	if rpc >= len(c.rc.Code) {
		return false
	}
	w := regAt(c.code, rpc)
	op := w.Op()
	argc := int(w.X())
	kind := ctx.GoOp >> abi.GoOpKindShift & 0xff
	if kind == abi.GoCallWhole || (kind == abi.GoCallRest || kind == abi.GoCallStep) && (op == bytecode.RIterRest || op == bytecode.RIterStep) {
		// Or a destructuring's step or rest, which native code had no
		// step of its own for: run whole too.
		return r.jitRegWhole(ctx, idx, run, rpc)
	}
	if op == bytecode.RTypeof && kind == abi.GoCallTypeOf {
		// typeof's name, as regTypeof makes it, which runs nothing.
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Str(r.typeofString(jitGoArg(ctx, 0)))
		return true
	}
	switch {
	case op == bytecode.RCallW && kind == 0, op == bytecode.RCallMW && kind == abi.GoCallMethod, op == bytecode.RNewW && kind == abi.GoCallNew:
	case op == bytecode.RCallWT && kind == 0, op == bytecode.RCallMWT && kind == abi.GoCallMethod:
		// A call in tail position native code makes while the stack is
		// shallow, as rhCallWT makes the ordinary call there.
		if r.frameDepth >= tailCallDepth {
			return false
		}
	case op == bytecode.RLit && kind == abi.GoCallMethod && w.X()&bytecode.RLitCall != 0:
		// regexp_lit's call, of the method its expansion read, on its
		// literal's RegExp, of its one argument (compile's regexpLit).
		argc = 1
	case op == bytecode.RGetProp && kind == abi.GoCallGet, op == bytecode.RSetProp && kind == abi.GoCallSet,
		(op == bytecode.RGetMethod || op == bytecode.RGetMethodW) && kind == abi.GoCallGet:
		// get_method's and get_method_w's too, a megamorphic site's miss
		// (ssa's megamorphic read): its value the expansion's, which puts it
		// and the receiver in their registers after the call.
		// A property's read or write whose site found an accessor the code
		// does not inline nor call natively (ssa.AccessorSite's call of
		// Go): made as the interpreter makes it, through the site's cache,
		// the getter or the setter called on the frame.
		f.pc = regSrc(c, rpc) + 1
		last, shared := s.ssaCtx, s.ssaSharedGen
		s.goCalls++
		r.noteRegGoCall(w, false, Value{})
		v, err := r.jitRegPropertyMade(f, w, Atom(ctx.GoOp>>abi.GoOpIndexShift), jitGoArg(ctx, 0), jitGoArg(ctx, 1))
		s.ssaCtx = last
		if s.ssaSharedGen != shared {
			r.jitShareContexts(idx, f)
		}
		if err != nil || r.stopped != nil || run.e.invalid {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		return true
	case op == bytecode.RInstanceOf && kind == abi.GoCallInstanceOf:
		// As regSlow makes it, by the site's cache: it may run code.
		aux := int(c.rc.Aux[rpc])
		if aux >= len(f.cl.ic) {
			return false
		}
		f.pc = regSrc(c, rpc) + 1
		last, shared := s.ssaCtx, s.ssaSharedGen
		s.goCalls++
		yes, err := r.instanceOfAt(&f.cl.ic[aux], jitGoArg(ctx, 0), jitGoArg(ctx, 1))
		s.ssaCtx = last
		if s.ssaSharedGen != shared {
			r.jitShareContexts(idx, f)
		}
		if err != nil || r.stopped != nil || run.e.invalid {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: Bool(yes), err: err}
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Bool(yes)
		return true
	default:
		return false
	}
	if int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != argc {
		return false
	}
	top := r.stackTop
	end := top + 2 + argc
	if end > len(r.stack) {
		return false
	}
	at := 0
	this := Undefined
	if op.MethodCall() {
		this, at = jitGoArg(ctx, 0), 1
	}
	callee := jitGoArg(ctx, at)
	r.noteRegGoCall(w, false, callee)
	r.stack[top] = callee
	r.stack[top+1] = this
	for i := range argc {
		r.stack[top+2+i] = jitGoArg(ctx, at+1+i)
	}
	r.stackTop, r.stackHigh = end, max(r.stackHigh, end)
	s.goCalls++
	f.pc = regSrc(c, rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	goCallDepth := s.goCallDepth
	s.goCallDepth = r.frameDepth + 1
	var v Value
	var err error
	if op == bytecode.RNewW {
		v, err = r.regConstruct(r.stack[top], top, argc)
	} else if op == bytecode.RLit {
		// As regLit calls it: the RegExp given back where the built-in
		// called keeps nothing of it (RMarkLitRecv).
		m := uint8(bytecode.RMarkLitRecv)
		if w.X()&bytecode.RLitDead != 0 {
			m |= bytecode.RMarkDead
		}
		v, err = r.regMarkedCall(f, c, top, 1, rpc, m, uint32(w.C()))
	} else {
		v, err = r.regCallSlow(f, c, w, top, rpc)
		if err == errTailCall {
			// The callee's frame ended in a tail call.
			v, err = r.regTailRest()
		}
	}
	s.goCallDepth = goCallDepth
	jitClearArgs(r.stack, top, end)
	r.stackTop = top
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err == nil && r.stopped == nil {
		// What the call called, which the code may call natively once
		// compiled again (jitRegCallSeen), or make a loop of its own
		// (jitRegForEachSeen).
		r.jitRegCallSeen(f, c, run.e, rpc, callee)
		if op == bytecode.RCallMW {
			r.jitRegForEachSeen(f, run.e, rpc, callee, this, jitGoArg(ctx, at+1), argc)
		}
	}
	if err != nil || r.stopped != nil || run.e.invalid {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitRegWhole runs, for native code, register code's instruction at
// register pc rpc whole on run's frame (abi.GoCallWhole, ir's Whole), as
// an exit there would have Go run it (regWhole), not leaving native code:
// the registers it reads given the values the code passed (abi.Context's
// GoArgs), its result, the register it writes, to the call's keep cell.
// The code keeps values in the frame's registers as it found them, so
// what the call wrote there is put back -- but a captured register of the
// run's own frame, which is where its cell holds it. What does not go on
// at the next instruction -- a throw, a handler that took one, the
// runtime stopped, the code found invalid -- Go finishes at the exit the
// code then takes (regPending, regFinishWhole): the register the
// instruction wrote, or the handler's, written over the exit's records,
// the instruction not run again. On a native callee's frame (a level),
// what reads the frame's this is left to the exit.
func (r *Runtime) jitRegWhole(ctx *abi.Context, idx int, run jitRegRun, rpc int) bool {
	s := r.jit
	f, c, e, wb := run.f, run.c, run.e, run.wb
	if e == nil || e.p == nil {
		return false
	}
	ipc := e.p.At(uint32(rpc))
	if ipc < 0 || ipc+1 >= len(e.p.Code) {
		return false
	}
	if e.p.Origin[ipc+1] == e.p.Origin[ipc] || ipc > 0 && e.p.Origin[ipc-1] == e.p.Origin[ipc] {
		return false
	}
	ops, argc, dest, ok := e.p.Code[ipc].WholeOperands(e.p.Regs)
	whole := ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallWhole
	if !ok || whole && int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != argc || wb+e.p.Regs > len(r.stack) {
		return false
	}
	own := ctx.Live == 0
	switch op := regAt(c.code, rpc).Op(); {
	case !own && (op == bytecode.RLoadThis || op == bytecode.RThis):
		return false
	case op == bytecode.RCloseRegs && (!own || ctx.Cells != nil):
		// A scope's end where the code's captured registers are in cells
		// of their own, which it takes fresh ones for (jitRegCellsClosure):
		// left to the exit, as a native callee's.
		return false
	}
	after := int(e.p.Origin[ipc+1])
	h := c.rc.HandlerAt(rpc)
	if h != nil && int(h.To) == after {
		// A handler at the next instruction, where what threw and what did
		// not would go on alike.
		return false
	}
	var regs [4]int
	var saved [4]Value
	n := 0
	for i := 0; i <= argc; i++ {
		reg := dest
		if i < argc {
			reg = ops[i]
		}
		if reg < 0 || own && e.p.IsCaptured(reg) || slices.Contains(regs[:n], reg) {
			continue
		}
		regs[n], saved[n] = reg, r.stack[wb+reg]
		n++
	}
	for i := range argc {
		if reg := ops[i]; !own || !e.p.IsCaptured(reg) {
			r.stack[wb+reg] = jitGoArg(ctx, i)
		}
	}
	s.goCalls++
	r.noteRegGoCall(regAt(c.code, rpc), true, Value{})
	last, shared := s.ssaCtx, s.ssaSharedGen
	goCallDepth := s.goCallDepth
	s.goCallDepth = r.frameDepth + 1
	next, _, err := r.regWhole(f, c, wb, rpc)
	s.goCallDepth = goCallDepth
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	var v Value
	if dest >= 0 {
		v = r.stack[wb+dest]
	}
	if err != nil || next != after || r.stopped != nil || e.invalid {
		// Left at the call's exit, whose records write the frame as it was
		// before the instruction: then the register it wrote, or the
		// handler's that took what it threw.
		p := jitGoPending{set: true, pc: rpc, ctx: idx, whole: true, next: next, err: err, reg: -1}
		switch {
		case err != nil:
		case h != nil && next == int(h.To):
			p.reg, p.v = int(h.Reg), r.stack[wb+int(h.Reg)]
		default:
			p.reg, p.v = dest, v
		}
		s.regPending = p
		s.wholeLeft++
		return false
	}
	for i := range n {
		r.stack[wb+regs[i]] = saved[i]
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// regFinishWhole finishes, on the registers of a window at wb, an
// instruction Go ran whole for native code (jitRegWhole) that did not go
// on natively, as p says: the register it wrote written over what the
// exit's records wrote, or its exception, which no handler of the frame's
// took, thrown. It reports as regWhole does.
func (r *Runtime) regFinishWhole(wb int, p jitGoPending) (next int, ok bool, _ error) {
	if p.err != nil {
		return 0, true, p.err
	}
	if p.reg >= 0 {
		r.stack[wb+p.reg] = p.v
	}
	return p.next, true, nil
}

// jitRegPropertyMade makes, for native code, the property read or write w
// of f's register code, of key, on obj -- of val, a write -- as the
// interpreter makes it, through the site's cache: a getter or a setter is
// called, with obj as its this. A write's result is undefined.
func (r *Runtime) jitRegPropertyMade(f *frame, w bytecode.RInstr, key Atom, obj, val Value) (Value, error) {
	var c *propCache
	if site := int(w.C()); site < len(f.cl.ic) {
		c = &f.cl.ic[site]
	}
	if w.Op() == bytecode.RSetProp {
		if obj.IsObject() && c != nil {
			return Undefined, r.setPropCached(c, obj.Object(), key, val, f.cl.fn.Strict)
		}
		return Undefined, r.setValueProp(obj, key, val, f.cl.fn.Strict)
	}
	if obj.IsObject() && c != nil {
		o := obj.Object()
		if v, ok := plainOwn(o, key); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(c, o, key); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, key)
}

// regFinishCall finishes, on f's registers, a window at wb, the call at
// register pc rpc of c that Go made for native code (jitRegGoCall) with v
// or err as its answer, as regCallW finishes one: its value to its
// register, its exception to the frame's handlers (regCatch). It reports
// as regWhole does.
func (r *Runtime) regFinishCall(f *frame, c *regCode, wb, rpc int, v Value, err error) (next int, ok bool, _ error) {
	w := regAt(c.code, rpc)
	k := regKept{pc: rpc + 1}
	if err == nil {
		dest := int(w.B())
		switch w.Op() {
		case bytecode.RInstanceOf, bytecode.RTypeof, bytecode.RGetProp, bytecode.RGetMethod:
			// R[A]'s: a getter's, called for get_prop (an accessor
			// inlined, jitRegInlined), as typeof's and instanceof's.
			dest = int(w.A())
		case bytecode.RGetMethodW:
			// R[A]'s, and the receiver to R[A+1], as get_method_w puts
			// them.
			dest = int(w.A())
			r.stack[wb+dest+1] = r.stack[wb+int(w.B())]
		case bytecode.RSetProp:
			// A setter's result, dropped.
			return k.pc, true, nil
		}
		r.stack[wb+dest] = v
		return k.pc, true, nil
	}
	h := r.regCatch(f, c, &k, regWindow(r.stack, wb), err)
	if h < 0 {
		return 0, true, err
	}
	return h, true, nil
}

// RegNativeResult is how a call's native run went (RegNativeCall).
type RegNativeResult struct {
	// Value and Err are what the call answered.
	Value Value
	Err   error
	// Skipped says why the call was made as an ordinary call: no register
	// frame, a refusal, no memory for the code.
	Skipped string
	// Compiled says the code was compiled for this call; Entered, at
	// which register pc native code was first entered, -1 for none;
	// OSR that it was a loop's header the evaluator had reached.
	Compiled bool
	Entered  int
	OSR      bool
}

// RegNativeStats are the runtime's counts of what native code made from
// register code did, for tests: its exits by kind, the invocations the
// interpreter finished, the records Go applied, the objects it made.
type RegNativeStats struct {
	Entries, Hosts, Guards, Polls, Interpreted, Records, Materialized uint64
	// GoCalls counts the calls of Go native code made that ran script, a
	// call's on the register frame among them (jitRegGoCall).
	GoCalls uint64
	// Unwound counts the native calls' callees Go finished after one of
	// them left native code (jitRegUnwind), and Resumed the native callers
	// that went on natively after such a callee.
	Unwound, Resumed uint64
	// HostOps is Hosts by the register instruction Go ran whole.
	HostOps map[string]uint64
	// GoCallOps is the calls of Go native code made that may run script,
	// by the register instruction and what it called -- a built-in by its
	// name, a function as "function" or, where it had code native callers
	// may call, "function with code" -- counted while the runtime reports
	// (SetJITReport).
	GoCallOps map[string]uint64
	// WholeLeft counts the instructions Go ran whole for native code,
	// called from it, that it finished at an exit (jitRegWhole).
	WholeLeft uint64
}

// RegNativeStats reports r's counts (RegNativeStats); zero before any.
func (r *Runtime) RegNativeStats() RegNativeStats {
	s := r.jit
	if s == nil {
		return RegNativeStats{}
	}
	return RegNativeStats{Entries: s.ssaEntries, Hosts: s.hosts, Guards: s.guards, Polls: s.budgets,
		Interpreted: s.interpreted, Records: s.ssaRecords, Materialized: s.materialized, GoCalls: s.goCalls,
		Unwound: s.unwound, Resumed: s.resumed, HostOps: maps.Clone(s.regHostOps), WholeLeft: s.wholeLeft,
		GoCallOps: s.regGoOpsByName()}
}

// jitRegGoKey is what a call of Go register code made made
// (RegNativeStats' GoCallOps): the register instruction and, for a call,
// what it called.
type jitRegGoKey struct {
	op     bytecode.ROp
	whole  bool
	callee string
}

// noteRegGoCall counts, while s reports, a call of Go of register
// instruction w that made what k says, callee the function a call called.
func (r *Runtime) noteRegGoCall(w bytecode.RInstr, whole bool, callee Value) {
	s := r.jit
	if !s.reporting {
		return
	}
	k := jitRegGoKey{op: w.Op(), whole: whole}
	if callee.IsObject() {
		switch fd := callee.Object().fn(); {
		case fd == nil:
			k.callee = "object"
		case fd.native != nil:
			k.callee = fd.name
			if k.callee == "" {
				k.callee = "built-in"
			}
		case fd.closure != nil && !fd.bound && r.jitRegHas(fd.closure):
			k.callee = "function with code"
		case fd.closure != nil && !fd.bound:
			k.callee = "function"
		default:
			k.callee = "bound"
		}
	}
	if s.regGoOps == nil {
		s.regGoOps = map[jitRegGoKey]uint64{}
	}
	s.regGoOps[k]++
}

// regGoOpsByName is regGoOps by a name of each key.
func (s *jitState) regGoOpsByName() map[string]uint64 {
	if len(s.regGoOps) == 0 {
		return nil
	}
	m := make(map[string]uint64, len(s.regGoOps))
	for k, n := range s.regGoOps {
		name := k.op.String()
		if k.whole {
			name = "whole " + name
		}
		if k.callee != "" {
			name += "->" + k.callee
		}
		m[name] += n
	}
	return m
}

// noteRegHost counts a host exit at register pc rpc of c by its register
// instruction (RegNativeStats' HostOps).
func (s *jitState) noteRegHost(c *regCode, rpc int) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return
	}
	w := regAt(c.code, rpc)
	k := w.Op().String()
	if s.regHostOps == nil {
		s.regHostOps = map[string]uint64{}
	}
	s.regHostOps[k]++
}

// RegNativeCall calls fn, a function of r's, with args, as a call from Go
// does, its frame run by native code compiled from its register code at
// its first call (jitRegFor) -- from its start, or, if osr, by on-stack
// replacement at the first loop header the evaluator reaches a second
// time, the frame run by the evaluator with the VM until then, as
// RegLowerCall runs it -- its exits going on in the register frame
// (jitRegRun). For tests, which compare it with an ordinary call.
func (r *Runtime) RegNativeCall(fnv Value, args []Value, osr bool) RegNativeResult {
	var o *Object
	var fd *funcData
	if fnv.IsObject() {
		o = fnv.Object()
		fd = o.fn()
	}
	var c *regCode
	if fd != nil && fd.closure != nil && !fd.bound && fd.extra == nil && fd.closure.realm == r.Realm {
		if c = fd.closure.reg; c == nil {
			c = r.regOf(fd.closure)
		}
	}
	plain := func(why string) RegNativeResult {
		v, err := r.Call(fnv, Undefined, args)
		return RegNativeResult{Value: v, Err: err, Skipped: why, Entered: -1}
	}
	if !r.jitEnabled {
		return plain("no JIT")
	}
	if c == nil || !c.wide || c.suspends || fd.arrow || c.leaf != 0 {
		return plain("no register frame")
	}
	cl := fd.closure
	var compiled bool
	if r.jit == nil || r.jit.regCache[r.jitKeyOf(cl.fn)] == nil || r.jit.regCache[r.jitKeyOf(cl.fn)].invalid {
		compiled = true
	}
	// Compiled here, on the spot: the call runs the code it asks for,
	// whatever the runtime's compiles in the background.
	bg, stressBg := r.jitBackground, r.jitStress.background
	r.jitBackground, r.jitStress.background = false, false
	e := r.jitRegFor(cl, c)
	r.jitBackground, r.jitStress.background = bg, stressBg
	switch {
	case e == nil:
		return plain("no room")
	case e.code == nil:
		return plain("refused: " + e.refusal)
	}
	this := Undefined
	if c.coerceThis {
		this = r.globalThis
	}
	base := r.stackTop
	if base+int(c.numRegs)+e.scratch+e.frame > len(r.stack) {
		return plain("no room")
	}
	m := min(int(c.params), len(args))
	window := r.stack[base : base+2+m : base+2+m]
	window[0] = Obj(o)
	window[1] = this
	copy(window[2:], args[:m])
	res := RegNativeResult{Compiled: compiled, Entered: -1}
	res.Value, res.Err = r.regLowerEnter(o, cl, c, base, m, args, func(f *frame) (Value, error) {
		if !osr || len(e.loops) == 0 {
			res.Entered = 0
			return r.jitRegRun(f, c, e, base, 0)
		}
		// The evaluator, the VM making what it does not, up to a loop
		// header the frame reaches a second time.
		p := e.p
		x := &regRunner{r: r, f: f, c: c, p: p, wb: base}
		slots := make([]ir.Value, p.Locals)
		x.load(slots)
		seen := map[uint32]bool{}
		pc := 0
		for {
			exit, err := p.EvaluateHost(slots, nil, x, pc, 1)
			switch {
			case err == errRegThrown:
				return Undefined, x.thrown
			case err != nil:
				return Undefined, err
			case exit.Kind == ir.Returned:
				return x.fromIR(exit.Value), nil
			}
			at := exit.State.PC
			x.store(slots)
			if exit.Kind != ir.BudgetExit {
				// Left for good: runReg from its pc.
				return r.runReg(f, c, int(at))
			}
			if _, ok := e.loopEntry(int(at)); ok {
				if seen[at] {
					res.Entered, res.OSR = int(at), true
					return r.jitRegRun(f, c, e, base, int(at))
				}
				seen[at] = true
			}
			pc = p.At(at)
			x.handles = x.handles[:0]
			x.load(slots)
		}
	})
	return res
}
