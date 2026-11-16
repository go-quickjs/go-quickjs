package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// What the register interpreter runs out of line that is not cold's own
// dispatch (zzreg_switch.go): the layout lock's pad K
// (zzreg_switch_a_layoutpad.go) keeps it where it was whatever regColdSub's
// sub-operations add (docs/optimizing.md, "The layout lock").

// regDeadCall is a call of o on recv with args whose value nothing reads
// but its truth: of the realm's exec or replace, the function told so, as
// deadExec's node tells it (deadCallRest); of anything else, the call.
func (r *Runtime) regDeadCall(o *Object, recv Value, args []Value) (Value, error) {
	switch o {
	case r.regexpExecFn:
		var mark *Object
		if recv.IsObject() {
			mark = recv.Object()
		}
		return deadCallRest[struct{}](r, r.regexpExecDead, mark, o, o.fn(), recv, args)
	case r.strReplaceFn:
		var mark *Object
		if len(args) > 0 && args[0].IsObject() {
			mark = args[0].Object()
		}
		return deadCallRest[struct{}](r, r.strReplaceDead, mark, o, o.fn(), recv, args)
	}
	return r.callRest(o, o.fn(), recv, args)
}

// regReuseSites is the sites of rc's calls of exec marked RMarkReuse, by
// call site, whose arrays execReuseOf says may be made again: its answer,
// of the stack form's call and locals, put in the frame's registers.
func regReuseSites(fn *bytecode.Function, rc *bytecode.RegCode) []*execReuseSite {
	var out []*execReuseSite
	for pc, w := range rc.Code {
		if !w.Op().MethodCall() || pc >= len(rc.Marks) || rc.Marks[pc]&bytecode.RMarkReuse == 0 {
			continue
		}
		spc := int(rc.Aux[pc])
		site := execReuseOf[struct{}](fn, spc+1, int(fn.Code[spc+1].A))
		if site == nil || rc.LocalReg[site.l] < 0 || site.s >= 0 && rc.LocalReg[site.s] < 0 {
			continue
		}
		rs := &execReuseSite{l: int(rc.LocalReg[site.l]), s: -1, strs: site.strs}
		if site.s >= 0 {
			rs.s = int(rc.LocalReg[site.s])
		}
		if out == nil {
			out = make([]*execReuseSite, rc.CallSites)
		}
		out[w.C()] = rs
	}
	return out
}

// regNewObjectLit is new_object_lit: the literal made with the room its
// site's objects came to need, as new_object makes it, and each property
// defined as define_field defines it, the values read from their
// registers. A fresh ordinary object takes each new key by a transition,
// and defining one can neither throw nor run code.
func (r *Runtime) regNewObjectLit(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	cl := f.cl
	n := int(w.X())
	ic := &cl.ic[w.C()]
	root := r.shapes.siteRoot(ic, r.proto.object)
	m := n
	if root != nil && int(root.slack) > m {
		m = int(root.slack)
	}
	o := newShapedLiteral(root.orRoot(ClassObject, r.proto.object), ClassObject, m)
	vals := regs[w.B() : int(w.B())+n]
	if final := ic.next; final != nil && root != nil && ic.s1 == root && n <= int(o.valc) {
		// The shape the keys led to from this root before, which the
		// site keeps as the tree's literal node does: transitions are
		// never undone, so the values go in place.
		dst := o.vals()[:n]
		copy(dst, vals)
		o.setVals(dst)
		o.shape = final
		if verifyShapes {
			checkShape(o)
		}
		regs[w.A()] = Obj(o)
		return
	}
	along := root != nil
	keys := c.rc.ObjKeys[c.rc.Aux[pc]]
	for i, key := range keys {
		name := cl.names[key]
		if next := o.transition(name, propDefault); next != nil {
			o.appendTransition(vals[i], next)
		} else {
			// No transition where the shape tree is full: the long way,
			// which for a fresh extensible object of distinct keys
			// cannot fail.
			along = false
			_ = r.defineOwnProp(o, name, vals[i], propDefault)
		}
	}
	if s := o.shape; along && s != nil && !s.unique && !s.loose && int(s.n) == n && int(o.valn) == n &&
		(s.index != nil || n <= linearScanLimit) {
		ic.next, ic.s1 = s, root
	}
	regs[w.A()] = Obj(o)
}

