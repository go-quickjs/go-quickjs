package parser

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
)

// binaryPrec maps a binary operator to its precedence. Higher binds tighter.
// Operators absent from the table are not binary operators.
//
// `in` and `instanceof` share the relational level, and `**` is the only
// right-associative entry.
var binaryPrec = map[string]int{
	"??": 1,
	"||": 2,
	"&&": 3,
	"|":  4,
	"^":  5,
	"&":  6,

	"==": 7, "!=": 7, "===": 7, "!==": 7,

	"<": 8, ">": 8, "<=": 8, ">=": 8, "instanceof": 8, "in": 8,

	"<<": 9, ">>": 9, ">>>": 9,

	"+": 10, "-": 10,

	"*": 11, "/": 11, "%": 11,

	"**": 12,
}

// assignOps are the compound assignment operators.
var assignOps = map[string]bool{
	"=": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"**=": true, "<<=": true, ">>=": true, ">>>=": true, "&=": true,
	"|=": true, "^=": true, "&&=": true, "||=": true, "??=": true,
}

// parseExpr parses a full Expression, including the comma operator.
func (p *parser) parseExpr() ast.Expr {
	start := p.tok.Pos
	first := p.parseAssign()
	if !p.isPunct(",") {
		return first
	}
	exprs := []ast.Expr{first}
	for p.eatPunct(",") {
		exprs = append(exprs, p.parseAssign())
	}
	return &ast.Sequence{Exprs: exprs, Start: start}
}

// parseExprFrom continues parsing an expression whose first primary has already
// been consumed. The directive prologue needs it after deciding that a leading
// string literal was not a directive.
func (p *parser) parseExprFrom(left ast.Expr) ast.Expr {
	left = p.parseCallTail(p.parseMemberTail(left, true), true)
	left = p.parseBinaryFrom(left, 0)
	left = p.parseConditionalFrom(left)
	left = p.parseAssignFrom(left)
	if !p.isPunct(",") {
		return left
	}
	exprs := []ast.Expr{left}
	for p.eatPunct(",") {
		exprs = append(exprs, p.parseAssign())
	}
	return &ast.Sequence{Exprs: exprs, Start: left.Pos()}
}

// parseAssign parses an AssignmentExpression.
func (p *parser) parseAssign() ast.Expr {
	// `yield` is an operator rather than an identifier inside a generator.
	if p.allowYield && p.isContextual("yield") {
		return p.parseYield()
	}
	// An arrow function with a single unparenthesized parameter is the one form
	// that cannot be recognized from a cover grammar, because `x` alone is a
	// valid expression. One token of lookahead past the identifier settles it.
	if arrow := p.tryParseSingleParamArrow(); arrow != nil {
		return arrow
	}

	// The parenthesized arrow form does not need handling here: parseParenOrArrow
	// consumes the `=>` itself, because it is the only place that still holds
	// the cover list needed to reinterpret the parentheses as parameters.
	return p.parseAssignFrom(p.parseConditional())
}

// parseAssignFrom completes an assignment whose left side is already parsed.
func (p *parser) parseAssignFrom(left ast.Expr) ast.Expr {
	if p.tok.Kind != lexer.Punct || !assignOps[p.tok.Value] {
		return left
	}
	op := p.tok.Value
	opTok := p.tok
	p.next()

	if op == "=" {
		// The left side of a plain assignment may be a destructuring pattern,
		// which until now was parsed as an object or array literal.
		left = p.toPattern(left, false)
	} else {
		p.checkSimpleAssignTarget(left, opTok)
	}
	right := p.parseAssign()
	return p.nodes.assignOp(op, left, right)
}

// checkSimpleAssignTarget rejects assignment to anything that is not a
// reference, which compound assignment and update operators require.
func (p *parser) checkSimpleAssignTarget(target ast.Expr, tok lexer.Token) {
	switch t := target.(type) {
	case *ast.Ident:
		if p.strict && (t.Name == "eval" || t.Name == "arguments") {
			p.errorAt(tok, "cannot assign to %q in strict mode", t.Name)
		}
	case *ast.Member:
		// Always a valid target.
	default:
		p.errorAt(tok, "invalid assignment target")
	}
}

