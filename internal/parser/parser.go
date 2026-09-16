// Package parser turns ECMAScript source text into an ast.Program.
//
// It is a recursive-descent parser with one token of lookahead. Two parts of
// the grammar need more than that, and both are handled with a cover grammar
// rather than backtracking, so parsing stays linear:
//
//   - `(a, b)` is parsed as a parenthesized expression list that also permits
//     rest elements and defaults; if `=>` follows, the list is reinterpreted as
//     an arrow function's parameters by toPattern.
//   - `{a: b}` in an assignment position is parsed as an object literal and
//     reinterpreted as a destructuring pattern when `=` is found.
package parser

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
)

// Error is a syntax error with source position information.
type Error struct {
	Msg  string
	Line int
	Col  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("SyntaxError: %s (line %d, column %d)", e.Msg, e.Line, e.Col)
}

// Options configures a parse.
type Options struct {
	// Module parses the input as an ECMAScript module, which implies strict
	// mode and permits import/export declarations.
	Module bool
	// Strict forces strict mode even without a "use strict" directive.
	Strict bool
}

// parser holds the state of one parse.
type parser struct {
	lex *lexer.Lexer
	// tok is the current token; the parser always has exactly one token of
	// lookahead available.
	tok lexer.Token
	// prevEnd is the byte offset just past the previous token, used to decide
	// whether a semicolon can be inserted.
	prevEnd int

	strict bool
	module bool

	// Context flags controlling which productions are legal at this point.
	// They are saved and restored around function bodies rather than kept on a
	// stack, because the nesting is exactly the recursion.
	inFunc         bool
	inLoop         bool
	inSwitch       bool
	inClassBody    bool
	allowYield     bool
	allowAwait     bool
	allowSuperProp bool
	allowSuperCall bool
	allowNewTarget bool

	// noIn suppresses `in` as a relational operator while parsing the head of a
	// for statement, where `for (x in y)` must not be read as a comparison. It
	// is parser state rather than a parameter because it has to survive the
	// whole binary-expression recursion.
	noIn bool

	// labels holds the labels currently in scope, mapping name to whether the
	// label names an iteration statement (and so permits `continue`).
	labels map[string]bool

	// nodes batches the allocation of the most common node types. See arena.go.
	nodes arena

	// depth bounds nesting. A recursive-descent parser uses Go stack in
	// proportion to how deeply the input nests, and a goroutine stack overflow
	// is a process-wide crash rather than a catchable error, so pathological
	// input is rejected before it gets that far.
	depth int
}

// maxNestingDepth is the deepest expression or statement nesting accepted.
//
// Real code never approaches this; generated or hostile input can. The figure
// is well below where the Go stack would be exhausted, leaving room for the
// compiler, which recurses over the same tree.
const maxNestingDepth = 1000

// enter increases the nesting depth, failing if the input is too deep.
func (p *parser) enter() {
	p.depth++
	if p.depth > maxNestingDepth {
		p.errorf("the expression nests too deeply")
	}
}

func (p *parser) leave() { p.depth-- }

// parserMark records a position the parser can return to. It is used only for
// the few one-token lookaheads that the cover grammar cannot express, such as
// telling `get x() {}` from a property literally named `get`.
type parserMark struct {
	pos       int
	line      int
	lineStart int
	prevEnd   int
	tok       lexer.Token
}

func (p *parser) mark() parserMark {
	return parserMark{
		pos:       p.lex.Pos(),
		line:      p.lex.Line(),
		lineStart: p.lex.LineStart(),
		prevEnd:   p.prevEnd,
		tok:       p.tok,
	}
}

func (p *parser) reset(m parserMark) {
	p.lex.Seek(m.pos, m.line, m.lineStart)
	p.prevEnd = m.prevEnd
	p.tok = m.tok
}

// Parse parses src into a Program.
func Parse(src string, opts Options) (prog *ast.Program, err error) {
	p := &parser{
		lex:    lexer.New(src),
		strict: opts.Strict || opts.Module,
		module: opts.Module,
		labels: make(map[string]bool),
	}
	// The recursive-descent routines report errors by panicking with a
	// *Error, which keeps their signatures free of error returns. Nothing else
	// panics deliberately, so any other value is a genuine bug and is re-raised.
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Error); ok {
				prog, err = nil, e
				return
			}
			panic(r)
		}
	}()

	p.next()
	body := p.parseDirectivePrologue(func() bool { return p.tok.Kind == lexer.EOF })
	body = append(body, p.parseStatements(func() bool { return p.tok.Kind == lexer.EOF })...)

	return &ast.Program{
		Body:   body,
		Strict: p.strict,
		Module: p.module,
	}, nil
}

