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
	p.inParams = false
	// A function has its own arguments object even when declared inside a
	// class field initializer.
	p.noArguments = false
	p.noAwaitIdent = false
	p.labels = make(map[string]bool)

	switch fn.Kind {
	case ast.FuncMethod, ast.FuncGetter, ast.FuncSetter:
		p.allowSuperProp = true
		p.allowSuperCall = false
	case ast.FuncConstructor, ast.FuncDerivedConstructor:
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
	p.checkParamNames(fn)
}

// checkParamNames rejects a parameter list that binds a name twice.
//
// Two parameters may share a name only where the list is simple and the code
// is sloppy -- which is what makes the arguments object's mapping well defined,
// since only the last of them is mapped. Everywhere else the names must be
// unique: in strict mode, in a list with a default, a pattern or a rest
// element, and in every method and arrow, whose grammar demands it outright.
//
// It runs after the body, because a "use strict" directive there applies to the
// parameter list too.
func (p *parser) checkParamNames(fn *ast.FuncLit) {
	unique := fn.Strict || fn.Async || !simpleParams(fn.Params)
	switch fn.Kind {
	case ast.FuncArrow, ast.FuncMethod, ast.FuncGetter, ast.FuncSetter,
		ast.FuncConstructor, ast.FuncDerivedConstructor:
		unique = true
	}
	if !unique {
		return
	}
	seen := make(map[string]bool, len(fn.Params))
	var names []string
	for _, prm := range fn.Params {
		names = boundNames(prm, names[:0])
		for _, n := range names {
			if seen[n] {
				p.errorf("parameter %q is bound twice", n)
			}
			seen[n] = true
		}
	}
}

// simpleParams reports whether every parameter is a plain identifier.
func simpleParams(params []ast.Expr) bool {
	for _, prm := range params {
		if _, ok := prm.(*ast.Ident); !ok {
			return false
		}
	}
	return true
}

// boundNames appends the names a binding target introduces.
func boundNames(e ast.Expr, out []string) []string {
	switch n := e.(type) {
	case *ast.Ident:
		out = append(out, n.Name)
	case *ast.AssignPattern:
		out = boundNames(n.Target, out)
	case *ast.RestElement:
		out = boundNames(n.Arg, out)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			if el != nil {
				out = boundNames(el, out)
			}
		}
		if n.Rest != nil {
			out = boundNames(n.Rest, out)
		}
	case *ast.ObjectPattern:
		for _, prop := range n.Props {
			out = boundNames(prop.Value, out)
		}
		if n.Rest != nil {
			out = boundNames(n.Rest, out)
		}
	}
	return out
}

