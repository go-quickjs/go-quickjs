package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regDirectEval is direct_eval: a call
// written eval(...), whose window holds the callee, its receiver and the
// arguments, or an array of them. Where the callee is the realm's eval and
// the first argument a string, the code runs in the frame's scope
// (regEval); a value that is no string is given back; any other callee is
// an ordinary call, and a tail call in tail position (RMarkTail).
func (r *Runtime) regDirectEval(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := int(w.A())
	callee, recv := regs[a], regs[a+1]
	aux := regAuxAt(c, pc)
	var args []Value
	if aux&bytecode.RDirectEvalSpread != 0 {
		if l := regs[a+2]; l.IsObject() {
			args = l.Object().elemsOf()
		}
	} else {
		args = regs[a+2 : a+2+int(w.C())]
	}
	src := arg(args, 0)
	var v Value
	var err error
	switch {
	case !callee.IsObject() || callee.Object() != r.evalFn:
		if r.frameDepth >= tailCallDepth && pc < len(c.rc.Marks) && c.rc.Marks[pc]&bytecode.RMarkTail != 0 &&
			(f.regKind == regFrameCall || f.newTarget.IsUndefined() && f.thisRef == nil) {
			// As regTail gives a call_w's callee the frame.
			r.pendingTail = tailCall{callee: callee, this: recv, args: append([]Value(nil), args...)}
			return errTailCall
		}
		v, err = r.call(callee, recv, args)
	case !src.IsString():
		v = src
	default:
		v, err = r.regEval(f, c, regs, c.fn.EvalScopes[aux&^bytecode.RDirectEvalSpread], src.String().Go())
	}
	if err != nil {
		return err
	}
	regs[w.B()] = v
	return nil
}

// regEval runs a direct eval's code src in register frame f's scope, as
// evalIn runs it in a heap frame's: each binding of the frame's the
// code names is captured from its register (RegCode.LocalReg), which is
// what lets the code write it, as a closure the frame makes captures it
// (regClosure); an upvalue of the frame's is shared.
func (r *Runtime) regEval(f *frame, c *regCode, regs *[1 << 16]Value, scope bytecode.EvalScope, src string) (Value, error) {
	if r.evaluator == nil {
		return Undefined, r.throwTypeError("code generation from strings is disabled")
	}
	fn, err := r.evaluator(src, EvalRequest{Direct: true, Scope: scope})
	if err != nil {
		return Undefined, r.wrapEvalError(err)
	}
	if fn.Script != nil {
		fn.Script.EvalOrigin = r.evalOrigin()
		fn.Script.Referrer = r.activeReferrer()
	}
	return r.regEvalCode(f, c, regs, fn)
}

// regEvalCode runs fn, a direct eval's compiled code, in register frame
// f's scope: regEval's, and a debugger's evaluation in a paused frame's.
func (r *Runtime) regEvalCode(f *frame, c *regCode, regs *[1 << 16]Value, fn *bytecode.Function) (Value, error) {
	cl := r.prepareIn(fn, f.cl.env)
	cl.upvalues = make([]*upvalue, len(fn.Upvalues))
	for i, desc := range fn.Upvalues {
		switch {
		case desc.FromParent:
			cl.upvalues[i] = regOpenUpvalue(f, &regs[c.rc.LocalReg[desc.Index]])
		case int(desc.Index) < len(f.cl.upvalues):
			cl.upvalues[i] = f.cl.upvalues[desc.Index]
		default:
			// As evalIn: nothing to bind to reads as undefined.
			cl.upvalues[i] = &upvalue{}
			cl.upvalues[i].slot = &cl.upvalues[i].closed
		}
	}
	// What the code shares of the frame's this, read from its register 1.
	regSyncThis(f, regs)
	return r.evalLinked(f, cl)
}

