// Command layoutpad keeps package vm's layout lock: dead padding, generated,
// that puts the hot code and the Go runtime's hot data at the addresses a
// set of recorded targets gives them, whatever a change does around them.
//
// Package vm's speed on AMD Zen 2 depends on where its hot code falls
// modulo 4 KB, which the op cache indexes it by, and the Go runtime's hot
// variables within their 64-byte cache lines; a change anywhere moves what
// comes after it, and micro-benchmarks running the same instructions have
// measured 3 to 10% slower at every placement for it. The lock pins a build's
// layout so that a change measures as its code: each pad absorbs what the
// code before it moved.
//
// The levers, each a pad in a file internal/vm sorts where its gap must
// open, its size a constant of the build's configuration
// (layoutpad_GOOS_GOARCH.go, layoutpad_jit_GOOS_GOARCH.go), all of them
// byte stores to a variable never read, 7 bytes of code a store, so that a
// pad fills any whole number of 32-byte slots:
//
//   - A function pad (layoutPadA ... layoutPadZ): it moves every function
//     after it in file order and, since every function comes before every
//     closure and generic, everything after.
//   - A generic pad (layoutPadGA ... layoutPadGZ) the function pad calls.
//     Go reads a package's function bodies last first, then its methods',
//     and instantiates a generic where a body first meets it: a generic one
//     file's function calls is laid out after those the files after it meet
//     first and before those of the files before it. It moves the generics
//     after it.
//   - A method's generic pad (layoutPadMA ... layoutPadMZ), which a method
//     of *Runtime at the file calls, the same among the generics methods
//     meet first.
//   - A generic closure pad (layoutPadCZ), a closure in a generic the last
//     file calls: the closures of generics come first among the generics.
//   - Data pads: 8-byte pointer variables (DH) and 8- and 16-byte variables
//     without pointers (DW, DG), which the linker sorts by size among the
//     zeroed data before the Go runtime's mheap_, writeBarrier and
//     gcController, and so moves within their cache lines.
//
// docs/optimizing.md ("The layout lock") says what each can and cannot move.
//
// The pads are never called and their variables never read: one init calls
// them behind a variable that is never true, which keeps the linker from
// dropping them. The constants are all that changes from one regeneration to
// the next, and the pads' data is the same size whatever they are but the
// data levers' own. A pad only grows: an anchor a change moves back by 32
// bytes takes a pad 4,064 bytes longer.
//
// The targets (internal/vm/layoutlock.txt) give, for each configuration --
// GOOS, GOARCH and with the JIT or not -- each lever's anchor, the symbol
// right after its gap, and the address the anchor must have modulo 4,096
// for code and 64 for data, in a build of internal/cmd/v8bench made as
//
//	CGO_ENABLED=0 GOAMD64=v1 go build -buildvcs=false [-tags quickjs_jit] ./internal/cmd/v8bench
//
// with the toolchain the targets name; that is how the placements tool
// builds too. A lever without an anchor in a configuration is left out of
// its builds, and a generic pad no configuration anchors is not written.
// The targets are one toolchain's: the tool refuses another (-check exits 3,
// which TestLayoutLock takes as a skip), and other platforms build no pads
// (layoutpad_other.go).
//
//	go run ./internal/cmd/layoutpad             # pad every configuration to its targets
//	go run ./internal/cmd/layoutpad -check      # this machine's configurations: exit 1 on drift
//	go run ./internal/cmd/layoutpad -show       # where each pad and anchor is, and what follows each pad
//	go run ./internal/cmd/layoutpad -record     # set the targets to the tree's present layout
//
// -record, run at a base revision with the tool built beforehand and the
// pad files moved out of the tree, takes the base's layout as the targets:
// the anchors named in the targets file (-targets FILE, outside the tree),
// at the addresses they have there.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	vmDir      = "internal/vm"
	targetsRel = "internal/vm/layoutlock.txt"
	vmPrefix   = "github.com/go-quickjs/go-quickjs/internal/vm."
	// codeMod and dataMod are what a target is modulo: the op cache's 4 KB
	// for code, the cache line for data.
	codeMod = 4096
	dataMod = 64
	// slot is the alignment of every function on amd64.
	slot = 32
	// maxStores is the most byte stores a pad has, storeBits the bits of
	// its lever's constant, storesALine how many the source has on a line,
	// and padBytes the bytes they store to: two a store. A store is 7
	// bytes of code, and 4,096 bytes and a slot take 590.
	maxStores   = 1<<storeBits - 1
	storeBits   = 10
	storesALine = 8
	padBytes    = 2 * (maxStores + 1)
	// perStore is a store's bytes of code, before a build measures it.
	perStore = 7
	// dataVars is how many variables each data lever has.
	dataVars = 8
)

