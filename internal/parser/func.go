package parser

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
)

// parseFunction parses a function expression or declaration, with the
// `function` keyword as the current token.
func (p *parser) parseFunction(kind ast.FuncKind, isDecl bool) *ast.FuncLit {
	start := p.tok.Pos
	p.expectKeyword("function")
	generator := p.eatPunct("*")
	return p.parseFunctionRest(start, kind, false, generator, isDecl)
}

// parseFunctionRest parses a function from just after its `function *` prefix,
// which is shared by the sync and async forms.
func (p *parser) parseFunctionRest(start int, kind ast.FuncKind, async, generator, isDecl bool) *ast.FuncLit {
	fn := &ast.FuncLit{Kind: kind, Async: async, Generator: generator, Start: start}

	// A function expression's own name is in scope inside its body, so it is
	// parsed under the new function's yield/await rules rather than the
	// enclosing ones. A declaration's name binds in the enclosing scope and so
	// uses the enclosing rules.
	if p.tok.Kind == lexer.Ident {
		if isDecl {
			fn.Name = p.parseBindingIdent()
		} else {
			outerYield, outerAwait := p.allowYield, p.allowAwait
			p.allowYield, p.allowAwait = generator, async
			fn.Name = p.parseBindingIdent()
			p.allowYield, p.allowAwait = outerYield, outerAwait
		}
	} else if isDecl {
		p.errorf("a function declaration requires a name")
	}

	p.parseFunctionParamsAndBody(fn)
	return fn
}

// parseFunctionParamsAndBody parses a parameter list and body under the
// context that fn's flags imply.
func (p *parser) parseFunctionParamsAndBody(fn *ast.FuncLit) {
	ctx := p.saveContext()
	defer p.restoreContext(ctx)

	p.inFunc = true
	p.inLoop = false
	p.inSwitch = false
	p.noIn = false
	p.allowYield = fn.Generator
	p.allowAwait = fn.Async
	p.allowNewTarget = true
	p.labels = make(map[string]bool)

	switch fn.Kind {
	case ast.FuncMethod, ast.FuncGetter, ast.FuncSetter:
		p.allowSuperProp = true
		p.allowSuperCall = false
	case ast.FuncConstructor:
		p.allowSuperProp = true
		// allowSuperCall is set by the caller, which knows whether the class
		// has a heritage clause.
	default:
		p.allowSuperProp = false
		p.allowSuperCall = false
	}

	fn.Params = p.parseParams()
	fn.Body = p.parseFunctionBody(fn)
	fn.Strict = p.strict
}

// parseParams parses a parenthesized formal parameter list.
func (p *parser) parseParams() []ast.Expr {
	p.expectPunct("(")
	var params []ast.Expr
	for !p.isPunct(")") {
		if p.isPunct("...") {
			start := p.tok.Pos
			p.next()
			params = append(params, &ast.RestElement{Arg: p.parseBindingTarget(), Start: start})
			// A rest parameter must be last and cannot have a default.
			if p.isPunct(",") {
				p.errorf("a rest parameter must be the last parameter")
			}
			break
		}
		target := p.parseBindingTarget()
		if p.eatPunct("=") {
			target = &ast.AssignPattern{Target: target, Default: p.parseAssign(), Start: target.Pos()}
		}
		params = append(params, target)
		if !p.eatPunct(",") {
			break
		}
	}
	p.expectPunct(")")
	return params
}

// parseBindingTarget parses an identifier or a destructuring pattern in a
// binding position.
func (p *parser) parseBindingTarget() ast.Expr {
	switch {
	case p.isPunct("["):
		return p.toPattern(p.parseArrayLiteral(), true)
	case p.isPunct("{"):
		return p.toPattern(p.parseObjectLiteral(), true)
	default:
		return p.parseBindingIdent()
	}
}

// parseFunctionBody parses a brace-enclosed function body, including its
// directive prologue.
func (p *parser) parseFunctionBody(fn *ast.FuncLit) []ast.Stmt {
	p.expectPunct("{")
	// The prologue may turn on strict mode, which then applies to the whole
	// body and retroactively constrains the parameter list.
	wasStrict := p.strict
	body := p.parseDirectivePrologue(func() bool { return p.isPunct("}") })
	if !wasStrict && p.strict {
		p.checkStrictParams(fn.Params)
	}
	body = append(body, p.parseStatements(func() bool { return p.isPunct("}") })...)
	p.expectPunct("}")
	return body
}