// regSwitch is switch: k's pc, past the instruction, made the body's
// where v equals a case.
func regSwitch(c *regCode, k *regKept, v Value, consts []Value) {
	w := regAt(c.code, k.pc-1)
	if to := c.switches[w.BC()].lookup(v, consts); to >= 0 {
		k.pc = int(to)
	}
}

// regSwitchTypeof is switch_typeof: k's pc made the body of v's type's
// name, where a case has it.
func regSwitchTypeof(c *regCode, k *regKept, v Value) {
	w := regAt(c.code, k.pc-1)
	if to := c.switches[w.BC()].typeTo[typeofKind[struct{}](v)]; to >= 0 {
		k.pc = int(to)
	}
}

// regTypeof is typeof: the type's name the runtime keeps, as
// typeofString answers it.
func (r *Runtime) regTypeof(c *regCode, k *regKept, regs *[1 << 16]Value) {
	w := regAt(c.code, k.pc-1)
	regs[w.A()] = Str(r.typeofString(regs[w.B()]))
}

// regNewArrayE is new_array_empty: the tree's node for the literal, made
// here, its site in the closure's arrays, named to native code by the
// stack form's pc. Making an array throws nothing, so the frame's pc is
// left as it is.
func (r *Runtime) regNewArrayE(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	n := arrayEmptyNode[struct{}]{site: int(w.B()), pc: int32(regSrc(c, pc))}
	regs[w.A()] = n.make(regTctx(r, f, f.cl))
}

