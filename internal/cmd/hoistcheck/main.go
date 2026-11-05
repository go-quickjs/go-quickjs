// Command hoistcheck finds the pointer sums Go's loop invariant pass moves
// out of the loops and branches that compute them, and fails for one it
// does not know to be safe.
//
//	go run ./internal/cmd/hoistcheck            # from the repository root
//	go run ./internal/cmd/hoistcheck -v         # every sum moved, known or not
//	go run ./internal/cmd/hoistcheck -p 2       # on a machine that is busy
//
// Go 1.27's pass (cmd/compile/internal/ssa/licm.go) means to leave pointer
// arithmetic where the program has it -- "can't move pointer arithmetic, as
// it may be guarded by conditionals and thus could materialize a bad
// pointer across a safepoint" -- but tests Type.IsPtr, which a *T has and
// an unsafe.Pointer has not. unsafe.Add's sum is an unsafe.Pointer, so the
// pass moves it out of an innermost loop, and out of the branch that guards
// it, to the loop's preheader, wherever its operands are defined outside
// the loop and the sum is used as an address rather than folded into a
// load's or a store's: passed to a call, stored, or, under -d=checkptr,
// given to checkptr's check, which makes almost every sum one. Where the
// base is nil on the path the branch excluded, or the offset past its
// allocation -- an object's side fields (extAt, dataExtAt) when it has none
// -- the sum is then a bad pointer in a slot live across the loop's calls,
// and a stack's copy or a collection throws ("invalid pointer found on
// stack"). jitSortCopy's a.extAt() was one, with a nil a. It is reported as
// golang/go#82865: once a Go release fixes it, and go.mod requires that Go,
// hoistcheck and its CI job can go.
//
// No Go reports what the pass moves, so hoistcheck builds one that does:
// the toolchain's own compiler, with licm.go given, through -overlay, a
// line that reports each pointer-shaped value it moves. It then builds
// every package and test binary of the module with it, through -toolexec
// and a build cache of its own, with and without the JIT and with and
// without -d=checkptr=2, and compares each sum moved in the module's code
// with known: a sum is known by its file, its function and its line's text,
// with why its base exists, and has its offset, wherever the loop is
// entered. A new one fails until the code reads what it needs before the
// loop, or reaches it by a field's address (Object.arrayExt), or it is
// added to known with its reason.
//
// It needs the toolchain's sources, which every Go distribution has, and
// does nothing with a Go that has no loop invariant pass. It takes a few
// minutes: the build cache is its own, so the standard library is compiled
// again too.
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
	"strings"
)

// known is every sum the pass moves in the module's code, plain or with the
// JIT, with checkptr or without, that is safe: its key (sumKey) and why.
var known = map[string]string{
	// The array the method made before the loop, which has side fields
	// wherever the loop is entered.
	"internal/vm/builtins_extra.go|(*Runtime).initArrayExtras2.func|out.extOf().elems = append(out.elemsOf(), v)":            "out is the array arrayCreate made",
	"internal/vm/builtins_extra.go|(*Runtime).initArrayExtras2.func|out.extOf().elems = append(out.elemsOf(), arg(args, 1))": "out is the array arrayCreate made",
	"internal/vm/iterate.go|(*Runtime).iterRest|out.extOf().elems = append(out.elemsOf(), v)":                                "out is the array newObject made",
	"internal/vm/iterate.go|(*Runtime).iterToArray|for want == bytecode.IterAll || uint32(len(out.elemsOf())) < want {":      "out is the array newObject made",
	"internal/vm/iterate.go|(*Runtime).iterToArray|out.extOf().elems = append(out.elemsOf(), v)":                             "out is the array newObject made",
	// An array, which the function checked before the loop.
	"internal/vm/iterate.go|elemKeysOnly|if holeAt(&o.extOf().elems[i]) {": "o is a ClassArray",
	// An object setElem's callers give it with side fields; one without
	// would panic in extOf before the loop's first call.
	"internal/vm/vm_object.go|(*Object).setElem|for uint(len(o.elemsOf())) < uint(i) {":          "o has side fields",
	"internal/vm/vm_object.go|(*Object).setElem|o.extOf().elems = append(o.elemsOf(), elemHole)": "o has side fields",
	// A runtime's counters, at their offsets in it.
	"stopwatch.go|pokeEngine|*field(vm.OffsetInterruptCounter), *field(vm.OffsetBackEdges) = 0, 0":     "rt is never nil",
	"stopwatch.go|pokeEngine|if *field(vm.OffsetFrameDepth) == 0 && *field(vm.OffsetHostCalls) == 0 {": "rt is never nil",
}

