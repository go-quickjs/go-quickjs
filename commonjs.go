package quickjs

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// CommonJSExports reads CommonJS source for the names it exports, without
// running it -- which is how node gives an ES module that imports a CommonJS
// file its named exports, and what a host defining a synthetic module for
// such a file needs before the file runs.
//
// The names are those the source assigns to `exports.name`,
// `module.exports.name` or `exports["name"]`, defines on exports with
// `Object.defineProperty`, or lists in an object literal it assigns to
// `module.exports`, each once. reexports are the specifiers of modules the
// source hands on whole -- `module.exports = require("x")`, `...require("x")`
// in that literal, or TypeScript's `__exportStar(require("x"), exports)` --
// whose names are its too.
//
// Like node's, the reading is of the source's shape, not its meaning: a name
// assigned only under a condition is found, and one computed is not. Source
// that does not parse is a *SyntaxError naming name, at the file's own lines.
func CommonJSExports(name, source string) (exports, reexports []string, err error) {
	if strings.HasPrefix(source, "#!") {
		source = "//" + source[2:]
	}
	// The source is a function body, as node runs it: a return at its top
	// level is allowed.
	wrapped := "(function (exports, require, module, __filename, __dirname) {\n" + source + "\n})"
	prog, perr := parser.Parse(wrapped, parser.Options{})
	if perr != nil {
		return nil, nil, newSyntaxError(perr, name, -1, 0)
	}
	s := cjsScan{seen: map[string]bool{}, seenRe: map[string]bool{}}
	s.stmts(prog.Body)
	return s.exports, s.reexports, nil
}

// cjsScan walks a CommonJS file's syntax tree for what it exports.
type cjsScan struct {
	exports, reexports []string
	seen, seenRe       map[string]bool
}

func (s *cjsScan) export(name string) {
	if !s.seen[name] {
		s.seen[name] = true
		s.exports = append(s.exports, name)
	}
}

func (s *cjsScan) reexport(specifier string) {
	if !s.seenRe[specifier] {
		s.seenRe[specifier] = true
		s.reexports = append(s.reexports, specifier)
	}
}

// assign finds `exports.a = …`, `module.exports.a = …` and `module.exports =
// …`.
func (s *cjsScan) assign(a *ast.Assign) {
	if a.Op != "=" {
		return
	}
	t, ok := a.Target.(*ast.Member)
	if !ok {
		return
	}
	if isModuleExports(t) {
		s.moduleExports(a.Value)
		return
	}
	if isExportsObject(t.Object) {
		if name, ok := memberName(t); ok {
			s.export(name)
		}
	}
}

// moduleExports finds what an object literal assigned to module.exports
// lists, and what `module.exports = require("x")` hands on.
func (s *cjsScan) moduleExports(v ast.Expr) {
	switch v := v.(type) {
	case *ast.ObjectLit:
		for _, p := range v.Props {
			switch {
			case p.Kind == ast.PropSpread:
				if spec, ok := requireOf(p.Value); ok {
					s.reexport(spec)
				}
			case p.Kind == ast.PropInit && !p.Computed:
				if name, ok := keyName(p.Key); ok {
					s.export(name)
				}
			}
		}
	default:
		if spec, ok := requireOf(v); ok {
			s.reexport(spec)
		}
	}
}

// call finds `Object.defineProperty(exports, "a", …)` and
// `__exportStar(require("x"), exports)`.
func (s *cjsScan) call(c *ast.Call) {
	switch callee := c.Callee.(type) {
	case *ast.Member:
		if obj, ok := callee.Object.(*ast.Ident); ok && obj.Name == "Object" && len(c.Args) >= 2 {
			if name, _ := memberName(callee); name == "defineProperty" && isExportsObject(c.Args[0]) {
				if key, ok := c.Args[1].(*ast.StringLit); ok {
					s.export(key.Value)
				}
			}
			return
		}
		if name, _ := memberName(callee); name == "__exportStar" {
			s.exportStar(c)
		}
	case *ast.Ident:
		if callee.Name == "__exportStar" || callee.Name == "__export" {
			s.exportStar(c)
		}
	}
}

func (s *cjsScan) exportStar(c *ast.Call) {
	if len(c.Args) > 0 {
		if spec, ok := requireOf(c.Args[0]); ok {
			s.reexport(spec)
		}
	}
}

// isExportsObject reports whether e is `exports` or `module.exports`.
func isExportsObject(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "exports"
	case *ast.Member:
		return isModuleExports(e)
	}
	return false
}

// isModuleExports reports whether m is `module.exports`.
func isModuleExports(m *ast.Member) bool {
	obj, ok := m.Object.(*ast.Ident)
	if !ok || obj.Name != "module" {
		return false
	}
	name, ok := memberName(m)
	return ok && name == "exports"
}

// memberName is the name a member expression reads: `.a`, or `["a"]`.
func memberName(m *ast.Member) (string, bool) {
	if !m.Computed {
		if id, ok := m.Property.(*ast.Ident); ok {
			return id.Name, true
		}
		return "", false
	}
	if lit, ok := m.Property.(*ast.StringLit); ok {
		return lit.Value, true
	}
	return "", false
}

// keyName is an object literal's key: a name or a string.
func keyName(k ast.Expr) (string, bool) {
	switch k := k.(type) {
	case *ast.Ident:
		return k.Name, true
	case *ast.StringLit:
		return k.Value, true
	}
	return "", false
}

