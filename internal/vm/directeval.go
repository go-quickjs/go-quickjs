package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Direct eval.
//
// `eval(src)` written as a plain call runs its code in the caller's scope: it
// reads and writes the caller's variables, and sees the caller's `this`,
// `new.target` and `super`. Calling the same function through anything else --
// `(0, eval)(src)`, or a variable holding it -- runs the code in global scope,
// which is what makes the indirect form the way to ask for a sandbox.
//
// The evaluated code is compiled as a function whose upvalues are the caller's
// bindings it actually used, and linked to the calling frame. That is what
// upvalues already do for an ordinary closure, so nothing else in the
// interpreter changes and no function pays anything for the possibility of an
// eval appearing in it.

// EvalRequest describes the code to evaluate and the scope it runs in.
type EvalRequest struct {
	// Direct is false for an indirect eval and for the Function constructor,
	// where none of the rest applies.
	Direct bool
	Scope  bytecode.EvalScope
}

// Evaluator compiles source text, which eval and the Function constructor need.
//
// It is supplied by the host rather than implemented here, because the vm
// package does not import the parser or the compiler. It compiles rather than
// runs: linking the result to the calling frame is this package's business.
type Evaluator func(source string, req EvalRequest) (*bytecode.Function, error)

// evalDirect runs a direct eval's code in the caller's scope.
func (r *Runtime) evalDirect(caller *frame, scope bytecode.EvalScope, src string) (Value, error) {
	if r.evaluator == nil {
		return Undefined, r.throwTypeError("code generation from strings is disabled")
	}
	fn, err := r.evaluator(src, EvalRequest{Direct: true, Scope: scope})
	if err != nil {
		return Undefined, r.wrapEvalError(err)
	}

	cl := r.prepare(fn)
	// Each upvalue the code asked for names one of the caller's bindings. A
	// local is captured from the live frame, which is what lets the evaluated
	// code write to it; an upvalue of the caller is shared directly.
	cl.upvalues = make([]*upvalue, len(fn.Upvalues))
	for i, desc := range fn.Upvalues {
		if desc.FromParent {
			cl.upvalues[i] = r.captureLocal(caller, int(desc.Index))
			continue
		}
		if caller.cl != nil && int(desc.Index) < len(caller.cl.upvalues) {
			cl.upvalues[i] = caller.cl.upvalues[desc.Index]
			continue
		}
		// Nothing to bind to, which the compiler should have prevented; an
		// unbound box reads as undefined rather than crashing.
		cl.upvalues[i] = &upvalue{}
		cl.upvalues[i].slot = &cl.upvalues[i].closed
	}
	cl.env = caller.cl.env

	// The code shares the caller's `this`, `new.target`, `arguments` and home
	// object, which is what an arrow does -- so it is run as one.
	callee := newObject(r.proto.function, ClassFunction)
	fd := &funcData{
		closure:      cl,
		name:         "eval",
		arrow:        true,
		lexThis:      caller.this,
		lexThisRef:   caller.thisRef,
		lexNewTarget: caller.newTarget,
		lexArgs:      caller.args,
		lexWith:      caller.withScopes,
	}
	if caller.callee != nil {
		if outer := caller.callee.fn(); outer != nil {
			fd.homeObject = outer.homeObject
			fd.parentCtor = outer.parentCtor
			if outer.arrow {
				fd.lexArgs = outer.lexArgs
			}
		}
	}
	callee.data = fd
	return r.run(cl, caller.this, caller.args, caller.newTarget, callee)
}

// evalIndirect runs code in global scope, which is what eval reached through
// anything but a plain call does, and what the Function constructor does.
func (r *Runtime) evalIndirect(src string) (Value, error) {
	if r.evaluator == nil {
		return Undefined, r.throwTypeError("code generation from strings is disabled")
	}
	fn, err := r.evaluator(src, EvalRequest{})
	if err != nil {
		return Undefined, r.wrapEvalError(err)
	}
	return r.Run(fn)
}
