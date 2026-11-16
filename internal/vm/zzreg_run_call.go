package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's calls and slow path, after its loop
// (zzreg_run.go): regCallW first, which the layout lock's pad W
// (zzreg_run_a_layoutpad.go) keeps where it was against runReg whatever
// the loop's size does (docs/optimizing.md, "The layout lock").

// regCallW is call_w and call_method_w: a call whose callee's frame
// starts at the caller's window, R[A], its parameters the arguments where
// they lie. It writes the call's value to R[B] itself, so that the loop
// keeps nothing of the instruction across the call.
//
// A callee the site called last, of the form's own layout and with nothing
// of its surroundings to establish but a this its cache holds, is entered
// here: its frame is the window, the frame record the few fields a stack
// trace and a closure read, and runReg is the only call between the two
// loops. What the frame needs is read from the callee's register code,
// whose facts are the function's, precomputed. Anything else is
// regCallMiss's.
func (r *Runtime) regCallW(f *frame, c *regCode, k *regKept, wb int) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	base := wb + int(w.A())
	win := regWindow(r.stack, base)
	if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
		e := &calls[w.C()]
		// The callee's tag and pointer read as words: read as a Value, the
		// tag goes through a float register to be compared.
		if regIsObject(&win[0]) && (*Object)(win[0].ref) == e.callee {
			// A leaf is answered first, before the frame's pc is written:
			// its body reads nothing of the frame and throws nothing.
			if e.kind&regCallLeaf != 0 {
				if leaf := e.rc.leaf; leaf >= regLeafArg {
					v := e.rc.leafVal
					if p := int(e.rc.leafArg); leaf == regLeafArg {
						v = Undefined
						if p < int(w.X()) {
							v = win[2+p]
						}
					}
					regWindow(r.stack, wb)[w.B()] = v
					return nil
				}
				if e.rc.leaf == regLeafCode {
					if v, ok := r.regLeafRun(e.cl, e.rc, base, int(w.X())); ok {
						regWindow(r.stack, wb)[w.B()] = v
						return nil
					}
					if e.cl.pureMiss >= pureMissLimit {
						// A body that keeps meeting what it cannot run
						// without a frame is entered with one from now on.
						e.kind &^= regCallOut | regCallLeaf
					}
					f.pc = regSrc(c, pc) + 1
					return r.regCallMiss(f, c, w, wb, pc)
				}
				// The this is given to the body, not written to the
				// window: a miss writes it (regCallSlow).
				this, ok := win[1], true
				switch e.kind &^ (regCallOut | regCallLeaf) {
				case regCallThis:
					this = e.this
				case regCallObjThis:
					ok = this.IsObject()
				}
				if ok {
					var v Value
					argc := int(w.X())
					if e.rc.leaf == regLeafPure {
						// pureCallAt checks the closure's misses.
						r.pureRefused = false
						v, ok = r.pureCallAt(e.callee.fn(), this, win[2:2+argc], 0, true)
					} else if ok = this.IsObject(); ok {
						v, ok = r.leafCall(e.cl, this.Object(), win[2:2+argc])
					}
					if ok {
						regWindow(r.stack, wb)[w.B()] = v
						return nil
					}
				}
				f.pc = regSrc(c, pc) + 1
				return r.regCallMiss(f, c, w, wb, pc)
			}
			f.pc = regSrc(c, pc) + 1
			if e.kind == regCallNative {
				return r.regNative(e.callee, w, wb)
			}
			if e.kind == regCallMarked {
				v, err := r.regMarkedCall(f, c, base, int(w.X()), pc, c.rc.Marks[pc], regAuxAt(c, pc))
				if err == nil {
					regWindow(r.stack, wb)[w.B()] = v
				}
				return err
			}
			ok := e.kind == regCallEnter
			if e.kind == regCallThis {
				win[1] = e.this
				ok = true
			} else if e.kind == regCallObjThis {
				ok = win[1].IsObject()
			}
			if ok {
				o, cl, rc := e.callee, e.cl, e.rc
				argc := int(w.X())
				top := base + int(rc.numRegs)
				if top > len(r.stack) || r.frameDepth >= r.maxFrames {
					return r.throwRangeError("maximum call stack size exceeded")
				}
				if r.interruptCounter--; r.interruptCounter <= 0 {
					if err := r.checkInterruptNow(); err != nil {
						return err
					}
				}
				saved := r.stackTop
				if top > saved {
					r.stackTop = top
					if top > r.stackHigh {
						r.stackHigh = top
					}
				}
				if argc > int(rc.keep) {
					// The arguments past the parameters, which the
					// bindings overwrite: f.arguments reads them.
					r.regKeepWindow(base, int(rc.keep), argc)
				}
				// Parameters no argument was given for, and the bindings
				// past them.
				for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
					win[2+i] = Undefined
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
				nf := &r.cur[off]
				nf.cl = cl
				nf.callee = o
				nf.base = base
				nf.pc = 0
				if len(nf.openUpvalues) != 0 {
					nf.openUpvalues = nf.openUpvalues[:0]
				}
				if nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" || nf.args != nil {
					regClearFrame(nf)
				}
				if rc.newTarget {
					nf.newTarget = Undefined
				}
				nf.regKind = regFrameCall
				v, err := r.runReg(nf, rc, 0)
				nf.regKind = 0
				if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
					regCloseUpvalues(nf)
				}
				r.frameDepth--
				r.stackTop = saved
				if err != nil {
					return r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B()))
				}
				regWindow(r.stack, wb)[w.B()] = v
				return nil
			}
		}
	}
	f.pc = regSrc(c, pc) + 1
	return r.regCallMiss(f, c, w, wb, pc)
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
	v, err := r.runReg(f, rc, 0)
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

