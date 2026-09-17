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

// referencesArgumentsInParams reports whether a parameter list mentions
// `arguments`, which a default value may: the object exists before the
// parameters are initialized.
func referencesArgumentsInParams(params []ast.Expr) bool {
	w := &argumentsScanner{}
	for _, p := range params {
		w.expr(p)
	}
	return w.found
}

type argumentsScanner struct {
	found bool
	// seekThis looks for `this` and `super` instead of `arguments`, which needs
	// the same walk: both are bindings an arrow shares with its enclosing
	// function and an ordinary function does not.
	seekThis bool
	// seekSuper narrows that to `super` alone.
	seekSuper bool
	// seekEval widens it back to include a direct eval, whose code may say
	// anything.
	seekEval bool
	// literalOnly narrows the search to the name itself, with no allowance for
	// a direct eval: what the evaluated code says is the evaluated code's own
	// problem, reported when it is compiled.
	literalOnly bool
	// seekName looks for one particular identifier, and descends into every
	// function rather than stopping at the ones with bindings of their own.
	seekName string
	// seekDirectEval looks for a call to `eval` written as that name, and
	// stops at every function: what such a call declares belongs to the
	// function it appears in, and a nested one has a variable scope of its own.
	seekDirectEval bool
	// thisProps collects the names a body assigns directly to `this`, which is
	// what a constructor's object is about to be given. It is non-nil only in
	// that mode, where nothing is being sought and the walk runs to the end.
	thisProps []string
}

// noteThisProp records an assignment of the form `this.name = ...`.
func (w *argumentsScanner) noteThisProp(n *ast.Assign) {
	m, ok := n.Target.(*ast.Member)
	if !ok || m.Computed {
		return
	}
	if _, ok := m.Object.(*ast.This); !ok {
		return
	}
	name := propKeyName(m.Property)
	if name == "" || len(w.thisProps) >= maxThisProps {
		return
	}
	for _, have := range w.thisProps {
		if have == name {
			return
		}
	}
	w.thisProps = append(w.thisProps, name)
}

// maxThisProps bounds the count, which is only a hint at how much room to make.
const maxThisProps = 16

// thisPropertyCount is how many distinct properties a function body assigns to
// `this` by name.
//
// A constructor's object is made with room for them, rather than growing its
// table as the body fills it in. The walk descends into arrows, which share the
// `this` they are written in, and stops at any other function.
func thisPropertyCount(body []ast.Stmt) int {
	w := &argumentsScanner{thisProps: []string{}}
	w.stmts(body)
	return len(w.thisProps)
}

// containsDirectEval reports whether a statement list calls `eval` by that
// name, outside any function written inside it.
//
// The answer is needed before the body is compiled: a frame of a function that
// contains one needs somewhere to put the bindings the evaluated code may
// declare, and the references to them have to be compiled to look there.
func containsDirectEval(body []ast.Stmt) bool {
	w := &argumentsScanner{seekDirectEval: true}
	w.stmts(body)
	return w.found
}

// containsDirectEvalInParams is the same for a parameter list, where a default
// value may contain one.
func containsDirectEvalInParams(params []ast.Expr) bool {
	w := &argumentsScanner{seekDirectEval: true}
	for _, p := range params {
		w.expr(p)
	}
	return w.found
}

// referencesName reports whether a function mentions a name anywhere, nested
// functions included.
//
// It decides whether a named function expression needs a real binding for its
// own name: a reference in its own body can be answered from the running
// closure, but one inside a nested function can only come from a slot. A direct
// eval counts, because what it will say cannot be known from here.
func referencesName(fn *ast.FuncLit, name string) bool {
	w := &argumentsScanner{seekName: name}
	for _, p := range fn.Params {
		w.expr(p)
	}
	w.stmts(fn.Body)
	return w.found
}

