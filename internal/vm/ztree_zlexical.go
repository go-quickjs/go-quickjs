package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Lexical bindings' dead zone in the tree tier.
//
// A let, a const or a parameter list with defaults has its bindings start
// in their dead zone: the compiler marks a slot uninitialized
// (push_uninitialized, params_to_dead_zone), and reads and writes that may
// come before the binding is initialized are the checked kinds, which throw
// a ReferenceError there, and a TypeError for an assignment to a const,
// which assign_const always throws where the compiler knows the binding. The
// nodes here do what the interpreter does for each. Without them a function
// with a for (let ...) loop, whose binding the compiler marks before
// initializing it, ran in the interpreter, loop and all.
//
// It is in the package's last file, after ztree_zexception.go, so that its
// code sits after everything else's.

// treeBuildsDeadZone reports whether the tier builds op, one of the
// instructions of a binding's dead zone or of a parameter list's, or one
// treeBuildsIter names.
func treeBuildsDeadZone(op bytecode.Op) bool {
	switch op {
	case bytecode.OpPushUninitialized, bytecode.OpAssignConst, bytecode.OpGetLocalCheck, bytecode.OpSetLocalCheck,
		bytecode.OpGetUpvalueCheck, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue,
		bytecode.OpParamsToDeadZone, bytecode.OpInitParam, bytecode.OpParamNeedsDefault:
		return true
	}
	return treeBuildsIter(op)
}

// deadZoneOp builds the instructions treeBuildsDeadZone names, and hands
// iterOp those treeBuildsIter does. The names
// and whether a binding is mutable are the function's, which every closure
// of it shares, as the tree is.
func (b *tbuilder) deadZoneOp(pc int, in bytecode.Instr) (int, bool) {
	fn, k := b.fn, in.A
	switch in.Op {
	case bytecode.OpPushUninitialized:
		b.push(func(*tctx) Value { return uninitialized })
	case bytecode.OpAssignConst:
		// Assigning to a const is a runtime error, not an early one: the
		// assignment may sit in a function that is never called.
		return 0, b.stmt(func(c *tctx) {
			c.at(pc)
			c.throw(c.r.throwTypeError("assignment to constant variable %q", c.r.atoms.name(c.cl.names[k])))
		})
	case bytecode.OpGetLocalCheck:
		if int(k) >= len(fn.Locals) {
			return 0, false
		}
		name := fn.Locals[k].Name
		b.push(func(c *tctx) Value {
			v := c.locals[k]
			if v.IsUninitialized() {
				c.at(pc)
				c.throw(c.r.throwReferenceError("cannot access %q before initialization", name))
			}
			return v
		})
	case bytecode.OpSetLocalCheck:
		if int(k) >= len(fn.Locals) {
			return 0, false
		}
		name, mutable := fn.Locals[k].Name, fn.Locals[k].Mutable
		v := b.pop().tree()
		return 0, b.stmt(func(c *tctx) {
			x := v(c)
			if c.locals[k].IsUninitialized() {
				c.at(pc)
				c.throw(c.r.throwReferenceError("cannot access %q before initialization", name))
			}
			if !mutable {
				c.at(pc)
				c.throw(c.r.throwTypeError("assignment to constant variable %q", name))
			}
			c.locals[k] = x
		})
	case bytecode.OpGetUpvalueCheck:
		if int(k) >= len(fn.Upvalues) {
			return 0, false
		}
		name := fn.Upvalues[k].Name
		b.push(func(c *tctx) Value {
			v := c.cl.upvalues[k].get()
			if v.IsUninitialized() {
				c.at(pc)
				c.throw(c.r.throwReferenceError("cannot access %q before initialization", name))
			}
			return v
		})
	case bytecode.OpSetUpvalueCheck:
		if int(k) >= len(fn.Upvalues) {
			return 0, false
		}
		name, mutable := fn.Upvalues[k].Name, fn.Upvalues[k].Mutable
		v := b.pop().tree()
		return 0, b.stmt(func(c *tctx) {
			x := v(c)
			u := c.cl.upvalues[k]
			if u.get().IsUninitialized() {
				c.at(pc)
				c.throw(c.r.throwReferenceError("cannot access %q before initialization", name))
			}
			if !mutable {
				c.at(pc)
				c.throw(c.r.throwTypeError("assignment to constant variable %q", name))
			}
			u.set(x)
		})
	case bytecode.OpInitUpvalue:
		v := b.pop().tree()
		return 0, b.stmt(func(c *tctx) { c.cl.upvalues[k].set(v(c)) })
	case bytecode.OpParamsToDeadZone:
		// The parameters of such a list are bound one at a time, so they
		// all start in the dead zone.
		return 0, b.stmt(func(c *tctx) {
			for i := range min(int(k), len(c.locals)) {
				c.locals[i] = uninitialized
			}
		})
	case bytecode.OpInitParam:
		// A parameter's slot is its position, so the argument that fills it
		// is at the same index.
		return 0, b.stmt(func(c *tctx) {
			if int(k) < len(c.f.args) {
				c.locals[k] = c.f.args[k]
			} else {
				c.locals[k] = Undefined
			}
		})
	case bytecode.OpParamNeedsDefault:
		b.push(func(c *tctx) Value { return Bool(int(k) >= len(c.f.args) || c.f.args[k].IsUndefined()) })
	default:
		return b.iterOp(pc, in)
	}
	return 0, true
}
