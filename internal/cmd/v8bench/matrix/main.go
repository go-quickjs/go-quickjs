// Command matrix runs the benchmark suites on several engines and prints
// the tables that compare them: go-quickjs, without the JIT and with it, C
// QuickJS, Node, with its JIT and without, and goja -- the comparisons
// docs/benchmarks.md makes, on whatever machine it runs on.
//
//	go run ./internal/cmd/v8bench/matrix -qjs /path/to/qjs \
//	    -v8 /tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -fetch
//
// It builds each engine's runner from the checkout -- internal/cmd/v8bench
// for go-quickjs, with and without the quickjs_jit tag, its external
// runner for QuickJS and Node, and the goja module's -- and runs every
// suite (-suites) on every engine (-engines) that runs it, -runs times,
// each run a fresh process: the V8 suite's fixed work -n 3 times in a run,
// Octane's and Are We Fast Yet's once (-n), QuickJS's micro-benchmarks,
// and the V8 suite's own scores. A figure is the median of the runs; a
// micro-benchmark's, the fastest. The tables follow, with each go-quickjs
// column's time over QuickJS's where QuickJS ran, and the micro-benchmarks'
// largest gaps either way.
//
// Engines it cannot find are left out: QuickJS where -qjs gives none and
// none is on the PATH, Node where none is. goja runs the V8 suite and its
// scores (-goja-suites says otherwise): it takes several times go-quickjs's
// time, too long for Octane's.
//
// With -parallel, which is the default where the OS gives a process CPUs
// of its own (Windows, Linux), two runs go at once, each on a group of
// CPUs found as placements finds its own (-cpus; see package cpuset), the
// groups swapped from one run to the next, each run with GOMAXPROCS as
// many as its group's: half the time, at figures within a percent of each
// other on the groups of a Ryzen's core complexes. -parallel=false runs
// one at a time on every CPU, as docs/benchmarks.md's figures were made.
//
// Every run's output is kept in -out, with what the machine and the
// engines are (env.txt), and the tables (tables.md); -report DIR prints
// the tables of a directory again, without running anything.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/cmd/v8bench/internal/cpuset"
)

// An engine is a runner and what it runs.
type engine struct {
	name, title string
	// runner is the built program's name in the build directory, and args
	// what each run of it is given first.
	runner string
	args   []string
	// suites are the suites it runs.
	suites []string
}

var allSuites = []string{"v8", "octane", "awfy", "micro", "score"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "matrix:", err)
		os.Exit(1)
	}
}

