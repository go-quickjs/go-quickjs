package parser

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
)

// parseStatement parses a single statement or declaration.
func (p *parser) parseStatement() ast.Stmt {
	p.enter()
	defer p.leave()

	// The restriction applies to this statement and no further: a `let`
	// declaration nested anywhere inside it is fine.
	noLet := p.noLetDeclaration
	p.noLetDeclaration = false

	start := p.tok.Pos

	switch p.tok.Kind {
	case lexer.Punct:
		switch p.tok.Value {
		case "{":
			return p.parseBlock()
		case ";":
			p.next()
			return &ast.EmptyStmt{Start: start}
		}

	case lexer.Keyword:
		switch p.tok.Value {
		case "var":
			decl := p.parseVarDecl(ast.DeclVar)
			p.semicolon()
			return decl
		case "function":
			// A function declaration is not a Statement in the grammar, so it
			// cannot be the lone body of an `if` or a loop.
			return &ast.FuncDecl{Fn: p.parseFunction(ast.FuncNormal, true), Start: start}
		case "class":
			return &ast.ClassDecl{Class: p.parseClass(true), Start: start}
		case "if":
			return p.parseIf()
		case "for":
			return p.parseFor()
		case "while":
			return p.parseWhile()
		case "do":
			return p.parseDoWhile()
		case "return":
			return p.parseReturn()
		case "break", "continue":
			return p.parseBreakOrContinue()
		case "throw":
			return p.parseThrow()
		case "try":
			return p.parseTry()
		case "switch":
			return p.parseSwitch()
		case "debugger":
			p.next()
			p.semicolon()
			return &ast.DebuggerStmt{Start: start}
		case "with":
			return p.parseWith()
		case "import":
			// `import(` and `import.meta` are expressions, not declarations.
			if p.module && !p.importIsExpression() {
				return p.parseImportDecl()
			}
		case "export":
			return p.parseExportDecl()
		}

	case lexer.Ident:
		// A name written with escapes is not the contextual keyword: neither
		// `\u006Cet x = 1` nor `\u0061sync function f() {}` is a declaration.
		name := ""
		if !p.tok.Escaped {
			name = p.tok.Value
		}
		switch name {
		case "let":
			// `let` is only a declaration when what follows can begin a binding.
			// Otherwise it is an ordinary identifier, so `let = 1` still works.
			if !noLet && p.letStartsDeclaration() {
				decl := p.parseVarDecl(ast.DeclLet)
				p.semicolon()
				return decl
			}
		case "async":
			// `async function` at statement level is a declaration.
			m := p.mark()
			p.next()
			if p.isKeyword("function") && !p.tok.NewlineBefore {
				p.next()
				generator := p.eatPunct("*")
				fn := p.parseFunctionRest(start, ast.FuncNormal, true, generator, true)
				return &ast.FuncDecl{Fn: fn, Start: start}
			}
			p.reset(m)
		}
	}

	if p.isKeyword("const") {
		decl := p.parseVarDecl(ast.DeclConst)
		p.semicolon()
		return decl
	}

	// A labelled statement is an identifier followed by a colon.
	if p.tok.Kind == lexer.Ident {
		m := p.mark()
		tok := p.tok
		name := tok.Value
		p.next()
		if p.isPunct(":") {
			if tok.Escaped && lexer.IsReservedWord(name) {
				p.errorAt(tok, "%q is a reserved word", name)
			}
			p.next()
			return p.parseLabeled(name, start)
		}
		p.reset(m)
	}

	expr := p.parseExpr()
	p.semicolon()
	return p.exprStatement(expr, start)
}

// letStartsDeclaration reports whether a `let` token begins a declaration
// rather than being used as an identifier.
// nextIsPunct reports whether the token after the current one is the given
// punctuator, without consuming anything.
func (p *parser) nextIsPunct(v string) bool {
	m := p.mark()
	defer p.reset(m)
	p.next()
	return p.isPunct(v)
}

// nextIsKeywordOnSameLine reports whether the token after the current one is
// the given keyword with no line terminator before it, which is what makes
// `async function` one thing rather than two.
func (p *parser) nextIsKeywordOnSameLine(v string) bool {
	m := p.mark()
	defer p.reset(m)
	p.next()
	return p.isKeyword(v) && !p.tok.NewlineBefore
}

