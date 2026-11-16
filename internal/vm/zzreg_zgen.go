package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Generators and async functions as register code
// (docs/register-interpreter.md, "Generators and async functions").
//
// A generator of register code keeps its frame's window in its vals,
// numRegs values and then, where the code keeps them, the arguments it
// was called with; its pc is the register pc it resumes at. A resumption
// copies the window to the top of the stack, moves the captures open on
// its registers there and runs the code, whose first instruction,
// gen_entry, goes on at the pc (regColdGen); a suspension hands back the
// value and its pc, and the window and the captures go back to the
// generator. What the resumption delivers -- the value sent in, a throw
// injected, a return, how a yield* is resumed -- the generator writes to
// the suspension's registers and pc before it runs (regGenResume): the
// code has a return path after each yield, and yield* is a loop of the
// code's own, as the stack form's is. The drivers above it -- next,
// return and throw, the async functions' and the async generators' steps
// -- are the stack form's, which resumeFrame hands such a generator's
// resumption.

// regGenOf is the register code a call of cl, callee's closure, runs as a
// generator's or an async function's body, or nil: the register tier is on,
// the function has register code, and nothing of its surroundings is what
// that code cannot run in: a derived constructor's this the code does not
// read through its binding. The with objects and the eval's variables
// around it are the frame's (frameRare), kept across a suspension.
func (r *Runtime) regGenOf(cl *closure, callee *Object) *regCode {
	if !r.regOn() {
		return nil
	}
	c := cl.reg
	if c == nil {
		c = r.regOf(cl)
	}
	if c == nil || !c.wide || !c.suspends {
		return nil
	}
	if callee != nil {
		if fd := callee.fn(); fd != nil {
			if x := fd.extra; x != nil && x.lexThisRef != nil && !c.rc.ThisRef {
				return nil
			}
		}
	}
	return c
}

// newRegGenerator is newGenerator for a function of register code c: the
// window made as a call makes it, kept in the generator, and a
// generator's parameters bound at once, up to end_params, which suspends
// it. An async function's are bound by its first resumption, which runs
// at once (runAsync), an error rejecting its promise.
func (r *Runtime) newRegGenerator(cl *closure, c *regCode, this Value, args []Value, callee *Object, async bool, newTarget Value) (*Object, error) {
	fn := cl.fn
	n := int(c.numRegs)
	kept := 0
	if c.args {
		kept = len(args)
	}
	room := genPlainKind
	if async {
		room = genAsyncKind
	} else if cl.genLoops {
		room = genLoopKind
	}
	// The window's registers from genFrom: the callee's and this's, which
	// a resumption makes again, kept only where the code needs them kept.
	from := int(c.genFrom)
	gobj, vals := genAlloc(n-from+kept, room)
	var fd *funcData
	if callee != nil {
		if fd = callee.fn(); fd != nil && fd.arrow {
			// An arrow's this is its surroundings', its frame's as a
			// call's is: the generator's this is this's register,
			// which a resumption makes again from it (genFrom).
			this = fd.extra.lexThis
		}
	}
	if from == 0 {
		vals[0], vals[1] = Undefined, this
		if callee != nil {
			vals[0] = Obj(callee)
		}
	}
	w := vals[: n-from : n-from]
	p := int(c.params)
	m := min(p, len(args))
	copy(w[2-from:2-from+m], args[:m])
	for i := 2 + m; i < 2+p+int(c.inits); i++ {
		w[i-from] = Undefined
	}
	copy(vals[n-from:], args[:kept])
	gobj.Object = Object{class: ClassGenerator, flags: objExtensible}
	g := &gobj.g
	*g = generator{
		cl:     cl,
		this:   this,
		callee: callee,
		vals:   vals,
		nargs:  uint32(kept),
		async:  async,
		room:   g.room,
		reg:    true,
	}
	if !newTarget.IsUndefined() {
		g.rareOf().newTarget = newTarget
	}
	if async && fn.Generator {
		g.rareOf()
	}
	if fd != nil && fd.arrow && fd.extra.lexThisRef != nil {
		g.rareOf().thisRef = fd.extra.lexThisRef
	}
	// The with objects and the eval's variables around it, and a scope
	// for its own eval's, as newGenerator gives a generator's frames.
	if fd != nil && fd.extra != nil {
		if w := fd.extra.with(); len(w) > 0 {
			g.rareOf().withScopes = w[:len(w):len(w)]
		}
		if ev := fd.extra.evalVars(); ev != nil {
			g.rareOf().evalVars = ev
		}
	}
	if fn.HasDirectEval {
		ev := newObject(nil, ClassObject)
		ev.flags |= objEvalVars
		g.rareOf().evalVars = ev
		w := g.withScopes()
		g.rareOf().withScopes = append(w[:len(w):len(w)], ev)
	}
	if fn.Generator {
		if w := regAt(c.code, 1); w.Op() == bytecode.REndParams {
			// Parameters the window's copy has bound, end_params next to
			// gen_entry: the body starts past it.
			g.pc = 2
		} else if _, _, err := r.regGenRun(g, c, 0); err != nil {
			return nil, err
		}
	}
	proto := r.proto.generator
	if async {
		proto = r.proto.asyncGenerator
	}
	if callee != nil && fn.Generator {
		if p := callee.getOwn(atomPrototype); p.ok() && p.flags()&(propAccessor|propPrivate|propDeleted) == 0 {
			if p.value().IsObject() {
				proto = p.value().Object()
			}
		} else if custom, err := r.getValueProp(Obj(callee), atomPrototype); err == nil &&
			custom.IsObject() {
			proto = custom.Object()
		}
	}
	gobj.setProto(proto)
	return &gobj.Object, nil
}

