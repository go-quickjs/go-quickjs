package vm

// reg_run_call.go's after rhCallW, the frames it enters (regSameTemplate,
// regEnterW), in a file of its own after the layout lock's pad WE
// (reg_run_call_e_layoutpad.go), so that rhCallW's test of the JIT's entry,
// with the JIT built in, moves none of it (docs/optimizing.md, "The layout
// lock").

// regSameTemplate reports whether o is a closure of the template of the
// callee site e entered last, which the site's cache answers as it
// answers that callee, and makes it the cache's: its kind and this are
// the template's -- a plain function's, entered by a call as the cached
// one was, of the cached one's realm -- and a closure made anew each time
// its maker runs, `var f = function () {...}; f()`, would otherwise miss
// at every call, and be entered by Go from the miss, in a code entry of
// its own. An arrow's this is the new arrow's, as the miss would cache
// it; a bound function, a function with more of its own (extra) and
// other realms are left to the miss.
//
// The call is made again rather than entered here: the native code
// enters what its cache answers itself, and rhCallW's hit path stays as
// it is, which a branch into it from here made 37% slower in the table's
// loop (func_closure_call).
//
//go:noinline
func regSameTemplate(e *regCallCache, o *Object) bool {
	switch e.kind &^ (regCallOut | regCallLeaf) {
	case regCallEnter, regCallThis, regCallObjThis:
	default:
		return false
	}
	ecl := e.cl
	if ecl == nil || e.rc == nil {
		return false
	}
	if e.kind&(regCallOut|regCallLeaf) == regCallOut && !(regNativeBuilt && ecl.realm.agent.runNative) {
		// A frame that keeps its arguments, which the table's loop
		// enters from the miss whatever the cache holds; native code
		// enters it itself.
		return false
	}
	fd := o.fn()
	if fd == nil || fd.bound {
		return false
	}
	this := e.this
	if x := fd.extra; x != nil {
		// An arrow's this is its own, as the miss caches it.
		if !fd.arrow || x.lexThisRef != nil || x.rare != nil || e.kind&^(regCallOut|regCallLeaf) != regCallThis {
			return false
		}
		this = x.lexThis
	} else if fd.arrow {
		return false
	}
	cl := fd.closure
	if cl == nil || cl.closureCode != ecl.closureCode || cl.reg != e.rc || cl.realm != ecl.realm {
		return false
	}
	e.callee, e.cl, e.this = o, cl, this
	return true
}

// regEnterW runs a register function of the form's own layout in the
// window at base, which holds the function, its this and its argc
// arguments.
//
// The frame record is written only where a reader needs it: the function,
// the window's base and the pc, which stack traces read, and the fields a
// frame's last user may have left set, cleared where they are. Its this is
// the window's register 1, which regClosure gives the frame when it makes
// an arrow that captures it.
func (r *Runtime) regEnterW(o *Object, cl *closure, rc *regCode, base, argc int, args []Value, newTarget Value) (Value, error) {
	top := base + int(rc.numRegs)
	if top > len(r.stack) || r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return Undefined, err
		}
	}
	saved := r.stackTop
	if top > saved {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	var kept []Value
	if rc.args {
		ka := len(r.argStack)
		// Before the bindings take the window past the parameters, which
		// args may be; kept on the runtime's argument stack until the call
		// returns.
		r.argStack = append(r.argStack, args...)
		kept = r.argStack[ka:len(r.argStack):len(r.argStack)]
	}
	// Parameters no argument was given for, and the bindings past them.
	for i := base + 2 + min(argc, int(rc.params)); i < base+2+int(rc.params)+int(rc.inits); i++ {
		r.stack[i] = Undefined
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
	f := &r.cur[off]
	f.cl = cl
	f.callee = o
	f.base = base
	f.pc = 0
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil || f.withScopes != nil || f.evalVars != nil || f.native != "" || f.args != nil {
		regClearFrame(f)
	}
	// What an arrow made here captures of new.target, and what a stack
	// trace reads.
	f.newTarget = newTarget
	f.regKind = regFrameEnter
	if rc.args {
		f.args = kept
		p := int(rc.params)
		f.locals = r.stack[base+2 : base+2+p : base+2+p]
	}
	var v Value
	var err error
	if jitBuilt && cl.jitEnters {
		// Native code made from its register code, or a compile of it in
		// the background to install (jitRegEnter).
		v, err = r.jitRegEnter(f, cl, rc, base, 0)
	} else {
		v, err = r.runReg(f, rc, 0)
	}
	f.regKind = 0
	if len(f.openUpvalues) != 0 {
		regCloseUpvalues(f)
	}
	r.frameDepth--
	r.stackTop = saved
	if a := f.args; a != nil {
		// The arguments kept for it, the last on the argument stack.
		r.argStack = r.argStack[:len(r.argStack)-len(a)]
	}
	return v, err
}