// regDirect is callDirect's call of a function whose tree runs register
// code of the form's own layout: its window is made at the top of the stack
// and run there, without callTree's frame, runTree's recover and the copy
// regEnterFrame makes. callDirect has counted the call.
func (r *Runtime) regDirect(o *Object, fd *funcData, c *regCode, this Value, args []Value) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossDirect, c.fn, "")
	}
	// A body answered without a frame is answered here, as callDirect
	// answers one without register code and regCallW one a register call
	// makes: a built-in's callback is most often such a body.
	switch c.leaf {
	case regLeafArg:
		if p := int(c.leafArg); p < len(args) {
			return args[p], nil
		}
		return Undefined, nil
	case regLeafConst:
		return c.leafVal, nil
	case regLeafCode:
		// The parameters' arguments in a window at the top of the stack,
		// the rest of them unread; the body reads neither its callee's
		// register nor its this's (regLeafRunnable).
		m := min(int(c.params), len(args))
		if base := r.stackTop; base+2+m <= len(r.stack) {
			w := r.stack[base+2 : base+2+m : base+2+m]
			for i, a := range args[:m] {
				w[i] = a
			}
			if v, ok := r.regLeafRun(fd.closure, c, base, m); ok {
				return v, nil
			}
		}
	case regLeafPure:
		if fd.closure.pureMiss < pureMissLimit {
			if v, ok := r.pureCall(fd, this, args); ok {
				return v, nil
			}
		}
	case regLeafThis:
		if this.IsObject() {
			if v, ok := r.leafCall(fd.closure, this.Object(), args); ok {
				return v, nil
			}
		}
	}
	if fd.arrow {
		this = fd.extra.lexThis
	} else if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			// The this made an object as runFD makes it.
			v, err, _ := r.regRunFD(fd.closure, this, args, Undefined, o, fd)
			return v, err
		}
		this = r.globalThis
	}
	base := r.stackTop
	if base+int(c.numRegs) > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	m := min(int(c.params), len(args))
	w := r.stack[base : base+2+m : base+2+m]
	w[0] = Obj(o)
	w[1] = this
	for i, a := range args[:m] {
		w[2+i] = a
	}
	if r.nodeQuirks {
		// f.arguments reads the arguments as the built-in passed them.
		return r.regDirectLegacy(o, fd.closure, c, base, m, args)
	}
	return r.regEnterW(o, fd.closure, c, base, m, args, Undefined)
}

// regNative is call_w's call of the built-in o a site has cached, made as
// callDirect makes one: Math's functions, push and pop and the native
// leaves answered without a frame where their checks pass, anything else
// with the built-in's frame. Its value goes to R[B].
func (r *Runtime) regNative(o *Object, w bytecode.RInstr, wb int) error {
	base := wb + int(w.A())
	args := r.stack[base+2 : base+2+int(w.X())]
	this := Undefined
	if w.Op().MethodCall() {
		this = r.stack[base+1]
	}
	fd := o.fn()
	v, ok := Undefined, false
	if fd.mathOp != 0 {
		v, ok = mathCall(fd.mathOp, args)
	} else if op := fd.elemOp; op >= elemNativeFirst {
		if r.meter == nil && leafChecks[op](r, this, args) {
			var err error
			v, err = fd.native(r, this, args)
			ok = err == nil
		}
	} else if op != 0 {
		v, ok = r.pureElemOp(op, this, args, true)
	}
	if !ok {
		if err := r.tick(); err != nil {
			return err
		}
		if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
			return err
		}
		var err error
		v, err = fd.native(r, this, args)
		r.frameDepth--
		if r.meter != nil && err == nil {
			err = r.chargeResult(v)
		}
		if err != nil {
			return err
		}
	}
	r.stack[wb+int(w.B())] = v
	return nil
}

