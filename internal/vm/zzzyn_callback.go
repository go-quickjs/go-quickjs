package vm

// A callback a built-in calls many times over, kept ready from one call to
// the next (docs/register-interpreter.md, "Frames and calls").

// regCallback is a function of register code a built-in calls many times
// over -- a sort's comparator -- its window reserved at the stack's top and,
// natively, its code entry's state and context kept from one call to the
// next (regCallbackOf): each call writes the window's registers and the
// frame record and runs the code, rather than going through callDirect,
// regDirect and regEnterW and making a code entry anew. The frame record
// is pushed for the call alone, so that what Go runs between two calls --
// a comparator's result converted to a number -- finds no frame of it.
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
}

// regCallbackOf is fn, called with this, as a regCallback, or nil where its
// calls are callDirect's: a function regDirect does not enter with a frame
// -- not register code of the form's own layout of this realm, a leaf
// answered without one (but one the frameless run has given up on), a body that keeps its arguments, a sloppy one of a
// primitive this -- or one of a runtime of Node's quirks or a debugger, a
// stack with no room for its window. The caller ends it (end).
func (r *Runtime) regCallbackOf(fn, this Value) *regCallback {
	if !fn.IsObject() || r.nodeQuirks || r.debug != nil {
		return nil
	}
	o := fn.Object()
	fd := o.fn()
	if fd == nil || fd.treeCall == nil || fd.closure.realm != r.Realm {
		return nil
	}
	c := fd.treeCall.reg
	if c == nil || !c.wide || c.args {
		return nil
	}
	if c.leaf != regLeafNone && (c.leaf == regLeafArg || c.leaf == regLeafConst || fd.closure.pureMiss < pureMissLimit) {
		// A leaf regDirect answers without a frame -- but one whose body
		// the frameless run has given up on, which it enters with one.
		return nil
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
	cb := &regCallback{r: r, o: o, cl: fd.closure, c: c, this: this, base: base, saved: base}
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
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return Undefined, err
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
	if st := cb.st; st != nil {
		st.f, st.c, st.cl, st.err = f, c, cb.cl, nil
		st.wb = cb.base
		st.regs = w
		v, err = r.nativeLoop(st, cb.ctx, f, c, 0)
		nativeReset(cb.ctx)
	} else {
		v, err = r.runReg(f, c, 0)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	return v, err
}

// end lets go of the callback's window and code entry.
func (cb *regCallback) end() {
	if cb.st != nil {
		cb.r.regLeave(cb.st)
		cb.st, cb.ctx = nil, nil
	}
	cb.r.stackTop = cb.saved
}
