package compiler

import "github.com/go-quickjs/go-quickjs/internal/ast"

// The var/lexical conflict rule.
//
// A `var` declaration hoists its name to the top of the enclosing function, so
// if any scope between the declaration and that function already binds the name
// lexically, the two disagree about which binding the name refers to. The
// specification makes that an early error:
//
//	{ let x; var x; }            // error
//	{ let x; } var x;            // fine: the block has closed
//	var x; { let x; }            // fine: the block shadows
//
// It cannot be checked while compiling, because vars are hoisted before any
// block is entered, so at the moment a var's binding is created the lexical
// bindings it might collide with do not exist yet. Checking the source shape
// directly is both simpler and exactly what the rule says.

// scopeChecker walks a function's body tracking the lexical scopes in effect.
type scopeChecker struct {
	c *compiler
	// scopes is the stack of lexical scopes enclosing the current position,
	// innermost last. It is reset at every function boundary, since a var never
	// hoists past one.
	scopes []lexScope
}

// lexScope is the set of names one scope binds lexically.
type lexScope struct {
	names map[string]int
	// catchParam names a simple catch parameter. Web reality requires
	// `try {} catch (e) { var e; }` to keep working, so that one collision is
	// permitted where every other is an error.
	catchParam string
}

// checkScopes reports the var/lexical collisions in a program.
func (c *compiler) checkScopes(body []ast.Stmt) {
	sc := &scopeChecker{c: c}
	sc.block(body, nil)
}

// block walks a statement list as one lexical scope.
//
// extra holds names the construct binds outside the list itself: a `for (let
// x;;)` head, or a catch parameter.
func (s *scopeChecker) block(body []ast.Stmt, extra map[string]int) {
	scope := lexScope{names: extra}
	if scope.names == nil {
		scope.names = make(map[string]int)
	}
	// Every lexical name in the list is in scope throughout it, so they are all
	// collected before any statement is walked.
	for _, st := range body {
		lexicalNamesOf(st, scope.names)
	}
	s.scopes = append(s.scopes, scope)
	for _, st := range body {
		s.stmt(st)
	}
	s.scopes = s.scopes[:len(s.scopes)-1]
}

// catchBlock walks a catch clause, recording a simple parameter as the one
// name a var may legally redeclare.
func (s *scopeChecker) catchBlock(cc *ast.CatchClause) {
	extra := make(map[string]int)
	simple := ""
	if cc.Param != nil {
		collectPatternNamesAt(cc.Param, extra)
		if id, ok := cc.Param.(*ast.Ident); ok {
			simple = id.Name
		}
	}
	scope := lexScope{names: extra, catchParam: simple}
	for _, st := range cc.Body {
		lexicalNamesOf(st, scope.names)
	}
	s.scopes = append(s.scopes, scope)
	for _, st := range cc.Body {
		s.stmt(st)
	}
	s.scopes = s.scopes[:len(s.scopes)-1]
}

// nested walks a single statement that is a scope in its own right, such as the
// unbraced body of an `if`.
func (s *scopeChecker) nested(st ast.Stmt) {
	if st == nil {
		return
	}
	if b, ok := st.(*ast.BlockStmt); ok {
		s.block(b.Body, nil)
		return
	}
	s.stmt(st)
}

