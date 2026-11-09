// Package worker is the measuring side of wasmbench: a process that is given
// one workload on its standard input, compiles it, instantiates it, runs it a
// number of times, checks each run's result, and writes what it measured to
// its standard output as JSON. Each Go runner -- go-quickjs's and wazero's --
// links it with an Engine of its own, so that every engine is timed by the
// same code.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
)

// Request is one measurement: a workload and how often to run it.
type Request struct {
	// Engine names the configuration the runner uses: "interp", "jit",
	// "wazero-interp" or "wazero-compiler".
	Engine string `json:"engine"`
	// Wasm is the path of the module's binary.
	Wasm string `json:"wasm"`
	// Kind is "call", a module whose export Export is called with Arg and
	// returns an i64, or "wasi", a WASI preview 1 program run by
	// instantiating it with Args.
	Kind   string   `json:"kind"`
	Export string   `json:"export,omitempty"`
	Arg    uint32   `json:"arg,omitempty"`
	Args   []string `json:"args,omitempty"`
	// Want is the result every run must give: the call's i64 as a signed
	// decimal, or what the program writes to its standard output.
	Want string `json:"want"`
	// Warmup runs come first and are not timed as runs; Reps follow.
	Warmup int `json:"warmup"`
	Reps   int `json:"reps"`
	// CPUProfile, if set, is the file the timed runs' CPU profile is
	// written to.
	CPUProfile string `json:"cpu_profile,omitempty"`
}

// Result is what a worker measured. Times are in nanoseconds.
type Result struct {
	Compile     int64 `json:"compile_ns"`
	Instantiate int64 `json:"instantiate_ns,omitempty"`
	// Runs and RunsCPU are each run's wall and process CPU time, the
	// warmup runs first.
	Runs    []int64 `json:"runs_ns"`
	RunsCPU []int64 `json:"runs_cpu_ns"`
	// AllocBytes and Allocs are what the Go heap allocated per timed run,
	// for the Go runners.
	AllocBytes uint64 `json:"alloc_bytes,omitempty"`
	Allocs     uint64 `json:"allocs,omitempty"`
	// PeakRSS is the process's peak resident set, in bytes, where the
	// platform says.
	PeakRSS uint64 `json:"peak_rss,omitempty"`
	// Stage and Error say what failed: "compile", "instantiate", "run" or
	// "check".
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
}

// Engine compiles modules for the worker.
type Engine interface {
	Compile(ctx context.Context, bin []byte) (Module, error)
	Close(ctx context.Context) error
}

// Module is a compiled module.
type Module interface {
	// Instantiate instantiates the module without WASI, for a call.
	Instantiate(ctx context.Context) (Instance, error)
	// RunWASI instantiates the module with WASI preview 1, the program's
	// arguments args and its output going to stdout and stderr, which runs
	// it, and closes the instance. A program that exits with code zero is
	// not an error.
	RunWASI(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

// Instance is an instantiated module.
type Instance interface {
	// Call calls the export name with arg and returns its i64 result.
	Call(ctx context.Context, name string, arg uint32) (uint64, error)
	Close(ctx context.Context) error
}

// Serve reads a Request from standard input, has newEngine make the engine
// it names, measures, writes the Result to standard output, and exits, with
// status 1 if anything failed.
func Serve(newEngine func(name string) (Engine, error)) {
	var req Request
	b, err := io.ReadAll(os.Stdin)
	if err == nil {
		err = json.Unmarshal(b, &req)
	}
	var res Result
	if err != nil {
		res = Result{Stage: "request", Error: err.Error()}
	} else if e, err := newEngine(req.Engine); err != nil {
		res = Result{Stage: "engine", Error: err.Error()}
	} else {
		res = Measure(e, req)
	}
	out, _ := json.Marshal(res)
	os.Stdout.Write(append(out, '\n'))
	if res.Error != "" {
		os.Exit(1)
	}
	os.Exit(0)
}

// Measure runs req on e.
func Measure(e Engine, req Request) (res Result) {
	ctx := context.Background()
	defer e.Close(ctx)
	bin, err := os.ReadFile(req.Wasm)
	if err != nil {
		return Result{Stage: "compile", Error: err.Error()}
	}
	fail := func(stage string, err error) Result {
		res.Stage, res.Error = stage, err.Error()
		return res
	}

	t0 := clock()
	mod, err := e.Compile(ctx, bin)
	res.Compile = int64(clock() - t0)
	if err != nil {
		return fail("compile", err)
	}

	var run func() error
	switch req.Kind {
	case "call":
		t0 := clock()
		inst, err := mod.Instantiate(ctx)
		res.Instantiate = int64(clock() - t0)
		if err != nil {
			return fail("instantiate", err)
		}
		defer inst.Close(ctx)
		run = func() error {
			v, err := inst.Call(ctx, req.Export, req.Arg)
			if err != nil {
				return err
			}
			if got := strconv.FormatInt(int64(v), 10); got != req.Want {
				return checkError{fmt.Sprintf("%s(%d) = %s, want %s", req.Export, req.Arg, got, req.Want)}
			}
			return nil
		}
	case "wasi":
		var stdout, stderr bytes.Buffer
		run = func() error {
			stdout.Reset()
			stderr.Reset()
			if err := mod.RunWASI(ctx, req.Args, &stdout, &stderr); err != nil {
				return fmt.Errorf("%w\nstderr:\n%s", err, stderr.Bytes())
			}
			if got := stdout.String(); got != req.Want {
				return checkError{fmt.Sprintf("%q wrote %q, want %q", req.Args, got, req.Want)}
			}
			return nil
		}
	default:
		return fail("request", fmt.Errorf("unknown kind %q", req.Kind))
	}

	var before, after runtime.MemStats
	var profile *os.File
	defer func() {
		if profile != nil {
			pprof.StopCPUProfile()
			profile.Close()
		}
	}()
	for i := range req.Warmup + req.Reps {
		if i == req.Warmup {
			runtime.ReadMemStats(&before)
			if req.CPUProfile != "" {
				f, err := os.Create(req.CPUProfile)
				if err != nil {
					return fail("profile", err)
				}
				if err := pprof.StartCPUProfile(f); err != nil {
					f.Close()
					return fail("profile", err)
				}
				profile = f
			}
		}
		c0, t0 := processCPU(), clock()
		err := run()
		t1, c1 := clock(), processCPU()
		res.Runs = append(res.Runs, int64(t1-t0))
		res.RunsCPU = append(res.RunsCPU, int64(c1-c0))
		if err != nil {
			stage := "run"
			if _, ok := err.(checkError); ok {
				stage = "check"
			}
			return fail(stage, err)
		}
	}
	if req.Reps > 0 {
		runtime.ReadMemStats(&after)
		res.AllocBytes = (after.TotalAlloc - before.TotalAlloc) / uint64(req.Reps)
		res.Allocs = (after.Mallocs - before.Mallocs) / uint64(req.Reps)
	}
	res.PeakRSS = peakRSS()
	return res
}

// checkError is a run whose result was not the one wanted.
type checkError struct{ msg string }

func (e checkError) Error() string { return e.msg }