// regLeafRun runs cl's register code c, a regLeafCode body, in the window
// at base, where the callee, its this and argc arguments are, without a
// frame: an arithmetic operation of what is no number gives up, before any
// store, and the call is then made with one. It is the frameless evaluator
// of a pure body (pureCallAt) for the register form, whose instructions
// are fewer, dispatched by a switch of their own (regLeafOp).
func (r *Runtime) regLeafRun(cl *closure, c *regCode, base, argc int) (Value, bool) {
	if cl.pureMiss >= pureMissLimit {
		return Undefined, false
	}
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return Undefined, false
	}
	if top > r.stackHigh {
		r.stackHigh = top
	}
	regs := regWindow(r.stack, base)
	for i := argc; i < int(c.params+c.inits); i++ {
		regs[2+i] = Undefined
	}
	// The stores to upvalues, made when the body returns.
	var held [regLeafStores]regHeld
	n := 0
	for i := range c.leafOps {
		o := &c.leafOps[i]
		switch o.op {
		case rlMove:
			regs[o.a] = regs[o.b]
		case rlLoad:
			regs[o.a] = o.k
		case rlGetUp:
			regs[o.a] = cl.upvalues[o.b].get()
		case rlSetUp:
			held[n] = regHeld{cl.upvalues[o.b].slot, regs[o.a]}
			n++
		case rlAdd:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() + b.Number())
		case rlSub:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() - b.Number())
		case rlMul:
			a, b := regs[o.b], regs[o.c]
			if !a.IsNumber() || !b.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() * b.Number())
		case rlAddK:
			a := regs[o.b]
			if !a.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() + o.k.num)
		case rlRet:
			regLeafStore(held[:n])
			return regs[o.a], true
		case rlRetUndef:
			regLeafStore(held[:n])
			return Undefined, true
		}
	}
	return Undefined, true
}

// regCloseUpvalues closes the upvalues a frame's closures left open on its
// registers.
//
//go:noinline
func regCloseUpvalues(f *frame) {
	for _, u := range f.openUpvalues {
		u.close()
	}
}

