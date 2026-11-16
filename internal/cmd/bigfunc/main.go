// Command bigfunc prints the size Go's inliner gives each large function of
// a package, against the size at which it calls a function big.
//
// Go's inliner calls a function of 5,000 or more IR nodes big
// (inlineBigFunctionNodes in cmd/compile/internal/inline) and inlines into
// it only callees of cost 20 or less: past it, every helper above that cost
// becomes a call in the whole function. The register interpreter's loop,
// runReg, is kept below it (docs/register-interpreter.md, "The loop"), and
// TestRegLoopIsNotBig says whether it is; this says by how much. -m=2, which
// prints the verdict and not the size, runs out of memory on package vm.
//
// It builds the toolchain's own compiler with inl.go given through
// -overlay, so that IsBigFunc prints the count it makes of each function of
// 2,500 nodes or more, and builds the package with it through -toolexec, in
// a build cache of its own kept under the user's cache directory: the first
// run compiles the standard library too, later ones only what changed.
//
//	go run ./internal/cmd/bigfunc                      # package vm
//	go run ./internal/cmd/bigfunc -func runReg -cases 40  # and its clauses of 40 nodes or more
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// limit is inlineBigFunctionNodes.
const limit = 5000

// patchAnchor is where IsBigFunc starts counting.
const patchAnchor = "func IsBigFunc(fn *ir.Func) bool {\n\tbudget := inlineBigFunctionNodes\n"

// reportSource is added to package inline: the count IsBigFunc makes, the
// credit for unified IR's "a, b = f()" included, made in full once a
// function and printed when it is 2,500 or more.
const reportSource = `package inline

import (
	"fmt"

	"cmd/compile/internal/ir"
)

var bigFuncSeen = map[*ir.Func]bool{}

func reportBigFunc(fn *ir.Func) {
	if bigFuncSeen[fn] {
		return
	}
	bigFuncSeen[fn] = true
	size := 0
	ir.Any(fn, func(n ir.Node) bool {
		if n, ok := n.(*ir.AssignListStmt); ok && n.Op() == ir.OAS2 && len(n.Rhs) > 0 {
			if init := n.Rhs[0].Init(); len(init) == 1 {
				if _, ok := init[0].(*ir.AssignListStmt); ok {
					size -= 4*len(n.Lhs) + 1
				}
			}
		}
		size++
		return false
	})
	if size < 2500 {
		return
	}
	fmt.Printf("%v: bigfunc %v %d\n", ir.Line(fn), fn, size)
	// Each case clause's share, its body and its values, nested ones
	// within their clause's.
	var visit func(n ir.Node) bool
	visit = func(n ir.Node) bool {
		if cc, ok := n.(*ir.CaseClause); ok {
			k := 0
			ir.Any(cc, func(ir.Node) bool { k++; return false })
			fmt.Printf("%v: bigcase %v %d\n", ir.Line(cc), fn, k)
		}
		return ir.DoChildren(n, visit)
	}
	ir.DoChildren(fn, visit)
	// The callees it inlines while it is not big that it would call if
	// it were: their cost is above inlineBigFunctionMaxCost.
	calls := map[*ir.Func]int{}
	ir.Any(fn, func(n ir.Node) bool {
		call, ok := n.(*ir.CallExpr)
		if !ok || call.Fun == nil {
			return false
		}
		var name *ir.Name
		switch f := call.Fun.(type) {
		case *ir.Name:
			if f.Class == ir.PFUNC {
				name = f
			}
		case *ir.SelectorExpr:
			if f.Op() == ir.OMETHEXPR || f.Op() == ir.ODOTMETH {
				name = ir.MethodExprName(f)
			}
		}
		if name != nil && name.Func != nil && name.Func.Inl != nil && name.Func.Inl.Cost > inlineBigFunctionMaxCost {
			calls[name.Func]++
		}
		return false
	})
	for callee, k := range calls {
		fmt.Printf("%v: bigcallee %v %v cost %d, %d calls\n", ir.Line(fn), fn, callee, callee.Inl.Cost, k)
	}
}
`

// toolexecSource is the -toolexec program: it runs BIGFUNC_COMPILE for the
// compiler and every other tool as it is.
const toolexecSource = `package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	tool := os.Args[1]
	if strings.TrimSuffix(filepath.Base(tool), ".exe") == "compile" {
		tool = os.Getenv("BIGFUNC_COMPILE")
	}
	cmd := exec.Command(tool, os.Args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		os.Exit(1)
	}
}
`

func main() {
	pkg := flag.String("pkg", "./internal/vm", "the package to report")
	fn := flag.String("func", "", "report only functions whose name ends with this")
	cases := flag.Int("cases", 0, "list the case clauses of N nodes or more, nested ones too")
	tags := flag.String("tags", "", "build tags")
	flag.Parse()
	if err := run(*pkg, *fn, *tags, *cases); err != nil {
		fmt.Fprintln(os.Stderr, "bigfunc:", err)
		os.Exit(1)
	}
}

