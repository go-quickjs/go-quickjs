package parser

import "github.com/go-quickjs/go-quickjs/internal/ast"

// slab hands out pointers to elements of a pre-allocated block, turning N
// individual heap allocations into one allocation per block.
//
// This is safe because a block is never grown in place: when the current block
// fills up, a fresh one is allocated and the old one is left untouched, so
// every pointer previously handed out stays valid. Appending within a block
// cannot reallocate, since len < cap is checked first.
//
// The cost is that a block stays alive as long as any single node in it is
// referenced. That suits an abstract syntax tree, whose nodes are all reachable
// from the root and are all discarded together once compilation finishes.
type slab[T any] struct {
	buf []T
	// nextCap is the capacity of the next block to allocate. Blocks grow
	// geometrically: a small script must not pay for a large block it will
	// never fill, while a large one must not pay per-node allocation costs.
	nextCap int
}

const (
	// slabMin keeps the first block cheap. Most scripts use only a few of the
	// node types, and a parse of a one-line expression should not allocate
	// kilobytes per type.
	slabMin = 8
	// slabMax bounds the waste from the final, partly-filled block.
	slabMax = 512
)

func (s *slab[T]) alloc() *T {
	if len(s.buf) == cap(s.buf) {
		switch {
		case s.nextCap == 0:
			s.nextCap = slabMin
		case s.nextCap < slabMax:
			s.nextCap *= 2
		}
		// Start a new block rather than growing the old one, so that every
		// pointer already handed out stays valid.
		s.buf = make([]T, 0, s.nextCap)
	}
	var zero T
	s.buf = append(s.buf, zero)
	return &s.buf[len(s.buf)-1]
}

// arena groups the slabs for the node types that dominate a typical parse.
// Rarer nodes -- classes, templates, switch statements -- are allocated
// individually, because a slab for them would waste more than it saves.
type arena struct {
	idents   slab[ast.Ident]
	numbers  slab[ast.NumberLit]
	strings  slab[ast.StringLit]
	binary   slab[ast.Binary]
	logical  slab[ast.Logical]
	assign   slab[ast.Assign]
	member   slab[ast.Member]
	call     slab[ast.Call]
	unary    slab[ast.Unary]
	update   slab[ast.Update]
	exprStmt slab[ast.ExprStmt]
	cond     slab[ast.Conditional]
}

func (a *arena) ident(name string, start int) *ast.Ident {
	n := a.idents.alloc()
	n.Name, n.Start = name, start
	return n
}

func (a *arena) number(value float64, start int) *ast.NumberLit {
	n := a.numbers.alloc()
	n.Value, n.Start = value, start
	return n
}

func (a *arena) str(value string, start int) *ast.StringLit {
	n := a.strings.alloc()
	n.Value, n.Start = value, start
	return n
}

func (a *arena) binaryOp(op string, left, right ast.Expr) *ast.Binary {
	n := a.binary.alloc()
	n.Op, n.Left, n.Right, n.Start = op, left, right, left.Pos()
	return n
}

func (a *arena) logicalOp(op string, left, right ast.Expr) *ast.Logical {
	n := a.logical.alloc()
	n.Op, n.Left, n.Right, n.Start = op, left, right, left.Pos()
	// Paren is reset explicitly because a recycled block may hold a stale value
	// from a previous parse only if the arena were reused; it is not, but
	// zeroing keeps the invariant local and obvious.
	n.Paren = false
	return n
}

func (a *arena) assignOp(op string, target, value ast.Expr) *ast.Assign {
	n := a.assign.alloc()
	n.Op, n.Target, n.Value, n.Start = op, target, value, target.Pos()
	return n
}

func (a *arena) memberOf(obj, prop ast.Expr, computed, optional bool) *ast.Member {
	n := a.member.alloc()
	n.Object, n.Property = obj, prop
	n.Computed, n.Optional, n.Start = computed, optional, obj.Pos()
	return n
}

func (a *arena) callOf(callee ast.Expr, args []ast.Expr, optional bool) *ast.Call {
	n := a.call.alloc()
	n.Callee, n.Args, n.Optional, n.Start = callee, args, optional, callee.Pos()
	return n
}

func (a *arena) unaryOp(op string, operand ast.Expr, start int) *ast.Unary {
	n := a.unary.alloc()
	n.Op, n.Operand, n.Start = op, operand, start
	return n
}

func (a *arena) updateOp(op string, operand ast.Expr, prefix bool, start int) *ast.Update {
	n := a.update.alloc()
	n.Op, n.Operand, n.Prefix, n.Start = op, operand, prefix, start
	return n
}

func (a *arena) exprStatement(x ast.Expr, start int) *ast.ExprStmt {
	n := a.exprStmt.alloc()
	n.X, n.Start = x, start
	return n
}

func (a *arena) conditional(test, cons, alt ast.Expr) *ast.Conditional {
	n := a.cond.alloc()
	n.Test, n.Cons, n.Alt, n.Start = test, cons, alt, test.Pos()
	return n
}

// exprStatement is a convenience wrapper so callers do not reach into the
// arena directly for the single most common statement form.
func (p *parser) exprStatement(x ast.Expr, start int) *ast.ExprStmt {
	return p.nodes.exprStatement(x, start)
}