func run() error {
	engines := flag.String("engines", "gq,gqjit,qjs,node-jitless,node,goja", "the engines, of gq, gqjit, qjs, node-jitless, node and goja")
	suites := flag.String("suites", strings.Join(allSuites, ","), "the suites: v8, octane, awfy, micro, score")
	gojaSuites := flag.String("goja-suites", "v8,score", "the suites goja runs")
	runs := flag.Int("runs", 3, "fresh-process runs of each suite on each engine")
	nV8 := flag.Int("n-v8", 3, "the V8 suite's runs of its fixed work in a run")
	nLong := flag.Int("n", 1, "Octane's and Are We Fast Yet's runs of their fixed work in a run")
	v8Dir := flag.String("v8", filepath.Join(os.TempDir(), "v8-v7"), "the V8 suite's directory")
	octaneDir := flag.String("octane", filepath.Join(os.TempDir(), "octane"), "Octane's directory")
	awfyDir := flag.String("awfy", filepath.Join(os.TempDir(), "awfy"), "Are We Fast Yet's directory")
	fetch := flag.Bool("fetch", false, "download the suites that are missing first")
	qjs := flag.String("qjs", "", "C QuickJS's qjs, by default the PATH's")
	node := flag.String("node", "", "node, by default the PATH's")
	parallel := flag.Bool("parallel", cpuset.CanPin, "run two at once, each on a group of CPUs of its own (-cpus)")
	cpus := flag.String("cpus", "auto", "the groups of CPUs -parallel runs on: auto, halves, or lists separated by ';'")
	out := flag.String("out", "", "where the runs' outputs go, by default a new directory in the temporary one")
	report := flag.String("report", "", "print the tables of a directory a run kept, and run nothing")
	microRows := flag.Int("micro-rows", 12, "the micro-benchmarks' largest gaps each way the table shows")
	flag.Parse()

	if *report != "" {
		tables, err := render(*report, *microRows)
		if err != nil {
			return err
		}
		fmt.Print(tables)
		return nil
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(os.TempDir(), "v8bench-matrix-"+time.Now().Format("20060102-150405"))
	}
	bin := filepath.Join(*out, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	dirs := map[string]string{"v8": *v8Dir, "octane": *octaneDir, "awfy": *awfyDir, "score": *v8Dir}
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return name + ".exe"
		}
		return name
	}

	// The engines asked for that are here.
	if *qjs == "" {
		*qjs, _ = exec.LookPath("qjs")
	}
	if *node == "" {
		*node, _ = exec.LookPath("node")
	}
	known := map[string]engine{
		"gq":           {name: "gq", title: "go-quickjs", runner: exe("gq"), suites: allSuites},
		"gqjit":        {name: "gqjit", title: "go-quickjs JIT", runner: exe("gqjit"), args: []string{"-jit"}, suites: allSuites},
		"qjs":          {name: "qjs", title: "QuickJS", runner: exe("external"), args: []string{"-engine", "qjs", "-cmd", *qjs}, suites: allSuites},
		"node-jitless": {name: "node-jitless", title: "Node --jitless", runner: exe("external"), args: []string{"-engine", "node", "-cmd", *node, "-arg", "--jitless"}, suites: allSuites},
		"node":         {name: "node", title: "Node", runner: exe("external"), args: []string{"-engine", "node", "-cmd", *node}, suites: allSuites},
		"goja":         {name: "goja", title: "goja", runner: exe("goja"), suites: strings.Split(*gojaSuites, ",")},
	}
	var chosen []engine
	for _, name := range strings.Split(*engines, ",") {
		e, ok := known[strings.TrimSpace(name)]
		switch {
		case !ok:
			return fmt.Errorf("-engines: no engine %q", name)
		case e.name == "qjs" && *qjs == "":
			fmt.Println("left out: QuickJS, which -qjs names none of and the PATH has none of")
			continue
		case strings.HasPrefix(e.name, "node") && *node == "":
			fmt.Println("left out:", e.title+", which the PATH has none of")
			continue
		}
		chosen = append(chosen, e)
	}
	wanted := strings.Split(*suites, ",")
	for _, s := range wanted {
		if !slices.Contains(allSuites, s) {
			return fmt.Errorf("-suites: no suite %q", s)
		}
	}

	// The runners, built from the checkout.
	builds := map[string][]string{
		exe("gq"):       {"go", "build", "-o", filepath.Join(bin, exe("gq")), "./internal/cmd/v8bench"},
		exe("gqjit"):    {"go", "build", "-tags", "quickjs_jit", "-o", filepath.Join(bin, exe("gqjit")), "./internal/cmd/v8bench"},
		exe("external"): {"go", "build", "-o", filepath.Join(bin, exe("external")), "./internal/cmd/v8bench/external"},
		exe("goja"):     {"go", "build", "-o", filepath.Join(bin, exe("goja")), "."},
	}
	built := map[string]bool{}
	var usable []engine
	for _, e := range chosen {
		if !built[e.runner] {
			b := builds[e.runner]
			cmd := exec.Command(b[0], b[1:]...)
			cmd.Dir = root
			if e.name == "goja" {
				cmd.Dir = filepath.Join(root, "internal", "cmd", "v8bench", "goja")
			}
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			fmt.Println("building", e.runner)
			if err := cmd.Run(); err != nil {
				fmt.Printf("left out: %s, whose runner did not build: %v\n", e.title, err)
				continue
			}
			built[e.runner] = true
		}
		usable = append(usable, e)
	}
	if *fetch {
		for _, s := range []string{"v8", "octane", "awfy"} {
			if !slices.Contains(wanted, s) && !(s == "v8" && slices.Contains(wanted, "score")) {
				continue
			}
			if _, err := os.Stat(dirs[s]); err == nil {
				continue
			}
			args := []string{"-dir", dirs[s], "-fetch", "-mode", "compile", "-n", "1"}
			if s != "v8" {
				args = append(args, "-bench", s)
			}
			fmt.Println("fetching", s, "into", dirs[s])
			cmd := exec.Command(filepath.Join(bin, exe("gq")), args...)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("fetching %s: %w", s, err)
			}
		}
	}

	var groups []cpuset.Set
	how := "one run at a time, on every CPU"
	if *parallel {
		g, found, err := cpuset.Parse(*cpus)
		if err != nil {
			return err
		}
		if len(g) < 2 || len(g[0]) == 0 || len(g[1]) == 0 {
			return fmt.Errorf("-parallel needs two groups of CPUs, -cpus %q gives %v", *cpus, g)
		}
		groups = g[:2]
		how = fmt.Sprintf("two runs at once, on CPUs %s and %s (%s)", groups[0], groups[1], found)
		if !cpuset.CanPin {
			how += ", unpinned: this OS gives a process no CPUs of its own"
		}
	}
	if err := writeEnv(*out, root, usable, how, *qjs, *node); err != nil {
		return err
	}
	fmt.Println(how)

	// The runs: of each suite, each engine's, two at a time where -parallel.
	start := time.Now()
	for _, s := range wanted {
		var es []engine
		for _, e := range usable {
			if slices.Contains(e.suites, s) {
				es = append(es, e)
			}
		}
		for rep := range *runs {
			step := 1
			if groups != nil {
				step = 2
			}
			for i := 0; i < len(es); i += step {
				batch := es[i:min(i+step, len(es))]
				errs := make([]error, len(batch))
				done := make(chan struct{}, len(batch))
				t := time.Now()
				for j, e := range batch {
					var g cpuset.Set
					if groups != nil {
						g = groups[(j+rep)%2]
					}
					args := runArgs(e, s, dirs[s], *nV8, *nLong)
					file := filepath.Join(*out, fmt.Sprintf("%s-%s-%d.txt", e.name, s, rep))
					go func() {
						errs[j] = runOne(filepath.Join(bin, e.runner), args, file, g)
						done <- struct{}{}
					}()
				}
				for range batch {
					<-done
				}
				var names []string
				for j, e := range batch {
					names = append(names, e.name)
					if errs[j] != nil {
						fmt.Printf("  %s %s run %d: %v\n", e.name, s, rep, errs[j])
					}
				}
				fmt.Printf("%s run %d: %s, %.0f s\n", s, rep+1, strings.Join(names, " and "), time.Since(t).Seconds())
			}
		}
	}
	fmt.Printf("all runs: %.0f s\n\n", time.Since(start).Seconds())
	tables, err := render(*out, *microRows)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, "tables.md"), []byte(tables), 0o644); err != nil {
		return err
	}
	fmt.Print(tables)
	fmt.Println("\nthe runs' outputs and the tables are in", *out)
	return nil
}