// parseYield parses a yield expression. The operand is optional, and a newline
// after `yield` terminates the expression.
func (p *parser) parseYield() ast.Expr {
	start := p.tok.Pos
	p.next()
	if p.canInsertSemicolon() {
		return &ast.Yield{Start: start}
	}
	delegate := p.eatPunct("*")
	// Without a delegate, these tokens all end the expression rather than
	// beginning an operand.
	if !delegate {
		switch {
		case p.isPunct(")"), p.isPunct("]"), p.isPunct("}"), p.isPunct(","),
			p.isPunct(";"), p.isPunct(":"):
			return &ast.Yield{Start: start}
		}
	}
	return &ast.Yield{Arg: p.parseAssign(), Delegate: delegate, Start: start}
}

// parseConditional parses a ConditionalExpression.
func (p *parser) parseConditional() ast.Expr {
	return p.parseConditionalFrom(p.parseBinary(0))
}

func (p *parser) parseConditionalFrom(test ast.Expr) ast.Expr {
	if !p.isPunct("?") {
		return test
	}
	p.next()
	// The branches of a conditional are AssignmentExpressions, and `in` is
	// always permitted inside them.
	cons := p.parseAssign()
	p.expectPunct(":")
	alt := p.parseAssign()
	return p.nodes.conditional(test, cons, alt)
}

// noIn tracks whether the `in` operator is currently a relational operator or
// part of a for-in head. It is a field rather than a parameter because it must
// survive the whole binary-expression recursion.
func (p *parser) parseBinary(minPrec int) ast.Expr {
	return p.parseBinaryFrom(p.parseUnary(), minPrec)
}

// parseBinaryFrom performs precedence climbing over binary operators, starting
// from an already-parsed left operand.
func (p *parser) parseBinaryFrom(left ast.Expr, minPrec int) ast.Expr {
	for {
		op, prec, ok := p.peekBinaryOp()
		if !ok || prec <= minPrec {
			return left
		}
		p.next()

		var right ast.Expr
		if op == "**" {
			// Right-associative: recurse at one below its own precedence.
			right = p.parseBinaryFrom(p.parseUnary(), prec-1)
		} else {
			right = p.parseBinaryFrom(p.parseUnary(), prec)
		}

		switch op {
		case "&&", "||", "??":
			// `a ?? b || c` is a syntax error without parentheses, because the
			// precedence would be surprising.
			if op == "??" {
				p.checkNullishMix(left)
				p.checkNullishMix(right)
			} else {
				p.checkNullishOperand(left, op)
				p.checkNullishOperand(right, op)
			}
			left = p.nodes.logicalOp(op, left, right)
		default:
			left = p.nodes.binaryOp(op, left, right)
		}
	}
}

// checkNullishMix rejects an unparenthesized && or || as an operand of ??.
func (p *parser) checkNullishMix(e ast.Expr) {
	if l, ok := e.(*ast.Logical); ok && !l.Paren && (l.Op == "&&" || l.Op == "||") {
		p.errorf("%q and \"??\" cannot be mixed without parentheses", l.Op)
	}
}

// checkNullishOperand rejects an unparenthesized ?? as an operand of && or ||.
func (p *parser) checkNullishOperand(e ast.Expr, op string) {
	if l, ok := e.(*ast.Logical); ok && !l.Paren && l.Op == "??" {
		p.errorf("%q and \"??\" cannot be mixed without parentheses", op)
	}
}

// peekBinaryOp reports the binary operator at the current token, if any.
func (p *parser) peekBinaryOp() (op string, prec int, ok bool) {
	switch p.tok.Kind {
	case lexer.Punct:
		prec, ok = binaryPrec[p.tok.Value]
		return p.tok.Value, prec, ok
	case lexer.Keyword:
		switch p.tok.Value {
		case "instanceof":
			return "instanceof", binaryPrec["instanceof"], true
		case "in":
			if p.noIn {
				return "", 0, false
			}
			return "in", binaryPrec["in"], true
		}
	}
	return "", 0, false
}