func (p *parser) letStartsDeclaration() bool {
	m := p.mark()
	defer p.reset(m)
	p.next()
	switch p.tok.Kind {
	case lexer.Ident:
		return true
	case lexer.Punct:
		// `let [` is always a declaration; `let {` likewise. Note that
		// `let[0] = x` is therefore a declaration error, which matches the spec.
		return p.tok.Value == "[" || p.tok.Value == "{"
	}
	return false
}

// parseBlock parses a brace-enclosed statement list.
func (p *parser) parseBlock() ast.Stmt {
	start := p.tok.Pos
	p.expectPunct("{")
	body := p.parseStatements(func() bool { return p.isPunct("}") })
	p.expectPunct("}")
	return &ast.BlockStmt{Body: body, Start: start}
}

// parseVarDecl parses a var, let or const declaration without its terminator,
// so that the for-statement head can reuse it.
func (p *parser) parseVarDecl(kind ast.DeclKind) *ast.VarDecl {
	start := p.tok.Pos
	p.next() // consume var/let/const

	decl := &ast.VarDecl{Kind: kind, Start: start}
	for {
		nameTok := p.tok
		target := p.parseBindingTarget()
		if kind != ast.DeclVar {
			var names []string
			collectBindingNames(target, &names)
			for _, n := range names {
				p.checkLexicalBindingName(n, nameTok)
			}
		}
		var init ast.Expr
		if p.eatPunct("=") {
			init = p.parseAssign()
		} else {
			switch target.(type) {
			case *ast.ArrayPattern, *ast.ObjectPattern:
				// A destructuring declaration has nothing to destructure
				// without an initializer. The for-in/of head is the exception,
				// and it calls this with noIn set.
				if !p.noIn {
					p.errorf("a destructuring declaration requires an initializer")
				}
			default:
				if kind == ast.DeclConst && !p.noIn {
					p.errorf("a const declaration requires an initializer")
				}
			}
		}
		decl.Decls = append(decl.Decls, ast.Declarator{Target: target, Init: init})
		if !p.eatPunct(",") {
			break
		}
	}
	return decl
}

// parseIf parses an if statement.
func (p *parser) parseIf() ast.Stmt {
	start := p.tok.Pos
	p.next()
	p.expectPunct("(")
	test := p.parseExpr()
	p.expectPunct(")")

	stmt := &ast.IfStmt{Test: test, Start: start}
	stmt.Cons = p.parseSubStatement(true)
	if p.eatKeyword("else") {
		stmt.Alt = p.parseSubStatement(true)
	}
	return stmt
}

// parseSubStatement parses the body of an if or a loop, where a declaration is
// not a valid body because it would have no scope to bind into.
//
// allowFunction marks the one position where a bare function declaration is
// tolerated: the branches of an if, which Annex B permits in sloppy mode for
// web compatibility. A loop body does not get the same allowance.
func (p *parser) parseSubStatement(allowFunction bool) ast.Stmt {
	switch {
	case p.isKeyword("class"), p.isKeyword("const"):
		p.errorf("a declaration cannot be the body of a statement")
	case p.isContextual("let") && p.nextIsPunct("["):
		// `let [` cannot begin an expression either, so there is nothing else
		// this could be. Any other `let` here is an ordinary identifier, and
		// `if (false) let\nx = 1` is two statements rather than an error.
		p.errorf("a declaration cannot be the body of a statement")
	case p.isContextual("async") && p.nextIsKeywordOnSameLine("function"):
		// Annex B's allowance is for a plain function declaration and nothing
		// else: an async function was introduced after the mistake was
		// recognized, so there is nothing to be compatible with.
		p.errorf("a function declaration cannot be the body of a statement")
	case p.isKeyword("function"):
		if p.strict || !allowFunction || p.nextIsPunct("*") {
			p.errorf("a function declaration cannot be the body of a statement")
		}
		// Annex B defines it as a block containing the declaration, which is
		// what gives it the hoisting a bare declaration here would not have.
		start := p.tok.Pos
		return &ast.BlockStmt{Body: []ast.Stmt{p.parseStatement()}, Start: start}
	case p.isKeyword("async") || p.tok.Kind == lexer.Ident:
		// A label may not be put on a function declaration here either, and
		// that rule holds in sloppy mode too: the Annex B allowance is for a
		// bare declaration, not a labelled one.
		saved := p.noLabelledFunction
		p.noLabelledFunction = true
		defer func() { p.noLabelledFunction = saved }()
		// A lexical declaration has no scope to bind into here, so `let` is an
		// ordinary identifier: `if (false) let\nx = 1` is two statements.
		p.noLetDeclaration = true
		return p.parseStatement()
	}
	return p.parseStatement()
}

