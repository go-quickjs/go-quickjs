package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The register form's classes, private names, super and this
// (docs/register-interpreter.md, stage 2): cold operations, each what the
// stack form's instruction of the same name does, with the runtime
// functions the stack interpreter calls.

// regNewClass is new_class.
func (r *Runtime) regNewClass(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	return r.linkClass(a, b)
}

// regSetFieldInit is set_field_init.
func (r *Runtime) regSetFieldInit(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() && b.IsObject() {
		if fd := a.Object().fn(); fd != nil {
			fd.setFieldInit(b.Object())
		}
	}
	return nil
}

// regDefineMethod is define_method.
func (r *Runtime) regDefineMethod(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() {
		key := regName(f.cl, c, pc, w.C())
		// A static member named after a function's synthesized length
		// or name replaces it in place, so it has to exist first.
		r.materializeFunctionProp(a.Object(), key)
		a.Object().setOwnRaw(key, b, propWritable|propConfigurable)
	}
	return nil
}

// regClassAccessor is class_getter, class_setter.
func (r *Runtime) regClassAccessor(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	op := w.Op()
	if a.IsObject() && b.IsObject() {
		r.defineHalfAccessor(a.Object(), regName(f.cl, c, pc, w.C()), b.Object(), op == bytecode.RClassGetter, memberFlags(1))
	}
	return nil
}

// regClassAccessorIndex is class_getter_index, class_setter_index.
func (r *Runtime) regClassAccessorIndex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	op := w.Op()
	k, err := r.toPropertyKey(b)
	if err != nil {
		return err
	}
	if a.IsObject() && x.IsObject() {
		return r.defineHalfAccessorChecked(a.Object(), k, x.Object(), op == bytecode.RClassGetterIndex, memberFlags(1))
	}
	return nil
}

// regClassIndex is class_index.
func (r *Runtime) regClassIndex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	k, err := r.toPropertyKey(b)
	if err != nil {
		return err
	}
	if a.IsObject() {
		return r.createDataProperty(a.Object(), k, x, memberFlags(1))
	}
	return nil
}

// regPrivateName is private_name.
func (r *Runtime) regPrivateName(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regs[w.A()] = r.newPrivateName(regName(f.cl, c, pc, w.B()))
	return nil
}

// regNewPrivateMethods is new_private_methods.
func (r *Runtime) regNewPrivateMethods(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regs[w.A()] = r.newPrivateMethods()
	return nil
}

// regAddPrivateMethod is add_private_method.
func (r *Runtime) regAddPrivateMethod(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateMethod)
	return nil
}

// regAddPrivateGetter is add_private_getter.
func (r *Runtime) regAddPrivateGetter(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateGetter)
	return nil
}

// regAddPrivateSetter is add_private_setter.
func (r *Runtime) regAddPrivateSetter(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateSetter)
	return nil
}

// regInstallPrivateMethods is install_private_methods.
func (r *Runtime) regInstallPrivateMethods(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	this, bound := regThisValue(f, regs)
	if !bound {
		return r.throwError(errReference, "\"this\" is not bound until super() has been called")
	}
	return r.installPrivateMethods(this, a)
}

// regDefinePrivateField is define_private, define_private_method.
func (r *Runtime) regDefinePrivateField(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	op := w.Op()
	if a.IsObject() {
		key := regPrivateKey(f, x)
		// A private field can only be added once. A constructor that
		// returns an object it has already built would otherwise give
		// it the same field twice.
		if a.Object().getOwn(key).ok() {
			return r.throwTypeError("%s is already present on this object", r.atoms.name(key))
		}
		if !a.Object().IsExtensible() {
			return r.throwTypeError("cannot add %s to a non-extensible object", r.atoms.name(key))
		}
		flags := propFlags(propWritable | propPrivate)
		if op == bytecode.RDefinePrivateMethod {
			flags = propPrivate
		}
		a.Object().setOwnRaw(key, b, flags)
	}
	return nil
}

// regDefinePrivateAccessor is define_private_getter, define_private_setter.
func (r *Runtime) regDefinePrivateAccessor(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	op := w.Op()
	if a.IsObject() && b.IsObject() {
		r.definePrivateAccessor(a.Object(), regPrivateKey(f, x), b.Object(), op == bytecode.RDefinePrivateGetter)
	}
	return nil
}

