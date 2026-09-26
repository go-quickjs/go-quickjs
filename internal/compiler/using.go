package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Using declarations.
//
// A scope with using declarations disposes of the values they bound when it
// is left, however it is left. That is what a finally clause is for, so the
// scope is compiled as one: a hidden binding holds the scope's stack of
// resources, the scope's own statements are the protected block, and the
// clause disposes of the stack.
//
// Compiled as the clause proper, the disposal knows from the completion record
// whether an exception is leaving the scope, and folds it into anything the
// disposal throws. A break or continue compiles the clause again where it
// jumps, with no record and no exception to fold. Either way an error from
// disposing is thrown, which is the specification's rule that it replaces
// whatever the scope was completing with.
//
// An `await using` makes the whole scope's disposal asynchronous, since the
// resources are disposed of in one walk: the clause awaits a promise of it.

// disposeScope is the innermost scope a using declaration adds its value to.
type disposeScope struct {
	slot uint32
}

// usingKind reports whether a statement list declares anything with using,
// and whether any of it is `await using`.
func usingKind(body []ast.Stmt) (has, async bool) {
	for _, s := range body {
		if vd, ok := s.(*ast.VarDecl); ok && vd.Kind.IsUsing() {
			has = true
			async = async || vd.Kind == ast.DeclAwaitUsing
		}
	}
	return has, async
}

// compileDisposeScope compiles body as a scope whose resources are disposed of
// when it is left. The bindings are the enclosing scope's: body runs in it
// rather than in a scope of its own.
func (c *compiler) compileDisposeScope(pos int, async bool, body func()) {
	name := fmt.Sprintf("%%dispose%d", *c.hiddenCount)
	*c.hiddenCount++
	slot := c.declare(name, bindVar, pos)
	c.emit(bytecode.OpNewDisposeCapability, 0, 0)
	c.emit(bytecode.OpSetLocal, slot, 0)

	c.disposeScopes = append(c.disposeScopes, disposeScope{slot: slot})
	c.compileTryFinally(pos, func() {
		body()
	}, []ast.Stmt{&ast.DisposeStmt{Capability: name, Async: async, Start: pos}})
	c.disposeScopes = c.disposeScopes[:len(c.disposeScopes)-1]
}

// compileDispose compiles the disposal of a scope's resources.
func (c *compiler) compileDispose(n *ast.DisposeStmt) {
	l, ok := c.resolveLocal(n.Capability)
	if !ok {
		c.errorf(n.Start, "internal error: no resources to dispose of")
		return
	}
	c.emit(bytecode.OpGetLocal, l.slot, 0)
	flags := uint32(0)
	if c.finallyRecord {
		flags |= bytecode.DisposeRecord
	}
	if n.Async {
		flags |= bytecode.DisposeAsync
	}
	c.emitAt(n.Start, bytecode.OpDispose, flags, 0)
	if n.Async {
		// The disposal leaves undefined rather than a promise when there was
		// nothing to await: no `await using` was reached.
		skip := c.emitJump(bytecode.OpJumpIfNullish)
		c.emitAwait(n.Start)
		c.patchJump(skip)
		c.emit(bytecode.OpDrop, 0, 0)
	}
}

// addDisposable adds the value on top of the stack, which stays there, to the
// innermost scope's resources.
func (c *compiler) addDisposable(kind ast.DeclKind, pos int) {
	if len(c.disposeScopes) == 0 {
		c.errorf(pos, "internal error: a using declaration outside a scope that disposes")
		return
	}
	async := uint32(0)
	if kind == ast.DeclAwaitUsing {
		async = 1
	}
	c.emit(bytecode.OpGetLocal, c.disposeScopes[len(c.disposeScopes)-1].slot, 0)
	c.emitAt(pos, bytecode.OpAddDisposable, async, 0)
}
