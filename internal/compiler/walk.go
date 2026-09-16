package compiler

import "github.com/go-quickjs/go-quickjs/internal/ast"

// referencesArguments reports whether a function body mentions `arguments`.
//
// The scan descends into arrow functions, because an arrow has no arguments
// object of its own and refers to the enclosing function's, but stops at any
// other function, which gets its own.
//
// The compiler needs this before emitting a function's prologue: an arrow can
// only capture `arguments` if the enclosing function materialized it into a
// slot, and adding that slot later would mean rewriting a prologue that has
// already been emitted.
func referencesArguments(body []ast.Stmt) bool {
	w := &argumentsScanner{}
	w.stmts(body)
	return w.found
}

type argumentsScanner struct{ found bool }

func (w *argumentsScanner) stmts(list []ast.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *argumentsScanner) stmt(s ast.Stmt) {
	if w.found || s == nil {
		return
	}
	switch n := s.(type) {
	case *ast.ExprStmt:
		w.expr(n.X)
	case *ast.BlockStmt:
		w.stmts(n.Body)
	case *ast.VarDecl:
		for _, d := range n.Decls {
			w.expr(d.Target)
			w.expr(d.Init)
		}
	case *ast.FuncDecl:
		// A nested function declaration has its own arguments object.
	case *ast.ClassDecl:
		w.class(n.Class)
	case *ast.ReturnStmt:
		w.expr(n.Arg)
	case *ast.IfStmt:
		w.expr(n.Test)
		w.stmt(n.Cons)
		w.stmt(n.Alt)
	case *ast.ForStmt:
		w.stmt(n.Init)
		w.expr(n.Test)
		w.expr(n.Update)
		w.stmt(n.Body)
	case *ast.ForInStmt:
		w.node(n.Left)
		w.expr(n.Right)
		w.stmt(n.Body)
	case *ast.ForOfStmt:
		w.node(n.Left)
		w.expr(n.Right)
		w.stmt(n.Body)
	case *ast.WhileStmt:
		w.expr(n.Test)
		w.stmt(n.Body)
	case *ast.DoWhileStmt:
		w.stmt(n.Body)
		w.expr(n.Test)
	case *ast.ThrowStmt:
		w.expr(n.Arg)
	case *ast.TryStmt:
		w.stmts(n.Block)
		if n.Catch != nil {
			w.expr(n.Catch.Param)
			w.stmts(n.Catch.Body)
		}
		w.stmts(n.Finally)
	case *ast.SwitchStmt:
		w.expr(n.Disc)
		for _, cs := range n.Cases {
			w.expr(cs.Test)
			w.stmts(cs.Body)
		}
	case *ast.LabeledStmt:
		w.stmt(n.Body)
	case *ast.WithStmt:
		w.expr(n.Object)
		w.stmt(n.Body)
	}
}

func (w *argumentsScanner) node(n ast.Node) {
	switch v := n.(type) {
	case ast.Stmt:
		w.stmt(v)
	case ast.Expr:
		w.expr(v)
	}
}

func (w *argumentsScanner) expr(e ast.Expr) {
	if w.found || e == nil {
		return
	}
	switch n := e.(type) {
	case *ast.Ident:
		if n.Name == "arguments" {
			w.found = true
		}
	case *ast.FuncLit:
		// Only an arrow shares the enclosing arguments object.
		if n.Kind == ast.FuncArrow {
			for _, p := range n.Params {
				w.expr(p)
			}
			w.stmts(n.Body)
		}
	case *ast.ClassLit:
		w.class(n)
	case *ast.TemplateLit:
		for _, x := range n.Exprs {
			w.expr(x)
		}
	case *ast.TaggedTemplate:
		w.expr(n.Tag)
		w.expr(n.Quasi)
	case *ast.ArrayLit:
		for _, el := range n.Elements {
			w.expr(el)
		}
	case *ast.ObjectLit:
		for _, p := range n.Props {
			w.prop(p)
		}
	case *ast.Unary:
		w.expr(n.Operand)
	case *ast.Update:
		w.expr(n.Operand)
	case *ast.Binary:
		w.expr(n.Left)
		w.expr(n.Right)
	case *ast.Logical:
		w.expr(n.Left)
		w.expr(n.Right)
	case *ast.Assign:
		w.expr(n.Target)
		w.expr(n.Value)
	case *ast.Conditional:
		w.expr(n.Test)
		w.expr(n.Cons)
		w.expr(n.Alt)
	case *ast.Call:
		w.expr(n.Callee)
		for _, a := range n.Args {
			w.expr(a)
		}
	case *ast.New:
		w.expr(n.Callee)
		for _, a := range n.Args {
			w.expr(a)
		}
	case *ast.Member:
		w.expr(n.Object)
		if n.Computed {
			w.expr(n.Property)
		}
	case *ast.OptionalChain:
		w.expr(n.Base)
	case *ast.Sequence:
		for _, x := range n.Exprs {
			w.expr(x)
		}
	case *ast.Spread:
		w.expr(n.Arg)
	case *ast.Yield:
		w.expr(n.Arg)
	case *ast.Await:
		w.expr(n.Arg)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			w.expr(el)
		}
		w.expr(n.Rest)
	case *ast.ObjectPattern:
		for _, p := range n.Props {
			w.prop(p)
		}
		w.expr(n.Rest)
	case *ast.AssignPattern:
		w.expr(n.Target)
		w.expr(n.Default)
	case *ast.RestElement:
		w.expr(n.Arg)
	}
}

func (w *argumentsScanner) prop(p ast.Property) {
	if p.Computed {
		w.expr(p.Key)
	}
	w.expr(p.Value)
}

func (w *argumentsScanner) class(cls *ast.ClassLit) {
	if cls == nil {
		return
	}
	w.expr(cls.Extends)
	for _, m := range cls.Members {
		if m.Computed {
			w.expr(m.Key)
		}
	}
	for _, f := range cls.Fields {
		if f.Computed {
			w.expr(f.Key)
		}
		w.expr(f.Value)
	}
}
