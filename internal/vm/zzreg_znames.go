package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regColdNames is the cold operations of names the register form reaches
// as the stack form's instructions reach them.
func (r *Runtime) regColdNames(f *frame, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdGetUpvalCheck, bytecode.RColdSetUpvalCheck:
		// As get_upvalue_check and set_upvalue_check: the dead zone
		// outranks constness.
		i := w.BC()
		u := cl.fn.Upvalues[i]
		if cl.upvalues[i].get().IsUninitialized() {
			return r.throwReferenceError("cannot access %q before initialization", u.Name)
		}
		if bytecode.RColdOp(w.X()) == bytecode.RColdGetUpvalCheck {
			regs[w.A()] = cl.upvalues[i].get()
			return nil
		}
		if !u.Mutable {
			return r.throwTypeError("assignment to constant variable %q", u.Name)
		}
		cl.upvalues[i].set(regs[w.A()])
		return nil
	}
	panic("register code: no cold operation " + bytecode.RColdOp(w.X()).String())
}