// A position is where a function pad, its generic pad and its method's go.
type position struct {
	name string // the lever's letter
	file string // the file in internal/vm, sorted where the gap opens
	doc  string
}

var positions = []position{
	{"A", "aa_layoutpad.go", "before every file of package vm: the built-ins"},
	{"D", "defg_layoutpad.go", "after deferred.go, before define.go: the files after the built-ins"},
	{"R", "tree_a_layoutpad.go", "after temporal_yearmonth.go, before tree_build.go: the tree's builder and slow paths"},
	{"V", "vl_layoutpad.go", "after value.go, before vm.go: executeAt"},
	{"B", "vm_b_layoutpad.go", "after vm_amath.go, before vm_call.go: callObject, callDirect, runFD"},
	{"S", "vm_sz_layoutpad.go", "after vm_shape.go, before vm_tree.go: runTree, backEdgeCheck"},
	{"T", "zcall_tl_layoutpad.go", "after zcall_tail.go, before zcall_tree.go: callTree, runTreeNested"},
	{"J", "zzj_layoutpad.go", "after zzbigown.go, before the JIT's zzjit files"},
	{"Z", "zzzz_layoutpad.go", "after every file: the closures, and the generics the last files call"},
}

// A lever is one constant of a configuration's file.
type lever struct {
	name string // as in the targets file: A..Z, GA..GZ, MA..MZ, CZ, DH, DW, DG
	kind byte   // 'f' function pad, 'g' generic pad, 'c' generic closure pad, 'd' data
	pad  string // the pad's symbol, for code levers
	max  int    // the most the constant may be, for data levers
}

func levers() []lever {
	var ls []lever
	for _, p := range positions {
		ls = append(ls, lever{name: p.name, kind: 'f', pad: vmPrefix + "layoutPad" + p.name})
		ls = append(ls, lever{name: "G" + p.name, kind: 'g', pad: vmPrefix + "layoutPadG" + p.name + "[go.shape.struct {}]"})
		ls = append(ls, lever{name: "M" + p.name, kind: 'g', pad: vmPrefix + "layoutPadM" + p.name + "[go.shape.struct {}]"})
	}
	ls = append(ls, lever{name: "CZ", kind: 'c', pad: vmPrefix + "layoutPadCZ[go.shape.struct {}].func1"})
	ls = append(ls,
		lever{name: "DH", kind: 'd', max: dataVars},
		lever{name: "DW", kind: 'd', max: dataVars},
		lever{name: "DG", kind: 'd', max: dataVars / 2})
	return ls
}

func leverByName(name string) (lever, bool) {
	for _, l := range levers() {
		if l.name == name {
			return l, true
		}
	}
	return lever{}, false
}

// A config is one build the targets pin.
type config struct {
	goos, goarch string
	jit          bool
	anchors      []anchor
}

func (c *config) String() string {
	s := c.goos + "/" + c.goarch
	if c.jit {
		return s + " jit"
	}
	return s + " plain"
}

// An anchor is the symbol a lever puts at its target.
type anchor struct {
	lever  string
	symbol string
	target uint64
}

type targets struct {
	toolchain string
	configs   []*config
}