// parseParams parses a parenthesized formal parameter list.
func (p *parser) parseParams() []ast.Expr {
	// A default value is an expression, but not one that may suspend: the
	// parameters are bound as part of the call. A function nested in one has
	// its own rules again, which parseFunctionParamsAndBody restores.
	saved := p.inParams
	p.inParams = true
	defer func() { p.inParams = saved }()

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
	// The directive is what a non-simple parameter list forbids, whatever the
	// surrounding code's mode: a class method is strict already and still may
	// not carry one.
	if p.sawUseStrict {
		p.checkStrictParams(fn.Params)
	} else if !wasStrict && p.strict {
		p.checkStrictParams(fn.Params)
	}
	body = append(body, p.parseStatements(func() bool { return p.isPunct("}") })...)
	// The closing brace has not been consumed yet, so its position plus one is
	// where the function ends.
	fn.End = p.tok.Pos + 1
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
// parseMethodBody parses a method's parameters and body.
//
// start is where the method definition began -- at `async`, `*`, `get` or the
// key, whichever came first -- rather than at the parenthesis, because that is
// the source text Function.prototype.toString has to return. It excludes
// `static`, which belongs to the class element rather than to the method.
func (p *parser) parseMethodBody(kind ast.FuncKind, async, generator bool, start int) *ast.FuncLit {
	fn := &ast.FuncLit{Kind: kind, Async: async, Generator: generator, Start: start}
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
	// A class static block forbids `await` as an identifier in what it
	// contains, but not in a function written inside it: the rule is about the
	// block's own statements.
	p.noAwaitIdent = false
	// An arrow body is always [~Yield]; `await` is available only when the
	// arrow itself is async. Everything else -- this, super, new.target --
	// is inherited from the enclosing function, so those flags are left alone.
	p.allowYield = false
	p.allowAwait = async
	// The body is the arrow's own, so an enclosing parameter list no longer
	// applies: `async function f(a = async () => await 1) {}` is fine.
	p.inParams = false

	if p.isPunct("{") {
		fn.Body = p.parseFunctionBody(fn)
	} else {
		// A concise body is an expression, represented as a synthesized return
		// so that the compiler has only one shape to handle.
		bodyStart := p.tok.Pos
		expr := p.parseAssign()
		fn.Body = []ast.Stmt{&ast.ReturnStmt{Arg: expr, Start: bodyStart}}
		fn.ExprBody = true
		// A concise body ends wherever the expression did, which is just
		// before whatever token follows.
		fn.End = p.prevEnd
	}
	fn.Strict = p.strict
	p.checkParamNames(fn)
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
		// Parenthesizing changes what an expression may be used for, so the
		// nodes where it makes a difference remember it.
		switch n := items[0].(type) {
		case *ast.Logical:
			n.Paren = true
		case *ast.Assign:
			n.Paren = true
		case *ast.ArrayLit:
			n.Paren = true
		case *ast.ObjectLit:
			n.Paren = true
		}
		return items[0]
	}
	return &ast.Sequence{Exprs: items, Start: start}
}

// paramsFromCover reinterprets a parenthesized expression list as an arrow
// function's parameters.
func (p *parser) paramsFromCover(items []ast.Expr) []ast.Expr {
	params := make([]ast.Expr, len(items))
	for _, item := range items {
		// Arrow parameters may not contain a yield or await expression, even
		// though the enclosing context permits one. The cover grammar parsed
		// them as ordinary expressions, so the check happens on reinterpretation.
		if kind := containsSuspension(item); kind != "" {
			p.errorf("%q is not allowed in arrow function parameters", kind)
		}
	}
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
	// Private names must be unique within a class body, except that a getter
	// and a setter may share one.
	privateNames := map[string]privateKind{}

	for !p.isPunct("}") {
		// Stray semicolons between class members are permitted.
		if p.eatPunct(";") {
			continue
		}
		p.parseClassMember(cls, &sawConstructor, privateNames)
	}
	cls.End = p.tok.Pos + 1
	p.expectPunct("}")
	return cls
}