// parseUnary parses a UnaryExpression.
func (p *parser) parseUnary() ast.Expr {
	start := p.tok.Pos

	switch {
	case p.isPunct("!"), p.isPunct("~"), p.isPunct("+"), p.isPunct("-"):
		op := p.tok.Value
		p.next()
		operand := p.parseUnary()
		// `-2 ** 2` is a syntax error: the grammar refuses to guess whether the
		// negation or the exponentiation binds tighter.
		if p.isPunct("**") {
			p.errorf("unary %q before \"**\" requires parentheses", op)
		}
		return p.nodes.unaryOp(op, operand, start)

	case p.isKeyword("typeof"), p.isKeyword("void"), p.isKeyword("delete"):
		op := p.tok.Value
		p.next()
		operand := p.parseUnary()
		if p.isPunct("**") {
			p.errorf("unary %q before \"**\" requires parentheses", op)
		}
		if op == "delete" && p.strict {
			// Deleting a bare identifier is forbidden in strict mode.
			if _, isIdent := operand.(*ast.Ident); isIdent {
				p.errorf("cannot delete a variable in strict mode")
			}
		}
		return p.nodes.unaryOp(op, operand, start)

	case p.isPunct("++"), p.isPunct("--"):
		op := p.tok.Value
		opTok := p.tok
		p.next()
		operand := p.parseUnary()
		p.checkSimpleAssignTarget(operand, opTok)
		return p.nodes.updateOp(op, operand, true, start)

	case p.allowAwait && p.isContextual("await"):
		p.next()
		return &ast.Await{Arg: p.parseUnary(), Start: start}
	}

	return p.parsePostfix()
}

// parsePostfix parses a LeftHandSideExpression with an optional postfix
// ++ or --.
func (p *parser) parsePostfix() ast.Expr {
	expr := p.parseLeftHandSide()
	// A newline before a postfix operator triggers ASI, so `a\n++b` is two
	// statements rather than a postfix increment.
	if (p.isPunct("++") || p.isPunct("--")) && !p.tok.NewlineBefore {
		opTok := p.tok
		p.checkSimpleAssignTarget(expr, opTok)
		op := p.tok.Value
		p.next()
		return p.nodes.updateOp(op, expr, false, expr.Pos())
	}
	return expr
}

// parseLeftHandSide parses a call or new expression with all its member and
// call suffixes.
func (p *parser) parseLeftHandSide() ast.Expr {
	var expr ast.Expr
	if p.isKeyword("new") {
		expr = p.parseNew()
	} else {
		expr = p.parsePrimary()
	}
	return p.parseCallTail(p.parseMemberTail(expr, true), true)
}

// parseNew parses `new X(...)` and `new.target`.
func (p *parser) parseNew() ast.Expr {
	start := p.tok.Pos
	p.next()

	if p.isPunct(".") {
		p.next()
		if !p.isContextual("target") {
			p.errorf("expected \"new.target\"")
		}
		if !p.allowNewTarget {
			p.errorf("\"new.target\" is only valid inside a function")
		}
		p.next()
		return &ast.NewTarget{Start: start}
	}

	// The callee of `new` binds tighter than a call: in `new a.b()` the callee
	// is `a.b`, and the argument list belongs to the `new`.
	var callee ast.Expr
	if p.isKeyword("new") {
		callee = p.parseNew()
	} else {
		callee = p.parsePrimary()
	}
	callee = p.parseMemberTail(callee, false)

	var args []ast.Expr
	if p.isPunct("(") {
		args = p.parseArguments()
	}
	expr := ast.Expr(&ast.New{Callee: callee, Args: args, Start: start})
	return p.parseMemberTail(expr, true)
}

