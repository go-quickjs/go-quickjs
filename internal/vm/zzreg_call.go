package vm

import (
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's calls and slow paths: what its loop does out
// of line, so that the loop stays small enough for Go to inline into it what
// it inlines (see zzreg_run.go).

// regCall calls callee with this and args from the register interpreter. A
// function of this realm that has register code, and that a call runs with
// nothing to establish but its frame, is run here directly: its frame made
// as callTree makes one, its registers run by runReg, with no tree, no
// recover and no other Go call between the two loops. Anything else is
// called as the stack interpreter calls it.
func (r *Runtime) regCall(callee, this Value, args []Value) (Value, error) {
	if !callee.IsObject() {
		return r.callDirect(callee, this, args)
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound {
		return r.callDirect(callee, this, args)
	}
	cl := fd.closure
	c := cl.reg
	if c == nil {
		c = r.regOf(cl)
	}
	if c == nil || c.wide || cl.realm != r.Realm {
		return r.callDirect(callee, this, args)
	}
	if fd.extra != nil {
		// An arrow whose surroundings are its this alone; anything else a
		// function carries of its surroundings is runFD's to establish.
		x := fd.extra
		if !fd.arrow || x.lexThisRef != nil || x.rare != nil {
			return r.callDirect(callee, this, args)
		}
		this = x.lexThis
	}
	if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.callDirect(callee, this, args)
		}
		this = r.globalThis
	}
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = top
	if top > r.stackHigh {
		r.stackHigh = top
	}
	lc := int(c.locals)
	locals := r.stack[base : base+lc : base+lc]
	m := min(int(c.params), len(locals), len(args))
	//lint:ignore S1001 a loop, not copy: see callTree
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
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
	f.locals = locals
	f.base = base + lc
	f.pc = 0
	f.this = this
	f.newTarget = Undefined
	f.callee = o
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil {
		f.thisRef = nil
	}
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
	}
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if f.native != "" {
		f.native = ""
	}
	if f.savedSP != 0 {
		f.savedSP = 0
	}
	v, err := r.runReg(f, c)
	r.popFrameOf(f, base)
	return v, err
}

// regCallOp is call and call_method, the instruction before k's pc.
func (r *Runtime) regCallOp(f *frame, c *regCode, k *regKept, wb int) (Value, error) {
	pc := k.pc - 1
	f.pc = c.src[pc] + 1
	w := regAt(c.code, pc)
	b := wb + int(w.A())
	if w.Op() == bytecode.RCall {
		return r.regCall(r.stack[b], Undefined, r.stack[b+1:b+1+int(w.X())])
	}
	return r.regCall(r.stack[b+1], r.stack[b], r.stack[b+2:b+2+int(w.X())])
}

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
				return r.regCallMiss(f, c, w, wb)
			}
			f.pc = regSrc(c, pc) + 1
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
				v, err := r.runReg(nf, rc)
				if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
					regCloseUpvalues(nf)
				}
				r.frameDepth--
				r.stackTop = saved
				if err != nil {
					return err
				}
				regWindow(r.stack, wb)[w.B()] = v
				return nil
			}
		}
	}
	f.pc = regSrc(c, pc) + 1
	return r.regCallMiss(f, c, w, wb)
}

// regSrc is the stack form's instruction of register instruction pc, which
// the verifier proves is in the code.
func regSrc(c *regCode, pc int) uint32 {
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(c.src)), uintptr(pc)*4))
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

// regCallMiss is a call regCallW's cache does not answer: a site's first
// call or a new callee, a body answered without a frame, a frame that keeps
// its arguments, a this to coerce, or a callee of no register code.
//
//go:noinline
func (r *Runtime) regCallMiss(f *frame, c *regCode, w bytecode.RInstr, wb int) error {
	v, err := r.regCallSlow(f, c, w, wb+int(w.A()))
	if err != nil {
		return err
	}
	r.stack[wb+int(w.B())] = v
	return nil
}