func main() {
	check := flag.Bool("check", false, "build this machine's configurations and report anchors away from their targets; exit 1 if any is")
	show := flag.Bool("show", false, "print each pad, the symbol after it and each anchor, for this machine's configurations")
	record := flag.Bool("record", false, "set every configuration's targets to where its anchors are in the tree as it is")
	only := flag.String("config", "", "only the configurations named, comma-separated, as GOOS/GOARCH/plain or GOOS/GOARCH/jit")
	verbose := flag.Bool("v", false, "print each build's anchors away from their targets and what each lever is set to")
	targetsPath := flag.String("targets", targetsRel, "the targets file, from the repository's root")
	all := flag.Bool("all", false, "with -show, write the pads and set every lever of the configurations to 0 first, to see where each lands")
	flag.Parse()
	root, err := repoRoot()
	if err == nil {
		err = os.Chdir(root)
	}
	if err != nil {
		fatal(err)
	}
	t, err := readTargets(*targetsPath)
	if err != nil {
		fatal(err)
	}
	if v := goVersion(); v != t.toolchain && !*record {
		msg := fmt.Sprintf("the targets are %s's layout and this is %s: run with GOTOOLCHAIN=%s, or -record new targets", t.toolchain, v, t.toolchain)
		if *check {
			fmt.Println("layoutpad: skipped:", msg)
			os.Exit(3)
		}
		fatal(errors.New(msg))
	}
	var picked []*config
	for _, c := range t.configs {
		if *only != "" {
			name := c.goos + "/" + c.goarch + "/" + map[bool]string{false: "plain", true: "jit"}[c.jit]
			if !slices.Contains(strings.Split(*only, ","), name) {
				continue
			}
		} else if (*check || *show) && (c.goos != runtime.GOOS || c.goarch != runtime.GOARCH) {
			continue
		}
		picked = append(picked, c)
	}
	if len(picked) == 0 {
		if *check {
			fmt.Printf("layoutpad: skipped: no targets for %s/%s\n", runtime.GOOS, runtime.GOARCH)
			os.Exit(3)
		}
		fatal(errors.New("no configuration picked"))
	}
	switch {
	case *check:
		drift := false
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			for _, line := range compare(c, syms) {
				fmt.Printf("%s: %s\n", c, line)
				drift = true
			}
			if !drift {
				fmt.Printf("%s: every anchor at its target\n", c)
			}
		}
		if drift {
			fmt.Println("layoutpad: the layout has drifted from its targets: run go generate ./internal/vm (go run ./internal/cmd/layoutpad)")
			os.Exit(1)
		}
	case *show:
		if *all {
			if err := writeSources(t); err != nil {
				fatal(err)
			}
			for _, c := range picked {
				v := readValues(c)
				for _, l := range levers() {
					v[l.name] = 0
				}
				if err := writeValues(c, v); err != nil {
					fatal(err)
				}
			}
		}
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			showLayout(c, syms)
		}
	case *record:
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			for i, a := range c.anchors {
				s, ok := syms.byName[a.symbol]
				if !ok {
					fatal(fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol))
				}
				c.anchors[i].target = s.addr % modOf(a.lever)
			}
			fmt.Printf("%s: recorded\n", c)
		}
		t.toolchain = goVersion()
		if err := writeTargets(*targetsPath, t); err != nil {
			fatal(err)
		}
	default:
		if err := writeSources(t); err != nil {
			fatal(err)
		}
		for _, c := range picked {
			if err := solve(c, *verbose); err != nil {
				fatal(err)
			}
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "layoutpad:", err)
	os.Exit(2)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && isModule(b) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside the go-quickjs module")
		}
		dir = parent
	}
}

// isModule says whether a go.mod is go-quickjs's, whatever its line endings:
// a checkout on Windows may have CRLF.
func isModule(gomod []byte) bool {
	for _, line := range strings.Split(string(gomod), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1] == "github.com/go-quickjs/go-quickjs"
		}
	}
	return false
}

