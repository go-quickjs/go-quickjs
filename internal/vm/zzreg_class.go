package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The register form's classes, private names, super and this
// (docs/register-interpreter.md, stage 2): cold operations, each what the
// stack form's instruction of the same name does, with the runtime
// functions the stack interpreter calls.

// regColdClass runs cold operation w, at pc of c, in frame f.
//
//go:noinline
func (r *Runtime) regColdClass(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a, b, x := regs[w.A()], regs[w.B()], regs[w.C()]
	switch op := bytecode.RColdOp(w.X()); op {
	case bytecode.RColdNewClass:
		return r.linkClass(a, b)
	case bytecode.RColdSetFieldInit:
		if a.IsObject() && b.IsObject() {
			if fd := a.Object().fn(); fd != nil {
				fd.setFieldInit(b.Object())
			}
		}
	case bytecode.RColdDefineMethod:
		if a.IsObject() {
			key := regName(f.cl, c, pc, w.C())
			// A static member named after a function's synthesized length
			// or name replaces it in place, so it has to exist first.
			r.materializeFunctionProp(a.Object(), key)
			a.Object().setOwnRaw(key, b, propWritable|propConfigurable)
		}
	case bytecode.RColdClassGetter, bytecode.RColdClassSetter:
		if a.IsObject() && b.IsObject() {
			r.defineHalfAccessor(a.Object(), regName(f.cl, c, pc, w.C()), b.Object(), op == bytecode.RColdClassGetter, memberFlags(1))
		}
	case bytecode.RColdClassGetterIndex, bytecode.RColdClassSetterIndex:
		k, err := r.toPropertyKey(b)
		if err != nil {
			return err
		}
		if a.IsObject() && x.IsObject() {
			return r.defineHalfAccessorChecked(a.Object(), k, x.Object(), op == bytecode.RColdClassGetterIndex, memberFlags(1))
		}
	case bytecode.RColdClassIndex:
		k, err := r.toPropertyKey(b)
		if err != nil {
			return err
		}
		if a.IsObject() {
			return r.createDataProperty(a.Object(), k, x, memberFlags(1))
		}
	case bytecode.RColdPrivateName:
		regs[w.A()] = r.newPrivateName(regName(f.cl, c, pc, w.B()))
	case bytecode.RColdNewPrivateMethods:
		regs[w.A()] = r.newPrivateMethods()
	case bytecode.RColdAddPrivateMethod:
		r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateMethod)
	case bytecode.RColdAddPrivateGetter:
		r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateGetter)
	case bytecode.RColdAddPrivateSetter:
		r.addPrivateMethod(a, regPrivateKey(f, x), b, bytecode.OpAddPrivateSetter)
	case bytecode.RColdInstallPrivateMethods:
		this, bound := regThisValue(f, regs)
		if !bound {
			return r.throwError(errReference, "\"this\" is not bound until super() has been called")
		}
		return r.installPrivateMethods(this, a)
	case bytecode.RColdDefinePrivate, bytecode.RColdDefinePrivateMethod:
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
			if op == bytecode.RColdDefinePrivateMethod {
				flags = propPrivate
			}
			a.Object().setOwnRaw(key, b, flags)
		}
	case bytecode.RColdDefinePrivateGetter, bytecode.RColdDefinePrivateSetter:
		if a.IsObject() && b.IsObject() {
			r.definePrivateAccessor(a.Object(), regPrivateKey(f, x), b.Object(), op == bytecode.RColdDefinePrivateGetter)
		}
	case bytecode.RColdPrivateIn:
		if !b.IsObject() {
			return r.throwTypeError("the right operand of \"in\" must be an object, not %s", r.describe(b))
		}
		regs[w.A()] = Bool(b.Object().getOwn(regPrivateKey(f, x)).ok())
	case bytecode.RColdSuperCall, bytecode.RColdSuperForward, bytecode.RColdSuperSpread:
		var args []Value
		switch op {
		case bytecode.RColdSuperForward:
			args = f.args
		case bytecode.RColdSuperSpread:
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
	case bytecode.RColdSuperGet:
		regSyncThis(f, regs)
		v, err := r.superGet(f, regName(f.cl, c, pc, w.B()))
		if err != nil {
			return err
		}
		regs[w.A()] = v
	case bytecode.RColdSuperSet:
		regSyncThis(f, regs)
		return r.superSet(f, regName(f.cl, c, pc, w.B()), a, f.cl.fn.Strict)
	case bytecode.RColdSuperBase:
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
	case bytecode.RColdSuperGetIndex:
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
	case bytecode.RColdSuperSetIndex:
		if !a.IsObject() {
			return r.throwTypeError("\"super\" has no prototype to assign to")
		}
		key, err := r.toPropertyKey(b)
		if err != nil {
			return err
		}
		regSyncThis(f, regs)
		return r.superSetFrom(f, a, key, x, f.cl.fn.Strict)
	case bytecode.RColdThis:
		v, bound := regThisValue(f, regs)
		if !bound {
			return r.throwError(errReference, "\"this\" is not bound until super() has been called")
		}
		regs[w.A()] = v
	case bytecode.RColdCheckThis:
		if _, bound := regThisValue(f, regs); !bound {
			return r.throwError(errReference, "\"this\" is not bound until super() has been called")
		}
	case bytecode.RColdNewTarget:
		regs[w.A()] = f.newTarget
	case bytecode.RColdDerivedRet:
		v, err := r.derivedResult(f, f.cl, a)
		if err != nil {
			return err
		}
		regs[w.A()] = v
	case bytecode.RColdDeleteSuper:
		return r.throwError(errReference, "a super reference cannot be deleted")
	case bytecode.RColdCloseRegs:
		regCloseRegs(f, regs, int(w.A()), int(w.B()))
	}
	return nil
}

// regPrivate is get_private or set_private, which regColdSub passes on.
func (r *Runtime) regPrivate(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	if regAt(c.code, k.pc-1).Op() == bytecode.RGetPrivate {
		return r.regGetPrivate(f, c, k, regs)
	}
	return r.regSetPrivate(f, c, k, regs)
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
