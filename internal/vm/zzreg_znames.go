package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regUpvalCheck is get_upval_check, set_upval_check.
func (r *Runtime) regUpvalCheck(f *frame, w bytecode.RInstr, regs *[1 << 16]Value) error {
	cl := f.cl
	// As get_upvalue_check and set_upvalue_check: the dead zone
	// outranks constness.
	i := w.BC()
	u := cl.fn.Upvalues[i]
	if cl.upvalues[i].get().IsUninitialized() {
		return r.throwReferenceError("cannot access %q before initialization", u.Name)
	}
	if w.Op() == bytecode.RGetUpvalCheck {
		regs[w.A()] = cl.upvalues[i].get()
		return nil
	}
	if !u.Mutable {
		return r.throwTypeError("assignment to constant variable %q", u.Name)
	}
	cl.upvalues[i].set(regs[w.A()])
	return nil
}
