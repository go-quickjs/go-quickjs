package vm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRegThrowsKeepPC checks that every way runReg's loop leaves for its
// fail tail has stored the pc in kept first, in the same case or tail:
// the tail finds the handler of the instruction at kept.pc-1, and a pc
// left from an instruction before would find another's or none. It checks
// too that the loop returns an error nowhere but from the tail, which is
// where a handler is looked for.
func TestRegThrowsKeepPC(t *testing.T) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "zzreg_run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var loop *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runReg" {
			loop = fd
		}
	}
	if loop == nil {
		t.Fatal("no runReg")
	}
	var forBody *ast.BlockStmt
	for _, s := range loop.Body.List {
		if fs, ok := s.(*ast.ForStmt); ok {
			forBody = fs.Body
		}
	}
	if forBody == nil {
		t.Fatal("runReg has no loop")
	}
	failAt := token.NoPos
	for _, s := range forBody.List {
		if l, ok := s.(*ast.LabeledStmt); ok && l.Label.Name == "fail" {
			failAt = l.Pos()
		}
	}
	if failAt == token.NoPos {
		t.Fatal("runReg's loop has no fail tail")
	}
	stored, gotos := false, 0
	isKeptPC := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "pc" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "kept"
	}
	ast.Inspect(forBody, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CaseClause:
			stored = false
		case *ast.LabeledStmt:
			stored = false
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && isKeptPC(n.Lhs[0]) {
				if id, ok := n.Rhs[0].(*ast.Ident); ok && id.Name == "pc" {
					stored = true
				}
			}
		case *ast.BranchStmt:
			if n.Tok == token.GOTO && n.Label.Name == "fail" {
				gotos++
				if !stored {
					t.Errorf("%v: goto fail with no kept.pc = pc before it", fs.Position(n.Pos()))
				}
			}
		case *ast.ReturnStmt:
			if len(n.Results) == 2 && n.Pos() < failAt {
				if id, ok := n.Results[1].(*ast.Ident); !ok || id.Name != "nil" {
					t.Errorf("%v: an error returned past the handlers", fs.Position(n.Pos()))
				}
			}
		}
		return true
	})
	if gotos == 0 {
		t.Error("no goto fail found")
	}
}
