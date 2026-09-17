package parser

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Import and export declarations.
//
// Both are only legal at the top level of a module, which the parser enforces:
// in a script they are ordinary identifiers, and nested inside a block they are
// an error even in a module.

// parseImportDecl parses an import declaration, with `import` current.
func (p *parser) parseImportDecl() ast.Stmt {
	start := p.tok.Pos
	if !p.module {
		p.errorf("\"import\" is only valid inside a module")
	}
	if p.depth > 1 {
		p.errorf("an import declaration must be at the top level of a module")
	}
	p.next()

	decl := &ast.ImportDecl{Start: start}

	// `import "module"` imports for side effects only.
	if p.tok.Kind == lexer.String {
		decl.Source = p.tok.Value
		p.next()
		p.semicolon()
		return decl
	}

	// A default import comes first when present.
	if p.tok.Kind == lexer.Ident {
		decl.Specifiers = append(decl.Specifiers, ast.ImportSpecifier{
			Kind:  ast.ImportDefault,
			Local: p.tok.Value,
			Start: p.tok.Pos,
		})
		p.next()
		if p.isPunct(",") {
			p.next()
		}
	}

	switch {
	case p.isPunct("*"):
		nsStart := p.tok.Pos
		p.next()
		if !p.eatContextual("as") {
			p.errorf("expected \"as\" after \"*\"")
		}
		decl.Specifiers = append(decl.Specifiers, ast.ImportSpecifier{
			Kind:  ast.ImportNamespace,
			Local: p.parseBindingIdent().Name,
			Start: nsStart,
		})

	case p.isPunct("{"):
		p.next()
		for !p.isPunct("}") {
			spec := ast.ImportSpecifier{Kind: ast.ImportNamed, Start: p.tok.Pos}
			// The imported name may be any identifier name, including a
			// reserved word, since it is a property of the source module --
			// or a string, which is how a module exports a name no identifier
			// can spell.
			isString := p.tok.Kind == lexer.String
			spec.Imported = p.parseModuleExportName()
			spec.Local = spec.Imported
			if p.eatContextual("as") {
				spec.Local = p.parseBindingIdent().Name
			} else if isString {
				p.errorf("an imported string name needs a local name")
			}
			decl.Specifiers = append(decl.Specifiers, spec)
			if !p.eatPunct(",") {
				break
			}
		}
		p.expectPunct("}")
	}

	if !p.eatContextual("from") {
		p.errorf("expected \"from\" in an import declaration")
	}
	if p.tok.Kind != lexer.String {
		p.errorf("the module specifier must be a string")
	}
	decl.Source = p.tok.Value
	p.next()
	p.semicolon()
	return decl
}

// parseExportDecl parses an export declaration, with `export` current.
func (p *parser) parseExportDecl() ast.Stmt {
	start := p.tok.Pos
	if !p.module {
		p.errorf("\"export\" is only valid inside a module")
	}
	if p.depth > 1 {
		p.errorf("an export declaration must be at the top level of a module")
	}
	p.next()

	decl := &ast.ExportDecl{Start: start}

	switch {
	case p.isKeyword("default"):
		p.next()
		decl.Default = true
		// A default export of a function or class declaration binds its name
		// as well, so those are parsed as declarations rather than
		// expressions.
		switch {
		case p.isKeyword("function"):
			decl.Decl = &ast.FuncDecl{Fn: p.parseFunction(ast.FuncNormal, false), Start: start}
		case p.isContextual("async") && p.nextIsKeywordOnSameLine("function"):
			// An async function declaration binds its name too, which parsing
			// it as an expression would not.
			p.next()
			p.expectKeyword("function")
			generator := p.eatPunct("*")
			decl.Decl = &ast.FuncDecl{
				Fn:    p.parseFunctionRest(start, ast.FuncNormal, true, generator, false),
				Start: start,
			}
		case p.isKeyword("class"):
			decl.Decl = &ast.ClassDecl{Class: p.parseClass(false), Start: start}
		default:
			decl.DefaultExpr = p.parseAssign()
			p.semicolon()
		}
		return decl

	case p.isPunct("*"):
		p.next()
		decl.All = true
		if p.eatContextual("as") {
			decl.Alias = p.parseModuleExportName()
		}
		if !p.eatContextual("from") {
			p.errorf("expected \"from\" after \"export *\"")
		}
		if p.tok.Kind != lexer.String {
			p.errorf("the module specifier must be a string")
		}
		decl.Source = p.tok.Value
		p.next()
		p.semicolon()
		return decl

	case p.isPunct("{"):
		p.next()
		// A local name spelled as a string names nothing: only a re-export,
		// which asks another module for it, may be written that way.
		stringLocal := -1
		for !p.isPunct("}") {
			spec := ast.ExportSpecifier{Start: p.tok.Pos}
			if p.tok.Kind == lexer.String && stringLocal < 0 {
				stringLocal = p.tok.Pos
			}
			spec.Local = p.parseModuleExportName()
			spec.Exported = spec.Local
			if p.eatContextual("as") {
				spec.Exported = p.parseModuleExportName()
			}
			decl.Specifiers = append(decl.Specifiers, spec)
			if !p.eatPunct(",") {
				break
			}
		}
		p.expectPunct("}")
		// A clause may re-export from another module.
		if stringLocal >= 0 && !p.isContextual("from") {
			p.errorAt(lexer.Token{Pos: stringLocal}, "a local export name must be an identifier")
		}
		if p.eatContextual("from") {
			if p.tok.Kind != lexer.String {
				p.errorf("the module specifier must be a string")
			}
			decl.Source = p.tok.Value
			p.next()
		}
		p.semicolon()
		return decl
	}

	// `export` followed by a declaration exports the names it binds.
	decl.Decl = p.parseStatement()
	return decl
}

// parseModuleExportName parses the name a module exports something under.
//
// It is an identifier name -- reserved words included, since it is a property
// of the namespace rather than a binding -- or a string literal, which is what
// lets a module export a name no identifier can spell. A string that is not
// well-formed UTF-16 is not one: the name would have no unambiguous spelling
// for another module to ask for it by.
func (p *parser) parseModuleExportName() string {
	if p.tok.Kind != lexer.String {
		return p.parseIdentName().Name
	}
	name := p.tok.Value
	if !wtf8.WellFormed(name) {
		p.errorf("a module export name must be well-formed")
	}
	p.next()
	return name
}