func goVersion() string {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func modOf(name string) uint64 {
	if l, _ := leverByName(name); l.kind == 'd' {
		return dataMod
	}
	return codeMod
}

// readTargets reads the targets file: a toolchain line, then for each
// configuration a config line and its anchors.
func readTargets(path string) (*targets, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t := &targets{}
	var cur *config
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		bad := func() error { return fmt.Errorf("%s:%d: cannot read %q", path, n+1, line) }
		switch f[0] {
		case "toolchain":
			if len(f) != 2 {
				return nil, bad()
			}
			t.toolchain = f[1]
		case "config":
			if len(f) != 3 || (f[2] != "plain" && f[2] != "jit") {
				return nil, bad()
			}
			goos, goarch, ok := strings.Cut(f[1], "/")
			if !ok {
				return nil, bad()
			}
			cur = &config{goos: goos, goarch: goarch, jit: f[2] == "jit"}
			t.configs = append(t.configs, cur)
		case "anchor":
			// anchor LEVER TARGET SYMBOL..., the symbol last: it may have spaces.
			if cur == nil || len(f) < 4 {
				return nil, bad()
			}
			if _, ok := leverByName(f[1]); !ok {
				return nil, fmt.Errorf("%s:%d: no lever %s", path, n+1, f[1])
			}
			v, err := strconv.ParseUint(f[2], 0, 64)
			if err != nil {
				return nil, bad()
			}
			sym := strings.Join(f[3:], " ")
			cur.anchors = append(cur.anchors, anchor{lever: f[1], symbol: sym, target: v})
		default:
			return nil, bad()
		}
	}
	if t.toolchain == "" {
		return nil, fmt.Errorf("%s names no toolchain", path)
	}
	return t, nil
}

// writeTargets rewrites the targets file's values, keeping its comments.
func writeTargets(path string, t *targets) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out strings.Builder
	ci, ai := -1, 0
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) > 0 && f[0] == "toolchain":
			line = "toolchain " + t.toolchain
		case len(f) > 0 && f[0] == "config":
			ci, ai = ci+1, 0
		case len(f) > 0 && f[0] == "anchor":
			a := t.configs[ci].anchors[ai]
			ai++
			width := 3
			if modOf(a.lever) == codeMod {
				width = 5
			}
			line = fmt.Sprintf("anchor %-2s %#0*x %s", a.lever, width, a.target, a.symbol)
		}
		out.WriteString(line + "\n")
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

// A symbol is one line of go tool nm -size.
type symbol struct {
	addr, size uint64
	kind       string
	name       string
}

type symtab struct {
	list   []symbol // by address
	byName map[string]symbol
}

// build builds v8bench for c as its targets are measured and reads its
// symbols.
func build(c *config) (*symtab, error) {
	dir, err := os.MkdirTemp("", "layoutpad")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	exe := filepath.Join(dir, "v8bench.exe")
	args := []string{"build", "-buildvcs=false", "-o", exe}
	if c.jit {
		args = append(args, "-tags", "quickjs_jit")
	}
	args = append(args, "./internal/cmd/v8bench")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS="+c.goos, "GOARCH="+c.goarch, "CGO_ENABLED=0", "GOAMD64=v1", "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: go %s: %v\n%s", c, strings.Join(args, " "), err, out)
	}
	out, err := exec.Command("go", "tool", "nm", "-size", "-sort", "address", exe).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool nm: %w", err)
	}
	return parseNM(out), nil
}

func parseNM(out []byte) *symtab {
	t := &symtab{byName: map[string]symbol{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.SplitN(strings.TrimSpace(sc.Text()), " ", 4)
		if len(f) < 4 {
			continue
		}
		// The size is padded with spaces: split again past it.
		rest := strings.TrimSpace(strings.Join(f[1:], " "))
		g := strings.Fields(rest)
		if len(g) < 3 {
			continue
		}
		addr, err1 := strconv.ParseUint(f[0], 16, 64)
		size, err2 := strconv.ParseUint(g[0], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(rest, g[0])), g[1]))
		s := symbol{addr, size, g[1], name}
		t.list = append(t.list, s)
		if _, dup := t.byName[name]; !dup {
			t.byName[name] = s
		}
	}
	sort.SliceStable(t.list, func(i, j int) bool { return t.list[i].addr < t.list[j].addr })
	return t
}

