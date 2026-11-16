package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Top-level code in the register form (docs/register-interpreter.md, "Stage
// 2: zero refusals"): the declarations a script makes before its first
// statement, and the initializations of its lexical bindings, each a cold
// operation that does what the stack form's instruction of the same name
// does (executeAt's case, the tree's globalOp).

// regCheckGlobalLex is check_global_lex.
func (r *Runtime) regCheckGlobalLex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	if r.globalLex.getOwn(name).ok() {
		return r.throwError(errSyntax, "%q has already been declared", r.atoms.name(name))
	}
	// A lexical binding shadows the global object's property of the
	// same name for good, so one that cannot be deleted may not be
	// shadowed.
	if p := r.global.getOwn(name); p.ok() && p.flags()&propConfigurable == 0 {
		return r.throwError(errSyntax,
			"%q is already a property of the global object that cannot be removed", r.atoms.name(name))
	}
	return nil
}

// regCheckGlobalVar is check_global_var.
func (r *Runtime) regCheckGlobalVar(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	flag := w.C() != 0
	if cl.scope() != r.global {
		return nil
	}
	if err := r.checkGlobalVarName(name); err != nil {
		return err
	}
	if flag {
		return r.canDeclareGlobalFunc(name)
	}
	if !r.hasOwnProp(r.global, name) && !r.global.IsExtensible() {
		return r.throwTypeError("cannot declare %q on a non-extensible global", r.atoms.name(name))
	}
	return nil
}

// regDefineGlobalVar is define_global_var.
func (r *Runtime) regDefineGlobalVar(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	flag := w.C() != 0
	if f.evalVars != nil {
		// Eval code inside a function: what it declares is that
		// function's, as the stack form's define_global_var has it.
		if !evalVarProp(f.evalVars, name).ok() {
			f.evalVars.setOwnRaw(name, Undefined, propWritable|propConfigurable)
		}
		return nil
	}
	env := cl.scope()
	if env == r.global {
		if err := r.checkGlobalVarName(name); err != nil {
			return err
		}
	}
	flags := propWritable | moduleBindingFlags(r.atoms.name(name))
	if flag {
		flags |= propConfigurable
	}
	if proxyOf(env) != nil {
		// A context's global: the var is its sandbox's.
		return r.declareOnScope(env, name, Undefined, flags, false)
	}
	if !r.hasOwnProp(env, name) {
		if env == r.global && !env.IsExtensible() {
			return r.throwTypeError("cannot declare %q on a non-extensible global", r.atoms.name(name))
		}
		env.setOwnRaw(name, Undefined, flags)
	}
	return nil
}

// regDefineGlobalFunc is define_global_func.
func (r *Runtime) regDefineGlobalFunc(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	flag := w.C() != 0
	fn := regs[w.A()]
	if f.evalVars != nil {
		f.evalVars.setOwnRaw(name, fn, propWritable|propConfigurable)
		return nil
	}
	flags := propWritable | moduleBindingFlags(r.atoms.name(name))
	if flag {
		flags |= propConfigurable
	}
	env := cl.scope()
	if proxyOf(env) != nil {
		return r.declareOnScope(env, name, fn, flags, true)
	}
	if env == r.global {
		if err := r.checkGlobalVarName(name); err != nil {
			return err
		}
		if err := r.canDeclareGlobalFunc(name); err != nil {
			return err
		}
		if p := env.getOwn(name); p.ok() && p.flags()&(propConfigurable|propAccessor) == 0 {
			// A property that cannot be redefined is updated in place,
			// keeping the attributes it was given.
			p.setValue(fn)
			return nil
		}
	}
	env.setOwnRaw(name, fn, flags)
	return nil
}

// regDeclareGlobalLex is declare_global_lex.
func (r *Runtime) regDeclareGlobalLex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	flag := w.C() != 0
	flags := propFlags(propConfigurable)
	if flag {
		flags |= propWritable
	}
	r.declareGlobalLex(name, flags)
	return nil
}

// regInitGlobalLex is init_global_lex.
func (r *Runtime) regInitGlobalLex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	r.globalLex.setOwnRaw(name, regs[w.A()], r.globalLex.getOwn(name).flags())
	return nil
}

