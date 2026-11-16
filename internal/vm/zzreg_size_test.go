package vm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRegLoopCallsNoHelper checks that every call runReg's source makes
// of a function Go can inline is inlined there. Go's inliner calls a
// function of 5,000 or more IR nodes big, and inlines into it only
// callees of cost 20 or less (inlineBigFunctionMaxCost in
// cmd/compile/internal/inline, Go 1.24 to 1.27; it is the only effect of
// being big there, with PGO's hot calls, which no build here has). So
// runReg may be big as long as it calls no helper above that cost: the
// helpers it needs are written to cost 20 or less, or written out in it
// (docs/register-interpreter.md, "The loop"). A call of one above it
// would become a call in every case it is in -- regInt32 and lexShadows
// once did, and crypto_am3 went from 102 to 137 ns. The test reads -m's
// inlining at each call's position: a callee of the package or of
// bytecode that -m says can be inlined, matched by name, must be inlined
// at the call.
func TestRegLoopCallsNoHelper(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the package with -gcflags=-m")
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	out, err := exec.Command(gotool, "build",
		"-gcflags=github.com/go-quickjs/go-quickjs/internal/vm=-m",
		"-gcflags=github.com/go-quickjs/go-quickjs/internal/bytecode=-m",
		".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build -m: %v\n%s", err, out)
	}
	// The last part of a name -m prints: (*propCache).holder's holder,
	// Value.IsString's IsString, regInt32's regInt32.
	last := func(s string) string {
		if i := strings.LastIndexByte(s, '.'); i >= 0 {
			return s[i+1:]
		}
		return s
	}
	// A function -m names bare, a method by its type: Value.IsString,
	// (*propCache).holder.
	funcs, methods := map[string]bool{}, map[string]bool{}
	inlined := map[string]map[string]bool{}
	can := regexp.MustCompile(`: can inline (\S+)`)
	site := regexp.MustCompile(`zzreg_run\.go:(\d+:\d+): inlining call to (\S+)`)
	for _, line := range strings.Split(string(out), "\n") {
		if m := can.FindStringSubmatch(line); m != nil {
			if strings.Contains(m[1], ".") {
				methods[last(m[1])] = true
			} else {
				funcs[m[1]] = true
			}
		}
		if m := site.FindStringSubmatch(line); m != nil {
			if inlined[m[1]] == nil {
				inlined[m[1]] = map[string]bool{}
			}
			inlined[m[1]][last(m[2])] = true
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "zzreg_run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runReg" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// A function of the package, a function of bytecode, or a
			// method; len and the other built-ins are none of them.
			var name string
			can := false
			switch f := c.Fun.(type) {
			case *ast.Ident:
				name = f.Name
				can = funcs[name]
			case *ast.SelectorExpr:
				name = f.Sel.Name
				if x, ok := f.X.(*ast.Ident); ok && x.Name == "bytecode" {
					can = funcs[name]
				} else {
					can = methods[name]
				}
			}
			if !can {
				return true
			}
			checked++
			p := fset.Position(c.Lparen)
			if pos := fmt.Sprintf("%d:%d", p.Line, p.Column); !inlined[pos][name] {
				t.Errorf("zzreg_run.go:%s: runReg calls %s, which Go inlines elsewhere: past 5,000 nodes it inlines only callees of cost 20 or less; write it out or make it cheaper", pos, name)
			}
			return true
		})
	}
	if checked < 100 {
		t.Fatalf("runReg has %d calls of inlinable functions; the test is not reading it", checked)
	}
}

// TestRegDispatchKeepsNoPC checks that runReg's dispatch stores nothing of
// the pc to the stack: a case that reads the pc after a call it made
// without reading it back from kept makes the pc live across the call, and
// Go then saves it at every dispatch. A goto slow after regSetMiss did, and
// crypto_am3 and a dozen loops were 8 to 11% slower at every placement
// (docs/register-interpreter.md, "The loop").
func TestRegDispatchKeepsNoPC(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and disassembles cmd/qjs")
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	src, err := os.ReadFile("zzreg_run.go")
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(src), "\n") {
		if strings.TrimSpace(l) == "w := regAt(code, pc)" {
			line = i + 2 // the pc++ after it
			break
		}
	}
	if line == 0 {
		t.Fatal("no dispatch in zzreg_run.go")
	}
	exe := filepath.Join(t.TempDir(), "qjs.exe")
	if out, err := exec.Command(gotool, "build", "-o", exe, "../../cmd/qjs").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(gotool, "tool", "objdump", "-s", `vm\.\(\*Runtime\)\.runReg$`, exe).CombinedOutput()
	if err != nil {
		t.Fatalf("objdump: %v\n%s", err, out)
	}
	at := fmt.Sprintf("zzreg_run.go:%d\t", line)
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.Contains(l, at) {
			continue
		}
		n++
		if strings.Contains(l, "(SP)") {
			t.Errorf("runReg's dispatch stores the pc: %s", strings.TrimSpace(l))
		}
	}
	if n == 0 {
		t.Fatalf("no instruction of %s in runReg", at)
	}
}
