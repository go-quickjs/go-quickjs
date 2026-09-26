package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Tail calls.
//
// Strict code promises that a call in tail position -- the last thing a
// function does before returning what the call returns -- does not keep the
// caller's frame alive, so that recursion through tail calls runs in constant
// space. The compiler finds those calls and emits them as tail calls; the
// interpreter gives up the frame when it can and makes an ordinary call when
// it cannot, which is always safe, since a return follows either way.
//
// A call is in tail position when it is what a return statement returns,
// directly or as the branch of a conditional, the right of a logical operator
// or the last of a comma expression -- and when nothing is left to run after
// it: no handler around it to catch what it throws, no finally clause to run,
// and no iterator of a for-in or for-of to close. A generator or an async
// function has no tail calls, since its frame outlives any one call, and nor
// does a class constructor, whose result has to be checked.

// tailCallsAllowed reports whether a return compiled here may make a tail call.
func (c *compiler) tailCallsAllowed() bool {
	fn := c.fn
	if !fn.Strict || fn.Generator || fn.Async || fn.IsModule ||
		fn.Kind == bytecode.KindConstructor || fn.Kind == bytecode.KindDerivedConstructor {
		return false
	}
	if c.handlerDepth > 0 || len(c.finallys) > 0 {
		return false
	}
	for _, e := range c.exits {
		if e == exitCursor {
			return false
		}
	}
	return true
}

// compileTailExpr compiles the argument of a return statement, emitting the
// calls in tail position within it as tail calls.
func (c *compiler) compileTailExpr(e ast.Expr) {
	switch n := e.(type) {
	case *ast.Call, *ast.TaggedTemplate:
		saved := c.tailCall
		c.tailCall = e
		c.compileExpr(e)
		c.tailCall = saved
	case *ast.Conditional:
		elseJump := c.emitTestJumpIfFalse(n.Test)
		c.compileTailExpr(n.Cons)
		endJump := c.emitJump(bytecode.OpJump)
		c.patchJump(elseJump)
		c.stackDepth--
		c.compileTailExpr(n.Alt)
		c.patchJump(endJump)
	case *ast.Logical:
		c.compileExpr(n.Left)
		var jump int
		switch n.Op {
		case "&&":
			jump = c.emitJump(bytecode.OpJumpIfFalseKeep)
		case "||":
			jump = c.emitJump(bytecode.OpJumpIfTrueKeep)
		default: // "??"
			jump = c.emitJump(bytecode.OpJumpIfNotNullish)
		}
		c.compileTailExpr(n.Right)
		c.patchJump(jump)
	case *ast.Sequence:
		for i, x := range n.Exprs {
			if i < len(n.Exprs)-1 {
				c.compileExprForEffect(x)
				continue
			}
			c.compileTailExpr(x)
		}
	default:
		c.compileExpr(e)
	}
}