// parseWhile parses a while loop.
func (p *parser) parseWhile() ast.Stmt {
	start := p.tok.Pos
	p.next()
	p.expectPunct("(")
	test := p.parseExpr()
	p.expectPunct(")")

	wasLoop := p.inLoop
	p.inLoop = true
	body := p.parseSubStatement(false)
	p.inLoop = wasLoop

	return &ast.WhileStmt{Test: test, Body: body, Start: start}
}

// parseDoWhile parses a do-while loop.
func (p *parser) parseDoWhile() ast.Stmt {
	start := p.tok.Pos
	p.next()

	wasLoop := p.inLoop
	p.inLoop = true
	body := p.parseSubStatement(false)
	p.inLoop = wasLoop

	p.expectKeyword("while")
	p.expectPunct("(")
	test := p.parseExpr()
	p.expectPunct(")")
	// A do-while may be terminated by ASI even without a newline, which is a
	// special case in the grammar.
	p.eatPunct(";")

	return &ast.DoWhileStmt{Body: body, Test: test, Start: start}
}

// parseFor parses the three-clause for, for-in and for-of statements, which
// share a head that is ambiguous until the `in` or `of` token appears.
func (p *parser) parseFor() ast.Stmt {
	start := p.tok.Pos
	p.next()

	isAwait := false
	if p.allowAwait && p.isContextual("await") {
		p.next()
		isAwait = true
	}
	p.expectPunct("(")

	// `in` must not be read as a relational operator while parsing the head.
	savedNoIn := p.noIn
	p.noIn = true

	var init ast.Stmt
	var left ast.Node

	switch {
	case p.isPunct(";"):
		// No initializer; this is definitely a three-clause for.

	case p.isKeyword("var"), p.isKeyword("const"),
		p.isContextual("let") && p.letStartsDeclaration():
		kind := ast.DeclVar
		switch {
		case p.isKeyword("const"):
			kind = ast.DeclConst
		case p.isContextual("let"):
			kind = ast.DeclLet
		}
		decl := p.parseVarDecl(kind)
		if p.isKeyword("in") || p.isContextual("of") {
			if len(decl.Decls) != 1 {
				p.errorf("a for-in/of head may declare only one binding")
			}
			if decl.Decls[0].Init != nil {
				p.errorf("a for-in/of binding cannot have an initializer")
			}
			left = decl
		} else {
			init = decl
		}

	default:
		exprStart := p.tok.Pos
		// `for (async of ...)` is refused outright: the head of a for-of may
		// not be the name `async` written on its own, because `for (async of
		// => {};;)` would otherwise be ambiguous with it.
		asyncHead := p.isContextual("async") && !isAwait
		expr := p.parseExpr()
		if p.isKeyword("in") || p.isContextual("of") {
			if asyncHead && p.isContextual("of") {
				if id, ok := expr.(*ast.Ident); ok && id.Name == "async" && !id.Paren {
					p.errorf("\"async\" cannot be the left-hand side of a for-of")
				}
			}
			// The head was an assignment target all along.
			left = p.toPattern(expr, false)
		} else {
			init = p.exprStatement(expr, exprStart)
		}
	}

	if left != nil {
		isOf := p.isContextual("of")
		p.next() // consume `in` or `of`
		p.noIn = savedNoIn

		var right ast.Expr
		if isOf {
			// `for (x of a, b)` parses the right side as an AssignmentExpression,
			// so the comma is not part of it.
			right = p.parseAssign()
		} else {
			right = p.parseExpr()
		}
		p.expectPunct(")")

		wasLoop := p.inLoop
		p.inLoop = true
		body := p.parseSubStatement(false)
		p.inLoop = wasLoop

		if isOf {
			return &ast.ForOfStmt{Left: left, Right: right, Body: body, Await: isAwait, Start: start}
		}
		if isAwait {
			p.errorf("\"for await\" requires \"of\"")
		}
		return &ast.ForInStmt{Left: left, Right: right, Body: body, Start: start}
	}

	if isAwait {
		p.errorf("\"for await\" requires \"of\"")
	}

	// A three-clause for. Its test and update clauses allow `in` again.
	p.noIn = savedNoIn
	p.expectPunct(";")

	stmt := &ast.ForStmt{Init: init, Start: start}
	if !p.isPunct(";") {
		stmt.Test = p.parseExpr()
	}
	p.expectPunct(";")
	if !p.isPunct(")") {
		stmt.Update = p.parseExpr()
	}
	p.expectPunct(")")

	wasLoop := p.inLoop
	p.inLoop = true
	stmt.Body = p.parseSubStatement(false)
	p.inLoop = wasLoop

	return stmt
}