// regPrivateIn is private_in.
func (r *Runtime) regPrivateIn(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	b := regs[w.B()]
	x := regs[w.C()]
	if !b.IsObject() {
		return r.throwTypeError("the right operand of \"in\" must be an object, not %s", r.describe(b))
	}
	regs[w.A()] = Bool(b.Object().getOwn(regPrivateKey(f, x)).ok())
	return nil
}

// regSuperCallOf is super_call, super_forward, super_spread.
func (r *Runtime) regSuperCallOf(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	b := regs[w.B()]
	op := w.Op()
	var args []Value
	switch op {
	case bytecode.RSuperForward:
		args = f.args
	case bytecode.RSuperSpread:
		if b.IsObject() {
			args = b.Object().elemsOf()
		}
	default:
		// The arguments in their registers, as a call passes them: the
		// parent's frame is made above this one's window, and no callee
		// keeps the slice it is given.
		lo := int(w.C())
		args = regs[lo : lo+int(w.B()) : lo+int(w.B())]
	}
	if err := r.superCall(f, args); err != nil {
		return err
	}
	regs[w.A()] = f.this
	if f.cl.fn.Kind == bytecode.KindDerivedConstructor {
		// The constructor's own call: this, bound for good, is its
		// register 1 from here (RegCode.ThisRef's thisBound).
		regs[1] = f.this
	}
	return nil
}

// regSuperGet is super_get.
func (r *Runtime) regSuperGet(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regSyncThis(f, regs)
	v, err := r.superGet(f, regName(f.cl, c, pc, w.B()))
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regSuperSet is super_set.
func (r *Runtime) regSuperSet(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	regSyncThis(f, regs)
	return r.superSet(f, regName(f.cl, c, pc, w.B()), a, f.cl.fn.Strict)
}

// regSuperBase is super_base.
func (r *Runtime) regSuperBase(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regSyncThis(f, regs)
	base, _, err := r.superRef(f)
	if err != nil {
		return err
	}
	if base == nil {
		// No prototype to reach: the reference has a null base, which the
		// read or the write reports once it gets there.
		regs[w.A()] = Null
		return nil
	}
	regs[w.A()] = Obj(base)
	return nil
}

// regSuperGetIndex is super_get_index.
func (r *Runtime) regSuperGetIndex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	b := regs[w.B()]
	x := regs[w.C()]
	if !b.IsObject() {
		return r.throwTypeError("\"super\" has no prototype to read from")
	}
	key, err := r.toPropertyKey(x)
	if err != nil {
		return err
	}
	this, _ := regThisValue(f, regs)
	v, err := r.getProp(b.Object(), key, this)
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regSuperSetIndex is super_set_index.
func (r *Runtime) regSuperSetIndex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	x := regs[w.C()]
	if !a.IsObject() {
		return r.throwTypeError("\"super\" has no prototype to assign to")
	}
	key, err := r.toPropertyKey(b)
	if err != nil {
		return err
	}
	regSyncThis(f, regs)
	return r.superSetFrom(f, a, key, x, f.cl.fn.Strict)
}

// regThis is this_bound.
func (r *Runtime) regThis(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	v, bound := regThisValue(f, regs)
	if !bound {
		return r.throwError(errReference, "\"this\" is not bound until super() has been called")
	}
	regs[w.A()] = v
	return nil
}

// regCheckThis is check_this_init.
func (r *Runtime) regCheckThis(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	if _, bound := regThisValue(f, regs); !bound {
		return r.throwError(errReference, "\"this\" is not bound until super() has been called")
	}
	return nil
}

// regNewTarget is new_target.
func (r *Runtime) regNewTarget(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regs[w.A()] = f.newTarget
	return nil
}

