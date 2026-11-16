//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Tier-up from the register interpreter (docs/register-interpreter.md,
// Stage 3, 3a-3's step B): a runtime that runs the JIT compiles a register
// function's code once it is hot, and enters it at its calls and at its
// loops' headers.
//
// Hotness is sampled, not counted: no handler of either dispatcher -- the
// Go handler table's or the native handlers' -- does any work for it. The
// two budgets they already spend are the samples. A call that spends the
// interrupt budget (rhCallW's and regEnterW's check, which the native
// call handler leaves to rhCallW) credits its callee; a back edge that
// spends the back-edge budget (regBackEdge, which every dispatcher's jump
// leaves to its Go slow function) credits its frame's function. The
// function is compiled at its jitRegSamples-th sample, from what its
// caches learned meanwhile.
//
// Its code is entered:
//   - by a call that makes its frame in Go (regEnterW, jitRegEnter): a
//     call site is sent there once it is sampled calling a function that
//     has code (regCallJIT), which no fast path enters;
//   - by on-stack replacement at the loop whose back edge took the sample
//     (jitRegLoop), in the dispatcher that runs the frame, which goes on
//     where the code leaves it: at the register pc of a deoptimization or
//     of an instruction with no entry after it, or, where the code
//     returned, at a return of the code's own with what it returned.

// regCallJITMark is regCallJIT where the JIT is built in (regCallMarks).
const regCallJITMark = regCallJIT

// jitRegSamples is how many samples make a function hot.
const jitRegSamples = 2

// jitRegDeoptLimit is how many deoptimizations a function's code is
// compiled again after: past it, the function stays in the interpreter.
const jitRegDeoptLimit = 8

// jitRegTakes reports whether cl's register code c is compiled: a plain
// function's, a method's, an arrow's, an accessor's or a class's
// constructor's code of the form's own layout, a generator's and an async
// function's among them, and top-level code's -- a script's, a module's,
// an indirect eval's. Its frame is the call path's, an arrow's this, a
// constructor's object or a derived one's uninitialized this in its
// register 1 as the interpreter has them; a suspended frame's as
// regGenRun makes it again at each resumption. A leaf's -- a body the
// interpreter's calls answer without a frame -- is compiled for the calls
// that make one, native callers' among them (regCallSite). Only what
// jitRegCalled says is entered at its start; the rest at its loops, by
// on-stack replacement, and a suspended frame's code where it resumes.
func jitRegTakes(cl *closure, c *regCode) bool {
	switch cl.fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindArrow, bytecode.KindGetter, bytecode.KindSetter,
		bytecode.KindConstructor, bytecode.KindDerivedConstructor:
	default:
		return false
	}
	return c.wide && c.rc == cl.fn.RegCode()
}

// jitRegCalled reports whether cl's register code c, which jitRegTakes,
// is entered at its start by a call: that of neither a generator nor an
// async function, whose call makes its object and whose frame the
// interpreter suspends and makes again (regGenRun), nor top-level code,
// which runs once, as V8 enters top-level code only by on-stack
// replacement. Native callers call only such code, and inline only it.
func jitRegCalled(cl *closure, c *regCode) bool {
	return jitRegTakes(cl, c) && !c.suspends && !cl.fn.TopLevel && !cl.fn.IsModule
}

// jitRegHas reports whether cl's function has native code made from its
// register code.
func (r *Runtime) jitRegHas(cl *closure) bool {
	return cl.closureCode != nil && cl.closureCode.jitReg != nil
}

// jitRegSample is a sample of a run of cl's register code c (regEnterW).
func (r *Runtime) jitRegSample(cl *closure, c *regCode) {
	if r.jitEnabled && cl.closureCode != nil {
		r.jitRegHot(cl, c)
		r.jitRegStressSample()
	}
}

// jitRegNativeCallee compiles cl's function's register code c for the
// native callers that call it through its link, which has none yet: as the
// stack form's native callers have a callee with no code compiled at their
// first call of it (jitNativeCallee), not once it is hot -- they make no
// sample of it.
func (r *Runtime) jitRegNativeCallee(cl *closure, c *regCode) {
	cc := cl.closureCode
	if cc == nil || cc.jitRegRefused || cc.jitReg != nil || !r.jitEnabled {
		return
	}
	if !jitRegCalled(cl, c) {
		return
	}
	e := r.jitRegForAs(cl, c, true)
	switch {
	case e == nil:
	case e.code == nil:
		cc.jitRegRefused = true
	default:
		cc.jitReg = e
	}
}