// ---------------------------------------------------------------------------
// Token handling
// ---------------------------------------------------------------------------

// next advances to the following token.
func (p *parser) next() {
	p.prevEnd = p.lex.Pos()
	tok, err := p.lex.Next()
	if err != nil {
		p.fail(err)
	}
	p.tok = tok
}

// fail raises a lexer error as a parser error.
func (p *parser) fail(err error) {
	if le, ok := err.(*lexer.Error); ok {
		panic(&Error{Msg: le.Msg, Line: le.Line, Col: le.Col})
	}
	panic(&Error{Msg: err.Error(), Line: p.tok.Line, Col: p.lex.Column(p.tok.Pos, p.tok.LineStart)})
}

// errorf raises a syntax error at the current token.
func (p *parser) errorf(format string, args ...any) {
	p.errorAt(p.tok, "%s", fmt.Sprintf(format, args...))
}

// errorAt raises a syntax error at a specific token.
func (p *parser) errorAt(tok lexer.Token, format string, args ...any) {
	panic(&Error{
		Msg:  fmt.Sprintf(format, args...),
		Line: tok.Line,
		Col:  p.lex.Column(tok.Pos, tok.LineStart),
	})
}

// unexpected reports the current token as unexpected.
func (p *parser) unexpected() {
	p.errorf("unexpected %s", p.tok)
}

// isPunct reports whether the current token is the given punctuator.
func (p *parser) isPunct(v string) bool { return p.tok.IsPunct(v) }

// isKeyword reports whether the current token is the given keyword.
func (p *parser) isKeyword(v string) bool { return p.tok.IsKeyword(v) }

// isContextual reports whether the current token is the given contextual
// keyword, which the lexer returns as a plain identifier.
func (p *parser) isContextual(name string) bool {
	return p.tok.Kind == lexer.Ident && p.tok.Value == name
}

// eatPunct consumes the given punctuator if present.
func (p *parser) eatPunct(v string) bool {
	if p.isPunct(v) {
		p.next()
		return true
	}
	return false
}

// eatKeyword consumes the given keyword if present.
func (p *parser) eatKeyword(v string) bool {
	if p.isKeyword(v) {
		p.next()
		return true
	}
	return false
}

// eatContextual consumes the given contextual keyword if present.
func (p *parser) eatContextual(name string) bool {
	if p.isContextual(name) {
		p.next()
		return true
	}
	return false
}

// expectPunct consumes the given punctuator or fails.
func (p *parser) expectPunct(v string) {
	if !p.isPunct(v) {
		p.errorf("expected %q but found %s", v, p.tok)
	}
	p.next()
}

// expectKeyword consumes the given keyword or fails.
func (p *parser) expectKeyword(v string) {
	if !p.isKeyword(v) {
		p.errorf("expected %q but found %s", v, p.tok)
	}
	p.next()
}

// ---------------------------------------------------------------------------
// Automatic semicolon insertion
// ---------------------------------------------------------------------------

// semicolon consumes a statement terminator, applying the automatic semicolon
// insertion rules: a semicolon is inserted before a token that begins a new
// line, before '}', and at end of input.
func (p *parser) semicolon() {
	if p.eatPunct(";") {
		return
	}
	if p.isPunct("}") || p.tok.Kind == lexer.EOF || p.tok.NewlineBefore {
		return
	}
	p.errorf("expected ';' but found %s", p.tok)
}