func (r *Runtime) regCallSlow(f *frame, c *regCode, w bytecode.RInstr, base int) (Value, error) {
	argc := int(w.X())
	method := w.Op() == bytecode.RCallMW
	callee := r.stack[base]
	if callee.IsObject() {
		o := callee.Object()
		if f.cl.regCalls == nil && c.rc.CallSites > 0 {
			f.cl.regCalls = make([]regCallCache, c.rc.CallSites)
		}
		var e *regCallCache
		if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
			e = &calls[w.C()]
		}
		var (
			cl   *closure
			rc   *regCode
			kind uint8
			this Value
		)
		if e != nil && e.callee == o && e.kind != 0 {
			// A callee the site knows, whose call is not regCallW's alone.
			cl, rc, kind, this = e.cl, e.rc, e.kind&^(regCallOut|regCallLeaf), e.this
		} else if fd := o.fn(); fd != nil && fd.closure != nil && !fd.bound {
			cl = fd.closure
			if rc = cl.reg; rc == nil {
				rc = r.regOf(cl)
			}
			if rc != nil && rc.wide && cl.realm == r.Realm {
				if x := fd.extra; x == nil {
					switch {
					case !method:
						kind, this = regCallThis, Undefined
						if rc.coerceThis {
							this = r.globalThis
						}
					case rc.coerceThis:
						kind = regCallObjThis
					default:
						kind = regCallEnter
					}
				} else if fd.arrow && x.lexThisRef == nil && x.rare == nil {
					kind, this = regCallThis, x.lexThis
				}
			}
			if kind != 0 && e != nil {
				e.callee, e.cl, e.rc, e.this, e.kind = o, cl, rc, this, kind
				if rc.leaf != regLeafNone {
					e.kind |= regCallOut | regCallLeaf
				} else if rc.args {
					e.kind |= regCallOut
				}
			}
		}
		switch kind {
		case regCallThis:
			r.stack[base+1] = this
		case regCallObjThis:
			if !r.stack[base+1].IsObject() {
				// A this to coerce: the generic call's.
				kind = 0
			}
		}
		if kind != 0 {
			// A body that only reads or writes a property of its this, or
			// computes a value calling nothing, is answered without a frame,
			// as callDirect answers it (leafCall, pureCall): its frame would
			// cost more than its body. A pure body that calls is run as
			// register code, faster than the frameless evaluator's calls.
			if leaf := rc.leaf; leaf != regLeafNone {
				if leaf == regLeafPure {
					if cl.pureMiss < pureMissLimit {
						if v, ok := r.pureCall(o.fn(), r.stack[base+1], r.stack[base+2:base+2+argc]); ok {
							return v, nil
						}
					}
					if cl.pureMiss >= pureMissLimit && e != nil && e.callee == o && !rc.args {
						// A body the frameless evaluator has given up on,
						// for good: its calls are entered as frames.
						e.kind &^= regCallOut | regCallLeaf
					}
				} else if this := r.stack[base+1]; this.IsObject() {
					if v, ok := r.leafCall(cl, this.Object(), r.stack[base+2:base+2+argc]); ok {
						return v, nil
					}
				}
			}
			return r.regEnterW(o, cl, rc, base, argc, r.stack[base+2:base+2+argc], Undefined)
		}
	}
	if regReportOn {
		regCrossOf(regCrossOut, callee)
	}
	this := Undefined
	if method {
		this = r.stack[base+1]
	}
	return r.regCall(callee, this, r.stack[base+2:base+2+argc])
}

// regCallCache is a call site's cache: the function it called last, its
// closure and register code, how it is entered, and the this a call that
// is not a method's gives it.
type regCallCache struct {
	callee *Object
	cl     *closure
	rc     *regCode
	this   Value
	kind   uint8
}

// The kinds of regCallCache: what entering the callee takes of its this.
// regCallOut marks one regCallW leaves to regCallMiss: a body answered
// without a frame, or a frame that keeps its arguments.
const (
	// regCallEnter is a method's call of a function whose this is the
	// method's receiver as it is.
	regCallEnter = 1 + iota
	// regCallThis writes the cache's this: a plain call's undefined or
	// global object, or an arrow's this.
	regCallThis
	// regCallObjThis is a method's call of a function that coerces its
	// this, entered as it is for an object receiver.
	regCallObjThis

	regCallOut = 8
	// regCallLeaf marks a callee whose body regCallW may answer without a
	// frame (leafCall, pureCall).
	regCallLeaf = 16
)