func (s *scopeChecker) stmt(st ast.Stmt) {
	switch n := st.(type) {
	case *ast.VarDecl:
		if n.Kind == ast.DeclVar {
			names := make(map[string]int)
			for _, d := range n.Decls {
				collectPatternNamesAt(d.Target, names)
			}
			for name, pos := range names {
				s.checkVar(name, pos)
			}
		}
		// A declarator's initializer may contain a function, whose body is
		// checked as its own scope.
		for _, d := range n.Decls {
			s.expr(d.Init)
		}

	case *ast.BlockStmt:
		s.block(n.Body, nil)

	case *ast.IfStmt:
		s.expr(n.Test)
		s.nested(n.Cons)
		s.nested(n.Alt)

	case *ast.ForStmt:
		// A `for (let x ...)` head binds x across the head and the body, so
		// both are walked inside one scope.
		extra := headNames(n.Init)
		s.scopes = append(s.scopes, lexScope{names: extra})
		s.stmt(n.Init)
		s.expr(n.Test)
		s.expr(n.Update)
		s.nested(n.Body)
		s.scopes = s.scopes[:len(s.scopes)-1]

	case *ast.ForInStmt:
		s.forIn(n.Left, n.Right, n.Body)

	case *ast.ForOfStmt:
		s.forIn(n.Left, n.Right, n.Body)

	case *ast.WhileStmt:
		s.expr(n.Test)
		s.nested(n.Body)

	case *ast.DoWhileStmt:
		s.nested(n.Body)
		s.expr(n.Test)

	case *ast.SwitchStmt:
		s.expr(n.Disc)
		// All the clauses of a switch share one block scope.
		var body []ast.Stmt
		for _, cl := range n.Cases {
			body = append(body, cl.Body...)
		}
		scope := lexScope{names: make(map[string]int)}
		for _, x := range body {
			lexicalNamesOf(x, scope.names)
		}
		s.scopes = append(s.scopes, scope)
		for _, cl := range n.Cases {
			s.expr(cl.Test)
			for _, x := range cl.Body {
				s.stmt(x)
			}
		}
		s.scopes = s.scopes[:len(s.scopes)-1]

	case *ast.TryStmt:
		s.block(n.Block, nil)
		if n.Catch != nil {
			s.catchBlock(n.Catch)
		}
		if n.Finally != nil {
			s.block(n.Finally, nil)
		}

	case *ast.LabeledStmt:
		s.nested(n.Body)

	case *ast.WithStmt:
		s.expr(n.Object)
		s.nested(n.Body)

	case *ast.FuncDecl:
		s.function(n.Fn)

	case *ast.ClassDecl:
		s.class(n.Class)

	case *ast.ExprStmt:
		s.expr(n.X)

	case *ast.ReturnStmt:
		s.expr(n.Arg)

	case *ast.ThrowStmt:
		s.expr(n.Arg)

	case *ast.ExportDecl:
		if n.Decl != nil {
			s.stmt(n.Decl)
		}
		s.expr(n.DefaultExpr)
	}
}

// forIn walks the shared shape of for-in and for-of.
func (s *scopeChecker) forIn(left ast.Node, right ast.Expr, body ast.Stmt) {
	extra := make(map[string]int)
	if vd, ok := left.(*ast.VarDecl); ok && vd.Kind != ast.DeclVar {
		for _, d := range vd.Decls {
			collectPatternNamesAt(d.Target, extra)
		}
	}
	s.expr(right)
	s.scopes = append(s.scopes, lexScope{names: extra})
	if st, ok := left.(ast.Stmt); ok {
		s.stmt(st)
	}
	s.nested(body)
	s.scopes = s.scopes[:len(s.scopes)-1]
}

// headNames returns the names a three-clause for head binds lexically.
func headNames(init ast.Stmt) map[string]int {
	names := make(map[string]int)
	if vd, ok := init.(*ast.VarDecl); ok && vd.Kind != ast.DeclVar {
		for _, d := range vd.Decls {
			collectPatternNamesAt(d.Target, names)
		}
	}
	return names
}

// checkVar reports a var that collides with an enclosing lexical binding.
func (s *scopeChecker) checkVar(name string, pos int) {
	for i := len(s.scopes) - 1; i >= 0; i-- {
		if _, ok := s.scopes[i].names[name]; !ok {
			continue
		}
		if s.scopes[i].catchParam == name {
			// The one permitted collision.
			continue
		}
		s.c.errorf(pos, "identifier %q has already been declared", name)
	}
}

// function walks a function's body with a fresh scope stack, since a var inside
// it cannot reach any scope outside.
func (s *scopeChecker) function(fn *ast.FuncLit) {
	if fn == nil {
		return
	}
	saved := s.scopes
	s.scopes = nil
	for _, p := range fn.Params {
		s.expr(paramExpr(p))
	}
	s.block(fn.Body, nil)
	s.scopes = saved
}

// paramExpr unwraps a parameter to the expression a default value hangs off,
// so that a function nested in a default is still checked.
func paramExpr(p ast.Expr) ast.Expr { return p }

