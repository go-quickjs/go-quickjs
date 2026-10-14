package compiler

import "github.com/go-quickjs/go-quickjs/internal/ast"

// A function made anew at every iteration of a loop, and only ever called.
//
// `var f = function f() { ... }` in a loop body makes a new function each
// time round, though nothing can tell one from another but its identity: f
// holds one function or another, each called and nothing else. Older code
// writes its helpers so -- PDF.js's font parser makes a getToken at every
// iteration, tens of thousands a run, each a function object of hundreds of
// bytes for the collector. When nothing can observe which one f holds, the
// declaration makes its function the first time and leaves it after that
// (reusedClosures); the function's bindings are the enclosing function's,
// the same at every iteration. That is so when:
//
//   - f is the enclosing function's var, declared once, by this declaration,
//     and nothing else anywhere declares the name -- no parameter, no other
//     var, no lexical binding, no function or class of that name -- so that
//     nothing else assigns it, and it holds either undefined or a function
//     this declaration made;
//   - every use of the name, in the function and every function nested in
//     it, the function expression's own name inside it included, is a call
//     of it, f(...): not a read of it as a value, which could store,
//     compare or construct with it, read its properties or assign to it;
//   - the function expression names no binding declared lexically anywhere
//     else in the enclosing function -- a let, a const, a class or a catch
//     parameter, which a loop or block makes anew -- so that what it
//     captures is the same at every iteration;
//   - it is an ordinary function, not an arrow;
//   - and the code is strict, with no eval anywhere in it, and not compiled
//     for a debugger: a strict function's caller and a stack trace's
//     getFunction do not hand it out, and nothing reads the function's
//     bindings but its own code.

// reusedClosures finds the declarators of fn's body whose function the
// compiler makes once per invocation (see above). fn is strict, has no
// direct eval, and is not compiled for a debugger.
//
// Most functions have no such declaration, and their nested functions are
// not walked: whether there is one is found from fn's own statements alone
// (loopFuncVar), every function's once.
func reusedClosures(fn *ast.FuncLit) map[*ast.FuncLit]bool {
	if !loopFuncVar(fn.Body, false) {
		return nil
	}
	w := &reuseScanner{decls: map[string]int{}, uses: map[string]int{}, calls: map[string]int{}, lexical: map[string]bool{}}
	for _, p := range fn.Params {
		w.pattern(p, false)
	}
	w.own = true
	w.stmts(fn.Body)
	if w.eval || len(w.candidates) == 0 {
		return nil
	}
	var out map[*ast.FuncLit]bool
	for _, c := range w.candidates {
		if w.decls[c.name] != 1 || w.uses[c.name] != w.calls[c.name] {
			continue
		}
		if n := c.fn.Name; n != nil && w.uses[n.Name] != w.calls[n.Name] {
			// Its own name, which its body may hand out as a value.
			continue
		}
		names := &reuseScanner{decls: map[string]int{}, uses: map[string]int{}, calls: map[string]int{}, lexical: map[string]bool{}}
		names.funcLit(c.fn, true)
		ok := true
		for n := range names.uses {
			if w.lexical[n] && !names.lexical[n] {
				ok = false
			}
		}
		for n := range names.decls {
			if w.lexical[n] && !names.lexical[n] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		if out == nil {
			out = map[*ast.FuncLit]bool{}
		}
		out[c.fn] = true
	}
	return out
}

// reuseCandidate is a var declarator of the enclosing function's own whose
// initializer is an ordinary function expression.
type reuseCandidate struct {
	name string
	fn   *ast.FuncLit
}

// reuseScanner counts, for every name in a function and the functions
// nested in it, how often it is declared, how often it is used, and how
// many of those uses are calls of it; which names are declared lexically;
// and whether eval is named anywhere.
type reuseScanner struct {
	decls, uses, calls map[string]int
	lexical            map[string]bool
	eval               bool
	// own is set while the walk is in the enclosing function's own code, not
	// a nested function's, where candidates are found.
	own        bool
	candidates []reuseCandidate
}

func (w *reuseScanner) declare(name string, lexical bool) {
	w.decls[name]++
	if lexical {
		w.lexical[name] = true
	}
}

// pattern walks a binding target: its names are declared, anything
// evaluated in it -- defaults, computed keys -- used.
func (w *reuseScanner) pattern(e ast.Expr, lexical bool) {
	switch n := e.(type) {
	case nil:
	case *ast.Ident:
		w.declare(n.Name, lexical)
	case *ast.AssignPattern:
		w.pattern(n.Target, lexical)
		w.expr(n.Default)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			w.pattern(el, lexical)
		}
		w.pattern(n.Rest, lexical)
	case *ast.ObjectPattern:
		for _, p := range n.Props {
			if p.Computed {
				w.expr(p.Key)
			}
			w.pattern(p.Value, lexical)
		}
		w.pattern(n.Rest, lexical)
	case *ast.RestElement:
		w.pattern(n.Arg, lexical)
	default:
		// An assignment target in a for-in or for-of head, or anything
		// else: a use, not a call.
		w.expr(e)
	}
}