// parseReturn parses a return statement, which is restricted: a newline after
// `return` ends the statement.
func (p *parser) parseReturn() ast.Stmt {
	start := p.tok.Pos
	if !p.inFunc {
		p.errorf("\"return\" is only valid inside a function")
	}
	p.next()

	stmt := &ast.ReturnStmt{Start: start}
	if !p.isPunct(";") && !p.canInsertSemicolon() {
		stmt.Arg = p.parseExpr()
	}
	p.semicolon()
	return stmt
}

// parseBreakOrContinue parses break and continue, with their optional labels.
func (p *parser) parseBreakOrContinue() ast.Stmt {
	start := p.tok.Pos
	isBreak := p.tok.Value == "break"
	p.next()

	label := ""
	// A newline after the keyword ends the statement, so a label must be on the
	// same line.
	if p.tok.Kind == lexer.Ident && !p.tok.NewlineBefore {
		label = p.tok.Value
		isLoop, defined := p.labels[label]
		if !defined {
			p.errorf("undefined label %q", label)
		}
		if !isBreak && !isLoop {
			p.errorf("%q does not label an iteration statement", label)
		}
		p.next()
	} else if isBreak {
		if !p.inLoop && !p.inSwitch {
			p.errorf("\"break\" is only valid inside a loop or switch")
		}
	} else {
		if !p.inLoop {
			p.errorf("\"continue\" is only valid inside a loop")
		}
	}
	p.semicolon()

	if isBreak {
		return &ast.BreakStmt{Label: label, Start: start}
	}
	return &ast.ContinueStmt{Label: label, Start: start}
}

// parseThrow parses a throw statement, which is restricted: the operand must
// begin on the same line.
func (p *parser) parseThrow() ast.Stmt {
	start := p.tok.Pos
	p.next()
	if p.tok.NewlineBefore {
		p.errorf("a newline is not allowed between \"throw\" and its operand")
	}
	arg := p.parseExpr()
	p.semicolon()
	return &ast.ThrowStmt{Arg: arg, Start: start}
}

// parseTry parses a try statement.
func (p *parser) parseTry() ast.Stmt {
	start := p.tok.Pos
	p.next()

	stmt := &ast.TryStmt{Start: start}
	p.expectPunct("{")
	stmt.Block = p.parseStatements(func() bool { return p.isPunct("}") })
	p.expectPunct("}")

	if p.isKeyword("catch") {
		clauseStart := p.tok.Pos
		p.next()
		clause := &ast.CatchClause{Start: clauseStart}
		// The binding is optional: `catch { }` is legal.
		if p.eatPunct("(") {
			clause.Param = p.parseBindingTarget()
			p.expectPunct(")")
		}
		p.expectPunct("{")
		clause.Body = p.parseStatements(func() bool { return p.isPunct("}") })
		p.expectPunct("}")
		stmt.Catch = clause
	}

	if p.eatKeyword("finally") {
		p.expectPunct("{")
		stmt.Finally = p.parseStatements(func() bool { return p.isPunct("}") })
		p.expectPunct("}")
		// An empty finally block is legal but indistinguishable from none, so
		// record it explicitly.
		if stmt.Finally == nil {
			stmt.Finally = []ast.Stmt{}
		}
	}

	if stmt.Catch == nil && stmt.Finally == nil {
		p.errorf("\"try\" requires a \"catch\" or \"finally\" clause")
	}
	return stmt
}