// regOpenUpvalue is the upvalue open on register slot of frame f, opened
// where none is, as regClosure opens one.
func regOpenUpvalue(f *frame, slot *Value) *upvalue {
	for _, u := range f.openUpvalues {
		if u.slot == slot {
			return u
		}
	}
	u := &upvalue{slot: slot}
	f.openUpvalues = append(f.openUpvalues, u)
	return u
}

// evalLinked runs cl, a direct eval's code linked to caller's bindings, as
// evalIn runs it: as an arrow sharing the caller's this, new.target,
// arguments, home object and the scopes of its with statements and its
// eval's variables.
func (r *Runtime) evalLinked(caller *frame, cl *closure) (Value, error) {
	callee := newObject(r.proto.function, ClassFunction)
	fd := &funcData{
		closure: cl,
		name:    "eval",
		arrow:   true,
		extra: &funcExtra{
			lexThis:    caller.this,
			lexThisRef: caller.thisRef,
			rare:       scopesOf(caller.withScopes, caller.evalVars),
		},
	}
	fd.extra.setNewTarget(caller.newTarget)
	if caller.callee != nil {
		if outer := caller.callee.fn(); outer != nil {
			fd.cls = outer.cls
		}
	}
	callee.setData(fd)
	return r.run(cl, caller.this, caller.args, caller.newTarget, callee)
}

// The with statement's operations and the probes of a name a with
// object or an eval's variable may answer: with_push, with_pop, with_get,
// with_get_this, with_delete, with_set, with_resolve, and with_read and
// with_write of a reference read and then written back.

// regWithPush is with_push.
func (r *Runtime) regWithPush(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	if w.B() == bytecode.WithPushEvalScope {
		// The body's scope for what a direct eval in it declares, inside
		// the parameters' (the compiler's bodyEvalScope).
		o := &Object{class: ClassObject, flags: objExtensible | objEvalVars}
		f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], o)
		f.evalVars = o
		return nil
	}
	o, err := r.withObject(regs[w.A()])
	if err != nil {
		return err
	}
	// Capped, so that the append copies rather than writing into an
	// array a closure made under an earlier with keeps (with_push).
	f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], o)
	return nil
}

// regWithPop is with_pop.
func (r *Runtime) regWithPop(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	f.withScopes = f.withScopes[:len(f.withScopes)-1]
	return nil
}