// regIsObject reports whether *v is an object, its tag read as a word.
func regIsObject(v *Value) bool {
	return *(*uint64)(unsafe.Pointer(v)) == objectBits
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
	if rc.newTarget {
		// What an arrow made here captures of new.target.
		f.newTarget = newTarget
	}
	if rc.args {
		f.args = kept
		p := int(rc.params)
		f.locals = r.stack[base+2 : base+2+p : base+2+p]
	}
	v, err := r.runReg(f, rc)
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

// regClearFrame clears what a frame's last user left that a register
// frame has none of.
//
//go:noinline
func regClearFrame(f *frame) {
	f.thisRef = nil
	f.withScopes = nil
	f.evalVars = nil
	f.native = ""
	f.args = nil
}

// regClosure is closure_w: a closure of the nested function constant BC,
// each upvalue it takes from this frame open on the register the code's
// capture table names for it.
func (r *Runtime) regClosure(f *frame, c *regCode, w bytecode.RInstr, wb int) *Object {
	k := w.BC()
	tmpl := f.cl.consts[k]
	fn := f.cl.fn
	caps := c.rc.Caps[k]
	// The cells newClosureIn reads, kept in the template's code for the next
	// closure; the upvalues this closure is the first to open, one
	// allocation, as newClosureIn's room.
	cells := f.cl.regCells
	if len(cells) < int(c.ncap) {
		cells = make([]*upvalue, c.ncap)
		f.cl.regCells = cells
	}
	var room []upvalue
	ups := tmpl.template().fn.Upvalues
	for i, u := range ups {
		if !u.FromParent {
			continue
		}
		slot := &r.stack[wb+int(caps[i])]
		var up *upvalue
		for _, o := range f.openUpvalues {
			if o.slot == slot {
				up = o
				break
			}
		}
		if up == nil {
			if len(room) == 0 {
				room = make([]upvalue, len(ups)-i)
			}
			up, room = &room[0], room[1:]
			up.slot = slot
			f.openUpvalues = append(f.openUpvalues, up)
		}
		cells[cellOfLocal(fn, int(u.Index))] = up
	}
	if c.wide {
		// What an arrow captures of this is the frame's register 1, which
		// the frame record does not keep (regEnterW).
		f.this = r.stack[wb+1]
	}
	o := r.newClosureIn(f, tmpl, cells)
	clear(cells)
	if fd := o.fn(); fd != nil {
		if atomic.LoadPointer(&fd.closure.fn.VMCode) == nil && fd.closure.fn.Reg != nil && regTier.Load() {
			firstTree(fd.closure.fn)
		}
		if atomic.LoadPointer(&fd.closure.fn.VMCode) != nil {
			planTreeCall(fd)
			regPlan(fd)
		}
	}
	return o
}

// regDirect is callDirect's call of a function whose tree runs register
// code of the form's own layout: its window is made at the top of the stack
// and run there, without callTree's frame, runTree's recover and the copy
// regEnterFrame makes. callDirect has counted the call.
func (r *Runtime) regDirect(o *Object, fd *funcData, c *regCode, this Value, args []Value) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossDirect, c.fn, "")
	}
	if fd.arrow {
		this = fd.extra.lexThis
	} else if c.coerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.callTree(o, fd, this, args, false)
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
	return r.regEnterW(o, fd.closure, c, base, m, args, Undefined)
}

// regPlan gives a closure of a template with register code of the form's
// own layout the tree that runs it as its tree's call, where nothing of its
// surroundings but an arrow's this is to establish, so that callDirect --
// a built-in's callback, a tree's call -- enters the register code at once.
// An arrow's this is the one it captured, which regEnterFrame and
// regDirect give it.
func regPlan(fd *funcData) {
	if fd.treeCall != nil || fd.bound || fd.native != nil || fd.closure == nil {
		return
	}
	t := (*tree)(atomic.LoadPointer(&fd.closure.fn.VMCode))
	if t == nil || t.reg == nil || !t.reg.wide {
		return
	}
	if x := fd.extra; x != nil && (!fd.arrow || x.lexThisRef != nil || x.rare != nil) {
		return
	}
	fd.treeCall = t
}

// regOf is the register code of cl's function, built if the function has
// not been run yet, and kept in its template for regCall's next call of it;
// or nil.
func (r *Runtime) regOf(cl *closure) *regCode {
	if !regTier.Load() {
		return nil
	}
	t := (*tree)(atomic.LoadPointer(&cl.fn.VMCode))
	if t == nil {
		t = firstTree(cl.fn)
	}
	cl.reg = t.reg
	return t.reg
}