func (s *scopeChecker) class(cl *ast.ClassLit) {
	if cl == nil {
		return
	}
	s.expr(cl.Extends)
	for _, m := range cl.Members {
		s.expr(m.Key)
		s.expr(m.Value)
	}
	for _, f := range cl.Fields {
		s.expr(f.Key)
		s.expr(f.Value)
	}
	for _, blk := range cl.StaticBlocks {
		saved := s.scopes
		s.scopes = nil
		s.block(blk, nil)
		s.scopes = saved
	}
}

// expr descends into an expression looking for the functions and classes inside
// it, whose bodies are scopes of their own.
func (s *scopeChecker) expr(e ast.Expr) {
	switch n := e.(type) {
	case nil:
		return
	case *ast.FuncLit:
		s.function(n)
	case *ast.ClassLit:
		s.class(n)
	case *ast.Binary:
		s.expr(n.Left)
		s.expr(n.Right)
	case *ast.Logical:
		s.expr(n.Left)
		s.expr(n.Right)
	case *ast.Assign:
		s.expr(n.Target)
		s.expr(n.Value)
	case *ast.AssignPattern:
		s.expr(n.Target)
		s.expr(n.Default)
	case *ast.Conditional:
		s.expr(n.Test)
		s.expr(n.Cons)
		s.expr(n.Alt)
	case *ast.Unary:
		s.expr(n.Operand)
	case *ast.Update:
		s.expr(n.Operand)
	case *ast.Call:
		s.expr(n.Callee)
		for _, a := range n.Args {
			s.expr(a)
		}
	case *ast.New:
		s.expr(n.Callee)
		for _, a := range n.Args {
			s.expr(a)
		}
	case *ast.Member:
		s.expr(n.Object)
		s.expr(n.Property)
	case *ast.ArrayLit:
		for _, el := range n.Elements {
			s.expr(el)
		}
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			s.expr(el)
		}
		s.expr(n.Rest)
	case *ast.ObjectLit:
		for _, p := range n.Props {
			s.expr(p.Key)
			s.expr(p.Value)
		}
	case *ast.ObjectPattern:
		for _, p := range n.Props {
			s.expr(p.Key)
			s.expr(p.Value)
		}
		s.expr(n.Rest)
	case *ast.RestElement:
		s.expr(n.Arg)
	case *ast.Spread:
		s.expr(n.Arg)
	case *ast.Sequence:
		for _, x := range n.Exprs {
			s.expr(x)
		}
	case *ast.Yield:
		s.expr(n.Arg)
	case *ast.Await:
		s.expr(n.Arg)
	case *ast.TaggedTemplate:
		s.expr(n.Tag)
		for _, x := range n.Quasi.Exprs {
			s.expr(x)
		}
	case *ast.TemplateLit:
		for _, x := range n.Exprs {
			s.expr(x)
		}
	}
}

// lexicalNamesOf adds the names a statement binds lexically in the scope that
// directly contains it.
func lexicalNamesOf(st ast.Stmt, out map[string]int) {
	switch n := st.(type) {
	case *ast.VarDecl:
		if n.Kind == ast.DeclVar {
			return
		}
		for _, d := range n.Decls {
			collectPatternNamesAt(d.Target, out)
		}
	case *ast.ClassDecl:
		if n.Class != nil && n.Class.Name != nil {
			out[n.Class.Name.Name] = n.Start
		}
	case *ast.ExportDecl:
		if n.Decl != nil {
			lexicalNamesOf(n.Decl, out)
		}
	}
	// A function declaration inside a block is deliberately not counted. It is
	// lexical in strict mode, but Annex B gives it var-like behaviour in sloppy
	// mode, and treating it as lexical here would reject code the web relies on.
}

// collectPatternNamesAt gathers the names a binding pattern introduces, keeping
// each one's position for the error message.
func collectPatternNamesAt(e ast.Expr, out map[string]int) {
	var names []string
	collectPatternNames(e, &names)
	pos := 0
	if n, ok := e.(ast.Node); ok && n != nil {
		pos = n.Pos()
	}
	for _, name := range names {
		if _, seen := out[name]; !seen {
			out[name] = pos
		}
	}
}