func run(pkg, fn, tags string, minCase int) error {
	if minCase <= 0 {
		minCase = 1 << 30
	}
	goroot, err := output("go", "env", "GOROOT")
	if err != nil {
		return err
	}
	version, err := output("go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	work := filepath.Join(cache, "go-quickjs-bigfunc", version)
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	compiler := filepath.Join(work, "compile"+exe)
	wrapper := filepath.Join(work, "toolexec"+exe)
	// Built once for each toolchain, and again when the report changes.
	if old, err := os.ReadFile(filepath.Join(work, "bigfunc_report.go")); err != nil || string(old) != reportSource {
		os.Remove(wrapper)
	}
	if _, err := os.Stat(wrapper); err != nil {
		if err := build(goroot, work, compiler, wrapper); err != nil {
			return err
		}
	}

	args := []string{"build", "-toolexec", wrapper}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(work, "cache"), "BIGFUNC_COMPILE="+compiler, "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	line := regexp.MustCompile(`^(\S+): big(func|case) (\S+) (\d+)$`)
	type big struct {
		pos, name string
		size      int
	}
	var bigs, clauses []big
	var callees []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if i := strings.Index(text, ": bigcallee "); i >= 0 {
			if f := strings.Fields(text[i+len(": bigcallee "):]); len(f) > 0 && strings.HasSuffix(f[0], fn) {
				callees = append(callees, "  inlined, above the big function's 20: "+strings.Join(f[1:], " "))
			}
			continue
		}
		m := line.FindStringSubmatch(text)
		if m == nil || !strings.HasSuffix(m[3], fn) {
			continue
		}
		n, _ := strconv.Atoi(m[4])
		if m[2] == "case" {
			if n >= minCase {
				clauses = append(clauses, big{m[1], m[3], n})
			}
			continue
		}
		bigs = append(bigs, big{m[1], m[3], n})
	}
	for _, c := range clauses {
		fmt.Printf("%5d  %s  %s\n", c.size, filepath.Base(c.pos), sourceLine(c.pos))
	}
	sort.Slice(bigs, func(i, j int) bool { return bigs[i].size > bigs[j].size })
	for _, b := range bigs {
		verdict := fmt.Sprintf("%d below", limit-b.size)
		if b.size >= limit {
			verdict = fmt.Sprintf("big, %d over", b.size-limit+1)
		}
		fmt.Printf("%-40s %5d of %d nodes: %s  (%s)\n", b.name, b.size, limit, verdict, filepath.Base(b.pos))
	}
	sort.Strings(callees)
	for _, c := range callees {
		fmt.Println(c)
	}
	if len(bigs) == 0 {
		fmt.Println("no function of 2,500 nodes or more")
	}
	return nil
}

// build makes the compiler with the report and the -toolexec program.
func build(goroot, work, compiler, wrapper string) error {
	inl := filepath.Join(goroot, "src", "cmd", "compile", "internal", "inline", "inl.go")
	src, err := os.ReadFile(inl)
	if err != nil {
		return err
	}
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(src, []byte(patchAnchor)) {
		return fmt.Errorf("%s has changed: IsBigFunc no longer starts as patchAnchor says", inl)
	}
	patched := bytes.Replace(src, []byte(patchAnchor), []byte(patchAnchor+"\treportBigFunc(fn)\n"), 1)
	if err := os.MkdirAll(filepath.Join(work, "toolexec-src"), 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"inl.go":               patched,
		"bigfunc_report.go":    []byte(reportSource),
		"toolexec-src/go.mod":  []byte("module toolexec\n\ngo 1.24\n"),
		"toolexec-src/main.go": []byte(toolexecSource),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(work, name), data, 0o644); err != nil {
			return err
		}
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {
		inl: filepath.Join(work, "inl.go"),
		filepath.Join(filepath.Dir(inl), "bigfunc_report.go"): filepath.Join(work, "bigfunc_report.go"),
	}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "overlay.json"), overlay, 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("go", "build", "-overlay", filepath.Join(work, "overlay.json"), "-o", compiler, "cmd/compile").CombinedOutput(); err != nil {
		return fmt.Errorf("building the compiler: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "build", "-o", wrapper, ".")
	cmd.Dir = filepath.Join(work, "toolexec-src")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building the -toolexec program: %v\n%s", err, out)
	}
	return nil
}

// sourceLine is the text at pos, file:line:col.
func sourceLine(pos string) string {
	parts := strings.Split(pos, ":")
	if len(parts) < 3 {
		return ""
	}
	file := strings.Join(parts[:len(parts)-2], ":")
	n, _ := strconv.Atoi(parts[len(parts)-2])
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[n-1])
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