// regGenResume is resumeFrame for a generator of register code that has
// not finished and is not running: what the resumption delivers is
// written to the suspension's registers, and the code run from the pc
// that goes on with it -- a yield's next instruction for a return, the
// one after the jump past that for a next, the yield itself, negated, for
// a throw, which gen_entry throws there.
func (r *Runtime) regGenResume(g *generator, sent Value, mode resumeMode) (resumeResult, error) {
	c := g.cl.reg
	start := int(g.pc)
	if start > 0 {
		w := regAt(c.code, start-1)
		// vals keeps the window's registers from genFrom.
		a := int(w.A()) - int(c.genFrom)
		if w.Op() == bytecode.RYield {
			g.vals[a] = sent
			switch mode {
			case resumeThrow:
				start = -start
			case resumeReturn:
				start++
			default:
				start += 1 + int(regJumpOff(regAt(c.code, start)))
			}
		} else {
			switch w.Op() {
			case bytecode.RAwait:
				g.vals[a] = sent
				if mode == resumeThrow {
					start = -start
				}
			case bytecode.RYieldStar:
				g.vals[a] = sent
				g.vals[a+1] = Int(int(mode))
			}
		}
	}
	g.state = genExecuting
	v, sig, err := r.regGenRun(g, c, start)
	if sig != nil {
		g.delegating = sig.delegate
		g.state = genSuspendedYield
		g.started = true
		res := resumeResult{value: sig.value, how: suspendHow(sig)}
		if sig.delegate && !sig.raw && !g.async {
			// A value a delegate stepped directly gave (regDelegateNext),
			// whose result object is of the delegate's next's realm, this
			// one, as resumeFrame's.
			res.realm = g.cl.realm
		}
		// The suspension is the runtime's one, which is not to keep the
		// value alive.
		sig.value = Undefined
		return res, nil
	}
	g.state = genCompleted
	if err != nil {
		return resumeResult{value: Undefined, how: resumeDone}, err
	}
	return resumeResult{value: v, how: resumeDone}, nil
}