// parseMemberTail consumes property accesses and tagged templates. When
// allowCall is false it stops before an argument list, which is what the callee
// of `new` requires.
func (p *parser) parseMemberTail(expr ast.Expr, allowCall bool) ast.Expr {
	for {
		switch {
		case p.isPunct("."):
			p.next()
			if p.tok.Kind == lexer.PrivateIdent {
				name := &ast.PrivateName{Name: p.tok.Value, Start: p.tok.Pos}
				p.next()
				expr = &ast.Member{Object: expr, Property: name, Start: expr.Pos()}
				continue
			}
			prop := p.parseIdentName()
			expr = p.nodes.memberOf(expr, prop, false, false)

		case p.isPunct("["):
			p.next()
			// `in` is always allowed inside brackets, even in a for-in head.
			saved := p.noIn
			p.noIn = false
			prop := p.parseExpr()
			p.noIn = saved
			p.expectPunct("]")
			expr = p.nodes.memberOf(expr, prop, true, false)

		case p.startsTemplate():
			quasi := p.parseTemplate()
			expr = &ast.TaggedTemplate{Tag: expr, Quasi: quasi, Start: expr.Pos()}

		default:
			return expr
		}
		if !allowCall && p.isPunct("(") {
			return expr
		}
	}
}

// parseCallTail consumes call arguments and optional-chaining links, wrapping
// the result in an OptionalChain if any `?.` appeared.
func (p *parser) parseCallTail(expr ast.Expr, allowCall bool) ast.Expr {
	optional := false
	for {
		switch {
		case allowCall && p.isPunct("("):
			args := p.parseArguments()
			expr = p.nodes.callOf(expr, args, false)

		case p.isPunct("?."):
			optional = true
			p.next()
			switch {
			case p.isPunct("("):
				args := p.parseArguments()
				expr = p.nodes.callOf(expr, args, true)
			case p.isPunct("["):
				p.next()
				prop := p.parseExpr()
				p.expectPunct("]")
				expr = p.nodes.memberOf(expr, prop, true, true)
			case p.startsTemplate():
				p.errorf("a tagged template cannot appear in an optional chain")
			default:
				var prop ast.Expr
				if p.tok.Kind == lexer.PrivateIdent {
					prop = &ast.PrivateName{Name: p.tok.Value, Start: p.tok.Pos}
					p.next()
				} else {
					prop = p.parseIdentName()
				}
				expr = p.nodes.memberOf(expr, prop, false, true)
			}

		case p.isPunct("."), p.isPunct("["), p.startsTemplate():
			expr = p.parseMemberTail(expr, allowCall)
			continue

		default:
			if optional {
				return &ast.OptionalChain{Base: expr, Start: expr.Pos()}
			}
			return expr
		}
	}
}

// parseArguments parses a parenthesized argument list.
func (p *parser) parseArguments() []ast.Expr {
	p.expectPunct("(")
	saved := p.noIn
	p.noIn = false
	defer func() { p.noIn = saved }()

	var args []ast.Expr
	for !p.isPunct(")") {
		if p.isPunct("...") {
			start := p.tok.Pos
			p.next()
			args = append(args, &ast.Spread{Arg: p.parseAssign(), Start: start})
		} else {
			args = append(args, p.parseAssign())
		}
		if !p.eatPunct(",") {
			break
		}
	}
	p.expectPunct(")")
	return args
}