// parseClassMember parses one method, field or static block and appends it to
// the class.
func (p *parser) parseClassMember(cls *ast.ClassLit, sawConstructor *bool, privateNames map[string]privateKind) {
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

	// The method's own text begins here, after any `static`, which belongs to
	// the class element rather than to the function.
	defStart := p.tok.Pos

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
			p.checkClassMemberName(key, computed, isStatic, privateNames, kindOfAccessor(kind))
			// A constructor is the one member that has to be a plain method:
			// there is nothing for `new` to call if it is an accessor.
			if !isStatic && !computed && isConstructorKey(key) {
				p.errorf("a constructor cannot be an accessor")
			}
			fn := p.parseMethodBody(fnKind, false, false, defStart)
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
	p.checkClassMemberName(key, computed, isStatic, privateNames, privateOther)

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
		fn := p.parseMethodBody(kind, async, generator, defStart)
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
	if !computed {
		if name := propKeyText(key); name == "constructor" || (isStatic && name == "prototype") {
			p.errorf("a class field cannot be named %q", name)
		}
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
		// A field initializer is a function of its own, so new.target is
		// written there as it would be in any function -- it is simply always
		// undefined, since nothing constructs an initializer.
		p.allowNewTarget = true
		// It runs in a context with no arguments object, so naming one is an
		// early error rather than a runtime failure, and `await` is not an
		// identifier in it.
		p.noArguments = true
		p.noAwaitIdent = true
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
	// Like a field initializer, a static block has no arguments object, and
	// `await` is not an identifier in it.
	p.noArguments = true
	p.noAwaitIdent = true
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

// containsSuspension reports whether an expression contains a yield or await,
// naming whichever it found.
//
// Arrow parameters forbid both, so the cover list has to be checked once it is
// reinterpreted as a parameter list.
func containsSuspension(e ast.Expr) string {
	found := ""
	var walk func(ast.Expr)
	walk = func(n ast.Expr) {
		if found != "" || n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.Yield:
			found = "yield"
		case *ast.Await:
			found = "await"
		case *ast.AssignPattern:
			walk(v.Target)
			walk(v.Default)
		case *ast.Assign:
			walk(v.Target)
			walk(v.Value)
		case *ast.Binary:
			walk(v.Left)
			walk(v.Right)
		case *ast.Logical:
			walk(v.Left)
			walk(v.Right)
		case *ast.Conditional:
			walk(v.Test)
			walk(v.Cons)
			walk(v.Alt)
		case *ast.Unary:
			walk(v.Operand)
		case *ast.Call:
			walk(v.Callee)
			for _, a := range v.Args {
				walk(a)
			}
		case *ast.ArrayLit:
			for _, el := range v.Elements {
				walk(el)
			}
		case *ast.ArrayPattern:
			for _, el := range v.Elements {
				walk(el)
			}
			walk(v.Rest)
		case *ast.ObjectLit:
			for _, prop := range v.Props {
				walk(prop.Value)
			}
		case *ast.ObjectPattern:
			for _, prop := range v.Props {
				walk(prop.Value)
			}
			walk(v.Rest)
		case *ast.RestElement:
			walk(v.Arg)
		case *ast.Sequence:
			for _, x := range v.Exprs {
				walk(x)
			}
		case *ast.Spread:
			walk(v.Arg)
		}
	}
	walk(e)
	return found
}

// privateKind distinguishes the roles a private name can take, so that a getter
// and a setter sharing one can be told from a genuine duplicate.
type privateKind uint8

const (
	privateOther privateKind = iota
	privateGetter
	privateSetter
)

func kindOfAccessor(k ast.PropKind) privateKind {
	if k == ast.PropSet {
		return privateSetter
	}
	return privateGetter
}

// checkClassMemberName enforces the restrictions on what a class member may be
// called.
func (p *parser) checkClassMemberName(key ast.Expr, computed, isStatic bool,
	privateNames map[string]privateKind, kind privateKind) {
	if computed {
		return
	}
	name := propKeyText(key)

	// A static member may not be called "prototype": it would shadow the
	// object the class's instances inherit from.
	if isStatic && name == "prototype" {
		p.errorf("a static class member cannot be named \"prototype\"")
	}

	pn, isPrivate := key.(*ast.PrivateName)
	if !isPrivate {
		return
	}
	// #constructor is never a legal private name.
	if pn.Name == "constructor" {
		p.errorf("a private name cannot be #constructor")
	}
	prev, seen := privateNames[pn.Name]
	if !seen {
		privateNames[pn.Name] = kind
		return
	}
	// The one legal repeat is a getter paired with a setter.
	paired := (prev == privateGetter && kind == privateSetter) ||
		(prev == privateSetter && kind == privateGetter)
	if !paired {
		p.errorf("duplicate private name #%s", pn.Name)
	}
	privateNames[pn.Name] = privateOther
}

// propKeyText returns the textual form of a non-computed member name.
func propKeyText(key ast.Expr) string {
	switch k := key.(type) {
	case *ast.Ident:
		return k.Name
	case *ast.StringLit:
		return k.Value
	case *ast.PrivateName:
		return "#" + k.Name
	}
	return ""
}
