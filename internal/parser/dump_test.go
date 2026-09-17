package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// dump renders a program as a compact s-expression, so that tests can assert on
// tree shape in one readable line instead of walking nodes by hand.
func dump(p *ast.Program) string {
	var sb strings.Builder
	for i, stmt := range p.Body {
		if i > 0 {
			sb.WriteByte(' ')
		}
		dumpStmt(&sb, stmt)
	}
	return sb.String()
}

func dumpStmts(sb *strings.Builder, body []ast.Stmt) {
	sb.WriteByte('(')
	for i, s := range body {
		if i > 0 {
			sb.WriteByte(' ')
		}
		dumpStmt(sb, s)
	}
	sb.WriteByte(')')
}

func dumpStmt(sb *strings.Builder, s ast.Stmt) {
	switch n := s.(type) {
	case *ast.ExprStmt:
		dumpExpr(sb, n.X)
	case *ast.BlockStmt:
		sb.WriteString("block")
		dumpStmts(sb, n.Body)
	case *ast.EmptyStmt:
		sb.WriteString("empty")
	case *ast.VarDecl:
		fmt.Fprintf(sb, "(%s", n.Kind)
		for _, d := range n.Decls {
			sb.WriteByte(' ')
			if d.Init == nil {
				dumpExpr(sb, d.Target)
				continue
			}
			sb.WriteByte('(')
			dumpExpr(sb, d.Target)
			sb.WriteByte(' ')
			dumpExpr(sb, d.Init)
			sb.WriteByte(')')
		}
		sb.WriteByte(')')
	case *ast.FuncDecl:
		sb.WriteString("(decl ")
		dumpExpr(sb, n.Fn)
		sb.WriteByte(')')
	case *ast.ClassDecl:
		sb.WriteString("(decl ")
		dumpExpr(sb, n.Class)
		sb.WriteByte(')')
	case *ast.ReturnStmt:
		sb.WriteString("(return")
		if n.Arg != nil {
			sb.WriteByte(' ')
			dumpExpr(sb, n.Arg)
		}
		sb.WriteByte(')')
	case *ast.IfStmt:
		sb.WriteString("(if ")
		dumpExpr(sb, n.Test)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Cons)
		if n.Alt != nil {
			sb.WriteByte(' ')
			dumpStmt(sb, n.Alt)
		}
		sb.WriteByte(')')
	case *ast.ForStmt:
		sb.WriteString("(for ")
		if n.Init == nil {
			sb.WriteString("_")
		} else {
			dumpStmt(sb, n.Init)
		}
		sb.WriteByte(' ')
		dumpOptExpr(sb, n.Test)
		sb.WriteByte(' ')
		dumpOptExpr(sb, n.Update)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	case *ast.ForInStmt:
		sb.WriteString("(for-in ")
		dumpNode(sb, n.Left)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Right)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	case *ast.ForOfStmt:
		if n.Await {
			sb.WriteString("(for-await-of ")
		} else {
			sb.WriteString("(for-of ")
		}
		dumpNode(sb, n.Left)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Right)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	case *ast.WhileStmt:
		sb.WriteString("(while ")
		dumpExpr(sb, n.Test)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	case *ast.DoWhileStmt:
		sb.WriteString("(do ")
		dumpStmt(sb, n.Body)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Test)
		sb.WriteByte(')')
	case *ast.BreakStmt:
		sb.WriteString("(break")
		if n.Label != "" {
			sb.WriteByte(' ')
			sb.WriteString(n.Label)
		}
		sb.WriteByte(')')
	case *ast.ContinueStmt:
		sb.WriteString("(continue")
		if n.Label != "" {
			sb.WriteByte(' ')
			sb.WriteString(n.Label)
		}
		sb.WriteByte(')')
	case *ast.ThrowStmt:
		sb.WriteString("(throw ")
		dumpExpr(sb, n.Arg)
		sb.WriteByte(')')
	case *ast.TryStmt:
		sb.WriteString("(try ")
		dumpStmts(sb, n.Block)
		if n.Catch != nil {
			sb.WriteString(" (catch ")
			if n.Catch.Param != nil {
				dumpExpr(sb, n.Catch.Param)
				sb.WriteByte(' ')
			}
			dumpStmts(sb, n.Catch.Body)
			sb.WriteByte(')')
		}
		if n.Finally != nil {
			sb.WriteString(" (finally ")
			dumpStmts(sb, n.Finally)
			sb.WriteByte(')')
		}
		sb.WriteByte(')')
	case *ast.SwitchStmt:
		sb.WriteString("(switch ")
		dumpExpr(sb, n.Disc)
		for _, c := range n.Cases {
			if c.Test == nil {
				sb.WriteString(" (default ")
			} else {
				sb.WriteString(" (case ")
				dumpExpr(sb, c.Test)
				sb.WriteByte(' ')
			}
			dumpStmts(sb, c.Body)
			sb.WriteByte(')')
		}
		sb.WriteByte(')')
	case *ast.LabeledStmt:
		fmt.Fprintf(sb, "(label %s ", n.Label)
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	case *ast.DebuggerStmt:
		sb.WriteString("debugger")
	case *ast.WithStmt:
		sb.WriteString("(with ")
		dumpExpr(sb, n.Object)
		sb.WriteByte(' ')
		dumpStmt(sb, n.Body)
		sb.WriteByte(')')
	default:
		fmt.Fprintf(sb, "<%T>", s)
	}
}

