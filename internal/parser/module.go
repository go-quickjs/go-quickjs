package parser

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/lexer"
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
			// reserved word, since it is a property of the source module.
			spec.Imported = p.parseIdentName().Name
			spec.Local = spec.Imported
			if p.eatContextual("as") {
				spec.Local = p.parseBindingIdent().Name
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
			decl.Alias = p.parseIdentName().Name
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
		for !p.isPunct("}") {
			spec := ast.ExportSpecifier{Start: p.tok.Pos}
			spec.Local = p.parseIdentName().Name
			spec.Exported = spec.Local
			if p.eatContextual("as") {
				spec.Exported = p.parseIdentName().Name
			}
			decl.Specifiers = append(decl.Specifiers, spec)
			if !p.eatPunct(",") {
				break
			}
		}
		p.expectPunct("}")
		// A clause may re-export from another module.
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
