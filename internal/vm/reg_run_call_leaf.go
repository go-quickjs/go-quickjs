package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// reg_run_call_enter.go's after regEnterW, from a direct call's frame on
// (regDirect, regNative, regLeafRun), in a file of its own after the layout
// lock's pad LR (reg_run_call_l_layoutpad.go), so that regEnterW's test of
// the JIT's entry, with the JIT built in, moves none of it
// (docs/optimizing.md, "The layout lock").

// regDirect is callDirect's call of a function regPlan has given its
// register code of the form's own layout: its window is made at the top of
// the stack and run there. callDirect has counted the call.
func (r *Runtime) regDirect(o *Object, fd *funcData, c *regCode, this Value, args []Value) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossDirect, c.fn, "")
	}
	// A body answered without a frame is answered here, as callDirect
	// answers one without register code and rhCallW one a register call
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
	if c.fn.HasTailCall {
		return r.regDirectTail(o, fd.closure, c, base, m, args)
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
	if cl.pureMiss >= pureMissLimit || r.interruptCounter <= 1 {
		// An interrupt due is a frame's to count, which checks it: a run
		// with no frame counts none, and the call handler, which leaves a
		// call to Go while one is due, had left every call to it from
		// then on.
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
		case rlMulK:
			a := regs[o.b]
			if !a.IsNumber() {
				cl.pureMiss++
				return Undefined, false
			}
			regs[o.a] = regNum(a.Number() * o.k.num)
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