// jitRegCallsNatively reports whether a call cl's function's register code
// c makes would be a native call, of a callee its site's cache holds or
// its code learned (regCallSites), as jitCallsNatively asks of the stack
// form's: compiled for that alone, a function of host operations is worth
// its code.
func (r *Runtime) jitRegCallsNatively(cl *closure, c *regCode) bool {
	if l := r.jit.regLearned[r.jitKeyOf(cl.fn)]; l != nil && len(l.nativeCalls) != 0 {
		return true
	}
	for _, w := range c.rc.Code {
		if op := w.Op(); op != bytecode.RCallW && op != bytecode.RCallMW || uint(w.C()) >= uint(len(cl.regCalls)) {
			continue
		}
		if e := &cl.regCalls[w.C()]; jitRegCallKind(e) && r.jitRegCallable(e.callee, e.cl, e.rc, int(w.X())) {
			return true
		}
	}
	return false
}

// jitRegStressSample makes the next call a sample too where the runtime
// compiles at a function's first call (QJS_JIT_STRESS's threshold,
// SetJITWarmUp's 1), so that every call is.
func (r *Runtime) jitRegStressSample() {
	if r.jitCallThreshold <= 1 {
		r.interruptCounter = 1
	}
}

// jitRegCallSample is a sample of a call at the site whose cache is e
// (rhCallW): its callee's hotness, and the site sent to regCallMiss once
// the callee has code, where regEnterW enters it.
func (r *Runtime) jitRegCallSample(e *regCallCache) {
	if !r.jitEnabled || e.cl == nil || e.rc == nil || e.cl.closureCode == nil {
		return
	}
	if r.jitRegHot(e.cl, e.rc) != nil {
		e.kind |= regCallJIT
	}
	r.jitRegStressSample()
}

// jitRegHot counts a sample of cl's function, its register code c, and
// returns its code: compiled at the jitRegSamples-th sample, nil before
// and where it is not compiled.
func (r *Runtime) jitRegHot(cl *closure, c *regCode) *jitRegEntry {
	if r.jit != nil && r.jit.inFlight() {
		// A check: compiles in the background that are done installed
		// (jitDrain).
		r.jitDrain()
	}
	cc := cl.closureCode
	if cc.jitRegRefused {
		return nil
	}
	if e := cc.jitReg; e != nil {
		if !e.invalid && !e.stale && e.icWait == 0 && e.mark != jitFeedbackMark(cl) {
			// Its caches' feedback changed since it was compiled -- what
			// they hold, a site that knew nothing then knowing a shape now
			// -- as V8's NotifyICChanged finds and the stack form's code
			// learns (jitFeedbackMark): compiled again for it once it has
			// run the runtime's jitICThreshold invocations' worth more, as
			// the stack form's budget is raised to: a sample's worth where
			// samples are the interrupt budget's, as many samples where
			// every call is one (jitRegStressSample).
			e.icWait = 1
			if r.jitCallThreshold <= 1 {
				e.icWait = max(1, int(r.jitICThreshold))
			}
		}
		if e.icWait != 0 {
			if e.icWait--; e.icWait == 0 {
				r.jitRegReopt(cl, e)
			}
		}
		if !e.invalid && !e.stale {
			return e
		}
		cc.jitReg = nil
	}
	if cc.jitRegHot++; cc.jitRegHot < jitRegSamples && r.jitCallThreshold > 1 {
		return nil
	}
	cc.jitRegHot = 0
	if !jitRegTakes(cl, c) {
		cc.jitRegRefused = true
		return nil
	}
	e := r.jitRegFor(cl, c)
	switch {
	case e == nil:
		// No room now.
		return nil
	case e.code == nil:
		// Refused for good, but for host operations alone, asked again once
		// hot again (jitRegCompile).
		cc.jitRegRefused = !e.hostOnly
		return nil
	}
	cc.jitReg = e
	return e
}

// jitRegInvalidate marks e, cl's function's code, invalid after a
// deoptimization: the function is sampled anew, and compiled again from
// what the interpreter learns meanwhile, up to jitRegDeoptLimit times.
func (r *Runtime) jitRegInvalidate(cl *closure, e *jitRegEntry) {
	r.jitRegInvalidateAs(cl, e, true)
}