// regSlow is the slow path of the instruction at pc, whatever the loop's
// fast path for it did not answer: it sets the frame's pc to the stack
// form's instruction, does what the stack interpreter does, and stores the
// result in its register. A comparison's jump is left to the loop, which is
// given the comparison's result.
//
// It reads the instruction from the code rather than being given it, so that
// the loop keeps nothing of the instruction across the call (see runReg).
func (r *Runtime) regSlow(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) (Value, error) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	cl := f.cl
	wb := f.base - int(c.locals)
	var v Value
	var err error
	switch w.Op() {
	case bytecode.RCold, bytecode.RGetPrivate, bytecode.RSetPrivate:
		return Undefined, r.regColdSub(f, c, k, regs)
	case bytecode.RGetGlobal:
		v, err = r.regGetGlobal(f, cl, c, pc, w)
	case bytecode.RSetGlobal:
		return Undefined, r.regSetGlobal(f, cl, c, pc, w, regs[w.A()])
	case bytecode.RGetProp:
		v, err = r.regGetProp(cl, c, w, regs[w.B()])
	case bytecode.RGetMethod:
		v, err = r.regGetMethod(cl, c, w, regs[w.B()])
	case bytecode.RGetMethodW:
		o := regs[w.B()]
		v, err = r.regGetMethod(cl, c, w, o)
		(*[2]Value)(unsafe.Pointer(&regs[w.A()]))[1] = o
	case bytecode.RSetProp:
		return Undefined, r.regSetProp(cl, c, w, regs[w.A()], regs[w.B()])
	case bytecode.RGetIndex:
		// A key string the runtime handed out -- a for-in's -- read as
		// the tree's element read reads it (stringKeyRead): a[k] in for
		// (k in a) an element by the key's index.
		if o, key := regs[w.B()], regs[w.C()]; o.IsObject() && key.IsString() {
			if x, ok := r.stringKeyRead(o.Object(), key.String()); ok {
				v = x
				break
			}
		}
		v, err = r.getIndexed(regs[w.B()], regs[w.C()])
	case bytecode.RSetIndex:
		return Undefined, r.setIndexed(regs[w.A()], regs[w.B()], regs[w.C()], cl.fn.Strict)
	case bytecode.RAdd:
		v, err = r.add(regs[w.B()], regs[w.C()])
	case bytecode.RSub:
		v, err = r.arith(bytecode.OpSub, regs[w.B()], regs[w.C()])
	case bytecode.RMul:
		v, err = r.arith(bytecode.OpMul, regs[w.B()], regs[w.C()])
	case bytecode.RBin:
		v, err = r.regBinary(bytecode.Op(w.X()), regs[w.B()], regs[w.C()])
	case bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
		v, err = r.regBinImm(regStackOp[w.Op()], regs[w.B()], w.SC())
	case bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
		v, err = r.regBinary(regStackOp[w.Op()], regs[w.B()], regs[w.C()])
	case bytecode.RInc:
		v, err = r.regStep(regs[w.B()], 1)
	case bytecode.RDec:
		v, err = r.regStep(regs[w.B()], -1)
	case bytecode.RUnary:
		v, err = r.regUnary(bytecode.Op(w.X()), regs[w.B()])
	case bytecode.RDefineField:
		return Undefined, r.regDefineField(cl, regName(cl, c, pc, w.C()), regs[w.A()], regs[w.B()])
	case bytecode.RNewObject:
		v = Obj(r.regNewObject(cl, w))
	case bytecode.RAddIndex:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], regs[w.C()], false)
	case bytecode.RAddIndexK:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], Int32(int32(w.C())), regs[w.B()], w.X() != 0)
	case bytecode.RAddIndexI:
		return Undefined, r.regAddIndex(f, c.rc.Aux[pc], regs[w.A()], regs[w.B()], Int32(w.SC()), w.X() != 0)
	case bytecode.RNewArray:
		b := wb + int(w.B())
		v = Obj(r.newArrayFrom(r.stack[b : b+int(w.C())]))
	case bytecode.RGetIndexPre:
		// x[++i]: the step, then the read, each as the stack form makes it.
		// Assigned, not declared: the switch's err is what the call returns.
		var n Value
		if n, err = r.regStep(regs[w.C()], float64(int8(w.X()))); err != nil {
			return Undefined, err
		}
		regs[w.C()] = n
		v, err = r.getIndexed(regs[w.B()], n)
	case bytecode.RGetIndexPost:
		// x[i++]: the step, its old value the key, then the read, as the
		// stack form makes them.
		var old, n Value
		if old, err = r.toNumeric(regs[w.C()]); err != nil {
			return Undefined, err
		}
		if n, err = r.regStep(old, float64(int8(w.X()))); err != nil {
			return Undefined, err
		}
		o := regs[w.B()]
		regs[w.C()] = n
		v, err = r.getIndexed(o, old)
	case bytecode.RGetIndexI:
		v, err = r.getIndexed(regs[w.B()], Int32(int32(w.C())))
	case bytecode.RSetIndexI:
		return Undefined, r.setIndexed(regs[w.A()], Int32(int32(w.B())), regs[w.C()], cl.fn.Strict)
	case bytecode.RNewW:
		if regReportOn {
			regCrossOf(regCrossNew, regs[w.A()])
		}
		b := wb + int(w.A())
		if v, err = r.regConstruct(regs[w.A()], b, int(w.X())); err == nil {
			regs[w.B()] = v
		}
		return v, err
	case bytecode.RClosureW:
		v = Obj(r.regClosure(f, c, w, wb))
	case bytecode.RPostInc, bytecode.RPostDec:
		old, err := r.toNumeric(regs[w.B()])
		if err != nil {
			return Undefined, err
		}
		delta := 1.0
		if w.Op() == bytecode.RPostDec {
			delta = -1
		}
		next, err := r.regStep(old, delta)
		if err != nil {
			return Undefined, err
		}
		regs[w.A()] = old
		regs[w.B()] = next
		return old, nil
	case bytecode.RGetLength:
		if o := regs[w.B()]; o.IsString() {
			v = Int(o.String().Len())
		} else {
			v, err = r.getValueProp(o, atomLength)
		}
	case bytecode.RInstanceOf:
		var ok bool
		ok, err = r.instanceOfAt(&cl.ic[c.rc.Aux[pc]], regs[w.B()], regs[w.C()])
		v = Bool(ok)
	case bytecode.RIn:
		v, err = r.regIn(regs[w.B()], regs[w.C()])
	case bytecode.RDelete:
		v, err = r.regDelete(cl, regs[w.B()], regs[w.C()])
	case bytecode.RArguments:
		v = Obj(r.newArgumentsObject(f))
	case bytecode.RArgsLen:
		if a := regs[w.B()]; a.IsObject() {
			v, err = r.getValueProp(a, atomLength)
		} else {
			v = Int(len(f.args))
		}
	case bytecode.RConcat:
		// The registers are the parts' own, which concatParts may convert
		// in place: nothing reads them after.
		b := int(w.B())
		v, err = concatParts(r, regs[b:b+int(w.C())])
	case bytecode.RKeyOfBase:
		if base := regs[w.B()]; base.IsNullish() {
			return Undefined, r.throwTypeError("cannot read property of %s", r.describe(base))
		}
		var k Atom
		if k, err = r.toPropertyKey(regs[w.C()]); err == nil {
			// An object's Symbol.toPrimitive may give a symbol: the key
			// keeps whichever kind it turned out to be.
			v = r.keyToValue(k)
		}
	case bytecode.RStrPart:
		v, err = r.add(Str(emptyString), regs[w.A()])
	case bytecode.RArgsIndex:
		key := regs[w.C()]
		if !regs[w.B()].IsObject() {
			if i := uint32(key.num); key.IsNumber() && float64(i) == key.num && uint(i) < uint(len(f.args)) &&
				!(f.cl.fn.MappedArguments && uint(i) < uint(f.cl.fn.ParamCount)) {
				v = f.args[i]
				break
			}
			regs[w.B()] = Obj(r.newArgumentsObject(f))
		}
		v, err = r.getIndexed(regs[w.B()], key)
	case bytecode.RApplyArgs:
		b := int(w.B())
		v, err = r.regApplyArgs(f, regs[b], regs[b+1], regs[b+2], &regs[w.C()])
	case bytecode.RAddProp, bytecode.RAddPropI:
		b := Int32(w.SB())
		if w.Op() == bytecode.RAddProp {
			b = regs[w.B()]
		}
		return Undefined, r.regAddProp(cl, c, w, regs[w.A()], b)
	case bytecode.RNewArrayK:
		// The tree's shared literal (ztree_a0shared.go): the site keeps the
		// array of the constants, and each array shares its elements until
		// one is written.
		s := &cl.ic[w.C()]
		t := s.p1
		if t == nil {
			lit := c.rc.Literals[c.rc.Aux[pc]]
			vals := make([]Value, len(lit))
			for i, e := range lit {
				switch e.Kind {
				case bytecode.LitConst:
					vals[i] = cl.consts[e.K]
				case bytecode.LitTrue:
					vals[i] = True
				case bytecode.LitFalse:
					vals[i] = False
				case bytecode.LitNull:
					vals[i] = Null
				default:
					vals[i] = Undefined
				}
			}
			t = sharedMade(r, vals)
			s.p1 = t
		}
		v = Obj(sharedShare(r, s, t))
	case bytecode.RAddI:
		v, err = r.regBinImm(bytecode.OpAdd, regs[w.B()], w.SC())
	case bytecode.RSubI:
		v, err = r.regBinImm(bytecode.OpSub, regs[w.B()], w.SC())
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
		bytecode.RJEq, bytecode.RJNe, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJNull, bytecode.RJNotNull,
		bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
		k := regJumpCmp[w.Op()]
		b := Int32(w.SB())
		if k.reg {
			b = regs[w.B()]
		} else if w.Op() == bytecode.RJNull || w.Op() == bytecode.RJNotNull {
			b = Null
		} else if k.upval {
			b = cl.upvalues[w.B()].get()
		}
		res, err := r.regCompare(k.op, regs[w.A()], b)
		return Bool(res != k.not), err
	case bytecode.RJmpF, bytecode.RJmpT:
		// A truthiness the loop's regTruth does not answer.
		return Bool(regs[w.A()].Truthy() == (w.Op() == bytecode.RJmpT)), nil
	case bytecode.RJTypeof:
		t := typeofKind[struct{}](regs[w.A()])
		return Bool((t == int(w.X()&^bytecode.RCmpTrue)) == (w.X()&bytecode.RCmpTrue != 0)), nil
	case bytecode.RJSEq, bytecode.RJSNe:
		// A === the loop's regStrictEq does not answer: strings, BigInts.
		return Bool(regs[w.A()].StrictEquals(regs[w.B()]) == (w.Op() == bytecode.RJSEq)), nil
	default:
		panic("register code: no slow path for " + w.Op().String())
	}
	if err == nil {
		regs[w.A()] = v
	}
	return v, err
}