// compare lists each anchor away from its target.
func compare(c *config, syms *symtab) []string {
	var out []string
	for _, a := range c.anchors {
		s, ok := syms.byName[a.symbol]
		if !ok {
			out = append(out, fmt.Sprintf("anchor %s (%s) is not in the build: renamed? name the anchor anew in %s", a.lever, a.symbol, targetsRel))
			continue
		}
		m := modOf(a.lever)
		if got := s.addr % m; got != a.target {
			out = append(out, fmt.Sprintf("%s at %#x mod %d, its target %#x (lever %s)", a.symbol, got, m, a.target, a.lever))
		}
	}
	return out
}

// next is the first symbol after addr in the text, other than the pads.
func (t *symtab) next(addr uint64) (symbol, bool) {
	i := sort.Search(len(t.list), func(i int) bool { return t.list[i].addr > addr })
	for ; i < len(t.list); i++ {
		s := t.list[i]
		if (s.kind == "T" || s.kind == "t") && !strings.Contains(s.name, ".layoutPad") {
			return s, true
		}
	}
	return symbol{}, false
}

func showLayout(c *config, syms *symtab) {
	fmt.Printf("%s:\n", c)
	for _, l := range levers() {
		if l.kind == 'd' {
			continue
		}
		p, ok := syms.byName[l.pad]
		if !ok {
			fmt.Printf("  %-2s (not in the build)\n", l.name)
			continue
		}
		n, _ := syms.next(p.addr)
		fmt.Printf("  %-2s pad at %#x, %5d bytes; then %s at %#x\n", l.name, p.addr, p.size, n.name, n.addr%codeMod)
	}
	for _, a := range c.anchors {
		if s, ok := syms.byName[a.symbol]; ok {
			fmt.Printf("  anchor %-2s %-60s %#x (target %#x)\n", a.lever, a.symbol, s.addr%modOf(a.lever), a.target)
		} else {
			fmt.Printf("  anchor %-2s %-60s missing\n", a.lever, a.symbol)
		}
	}
}

// The configuration's file: its levers' constants.

func configFile(c *config) string {
	jit := ""
	if c.jit {
		jit = "jit_"
	}
	return filepath.Join(vmDir, "layoutpad_"+jit+c.goos+"_"+c.goarch+".go")
}

// readValues reads a configuration's constants, -1 for a lever its file
// does not have yet.
func readValues(c *config) map[string]int {
	v := map[string]int{}
	for _, l := range levers() {
		v[l.name] = -1
		if l.kind == 'd' {
			v[l.name] = 0
		}
	}
	b, err := os.ReadFile(configFile(c))
	if err != nil {
		return v
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[1] == "=" && strings.HasPrefix(f[0], "layoutPadN") {
			if n, err := strconv.Atoi(f[2]); err == nil {
				v[strings.TrimPrefix(f[0], "layoutPadN")] = n
			}
		}
	}
	return v
}

func writeValues(c *config, v map[string]int) error {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	if c.jit {
		b.WriteString("//go:build quickjs_jit\n\n")
	} else {
		b.WriteString("//go:build !quickjs_jit\n\n")
	}
	fmt.Fprintf(&b, "package vm\n\n// The layout lock's levers for %s (layoutlock.txt): a pad's byte\n// stores, -1 for a pad left out, or a data lever's variables.\nconst (\n", c)
	for _, l := range levers() {
		fmt.Fprintf(&b, "\tlayoutPadN%s = %d\n", l.name, v[l.name])
	}
	b.WriteString(")\n")
	return writeGo(configFile(c), b.String())
}

func writeGo(path, src string) error {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, formatted) {
		return nil
	}
	return os.WriteFile(path, formatted, 0o644)
}