// jitRegInvalidateLearned is jitRegInvalidate after a deoptimization that
// taught the code what it is compiled for next -- an operator's BigInts, an
// entry after a call Go went on at -- which no limit counts: what each site
// can learn so is bounded, a deoptimization at most for each kind it
// learns. Counted, a loop of seven BigInt operators, each learned at a
// deoptimization of its own, had its function refused for good before its
// code knew them all.
func (r *Runtime) jitRegInvalidateLearned(cl *closure, e *jitRegEntry) {
	r.jitRegInvalidateAs(cl, e, false)
}

func (r *Runtime) jitRegInvalidateAs(cl *closure, e *jitRegEntry, counted bool) {
	if !e.invalid {
		// Native callers call it no more.
		e.invalid = true
		key := r.jitKeyOf(cl.fn)
		if r.jit.regCache[key] == e {
			r.jit.regLinkUpdate(key, nil)
		}
	}
	cc := cl.closureCode
	if cc == nil {
		return
	}
	if cc.jitReg == e {
		cc.jitReg = nil
	}
	cc.jitRegHot = 0
	if !counted {
		return
	}
	if cc.jitRegDeopts++; cc.jitRegDeopts >= jitRegDeoptLimit {
		cc.jitRegRefused = true
	}
}

// jitRegReopt marks e, cl's function's code, stale for what it learned at
// its calls of Go or its exits (jitRegCallSeen), no deoptimization: its
// runs go on in it, as V8's code runs on with feedback it was not compiled
// for, until they leave it -- compiled again at an exit (jitRegGoOnAt), or
// the interpreter's at a loop's poll (jitRegLeft) -- but the function is
// sampled anew and compiled again for it, which no limit of
// deoptimizations counts -- what can be learned so is bounded. Its native
// callers call it through Go from here (its link has no entry), where it
// is sampled: code only native callers reach -- a callee of a loop's -- had
// otherwise never been compiled again, as the stack form's code is by its
// own exits (jitRecompileInRun).
func (r *Runtime) jitRegReopt(cl *closure, e *jitRegEntry) {
	if e.invalid || e.stale {
		return
	}
	e.stale = true
	if key := r.jitKeyOf(cl.fn); r.jit.regCache[key] == e {
		r.jit.regLinkUpdate(key, nil)
	}
	if cc := cl.closureCode; cc != nil {
		if cc.jitReg == e {
			cc.jitReg = nil
		}
		cc.jitRegHot = 0
	}
}

// regLearnedOf is what the code of the function whose key is key learned
// at its exits (regLearned), made if it has learned nothing yet.
func (s *jitState) regLearnedOf(key jitKey) *jitEntry {
	if l := s.regLearned[key]; l != nil {
		return l
	}
	if s.regLearned == nil {
		s.regLearned = map[jitKey]*jitEntry{}
	}
	l := &jitEntry{jitLearned: &jitLearned{reg: true}}
	s.regLearned[key] = l
	return l
}

// jitRegPropOp is the named property operation of the register instruction
// at rpc of c, on the frame whose window is at wb, as the JIT's learning
// sees it (jitPropOp): get_prop's, get_method's and get_method_w's reads,
// set_prop's write, add_prop's read and store, which share the store's
// site; false for any other instruction.
func (r *Runtime) jitRegPropOp(f *frame, c *regCode, wb, rpc int) (jitPropOp, bool) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return jitPropOp{}, false
	}
	w := c.rc.Code[rpc]
	recv := int(w.B())
	op := jitPropOp{}
	switch w.Op() {
	case bytecode.RGetProp:
	case bytecode.RGetMethod, bytecode.RGetMethodW:
		op.method = true
	case bytecode.RSetProp:
		op.write, recv = true, int(w.A())
	case bytecode.RAddProp, bytecode.RAddPropI:
		recv = int(w.A())
	default:
		return jitPropOp{}, false
	}
	site := int(w.C())
	if site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(f.cl.names) || wb+recv >= len(r.stack) {
		return jitPropOp{}, false
	}
	op.key, op.recv = f.cl.names[c.rc.SiteName[site]], r.stack[wb+recv]
	if site < len(f.cl.ic) {
		op.c = &f.cl.ic[site]
	}
	return op, true
}

