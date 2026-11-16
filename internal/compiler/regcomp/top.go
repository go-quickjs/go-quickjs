package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Top-level code: a script's body as a function of registers
// (docs/register-interpreter.md, "Stage 2: zero refusals"). Its var and
// function declarations are the global object's, its let, const and class
// declarations the global lexical environment's, and what it binds is
// declared before its first statement runs, in the order the stack form
// declares it (Record.Top): each declaration a cold operation that does what
// the stack form's instruction does. Its value is its completion value, which
// each expression statement sets and each statement that may produce none
// resets, in a register of its own.

// CompileTop compiles body, a script's top-level code, whose stack form is
// fn and whose identifiers' resolution is rec, into register code, or
// returns nil where it has anything the backend does not compile yet.
func CompileTop(body []ast.Stmt, fn *bytecode.Function, rec *Record) *bytecode.RegCode {
	sc := getScratch()
	defer putScratch(sc)
	g, ok := generate(fn, true, sc, func(g *gen) {
		*g = gen{lit: &ast.FuncLit{Body: body}, fn: fn, rec: rec, orig: fn, thisRef: rec.ThisRef}
	})
	if !ok {
		Refuse(fn, "its bindings' layout")
		return nil
	}
	if g.err != "" {
		Refuse(fn, g.err)
		return nil
	}
	return g.finish()
}

// topBody compiles what top-level code does before its first statement: its
// completion value made undefined, and its declarations, each as the stack
// form makes it.
func (g *gen) topBody() {
	g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(g.completion), 0, 0))
	for _, t := range g.rec.Top {
		g.at(int(t.Pos))
		name := g.name16(t.Name)
		flag := uint16(t.Flag)
		var sub bytecode.ROp
		a := 0
		switch t.Op {
		case bytecode.OpCheckGlobalLex:
			sub = bytecode.RCheckGlobalLex
		case bytecode.OpCheckGlobalVar:
			sub = bytecode.RCheckGlobalVar
		case bytecode.OpDefineGlobalVar:
			sub = bytecode.RDefineGlobalVar
		case bytecode.OpDeclareGlobalLex:
			sub = bytecode.RDeclareGlobalLex
		case bytecode.OpDeclareModuleLex:
			sub = bytecode.RDeclareModuleLex
		case bytecode.OpDefineGlobalFunc:
			sub = bytecode.RDefineGlobalFunc
			a = g.alloc()
			g.closure(t.Fn, a)
		case bytecode.OpSetUpvalue:
			a = g.alloc()
			g.closure(t.Fn, a)
			g.storeValue(t.Id, a)
			g.top = g.topAfterBindings()
			continue
		default:
			// The stack compiler records only the seven above (regTop's
			// callers and predeclareFunction's): any other is its bug,
			// not a form to refuse.
			panic("regcomp: top-level code's record holds " + t.Op.String())
		}
		g.far(g.emit(bytecode.MakeR(sub, 0, r16(a), name, flag)), t.Name)
		g.top = g.topAfterBindings()
	}
}

// resetCompletion makes the completion value undefined, as a statement that
// may produce none does before it runs, in top-level code.
func (g *gen) resetCompletion() {
	if g.completion > 0 {
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(g.completion), 0, 0))
	}
}

// exprStmt compiles an expression statement: in top-level code its value is
// the completion value, set once the expression has its value, so that one
// that throws leaves it as it was.
func (g *gen) exprStmt(e ast.Expr) {
	if g.completion == 0 {
		g.discard(e)
		return
	}
	if r := g.expr(e, -1); r != g.completion {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(g.completion), r16(r), 0))
	}
}

// lexInit initializes a script's or a module's top-level lexical binding,
// name, to v, as the stack form's init_global_lex or init_module_lex.
func (g *gen) lexInit(kind ResKind, name string, v, pos int) {
	sub := bytecode.RInitGlobalLex
	if kind == ResModuleLexInit {
		sub = bytecode.RInitModuleLex
	}
	g.at(pos)
	g.far(g.emit(bytecode.MakeR(sub, 0, r16(v), g.name16(name), 0)), name)
}

// exportDecl compiles a module's export declaration: the declaration it
// wraps, a function's hoisted already, or the default export's value,
// which initializes the module's binding of it.
func (g *gen) exportDecl(n *ast.ExportDecl) {
	switch {
	case n.Default && n.Decl == nil:
		v := g.expr(n.DefaultExpr, -1)
		g.lexInit(ResModuleLexInit, "*default*", v, n.Start)
	case n.Default:
		if cd, ok := n.Decl.(*ast.ClassDecl); ok && cd.Class.Name == nil {
			// export default class {}: the expression it looks like, which
			// initializes the module's binding of the default export.
			v := g.expr(cd.Class, -1)
			g.lexInit(ResModuleLexInit, "*default*", v, n.Start)
			return
		}
		if _, ok := n.Decl.(*ast.FuncDecl); !ok {
			g.stmt(n.Decl)
		}
	case n.Decl != nil:
		if _, ok := n.Decl.(*ast.FuncDecl); !ok {
			g.stmt(n.Decl)
		}
	}
}