// writeSources writes the pads' files and, for every platform without
// targets, a file of constants that leaves every pad out. A generic pad is
// written only where some configuration has an anchor for it.
func writeSources(t *targets) error {
	used := map[string]bool{}
	for _, c := range t.configs {
		for _, a := range c.anchors {
			used[a.lever] = true
		}
	}
	for i, p := range positions {
		if err := writeGo(filepath.Join(vmDir, p.file), positionFile(p, i == len(positions)-1, used)); err != nil {
			return err
		}
	}
	platforms := map[string]bool{}
	for _, c := range t.configs {
		platforms["("+c.goos+" && "+c.goarch+")"] = true
		if _, err := os.Stat(configFile(c)); err != nil {
			if err := writeValues(c, readValues(c)); err != nil {
				return err
			}
		}
	}
	keys := make([]string, 0, len(platforms))
	for k := range platforms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "//go:build !(%s)\n\n", strings.Join(keys, " || "))
	b.WriteString("package vm\n\n// The layout lock has no targets for this platform: every pad is left\n// out.\nconst (\n")
	for _, l := range levers() {
		v := -1
		if l.kind == 'd' {
			v = 0
		}
		fmt.Fprintf(&b, "\tlayoutPadN%s = %d\n", l.name, v)
	}
	b.WriteString(")\n")
	return writeGo(filepath.Join(vmDir, "layoutpad_other.go"), b.String())
}