// regCharCodeAtSlow is char_code_at as get_method_w and call_method_w make
// it: the method read, with the read's position (Aux), then the argument
// read, and the call, with the call's.
func (r *Runtime) regCharCodeAtSlow(f *frame, c *regCode, pc int, regs *[1 << 16]Value) error {
	w := regAt(c.code, pc)
	o := regs[w.B()]
	f.pc = c.rc.Aux[pc] + 1
	m, err := r.regGetMethod(f.cl, c, w, o)
	if err != nil {
		return err
	}
	a := regs[w.X()]
	f.pc = c.src[pc] + 1
	v, err := r.regCall(m, o, []Value{a})
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regCatch is where a register frame goes on after the instruction at
// k.pc-1 threw err: the pc of its handler, the innermost whose ranges hold
// the instruction, with the exception's value in the handler's register;
// or -1 where none does, or err is no JavaScript exception -- an interrupt,
// a memory limit -- which no handler catches, as in the interpreter
// (unwindToHandler). A call that threw has left the stack and the frames
// as they were before it.
//
//go:noinline
func (r *Runtime) regCatch(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value, err error) int {
	if r.debug != nil {
		// As unwindToHandler: a debugger may stop where it is thrown.
		r.debugThrow(err)
	}
	if len(c.rc.Handlers) == 0 {
		return -1
	}
	th, ok := err.(*Thrown)
	if !ok {
		return -1
	}
	h := c.rc.HandlerAt(k.pc - 1)
	if h == nil {
		return -1
	}
	regs[h.Reg] = th.Value
	return int(h.To)
}

// regIterNextTree is iter_next of a generator's cursor, stepped as the
// tree steps one (iterNextTree): a generator of tree code resumed in
// place (stepGenTree), not through its next's call. Its body throws as the
// tree throws, with a panic, which a function of the tree with a for-of
// loop catches under its own recover; register code has none, so the
// step has one, and leaves the frames and the stack as runTreeHandlers
// does. The frame's tctx is the step's: the tree reads only its runtime.
func (r *Runtime) regIterNextTree(f *frame, cur Value) (v Value, more bool, err error) {
	depth, top := r.frameDepth, r.stackTop
	defer func() {
		if p := recover(); p != nil {
			th, ok := p.(treeThrow)
			if !ok {
				panic(p)
			}
			r.unwindTreeFrames(depth)
			r.stackTop = top
			v, more, err = Undefined, false, th.err
		}
	}()
	tc := &f.tc
	tc.r, tc.f = r, f
	return iterNextTree(tc, cur)
}

// regStepNext is iter_next of a cursor neither of an array, a for-in nor
// a generator, as iterNext steps it (stepIter): a next of script called as
// register code calls (regCall), its register code entered directly rather
// than through the frames a call from Go makes, and its result's done and
// value read; a Map's or a Set's iterator of this realm stepped as the
// tree's iterNextTree steps it; anything else by stepIter. The cursor is
// done once it is, or once a step fails.
func (r *Runtime) regStepNext(st *iterState) (Value, bool, error) {
	var fd *funcData
	if st.next.IsObject() {
		fd = st.next.Object().fn()
	}
	if fd == nil || fd.closure == nil || fd.native != nil {
		if fd != nil && fd.iterNext == iterNextMap && (fd.realm == nil || fd.realm == r.Realm) && st.iter.IsObject() {
			if d, ok := st.iter.Object().dataOf().(*mapIterData); ok {
				if err := r.tick(); err != nil {
					st.done = true
					return Undefined, false, err
				}
				v, ok := r.mapIterStep(d)
				if !ok {
					st.done = true
				}
				return v, ok, nil
			}
		}
		v, ok, err := r.stepIter(st.iter, st.next)
		if !ok {
			st.done = true
		}
		return v, ok, err
	}
	res, err := r.regCall(st.next, st.iter, nil)
	if err == nil && !res.IsObject() {
		err = r.throwTypeError("an iterator result must be an object")
	}
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
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

// regColdWrap is wrap, R[A] = R[B] op R[C] for X.asUintN(w, x op y) or
// asIntN's, the call right after it: where the callee is BigInt.asUintN
// or asIntN and x and y BigInts held in their Values, the call's value
// made at once, x op y of the call's width, as the tree's wrapCall makes
// it (wrapShort), and the call passed over -- a built-in that has nothing
// else to do; anything else the operator's value as ever, for the call.
func (r *Runtime) regColdWrap(c *regCode, k *regKept, w bytecode.RInstr, regs *[1 << 16]Value) error {
	op := regStackOp[bytecode.ROp(regAuxAt(c, k.pc-1))&127]
	a, b := regs[w.B()], regs[w.C()]
	if a.isShortBig() && b.isShortBig() {
		cw := regAt(c.code, k.pc)
		if fv := regs[cw.A()]; fv.IsObject() {
			if fd := fv.Object().fn(); fd != nil && (fd.mathOp == mathAsUintN || fd.mathOp == mathAsIntN) {
				if v, ok := wrapShort(fd.mathOp == mathAsIntN, regs[int(cw.A())+2], op, a, b); ok {
					regs[w.A()] = v
					regs[cw.B()] = v
					k.pc++
					return nil
				}
			}
		}
	}
	var v Value
	var err error
	switch op {
	case bytecode.OpAdd:
		v, err = r.add(a, b)
	case bytecode.OpSub, bytecode.OpMul:
		v, err = r.arith(op, a, b)
	default:
		v, err = r.regBinary(op, a, b)
	}
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regGenCursor is the generator of register code st steps, or nil: its
// step is regGenStep's, with no recover, its body throwing as register
// code throws, with an error.
func regGenCursor(st *iterState) *generator {
	if !st.iter.IsObject() {
		return nil
	}
	if o := st.iter.Object(); o.class == ClassGenerator {
		if g := genOf(o); g.reg {
			return g
		}
	}
	return nil
}

// regFar is far register i of the window regs starts: past the 65,536
// registers regs' type covers, within the NumRegs the window was made with
// (the verifier checks each far register an instruction names).
func regFar(regs *[1 << 16]Value, i uint32) *Value {
	return (*Value)(unsafe.Add(unsafe.Pointer(regs), uintptr(i)*unsafe.Sizeof(Value{})))
}