// requireOf is the specifier of `require("x")`.
func requireOf(e ast.Expr) (string, bool) {
	c, ok := e.(*ast.Call)
	if !ok || len(c.Args) != 1 {
		return "", false
	}
	if id, ok := c.Callee.(*ast.Ident); !ok || id.Name != "require" {
		return "", false
	}
	lit, ok := c.Args[0].(*ast.StringLit)
	if !ok {
		return "", false
	}
	return lit.Value, true
}

// The walk visits every statement and expression, wherever it is nested.

func (s *cjsScan) stmts(list []ast.Stmt) {
	for _, st := range list {
		s.stmt(st)
	}
}

func (s *cjsScan) exprs(list []ast.Expr) {
	for _, e := range list {
		s.expr(e)
	}
}

func (s *cjsScan) stmt(st ast.Stmt) {
	switch st := st.(type) {
	case *ast.ExprStmt:
		s.expr(st.X)
	case *ast.BlockStmt:
		s.stmts(st.Body)
	case *ast.VarDecl:
		for _, d := range st.Decls {
			s.expr(d.Target)
			s.expr(d.Init)
		}
	case *ast.FuncDecl:
		s.fn(st.Fn)
	case *ast.ClassDecl:
		s.class(st.Class)
	case *ast.ReturnStmt:
		s.expr(st.Arg)
	case *ast.IfStmt:
		s.expr(st.Test)
		s.stmt(st.Cons)
		s.stmt(st.Alt)
	case *ast.ForStmt:
		s.stmt(st.Init)
		s.expr(st.Test)
		s.expr(st.Update)
		s.stmt(st.Body)
	case *ast.ForInStmt:
		s.node(st.Left)
		s.expr(st.Right)
		s.stmt(st.Body)
	case *ast.ForOfStmt:
		s.node(st.Left)
		s.expr(st.Right)
		s.stmt(st.Body)
	case *ast.WhileStmt:
		s.expr(st.Test)
		s.stmt(st.Body)
	case *ast.DoWhileStmt:
		s.stmt(st.Body)
		s.expr(st.Test)
	case *ast.ThrowStmt:
		s.expr(st.Arg)
	case *ast.TryStmt:
		s.stmts(st.Block)
		if st.Catch != nil {
			s.expr(st.Catch.Param)
			s.stmts(st.Catch.Body)
		}
		s.stmts(st.Finally)
	case *ast.SwitchStmt:
		s.expr(st.Disc)
		for _, c := range st.Cases {
			s.expr(c.Test)
			s.stmts(c.Body)
		}
	case *ast.LabeledStmt:
		s.stmt(st.Body)
	case *ast.WithStmt:
		s.expr(st.Object)
		s.stmt(st.Body)
	}
}

// node is a for-in or for-of's left side: a declaration or a target.
func (s *cjsScan) node(n ast.Node) {
	switch n := n.(type) {
	case ast.Stmt:
		s.stmt(n)
	case ast.Expr:
		s.expr(n)
	}
}

func (s *cjsScan) expr(e ast.Expr) {
	switch e := e.(type) {
	case *ast.Assign:
		s.assign(e)
		s.expr(e.Target)
		s.expr(e.Value)
	case *ast.Call:
		s.call(e)
		s.expr(e.Callee)
		s.exprs(e.Args)
	case *ast.TemplateLit:
		s.exprs(e.Exprs)
	case *ast.TaggedTemplate:
		s.expr(e.Tag)
		if e.Quasi != nil {
			s.exprs(e.Quasi.Exprs)
		}
	case *ast.ArrayLit:
		s.exprs(e.Elements)
	case *ast.ObjectLit:
		s.props(e.Props)
	case *ast.FuncLit:
		s.fn(e)
	case *ast.ClassLit:
		s.class(e)
	case *ast.Unary:
		s.expr(e.Operand)
	case *ast.Update:
		s.expr(e.Operand)
	case *ast.Binary:
		s.expr(e.Left)
		s.expr(e.Right)
	case *ast.Logical:
		s.expr(e.Left)
		s.expr(e.Right)
	case *ast.Conditional:
		s.expr(e.Test)
		s.expr(e.Cons)
		s.expr(e.Alt)
	case *ast.New:
		s.expr(e.Callee)
		s.exprs(e.Args)
	case *ast.Member:
		s.expr(e.Object)
		if e.Computed {
			s.expr(e.Property)
		}
	case *ast.OptionalChain:
		s.expr(e.Base)
	case *ast.Sequence:
		s.exprs(e.Exprs)
	case *ast.Spread:
		s.expr(e.Arg)
	case *ast.Yield:
		s.expr(e.Arg)
	case *ast.Await:
		s.expr(e.Arg)
	case *ast.ArrayPattern:
		s.exprs(e.Elements)
		s.expr(e.Rest)
	case *ast.ObjectPattern:
		s.props(e.Props)
		s.expr(e.Rest)
	case *ast.AssignPattern:
		s.expr(e.Target)
		s.expr(e.Default)
	case *ast.RestElement:
		s.expr(e.Arg)
	}
}

func (s *cjsScan) props(props []ast.Property) {
	for _, p := range props {
		if p.Computed {
			s.expr(p.Key)
		}
		s.expr(p.Value)
	}
}

func (s *cjsScan) fn(f *ast.FuncLit) {
	if f == nil {
		return
	}
	s.exprs(f.Params)
	s.stmts(f.Body)
}

func (s *cjsScan) class(c *ast.ClassLit) {
	if c == nil {
		return
	}
	s.expr(c.Extends)
	s.props(c.Members)
	for _, f := range c.Fields {
		if f.Computed {
			s.expr(f.Key)
		}
		s.expr(f.Value)
	}
	for _, b := range c.StaticBlocks {
		s.stmts(b.Body)
	}
}
