package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
	c.emitAt(n.Start, bytecode.OpWithPush, 0, 0)
	c.pushExit(exitWith)
	c.withDepth++
	// The body is a block of its own, so a let inside it does not escape.
	c.beginScope()
	if b, ok := n.Body.(*ast.BlockStmt); ok {
		c.compileStatements(b.Body)
	} else {
		c.compileStatement(n.Body)
	}
	c.endScope()
	c.withDepth--
	c.popExit()
	c.emit(bytecode.OpWithPop, 0, 0)
}

// withProbe emits the probe that precedes a name's static instruction, and
// returns the jump to patch once that instruction has been emitted.
//
// It returns -1 where there is nothing to probe: outside a `with` body, or for
// a name whose binding was declared inside every enclosing one, which no
// `with` object can shadow.
//
// The operand packs the name index with how many of the innermost objects to
// consult, since a binding declared between two `with` statements is shadowed
// by the inner one and not by the outer.
func (c *compiler) withProbe(op bytecode.Op, name string) int {
	limit := c.withLimit(name)
	if limit == 0 {
		return -1
	}
	return c.emit(op, c.nameIdx(name)|uint32(limit)<<bytecode.WithLimitShift, 0xFFFFFFFF)
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

// patchWithProbe points a probe past the static instruction it guards.
func (c *compiler) patchWithProbe(pc int) {
	if pc < 0 {
		return
	}
	c.fn.Code[pc].B = uint32(len(c.fn.Code))
}

// beginWithRef starts a reference that will be read and then written back, for
// a name an enclosing `with` object may answer.
//
// The reference is resolved once. The specification reads and writes through
// the same reference, so a getter that deletes the property while the read is
// running still leaves the write on the object rather than on the binding the
// name would otherwise mean.
//
// What it leaves on the stack is a base beneath the value read: the object the
// name resolved to, or undefined when it resolved to a binding. endWithRef
// reads that to decide where the write goes.
func (c *compiler) beginWithRef(n *ast.Ident) {
	limit := c.withLimit(n.Name)
	c.emit(bytecode.OpPushUndef, 0, 0)
	probe := c.emit(bytecode.OpWithGetUnder,
		c.nameIdx(n.Name)|uint32(limit)<<bytecode.WithLimitShift, 0xFFFFFFFF)
	c.compileIdentReadStatic(n)
	c.patchWithProbe(probe)
}

// resolveWithRef settles a reference without reading it, which is what a plain
// assignment needs: the name is resolved before the value is evaluated, but
// nothing is read from the binding it names.
func (c *compiler) resolveWithRef(n *ast.Ident) {
	limit := c.withLimit(n.Name)
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpWithResolve,
		c.nameIdx(n.Name)|uint32(limit)<<bytecode.WithLimitShift, 0)
}

// endWithRef writes the value on top back through the reference beginWithRef
// resolved, leaving the value and removing the base.
func (c *compiler) endWithRef(n *ast.Ident) {
	probe := c.emit(bytecode.OpWithPutUnder, c.nameIdx(n.Name), 0xFFFFFFFF)
	// The read came from a binding, so the base goes and the static store
	// stands in for the write.
	c.emit(bytecode.OpNipUnder, 1, 0)
	c.assignToIdentStatic(n, false)
	c.patchWithProbe(probe)
}
