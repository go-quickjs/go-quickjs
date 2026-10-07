package compiler

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Code compiled for a debugger.
//
// Each statement a debugger can stop at begins with an OpDebugStmt, and the
// function's Statements say where it is and what is in scope there: the
// bindings a debugger shows, and evaluates code against as a direct eval at
// that point would. Nothing else differs, but that an instruction between
// two statements keeps the two from being fused into one.

// debugStatement begins a statement a debugger can stop at.
func (c *compiler) debugStatement(s ast.Stmt) {
	var debugger uint32
	switch n := s.(type) {
	case *ast.FuncDecl, *ast.BlockStmt, *ast.EmptyStmt, *ast.LabeledStmt,
		*ast.InstallPrivateMethods, *ast.FieldInit, *ast.ImportDecl, *ast.ExportDecl:
		// Nothing runs where these are written: a function declaration is
		// hoisted, a block or a label is the statements inside it, and an
		// import or export declaration is bound before the module runs.
		return
	case *ast.VarDecl:
		if n.Kind == ast.DeclVar && !slices.ContainsFunc(n.Decls, func(d ast.Declarator) bool { return d.Init != nil }) {
			// `var x;` does nothing where it is written.
			return
		}
	case *ast.DebuggerStmt:
		debugger = 1
	}
	c.debugMark(s.Pos(), debugger)
}

// debugExpr is a place a debugger can stop that is not a statement: a
// loop's test, or a for loop's update, which a step through a loop stops at
// each time round.
func (c *compiler) debugExpr(e ast.Expr) {
	if c.opts.Debug && e != nil {
		c.debugMark(e.Pos(), 0)
	}
}

// debugAt is a place a debugger can stop at pos: a for-in or for-of head,
// where each time round the loop takes its next value.
func (c *compiler) debugAt(pos int) {
	if c.opts.Debug {
		c.debugMark(pos, 0)
	}
}

// debugMark emits an OpDebugStmt for a place at pos.
func (c *compiler) debugMark(pos int, debugger uint32) {
	c.fn.Debug.Statements = append(c.fn.Debug.Statements, bytecode.Statement{
		PC: uint32(len(c.fn.Code)), Pos: int32(pos), Scope: c.debugScope(),
	})
	c.recordLine(pos)
	c.emit(bytecode.OpDebugStmt, uint32(len(c.fn.Debug.Statements)-1), debugger)
}

// debugScope records what is in scope at the current position, and returns
// its index in the function's EvalScopes: the last statement's, where
// nothing has changed since.
func (c *compiler) debugScope() uint32 {
	scope := c.evalScope()
	if n := len(c.fn.Debug.Statements); n > 0 {
		last := c.fn.Debug.Statements[n-1].Scope
		if sameEvalScope(&c.fn.EvalScopes[last], &scope) {
			return last
		}
	}
	c.fn.EvalScopes = append(c.fn.EvalScopes, scope)
	return uint32(len(c.fn.EvalScopes) - 1)
}

// sameEvalScope reports whether two scopes are alike in everything.
func sameEvalScope(a, b *bytecode.EvalScope) bool {
	return slices.Equal(a.Bindings, b.Bindings) && a.WithDepth == b.WithDepth &&
		a.Strict == b.Strict && a.AllowSuperProp == b.AllowSuperProp &&
		a.AllowSuperCall == b.AllowSuperCall && a.AllowNewTarget == b.AllowNewTarget &&
		a.VarScopeIsGlobal == b.VarScopeIsGlobal && a.InFieldInit == b.InFieldInit &&
		slices.Equal(a.PrivateNames, b.PrivateNames) && slices.Equal(a.ArgumentNames, b.ArgumentNames)
}