// parsePrimary parses a PrimaryExpression.
func (p *parser) parsePrimary() ast.Expr {
	start := p.tok.Pos

	switch p.tok.Kind {
	case lexer.Number:
		v := p.tok.Num
		p.checkLegacyOctal()
		p.next()
		return p.nodes.number(v, start)

	case lexer.BigInt:
		raw := p.tok.Value
		p.next()
		return &ast.BigIntLit{Raw: raw, Start: start}

	case lexer.String:
		v := p.tok.Value
		p.next()
		return p.nodes.str(v, start)

	case lexer.Template, lexer.TemplateHead:
		return p.parseTemplate()

	case lexer.PrivateIdent:
		// A private name is only a primary as the left operand of `in`.
		name := &ast.PrivateName{Name: p.tok.Value, Start: start}
		p.next()
		if !p.isKeyword("in") {
			p.errorAt(p.tok, "a private name is only valid as the left operand of \"in\"")
		}
		return name

	case lexer.Ident:
		// `async function` and `async x =>` start an async function; a bare
		// `async` is an ordinary identifier.
		if p.tok.Value == "async" && !p.tok.NewlineBefore {
			if fn := p.tryParseAsyncFunction(); fn != nil {
				return fn
			}
		}
		name := p.tok.Value
		p.next()
		return p.nodes.ident(name, start)

	case lexer.Keyword:
		switch p.tok.Value {
		case "this":
			p.next()
			return &ast.This{Start: start}
		case "true", "false":
			v := p.tok.Value == "true"
			p.next()
			return &ast.BoolLit{Value: v, Start: start}
		case "null":
			p.next()
			return &ast.NullLit{Start: start}
		case "function":
			return p.parseFunction(ast.FuncNormal, false)
		case "class":
			return p.parseClass(false)
		case "super":
			return p.parseSuper()
		case "new":
			return p.parseNew()
		case "import":
			return p.parseImportExpr()
		}

	case lexer.Punct:
		switch p.tok.Value {
		case "(":
			return p.parseParenOrArrow()
		case "[":
			return p.parseArrayLiteral()
		case "{":
			return p.parseObjectLiteral()
		case "/", "/=":
			// The parser knows an expression may begin here, so this '/' starts
			// a regular expression rather than a division.
			tok, err := p.lex.ScanRegExp(p.tok)
			if err != nil {
				p.fail(err)
			}
			p.tok = tok
			re := &ast.RegexpLit{Pattern: tok.Value, Flags: tok.Flags, Start: start}
			p.next()
			return re
		}
	}

	p.unexpected()
	return nil
}

// checkLegacyOctal rejects legacy octal and non-octal decimal literals in
// strict mode, where they are forbidden.
func (p *parser) checkLegacyOctal() {
	if !p.strict {
		return
	}
	raw := p.tok.Raw
	if len(raw) >= 2 && raw[0] == '0' && raw[1] >= '0' && raw[1] <= '9' {
		p.errorf("legacy octal literals are not allowed in strict mode")
	}
}

// parseSuper parses `super.x` and `super(...)`, both of which are only valid in
// the right kind of method.
func (p *parser) parseSuper() ast.Expr {
	start := p.tok.Pos
	p.next()
	switch {
	case p.isPunct("."), p.isPunct("["):
		if !p.allowSuperProp {
			p.errorf("\"super\" property access is only valid inside a method")
		}
	case p.isPunct("("):
		if !p.allowSuperCall {
			p.errorf("\"super()\" is only valid inside a derived constructor")
		}
	default:
		p.errorf("\"super\" must be followed by an argument list or a property access")
	}
	return &ast.Super{Start: start}
}

// parseImportExpr parses `import(...)` and `import.meta`.
func (p *parser) parseImportExpr() ast.Expr {
	start := p.tok.Pos
	p.next()
	if p.isPunct(".") {
		p.next()
		if !p.isContextual("meta") {
			p.errorf("expected \"import.meta\"")
		}
		p.next()
		if !p.module {
			p.errorf("\"import.meta\" is only valid inside a module")
		}
		return &ast.Member{
			Object:   &ast.Ident{Name: "import", Start: start},
			Property: &ast.Ident{Name: "meta", Start: start},
			Start:    start,
		}
	}
	if !p.isPunct("(") {
		p.errorf("expected \"(\" after \"import\"")
	}
	args := p.parseArguments()
	return &ast.Call{
		Callee: &ast.Ident{Name: "import", Start: start},
		Args:   args,
		Start:  start,
	}
}