// stores writes a pad's body: as many byte stores as the lever's constant
// says, in groups of 1, 2, 4 ... 512 by the constant's bits, each store to
// its own byte, two apart, so that no two combine into one.
func stores(b *strings.Builder, indent, constant string) {
	for j := range storeBits {
		fmt.Fprintf(b, "%sif max(%s, 0)&(1<<%d) != 0 {\n", indent, constant, j)
		first, n := 1<<j-1, 1<<j
		for i := 0; i < n; i += storesALine {
			m := min(storesALine, n-i)
			lhs := make([]string, m)
			rhs := make([]string, m)
			for k := range m {
				lhs[k] = fmt.Sprintf("layoutPadBytes[%d]", 2*(first+i+k))
				rhs[k] = "1"
			}
			fmt.Fprintf(b, "%s\t%s = %s\n", indent, strings.Join(lhs, ", "), strings.Join(rhs, ", "))
		}
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

func positionFile(p position, last bool, used map[string]bool) string {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	if last {
		b.WriteString("//go:generate go run ../cmd/layoutpad\n\n")
	}
	b.WriteString("package vm\n\n")
	if last {
		b.WriteString("import \"unsafe\"\n\n")
	}
	g, m := used["G"+p.name], used["M"+p.name]
	fmt.Fprintf(&b, "// layoutPad%s is the layout lock's pad %s, %s.\n// It is never called: see internal/cmd/layoutpad.\n//\n//go:noinline\nfunc layoutPad%s() {\n", p.name, p.name, p.doc, p.name)
	if g {
		fmt.Fprintf(&b, "\tif layoutPadNG%s >= 0 {\n\t\tlayoutPadG%s[struct{}]()\n\t}\n", p.name, p.name)
	}
	stores(&b, "\t", "layoutPadN"+p.name)
	b.WriteString("}\n")
	if m {
		fmt.Fprintf(&b, "\n// layoutPadM%s calls the generic pad of the layout lock's methods at %s:\n// Go reads the bodies of methods after those of functions, so the generic\n// is laid out among those methods call first. It is never called.\n//\n//go:noinline\nfunc (r *Runtime) layoutPadM%s() {\n", p.name, p.name, p.name)
		fmt.Fprintf(&b, "\tlayoutPadM%s[struct{}]()\n}\n", p.name)
	}
	if g {
		fmt.Fprintf(&b, "\n// layoutPadG%s is the generic pad of layoutPad%s. It is never called.\n//\n//go:noinline\nfunc layoutPadG%s[_ any]() {\n", p.name, p.name, p.name)
		stores(&b, "\t", "layoutPadNG"+p.name)
		b.WriteString("}\n")
	}
	if m {
		fmt.Fprintf(&b, "\n// layoutPadM%s is the generic pad of (*Runtime).layoutPadM%s. It is never\n// called.\n//\n//go:noinline\nfunc layoutPadM%s[_ any]() {\n", p.name, p.name, p.name)
		stores(&b, "\t", "layoutPadNM"+p.name)
		b.WriteString("}\n")
	}
	if !last {
		return b.String()
	}
	cz := used["CZ"]
	if cz {
		b.WriteString(`
// layoutPadCZ makes the layout lock's generic closure pad, which is laid
// out first among the closures of generics. It is never called.
//
//go:noinline
func layoutPadCZ[_ any]() {
	layoutPadFunc = func() {
`)
		stores(&b, "\t\t", "layoutPadNCZ")
		b.WriteString("\t}\n}\n")
	}
	fmt.Fprintf(&b, `
// The pads' variables, never read: what the pads' stores go to, and the
// data levers, each variable 0 or its unit in size. They are the same size
// whatever the levers are but for the data levers' own.
var (
	layoutPadLive  bool
	layoutPadFunc  func()
	layoutPadBytes [%d]byte
	layoutPadPtrs  [%d]unsafe.Pointer
`, padBytes, 3*dataVars)
	for _, d := range dataLevers {
		for i := range d.n {
			fmt.Fprintf(&b, "\tlayoutPad%s%d "+d.typ+"\n", d.name, i, fmt.Sprintf("min(1, max(0, layoutPadN%s-%d))", d.name, i))
		}
	}
	b.WriteString(")\n\n")
	b.WriteString("// init keeps the pads in the binary: layoutPadLive is never true.\nfunc init() {\n\tif layoutPadLive {\n")
	for _, q := range positions {
		cond := "layoutPadN" + q.name + " >= 0"
		if used["G"+q.name] {
			cond += " || layoutPadNG" + q.name + " >= 0"
		}
		fmt.Fprintf(&b, "\t\tif %s {\n\t\t\tlayoutPad%s()\n\t\t}\n", cond, q.name)
		if used["M"+q.name] {
			fmt.Fprintf(&b, "\t\tif layoutPadNM%s >= 0 {\n\t\t\t(*Runtime)(nil).layoutPadM%s()\n\t\t}\n", q.name, q.name)
		}
	}
	if cz {
		b.WriteString("\t\tif layoutPadNCZ >= 0 {\n\t\t\tlayoutPadCZ[struct{}]()\n\t\t}\n")
	}
	k := 0
	for _, d := range dataLevers {
		for i := range d.n {
			fmt.Fprintf(&b, "\t\tlayoutPadPtrs[%d] = unsafe.Pointer(&layoutPad%s%d)\n", k, d.name, i)
			k++
		}
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// dataLevers are the data levers' variables: how many and of what type.
var dataLevers = []struct {
	name string
	n    int
	typ  string
}{{"DH", dataVars, "[%s]*byte"}, {"DW", dataVars, "[8 * %s]byte"}, {"DG", dataVars / 2, "[16 * %s]byte"}}

// solve sets c's levers so that every anchor is at its target.
func solve(c *config, verbose bool) error {
	v := readValues(c)
	active := map[string]bool{}
	for _, a := range c.anchors {
		active[a.lever] = true
	}
	for _, l := range levers() {
		switch {
		case !active[l.name] && l.kind != 'd':
			v[l.name] = -1
		case !active[l.name]:
			v[l.name] = 0
		case l.kind != 'd' && v[l.name] < 0:
			v[l.name] = 0
		}
	}
	for iter := 0; iter < 12; iter++ {
		if err := writeValues(c, v); err != nil {
			return err
		}
		syms, err := build(c)
		if err != nil {
			return err
		}
		if verbose {
			for _, line := range compare(c, syms) {
				fmt.Printf("  %s: %s\n", c, line)
			}
		}
		code, err := solveCode(c, syms, v, verbose)
		if err != nil {
			return err
		}
		if code {
			continue
		}
		data, err := solveData(c, syms, v)
		if err != nil {
			return err
		}
		if data {
			continue
		}
		fmt.Printf("%s: every anchor at its target (%s)\n", c, describe(v))
		return nil
	}
	return fmt.Errorf("%s: the levers did not settle", c)
}

func describe(v map[string]int) string {
	var parts []string
	for _, l := range levers() {
		if v[l.name] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", l.name, v[l.name]))
		}
	}
	return strings.Join(parts, " ")
}

// solveCode moves each code lever's anchor to its target, in the order the
// pads are laid out, and says whether it changed a lever.
func solveCode(c *config, syms *symtab, v map[string]int, verbose bool) (bool, error) {
	type item struct {
		l   lever
		pad symbol
		a   anchor
		at  symbol
	}
	var items []item
	for _, a := range c.anchors {
		l, _ := leverByName(a.lever)
		if l.kind == 'd' {
			continue
		}
		pad, ok := syms.byName[l.pad]
		if !ok {
			return false, fmt.Errorf("%s: pad %s is not in the build", c, l.pad)
		}
		at, ok := syms.byName[a.symbol]
		if !ok {
			return false, fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol)
		}
		if at.addr < pad.addr {
			return false, fmt.Errorf("%s: anchor %s is before its pad %s", c, a.symbol, l.pad)
		}
		items = append(items, item{l, pad, a, at})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].pad.addr < items[j].pad.addr })
	changed := false
	var shift uint64
	for _, it := range items {
		k := v[it.l.name]
		width := roundUp(it.pad.size, slot)
		delta := (it.a.target + codeMod - (it.at.addr+shift)%codeMod) % codeMod
		if delta%slot != 0 {
			return false, fmt.Errorf("%s: anchor %s at %#x cannot reach %#x in steps of %d", c, it.a.symbol, it.at.addr, it.a.target, slot)
		}
		if delta == 0 {
			continue
		}
		// go tool nm gives a function's size with the padding after it,
		// a whole number of slots: take the code itself to end mid-slot,
		// and aim the pad's code at the middle of the slot it must end in.
		// A build that shows it a slot off moves it a store at a time.
		// In bytes, all of it integers: base is the code but the stores.
		base := int64(it.pad.size) - slot/2 - perStore*int64(k)
		minWidth := roundUp(uint64(max(base, 1)), slot)
		want := minWidth + (width+delta-minWidth)%codeMod
		nk := 0
		if aim := int64(want) - slot/2 - base; aim > 0 {
			nk = int((aim + perStore/2) / perStore)
		}
		switch {
		case delta == slot && nk <= k:
			nk = k + 1
		case delta == codeMod-slot && nk >= k && k > 0:
			nk = k - 1
		}
		if nk > maxStores {
			return false, fmt.Errorf("%s: lever %s needs more than %d stores", c, it.l.name, maxStores)
		}
		if verbose {
			fmt.Printf("    %s: k %d size %d at %#x delta %d width %d want %d base %d -> %d\n", it.l.name, k, it.pad.size, it.at.addr%codeMod, delta, width, want, base, nk)
		}
		v[it.l.name] = nk
		shift += want - width
		changed = true
	}
	return changed, nil
}

