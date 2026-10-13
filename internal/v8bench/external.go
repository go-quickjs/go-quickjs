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
// script's own, measured with Node's monotonic clock or Date.now for QuickJS;
// the total is the process's, from start to exit.

// preludes define, for each kind of program, the load and print the suite
// calls, from a directory the driver is given.
var preludes = map[string]string{
	// QuickJS's qjs, run with --std, which makes std a global.
	"qjs": `var __dir = %q;
globalThis.load = function (f) { std.loadScript(__dir + "/" + f); };
globalThis.read = function (f) { return std.loadFile(__dir + "/" + f); };
globalThis.__v8benchNow = function () { return Date.now(); };
`,
	// node, where a file is a module of its own: a script loaded into the
	// global context declares its names there, as load does elsewhere.
	"node": `var __dir = %q;
var __fs = require("fs"), __vm = require("vm");
var __v8benchPerformance = require("perf_hooks").performance;
globalThis.__v8benchNow = function () { return __v8benchPerformance.now(); };
globalThis.load = function (f) { __vm.runInThisContext(__fs.readFileSync(__dir + "/" + f, "utf8"), { filename: f }); };
globalThis.read = function (f) { return __fs.readFileSync(__dir + "/" + f, "utf8"); };
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
	var __t0 = __v8benchNow();
	if (BenchmarkSuite.ResetRNG) BenchmarkSuite.ResetRNG();
	for (var b of s.benchmarks) {
		b.Setup();
		for (var i = 0; i < %d; i++) b.run();
		b.TearDown();
	}
	print(__pad(s.name, 13) + " " + __pad((__v8benchNow() - __t0).toFixed(1), 9, true) + " ms");
}
`

// awfyDriver is the fixed mode of Are We Fast Yet in JavaScript: its
// modules, then each benchmark n times, those a comma-separated list names,
// or all.
const awfyDriver = `%s
%s
var __work = %s;
var __only = %q;
function __pad(s, n, left) { s = String(s); while (s.length < n) s = left ? " " + s : s + " "; return s; }
for (var w of __work) {
	if (__only && ("," + __only + ",").indexOf("," + w[0] + ",") < 0) continue;
	var __t0 = __v8benchNow();
	var b = __awfyRequire("./" + w[1]).newInstance();
	for (var i = 0; i < %d; i++) {
		if (!b.innerBenchmarkLoop(w[2])) throw new Error(w[0] + ": the result is wrong");
	}
	print(__pad(w[0], 13) + " " + __pad((__v8benchNow() - __t0).toFixed(1), 9, true) + " ms");
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
	which := flag.String("bench", "v8", "the suite: v8, octane or awfy")
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
		script = suiteDriver(suiteNamed(*which), prelude, *dir, *fetch, *mode, *only, *n, tmp)
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

// suiteDriver writes the driver of the suite, in the fixed or the score
// mode, into tmp, and returns its path: the suite is read from dir,
// downloaded into it first where fetch says so.
func suiteDriver(s *suite, prelude, dir string, fetch bool, mode, only string, n int, tmp string) string {
	if dir == "" {
		fail(fmt.Errorf("-dir is required"))
	}
	if fetch {
		if err := download(s, dir); err != nil {
			fail(err)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fail(err)
	}
	prelude = fmt.Sprintf(prelude, filepath.ToSlash(abs))
	var driver string
	switch {
	case s.awfy && mode == "fixed":
		src := map[string]string{}
		for _, f := range s.files {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				fail(fmt.Errorf("%w (run with -fetch to download the suite)", err))
			}
			src[f] = string(b)
		}
		work := make([]string, len(awfyWork))
		for i, w := range awfyWork {
			work[i] = fmt.Sprintf("[%q, %q, %d]", w.name, w.module, w.inner)
		}
		driver = fmt.Sprintf(awfyDriver, prelude, awfyModules(s, src), "["+strings.Join(work, ", ")+"]", only, n)
	case s.awfy:
		fail(fmt.Errorf("Are We Fast Yet has no scores: run it in the fixed mode"))
	case mode == "score":
		driver = prelude + `load("run.js");` + "\n"
	case mode == "fixed":
		quoted := make([]string, len(s.files))
		for i, f := range s.files {
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
