package parser

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/fold"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// unaryOf builds op of operand -- !, ~, + or - -- or, where operand is a
// literal the operator folds (fold.Unary), the literal of its value, as
// V8's parser does (Parser::BuildUnaryExpression): ! of any literal, -, +
// and ~ of a number, and - of a BigInt. The operand was folded as it was
// built, so that a nest of them folds from the bottom up, once each.
func (p *parser) unaryOf(op string, operand ast.Expr, start int) ast.Expr {
	switch op {
	case "!":
		if t, ok := fold.Not(operand); ok {
			n := p.nodes.bools.alloc()
			n.Value, n.Start = t, start
			return n
		}
	case "-", "+", "~":
		if !fold.On(fold.Unary) {
			break
		}
		switch x := operand.(type) {
		case *ast.NumberLit:
			v := x.Value
			switch op {
			case "-":
				v = -v
			case "~":
				v = fold.BitNot(v)
			}
			return p.nodes.number(v, start)
		case *ast.BigIntLit:
			if op == "-" {
				if raw, ok := fold.Negate(x.Raw); ok {
					return &ast.BigIntLit{Raw: raw, Start: start}
				}
			}
		}
	}
	return p.nodes.unaryOp(op, operand, start)
}

// binaryOf builds op of left and right, or, where both are numbers and op
// one of the arithmetic, shift and bitwise operators (fold.Arith), the
// number of its value, as V8's parser does
// (Parser::ShortcutLiteralBinaryExpression).
func (p *parser) binaryOf(op string, left, right ast.Expr) ast.Expr {
	if x, ok := left.(*ast.NumberLit); ok {
		if y, ok := right.(*ast.NumberLit); ok && fold.On(fold.Numeric) {
			if v, ok := fold.Arith(op, x.Value, y.Value); ok {
				return p.nodes.number(v, x.Start)
			}
		}
	}
	return p.nodes.binaryOp(op, left, right)
}

// strBlockSize is the size of the blocks strCat's strings are carved
// from: a parse that joins a few literals does not pay for a large one.
const strBlockSize = 1024

// maxFoldedString is the longest string strCat joins, in UTF-16 code
// units: the runtime's most (fold.MaxStringLength), past which the
// program's concatenation throws as it runs. A test makes it smaller.
var maxFoldedString = fold.MaxStringLength

// strLit is the string a string literal is, where e is one: a string, or
// a template with no substitution and no tag, which V8 makes a string
// literal (Parser::CloseTemplateLiteral) -- not one with an invalid escape,
// which is an error the compiler reports.
func strLit(e ast.Expr) (string, bool) {
	switch n := e.(type) {
	case *ast.StringLit:
		return n.Value, true
	case *ast.TemplateLit:
		if len(n.Exprs) == 0 && len(n.Quasis) == 1 && n.Quasis[0].Valid {
			return n.Quasis[0].Cooked, true
		}
	}
	return "", false
}

// strCat is a run of string literals the + operators of one chain join,
// as V8's parser joins them (ShortcutStringLiteralAppendExpression, and
// CollapseNaryExpression's expr + "a" + "b"): the chain's left operand
// where it is a literal, or its last + operand's right where it is one.
// The pieces are joined once, as the chain ends (done), into a literal of
// their own -- not the first literal, which the directive prologue tells
// from what an expression made of it -- so that a long run costs its
// length once.
type strCat struct {
	pieces []string
	// units is the run's length in UTF-16 code units, which stays within
	// maxFoldedString.
	units int
	start int
	// in is the + whose right operand the run replaces, or nil for the
	// chain's left operand.
	in *ast.Binary
	// next is the next of the parser's runs not in use (freeCat).
	next *strCat
}

// isStrLit reports whether e may be a string literal (strLit), which a
// chain's + asks of each right operand before anything else.
func isStrLit(e ast.Expr) bool {
	switch e.(type) {
	case *ast.StringLit, *ast.TemplateLit:
		return true
	}
	return false
}

// startCat starts a run of the literal left ends with and right, or
// returns nil where left ends with none, or the two are too long.
func (p *parser) startCat(left, right ast.Expr) *strCat {
	r, ok := strLit(right)
	if !ok || !fold.On(fold.Strings) {
		return nil
	}
	var l string
	var in *ast.Binary
	var start int
	if l, ok = strLit(left); ok {
		start = left.Pos()
	} else if b, isBin := left.(*ast.Binary); isBin && b.Op == "+" {
		if l, ok = strLit(b.Right); !ok {
			return nil
		}
		in, start = b, b.Right.Pos()
	} else {
		return nil
	}
	lu, ru := wtf8.Count(l), wtf8.Count(r)
	if lu+ru > maxFoldedString {
		return nil
	}
	c := p.freeCat
	if c == nil {
		c = &strCat{}
	} else {
		p.freeCat = c.next
	}
	c.in, c.start, c.next = in, start, nil
	c.pieces, c.units = append(c.pieces[:0], l, r), lu+ru
	return c
}

// add joins right to the run, and reports whether it did: not where it
// is no string literal, or the run would be too long.
func (c *strCat) add(right ast.Expr) bool {
	r, ok := strLit(right)
	if !ok {
		return false
	}
	ru := wtf8.Count(r)
	if c.units+ru > maxFoldedString {
		return false
	}
	c.pieces = append(c.pieces, r)
	c.units += ru
	return true
}

// done ends the run, returning left with the run joined in it.
func (c *strCat) done(p *parser, left ast.Expr) ast.Expr {
	n := 0
	for _, s := range c.pieces {
		n += len(s)
	}
	// The bytes are carved from a block the parse's joined strings share,
	// as its nodes are from slabs: a run costs no allocation of its own.
	if cap(p.strBlock)-len(p.strBlock) < n {
		p.strBlock = make([]byte, 0, max(n, strBlockSize))
	}
	b := p.strBlock[len(p.strBlock) : len(p.strBlock) : len(p.strBlock)+n]
	for _, s := range c.pieces {
		// A surrogate pair split across two literals is the one code point
		// it spells, as the runtime's concatenation makes it.
		b = wtf8.AppendJoin(b, s)
	}
	p.strBlock = p.strBlock[:len(p.strBlock)+len(b)]
	// The bytes are the string's, written no more.
	lit := p.nodes.str(unsafe.String(unsafe.SliceData(b), len(b)), c.start)
	in := c.in
	clear(c.pieces)
	c.pieces, c.in, c.next, p.freeCat = c.pieces[:0], nil, p.freeCat, c
	if in != nil {
		in.Right = lit
		return left
	}
	return lit
}