// regGenStep is iter_next of a for-of's cursor over g, a generator of
// register code stepped by its realm's next: next's frame, as stepIter
// writes it out, and the body resumed from its suspension
// (regGenResume), without the general resumption's search for a tree and
// its answers for a generator running, finished or of another realm,
// which stepIter steps. The cursor is done once it is, or once a step
// fails.
func (r *Runtime) regGenStep(st *iterState, g *generator) (Value, bool, error) {
	if g.state > genSuspendedYield || g.cl.realm != nil && g.cl.realm != r.Realm {
		v, ok, err := r.stepIter(st.iter, st.next)
		if !ok {
			st.done = true
		}
		return v, ok, err
	}
	if err := r.tick(); err != nil {
		st.done = true
		return Undefined, false, err
	}
	if r.frameDepth >= r.maxFrames {
		st.done = true
		return Undefined, false, r.throwRangeError("maximum call stack size exceeded")
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	gn := r.genNextFn
	f := &r.cur[off]
	f.cl = nil
	f.native = gn.fn().name
	f.this = st.iter
	f.newTarget = Undefined
	f.callee = gn
	f.args = nil
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	got, err := r.regGenNext(g)
	r.frameDepth--
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	if !got.raw() || !got.value.IsObject() {
		if got.done() {
			st.done = true
			return Undefined, false, nil
		}
		return got.value, true, nil
	}
	// A delegate's own result, which a yield* hands out as it is.
	res := got.value
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	if done.Truthy() {
		st.done = true
		return Undefined, false, nil
	}
	v, err := r.getValueProp(res, atomValue)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	return v, true, nil
}

// regGenRun runs g's frame, of register code c, from start (see
// regGenResume): its window copied to the top of the stack and the
// captures open on it moved there, and back to the generator where it
// suspends again, which it reports; closed where it ends.
func (r *Runtime) regGenRun(g *generator, c *regCode, start int) (Value, *suspendSignal, error) {
	if r.frameDepth >= r.maxFrames {
		return Undefined, nil, r.throwRangeError("maximum call stack size exceeded")
	}
	n := int(c.numRegs)
	base := r.stackTop
	top := base + n
	if top > len(r.stack) {
		return Undefined, nil, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = top
	if top > r.stackHigh {
		r.stackHigh = top
	}
	w := r.stack[base:top:top]
	from := int(c.genFrom)
	saved := g.vals[: n-from : n-from]
	kept := w[from:]
	copy(kept, saved)
	if from != 0 {
		// The callee's register and this's, which the generator keeps.
		w[0], w[1] = Undefined, g.this
		if cal := g.callee; cal != nil {
			w[0] = Obj(cal)
		}
	}
	ups := g.savedUpvalues()
	regMoveUps(ups, saved, kept)
	f := r.pushFrame()
	f.cl = g.cl
	f.locals = nil
	if c.args {
		// A mapped arguments object's aliases are captures of the
		// parameters' registers (newArgumentsObject), which move with
		// the window.
		p := int(c.params)
		f.locals = w[2 : 2+p : 2+p]
	}
	f.base = base
	f.pc = 0
	f.this = g.this
	f.callee = g.callee
	f.args = g.vals[n-from:]
	f.handlers, f.openUpvalues = nil, ups
	g.frameRare(f)
	f.native = ""
	// A resumption that goes on runs from its pc; one that throws runs
	// gen_entry, which throws at the suspension.
	f.savedSP = 0
	if start < 0 {
		f.savedSP, start = start, 0
	}
	f.paramsOnly = false
	f.regKind = 0
	if g.awaitJob {
		g.awaitJob = false
		f.regKind = regFrameAwaitJob
	}
	v, err := r.runReg(f, c, start)
	var sig *suspendSignal
	if s, ok := err.(*suspendSignal); ok {
		sig, err = s, nil
		g.pc = uint32(f.savedSP)
		// The with objects it is inside, for its next resumption.
		g.saveWith(f.withScopes)
		copy(saved, kept)
		if ups := f.openUpvalues; len(ups) != 0 || g.saved != nil {
			regMoveUps(ups, kept, saved)
			g.keep(nil, nil, ups)
		}
	} else {
		for _, u := range f.openUpvalues {
			u.close()
		}
		g.saved = nil
	}
	f.savedSP = 0
	f.args = nil
	f.detachSuspended()
	r.frameDepth--
	r.stackTop = base
	return v, sig, err
}

// regCapsFrame reports whether a closure rc makes captures the callee's
// register or this's, which a generator's vals then keep (genFrom).
func regCapsFrame(rc *bytecode.RegCode) bool {
	for _, caps := range rc.Caps {
		for _, r := range caps {
			if r < 2 {
				return true
			}
		}
	}
	return false
}

// regYield is yield's suspension of frame f with v, to go on at pc, the
// instruction after it (regGenResume).
func (r *Runtime) regYield(f *frame, v Value, pc int) error {
	f.savedSP = pc
	sig := &r.treeSig
	*sig = suspendSignal{value: v}
	return sig
}

// regDelegate is the generator a synchronous yield*'s cursor st steps by
// its realm's intrinsic next, or nil: the delegation steps it directly
// (regDelegateNext), as the tree's delegateNext does.
func (r *Runtime) regDelegate(st *iterState) *generator {
	if st.forIn || st.arr != nil || !st.iter.IsObject() || !st.next.IsObject() {
		return nil
	}
	n, it := st.next.Object(), st.iter.Object()
	if fd := n.fn(); fd == nil || fd.iterNext != iterNextGenerator || fd.realm != nil && fd.realm != r.Realm ||
		it.class != ClassGenerator {
		return nil
	}
	return genOf(it)
}

// regDelegateNext is iter_resume of a synchronous yield* whose delegate,
// g, is a generator its realm's next steps, resumed by next: g is
// stepped directly, as a for-of steps one, in next's frame, and neither
// next's result nor its arguments are made. What g yields the delegation
// yields on as a value, the frame suspended as at the yield_star after
// iter_unpack_delegate (the verifier checks it is there), which the
// consumer's next puts in a result object or a for-of reads as it is; g's
// return value ends the delegation, the jump after iter_resume taken
// with it; a result object g hands over as it is, a delegation of its
// own to another iterator, goes to iter_unpack_delegate as any result
// does.
func (r *Runtime) regDelegateNext(f *frame, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value, st *iterState, g *generator) error {
	st.delegation = true
	if err := r.tick(); err != nil {
		return err
	}
	n := st.next.Object()
	if err := r.nativeFrame(n, n.fn(), st.iter, nil, Undefined); err != nil {
		return err
	}
	got, err := r.resumeFull(g, regs[w.C()], resumeNext)
	r.frameDepth--
	if err != nil {
		return err
	}
	switch {
	case got.raw() && got.value.IsObject():
		regs[w.A()] = got.value
		k.pc++
	case got.done():
		st.done = true
		regs[w.A()] = got.value
	default:
		f.savedSP = k.pc + 4
		sig := &r.treeSig
		*sig = suspendSignal{value: got.value, delegate: true}
		return sig
	}
	return nil
}

// regMoveUps points each of ups open on a register of window from at the
// same register of window to.
func regMoveUps(ups []*upvalue, from, to []Value) {
	if len(ups) == 0 || len(from) == 0 {
		return
	}
	lo := uintptr(unsafe.Pointer(unsafe.SliceData(from)))
	size := unsafe.Sizeof(Value{})
	for _, u := range ups {
		if i := (uintptr(unsafe.Pointer(u.slot)) - lo) / size; i < uintptr(len(from)) {
			u.slot = &to[i]
		}
	}
}

// regGenEntry is gen_entry.
func (r *Runtime) regGenEntry(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	// Run only by a frame's first run and by a throw injected: a
	// resumption that goes on starts past it (regGenRun).
	pc := f.savedSP
	f.savedSP = 0
	if pc < 0 {
		// A throw injected at the suspension at -pc-1, its value in
		// the suspension's register.
		k.pc = -pc
		f.pc = c.src[-pc-1] + 1
		return r.throw(regs[regAt(c.code, -pc-1).A()])
	}
	return nil
}

// regSuspend is end_params, await, yield_star.
func (r *Runtime) regSuspend(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	op := w.Op()
	if op == bytecode.RAwait && f.regKind == regFrameAwaitJob && !r.HasPendingJobs() {
		// An await's job, the queue's last, would resume the frame
		// with what a settled value settled to as the queue's next
		// job: it goes on in place.
		if v, ok := r.regSettled(regs[w.B()]); ok {
			if err := r.checkInterrupt(); err != nil {
				return err
			}
			regs[w.A()] = v
			return nil
		}
	}
	f.savedSP = k.pc
	sig := &r.treeSig
	*sig = suspendSignal{
		value:    Undefined,
		await:    op == bytecode.RAwait,
		delegate: op == bytecode.RYieldStar,
		raw:      op == bytecode.RYieldStar && !c.fn.Async,
	}
	if op != bytecode.REndParams {
		sig.value = regs[w.B()]
	}
	return sig
}

// regIterResume is iter_resume.
func (r *Runtime) regIterResume(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	if !c.fn.Async && resumeMode(regs[w.C()+1].Number()) == resumeNext {
		if st := cursorOf(regs[w.B()]); st != nil {
			if g := r.regDelegate(st); g != nil {
				return r.regDelegateNext(f, k, w, regs, st, g)
			}
		}
	}
	res, done, err := r.iterResume(regs[w.B()], regs[w.C()], resumeMode(regs[w.C()+1].Number()), c.fn.Async)
	if err != nil {
		return err
	}
	regs[w.A()] = res
	if !done {
		k.pc++
	}
	return nil
}

// regIterUnpackDelegate is iter_unpack_delegate.
func (r *Runtime) regIterUnpackDelegate(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	res := regs[w.B()]
	if !res.IsObject() {
		return r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		return err
	}
	if done.Truthy() || c.fn.Async {
		v, err := r.getValueProp(res, atomValue)
		if err != nil {
			return err
		}
		res = v
	}
	regs[w.A()] = res
	if !done.Truthy() {
		k.pc++
	}
	return nil
}

// regAsyncStart is for_await_of_start, async_iter_next.
func (r *Runtime) regAsyncStart(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	op := w.Op()
	start := (*Runtime).startForAwaitOf
	if op == bytecode.RAsyncIterNext {
		start = (*Runtime).asyncIterNext
	}
	v, err := start(r, regs[w.B()])
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regIterResult is iter_result.
func (r *Runtime) regIterResult(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	res := regs[w.B()]
	if !res.IsObject() {
		return r.throwTypeError("an iterator result must be an object")
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil || done.Truthy() {
		return err
	}
	v, err := r.getValueProp(res, atomValue)
	if err != nil {
		return err
	}
	regs[w.A()] = v
	k.pc++
	return nil
}

// regAsyncIterClose is async_iter_close.
func (r *Runtime) regAsyncIterClose(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	v, ok, err := asyncIterClose(r, regs[w.B()], 0)
	if ok {
		regs[w.A()] = v
		k.pc++
	}
	return err
}

// regAsyncIterCheck is async_iter_check.
func (r *Runtime) regAsyncIterCheck(f *frame, c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	_, _, err := asyncIterClose(r, regs[w.A()], 1)
	return err
}

// regSettled is what an await of v resumes with as its job's next, where
// that is known without a step of the queue's or code of the program's: a
// value that is not an object, or a fulfilled promise of the realm's own
// constructor, found as toPromiseErr finds it.
func (r *Runtime) regSettled(v Value) (Value, bool) {
	if !v.IsObject() {
		return v, true
	}
	o := v.Object()
	if o.class != ClassPromise || o.proto() != r.proto.promise || o.getOwn(atomConstructor).ok() {
		return Undefined, false
	}
	if p := o.proto().getOwn(atomConstructor); !p.ok() || p.flags()&(propAccessor|propPrivate) != 0 ||
		!p.value().IsObject() || p.value().Object() != r.promiseCtor {
		return Undefined, false
	}
	if d, ok := o.dataOf().(*promiseData); ok && d.state == promiseFulfilled {
		return d.value, true
	}
	return Undefined, false
}

// regGenNext is regGenResume of next with undefined, a for-of's step
// (regGenStep): a yield's, a loop's every step but its first, run past the
// jump after it without regGenResume's other resumptions' switch. It is
// last in the file, so that what comes before it, regDelegateNext's
// steps among them, stays where it was.
func (r *Runtime) regGenNext(g *generator) (resumeResult, error) {
	c := g.cl.reg
	pc := int(g.pc)
	if pc <= 0 {
		return r.regGenResume(g, Undefined, resumeNext)
	}
	w := regAt(c.code, pc-1)
	if w.Op() != bytecode.RYield {
		return r.regGenResume(g, Undefined, resumeNext)
	}
	g.vals[int(w.A())-int(c.genFrom)] = Undefined
	g.state = genExecuting
	v, sig, err := r.regGenRun(g, c, pc+1+int(regJumpOff(regAt(c.code, pc))))
	if sig != nil {
		g.delegating = sig.delegate
		g.state = genSuspendedYield
		res := resumeResult{value: sig.value, how: suspendHow(sig)}
		sig.value = Undefined
		return res, nil
	}
	g.state = genCompleted
	return resumeResult{value: v, how: resumeDone}, err
}
