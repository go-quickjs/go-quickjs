package vm

// A callback a built-in calls many times over, kept ready from one call to
// the next (docs/register-interpreter.md, "Frames and calls").

// regCallback is a function of register code a built-in calls many times
// over -- the callback of forEach, some or a sort's comparator -- its
// window reserved at the stack's top and, natively, its code entry's state
// and context kept from one call to the next (regCallbackOf): each call
// writes the window's registers and the frame record and runs the code,
// rather than going through callDirect, regDirect and regEnterW and making
// a code entry anew. A leaf is run so too, with a frame -- one entered
// natively is cheaper than the stack form's frameless evaluator
// (pureCallAt) -- but a short straight body of numbers and upvalues
// (regLeafCode), whose register code regLeafRun runs in the window faster
// still: acc + x, a - b. The frame record is pushed for the call alone, so that what Go runs between
// two calls -- a comparator's result converted to a number -- finds no
// frame of it.
type regCallback struct {
	r    *Runtime
	o    *Object
	cl   *closure
	c    *regCode
	this Value
	// st and ctx are the native code entry's state and context, or nil
	// where the Go table runs the frame.
	st          *regState
	ctx         *nativeCtx
	base, saved int
	// set is the frame record the state and the context's top frame were
	// last made for, nil where a call has left them otherwise (a tail
	// call's callee, an exit's state): a call in the same record finds
	// them made (nativeCallbackRun).
	set *frame
	// ownThis says the callee's this is the one a call gives it, where an
	// arrow's is its own (callWith).
	ownThis bool
	// next is the next of the runtime's callbacks not in use (cbFree).
	next *regCallback
}

// regCallbackOf is fn, called with this, as a regCallback, or nil where its
// calls are callDirect's: a function regDirect does not enter with a frame
// -- not register code of the form's own layout of this realm, a body
// answered from its window or its code (regLeafArg, regLeafConst), a body
// that keeps its arguments, a generator's or an async function's, a sloppy one of a primitive this -- or one of
// a runtime of Node's quirks or a debugger, a stack with no room for its
// window. A leaf is callDirect's as before in a runtime that runs the JIT,
// which counts it there, and where the Go table runs register frames
// (QJS_NONATIVE), whose loop is slower than the frameless evaluators --
// until they have given up on its body (pureMiss), as a BigInt sum makes
// them, from when the callback enters it with a frame. The caller ends it
// (end).
func (r *Runtime) regCallbackOf(fn, this Value) *regCallback {
	if !fn.IsObject() || r.nodeQuirks || r.debug != nil {
		return nil
	}
	o := fn.Object()
	fd := o.fn()
	if fd == nil || fd.regCall == nil || fd.closure.realm != r.Realm {
		return nil
	}
	c := fd.regCall
	if c == nil || !c.wide || c.args || c.suspends {
		return nil
	}
	switch c.leaf {
	case regLeafNone:
	case regLeafArg, regLeafConst:
		// Answered with no frame and no evaluator (regDirect).
		return nil
	default:
		if jitBuilt && r.jitOn() {
			// The JIT's runtime counts its leaves in callDirect.
			return nil
		}
		if !regNativeOn && fd.closure.pureMiss < pureMissLimit {
			// The Go table's loop, entered for each call, is slower than
			// the frameless evaluators while they answer the body; once
			// they have given up on it (pureMiss), every call through
			// callDirect would try them again and then enter a frame
			// there, which the callback enters itself.
			return nil
		}
	}
	if fd.arrow {
		this = fd.extra.lexThis
	} else if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return nil
		}
		this = r.globalThis
	}
	base := r.stackTop
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return nil
	}
	// One the runtime keeps from an earlier built-in's call: a built-in
	// that calls its callback once or twice -- a replacement of one match
	// -- had allocated it each time.
	cb := r.cbFree
	if cb != nil {
		r.cbFree = cb.next
	} else {
		cb = new(regCallback)
	}
	*cb = regCallback{r: r, o: o, cl: fd.closure, c: c, this: this, base: base, saved: base, ownThis: !fd.arrow}
	r.stackTop = top
	if top > r.stackHigh {
		r.stackHigh = top
	}
	if regNativeOn {
		cb.st, cb.ctx = r.nativeCallback(c)
	}
	return cb
}

// call calls the callback with args, as callDirect would, and returns what
// it returns.
func (cb *regCallback) call(args []Value) (Value, error) {
	r, c := cb.r, cb.c
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	// The call counted, as callDirect counts it.
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return Undefined, err
		}
	}
	if c.leaf == regLeafCode && cb.cl.pureMiss < pureMissLimit {
		// A short straight body of numbers and upvalues, run as its
		// register code in the window without a frame (regLeafRun), where
		// that is faster than entering it with one: the parameters'
		// arguments where the body reads them, the rest unread.
		m := min(int(c.params), len(args))
		w := regWindow(r.stack, cb.base)
		for i, a := range args[:m] {
			w[2+i] = a
		}
		if v, ok := r.regLeafRun(cb.cl, c, cb.base, m); ok {
			return v, nil
		}
	}
	// The window: the callee, its this, the parameters' arguments, and the
	// parameters none was given for and the bindings past them undefined.
	w := regWindow(r.stack, cb.base)
	w[0], w[1] = Obj(cb.o), cb.this
	m := min(len(args), int(c.params))
	for i, a := range args[:m] {
		w[2+i] = a
	}
	for i := m; i < int(c.params+c.inits); i++ {
		w[2+i] = Undefined
	}
	// The frame record, as regEnterW writes it.
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	f := &r.cur[off]
	f.cl = cb.cl
	f.callee = cb.o
	f.base = cb.base
	f.pc = 0
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	f.newTarget = Undefined
	f.regKind = regFrameEnter
	var v Value
	var err error
	if cb.st != nil {
		v, err = r.nativeCallbackRun(cb, f, w)
	} else {
		v, err = r.runReg(f, c, 0)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	if c.fn.HasTailCall {
		return r.regTailEntryEnd(v, err, cb.base+int(c.numRegs))
	}
	return v, err
}

// callWith calls the callback with args and this, an object, where its
// calls are given a this each (a reviver's, the object holding the value).
func (cb *regCallback) callWith(this Value, args []Value) (Value, error) {
	if cb.ownThis {
		cb.this = this
	}
	return cb.call(args)
}

// end lets go of the callback's window and code entry, and gives it back
// to the runtime (cbFree), holding nothing.
func (cb *regCallback) end() {
	r := cb.r
	if cb.st != nil {
		nativeReset(cb.ctx)
		r.regLeave(cb.st)
	}
	r.stackTop = cb.saved
	*cb = regCallback{next: r.cbFree}
	r.cbFree = cb
}
