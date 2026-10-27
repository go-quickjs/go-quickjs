package compiler

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/ast"
)

// A function or class the language leaves anonymous -- `a.b = function () {}`
// -- is still named in a stack trace, as V8's FuncNameInferrer names it: by
// the place it is assigned to, its name parts joined with dots (a.b,
// Foo.prototype.m as Foo.m, a[k] as a.<computed>). The name is its
// bytecode.Function's TraceName, which nothing but a trace reads: the
// function's name property is the language's.
//
// The compiler hands the name down as V8's parser does: an assignment or a
// declaration starts it, and only a conditional's branches, a logical
// operator's operands, a comma's last operand, an array literal's elements
// and an object literal's values hand it on to a function or class literal
// written there. Anything else -- a call's arguments, a function's body --
// starts afresh. compileExprNamed takes traceName as it begins, leaving it
// empty for what it compiles, and keeps it in traceIn for the helpers that
// hand it on.

// traceJoin is the name base and parts make: parts appended to base, or,
// where there is no base, to the name of the function being compiled if it
// starts with a capital letter -- V8 takes such a function for a
// constructor, so that `this.x = function () {}` in function C is C.x. With
// no parts there is no name.
func (c *compiler) traceJoin(base string, parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	if base == "" {
		if r, _ := utf8.DecodeRuneInString(c.fn.Name); unicode.IsUpper(r) {
			base = c.fn.Name
		}
	}
	all := parts
	if base != "" {
		all = append([]string{base}, parts...)
	}
	return strings.Join(all, ".")
}

// assignTraceName is the name an assignment to target gives the functions
// its value hands it to: a name's, or a member expression's path, after
// whatever the assignment's own place handed it (a.b = c.d = function () {}
// is a.b.c.d).
func (c *compiler) assignTraceName(target ast.Expr) string {
	base := c.traceIn
	switch t := target.(type) {
	case *ast.Ident:
		return c.traceJoin(base, t.Name)
	case *ast.Member:
		return c.traceJoin(base, memberTraceParts(t)...)
	}
	return ""
}

// handsName reports whether e may hand a trace name to a function or class
// literal: it is one, or one of the expressions that hand a name on has
// one where it would be handed (a conditional's branches, a logical
// operator's operands, a comma's last operand, an array literal's
// elements, an object literal's values, an assignment's value). Most
// assignments and declarations have none, and build no name.
func handsName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.FuncLit, *ast.ClassLit:
		return true
	case *ast.Conditional:
		return handsName(x.Cons) || handsName(x.Alt)
	case *ast.Logical:
		return handsName(x.Left) || handsName(x.Right)
	case *ast.Sequence:
		return len(x.Exprs) > 0 && handsName(x.Exprs[len(x.Exprs)-1])
	case *ast.Assign:
		return handsName(x.Value)
	case *ast.ArrayLit:
		for _, el := range x.Elements {
			if el != nil && handsName(el) {
				return true
			}
		}
	case *ast.ObjectLit:
		for _, p := range x.Props {
			if p.Value != nil && handsName(p.Value) {
				return true
			}
		}
	}
	return false
}

// memberTraceParts is a member expression's name parts: its names and its
// keys, a key that is no string literal as <computed>, `this` and the
// prototype key left out, as V8 leaves them.
func memberTraceParts(e ast.Expr) []string {
	switch x := e.(type) {
	case *ast.Ident:
		return []string{x.Name}
	case *ast.Member:
		parts := memberTraceParts(x.Object)
		key := "<computed>"
		switch {
		case !x.Computed:
			key = propKeyName(x.Property)
		default:
			if s, ok := x.Property.(*ast.StringLit); ok {
				key = s.Value
			}
		}
		if key != "prototype" {
			parts = append(parts, key)
		}
		return parts
	}
	return nil
}