// parseTemplate parses a template literal, including its substitutions.
func (p *parser) parseTemplate() *ast.TemplateLit {
	start := p.tok.Pos
	lit := &ast.TemplateLit{Start: start}

	for {
		tok := p.tok
		lit.Quasis = append(lit.Quasis, ast.TemplateElement{
			Cooked: tok.Value,
			// A part with an invalid escape has no cooked value, which is only
			// legal in a tagged template. The compiler enforces that.
			Valid: tok.TemplateValid,
			Raw:   tok.Raw,
		})
		// Template and TemplateTail both end the literal; Head and Mid are
		// followed by a substitution.
		if tok.Kind == lexer.Template || tok.Kind == lexer.TemplateTail {
			p.next()
			return lit
		}
		p.next()

		// Parse the substitution expression up to its closing brace.
		saved := p.noIn
		p.noIn = false
		lit.Exprs = append(lit.Exprs, p.parseExpr())
		p.noIn = saved

		if !p.isPunct("}") {
			p.errorf("expected \"}\" to close a template substitution")
		}
		// Re-scan the '}' as the resumption of the template.
		tok, err := p.lex.ScanTemplateTail(p.tok)
		if err != nil {
			p.fail(err)
		}
		p.tok = tok
	}
}

// parseArrayLiteral parses an array literal, which may contain holes and
// spreads, and may later be reinterpreted as a destructuring pattern.
func (p *parser) parseArrayLiteral() ast.Expr {
	start := p.tok.Pos
	p.expectPunct("[")
	saved := p.noIn
	p.noIn = false
	defer func() { p.noIn = saved }()

	lit := &ast.ArrayLit{Start: start}
	for !p.isPunct("]") {
		if p.isPunct(",") {
			// An elision leaves a nil element.
			p.next()
			lit.Elements = append(lit.Elements, nil)
			continue
		}
		if p.isPunct("...") {
			spreadStart := p.tok.Pos
			p.next()
			lit.Elements = append(lit.Elements, &ast.Spread{Arg: p.parseAssign(), Start: spreadStart})
		} else {
			lit.Elements = append(lit.Elements, p.parseAssign())
		}
		if !p.isPunct("]") {
			p.expectPunct(",")
		}
	}
	p.expectPunct("]")
	return lit
}

// parseObjectLiteral parses an object literal, which may later be reinterpreted
// as a destructuring pattern.
func (p *parser) parseObjectLiteral() ast.Expr {
	start := p.tok.Pos
	p.expectPunct("{")
	saved := p.noIn
	p.noIn = false
	defer func() { p.noIn = saved }()

	lit := &ast.ObjectLit{Start: start}
	for !p.isPunct("}") {
		lit.Props = append(lit.Props, p.parseObjectProperty())
		if !p.isPunct("}") {
			p.expectPunct(",")
		}
	}
	p.expectPunct("}")
	return lit
}