// checkStrictParams re-validates a parameter list after a "use strict"
// directive turned on strict mode, where a simple parameter list is required
// and duplicate or restricted names are forbidden.
func (p *parser) checkStrictParams(params []ast.Expr) {
	seen := make(map[string]bool, len(params))
	for _, param := range params {
		id, ok := param.(*ast.Ident)
		if !ok {
			p.errorf("a function with a non-simple parameter list cannot declare \"use strict\"")
		}
		switch id.Name {
		case "eval", "arguments", "yield", "let", "static", "implements",
			"interface", "package", "private", "protected", "public":
			p.errorf("%q cannot be a parameter name in strict mode", id.Name)
		}
		if seen[id.Name] {
			p.errorf("duplicate parameter %q is not allowed in strict mode", id.Name)
		}
		seen[id.Name] = true
	}
}

// parseMethodBody parses the parameter list and body of a method, whose name
// has already been consumed.
func (p *parser) parseMethodBody(kind ast.FuncKind, async, generator bool) *ast.FuncLit {
	fn := &ast.FuncLit{Kind: kind, Async: async, Generator: generator, Start: p.tok.Pos}
	p.parseFunctionParamsAndBody(fn)
	return fn
}

// ---------------------------------------------------------------------------
// Arrow functions
// ---------------------------------------------------------------------------

// tryParseSingleParamArrow recognizes `x => ...`, the one arrow form that a
// cover grammar cannot express, since `x` alone is already a complete
// expression. It costs a single token of lookahead.
func (p *parser) tryParseSingleParamArrow() ast.Expr {
	if p.tok.Kind != lexer.Ident {
		return nil
	}
	m := p.mark()
	nameTok := p.tok
	p.next()
	if !p.isPunct("=>") || p.tok.NewlineBefore {
		p.reset(m)
		return nil
	}
	p.checkBindingName(nameTok.Value, nameTok)
	params := []ast.Expr{&ast.Ident{Name: nameTok.Value, Start: nameTok.Pos}}
	return p.parseArrowBody(params, nameTok.Pos, false)
}

// parseArrowBody parses `=> body`, which is either a concise expression body or
// a brace-enclosed statement body.
func (p *parser) parseArrowBody(params []ast.Expr, start int, async bool) ast.Expr {
	p.expectPunct("=>")

	fn := &ast.FuncLit{
		Params: params,
		Kind:   ast.FuncArrow,
		Async:  async,
		Start:  start,
	}

	ctx := p.saveContext()
	defer p.restoreContext(ctx)

	p.inFunc = true
	p.inLoop = false
	p.inSwitch = false
	p.noIn = false
	p.labels = make(map[string]bool)
	// An arrow body is always [~Yield]; `await` is available only when the
	// arrow itself is async. Everything else -- this, super, new.target --
	// is inherited from the enclosing function, so those flags are left alone.
	p.allowYield = false
	p.allowAwait = async

	if p.isPunct("{") {
		fn.Body = p.parseFunctionBody(fn)
	} else {
		// A concise body is an expression, represented as a synthesized return
		// so that the compiler has only one shape to handle.
		bodyStart := p.tok.Pos
		expr := p.parseAssign()
		fn.Body = []ast.Stmt{&ast.ReturnStmt{Arg: expr, Start: bodyStart}}
		fn.ExprBody = true
	}
	fn.Strict = p.strict
	return fn
}

// parseParenOrArrow parses a parenthesized expression or an arrow function's
// parameter list. The two are indistinguishable until the closing paren, so
// both are parsed into the same cover list and reinterpreted afterwards.
func (p *parser) parseParenOrArrow() ast.Expr {
	start := p.tok.Pos
	p.expectPunct("(")

	saved := p.noIn
	p.noIn = false

	var items []ast.Expr
	sawRest := false
	trailingComma := false

	for !p.isPunct(")") {
		if p.isPunct("...") {
			restStart := p.tok.Pos
			p.next()
			target := p.parseBindingTarget()
			// `...x = 1` is never valid, in either reading.
			if p.isPunct("=") {
				p.errorf("a rest parameter cannot have a default value")
			}
			items = append(items, &ast.RestElement{Arg: target, Start: restStart})
			sawRest = true
			break
		}
		items = append(items, p.parseAssign())
		if !p.eatPunct(",") {
			break
		}
		if p.isPunct(")") {
			trailingComma = true
			break
		}
	}
	p.noIn = saved
	p.expectPunct(")")

	if p.isPunct("=>") && !p.tok.NewlineBefore {
		return p.parseArrowBody(p.paramsFromCover(items), start, false)
	}

	// Not an arrow, so the forms that only parameters allow are errors.
	switch {
	case sawRest:
		p.errorf("a rest element is only valid in a parameter list")
	case trailingComma:
		p.errorf("a trailing comma is only valid in a parameter list")
	case len(items) == 0:
		p.errorf("empty parentheses are only valid as an arrow parameter list")
	}
	if len(items) == 1 {
		if l, ok := items[0].(*ast.Logical); ok {
			l.Paren = true
		}
		return items[0]
	}
	return &ast.Sequence{Exprs: items, Start: start}
}