// regDeclareModuleLex is declare_module_lex.
func (r *Runtime) regDeclareModuleLex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	flag := w.C() != 0
	flags := propUninit | moduleBindingFlags(r.atoms.name(name))
	if flag {
		flags |= propWritable
	}
	cl.scope().setOwnRaw(name, uninitialized, flags)
	return nil
}

// regInitModuleLex is init_module_lex.
func (r *Runtime) regInitModuleLex(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	name := regName(cl, c, pc, w.B())
	if p := cl.scope().getOwn(name); p.ok() {
		// Its attributes change, so its layout does.
		p.setValue(regs[w.A()])
		p.setFlags(p.flags() &^ propUninit)
	}
	return nil
}

// regNewDispose is new_dispose_capability: R[A] = a using scope's
// capability, what the stack form's instruction of the same name makes.
func (r *Runtime) regNewDispose(w bytecode.RInstr, regs *[1 << 16]Value) error {
	o := newObjectExt(nil, ClassObject, objExtensible)
	o.setData(&disposeCapability{})
	regs[w.A()] = Obj(o)
	return nil
}

// regAddDisposable is add_disposable.
func (r *Runtime) regAddDisposable(w bytecode.RInstr, regs *[1 << 16]Value) error {
	c := regs[w.A()].Object().dataOf().(*disposeCapability)
	return r.addDisposableResource(c, regs[w.B()], w.C() == 1)
}

// regDispose is dispose.
func (r *Runtime) regDispose(w bytecode.RInstr, regs *[1 << 16]Value) error {
	c := regs[w.A()].Object().dataOf().(*disposeCapability)
	// A throw leaving the scope is folded into what the disposal
	// throws.
	var completion error
	if w.C() == 1 {
		completion = r.throw(regs[w.B()])
	}
	if err := r.disposeResources(c, completion); err != nil && err != completion {
		return err
	}
	return nil
}

// regDisposeStep is dispose_step.
func (r *Runtime) regDisposeStep(w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := w.A()
	mode := uint32(w.C()) | bytecode.DisposeStep
	v, more, err := usingStep(r, regs[w.B()], regs[a], Int(int(completionThrow)), mode)
	regs[a], regs[a+1] = v, Bool(more)
	return err
}

// regGlobalOpt is get_global_opt's value of the global name: typeof's
// operand, which is undefined for an undeclared name rather than an error,
// where a lexical binding in its dead zone still throws.
func (r *Runtime) regGlobalOpt(f *frame, name Atom) (Value, error) {
	env := f.cl.scope()
	if f.evalVars != nil {
		if p := evalVarProp(f.evalVars, name); p.ok() {
			return p.value(), nil
		}
	}
	if p := r.globalLexProp(env, name); p.ok() {
		if p.value().IsUninitialized() {
			return Undefined, r.throwReferenceError("cannot access %q before it is initialized", r.atoms.name(name))
		}
		return p.value(), nil
	}
	if !r.isGlobalScope(env) {
		if p := r.moduleLexProp(env, name); p.ok() {
			if p.flags()&propUninit != 0 {
				return Undefined, r.throwReferenceError("cannot access %q before it is initialized", r.atoms.name(name))
			}
			if p.flags()&(propAccessor|propPrivate|propDeleted) == 0 {
				return p.value(), nil
			}
		}
	}
	if chainHasProxy(env) {
		// Asked whether it has the name first, as for a read; what is not
		// there is undefined.
		has, err := r.hasPropErr(env, name)
		if err != nil || !has {
			return Undefined, err
		}
	}
	return r.getProp(env, name, r.globalReceiver(env))
}

// regCheckGlobalRef is check_global_ref: whether the global name resolves,
// settled where a strict assignment's reference is evaluated, before its
// value, through cache site site, as executeAt's case settles it.
func (r *Runtime) regCheckGlobalRef(f *frame, name Atom, site int) (bool, error) {
	cl := f.cl
	env := cl.scope()
	switch {
	case hasOwnGlobal(env, &cl.ic[site], name):
	case f.evalVars != nil && evalVarProp(f.evalVars, name).ok():
	case r.globalLexProp(env, name).ok():
	case !r.isGlobalScope(env) && r.moduleLexProp(env, name).ok():
	case r.nodeQuirks && chainHasProxy(env):
		// V8 asks a proxy on the global object's chain nothing before the
		// store, which it counts as finding the name.
	default:
		return r.hasPropErr(env, name)
	}
	return true, nil
}