// runArgs is what a run of suite s on e is given.
func runArgs(e engine, s, dir string, nV8, nLong int) []string {
	args := slices.Clone(e.args)
	switch s {
	case "micro":
		return append(args, "-mode", "micro", "-n", "1")
	case "score":
		return append(args, "-dir", dir, "-mode", "score")
	case "v8":
		return append(args, "-dir", dir, "-mode", "fixed", "-n", strconv.Itoa(nV8))
	}
	return append(args, "-dir", dir, "-bench", s, "-mode", "fixed", "-n", strconv.Itoa(nLong))
}

// runOne runs a runner once, on g if any, its output to file, what it
// wrote to its standard error, if anything, beside it.
func runOne(path string, args []string, file string, g cpuset.Set) error {
	cmd := exec.Command(path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	var err error
	if g != nil {
		cmd.Env = append(os.Environ(), "GOMAXPROCS="+strconv.Itoa(len(g)))
		err = cpuset.Start(cmd, g)
	} else {
		err = cmd.Start()
	}
	if err == nil {
		err = cmd.Wait()
	}
	werr := os.WriteFile(file, stdout.Bytes(), 0o644)
	if stderr.Len() > 0 {
		os.WriteFile(file+".err", stderr.Bytes(), 0o644)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return err
	}
	return werr
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// repoRoot is the checkout's root, where go.mod is.
func repoRoot() (string, error) {
	b, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(b))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("run from the go-quickjs checkout")
	}
	return filepath.Dir(mod), nil
}