// patchAnchor is where in licm.go a value is moved; the report goes before
// it.
const patchAnchor = "\t\t\tb.Values[i] = nil\n\t\t\tv.Block = dest\n"

// patchReport reports a pointer-shaped value the pass moves.
const patchReport = "\t\t\tif v.Type.IsPtrShaped() {\n" +
	"\t\t\t\tf.Warnl(v.Pos, \"licm: hoisted %s %v from b%d to b%d in %s\", v.Op, v.Type, b.ID, dest.ID, f.Name)\n" +
	"\t\t\t}\n"

// reportLine is one report: file:line:col: licm: hoisted OP TYPE from bN to bM in FUNC.
var reportLine = regexp.MustCompile(`^(.+?):(\d+):\d+: licm: hoisted (\S+) (\S+) from b\d+ to b\d+ in (.+)$`)

// parallel is the builds' -p.
var parallel int

// closureSuffix is a closure's number, which a closure added before it
// changes.
var closureSuffix = regexp.MustCompile(`\.func\d+`)

// sum is one value the pass moved.
type sum struct {
	file, fn, line string // the module-relative file, the function, the line's text
	pos            string // where, as the compiler said it
	configs        map[string]bool
}

// key is how known names s.
func (s *sum) key() string { return s.file + "|" + s.fn + "|" + s.line }

func main() {
	verbose := flag.Bool("v", false, "print every sum moved, known or not")
	keep := flag.String("work", "", "keep the patched compiler and the build cache in this directory")
	flag.IntVar(&parallel, "p", runtime.NumCPU(), "the builds' -p")
	flag.Parse()
	if err := run(*verbose, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "hoistcheck:", err)
		os.Exit(2)
	}
}