// containsArgumentsInStmts reports whether code mentions `arguments`.
//
// It is what a class field initializer may not do: an initializer is a function
// of its own, so there is no arguments object for the name to mean. The parser
// catches it in source; this catches it in what a direct eval there compiles.
// The scan descends into arrows, which share the enclosing arguments object,
// and stops at any other function, which has its own.
func containsArgumentsInStmts(body []ast.Stmt) bool {
	w := &argumentsScanner{literalOnly: true}
	w.stmts(body)
	return w.found
}

// needsHomeObject reports whether an expression mentions super, or a direct
// eval that might.
//
// It decides whether a static field initializer has to be an immediately
// invoked method of the class, which is what gives super a home object to
// resolve against. Only an initializer that needs one pays for the call -- and
// an eval counts, because what it will say cannot be known from here.
func needsHomeObject(e ast.Expr) bool {
	w := &argumentsScanner{seekThis: true, seekSuper: true, seekEval: true}
	w.expr(e)
	return w.found
}

// referencesThis reports whether a function body can observe its `this`.
//
// It is asked so that a sloppy-mode call can skip substituting the global
// object when nothing would see the difference -- which is most functions, and
// the substitution costs a pointer write on every call.
//
// A direct eval could see it, so a body containing one counts as using it.
func referencesThis(fn *ast.FuncLit) bool {
	w := &argumentsScanner{seekThis: true}
	for _, p := range fn.Params {
		w.expr(p)
	}
	w.stmts(fn.Body)
	return w.found
}

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
	case *ast.FieldInit:
		if n.Computed {
			w.expr(n.Key)
		}
		w.expr(n.Value)
	case *ast.BlockStmt:
		w.stmts(n.Body)
	case *ast.VarDecl:
		for _, d := range n.Decls {
			w.expr(d.Target)
			w.expr(d.Init)
		}
	case *ast.FuncDecl:
		// A nested function declaration has its own arguments object, but a
		// name search still descends into it.
		if w.seekName != "" {
			for _, p := range n.Fn.Params {
				w.expr(p)
			}
			w.stmts(n.Fn.Body)
		}
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
	case *ast.Call:
		if w.seekDirectEval {
			if id, ok := n.Callee.(*ast.Ident); ok && id.Name == "eval" {
				w.found = true
				return
			}
		}
		w.expr(n.Callee)
		for _, a := range n.Args {
			w.expr(a)
		}
	case *ast.Ident:
		if w.seekDirectEval {
			return
		}
		if w.seekName != "" {
			if n.Name == w.seekName || n.Name == "eval" {
				w.found = true
			}
			return
		}
		switch {
		case w.literalOnly:
			if n.Name == "arguments" {
				w.found = true
			}
		case w.seekSuper && !w.seekEval:
			// Only a literal `super` counts here.
		case w.seekSuper && n.Name != "eval":
		case n.Name == "eval":
			// A direct eval can read both `this` and `arguments`, so a body
			// containing one is treated as using whichever is being looked
			// for: the binding has to exist before the evaluated code can
			// reach it, and it cannot be added afterwards.
			w.found = true
		case !w.seekThis && n.Name == "arguments":
			w.found = true
		}
	case *ast.This:
		if w.seekThis && !w.seekSuper {
			w.found = true
		}
	case *ast.Super:
		if w.seekThis {
			w.found = true
		}
	case *ast.FuncLit:
		// Only an arrow shares the enclosing this and arguments; a search for
		// a name descends into every function, since a name is in scope
		// however deeply it is nested. A search for a direct eval stops at all
		// of them, an arrow included: an arrow has a variable scope of its own.
		if w.seekDirectEval {
			return
		}
		if w.seekName != "" || n.Kind == ast.FuncArrow {
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
		if w.thisProps != nil {
			w.noteThisProp(n)
		}
		w.expr(n.Target)
		w.expr(n.Value)
	case *ast.Conditional:
		w.expr(n.Test)
		w.expr(n.Cons)
		w.expr(n.Alt)
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
		if w.seekName != "" {
			w.expr(m.Value)
		}
	}
	for _, f := range cls.Fields {
		if f.Computed {
			w.expr(f.Key)
		}
		w.expr(f.Value)
	}
}