// paramsFromCover reinterprets a parenthesized expression list as an arrow
// function's parameters.
func (p *parser) paramsFromCover(items []ast.Expr) []ast.Expr {
	params := make([]ast.Expr, len(items))
	for i, item := range items {
		if rest, ok := item.(*ast.RestElement); ok {
			if i != len(items)-1 {
				p.errorf("a rest parameter must be the last parameter")
			}
			params[i] = rest
			continue
		}
		params[i] = p.toPattern(item, true)
	}
	return params
}

// tryParseAsyncFunction recognizes the three forms that the contextual keyword
// `async` can introduce, with `async` as the current token. It returns nil if
// `async` is being used as an ordinary identifier.
func (p *parser) tryParseAsyncFunction() ast.Expr {
	m := p.mark()
	start := p.tok.Pos
	p.next()

	// A newline after `async` always ends the expression, by ASI.
	if p.tok.NewlineBefore {
		p.reset(m)
		return nil
	}

	switch {
	case p.isKeyword("function"):
		p.next()
		generator := p.eatPunct("*")
		return p.parseFunctionRest(start, ast.FuncNormal, true, generator, false)

	case p.tok.Kind == lexer.Ident:
		// `async x => ...`
		nameTok := p.tok
		p.next()
		if !p.isPunct("=>") || p.tok.NewlineBefore {
			p.reset(m)
			return nil
		}
		params := []ast.Expr{&ast.Ident{Name: nameTok.Value, Start: nameTok.Pos}}
		return p.parseArrowBody(params, start, true)

	case p.isPunct("("):
		// `async (...) => ...`, or a call to a function named `async`. Parsing
		// the parenthesized list settles it.
		if arrow := p.tryParseAsyncParenArrow(start); arrow != nil {
			return arrow
		}
		p.reset(m)
		return nil
	}

	p.reset(m)
	return nil
}

// tryParseAsyncParenArrow parses `(...)` after `async` and returns an arrow
// function if `=>` follows, or nil if the parentheses were a call's arguments.
func (p *parser) tryParseAsyncParenArrow(start int) ast.Expr {
	m := p.mark()
	// Parsing the list can fail for input that is a valid call but not a valid
	// parameter list, such as `async(1 + 2)`. Recover and let the caller retry
	// as a call.
	var items []ast.Expr
	ok := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				if _, isSyntax := r.(*Error); isSyntax {
					ok = false
					return
				}
				panic(r)
			}
		}()
		p.expectPunct("(")
		for !p.isPunct(")") {
			if p.isPunct("...") {
				restStart := p.tok.Pos
				p.next()
				items = append(items, &ast.RestElement{Arg: p.parseBindingTarget(), Start: restStart})
				break
			}
			items = append(items, p.parseAssign())
			if !p.eatPunct(",") {
				break
			}
		}
		p.expectPunct(")")
		return true
	}()

	if !ok || !p.isPunct("=>") || p.tok.NewlineBefore {
		p.reset(m)
		return nil
	}
	return p.parseArrowBody(p.paramsFromCover(items), start, true)
}

// ---------------------------------------------------------------------------
// Classes
// ---------------------------------------------------------------------------

// parseClass parses a class declaration or expression, with `class` as the
// current token.
func (p *parser) parseClass(isDecl bool) *ast.ClassLit {
	start := p.tok.Pos
	p.expectKeyword("class")

	// A class body is always strict, including its heritage clause and name.
	ctx := p.saveContext()
	defer p.restoreContext(ctx)
	p.strict = true

	cls := &ast.ClassLit{Start: start}
	if p.tok.Kind == lexer.Ident {
		cls.Name = p.parseBindingIdent()
	} else if isDecl {
		p.errorf("a class declaration requires a name")
	}

	if p.eatKeyword("extends") {
		cls.Extends = p.parseLeftHandSide()
	}

	p.expectPunct("{")
	p.inClassBody = true
	sawConstructor := false

	for !p.isPunct("}") {
		// Stray semicolons between class members are permitted.
		if p.eatPunct(";") {
			continue
		}
		p.parseClassMember(cls, &sawConstructor)
	}
	p.expectPunct("}")
	return cls
}