func run(verbose bool, work string) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return fmt.Errorf("run from the repository root: %v", err)
	}
	goroot := strings.TrimSpace(output(exec.Command("go", "env", "GOROOT")))
	licm := filepath.Join(goroot, "src", "cmd", "compile", "internal", "ssa", "licm.go")
	src, err := os.ReadFile(licm)
	if os.IsNotExist(err) {
		fmt.Println("hoistcheck: this Go has no loop invariant pass; nothing to check")
		return nil
	} else if err != nil {
		return err
	}
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	if !bytes.Contains(src, []byte(patchAnchor)) {
		return fmt.Errorf("%s has changed: patchAnchor no longer finds where it moves a value", licm)
	}
	patched := bytes.Replace(src, []byte(patchAnchor), []byte(patchReport+patchAnchor), 1)

	if work == "" {
		if work, err = os.MkdirTemp("", "hoistcheck"); err != nil {
			return err
		}
		defer os.RemoveAll(work)
	} else if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	work, err = filepath.Abs(work)
	if err != nil {
		return err
	}

	// The compiler, with the report.
	if err := os.WriteFile(filepath.Join(work, "licm.go"), patched, 0o644); err != nil {
		return err
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {licm: filepath.Join(work, "licm.go")}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(work, "overlay.json"), overlay, 0o644); err != nil {
		return err
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	compiler := filepath.Join(work, "compile"+exe)
	if err := do(root, nil, "go", "build", "-overlay", filepath.Join(work, "overlay.json"), "-o", compiler, "cmd/compile"); err != nil {
		return fmt.Errorf("building the compiler: %v", err)
	}

	// The -toolexec program, which runs that compiler for the go command's.
	// Its source has a directory of its own name, apart from the binary's,
	// which has no suffix where the binary has none.
	wrapDir := filepath.Join(work, "toolexec-src")
	if err := os.MkdirAll(wrapDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wrapDir, "go.mod"), []byte("module toolexec\n\ngo 1.24\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wrapDir, "main.go"), []byte(toolexecSource), 0o644); err != nil {
		return err
	}
	wrapper := filepath.Join(work, "toolexec"+exe)
	if err := do(wrapDir, nil, "go", "build", "-o", wrapper, "."); err != nil {
		return fmt.Errorf("building the -toolexec program: %v", err)
	}

	env := []string{
		"GOCACHE=" + filepath.Join(work, "cache"),
		"HOISTCHECK_COMPILE=" + compiler,
		"GOFLAGS=",
	}
	sums := map[string]*sum{}
	for _, tags := range []string{"", "quickjs_jit"} {
		for _, gc := range []string{"", "all=-d=checkptr=2"} {
			config := "plain"
			if tags != "" {
				config = "jit"
			}
			if gc != "" {
				config += "+checkptr"
			}
			args := []string{"-p", fmt.Sprint(parallel), "-toolexec", wrapper}
			if tags != "" {
				args = append(args, "-tags", tags)
			}
			if gc != "" {
				args = append(args, "-gcflags", gc)
			}
			var log bytes.Buffer
			if err := doTo(&log, root, env, "go", append(append([]string{"build"}, args...), "./...")...); err != nil {
				return fmt.Errorf("go build (%s): %v\n%s", config, err, log.String())
			}
			list := []string{"list", "-f", "{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}"}
			if tags != "" {
				list = append(list, "-tags", tags)
			}
			for _, pkg := range strings.Fields(output(exec.Command("go", append(list, "./...")...))) {
				test := append(append([]string{"test", "-c", "-o", filepath.Join(work, "pkg.test"+exe)}, args...), pkg)
				if err := doTo(&log, root, env, "go", test...); err != nil {
					return fmt.Errorf("go test -c %s (%s): %v\n%s", pkg, config, err, log.String())
				}
			}
			if err := collect(root, log.String(), config, sums); err != nil {
				return err
			}
		}
	}

	var keys []string
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	unknown := 0
	for _, k := range keys {
		s := sums[k]
		reason, ok := known[k]
		if !ok {
			unknown++
		}
		if ok && !verbose {
			continue
		}
		var configs []string
		for c := range s.configs {
			configs = append(configs, c)
		}
		sort.Strings(configs)
		status := "NEW"
		if ok {
			status = "known: " + reason
		}
		fmt.Printf("%s\n\t%s (%s)\n\tkey %q\n\t%s\n", s.pos, s.fn, strings.Join(configs, ", "), k, status)
	}
	for k := range known {
		if sums[k] == nil {
			fmt.Printf("stale: %q is no longer moved\n", k)
		}
	}
	fmt.Printf("hoistcheck: %d sums moved, %d not known\n", len(sums), unknown)
	if unknown > 0 {
		fmt.Println("Read what the loop needs before it, or reach it by a field's address, or add the key to known with why its base exists wherever the loop is entered.")
		os.Exit(1)
	}
	return nil
}

// collect adds the reports in log of the module's own code to sums.
func collect(root, log, config string, sums map[string]*sum) error {
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		m := reportLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		file := m[1]
		if !filepath.IsAbs(file) {
			file = filepath.Join(root, file)
		}
		rel, err := filepath.Rel(root, file)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // the standard library's
		}
		rel = filepath.ToSlash(rel)
		text, err := lineOf(file, m[2])
		if err != nil {
			return err
		}
		s := &sum{file: rel, fn: closureSuffix.ReplaceAllString(m[5], ".func"), line: text, pos: m[1] + ":" + m[2]}
		if old := sums[s.key()]; old != nil {
			s = old
		} else {
			s.configs = map[string]bool{}
			sums[s.key()] = s
		}
		s.configs[config] = true
	}
	return sc.Err()
}

// lineOf is the text of line n of file, its spaces trimmed.
func lineOf(file, n string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var i int
	fmt.Sscan(n, &i)
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if i < 1 || i > len(lines) {
		return "", fmt.Errorf("%s has no line %s", file, n)
	}
	return strings.TrimSpace(lines[i-1]), nil
}

// do runs a command in dir with env added, its output to the terminal.
func do(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// doTo runs a command in dir with env added, its output to w.
func doTo(w *bytes.Buffer, dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// output is a command's standard output, or nothing if it fails.
func output(cmd *exec.Cmd) string {
	out, _ := cmd.Output()
	return string(out)
}

// toolexecSource is the -toolexec program: it runs HOISTCHECK_COMPILE for
// the compiler and every other tool as it is.
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
		tool = os.Getenv("HOISTCHECK_COMPILE")
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
