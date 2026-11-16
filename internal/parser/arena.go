package parser

import (
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/ast"
)

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
	// A block is zero from its make to its capacity.
	n := len(s.buf)
	s.buf = s.buf[:n+1]
	return &s.buf[n]
}

// lists builds the lists of a syntax tree -- arguments, statements,
// declarators -- on a scratch stack, and hands each finished one out as an
// exactly-sized slice of a shared block: a list costs neither the
// reallocations of appending to it nor an allocation of its own.
//
// A list begun inside another, an argument's own arguments, is finished
// before the outer one goes on, so the stack only ever grows and shrinks at
// its top. A finished list's capacity is its length, so appending to it
// later copies it rather than writing over the list after it.
type lists[T any] struct {
	scratch []T
	buf     []T
	nextCap int
}

const (
	listMin = 64
	listMax = 4096
)

// begin starts a list, returning where it begins on the stack.
func (l *lists[T]) begin() int { return len(l.scratch) }

// add appends an item to the list being built.
func (l *lists[T]) add(x T) {
	if len(l.scratch) == cap(l.scratch) {
		l.grow()
	}
	l.scratch = append(l.scratch, x)
}

// grow doubles the stack. append would grow a large one by a quarter, and
// have copied it four or five times over by the time it is as long as a
// generated program's longest function.
func (l *lists[T]) grow() {
	l.scratch = append(make([]T, 0, max(2*cap(l.scratch), listMin)), l.scratch...)
}

// finish ends the list begun at mark, and returns it; nil for none.
func (l *lists[T]) finish(mark int) []T {
	items := l.scratch[mark:]
	n := len(items)
	if n == 0 {
		return nil
	}
	if cap(l.buf)-len(l.buf) < n {
		if l.nextCap < listMax {
			l.nextCap = max(l.nextCap*2, listMin)
		}
		l.buf = make([]T, 0, max(n, l.nextCap))
	}
	start := len(l.buf)
	l.buf = append(l.buf, items...)
	clear(items)
	l.scratch = l.scratch[:mark]
	return l.buf[start:len(l.buf):len(l.buf)]
}

// listScratch is a parse's lists' scratch stacks, kept from one parse for
// the next (listScratchPool), so that each parse does not grow them from
// nothing: what a parse hands out is copied out of them (finish), so
// nothing of a syntax tree is in them.
type listScratch struct {
	exprs []ast.Expr
	stmts []ast.Stmt
	decls []ast.Declarator
	props []ast.Property
}

// listScratchPool holds the scratch stacks no parse has. Separate
// runtimes parse at once, each parse with stacks of its own.
var listScratchPool = sync.Pool{New: func() any { return new(listScratch) }}

// takeScratch gives a's lists scratch stacks from the pool.
func (a *arena) takeScratch() {
	s := listScratchPool.Get().(*listScratch)
	a.exprs.scratch, a.stmts.scratch = s.exprs, s.stmts
	a.decls.scratch, a.props.scratch = s.decls, s.props
	a.scratch = s
}

// giveScratch gives a's lists' scratch stacks back to the pool, emptied:
// a parse that stopped at an error leaves lists unfinished on them.
func (a *arena) giveScratch() {
	s := a.scratch
	clear(a.exprs.scratch)
	clear(a.stmts.scratch)
	clear(a.decls.scratch)
	clear(a.props.scratch)
	s.exprs, s.stmts = a.exprs.scratch[:0], a.stmts.scratch[:0]
	s.decls, s.props = a.decls.scratch[:0], a.props.scratch[:0]
	a.exprs.scratch, a.stmts.scratch, a.decls.scratch, a.props.scratch = nil, nil, nil, nil
	a.scratch = nil
	listScratchPool.Put(s)
}

// truncate abandons whatever lists were begun past mark, which a parse that
// is rewound after an error, and retried another way, leaves unfinished.
func (l *lists[T]) truncate(mark int) {
	if len(l.scratch) > mark {
		clear(l.scratch[mark:])
		l.scratch = l.scratch[:mark]
	}
}

// arena groups the slabs for the node types that dominate a typical parse.
// Rarer nodes -- classes, templates, switch statements -- are allocated
// individually, because a slab for them would waste more than it saves.
type arena struct {
	idents slab[ast.Ident]
	// nIdents is how many identifiers the function being parsed has
	// numbered (ast.Ident.ID).
	nIdents  uint32
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
	news     slab[ast.New]
	varDecls slab[ast.VarDecl]
	returns  slab[ast.ReturnStmt]
	blocks   slab[ast.BlockStmt]
	ifs      slab[ast.IfStmt]
	arrays   slab[ast.ArrayLit]
	objects  slab[ast.ObjectLit]
	thises   slab[ast.This]
	bools    slab[ast.BoolLit]

	exprs lists[ast.Expr]
	stmts lists[ast.Stmt]
	decls lists[ast.Declarator]
	props lists[ast.Property]
	// scratch is where the lists' scratch stacks came from (takeScratch).
	scratch *listScratch
}

func (a *arena) ident(name string, start int) *ast.Ident {
	n := a.name(name, start)
	a.numberIdent(n)
	return n
}

// name is an identifier that names a property -- `a.b`'s b, `{b: 1}`'s --
// which nothing resolves, and so is given no number (ast.Ident.ID) until
// it is found to be a reference too, as a shorthand property's is.
func (a *arena) name(name string, start int) *ast.Ident {
	n := a.idents.alloc()
	n.Name, n.Start = name, int32(start)
	return n
}

// numberIdent gives n the next number of the function being parsed.
func (a *arena) numberIdent(n *ast.Ident) {
	a.nIdents++
	n.SetID(a.nIdents)
}

// beginIdents starts the numbering of a function's identifiers
// (ast.Ident.ID), and returns the numbering of the code around it, which
// endIdents resumes.
func (a *arena) beginIdents() uint32 {
	outer := a.nIdents
	a.nIdents = 0
	return outer
}

// endIdents ends a function's numbering, resuming outer, and returns how
// many identifiers it numbered.
func (a *arena) endIdents(outer uint32) uint32 {
	n := a.nIdents
	a.nIdents = outer
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
	n.Op, n.Left, n.Right, n.Start = op, left, right, int32(left.Pos())
	// Paren is reset explicitly because a recycled block may hold a stale value
	// from a previous parse only if the arena were reused; it is not, but
	// zeroing keeps the invariant local and obvious.
	n.Paren = false
	return n
}

func (a *arena) assignOp(op string, target, value ast.Expr) *ast.Assign {
	n := a.assign.alloc()
	n.Op, n.Target, n.Value, n.Start = op, target, value, int32(target.Pos())
	return n
}

func (a *arena) memberOf(obj, prop ast.Expr, computed, optional bool) *ast.Member {
	n := a.member.alloc()
	n.Object, n.Property = obj, prop
	n.Computed, n.Optional, n.Start = computed, optional, int32(obj.Pos())
	return n
}

func (a *arena) callOf(callee ast.Expr, args []ast.Expr, optional bool, open int) *ast.Call {
	n := a.call.alloc()
	n.Callee, n.Args, n.Optional, n.Start, n.Open = callee, args, optional, int32(callee.Pos()), int32(open)
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
