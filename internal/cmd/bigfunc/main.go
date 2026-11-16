// Command bigfunc prints the size Go's inliner gives each large function of
// a package, against the size at which it calls a function big.
//
// Go's inliner calls a function of 5,000 or more IR nodes big
// (inlineBigFunctionNodes in cmd/compile/internal/inline) and inlines into
// it only callees of cost 20 or less: past it, every helper above that cost
// becomes a call in the whole function. The register interpreter's loop,
// runReg, is kept below it (docs/register-interpreter.md, "The loop"), and
// TestRegLoopCallsNoHelper says whether it calls what it should not; this
// says by how much it is below. -m=2, which prints the verdict and not the
// size, runs out of memory on package vm.
//
// It builds the toolchain's own compiler with inl.go given through
// -overlay, so that IsBigFunc prints the count it makes of each function of
// 2,500 nodes or more, and builds the package with it through -toolexec, in
// a build cache of its own kept under the user's cache directory: the first
// run compiles the standard library too, later ones only what changed.
//
//	go run ./internal/cmd/bigfunc                      # package vm
//	go run ./internal/cmd/bigfunc -func runReg -cases 40  # and its clauses of 40 nodes or more
//
// -force compiles a function as big whatever its size, to see what it would
// lose past the size before it is there: what -m says it inlines, and its
// code, against a build without -force. -small compiles it as not big, to
// see what a change does to its code with the inlining held. -gcflags, -o
// and -out are the build's, and where its output goes; -costs adds each
// inlinable function's cost to the output, as "inlcost" lines, which -m=2
// would print but runs out of memory on package vm:
//
//	go run ./internal/cmd/bigfunc -pkg ./cmd/qjs -force runReg -o /tmp/big.exe \
//		-gcflags github.com/go-quickjs/go-quickjs/internal/vm=-m -out /tmp/big-m.txt
//	go run ./internal/cmd/bigfunc -costs -out /tmp/costs.txt
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

// costAnchor is where CanInline records a function's cost.
const costAnchor = "\tn.Func.Inl = &ir.Inline{\n\t\tCost:            budget - visitor.budget,\n"

// reportSource is added to package inline: the count IsBigFunc makes, the
// credit for unified IR's "a, b = f()" included, made in full once a
// function and printed when it is 2,500 or more.
const reportSource = `package inline

import (
	"fmt"
	"os"
	"strings"

	"cmd/compile/internal/ir"
)

var bigFuncSeen = map[*ir.Func]bool{}

// forceBig reports whether BIGFUNC_FORCE names fn, by the end of its
// name: the compiler calls it big whatever its size (-force).
func forceBig(fn *ir.Func) bool {
	name := os.Getenv("BIGFUNC_FORCE")
	return name != "" && strings.HasSuffix(fmt.Sprint(fn), name)
}

// forceSmall reports whether BIGFUNC_SMALL names fn: the compiler calls it
// not big whatever its size (-small).
func forceSmall(fn *ir.Func) bool {
	name := os.Getenv("BIGFUNC_SMALL")
	return name != "" && strings.HasSuffix(fmt.Sprint(fn), name)
}

// reportCost prints fn's inlining cost where BIGFUNC_COSTS is set (-costs).
func reportCost(fn *ir.Func, cost int32) {
	if os.Getenv("BIGFUNC_COSTS") != "" {
		fmt.Printf("%v: inlcost %v %d\n", ir.Line(fn), fn, cost)
	}
}

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
	force := flag.String("force", "", "compile the functions whose name ends with this as big, whatever their size")
	small := flag.String("small", "", "compile the functions whose name ends with this as not big, whatever their size")
	gcflags := flag.String("gcflags", "", "the build's -gcflags, such as `pkg=-m`")
	o := flag.String("o", "", "the build's -o: where -pkg, a main package, is written")
	out := flag.String("out", "", "write the build's output, -gcflags=-m's included, to this file")
	costs := flag.Bool("costs", false, "print the inlining cost of each function that can be inlined, as `inlcost` lines of the build's output (-out)")
	flag.Parse()
	b := buildOpts{tags: *tags, force: *force, small: *small, gcflags: *gcflags, o: *o, out: *out, costs: *costs}
	if err := run(*pkg, *fn, b, *cases); err != nil {
		fmt.Fprintln(os.Stderr, "bigfunc:", err)
		os.Exit(1)
	}
}

// buildOpts is how run builds the package.
type buildOpts struct {
	tags, force, small, gcflags, o, out string
	costs                               bool
}

func run(pkg, fn string, b buildOpts, minCase int) error {
	tags := b.tags
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
	if b.gcflags != "" {
		args = append(args, "-gcflags", b.gcflags)
	}
	if b.o != "" {
		args = append(args, "-o", b.o)
	}
	cmd := exec.Command("go", append(args, pkg)...)
	// A forced build, and one that prints costs, has a cache of its own:
	// the go command's key for a compile includes neither BIGFUNC_FORCE
	// nor BIGFUNC_COSTS.
	gocache := filepath.Join(work, "cache")
	costs := ""
	if b.costs {
		costs = "1"
	}
	if b.force != "" || b.small != "" || b.costs {
		key := regexp.MustCompile(`[^A-Za-z0-9_]`).ReplaceAllString(b.force+"-"+b.small, "_")
		gocache = filepath.Join(work, "cache-force", key+"-"+costs)
	}
	cmd.Env = append(os.Environ(), "GOCACHE="+gocache, "BIGFUNC_COMPILE="+compiler, "BIGFUNC_FORCE="+b.force, "BIGFUNC_SMALL="+b.small, "BIGFUNC_COSTS="+costs, "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if b.out != "" {
		if err := os.WriteFile(b.out, out, 0o644); err != nil {
			return err
		}
	}
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
	if !bytes.Contains(src, []byte(costAnchor)) {
		return fmt.Errorf("%s has changed: CanInline no longer records the cost as costAnchor says", inl)
	}
	patched := bytes.Replace(src, []byte(patchAnchor), []byte(patchAnchor+"\treportBigFunc(fn)\n\tif forceBig(fn) {\n\t\treturn true\n\t}\n\tif forceSmall(fn) {\n\t\treturn false\n\t}\n"), 1)
	patched = bytes.Replace(patched, []byte(costAnchor), []byte("\treportCost(fn, budget-visitor.budget)\n"+costAnchor), 1)
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