// regTctx is f's tree context, made enough of for the tree tier's slow
// paths the register interpreter shares.
func regTctx(r *Runtime, f *frame, cl *closure) *tctx {
	c := &f.tc
	c.r, c.f, c.cl = r, f, cl
	c.closureCode = cl.closureCode
	return c
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
	case bytecode.RGetGlobal:
		v, err = r.regGetGlobal(f, cl, w)
	case bytecode.RSetGlobal:
		return Undefined, r.regSetGlobal(f, cl, w, regs[w.A()])
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
	case bytecode.RBinI:
		v, err = r.regBinImm(bytecode.Op(w.X()), regs[w.B()], w.SC())
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
		return Undefined, r.regDefineField(cl, w, regs[w.A()], regs[w.B()])
	case bytecode.RNewObject:
		v = Obj(r.regNewObject(cl, w))
	case bytecode.RNewArray:
		b := wb + int(w.B())
		v = Obj(r.newArrayFrom(r.stack[b : b+int(w.C())]))
	case bytecode.RClosure:
		v = Obj(r.makeClosure(f, cl.consts[w.BC()]))
	case bytecode.RNew:
		b := wb + int(w.A())
		if v, err = r.construct(regs[w.A()], r.stack[b+1:b+1+int(w.X())]); err == nil {
			regs[w.B()] = v
		}
		return v, err
	case bytecode.RGetIndexPre:
		// x[++i]: the step, then the read, each as the stack form makes it.
		// Assigned, not declared: the switch's err is what the call returns.
		var n Value
		if n, err = r.regStep(regs[w.C()], float64(int8(w.X()))); err != nil {
			return Undefined, err
		}
		regs[w.C()] = n
		v, err = r.getIndexed(regs[w.B()], n)
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
		v, err = r.regApplyArgs(f, regs[b], regs[b+1], regs[b+2])
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
	case bytecode.RCloseUpvals:
		if len(f.openUpvalues) != 0 {
			r.closeUpvaluesFrom(f, int(w.A()))
		}
		return Undefined, nil
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
	case bytecode.RJSEq, bytecode.RJSNe:
		// A === the loop's regStrictEq does not answer: strings, BigInts.
		return Bool(regs[w.A()].StrictEquals(regs[w.B()]) == (w.Op() == bytecode.RJSEq)), nil
	case bytecode.RJmpCmp, bytecode.RJmpCmpI:
		b := Int32(w.SB())
		if w.Op() == bytecode.RJmpCmp {
			b = regs[w.B()]
		}
		res, err := r.regCompare(bytecode.Op(w.X()&^bytecode.RCmpTrue), regs[w.A()], b)
		return Bool(res == (w.X()&bytecode.RCmpTrue != 0)), err
	default:
		panic("register code: no slow path for " + w.Op().String())
	}
	if err == nil {
		regs[w.A()] = v
	}
	return v, err
}

// regStackOp is the stack form's operator of each specialised operation,
// whose slow path is the stack form's.
var regStackOp = [128]bytecode.Op{
	bytecode.RAndI: bytecode.OpBitAnd, bytecode.ROrI: bytecode.OpBitOr, bytecode.RXorI: bytecode.OpBitXor,
	bytecode.RShlI: bytecode.OpShl, bytecode.RShrI: bytecode.OpShr, bytecode.RUShrI: bytecode.OpUShr,
	bytecode.RBitAnd: bytecode.OpBitAnd, bytecode.RBitOr: bytecode.OpBitOr, bytecode.RBitXor: bytecode.OpBitXor,
	bytecode.RShl: bytecode.OpShl, bytecode.RShr: bytecode.OpShr, bytecode.RUShr: bytecode.OpUShr,
}