func roundUp(n, m uint64) uint64 { return (n + m - 1) / m * m }

// solveData sets the first data lever whose anchor is off its target, and
// says whether it changed one. A data lever moves its anchor by the linker's
// rules for sorting and aligning data, which are cheaper to try than to
// model: the next value is tried, and solve builds again.
func solveData(c *config, syms *symtab, v map[string]int) (bool, error) {
	for _, name := range []string{"DH", "DW", "DG"} {
		var a *anchor
		for i := range c.anchors {
			if c.anchors[i].lever == name {
				a = &c.anchors[i]
			}
		}
		if a == nil {
			continue
		}
		s, ok := syms.byName[a.symbol]
		if !ok {
			return false, fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol)
		}
		if s.addr%dataMod == a.target {
			continue
		}
		l, _ := leverByName(name)
		tried := dataTried[c.String()+name]
		if tried == nil {
			tried = map[int]bool{}
			dataTried[c.String()+name] = tried
		}
		tried[v[name]] = true
		for n := 0; n <= l.max; n++ {
			if !tried[n] {
				v[name] = n
				return true, nil
			}
		}
		return false, fmt.Errorf("%s: no value of %s puts %s at %#x mod %d", c, name, a.symbol, a.target, dataMod)
	}
	return false, nil
}

var dataTried = map[string]map[int]bool{}