// regDerivedRet is derived_ret.
func (r *Runtime) regDerivedRet(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	v, err := r.derivedResult(f, f.cl, a)
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regDeleteSuper is delete_super.
func (r *Runtime) regDeleteSuper(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	return r.throwError(errReference, "a super reference cannot be deleted")
}

// regCloseRegsOp is close_regs.
func (r *Runtime) regCloseRegsOp(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	regCloseRegs(f, regs, int(w.A()), int(w.B()))
	return nil
}

// regGetPrivate is get_private, a slow path of runReg's, R[A] = R[B].#R[C]:
// the member where the site's cache (Aux's high 16 bits) remembers it for
// the object's shape, or found and remembered, as the tree's privateRead
// does, and getPrivate's answer otherwise. A function's caches are its
// closures', which every evaluation of the class shares, each with names
// of its own: the key at the remembered index is checked.
//
//go:noinline
func (r *Runtime) regGetPrivate(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	b := regs[w.B()]
	if !b.IsObject() {
		f.pc = c.src[pc] + 1
		return r.throwTypeError("cannot read a private member of %s", r.describe(b))
	}
	aux := c.rc.Aux[pc]
	ob, key := b.Object(), regPrivateKey(f, regs[w.C()])
	if site := aux >> 16; site != 0 {
		e := &f.cl.ic[site]
		if ob.shape == e.ownShape && ob.shape.keys[e.idx].key == key {
			regs[w.A()] = ob.vals()[e.idx]
			return nil
		}
		if p, h := e.polyEntry(ob); h == ob && ob.shape.keys[p.idx].key == key {
			regs[w.A()] = ob.vals()[p.idx]
			return nil
		}
		if i := ob.findOwn(key); i >= 0 && ob.shape.keys[i].flags&(propAccessor|propDeleted|propUninit) == 0 {
			e.noteOwn(ob, i)
			regs[w.A()] = ob.vals()[i]
			return nil
		}
	}
	f.pc = c.src[pc] + 1
	v, err := r.getPrivate(ob, key, regName(f.cl, c, pc, uint16(aux)))
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regSetPrivate is set_private, a slow path of runReg's, R[A].#R[C] = R[B]:
// cached as regGetPrivate's read is, a writable field's.
//
//go:noinline
func (r *Runtime) regSetPrivate(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	a := regs[w.A()]
	if !a.IsObject() {
		f.pc = c.src[pc] + 1
		return r.throwTypeError("cannot write a private member of %s", r.describe(a))
	}
	aux := c.rc.Aux[pc]
	ob, key, v := a.Object(), regPrivateKey(f, regs[w.C()]), regs[w.B()]
	if site := aux >> 16; site != 0 {
		e := &f.cl.ic[site]
		if ob.shape == e.ownShape && ob.shape.keys[e.idx].key == key {
			ob.vals()[e.idx] = v
			return nil
		}
		if p, h := e.polyEntry(ob); h == ob && ob.shape.keys[p.idx].key == key {
			ob.vals()[p.idx] = v
			return nil
		}
		if i := ob.findOwn(key); i >= 0 &&
			ob.shape.keys[i].flags&(propAccessor|propDeleted|propUninit|propWritable) == propWritable {
			e.noteOwn(ob, i)
			ob.vals()[i] = v
			return nil
		}
	}
	f.pc = c.src[pc] + 1
	return r.setPrivate(ob, key, regName(f.cl, c, pc, uint16(aux)), v)
}

// regPrivateKey is the key of the private name whose binding holds v, as
// privateKey reads it.
func regPrivateKey(f *frame, v Value) Atom {
	if !v.IsSymbol() {
		return atomEmpty
	}
	return f.cl.realm.agent.atoms.internSymbol(v.Symbol())
}

// regThisValue is the this of register frame f and whether it is bound: a
// derived constructor's binding, and an arrow's written inside one, or its
// register 1.
func regThisValue(f *frame, regs *[1 << 16]Value) (Value, bool) {
	if f.thisRef != nil {
		return f.thisRef.value, f.thisRef.init
	}
	return regs[1], true
}

// regSyncThis writes the frame's this, its register 1, to the frame record
// the runtime's super functions read it from.
func regSyncThis(f *frame, regs *[1 << 16]Value) {
	if f.thisRef == nil {
		f.this = regs[1]
	}
}

// regCloseRegs closes the upvalues f's closures keep open on its n
// registers from first: a scope's bindings, which its next entry makes new.
func regCloseRegs(f *frame, regs *[1 << 16]Value, first, n int) {
	if len(f.openUpvalues) == 0 {
		return
	}
	kept := f.openUpvalues[:0]
	for _, u := range f.openUpvalues {
		closed := false
		for i := first; i < first+n; i++ {
			if u.slot == &regs[i] {
				u.close()
				closed = true
				break
			}
		}
		if !closed {
			kept = append(kept, u)
		}
	}
	clear(f.openUpvalues[len(kept):])
	f.openUpvalues = kept
}