// regJumpCmp is each compare-and-branch operation's comparison: its stack
// form operator, whether it jumps when the comparison does not hold, and
// whether its right operand is a register.
var regJumpCmp = [128]struct {
	op              bytecode.Op
	not, reg, upval bool
}{
	bytecode.RJLtU: {bytecode.OpLt, false, false, true}, bytecode.RJNLtU: {bytecode.OpLt, true, false, true},
	bytecode.RJLeU: {bytecode.OpLe, false, false, true}, bytecode.RJNLeU: {bytecode.OpLe, true, false, true},
	bytecode.RJLt: {bytecode.OpLt, false, true, false}, bytecode.RJNLt: {bytecode.OpLt, true, true, false},
	bytecode.RJLe: {bytecode.OpLe, false, true, false}, bytecode.RJNLe: {bytecode.OpLe, true, true, false},
	bytecode.RJGt: {bytecode.OpGt, false, true, false}, bytecode.RJNGt: {bytecode.OpGt, true, true, false},
	bytecode.RJGe: {bytecode.OpGe, false, true, false}, bytecode.RJNGe: {bytecode.OpGe, true, true, false},
	bytecode.RJLtI: {bytecode.OpLt, false, false, false}, bytecode.RJNLtI: {bytecode.OpLt, true, false, false},
	bytecode.RJLeI: {bytecode.OpLe, false, false, false}, bytecode.RJNLeI: {bytecode.OpLe, true, false, false},
	bytecode.RJGtI: {bytecode.OpGt, false, false, false}, bytecode.RJNGtI: {bytecode.OpGt, true, false, false},
	bytecode.RJGeI: {bytecode.OpGe, false, false, false}, bytecode.RJNGeI: {bytecode.OpGe, true, false, false},
	bytecode.RJEq: {bytecode.OpEq, false, true, false}, bytecode.RJNe: {bytecode.OpEq, true, true, false},
	bytecode.RJEqI: {bytecode.OpEq, false, false, false}, bytecode.RJNeI: {bytecode.OpEq, true, false, false},
	bytecode.RJNull: {bytecode.OpEq, false, false, false}, bytecode.RJNotNull: {bytecode.OpEq, true, false, false},
}

// regConstruct is new_w: new callee with the argc arguments of the window
// at base. A constructor of this realm with register code of the form's own
// layout, a base function's, runs in the window as a register frame, its
// object made as constructWithTarget makes it -- its prototype, its room,
// the root its objects start from -- and the room it came to need noted.
// Anything else is constructed as construct constructs it.
func (r *Runtime) regConstruct(callee Value, base, argc int) (Value, error) {
	args := r.stack[base+2 : base+2+argc : base+2+argc]
	if !callee.IsObject() {
		return r.construct(callee, args)
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || fd.extra != nil || fd.ctorKind != ctorBase ||
		fd.closure.realm != r.Realm || proxyOf(o) != nil {
		return r.construct(callee, args)
	}
	cl := fd.closure
	if cl.fn.Leaf == bytecode.LeafForward {
		// What hands its arguments on, the construction answers without a
		// frame.
		return r.construct(callee, args)
	}
	rc := cl.reg
	if rc == nil {
		rc = r.regOf(cl)
	}
	if rc == nil || !rc.wide {
		return r.construct(callee, args)
	}
	var protoVal Value
	var err error
	if p := o.getOwnVisible(atomPrototype); p.ok() && p.flags()&propAccessor == 0 {
		protoVal = p.value()
	} else if protoVal, err = r.getProp(o, atomPrototype, Obj(o)); err != nil {
		return Undefined, err
	}
	var proto *Object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	} else if proto, err = r.protoForNewTarget(o, r.proto.object); err != nil {
		return Undefined, err
	}
	root := r.shapes.ctorRoot(fd, proto)
	obj := newOrdinary[struct{}](fd, root, proto, root.roomFor(int(cl.fn.ThisProps)))
	this := Obj(obj)
	if err := r.initInstanceElements(o, this); err != nil {
		return Undefined, err
	}
	if cl.fn.Leaf != bytecode.LeafNone && cl.fn.Leaf != bytecode.LeafPure {
		// A body that only stores its parameters in this, without a frame
		// where leafCall can.
		if _, done := r.leafCall(cl, obj, args); done {
			root.noteBuilt(int(obj.valn))
			return this, nil
		}
	}
	r.stack[base+1] = this
	res, err := r.regEnterW(o, cl, rc, base, argc, args, Obj(o))
	if err != nil {
		return Undefined, err
	}
	root.noteBuilt(int(obj.valn))
	if res.IsObject() {
		return res, nil
	}
	return this, nil
}