// parseSwitch parses a switch statement.
func (p *parser) parseSwitch() ast.Stmt {
	start := p.tok.Pos
	p.next()
	p.expectPunct("(")
	disc := p.parseExpr()
	p.expectPunct(")")
	p.expectPunct("{")

	wasSwitch := p.inSwitch
	p.inSwitch = true

	stmt := &ast.SwitchStmt{Disc: disc, Start: start}
	sawDefault := false
	for !p.isPunct("}") {
		caseStart := p.tok.Pos
		var test ast.Expr
		switch {
		case p.eatKeyword("case"):
			test = p.parseExpr()
		case p.eatKeyword("default"):
			if sawDefault {
				p.errorf("a switch may have only one default clause")
			}
			sawDefault = true
		default:
			p.errorf("expected \"case\" or \"default\"")
		}
		p.expectPunct(":")

		// A clause body runs until the next clause or the closing brace.
		body := p.parseStatements(func() bool {
			return p.isPunct("}") || p.isKeyword("case") || p.isKeyword("default")
		})
		stmt.Cases = append(stmt.Cases, ast.SwitchCase{Test: test, Body: body, Start: caseStart})
	}
	p.expectPunct("}")
	p.inSwitch = wasSwitch
	return stmt
}

// parseLabeled parses a labelled statement, with the label and colon already
// consumed.
func (p *parser) parseLabeled(name string, start int) ast.Stmt {
	p.checkLabelName(name, p.tok)
	if _, exists := p.labels[name]; exists {
		p.errorf("label %q is already in scope", name)
	}
	// Whether the label names an iteration statement decides if `continue` may
	// target it, which is known only from the statement that follows.
	isLoop := p.isKeyword("for") || p.isKeyword("while") || p.isKeyword("do")
	p.labels[name] = isLoop
	defer delete(p.labels, name)

	// A labelled function declaration is a statement list item, never the body
	// of another statement, and unlike a bare one it has no Annex B allowance
	// even in sloppy mode.
	if p.noLabelledFunction && p.isKeyword("function") {
		p.errorf("a labelled function declaration cannot be the body of a statement")
	}
	// A generator or an async function is never a labelled statement's body,
	// labelled or not, in strict mode or sloppy: only a plain declaration is.
	if (p.isKeyword("function") && p.nextIsPunct("*")) ||
		(p.isContextual("async") && p.nextIsKeywordOnSameLine("function")) {
		p.errorf("a function declaration cannot be labelled here")
	}
	// A labelled statement's body is a Statement, so a declaration cannot be
	// one -- the label would have nothing to name.
	switch {
	case p.isKeyword("class"), p.isKeyword("const"):
		p.errorf("a declaration cannot be labelled")
	case p.isContextual("let") && p.nextIsPunct("["):
		p.errorf("a declaration cannot be labelled")
	case p.strict && p.isKeyword("function"):
		p.errorf("a function declaration cannot be labelled in strict mode")
	}
	// A lexical declaration is not a statement, so `let` here is an ordinary
	// identifier.
	p.noLetDeclaration = true
	// A nested label inherits the restriction: `while (x) a: b: function f(){}`
	// is no better than one label.
	body := p.parseStatement()
	return &ast.LabeledStmt{Label: name, Body: body, Start: start}
}

// parseWith parses a with statement, which strict mode forbids.
func (p *parser) parseWith() ast.Stmt {
	if p.strict {
		p.errorf("\"with\" is not allowed in strict mode")
	}
	start := p.tok.Pos
	p.next()
	p.expectPunct("(")
	obj := p.parseExpr()
	p.expectPunct(")")
	body := p.parseSubStatement(false)
	// `with` is represented as a labelled block carrying the object, since the
	// compiler rejects it anyway outside sloppy mode.
	return &ast.WithStmt{Object: obj, Body: body, Start: start}
}

// importIsExpression reports whether an `import` token begins a dynamic import
// or import.meta rather than an import declaration.
func (p *parser) importIsExpression() bool {
	m := p.mark()
	defer p.reset(m)
	p.next()
	return p.isPunct("(") || p.isPunct(".")
}

// collectBindingNames gathers the identifiers a binding target introduces.
func collectBindingNames(target ast.Expr, out *[]string) {
	switch n := target.(type) {
	case *ast.Ident:
		*out = append(*out, n.Name)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			if el != nil {
				collectBindingNames(el, out)
			}
		}
		if n.Rest != nil {
			collectBindingNames(n.Rest, out)
		}
	case *ast.ObjectPattern:
		for _, prop := range n.Props {
			collectBindingNames(prop.Value, out)
		}
		if n.Rest != nil {
			collectBindingNames(n.Rest, out)
		}
	case *ast.AssignPattern:
		collectBindingNames(n.Target, out)
	case *ast.RestElement:
		collectBindingNames(n.Arg, out)
	}
}