// canInsertSemicolon reports whether ASI would apply at the current token,
// which the restricted productions use to decide whether an operand follows.
func (p *parser) canInsertSemicolon() bool {
	return p.isPunct("}") || p.tok.Kind == lexer.EOF || p.tok.NewlineBefore
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

// parseIdentName parses any identifier name, including reserved words. This is
// what property positions accept: `a.if` and `{if: 1}` are both legal.
func (p *parser) parseIdentName() *ast.Ident {
	switch p.tok.Kind {
	case lexer.Ident, lexer.Keyword:
		id := p.nodes.ident(p.tok.Value, p.tok.Pos)
		p.next()
		return id
	}
	p.errorf("expected an identifier but found %s", p.tok)
	return nil
}

// parseBindingIdent parses an identifier used as a binding or reference, and
// rejects reserved words and the strict-mode restricted names.
func (p *parser) parseBindingIdent() *ast.Ident {
	if p.tok.Kind != lexer.Ident {
		p.errorf("expected an identifier but found %s", p.tok)
	}
	name := p.tok.Value
	p.checkBindingName(name, p.tok)
	id := p.nodes.ident(name, p.tok.Pos)
	p.next()
	return id
}

// checkBindingName rejects names that cannot be bound in the current context.
func (p *parser) checkBindingName(name string, tok lexer.Token) {
	if p.strict {
		switch name {
		case "eval", "arguments":
			p.errorAt(tok, "cannot bind %q in strict mode", name)
		case "implements", "interface", "let", "package", "private", "protected",
			"public", "static", "yield":
			p.errorAt(tok, "%q is reserved in strict mode", name)
		}
	}
	if p.allowYield && name == "yield" {
		p.errorAt(tok, "cannot bind \"yield\" inside a generator")
	}
	if p.allowAwait && name == "await" {
		p.errorAt(tok, "cannot bind \"await\" inside an async function")
	}
	if p.module && name == "await" {
		p.errorAt(tok, "cannot bind \"await\" at the top level of a module")
	}
}

// ---------------------------------------------------------------------------
// Directive prologues
// ---------------------------------------------------------------------------

// parseDirectivePrologue consumes the leading string-literal statements of a
// script or function body, enabling strict mode if "use strict" appears.
//
// The directives must be recognized before the rest of the body is parsed,
// because strict mode changes which identifiers are legal and how octal
// literals are treated.
func (p *parser) parseDirectivePrologue(atEnd func() bool) []ast.Stmt {
	var out []ast.Stmt
	for !atEnd() {
		if p.tok.Kind != lexer.String {
			break
		}
		// A string literal is only a directive if the whole statement is just
		// that literal: `"use strict" + x` is an ordinary expression.
		tok := p.tok
		raw := tok.Raw
		p.next()
		if !p.canInsertSemicolon() && !p.isPunct(";") {
			// The string began an expression; parse the rest of it normally.
			expr := p.parseExprFrom(p.nodes.str(tok.Value, tok.Pos))
			p.semicolon()
			out = append(out, p.exprStatement(expr, tok.Pos))
			break
		}
		p.semicolon()
		out = append(out, p.exprStatement(p.nodes.str(tok.Value, tok.Pos), tok.Pos))
		// Compare the raw text so that "use strict" is not a directive.
		if raw == `"use strict"` || raw == `'use strict'` {
			p.strict = true
		}
	}
	return out
}

// parseStatements parses statements until atEnd reports true.
func (p *parser) parseStatements(atEnd func() bool) []ast.Stmt {
	var out []ast.Stmt
	for !atEnd() {
		if p.tok.Kind == lexer.EOF {
			break
		}
		out = append(out, p.parseStatement())
	}
	return out
}

// ---------------------------------------------------------------------------
// Context save/restore
// ---------------------------------------------------------------------------

// funcContext captures the parser flags that a function body resets.
type funcContext struct {
	inFunc         bool
	inLoop         bool
	inSwitch       bool
	allowYield     bool
	allowAwait     bool
	allowSuperProp bool
	allowSuperCall bool
	allowNewTarget bool
	strict         bool
	labels         map[string]bool
}

func (p *parser) saveContext() funcContext {
	return funcContext{
		inFunc: p.inFunc, inLoop: p.inLoop, inSwitch: p.inSwitch,
		allowYield: p.allowYield, allowAwait: p.allowAwait,
		allowSuperProp: p.allowSuperProp, allowSuperCall: p.allowSuperCall,
		allowNewTarget: p.allowNewTarget,
		strict:         p.strict, labels: p.labels,
	}
}

func (p *parser) restoreContext(c funcContext) {
	p.inFunc, p.inLoop, p.inSwitch = c.inFunc, c.inLoop, c.inSwitch
	p.allowYield, p.allowAwait = c.allowYield, c.allowAwait
	p.allowSuperProp, p.allowSuperCall = c.allowSuperProp, c.allowSuperCall
	p.allowNewTarget = c.allowNewTarget
	p.strict, p.labels = c.strict, c.labels
}
