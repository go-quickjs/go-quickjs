// Command wasmbench runs WebAssembly workloads on go-quickjs's wasm engine,
// its interpreter and its JIT, and on wazero and Node for comparison, and
// reports for each workload and engine the compile and instantiate times,
// the time of a run -- the first, the best and the median -- the CPU time,
// what the Go heap allocated, and the peak resident set.
//
//	go run ./internal/cmd/wasmbench -n 3
//	go run -tags quickjs_jit ./internal/cmd/wasmbench -engine interp,jit,node -suite micro
//	go run -tags quickjs_jit ./internal/cmd/wasmbench -matrix -n 5 > matrix.md
//
// The workloads are micro-benchmarks written in the text format
// (testdata/wat) and a Go program built for GOOS=wasip1 (testdata/goprog),
// run through WASI. Each measurement is a fresh process, a worker, which
// compiles the module, instantiates it, and runs it -warmup times and then
// -reps times, checking every run's result: this program again for
// go-quickjs, the runner in internal/cmd/wasmbench/wazero -- a module of its
// own, built on first use -- for wazero, and node_worker.mjs for Node. The
// engines take turns, sample by sample.
//
// The engines (-engine) are interp and jit (go-quickjs, jit needing
// -tags quickjs_jit on amd64 Linux or Windows), wazero-interp and
// wazero-compiler, and node (its default tiering), node-liftoff (Liftoff
// alone) and node-turbofan (TurboFan alone).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
)

var (
	suiteFlag   = flag.String("suite", "", "run the workloads whose names, or suite/names, start with one of these comma-separated prefixes (micro, programs, fib, ...)")
	nFlag       = flag.Int("n", 3, "measure each workload on each engine in this many fresh processes")
	repsFlag    = flag.Int("reps", 1, "timed runs in each process, after the warmup runs")
	warmupFlag  = flag.Int("warmup", 1, "runs in each process before the timed ones; the first run is reported apart")
	engineFlag  = flag.String("engine", "", "comma-separated engines: interp, jit, wazero-interp, wazero-compiler, node, node-liftoff, node-turbofan (default interp, and jit in a JIT build; with -matrix, all)")
	jsonFlag    = flag.Bool("json", false, "print the results as JSON")
	matrixFlag  = flag.Bool("matrix", false, "print a Markdown table for each suite, the engines side by side, with ratios to Node and to wazero's compiler")
	nodeFlag    = flag.String("node", "node", "the node command")
	wazeroFlag  = flag.String("wazero", "", "a built wazero runner (internal/cmd/wasmbench/wazero); by default it is built from the checkout")
	cacheFlag   = flag.String("cache", "", "where the built modules are kept (default: go-quickjs-wasmbench in the user's cache directory)")
	timeoutFlag = flag.Duration("timeout", 10*time.Minute, "the longest a worker process may take")
	workerFlag  = flag.Bool("worker", false, "be a worker: read a request on standard input (used by wasmbench itself)")
)

// An engine is a way to run a worker.
type engine struct {
	name, title string
	// kind is "self" (this program), "wazero" or "node".
	kind      string
	nodeFlags []string
}

var engines = []engine{
	{name: "interp", title: "go-quickjs interp", kind: "self"},
	{name: "jit", title: "go-quickjs JIT", kind: "self"},
	{name: "wazero-interp", title: "wazero interp", kind: "wazero"},
	{name: "wazero-compiler", title: "wazero compiler", kind: "wazero"},
	{name: "node", title: "Node", kind: "node"},
	{name: "node-liftoff", title: "Node Liftoff", kind: "node", nodeFlags: []string{"--liftoff", "--no-wasm-tier-up"}},
	{name: "node-turbofan", title: "Node TurboFan", kind: "node", nodeFlags: []string{"--no-liftoff"}},
}

func main() {
	flag.Parse()
	if *workerFlag {
		worker.Serve(newEngine)
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wasmbench:", err)
		os.Exit(1)
	}
}