// regWithRead is with_read.
func (r *Runtime) regWithRead(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	v, err := r.withRead(regs[w.B()].Object(), regName(f.cl, c, pc, w.C()), f.cl.fn.Strict)
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regWithWrite is with_write.
func (r *Runtime) regWithWrite(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	return r.withWrite(regs[w.B()].Object(), regName(f.cl, c, pc, w.C()), regs[w.A()], f.cl.fn.Strict)
}

// regWithFind is the probe with_get, with_get_this, with_delete, with_set
// and with_resolve make of the name Names[B]: the with object that has it,
// found or not in the register Aux.
func (r *Runtime) regWithFind(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) (*Object, Atom, bool, error) {
	name := regName(f.cl, c, pc, w.B())
	o, found, err := r.withFound(f.withScopes, name, int(w.C()))
	if err != nil {
		return nil, name, false, err
	}
	regs[regAuxAt(c, pc)] = Bool(found)
	return o, name, found, nil
}

// regWithGet is with_get and with_get_this.
func (r *Runtime) regWithGet(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	o, name, found, err := r.regWithFind(f, c, pc, w, regs)
	if err != nil || !found {
		return err
	}
	v, err := r.withRead(o, name, f.cl.fn.Strict)
	if err != nil {
		return err
	}
	regs[w.A()] = v
	if w.Op() == bytecode.RWithGetThis {
		// The bindings a direct eval declared are no object a script
		// sees: a call through one has no receiver.
		regs[w.A()+1] = Obj(o)
		if o.flags&objEvalVars != 0 {
			regs[w.A()+1] = Undefined
		}
	}
	return nil
}

// regWithDelete is with_delete.
func (r *Runtime) regWithDelete(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	o, name, found, err := r.regWithFind(f, c, pc, w, regs)
	if err != nil || !found {
		return err
	}
	ok, err := r.deleteProp(o, name, f.cl.fn.Strict)
	if err != nil {
		return err
	}
	regs[w.A()] = Bool(ok)
	return nil
}

// regWithSet is with_set.
func (r *Runtime) regWithSet(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	o, name, found, err := r.regWithFind(f, c, pc, w, regs)
	if err != nil || !found {
		return err
	}
	return r.withWrite(o, name, regs[w.A()], f.cl.fn.Strict)
}

// regWithResolve is with_resolve.
func (r *Runtime) regWithResolve(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	o, _, found, err := r.regWithFind(f, c, pc, w, regs)
	if err != nil {
		return err
	}
	regs[w.A()] = Undefined
	if found {
		regs[w.A()] = Obj(o)
	}
	return nil
}

// regDebug is debug: a place code compiled for a debugger can stop, the
// statement's index in Aux. The frame's pc is the instruction's, which
// the debugger finds the statement, its scope and its position by
// (regStatementAt), and its this the one in register 1.
func (r *Runtime) regDebug(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	d := r.debug
	if d == nil || !d.armed && w.B() == 0 {
		return nil
	}
	idx := regAuxAt(c, pc)
	f.pc = uint32(pc) + 1
	regSyncThis(f, regs)
	return r.debugStatement(f, int(idx), w.B() != 0)
}

// regDebugFrame is the register code and window of frame f where it runs
// as register code, for a debugger's look at its bindings: nil where it has
// none.
func (r *Runtime) regDebugFrame(f *frame) (*regCode, *[1 << 16]Value) {
	if f.cl == nil {
		return nil, nil
	}
	c := f.cl.reg
	if c == nil {
		c = r.regOf(f.cl)
	}
	if c == nil {
		return nil, nil
	}
	return c, regWindow(r.stack, f.base-int(c.locals))
}

// regCaughtAt reports whether register code c catches what an instruction
// of it throws at pc -- one past, as a frame keeps it -- in a catch
// clause's handler (RHandler.Catches), for a debugger asking whether an
// exception is caught.
func regCaughtAt(c *regCode, pc uint32) bool {
	if pc == 0 {
		return false
	}
	i, ok := regThrowerAt(c, int(pc-1))
	if !ok {
		return false
	}
	for j := range c.rc.Handlers {
		h := &c.rc.Handlers[j]
		for k := 0; h.Catches && k+1 < len(h.Ranges); k += 2 {
			if uint32(i) >= h.Ranges[k] && uint32(i) < h.Ranges[k+1] {
				return true
			}
		}
	}
	return false
}

// regThrowerAt is the instruction of c a frame's pc, less one, stands
// for: pc itself, or, for a pc past the code -- the position of an error
// an instruction places apart from its own (bytecode.RegCode.Lines) --
// the instruction whose Aux holds it.
func regThrowerAt(c *regCode, pc int) (int, bool) {
	rc := c.rc
	if pc < len(rc.Code) {
		return pc, pc >= 0
	}
	for i, a := range rc.Aux {
		if int(a) == pc {
			switch rc.Code[i].Op() {
			case bytecode.RCharCodeAt, bytecode.RLit, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32,
				bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI:
				return i, true
			}
		}
	}
	return 0, false
}

// regStatementAt is the index of the statement of code compiled for a
// debugger that the instruction at pc is in: that of the last debug
// instruction at or before it, or -1.
func regStatementAt(c *regCode, pc int) int {
	pc, ok := regThrowerAt(c, pc)
	if !ok {
		return -1
	}
	for ; pc >= 0; pc-- {
		if regAt(c.code, pc).Op() == bytecode.RDebug {
			return int(regAuxAt(c, pc))
		}
	}
	return -1
}
