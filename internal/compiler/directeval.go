package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Direct eval.
//
// `eval(src)` written as a plain call is not the same thing as calling the same
// function through a variable. A direct eval runs its code in the caller's
// scope: it can read and write the caller's variables, and it sees the caller's
// `this`, `new.target` and `super`. An indirect one runs in global scope, which
// is what makes `(0, eval)(src)` the way to ask for a sandbox.
//
// The compiler cannot know what the evaluated code will refer to, so at each
// direct call site it records everything in scope. The evaluated code is then
// compiled as a function whose upvalues are the names it actually used, and the
// interpreter binds those upvalues to the calling frame's slots -- which is
// what upvalues are for, and means no ordinary function pays anything for the
// possibility of an eval appearing.

// isDirectEval reports whether a call is written as a direct eval.
//
// It is a syntactic question: a call whose callee is the name `eval`. Whether
// that name resolves to the intrinsic is decided when the call runs, because
// only then is it known.
func isDirectEval(n *ast.Call) bool {
	id, ok := n.Callee.(*ast.Ident)
	return ok && id.Name == "eval"
}

// compileDirectEval compiles a call that may turn out to be a direct eval.
func (c *compiler) compileDirectEval(n *ast.Call) {
	idx := c.evalScopeIdx()
	c.compileExpr(n.Callee)
	argc := c.compileArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpDirectEval, idx, uint32(argc))
}

// evalScopeIdx records what is in scope at the current position and returns its
// index in the function's table.
func (c *compiler) evalScopeIdx() uint32 {
	scope := bytecode.EvalScope{
		Bindings:       c.visibleBindings(),
		Strict:         c.fn.Strict,
		AllowSuperProp: c.allowSuperProp(),
		AllowSuperCall: c.allowSuperCall(),
		AllowNewTarget: c.fn.Kind != bytecode.KindNormal || c.parent != nil,
		PrivateNames:   c.visiblePrivateNames(),
	}
	c.fn.EvalScopes = append(c.fn.EvalScopes, scope)
	return uint32(len(c.fn.EvalScopes) - 1)
}

// visibleBindings lists every name the evaluated code could reach, innermost
// first, so that a shadowed one is never the answer.
//
// A local of this function is named by its slot; a binding of an enclosing one
// is captured as an upvalue here first, because that is how the interpreter can
// reach it once the enclosing frame has returned.
func (c *compiler) visibleBindings() []bytecode.EvalBinding {
	seen := make(map[string]bool, len(c.locals))
	var out []bytecode.EvalBinding

	for i := len(c.locals) - 1; i >= 0; i-- {
		l := &c.locals[i]
		if l.name == "" || seen[l.name] {
			continue
		}
		seen[l.name] = true
		// The evaluated code may write to it from a closure of its own, so it
		// has to be boxed like any other captured binding.
		l.captured = true
		out = append(out, bytecode.EvalBinding{
			Name:      l.name,
			FromLocal: true,
			Index:     l.slot,
			Mutable:   l.kind != bindConst,
			TDZ:       !l.initialized,
		})
	}

	capture := func(name string) {
		if name == "" || seen[name] {
			return
		}
		idx, ok := c.resolveUpvalue(name)
		if !ok {
			return
		}
		seen[name] = true
		out = append(out, bytecode.EvalBinding{
			Name:    name,
			Index:   idx,
			Mutable: c.fn.Upvalues[idx].Mutable,
			TDZ:     c.fn.Upvalues[idx].TDZ,
		})
	}

	root := c
	for p := c.parent; p != nil; p = p.parent {
		for i := len(p.locals) - 1; i >= 0; i-- {
			capture(p.locals[i].name)
		}
		root = p
	}
	// The code of a direct eval is itself a function whose upvalues are its
	// caller's bindings, so an eval written inside one -- at any depth -- has
	// to reach those too: eval("eval(\"x\")") names the same x as eval("x").
	for _, b := range root.opts.EvalScope {
		capture(b.Name)
	}
	return out
}

// visiblePrivateNames lists the private names of the enclosing classes, which
// evaluated code may refer to exactly as the surrounding code may.
func (c *compiler) visiblePrivateNames() []string {
	var out []string
	for s := c; s != nil; s = s.parent {
		for _, scope := range s.privateScopes {
			out = append(out, scope...)
		}
	}
	return out
}

// allowSuperProp reports whether `super.x` is legal at the current position,
// which is what the evaluated code inherits.
func (c *compiler) allowSuperProp() bool {
	switch c.fn.Kind {
	case bytecode.KindMethod, bytecode.KindGetter, bytecode.KindSetter,
		bytecode.KindConstructor, bytecode.KindDerivedConstructor,
		bytecode.KindClassFieldInit, bytecode.KindStaticBlock:
		return true
	case bytecode.KindArrow:
		return c.parent != nil && c.parent.allowSuperProp()
	}
	return false
}

// allowSuperCall reports whether `super()` is legal at the current position.
func (c *compiler) allowSuperCall() bool {
	if c.inFieldInit {
		// A field initializer is inside the constructor but is not it: there is
		// only one constructor, and it is not this.
		return false
	}
	switch c.fn.Kind {
	case bytecode.KindDerivedConstructor:
		return true
	case bytecode.KindArrow:
		return c.parent != nil && c.parent.allowSuperCall()
	}
	return false
}