// parseClassMember parses one method, field or static block and appends it to
// the class.
func (p *parser) parseClassMember(cls *ast.ClassLit, sawConstructor *bool) {
	start := p.tok.Pos

	// `static` is contextual: `static x` declares a static member, but
	// `static() {}` and `static = 1` declare a member named "static".
	isStatic := false
	if p.isContextual("static") {
		m := p.mark()
		p.next()
		if p.isPunct("(") || p.isPunct("=") || p.isPunct(";") || p.isPunct("}") {
			p.reset(m)
		} else {
			isStatic = true
		}
	}

	if isStatic && p.isPunct("{") {
		cls.StaticBlocks = append(cls.StaticBlocks, p.parseStaticBlock())
		return
	}

	async, generator := false, false
	if p.isContextual("async") {
		m := p.mark()
		p.next()
		if p.startsPropertyName() && !p.isPunct("(") && !p.isPunct("=") &&
			!p.isPunct(";") && !p.isPunct("}") && !p.tok.NewlineBefore {
			async = true
		} else {
			p.reset(m)
		}
	}
	if p.isPunct("*") {
		p.next()
		generator = true
	}

	// Accessors, again contextual.
	if !async && !generator && (p.isContextual("get") || p.isContextual("set")) {
		kind := ast.PropGet
		fnKind := ast.FuncGetter
		if p.tok.Value == "set" {
			kind, fnKind = ast.PropSet, ast.FuncSetter
		}
		m := p.mark()
		p.next()
		if p.startsPropertyName() && !p.isPunct("(") && !p.isPunct("=") &&
			!p.isPunct(";") && !p.isPunct("}") {
			key, computed := p.parsePropertyName()
			fn := p.parseMethodBody(fnKind, false, false)
			p.checkAccessorArity(kind, fn)
			cls.Members = append(cls.Members, ast.Property{
				Kind: kind, Key: key, Value: fn, Computed: computed,
				Static: isStatic, Start: start,
			})
			return
		}
		p.reset(m)
	}

	key, computed := p.parsePropertyName()

	if p.isPunct("(") {
		kind := ast.FuncMethod
		if !isStatic && !computed && isConstructorKey(key) {
			if *sawConstructor {
				p.errorf("a class may have only one constructor")
			}
			if async || generator {
				p.errorf("a constructor cannot be async or a generator")
			}
			*sawConstructor = true
			kind = ast.FuncConstructor
		}

		// Only a derived class's constructor may call super().
		savedSuperCall := p.allowSuperCall
		if kind == ast.FuncConstructor {
			p.allowSuperCall = cls.Extends != nil
		}
		fn := p.parseMethodBody(kind, async, generator)
		p.allowSuperCall = savedSuperCall

		cls.Members = append(cls.Members, ast.Property{
			Kind: ast.PropInit, Key: key, Value: fn, Computed: computed,
			Method: true, Static: isStatic, Start: start,
		})
		return
	}

	// A field definition.
	if async || generator {
		p.errorf("a class field cannot be async or a generator")
	}
	if !computed && isConstructorKey(key) {
		p.errorf("a class field cannot be named \"constructor\"")
	}
	field := ast.ClassField{Key: key, Computed: computed, Static: isStatic, Start: start}
	if p.eatPunct("=") {
		// A field initializer runs with the instance as `this` and may use
		// super properties, but it is not a function body for break/continue.
		ctx := p.saveContext()
		p.inFunc = true
		p.allowSuperProp = true
		p.allowSuperCall = false
		p.allowYield = false
		p.allowAwait = false
		p.labels = make(map[string]bool)
		field.Value = p.parseAssign()
		p.restoreContext(ctx)
	}
	p.semicolon()
	cls.Fields = append(cls.Fields, field)
}

// parseStaticBlock parses a `static { ... }` initializer block.
func (p *parser) parseStaticBlock() []ast.Stmt {
	ctx := p.saveContext()
	defer p.restoreContext(ctx)

	p.inFunc = true
	p.inLoop = false
	p.inSwitch = false
	p.allowSuperProp = true
	p.allowSuperCall = false
	p.allowYield = false
	p.allowAwait = false
	p.allowNewTarget = true
	p.labels = make(map[string]bool)

	p.expectPunct("{")
	body := p.parseStatements(func() bool { return p.isPunct("}") })
	p.expectPunct("}")
	return body
}

// isConstructorKey reports whether a non-computed key names the constructor.
func isConstructorKey(key ast.Expr) bool {
	switch k := key.(type) {
	case *ast.Ident:
		return k.Name == "constructor"
	case *ast.StringLit:
		return k.Value == "constructor"
	}
	return false
}