func dumpNode(sb *strings.Builder, n ast.Node) {
	switch v := n.(type) {
	case ast.Stmt:
		dumpStmt(sb, v)
	case ast.Expr:
		dumpExpr(sb, v)
	default:
		fmt.Fprintf(sb, "<%T>", n)
	}
}

func dumpOptExpr(sb *strings.Builder, e ast.Expr) {
	if e == nil {
		sb.WriteString("_")
		return
	}
	dumpExpr(sb, e)
}

func dumpExpr(sb *strings.Builder, e ast.Expr) {
	switch n := e.(type) {
	case nil:
		sb.WriteString("hole")
	case *ast.Ident:
		sb.WriteString(n.Name)
	case *ast.PrivateName:
		sb.WriteString("#" + n.Name)
	case *ast.NumberLit:
		sb.WriteString(jsnum.FormatFloat(n.Value))
	case *ast.StringLit:
		sb.WriteString(strconv.Quote(n.Value))
	case *ast.BigIntLit:
		sb.WriteString(n.Raw + "n")
	case *ast.BoolLit:
		if n.Value {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case *ast.NullLit:
		sb.WriteString("null")
	case *ast.RegexpLit:
		fmt.Fprintf(sb, "/%s/%s", n.Pattern, n.Flags)
	case *ast.This:
		sb.WriteString("this")
	case *ast.Super:
		sb.WriteString("super")
	case *ast.NewTarget:
		sb.WriteString("new.target")
	case *ast.TemplateLit:
		sb.WriteString("(tpl")
		for i, q := range n.Quasis {
			fmt.Fprintf(sb, " %s", strconv.Quote(q.Cooked))
			if i < len(n.Exprs) {
				sb.WriteByte(' ')
				dumpExpr(sb, n.Exprs[i])
			}
		}
		sb.WriteByte(')')
	case *ast.TaggedTemplate:
		sb.WriteString("(tagged ")
		dumpExpr(sb, n.Tag)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Quasi)
		sb.WriteByte(')')
	case *ast.ArrayLit:
		sb.WriteString("(array")
		for _, el := range n.Elements {
			sb.WriteByte(' ')
			dumpExpr(sb, el)
		}
		sb.WriteByte(')')
	case *ast.ObjectLit:
		sb.WriteString("(object")
		for _, prop := range n.Props {
			sb.WriteByte(' ')
			dumpProp(sb, prop)
		}
		sb.WriteByte(')')
	case *ast.FuncLit:
		dumpFunc(sb, n)
	case *ast.ClassLit:
		dumpClass(sb, n)
	case *ast.Unary:
		fmt.Fprintf(sb, "(%s ", n.Op)
		dumpExpr(sb, n.Operand)
		sb.WriteByte(')')
	case *ast.Update:
		if n.Prefix {
			fmt.Fprintf(sb, "(pre%s ", n.Op)
		} else {
			fmt.Fprintf(sb, "(post%s ", n.Op)
		}
		dumpExpr(sb, n.Operand)
		sb.WriteByte(')')
	case *ast.Binary:
		fmt.Fprintf(sb, "(%s ", n.Op)
		dumpExpr(sb, n.Left)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Right)
		sb.WriteByte(')')
	case *ast.Logical:
		fmt.Fprintf(sb, "(%s ", n.Op)
		dumpExpr(sb, n.Left)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Right)
		sb.WriteByte(')')
	case *ast.Assign:
		fmt.Fprintf(sb, "(%s ", n.Op)
		dumpExpr(sb, n.Target)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Value)
		sb.WriteByte(')')
	case *ast.Conditional:
		sb.WriteString("(?: ")
		dumpExpr(sb, n.Test)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Cons)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Alt)
		sb.WriteByte(')')
	case *ast.Call:
		if n.Optional {
			sb.WriteString("(?call ")
		} else {
			sb.WriteString("(call ")
		}
		dumpExpr(sb, n.Callee)
		for _, a := range n.Args {
			sb.WriteByte(' ')
			dumpExpr(sb, a)
		}
		sb.WriteByte(')')
	case *ast.New:
		sb.WriteString("(new ")
		dumpExpr(sb, n.Callee)
		for _, a := range n.Args {
			sb.WriteByte(' ')
			dumpExpr(sb, a)
		}
		sb.WriteByte(')')
	case *ast.Member:
		switch {
		case n.Optional && n.Computed:
			sb.WriteString("(?idx ")
		case n.Optional:
			sb.WriteString("(?. ")
		case n.Computed:
			sb.WriteString("(idx ")
		default:
			sb.WriteString("(. ")
		}
		dumpExpr(sb, n.Object)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Property)
		sb.WriteByte(')')
	case *ast.OptionalChain:
		sb.WriteString("(chain ")
		dumpExpr(sb, n.Base)
		sb.WriteByte(')')
	case *ast.Sequence:
		sb.WriteString("(seq")
		for _, x := range n.Exprs {
			sb.WriteByte(' ')
			dumpExpr(sb, x)
		}
		sb.WriteByte(')')
	case *ast.Spread:
		sb.WriteString("(... ")
		dumpExpr(sb, n.Arg)
		sb.WriteByte(')')
	case *ast.Yield:
		if n.Delegate {
			sb.WriteString("(yield*")
		} else {
			sb.WriteString("(yield")
		}
		if n.Arg != nil {
			sb.WriteByte(' ')
			dumpExpr(sb, n.Arg)
		}
		sb.WriteByte(')')
	case *ast.Await:
		sb.WriteString("(await ")
		dumpExpr(sb, n.Arg)
		sb.WriteByte(')')
	case *ast.ArrayPattern:
		sb.WriteString("(apat")
		for _, el := range n.Elements {
			sb.WriteByte(' ')
			dumpExpr(sb, el)
		}
		if n.Rest != nil {
			sb.WriteString(" (rest ")
			dumpExpr(sb, n.Rest)
			sb.WriteByte(')')
		}
		sb.WriteByte(')')
	case *ast.ObjectPattern:
		sb.WriteString("(opat")
		for _, prop := range n.Props {
			sb.WriteByte(' ')
			dumpProp(sb, prop)
		}
		if n.Rest != nil {
			sb.WriteString(" (rest ")
			dumpExpr(sb, n.Rest)
			sb.WriteByte(')')
		}
		sb.WriteByte(')')
	case *ast.AssignPattern:
		sb.WriteString("(def ")
		dumpExpr(sb, n.Target)
		sb.WriteByte(' ')
		dumpExpr(sb, n.Default)
		sb.WriteByte(')')
	case *ast.RestElement:
		sb.WriteString("(rest ")
		dumpExpr(sb, n.Arg)
		sb.WriteByte(')')
	default:
		fmt.Fprintf(sb, "<%T>", e)
	}
}

