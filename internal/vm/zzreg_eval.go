package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regDirectEval is direct_eval, the stack form's direct_eval: a call
// written eval(...), whose window holds the callee, its receiver and the
// arguments, or an array of them. Where the callee is the realm's eval and
// the first argument a string, the code runs in the frame's scope
// (regEval); a value that is no string is given back; any other callee is
// an ordinary call, and a tail call where the stack form made one.
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
// evalDirect runs it in a heap frame's: each binding of the frame's the
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
	cl := r.prepare(fn)
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
	cl.env = caller.cl.env
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

// regColdWith is the with statement's operations and the probes of a
// name a with object or an eval's variable may answer, as the stack
// form's with_push, with_pop, with_get, with_get_this, with_delete,
// with_set, with_resolve, with_get_under and with_put_under do them.
func (r *Runtime) regColdWith(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	strict := f.cl.fn.Strict
	op := bytecode.RColdOp(w.X())
	switch op {
	case bytecode.RColdWithPush:
		o, err := r.withObject(regs[w.A()])
		if err != nil {
			return err
		}
		// Capped, so that the append copies rather than writing into an
		// array a closure made under an earlier with keeps (with_push).
		f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], o)
		return nil
	case bytecode.RColdWithPop:
		f.withScopes = f.withScopes[:len(f.withScopes)-1]
		return nil
	case bytecode.RColdWithRead:
		v, err := r.withRead(regs[w.B()].Object(), regName(f.cl, c, pc, w.C()), strict)
		if err != nil {
			return err
		}
		regs[w.A()] = v
		return nil
	case bytecode.RColdWithWrite:
		return r.withWrite(regs[w.B()].Object(), regName(f.cl, c, pc, w.C()), regs[w.A()], strict)
	}
	name := regName(f.cl, c, pc, w.B())
	o, found, err := r.withFound(f.withScopes, name, int(w.C()))
	if err != nil {
		return err
	}
	regs[regAuxAt(c, pc)] = Bool(found)
	if !found {
		if op == bytecode.RColdWithResolve {
			regs[w.A()] = Undefined
		}
		return nil
	}
	switch op {
	case bytecode.RColdWithGet, bytecode.RColdWithGetThis:
		v, err := r.withRead(o, name, strict)
		if err != nil {
			return err
		}
		regs[w.A()] = v
		if op == bytecode.RColdWithGetThis {
			// The bindings a direct eval declared are no object a script
			// sees: a call through one has no receiver.
			regs[w.A()+1] = Obj(o)
			if o.flags&objEvalVars != 0 {
				regs[w.A()+1] = Undefined
			}
		}
	case bytecode.RColdWithDelete:
		ok, err := r.deleteProp(o, name, strict)
		if err != nil {
			return err
		}
		regs[w.A()] = Bool(ok)
	case bytecode.RColdWithSet:
		return r.withWrite(o, name, regs[w.A()], strict)
	case bytecode.RColdWithResolve:
		regs[w.A()] = Obj(o)
	}
	return nil
}

// regDebug is debug, the stack form's debug_stmt: a place code compiled
// for a debugger can stop, the statement's index in Aux. The frame's pc is
// the stack form's debug_stmt's, which the debugger finds the statement,
// its scope and its position by, and its this the one in register 1.
func (r *Runtime) regDebug(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	d := r.debug
	if d == nil || !d.armed && w.B() == 0 {
		return nil
	}
	idx := regAuxAt(c, pc)
	f.pc = f.cl.fn.Debug.Statements[idx].PC + 1
	regSyncThis(f, regs)
	return r.debugStatement(f, bytecode.Instr{Op: bytecode.OpDebugStmt, A: idx, B: uint32(w.B())})
}

// regDebugFrame is the register code and window of frame f where it runs
// as register code, for a debugger's look at its bindings: nil where it
// runs as the stack form.
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
// of it throws at the stack form's pc -- one past, as a frame keeps it --
// in a catch clause's handler (RHandler.Catches), for a debugger asking
// whether an exception is caught.
func regCaughtAt(c *regCode, pc uint32) bool {
	if pc == 0 {
		return false
	}
	for i, s := range c.src {
		if s != pc-1 {
			continue
		}
		for j := range c.rc.Handlers {
			h := &c.rc.Handlers[j]
			for k := 0; h.Catches && k+1 < len(h.Ranges); k += 2 {
				if uint32(i) >= h.Ranges[k] && uint32(i) < h.Ranges[k+1] {
					return true
				}
			}
		}
	}
	return false
}