// jitRegSeenFeedback adds to what cl's register code c learned
// (regLearned) what its property sites' caches have seen besides their
// entries, at each compile, as the stack form's compiles do
// (jitSeenFeedback): the shapes the interpreter met at each, which the code
// is compiled for from the first, not learned one exit and one compile at
// a time.
func (r *Runtime) jitRegSeenFeedback(cl *closure, c *regCode) {
	l := r.jitState().regLearnedOf(r.jitKeyOf(cl.fn))
	for rpc, w := range c.rc.Code {
		write := false
		switch w.Op() {
		case bytecode.RGetProp, bytecode.RGetMethod, bytecode.RGetMethodW:
		case bytecode.RSetProp:
			write = true
		default:
			// add_prop's site is its store's, whose read the stack form
			// makes at a site of its own.
			continue
		}
		site := int(w.C())
		if site >= len(cl.ic) || site >= len(c.rc.SiteName) || int(c.rc.SiteName[site]) >= len(cl.names) {
			continue
		}
		r.jitSeenSite(l, rpc, &cl.ic[site], write, cl.names[c.rc.SiteName[site]], true)
	}
}

// jitRegDeoptSeen learns from a deoptimization of e, code made from c, at
// register pc rpc, the frame's window at wb holding the state at its
// instruction's start, before the interpreter runs it: a property site's
// receiver of a shape its cases do not have, or that its cache cannot
// hold, as the stack form's code learns (jitDeoptSeenOp) -- the site's
// cache learning the shape as the interpreter makes the operation, the
// code compiled again with its cases -- and any other guard's failure as
// a register pc the next compiles build generic.
func (r *Runtime) jitRegDeoptSeen(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) {
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	if op, ok := r.jitRegPropOp(f, c, wb, rpc); ok {
		r.jit.shapeDeopts++
		r.jitDeoptSeenOp(l, rpc, op, e.serial)
		return
	}
	if !slices.Contains(l.failed, int32(rpc)) {
		l.failed = append(l.failed, int32(rpc))
	}
}

// jitRegEntrySeen learns that an entry's speculation of f's function's
// code failed at register pc rpc -- a loop's header or the start, the
// registers it loads not of the kinds the code was compiled for -- which
// the next compiles enter generic there (jitRegFeedback's EntryGeneric),
// as the stack form's code learns it (failedEntries), the instructions
// there not made generic for it.
func (r *Runtime) jitRegEntrySeen(f *frame, rpc int) {
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	if !slices.Contains(l.failedEntries, uint32(rpc)) {
		l.failedEntries = append(l.failedEntries, uint32(rpc))
	}
}

// jitRegResumeSeen learns that Go goes on at register pc rpc of e's code,
// which has no entry there: where that is after a call the code was built
// with no entry after (jitRegEntry's unresumed), the function's next
// compile has one (jitRegFeedback's Resumed), and the code is invalid from
// here, as the stack form's code is compiled again with the entry
// (jitGoOn).
func (r *Runtime) jitRegResumeSeen(f *frame, e *jitRegEntry, rpc int) {
	if e.invalid || !slices.Contains(e.unresumed, rpc) {
		return
	}
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	if slices.Contains(l.resumed, uint32(rpc)) {
		return
	}
	l.resumed = append(l.resumed, uint32(rpc))
	r.jitRegInvalidateLearned(f.cl, e)
}

// jitRegHostSeen learns from a host exit at register pc rpc of c, before
// Go runs its instruction: a property read native code left to Go -- a
// megamorphic site's receiver that neither its table nor the megamorphic
// cache had, a primitive's -- as the stack form's code learns
// (jitPolySeenOp). A write's it learns after Go made it (jitRegWriteSeen),
// from what it returns: its operation, the object written and its shape
// before.
func (r *Runtime) jitRegHostSeen(f *frame, c *regCode, wb, rpc int) jitRegWrite {
	op, ok := r.jitRegPropOp(f, c, wb, rpc)
	if !ok {
		return jitRegWrite{}
	}
	if op.write {
		x := jitRegWrite{op: op, write: true}
		if op.recv.IsObject() {
			x.o = op.recv.Object()
			x.before = x.o.shape
		}
		return x
	}
	r.jitPolySeenOp(r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn)), rpc, op)
	return jitRegWrite{}
}

// jitRegPrivateSeen learns from a host exit of e's code at get_private,
// register pc rpc of c, before Go runs it, as the stack form's code learns
// (jitPrivateSeen): a member that is its object's own data but no number --
// a private method's function -- the read met, which the code compiled
// again reads as a value (jitNumberMissed), compiled again from here
// (jitRegReopt). The shapes the read meets the interpreter's run of it
// adds to its site's cache, which the code compiled again is compiled for.
func (r *Runtime) jitRegPrivateSeen(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return
	}
	w := c.rc.Code[rpc]
	if w.Op() != bytecode.RGetPrivate || wb+int(w.B()) >= len(r.stack) || wb+int(w.C()) >= len(r.stack) {
		return
	}
	o, k := r.stack[wb+int(w.B())], r.stack[wb+int(w.C())]
	if !o.IsObject() {
		return
	}
	ob := o.Object()
	i := ob.findOwn(regPrivateKey(f, k))
	if i < 0 || ob.shape == nil || i >= ob.shape.n || int(i) >= int(ob.valn) || ob.shape.keys[i].isAccessor() || ob.vals()[i].IsNumber() {
		return
	}
	l := r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn))
	if slices.Contains(l.failed, int32(rpc)) {
		return
	}
	jitNumberMissed(l, rpc)
	r.jitRegReopt(f.cl, e)
}

