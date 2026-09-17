package parser

import "github.com/go-quickjs/go-quickjs/internal/ast"

// toPattern reinterprets an expression as a destructuring pattern.
//
// The parser cannot know whether `{a: b}` or `[a, b]` is a literal or a pattern
// until it reaches what follows: an `=` makes it a pattern, an `=>` makes the
// enclosing parentheses a parameter list. Rather than backtrack, the parser
// always builds the literal form and converts it here once the question is
// settled.
//
// binding selects the stricter rules that apply to declarations and parameters,
// where every leaf must be a plain identifier. In an assignment pattern a leaf
// may be any reference, so `[a.b] = c` is legal but `let [a.b] = c` is not.
func (p *parser) toPattern(e ast.Expr, binding bool) ast.Expr {
	switch n := e.(type) {
	case *ast.Ident:
		if binding {
			p.checkBindingName(n.Name, p.tok)
			return n
		}
		// An assignment target is an IdentifierReference, so it may not be a
		// word that is reserved where it stands -- which the cover grammar did
		// not check, since as an expression it was only ever going to be a
		// reference.
		if p.strict {
			switch n.Name {
			case "eval", "arguments":
				p.errorf("cannot assign to %q in strict mode", n.Name)
			case "implements", "interface", "let", "package", "private",
				"protected", "public", "static", "yield":
				p.errorf("%q is reserved in strict mode", n.Name)
			}
		}
		if p.allowYield && n.Name == "yield" {
			p.errorf("cannot assign to \"yield\" inside a generator")
		}
		if (p.allowAwait || p.module) && n.Name == "await" {
			p.errorf("cannot assign to \"await\" here")
		}
		return n

	case *ast.Member:
		if binding {
			p.errorf("a property access is not a valid binding target")
		}
		return n

	case *ast.ArrayLit:
		return p.arrayToPattern(n, binding)

	case *ast.ObjectLit:
		return p.objectToPattern(n, binding)

	case *ast.Assign:
		// `a = 1` inside a pattern position is a default value, not an
		// assignment. Only the plain `=` form can be reinterpreted.
		if n.Op != "=" {
			p.errorf("%q is not valid in a destructuring pattern", n.Op)
		}
		return &ast.AssignPattern{
			Target:  p.toPattern(n.Target, binding),
			Default: n.Value,
			Start:   n.Start,
		}

	case *ast.AssignPattern:
		// Already converted, which happens for object shorthand with a default.
		n.Target = p.toPattern(n.Target, binding)
		return n

	case *ast.ArrayPattern, *ast.ObjectPattern, *ast.RestElement:
		// Produced directly by parseBindingTarget; nothing to convert.
		return e
	}

	p.errorf("invalid destructuring target")
	return nil
}

// arrayToPattern converts an array literal to an array pattern, moving a
// trailing spread into the pattern's rest slot.
func (p *parser) arrayToPattern(lit *ast.ArrayLit, binding bool) ast.Expr {
	pat := &ast.ArrayPattern{Start: lit.Start}

	for i, el := range lit.Elements {
		if el == nil {
			// A hole stays a hole.
			pat.Elements = append(pat.Elements, nil)
			continue
		}
		if spread, ok := el.(*ast.Spread); ok {
			// The rest element must be last, and a comma after it is not
			// merely tolerated punctuation: there is nothing it could separate.
			if i != len(lit.Elements)-1 || lit.TrailingComma {
				p.errorf("a rest element must be the last element of a pattern")
			}
			target := p.toPattern(spread.Arg, binding)
			// `[...a = 1] = b` is never valid: a rest element takes no default.
			if _, hasDefault := target.(*ast.AssignPattern); hasDefault {
				p.errorf("a rest element cannot have a default value")
			}
			pat.Rest = target
			continue
		}
		pat.Elements = append(pat.Elements, p.toPattern(el, binding))
	}
	return pat
}

// objectToPattern converts an object literal to an object pattern, moving a
// trailing spread into the pattern's rest slot.
func (p *parser) objectToPattern(lit *ast.ObjectLit, binding bool) ast.Expr {
	pat := &ast.ObjectPattern{Start: lit.Start}

	for i, prop := range lit.Props {
		if prop.Kind == ast.PropSpread {
			// The rest property must be last, and a comma after it is not
			// merely tolerated punctuation: there is nothing it could separate.
			if i != len(lit.Props)-1 || lit.TrailingComma {
				p.errorf("a rest element must be the last property of a pattern")
			}
			// An object rest target must be a simple reference: `{...{a}}` is
			// not a valid pattern.
			switch prop.Value.(type) {
			case *ast.Ident, *ast.Member:
				pat.Rest = p.toPattern(prop.Value, binding)
			default:
				p.errorf("an object rest element must be an identifier")
			}
			continue
		}
		if prop.Kind == ast.PropGet || prop.Kind == ast.PropSet || prop.Method {
			p.errorf("a method is not valid in a destructuring pattern")
		}
		prop.Value = p.toPattern(prop.Value, binding)
		pat.Props = append(pat.Props, prop)
	}
	return pat
}