func (w *reuseScanner) stmts(list []ast.Stmt) {
	for _, s := range list {
		w.stmt(s)
	}
}

func (w *reuseScanner) stmt(s ast.Stmt) {
	switch n := s.(type) {
	case nil:
	case *ast.ExprStmt:
		w.expr(n.X)
	case *ast.BlockStmt:
		w.stmts(n.Body)
	case *ast.VarDecl:
		lexical := n.Kind != ast.DeclVar
		for _, d := range n.Decls {
			id, isIdent := d.Target.(*ast.Ident)
			fn, isFunc := d.Init.(*ast.FuncLit)
			if isIdent && isFunc && fn.Kind == ast.FuncNormal && !lexical {
				if w.own {
					w.candidates = append(w.candidates, reuseCandidate{id.Name, fn})
				}
				w.declare(id.Name, false)
				// Its own name, if it is the var's, is the same function's,
				// and counts as no other declaration.
				w.funcLit(fn, fn.Name != nil && fn.Name.Name == id.Name)
				continue
			}
			w.pattern(d.Target, lexical)
			w.expr(d.Init)
		}
	case *ast.FuncDecl:
		if n.Fn.Name != nil {
			w.declare(n.Fn.Name.Name, false)
		}
		w.funcLit(n.Fn, true)
	case *ast.ClassDecl:
		if n.Class.Name != nil {
			w.declare(n.Class.Name.Name, true)
		}
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
		w.forHead(n.Left)
		w.expr(n.Right)
		w.stmt(n.Body)
	case *ast.ForOfStmt:
		w.forHead(n.Left)
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
			w.pattern(n.Catch.Param, true)
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
	case *ast.EmptyStmt, *ast.BreakStmt, *ast.ContinueStmt, *ast.DebuggerStmt:
	case *ast.WithStmt:
		// Strict code has none; a name inside could be anything's.
		w.eval = true
	case *ast.FieldInit:
		if n.Computed {
			w.expr(n.Key)
		}
		w.expr(n.Value)
	default:
		// A statement this walk does not know: it may hold anything.
		w.eval = true
	}
}

func (w *reuseScanner) forHead(left ast.Node) {
	switch l := left.(type) {
	case *ast.VarDecl:
		for _, d := range l.Decls {
			w.pattern(d.Target, l.Kind != ast.DeclVar)
			w.expr(d.Init)
		}
	case ast.Expr:
		w.pattern(l, false)
	default:
		w.eval = true
	}
}

// funcLit walks a nested function: its name, unless ownName says it is the
// var it initializes, its parameters and its body, outside the enclosing
// function's own code.
func (w *reuseScanner) funcLit(fn *ast.FuncLit, ownName bool) {
	if fn.Name != nil && !ownName {
		w.declare(fn.Name.Name, false)
	}
	own := w.own
	w.own = false
	for _, p := range fn.Params {
		w.pattern(p, false)
	}
	w.stmts(fn.Body)
	w.own = own
}

func (w *reuseScanner) class(cls *ast.ClassLit) {
	if cls == nil {
		return
	}
	own := w.own
	w.own = false
	w.expr(cls.Extends)
	for _, m := range cls.Members {
		w.prop(m)
	}
	for _, f := range cls.Fields {
		if f.Computed {
			w.expr(f.Key)
		}
		w.expr(f.Value)
	}
	for _, b := range cls.StaticBlocks {
		w.stmts(b.Body)
	}
	w.own = own
}

