package asmjs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// modules finds the "use asm" functions of src, a script.
func modules(t *testing.T, src string) []*ast.FuncLit {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.FuncLit
	var walk func(n any)
	walk = func(n any) {
		switch n := n.(type) {
		case *ast.FuncLit:
			if IsModule(n, src) {
				out = append(out, n)
				return
			}
			for _, s := range n.Body {
				walk(s)
			}
		case *ast.FuncDecl:
			walk(n.Fn)
		case *ast.ExprStmt:
			walk(n.X)
		case *ast.VarDecl:
			for _, d := range n.Decls {
				walk(d.Init)
			}
		case *ast.Call:
			walk(n.Callee)
			for _, a := range n.Args {
				walk(a)
			}
		case *ast.Assign:
			walk(n.Value)
		case *ast.ReturnStmt:
			walk(n.Arg)
		case *ast.BlockStmt:
			for _, s := range n.Body {
				walk(s)
			}
		case *ast.IfStmt:
			walk(n.Cons)
			walk(n.Alt)
		}
	}
	for _, s := range prog.Body {
		walk(s)
	}
	return out
}

func TestValidateMicroKernels(t *testing.T) {
	b, err := os.ReadFile("../../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "\r\n", "\n")
	mods := modules(t, src)
	if len(mods) != 1 {
		t.Fatalf("%d modules, want 1", len(mods))
	}
	m, err := Validate(mods[0], src)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Funcs) != 17 || len(m.Tables) != 1 || len(m.Exports) != 12 {
		t.Fatalf("%d functions, %d tables, %d exports", len(m.Funcs), len(m.Tables), len(m.Exports))
	}
	for _, f := range m.Funcs {
		t.Logf("%s %v -> %v, %d locals", f.Name, f.Sig.Params, f.Sig.Ret, len(f.Locals))
	}
}

// TestValidateZlib validates Octane's zlib, whose module is in the source
// zlib-data.js evaluates, where QUICKJS_OCTANE_DIR names a checkout.
func TestValidateZlib(t *testing.T) {
	dir := os.Getenv("QUICKJS_OCTANE_DIR")
	if dir == "" {
		t.Skip("QUICKJS_OCTANE_DIR is not set")
	}
	b, err := os.ReadFile(filepath.Join(dir, "zlib-data.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := zlibSource(t, string(b))
	mods := modules(t, src)
	if len(mods) != 1 {
		t.Fatalf("%d modules, want 1", len(mods))
	}
	m, err := Validate(mods[0], src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d functions, %d tables, %d exports, %d globals", len(m.Funcs), len(m.Tables), len(m.Exports), len(m.Globals))
}

// zlibSource is the source zlib-data.js passes to zlibEval: the string
// literal of its call, unescaped.
func zlibSource(t *testing.T, data string) string {
	t.Helper()
	data = strings.ReplaceAll(data, "\r\n", "\n")
	i := strings.Index(data, "{zlibEval('")
	if i < 0 {
		t.Fatal("no zlibEval in zlib-data.js")
	}
	var sb strings.Builder
	for j := i + len("{zlibEval('"); j < len(data); j++ {
		c := data[j]
		switch c {
		case '\\':
			j++
			switch data[j] {
			case '\n':
			case 'n':
				sb.WriteByte('\n')
			default:
				sb.WriteByte(data[j])
			}
		case '\'':
			return sb.String()
		default:
			sb.WriteByte(c)
		}
	}
	t.Fatal("zlib-data.js's string does not end")
	return ""
}