// jitRegWrite is a property write a host exit is about to have Go make
// (jitRegHostSeen): its operation, and the object it writes, of shape
// before then.
type jitRegWrite struct {
	op     jitPropOp
	write  bool
	o      *Object
	before *shape
}

// jitRegWriteSeen learns from the property write x at register pc rpc,
// which Go made at a host exit: the cases its cache now knows, as the
// stack form's code learns (jitPolyWriteSeenOp).
func (r *Runtime) jitRegWriteSeen(f *frame, c *regCode, e *jitRegEntry, rpc int, x jitRegWrite) {
	if x.op.c == nil {
		return
	}
	r.jitPolyWriteSeenOp(r.jit.regLearnedOf(r.jitKeyOf(f.cl.fn)), rpc, x.op.c, x.op.key, true, x.o, x.before)
}

// jitRegEnter runs f, the frame regEnterW made for a call of cl, its
// register code c and its window at base, in cl's function's native code
// from register pc start -- its start, or, for the frame of a generator or
// an async function regGenRun made again, where it resumes -- and in the
// interpreter where the code leaves it, or has no entry there.
func (r *Runtime) jitRegEnter(f *frame, cl *closure, c *regCode, base, start int) (Value, error) {
	e := cl.closureCode.jitReg
	if e == nil || e.invalid || !r.jitEnabled {
		return r.runReg(f, c, start)
	}
	if ipc := e.p.At(uint32(start)); start != 0 && e.code != nil && ipc >= 0 && e.code.HasEntry(ipc) {
		r.jit.regResumes++
	}
	return r.jitRegRun(f, c, e, f.base-int(c.locals), start)
}

// jitRegLoop is the back edge to register pc target's sample, in the frame
// st runs (regBackEdge): its function's hotness, and its code entered at
// the loop's header where it has an entry there. It returns the pc the
// dispatcher goes on at: target where the code is not entered; where the
// code left the frame to it; a return of the code's own where the code
// returned, its register given the value (jitRegEntry's returns); regDone
// where the code threw what none of the frame's handlers takes, the error
// in st.
func (r *Runtime) jitRegLoop(st *regState, target int) int {
	r.backEdges = r.jit.backEdgeBudget()
	f, c := st.f, st.c
	cl := f.cl
	if !r.jitEnabled || cl == nil || cl.closureCode == nil || cl.reg != c {
		return target
	}
	e := r.jitRegHot(cl, c)
	if e == nil || !e.goesOn {
		return target
	}
	if _, ok := e.loopEntry(target); !ok {
		return target
	}
	pc, v, done, err := r.jitRegGoOn(f, c, e, st.wb, target)
	switch {
	case err != nil:
		st.err = err
		return regDone
	case !done:
		return pc
	case v.IsUndefined() && e.retUndef >= 0:
		return e.retUndef
	case e.retPC >= 0:
		st.regs[e.retReg] = v
		return e.retPC
	}
	// Unreachable: code that returns a value has a return of one.
	panic("vm: native code returned where its register code has no return")
}

// findReturns finds the returns of e's register code rc that a frame the
// code returned from goes on at in the interpreter (jitRegLoop): a
// ret_undef, and a ret whose register no closure captures, which the
// value is written to -- the frame ends there, and nothing reads it after.
// Each is -1 where the code has none. goesOn says the interpreter can end
// every frame the code returns from so: the code has such a ret where it
// has a ret at all.
func (e *jitRegEntry) findReturns(rc *bytecode.RegCode) {
	e.retPC, e.retUndef = -1, -1
	rets := false
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RRetUndef:
			if e.retUndef < 0 {
				e.retUndef = pc
			}
		case bytecode.RRet:
			rets = true
			if a := int(w.A()); e.retPC < 0 && (a >= len(e.p.Captured) || !e.p.Captured[a]) {
				e.retPC, e.retReg = pc, a
			}
		}
	}
	e.goesOn = e.retPC >= 0 || !rets && e.retUndef >= 0
}
