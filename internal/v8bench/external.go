package v8bench

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// An engine that is a program of its own -- QuickJS's qjs, node -- runs the
// suite from a driver script, which loads the suite and does in JavaScript
// what the fixed and score modes do in Go, and prints what they print. What
// it cannot report is what Go counts: the allocations. The time is the
// script's own, measured by Date.now around each suite, and the total is the
// process's, from start to exit.

// preludes define, for each kind of program, the load and print the suite
// calls, from a directory the driver is given.
var preludes = map[string]string{
	// QuickJS's qjs, run with --std, which makes std a global.
	"qjs": `var __dir = %q;
globalThis.load = function (f) { std.loadScript(__dir + "/" + f); };
`,
	// node, where a file is a module of its own: a script loaded into the
	// global context declares its names there, as load does elsewhere.
	"node": `var __dir = %q;
var __fs = require("fs"), __vm = require("vm");
globalThis.load = function (f) { __vm.runInThisContext(__fs.readFileSync(__dir + "/" + f, "utf8"), { filename: f }); };
globalThis.print = function (s) { console.log(s); };
`,
}

// args are the arguments each kind of program takes before the driver.
var args = map[string][]string{"qjs": {"--std"}, "node": nil}

// argList is a flag given once for each argument it adds, so that an
// argument needs no quoting and none is split: -arg --jitless -arg
// --max-old-space-size=4096.
type argList []string

func (a *argList) String() string { return strings.Join(*a, " ") }

func (a *argList) Set(s string) error {
	*a = append(*a, s)
	return nil
}

// fixedDriver is the fixed mode in JavaScript: each benchmark n times, the
// suites a comma-separated list names, or all.
const fixedDriver = `%s
var __files = %s;
for (var i = 0; i < __files.length; i++) load(__files[i]);
var __only = %q;
function __pad(s, n, left) { s = String(s); while (s.length < n) s = left ? " " + s : s + " "; return s; }
for (var s of BenchmarkSuite.suites) {
	if (__only && ("," + __only + ",").indexOf("," + s.name + ",") < 0) continue;
	var __t0 = Date.now();
	for (var b of s.benchmarks) {
		b.Setup();
		for (var i = 0; i < %d; i++) b.run();
		b.TearDown();
	}
	print(__pad(s.name, 13) + " " + __pad((Date.now() - __t0).toFixed(1), 9, true) + " ms");
}
`

// MainExternal parses the command line and runs the suite on a program of
// its own, of a kind preludes knows, in the fixed or the score mode, or
// QuickJS's micro-benchmarks in the micro mode, from the same driver the Go
// runners run (see micro.go), in a directory of its own.
func MainExternal() {
	kind := flag.String("engine", "qjs", "the kind of program: qjs or node")
	command := flag.String("cmd", "", "the program, qjs or node by default")
	dir := flag.String("dir", "", "the directory the suite is in")
	fetch := flag.Bool("fetch", false, "download the suite into -dir first")
	mode := flag.String("mode", "fixed", "score, fixed or micro")
	n := flag.Int("n", 5, "runs of each benchmark (fixed, micro)")
	only := flag.String("suite", "", "comma-separated suites to run, all by default (fixed); tests by the start of their names (micro)")
	var extra argList
	flag.Var(&extra, "arg", "an argument for the program, before the driver script; given once for each, as -arg --jitless")
	flag.Parse()
	prelude, ok := preludes[*kind]
	if !ok {
		fail(fmt.Errorf("unknown engine %q", *kind))
	}
	tmp, err := os.MkdirTemp("", "v8bench")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)
	var script string
	if *mode == "micro" {
		driver, err := microDriver(*n, *only)
		if err != nil {
			fail(err)
		}
		script = filepath.Join(tmp, "microbench.js")
		if err := os.WriteFile(script, []byte(driver), 0o644); err != nil {
			fail(err)
		}
	} else {
		script = suiteDriver(prelude, *dir, *fetch, *mode, *only, *n, tmp)
	}

	program := *command
	if program == "" {
		program = *kind
	}
	// The header names the arguments too: node --jitless is not node.
	name := filepath.Base(program)
	if len(extra) > 0 {
		name += " " + extra.String()
	}
	fmt.Printf("%s, %s\n", name, *mode)
	argv := append(append(append([]string(nil), args[*kind]...), extra...), script)
	cmd := exec.Command(program, argv...)
	cmd.Dir = tmp
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		fail(err)
	}
	fmt.Printf("%-13s %9.1f ms\n", "TOTAL", float64(time.Since(start).Microseconds())/1000)
}

// suiteDriver writes the driver of the V8 suite, in the fixed or the score
// mode, into tmp, and returns its path: the suite is read from dir,
// downloaded into it first where fetch says so.
func suiteDriver(prelude, dir string, fetch bool, mode, only string, n int, tmp string) string {
	if dir == "" {
		fail(fmt.Errorf("-dir is required"))
	}
	if fetch {
		if err := download(dir); err != nil {
			fail(err)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fail(err)
	}
	prelude = fmt.Sprintf(prelude, filepath.ToSlash(abs))
	var driver string
	switch mode {
	case "score":
		driver = prelude + `load("run.js");` + "\n"
	case "fixed":
		quoted := make([]string, len(files))
		for i, f := range files {
			quoted[i] = strconv.Quote(f)
		}
		driver = fmt.Sprintf(fixedDriver, prelude, "["+strings.Join(quoted, ", ")+"]", only, n)
	default:
		fail(fmt.Errorf("mode %q is not one an external engine runs", mode))
	}
	script := filepath.Join(tmp, "driver.js")
	if err := os.WriteFile(script, []byte(driver), 0o644); err != nil {
		fail(err)
	}
	return script
}