func dumpProp(sb *strings.Builder, prop ast.Property) {
	switch prop.Kind {
	case ast.PropSpread:
		sb.WriteString("(... ")
		dumpExpr(sb, prop.Value)
		sb.WriteByte(')')
		return
	case ast.PropGet:
		sb.WriteString("(get ")
	case ast.PropSet:
		sb.WriteString("(set ")
	default:
		if prop.Method {
			sb.WriteString("(method ")
		} else {
			sb.WriteString("(prop ")
		}
	}
	if prop.Static {
		sb.WriteString("static ")
	}
	if prop.Computed {
		sb.WriteString("[")
		dumpExpr(sb, prop.Key)
		sb.WriteString("] ")
	} else {
		dumpExpr(sb, prop.Key)
		sb.WriteByte(' ')
	}
	dumpExpr(sb, prop.Value)
	sb.WriteByte(')')
}

func dumpFunc(sb *strings.Builder, fn *ast.FuncLit) {
	sb.WriteByte('(')
	if fn.Async {
		sb.WriteString("async ")
	}
	switch fn.Kind {
	case ast.FuncArrow:
		sb.WriteString("arrow")
	default:
		if fn.Generator {
			sb.WriteString("function*")
		} else {
			sb.WriteString("function")
		}
	}
	if fn.Name != nil {
		sb.WriteByte(' ')
		sb.WriteString(fn.Name.Name)
	}
	sb.WriteString(" (")
	for i, param := range fn.Params {
		if i > 0 {
			sb.WriteByte(' ')
		}
		dumpExpr(sb, param)
	}
	sb.WriteString(") ")
	dumpStmts(sb, fn.Body)
	sb.WriteByte(')')
}

func dumpClass(sb *strings.Builder, cls *ast.ClassLit) {
	sb.WriteString("(class")
	if cls.Name != nil {
		sb.WriteByte(' ')
		sb.WriteString(cls.Name.Name)
	}
	if cls.Extends != nil {
		sb.WriteString(" extends ")
		dumpExpr(sb, cls.Extends)
	}
	for _, m := range cls.Members {
		sb.WriteByte(' ')
		dumpProp(sb, m)
	}
	for _, f := range cls.Fields {
		sb.WriteString(" (field ")
		if f.Static {
			sb.WriteString("static ")
		}
		dumpExpr(sb, f.Key)
		if f.Value != nil {
			sb.WriteByte(' ')
			dumpExpr(sb, f.Value)
		}
		sb.WriteByte(')')
	}
	for _, b := range cls.StaticBlocks {
		sb.WriteString(" (static-block ")
		dumpStmts(sb, b.Body)
		sb.WriteByte(')')
	}
	sb.WriteByte(')')
}