// writeEnv keeps what the runs ran on: the machine, the checkout, the
// engines and how the runs were made.
func writeEnv(out, root string, engines []engine, how, qjs, node string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "date: %s\n", time.Now().Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "machine: %s, %s/%s, %d CPUs\n", cpuName(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if v, err := exec.Command("go", "version").Output(); err == nil {
		fmt.Fprintf(&b, "go: %s\n", strings.TrimSpace(string(v)))
	}
	commit := "unknown"
	if v, err := gitOutput(root, "rev-parse", "--short", "HEAD"); err == nil {
		commit = v
		if d, err := gitOutput(root, "status", "--porcelain", "--untracked-files=no"); err == nil && d != "" {
			commit += " with changes"
		}
	}
	fmt.Fprintf(&b, "go-quickjs: %s\n", commit)
	for _, e := range engines {
		switch e.name {
		case "qjs":
			fmt.Fprintf(&b, "QuickJS: %s\n", qjs)
		case "node":
			if v, err := exec.Command(node, "--version").Output(); err == nil {
				fmt.Fprintf(&b, "Node: %s (%s)\n", strings.TrimSpace(string(v)), node)
			}
		}
	}
	fmt.Fprintf(&b, "runs: %s\n", how)
	return os.WriteFile(filepath.Join(out, "env.txt"), []byte(b.String()), 0o644)
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

// cpuName is the CPU's model, as the OS names it, or "".
func cpuName() string {
	switch runtime.GOOS {
	case "windows":
		if v, err := exec.Command("powershell", "-NoProfile", "-Command", "(Get-CimInstance Win32_Processor).Name").Output(); err == nil {
			return strings.TrimSpace(string(v))
		}
		return os.Getenv("PROCESSOR_IDENTIFIER")
	case "darwin":
		if v, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(v))
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return "unknown CPU"
}

var (
	fixedLine = regexp.MustCompile(`^(\w+)\s+([\d.]+) ms`)
	scoreLine = regexp.MustCompile(`^(\w+): ([\d.]+)$`)
	totalLine = regexp.MustCompile(`^Score \(version \d+\): ([\d.]+)$`)
)

// figures reads a run's output: each benchmark's milliseconds, a
// micro-benchmark's nanoseconds, or each benchmark's score and the
// composite's, as "TOTAL", in the order it printed them.
func figures(text, suite string) (names []string, vals map[string]float64) {
	vals = map[string]float64{}
	add := func(n string, v float64) {
		if _, ok := vals[n]; !ok {
			names = append(names, n)
		}
		vals[n] = v
	}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		switch suite {
		case "micro":
			f := strings.Fields(l)
			if len(f) == 3 && f[0] != "TEST" {
				if _, err := strconv.Atoi(f[1]); err == nil {
					if v, err := strconv.ParseFloat(f[2], 64); err == nil {
						add(f[0], v)
					}
				}
			}
		case "score":
			if m := totalLine.FindStringSubmatch(l); m != nil {
				v, _ := strconv.ParseFloat(m[1], 64)
				add("TOTAL", v)
			} else if m := scoreLine.FindStringSubmatch(l); m != nil {
				v, _ := strconv.ParseFloat(m[2], 64)
				add(m[1], v)
			}
		default:
			if m := fixedLine.FindStringSubmatch(l); m != nil {
				v, _ := strconv.ParseFloat(m[2], 64)
				add(m[1], v)
			}
		}
	}
	return names, vals
}

// render is the tables of the runs kept in dir.
func render(dir string, microRows int) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*-*-*.txt"))
	if err != nil {
		return "", err
	}
	run := regexp.MustCompile(`^(.+)-(v8|octane|awfy|micro|score)-(\d+)\.txt$`)
	// res[suite][engine] is each run's figures; order the engines' and the
	// benchmarks' order.
	res := map[string]map[string][]map[string]float64{}
	order := map[string][]string{}
	var enginesSeen []string
	for _, f := range files {
		m := run.FindStringSubmatch(filepath.Base(f))
		if m == nil {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		e, s := m[1], m[2]
		names, vals := figures(string(b), s)
		if len(vals) == 0 {
			continue
		}
		if res[s] == nil {
			res[s] = map[string][]map[string]float64{}
		}
		res[s][e] = append(res[s][e], vals)
		for _, n := range names {
			if !slices.Contains(order[s], n) {
				order[s] = append(order[s], n)
			}
		}
		if !slices.Contains(enginesSeen, e) {
			enginesSeen = append(enginesSeen, e)
		}
	}
	if len(res) == 0 {
		return "", fmt.Errorf("%s: no runs' outputs", dir)
	}
	titles := map[string]string{"gq": "go-quickjs", "gqjit": "go-quickjs JIT", "qjs": "QuickJS",
		"node-jitless": "Node --jitless", "node": "Node", "goja": "goja"}
	engineOrder := []string{"gq", "gqjit", "qjs", "node-jitless", "node", "goja"}
	sort.SliceStable(enginesSeen, func(i, j int) bool {
		return slices.Index(engineOrder, enginesSeen[i]) < slices.Index(engineOrder, enginesSeen[j])
	})

	var b strings.Builder
	if env, err := os.ReadFile(filepath.Join(dir, "env.txt")); err == nil {
		b.WriteString("```\n" + strings.TrimSpace(string(env)) + "\n```\n")
	}
	median := func(runs []map[string]float64, n string) (float64, bool) {
		var xs []float64
		for _, r := range runs {
			if v, ok := r[n]; ok {
				xs = append(xs, v)
			}
		}
		if len(xs) == 0 {
			return 0, false
		}
		slices.Sort(xs)
		if len(xs)%2 == 1 {
			return xs[len(xs)/2], true
		}
		return (xs[len(xs)/2-1] + xs[len(xs)/2]) / 2, true
	}
	suiteTitles := map[string]string{"v8": "V8 suite, version 7", "octane": "Octane 2.0", "awfy": "Are We Fast Yet"}
	for _, s := range []string{"v8", "octane", "awfy"} {
		if res[s] == nil {
			continue
		}
		var cols []string
		for _, e := range enginesSeen {
			if res[s][e] != nil {
				cols = append(cols, e)
			}
		}
		fmt.Fprintf(&b, "\n### %s: milliseconds for the fixed work, the median of the runs (lower is better)\n\n", suiteTitles[s])
		b.WriteString("| Benchmark |")
		for _, e := range cols {
			fmt.Fprintf(&b, " %s |", titles[e])
		}
		vsQJS := res[s]["qjs"] != nil
		var ratios []string
		if vsQJS {
			for _, e := range []string{"gq", "gqjit"} {
				if res[s][e] != nil {
					ratios = append(ratios, e)
					fmt.Fprintf(&b, " %s / QuickJS |", titles[e])
				}
			}
		}
		b.WriteString("\n|---|" + strings.Repeat("---:|", len(cols)+len(ratios)) + "\n")
		for _, n := range order[s] {
			bold := ""
			if n == "TOTAL" {
				bold = "**"
			}
			fmt.Fprintf(&b, "| %s%s%s |", bold, n, bold)
			for _, e := range cols {
				if v, ok := median(res[s][e], n); ok {
					fmt.Fprintf(&b, " %s%s%s |", bold, ms(v), bold)
				} else {
					b.WriteString(" — |")
				}
			}
			q, qok := median(res[s]["qjs"], n)
			for _, e := range ratios {
				if v, ok := median(res[s][e], n); ok && qok && q > 0 {
					fmt.Fprintf(&b, " %s%.2fx%s |", bold, v/q, bold)
				} else {
					b.WriteString(" — |")
				}
			}
			b.WriteString("\n")
		}
	}
	if sc := res["score"]; sc != nil {
		var cols []string
		for _, e := range enginesSeen {
			if sc[e] != nil {
				cols = append(cols, e)
			}
		}
		b.WriteString("\n### V8 suite, version 7: the suite's own scores, the median of the runs (higher is better)\n\n| Benchmark |")
		for _, e := range cols {
			fmt.Fprintf(&b, " %s |", titles[e])
		}
		b.WriteString("\n|---|" + strings.Repeat("---:|", len(cols)) + "\n")
		for _, n := range order["score"] {
			label := n
			if n == "TOTAL" {
				label = "**Composite**"
			}
			fmt.Fprintf(&b, "| %s |", label)
			for _, e := range cols {
				if v, ok := median(sc[e], n); ok {
					fmt.Fprintf(&b, " %s |", grouped(v))
				} else {
					b.WriteString(" — |")
				}
			}
			b.WriteString("\n")
		}
	}
	if mi := res["micro"]; mi != nil && mi["gq"] != nil && mi["qjs"] != nil {
		best := func(runs []map[string]float64, n string) (float64, bool) {
			v, ok := math.Inf(1), false
			for _, r := range runs {
				if x, have := r[n]; have && x < v {
					v, ok = x, true
				}
			}
			return v, ok
		}
		type gap struct {
			name     string
			g, q, by float64
		}
		var gaps []gap
		for _, n := range order["micro"] {
			g, gok := best(mi["gq"], n)
			q, qok := best(mi["qjs"], n)
			// sort_bench reports a run's time over its fastest sort, not
			// an operation's.
			if gok && qok && q > 0 && n != "sort_bench" {
				gaps = append(gaps, gap{n, g, q, g / q})
			}
		}
		sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].by > gaps[j].by })
		logSum, slower, faster := 0.0, 0, 0
		for _, x := range gaps {
			logSum += math.Log(x.by)
			if x.by > 1.05 {
				slower++
			} else if x.by < 1/1.05 {
				faster++
			}
		}
		fmt.Fprintf(&b, "\n### Micro-benchmarks: nanoseconds an operation, the fastest of the runs\n\n"+
			"Over %d tests go-quickjs takes %.2f times QuickJS's time by the geometric mean: slower on %d, faster on %d, within 5%% on the rest. The largest gaps either way:\n\n",
			len(gaps), math.Exp(logSum/float64(len(gaps))), slower, faster)
		b.WriteString("| Test | go-quickjs | QuickJS | go-quickjs / QuickJS |\n|---|---:|---:|---:|\n")
		shown := gaps
		if len(gaps) > 2*microRows {
			shown = append(slices.Clone(gaps[:microRows]), gap{name: "…"})
			shown = append(shown, gaps[len(gaps)-microRows:]...)
		}
		for _, x := range shown {
			if x.name == "…" {
				b.WriteString("| … | | | |\n")
				continue
			}
			fmt.Fprintf(&b, "| `%s` | %s ns | %s ns | %.2fx |\n", x.name, ns(x.g), ns(x.q), x.by)
		}
	}
	return b.String(), nil
}

// ms is a time in milliseconds, as the tables give them: a tenth below 100.
func ms(v float64) string {
	if v < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return grouped(v)
}

// ns is a time in nanoseconds: three figures.
func ns(v float64) string {
	switch {
	case v < 10:
		return strconv.FormatFloat(v, 'f', 2, 64)
	case v < 100:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return grouped(v)
}

// grouped is v rounded, its thousands separated by commas.
func grouped(v float64) string {
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
