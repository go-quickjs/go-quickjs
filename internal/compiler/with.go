package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)

// The `with` statement.
//
// Inside a `with` body an unqualified name may resolve to a property of the
// object rather than to any binding, and which it is depends on what the object
// holds at the moment the name is evaluated. Nothing else in the language works
// that way, which is why `with` is forbidden in strict mode.
//
// The cost is confined to the body. Every name used inside one compiles to a
// probe followed by the instruction that would have been emitted anyway: the
// probe either answers from the enclosing `with` objects and jumps over the
// static instruction, or falls through to it. Code outside a `with` is
// unchanged.

// compileWith compiles `with (obj) body`.
func (c *compiler) compileWith(n *ast.WithStmt) {
	if c.fn.Strict {
		c.errorf(n.Start, "\"with\" is not allowed in strict mode")
		return
	}
	c.compileExpr(n.Object)
	c.pushExit(exitWith)
	c.withDepth++
	c.withStatements++
	// The body is a block of its own, so a let inside it does not escape.
	c.beginScope()
	if b, ok := n.Body.(*ast.BlockStmt); ok {
		c.compileStatements(b.Body)
	} else {
		c.compileStatement(n.Body)
	}
	c.endScope()
	c.withDepth--
	c.withStatements--
	c.popExit()
}

// withProbe notes the probe op that precedes a name's static access (regWith),
// and returns its handle, 0.
//
// It returns -1 where there is nothing to probe: outside a `with` body, or for
// a name whose binding was declared inside every enclosing one, which no
// `with` object can shadow.
//
// A probe consults the innermost objects as many as withLimit says, since a
// binding declared between two `with` statements is shadowed by the inner one
// and not by the outer.
func (c *compiler) withProbe(op bytecode.Op, name string) int {
	limit := c.withLimit(name)
	if limit == 0 {
		return -1
	}
	return c.emitProbe(op, name, limit, 0xFFFFFFFF)
}

// emitProbe notes the probe op of the innermost limit with objects for
// name, for the register form (regWith), and returns 0, a probe's handle.
func (c *compiler) emitProbe(op bytecode.Op, name string, limit int, b uint32) int {
	c.probe = regcomp.WithProbe{Op: op, Limit: uint16(limit)}
	return 0
}

// withLimit is how many of the innermost `with` objects a reference to a name
// has to be probed against.
func (c *compiler) withLimit(name string) int {
	limit := c.withDepth
	switch {
	case limit == 0:
		return 0
	case name == c.selfName:
		// A named function expression's own name is a binding of the function,
		// which is inside every `with` its body is inside.
		return 0
	}
	if l, ok := c.resolveLocal(name); ok {
		limit -= l.withDepth
	} else if idx, ok := c.resolveUpvalue(name); ok {
		limit -= c.fn.Upvalues[idx].WithDepth
	}
	switch {
	case limit < 0:
		return 0
	case limit > bytecode.WithLimitMax:
		return bytecode.WithLimitMax
	}
	return limit
}

// checkGlobalRef emits the check that a name is resolvable, which strict mode
// requires before assigning to one.
//
// Only a name that resolves to nothing the compiler can see needs it: a local,
// an upvalue or a binding of the module's own environment is resolvable by
// construction, and sloppy code creates the global rather than refusing.
func (c *compiler) checkGlobalRef(n *ast.Ident) bool {
	if !c.fn.Strict {
		return false
	}
	if _, ok := c.resolveLocal(n.Name); ok {
		return false
	}
	if _, ok := c.resolveUpvalue(n.Name); ok {
		return false
	}
	if c.globalLex[n.Name] || c.isModuleLex(n.Name) || c.withLimit(n.Name) > 0 {
		return false
	}
	return true
}

// beginWithRef starts a reference that will be read and then written back, for
// a name an enclosing `with` object may answer.
//
// The reference is resolved once. The specification reads and writes through
// the same reference, so a getter that deletes the property while the read is
// running still leaves the write on the object rather than on the binding the
// name would otherwise mean.
//
// What the read resolved to -- an object, or a binding -- is where endWithRef's
// write goes.
func (c *compiler) beginWithRef(n *ast.Ident) {
	limit := c.withLimit(n.Name)
	probe := c.emitProbe(bytecode.OpWithGetUnder, n.Name, limit, 0xFFFFFFFF)
	c.regWith(n, probe)
	c.compileIdentReadStatic(n)
}

// resolveWithRef settles a reference without reading it, which is what a plain
// assignment needs: the name is resolved before the value is evaluated, but
// nothing is read from the binding it names.
func (c *compiler) resolveWithRef(n *ast.Ident) {
	limit := c.withLimit(n.Name)
	c.regWith(n, c.emitProbe(bytecode.OpWithResolve, n.Name, limit, 0))
}

// endWithRef walks the write back through the reference beginWithRef
// resolved: the static store, where the read came from a binding.
func (c *compiler) endWithRef(n *ast.Ident) {
	c.assignToIdentStatic(n, false)
}