// regAddProp is add_prop's slow path: the property read as get_prop
// reads it, b added, and the sum stored as set_prop stores it, through the
// site's entry, which the store fills.
func (r *Runtime) regAddProp(cl *closure, c *regCode, w bytecode.RInstr, obj, b Value) error {
	name := cl.names[c.siteName[w.C()]]
	cur, err := r.getValueProp(obj, name)
	if err != nil {
		return err
	}
	var sum Value
	if w.X() != 0 {
		sum, err = r.regStep(cur, b.Number())
	} else {
		sum, err = r.add(cur, b)
	}
	if err != nil {
		return err
	}
	if obj.IsObject() {
		return r.setPropCached(&cl.ic[w.C()], obj.Object(), name, sum, cl.fn.Strict)
	}
	return r.setValueProp(obj, name, sum, cl.fn.Strict)
}

// regThrow is throw: the value thrown, from the instruction at pc.
//
// next is the pc after the throw's, which the loop holds: the throw's own,
// next-1 there, is the value before the loop's increment, which Go would keep
// for it at every dispatch.
func (r *Runtime) regThrow(f *frame, c *regCode, next int, regs *[1 << 16]Value) error {
	pc := next - 1
	f.pc = c.src[pc] + 1
	return r.throw(regs[regAt(c.code, pc).A()])
}

// regIn is key in obj, as the stack interpreter's in.
func (r *Runtime) regIn(key, obj Value) (Value, error) {
	if !obj.IsObject() {
		return Undefined, r.throwTypeError("the right operand of \"in\" must be an object")
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	k, known := r.keyFor(obj.Object(), n)
	if !known {
		return False, nil
	}
	has, err := r.hasPropErr(obj.Object(), k)
	return Bool(has), err
}

// regDelete is delete obj[key], as the stack interpreter's delete_prop.
func (r *Runtime) regDelete(cl *closure, obj, key Value) (Value, error) {
	if obj.IsNullish() {
		return Undefined, r.throwTypeError("cannot delete a property of %s", r.describe(obj))
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	if !obj.IsObject() {
		if obj, err = toObjectValue(r, obj); err != nil {
			return Undefined, err
		}
	}
	k, has := r.keyFor(obj.Object(), n)
	if !has {
		return True, nil
	}
	ok, err := r.deleteProp(obj.Object(), k, cl.fn.Strict)
	return Bool(ok), err
}

// regThrowDeadZone is the ReferenceError of a read of a binding in its dead
// zone, at the check at pc.
// next is the pc after the read's, as regThrow's is.
func (r *Runtime) regThrowDeadZone(f *frame, c *regCode, next int) error {
	pc := next - 1
	f.pc = c.src[pc] + 1
	return r.throwReferenceError("cannot access %q before initialization", f.cl.fn.Locals[regAt(c.code, pc).B()].Name)
}

// regGetGlobal is get_global's slow path: what the stack interpreter does,
// then the layout of the global object the site found the slot in noted,
// for the loop's check.
func (r *Runtime) regGetGlobal(f *frame, cl *closure, w bytecode.RInstr) (Value, error) {
	in := bytecode.Instr{Op: bytecode.OpGetGlobal, A: uint32(w.B()), B: uint32(w.C())}
	v, err := r.getGlobalAt(regTctx(r, f, cl), in, int(f.pc-1))
	if err == nil {
		r.regNoteGlobal(cl, w.C(), cl.names[w.B()], false)
	}
	return v, err
}

// regSetGlobal is set_global's, or set_global_strict's, slow path.
func (r *Runtime) regSetGlobal(f *frame, cl *closure, w bytecode.RInstr, v Value) error {
	in := bytecode.Instr{Op: bytecode.OpSetGlobal, A: uint32(w.B()), B: uint32(w.C())}
	var err error
	if w.X() != 0 {
		in.Op = bytecode.OpSetGlobalStrict
		err = r.setGlobalStrict(f, cl, in, v)
	} else {
		err = r.setGlobalIn(f, cl, in, v)
	}
	if err == nil {
		r.regNoteGlobal(cl, w.C(), cl.names[w.B()], true)
	}
	return err
}

// regNoteGlobal remembers, at global site site of cl's code, the global
// epoch it found name's binding it may read or write in place at: a plain data
// property of the global object -- a writable one for a write -- of a
// layout of its own, which no script-level lexical binding shadows. While
// the epoch is what it was, nothing of that has changed (shapeTree's
// globalEpoch): the global object's table has only been appended to and no
// lexical binding declared. The epoch is kept as one more than it is, so
// that a site never filled is never current.
func (r *Runtime) regNoteGlobal(cl *closure, site uint16, name Atom, write bool) {
	if cl.regEpochs == nil {
		cl.regEpochs = make([]uint64, len(cl.ic))
	}
	cl.regEpochs[site] = 0
	s := &cl.ic[site]
	env := s.p1
	if env == nil || s.idx < 0 || int(s.idx) >= int(env.valn) || env.shape == nil {
		return
	}
	sh := env.shape
	if env.flags&objGlobalBindings == 0 || !sh.unique || sh.loose || sh.tree != r.shapes {
		return
	}
	p := sh.keys[s.idx]
	if p.key != name || r.lexShadows(name) {
		return
	}
	mask := propAccessor | propPrivate | propDeleted | propUninit
	want := propFlags(0)
	if write {
		mask |= propWritable
		want = propWritable
	}
	if p.flags&mask == want {
		cl.regEpochs[site] = r.shapes.globalEpoch + 1
	}
}

// regGetProp is get_prop's slow path, as the stack interpreter's case.
func (r *Runtime) regGetProp(cl *closure, c *regCode, w bytecode.RInstr, obj Value) (Value, error) {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		o := obj.Object()
		// A plain own property of a small table, remembered for the loop's
		// check of the next object of its layout (ownScan).
		if v, ok := ownScan(&cl.ic[site], o, name); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(&cl.ic[site], o, name); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, name)
}

// regGetMethod is get_prop_this's slow path.
func (r *Runtime) regGetMethod(cl *closure, c *regCode, w bytecode.RInstr, obj Value) (Value, error) {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		if v, ok := ownScan(&cl.ic[site], obj.Object(), name); ok {
			return v, nil
		}
		if v, ok := r.cachedProp(&cl.ic[site], obj.Object(), name); ok {
			return v, nil
		}
	}
	return r.getValueProp(obj, name)
}

