package vm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestRegThrowsKeepPC checks that every handler and slow function of the
// register interpreter (zzreg_table*.go) that goes to the fail tail --
// st.fail, or st.done and st.put, which go there with an error -- has
// kept the pc past its instruction first, by st.at or st.kept.pc =: the
// tail finds the handler of the instruction at st.kept.pc-1, and a pc left
// from an instruction before would find another's or none. A call that
// reads the kept pc (rhCallW, regElemMiss) is given it the same way.
func TestRegThrowsKeepPC(t *testing.T) {
	files, err := filepath.Glob("zzreg_table*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no handler files: %v", err)
	}
	fs := token.NewFileSet()
	fails := 0
	isSt := func(e ast.Expr, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "st"
	}
	for _, name := range files {
		file, err := parser.ParseFile(fs, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil {
				continue
			}
			// The position of the first keep of the pc in the function,
			// and every use of the tail or of the kept pc after it.
			kept := token.NoPos
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "pc" && isSt(sel.X, "kept") && kept == token.NoPos {
						kept = n.Pos()
					}
				case *ast.CallExpr:
					if isSt(n.Fun, "at") && kept == token.NoPos {
						kept = n.Pos()
					}
					uses := isSt(n.Fun, "fail") || isSt(n.Fun, "done") || isSt(n.Fun, "put")
					for _, a := range n.Args {
						if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND && isSt(u.X, "kept") {
							uses = true
						}
					}
					if uses {
						fails++
						// st.done(r.f(st.at(pc), ...)) keeps it in an argument.
						ast.Inspect(n, func(m ast.Node) bool {
							if c, ok := m.(*ast.CallExpr); ok && isSt(c.Fun, "at") && kept == token.NoPos {
								kept = c.Pos()
							}
							return true
						})
						if kept == token.NoPos || kept > n.End() {
							t.Errorf("%v: %s goes to the fail tail, or reads the kept pc, before it keeps the pc", fs.Position(n.Pos()), fd.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
	if fails < 100 {
		t.Fatalf("only %d uses of the fail tail found: the test is not reading the handlers", fails)
	}
}