func (w *reuseScanner) prop(p ast.Property) {
	if p.Computed {
		w.expr(p.Key)
	}
	w.expr(p.Value)
}

func (w *reuseScanner) expr(e ast.Expr) {
	switch n := e.(type) {
	case nil:
	case *ast.Ident:
		w.uses[n.Name]++
		if n.Name == "eval" {
			w.eval = true
		}
	case *ast.Call:
		if id, ok := n.Callee.(*ast.Ident); ok && !n.Optional {
			w.uses[id.Name]++
			w.calls[id.Name]++
			if id.Name == "eval" {
				w.eval = true
			}
		} else {
			w.expr(n.Callee)
		}
		for _, a := range n.Args {
			w.expr(a)
		}
	case *ast.FuncLit:
		w.funcLit(n, false)
	case *ast.ClassLit:
		if n.Name != nil {
			w.declare(n.Name.Name, true)
		}
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
		w.pattern(n.Target, false)
		w.expr(n.Value)
		// An assignment to a name is no declaration: it is a use, which no
		// call is.
		w.assignTarget(n.Target)
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
	case *ast.ArrayPattern, *ast.ObjectPattern, *ast.AssignPattern, *ast.RestElement:
		w.pattern(e, false)
	case *ast.This, *ast.Super, *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit, *ast.RegexpLit, *ast.BigIntLit,
		*ast.NewTarget, *ast.ImportMeta, *ast.PrivateName:
	default:
		// An expression this walk does not know: it may hold anything.
		w.eval = true
	}
}

// assignTarget undoes the declaration pattern counted for an assignment's
// target, which is not one: what it names it uses.
func (w *reuseScanner) assignTarget(target ast.Expr) {
	var undo func(e ast.Expr)
	undo = func(e ast.Expr) {
		switch n := e.(type) {
		case *ast.Ident:
			w.decls[n.Name]--
			w.uses[n.Name]++
		case *ast.AssignPattern:
			undo(n.Target)
		case *ast.ArrayPattern:
			for _, el := range n.Elements {
				undo(el)
			}
			undo(n.Rest)
		case *ast.ObjectPattern:
			for _, p := range n.Props {
				undo(p.Value)
			}
			undo(n.Rest)
		case *ast.RestElement:
			undo(n.Arg)
		}
	}
	undo(target)
}

// loopFuncVar reports whether statements, in a loop's body if loop is set,
// declare a var of a function expression in a loop: their own statements
// alone, which are where a var is declared, and not the expressions in
// them or the functions nested there.
func loopFuncVar(list []ast.Stmt, loop bool) bool {
	for _, s := range list {
		if loopFuncVarStmt(s, loop) {
			return true
		}
	}
	return false
}

func loopFuncVarStmt(s ast.Stmt, loop bool) bool {
	switch n := s.(type) {
	case *ast.VarDecl:
		if !loop || n.Kind != ast.DeclVar {
			return false
		}
		for _, d := range n.Decls {
			if fn, ok := d.Init.(*ast.FuncLit); ok && fn.Kind == ast.FuncNormal {
				return true
			}
		}
	case *ast.BlockStmt:
		return loopFuncVar(n.Body, loop)
	case *ast.IfStmt:
		return loopFuncVarStmt(n.Cons, loop) || loopFuncVarStmt(n.Alt, loop)
	case *ast.ForStmt:
		return loopFuncVarStmt(n.Body, true)
	case *ast.ForInStmt:
		return loopFuncVarStmt(n.Body, true)
	case *ast.ForOfStmt:
		return loopFuncVarStmt(n.Body, true)
	case *ast.WhileStmt:
		return loopFuncVarStmt(n.Body, true)
	case *ast.DoWhileStmt:
		return loopFuncVarStmt(n.Body, true)
	case *ast.TryStmt:
		if loopFuncVar(n.Block, loop) || loopFuncVar(n.Finally, loop) {
			return true
		}
		return n.Catch != nil && loopFuncVar(n.Catch.Body, loop)
	case *ast.SwitchStmt:
		for _, cs := range n.Cases {
			if loopFuncVar(cs.Body, loop) {
				return true
			}
		}
	case *ast.LabeledStmt:
		return loopFuncVarStmt(n.Body, loop)
	}
	return false
}