// regSetProp is set_prop's slow path.
func (r *Runtime) regSetProp(cl *closure, c *regCode, w bytecode.RInstr, obj, v Value) error {
	site := w.C()
	name := cl.names[c.siteName[site]]
	if obj.IsObject() {
		return r.setPropCached(&cl.ic[site], obj.Object(), name, v, cl.fn.Strict)
	}
	return r.setValueProp(obj, name, v, cl.fn.Strict)
}

// regDefineField is define_field.
func (r *Runtime) regDefineField(cl *closure, w bytecode.RInstr, obj, v Value) error {
	if !obj.IsObject() {
		return nil
	}
	name := cl.names[w.C()]
	if o := obj.Object(); o.class == ClassObject && o.flags&objExtensible != 0 {
		if next := o.transition(name, propDefault); next != nil {
			o.appendTransition(v, next)
			return nil
		}
	}
	return r.defineOwnProp(obj.Object(), name, v, propDefault)
}

// regNewObject is new_object.
func (r *Runtime) regNewObject(cl *closure, w bytecode.RInstr) *Object {
	// With the room the site's objects came to need, as the tree tier makes
	// them (outgrowProps learns it), where the literal says less.
	root := r.shapes.siteRoot(&cl.ic[w.C()], r.proto.object)
	m := int(w.B())
	if root != nil && int(root.slack) > m {
		m = int(root.slack)
	}
	return newShapedLiteral(root.orRoot(ClassObject, r.proto.object), ClassObject, m)
}

// regApplyArgs is apply_args: target.apply(this, the frame's arguments),
// which the built-in apply makes with the arguments as passed, no object
// made; any other apply is given one.
func (r *Runtime) regApplyArgs(f *frame, target, apply, this Value) (Value, error) {
	if !apply.IsObject() || apply.Object() != r.applyFn {
		return r.call(apply, target, []Value{this, Obj(r.newArgumentsObject(f))})
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show.
				args = append([]Value(nil), args...)
				copy(args, f.locals[:n])
				break
			}
		}
	}
	return r.applyCall(target, this, args)
}