func run() error {
	if *nFlag < 1 || *repsFlag < 1 || *warmupFlag < 0 {
		return errors.New("-n and -reps must be at least 1, -warmup at least 0")
	}
	es, err := selectEngines()
	if err != nil {
		return err
	}
	var ws []workload
	for _, w := range workloads {
		if w.selected(*suiteFlag) {
			ws = append(ws, w)
		}
	}
	if len(ws) == 0 {
		return fmt.Errorf("no workload matches -suite %q", *suiteFlag)
	}

	var cells []*cell
	text := !*jsonFlag && !*matrixFlag
	if text {
		printTextHeader()
	}
	for _, w := range ws {
		bin, err := w.binary()
		if err != nil {
			return err
		}
		samples := make([][]worker.Result, len(es))
		errs := make([]error, len(es))
		for i := range *nFlag {
			for k, e := range es {
				if errs[k] != nil {
					continue
				}
				fmt.Fprintf(os.Stderr, "%s %s %d/%d\n", w.fullName(), e.name, i+1, *nFlag)
				res, err := sample(e, w.request(e.name, bin))
				if err != nil {
					errs[k] = err
					continue
				}
				samples[k] = append(samples[k], res)
				if res.Error != "" {
					errs[k] = fmt.Errorf("%s: %s", res.Stage, res.Error)
				}
			}
		}
		for k, e := range es {
			c := summarize(w, e, samples[k], errs[k])
			cells = append(cells, c)
			if text {
				printTextRow(c)
			}
		}
	}
	switch {
	case *jsonFlag:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cells)
	case *matrixFlag:
		printMatrix(os.Stdout, es, ws, cells)
	}
	return nil
}

// selectEngines is -engine's engines, in the order of engines.
func selectEngines() ([]engine, error) {
	names := *engineFlag
	if names == "" {
		switch {
		case *matrixFlag:
			names = "interp,jit,wazero-interp,wazero-compiler,node,node-liftoff,node-turbofan"
		case jitBuilt:
			names = "interp,jit"
		default:
			names = "interp"
		}
	}
	want := map[string]bool{}
	for _, n := range strings.Split(names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var es []engine
	for _, e := range engines {
		if want[e.name] {
			es = append(es, e)
			delete(want, e.name)
		}
	}
	for n := range want {
		return nil, fmt.Errorf("no engine %q", n)
	}
	for _, e := range es {
		if e.name == "jit" && !jitBuilt {
			return nil, errors.New("the jit engine needs a JIT build: go run -tags quickjs_jit ./internal/cmd/wasmbench (amd64 Linux or Windows)")
		}
	}
	return es, nil
}

// sample runs one worker process for e.
func sample(e engine, req worker.Request) (worker.Result, error) {
	var res worker.Result
	cmd, err := command(e)
	if err != nil {
		return res, err
	}
	in, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	out := bytes.TrimSpace(stdout.Bytes())
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return res, fmt.Errorf("%s worker: %v\n%s%s", e.name, runErr, stdout.Bytes(), stderr.Bytes())
	}
	return res, nil
}

// command is the worker command for e, without its context.
func command(e engine) (*exec.Cmd, error) {
	switch e.kind {
	case "self":
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return exec.Command(exe, "-worker"), nil
	case "wazero":
		bin, err := wazeroRunner()
		if err != nil {
			return nil, err
		}
		return exec.Command(bin), nil
	case "node":
		script, err := nodeWorkerScript()
		if err != nil {
			return nil, err
		}
		node, err := exec.LookPath(*nodeFlag)
		if err != nil {
			return nil, err
		}
		args := append(append([]string{}, e.nodeFlags...), "--no-warnings", script)
		return exec.Command(node, args...), nil
	}
	return nil, fmt.Errorf("unknown engine kind %q", e.kind)
}

// wazeroRunner is -wazero, or the wazero runner built into the cache
// directory from internal/cmd/wasmbench/wazero of the checkout the current
// directory is in.
var wazeroRunner = sync.OnceValues(func() (string, error) {
	if *wazeroFlag != "" {
		return *wazeroFlag, nil
	}
	goCmd := goCommand()
	gomod, err := exec.Command(goCmd, "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	dir := filepath.Join(root, "internal", "cmd", "wasmbench", "wazero")
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("no wazero runner at %s: run wasmbench from the go-quickjs checkout, or give -wazero: %v", dir, err)
	}
	cache, err := cacheDir()
	if err != nil {
		return "", err
	}
	out := filepath.Join(cache, "wasmbench-wazero")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command(goCmd, "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if msg, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the wazero runner: %v\n%s", err, msg)
	}
	return out, nil
})

// nodeWorkerScript is node_worker.mjs, written to the cache directory.
var nodeWorkerScript = sync.OnceValues(func() (string, error) {
	return cacheFile(fmt.Sprintf("node_worker-%x.mjs", hashOf(nodeWorker)), nodeWorker)
})
