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
	"strconv"
	"strings"
	"testing"
)

// TestRegLoopIsNotBig checks that runReg stays below Go's big-function
// size. Go's inliner calls a function of 5,000 or more IR nodes big and
// inlines into it only callees of cost 20 or less: past it, regInt32 and
// lexShadows became calls in every case and crypto_am3 went from 102 to
// 137 ns (docs/register-interpreter.md, "Where stage 1 stands"). -m=2,
// which prints the verdict, runs out of memory on this package, so the
// test reads -m's inlining of regInt32, which costs more than 20: every
// call of it written in runReg is inlined, or runReg is big.
func TestRegLoopIsNotBig(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the package with -gcflags=-m")
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	out, err := exec.Command(gotool, "build", "-gcflags=github.com/go-quickjs/go-quickjs/internal/vm=-m", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build -m: %v\n%s", err, out)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "zzreg_run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, last, calls := 0, 0, 0
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runReg" {
			continue
		}
		first, last = fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "regInt32" {
					calls++
				}
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("runReg calls no regInt32")
	}
	site := regexp.MustCompile(`zzreg_run\.go:(\d+):\d+: inlining call to regInt32`)
	inlined := 0
	for _, line := range strings.Split(string(out), "\n") {
		if m := site.FindStringSubmatch(line); m != nil {
			if n, _ := strconv.Atoi(m[1]); n >= first && n <= last {
				inlined++
			}
		}
	}
	if inlined != calls {
		t.Errorf("runReg inlines %d of its %d calls of regInt32: it has reached Go's big-function size; move code out of it", inlined, calls)
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