// parseObjectProperty parses one member of an object literal.
func (p *parser) parseObjectProperty() ast.Property {
	start := p.tok.Pos

	if p.isPunct("...") {
		p.next()
		return ast.Property{Kind: ast.PropSpread, Value: p.parseAssign(), Start: start}
	}

	// Accessors and generator/async methods are all introduced by a prefix
	// token that is itself a valid property name, so each needs one token of
	// lookahead to disambiguate `get: 1` from `get x() {}`.
	async, generator := false, false
	if p.isContextual("async") {
		mark := p.mark()
		p.next()
		if p.startsPropertyName() && !p.isPunct("(") && !p.isPunct(":") &&
			!p.isPunct(",") && !p.isPunct("}") && !p.isPunct("=") && !p.tok.NewlineBefore {
			async = true
		} else {
			p.reset(mark)
		}
	}
	if p.isPunct("*") {
		p.next()
		generator = true
	}
	if !async && !generator && (p.isContextual("get") || p.isContextual("set")) {
		kind := ast.PropGet
		if p.tok.Value == "set" {
			kind = ast.PropSet
		}
		mark := p.mark()
		p.next()
		if p.startsPropertyName() && !p.isPunct("(") && !p.isPunct(":") &&
			!p.isPunct(",") && !p.isPunct("}") && !p.isPunct("=") {
			key, computed := p.parsePropertyName()
			fn := p.parseMethodBody(ast.FuncGetter, false, false)
			if kind == ast.PropSet {
				fn.Kind = ast.FuncSetter
			}
			p.checkAccessorArity(kind, fn)
			return ast.Property{Kind: kind, Key: key, Value: fn, Computed: computed, Start: start}
		}
		p.reset(mark)
	}

	key, computed := p.parsePropertyName()

	switch {
	case p.isPunct("("):
		fn := p.parseMethodBody(ast.FuncMethod, async, generator)
		return ast.Property{Kind: ast.PropInit, Key: key, Value: fn, Computed: computed, Method: true, Start: start}

	case p.isPunct(":"):
		p.next()
		return ast.Property{Kind: ast.PropInit, Key: key, Value: p.parseAssign(), Computed: computed, Start: start}
	}

	// Shorthand: `{a}` or `{a = 1}`, the latter only valid as a pattern.
	id, ok := key.(*ast.Ident)
	if !ok || computed {
		p.errorf("expected \":\" after a property name")
	}
	if lexer.IsReservedWord(id.Name) {
		p.errorAt(p.tok, "%q cannot be used as a shorthand property", id.Name)
	}
	if p.isPunct("=") {
		p.next()
		def := p.parseAssign()
		return ast.Property{
			Kind:      ast.PropInit,
			Key:       key,
			Value:     &ast.AssignPattern{Target: id, Default: def, Start: start},
			Shorthand: true,
			Start:     start,
		}
	}
	return ast.Property{Kind: ast.PropInit, Key: key, Value: id, Shorthand: true, Start: start}
}

// checkAccessorArity enforces that a getter takes no parameters and a setter
// takes exactly one non-rest parameter.
func (p *parser) checkAccessorArity(kind ast.PropKind, fn *ast.FuncLit) {
	if kind == ast.PropGet && len(fn.Params) != 0 {
		p.errorf("a getter must have no parameters")
	}
	if kind == ast.PropSet {
		if len(fn.Params) != 1 {
			p.errorf("a setter must have exactly one parameter")
		}
		if _, isRest := fn.Params[0].(*ast.RestElement); isRest {
			p.errorf("a setter parameter cannot be a rest element")
		}
	}
}

// startsPropertyName reports whether the current token can begin a property
// name, which the accessor and async lookahead need.
func (p *parser) startsPropertyName() bool {
	switch p.tok.Kind {
	case lexer.Ident, lexer.Keyword, lexer.String, lexer.Number, lexer.PrivateIdent:
		return true
	case lexer.Punct:
		return p.tok.Value == "[" || p.tok.Value == "*"
	}
	return false
}

// parsePropertyName parses a property key, reporting whether it was computed.
func (p *parser) parsePropertyName() (ast.Expr, bool) {
	start := p.tok.Pos
	switch p.tok.Kind {
	case lexer.String:
		v := p.tok.Value
		p.next()
		return p.nodes.str(v, start), false
	case lexer.Number:
		v := p.tok.Num
		p.next()
		return p.nodes.number(v, start), false
	case lexer.PrivateIdent:
		v := p.tok.Value
		p.next()
		return &ast.PrivateName{Name: v, Start: start}, false
	case lexer.Punct:
		if p.tok.Value == "[" {
			p.next()
			key := p.parseAssign()
			p.expectPunct("]")
			return key, true
		}
	}
	return p.parseIdentName(), false
}

// startsTemplate reports whether the current token begins a template literal,
// which is either a complete template or the head of one with substitutions.
func (p *parser) startsTemplate() bool {
	return p.tok.Kind == lexer.Template || p.tok.Kind == lexer.TemplateHead
}
