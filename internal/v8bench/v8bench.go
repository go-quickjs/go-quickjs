// Package v8bench runs benchmark suites -- the V8 suite's version 7, as
// AreWeFastYet publishes it, by default; Octane 2.0; Are We Fast Yet (see
// suites.go) -- and QuickJS's micro-benchmarks on a JavaScript engine, for
// measuring and profiling it.
//
// It knows nothing of any engine: a command gives it an Engine and calls
// Main. internal/cmd/v8bench runs the suite on go-quickjs, and
// internal/cmd/v8bench/goja, a module of its own so that go-quickjs does not
// depend on goja, runs it on goja, for comparison. Both report the same way.
//
// The suites are not part of this repository: some of their files are under
// licenses this one's is not. -fetch downloads the one -bench names, at a
// fixed revision -- the V8 suite's the figures in docs/benchmarks.md were
// measured at -- into -dir. QuickJS's
// tests/microbench.js is: microbench.js is QuickJS 2026-06-04's (92a3d4d),
// as it is there, under its MIT license, and is built into the package, as
// is microbench_extra.js, ours, written as its tests are.
// Four modes measure four things:
//
//   - score runs the suite as its own run.js does, and prints its scores
//     (the V8 suite and Octane):
//     each benchmark runs for at least a second, so the work done depends
//     on the speed, and the scores are what compare with other engines.
//   - fixed runs each benchmark -n times after its setup, and prints the
//     time and the allocations of each suite: the same work every time,
//     which is what compares one build, or one engine, with another. An
//     Are We Fast Yet benchmark's run is one of its harness's iterations,
//     its result checked.
//   - compile parses and compiles the suite's sources -n times, which is
//     the parser and the compiler alone.
//   - micro runs QuickJS's micro-benchmarks, and ours of generators and
//     async functions, each test -n times with the same work, and prints
//     the fastest time of one operation, in nanoseconds (see micro.go). It
//     needs no -dir; -suite names tests by the start of their names.
//
// -cpuprofile and -memprofile write profiles for go tool pprof.
package v8bench

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// An Engine is what the suite runs on.
type Engine interface {
	// Compile parses and compiles a script without running it.
	Compile(name, src string) error
	// NewRuntime makes a runtime with the host's functions as globals of
	// their names: print, load, read and now.
	NewRuntime(host Host) (Runtime, error)
}

// Host is the functions a runtime is given: print, load, read and now.
type Host struct {
	// Print writes a line.
	Print func(string)
	// Load reads one of the suite's files by its name, for load to run, and
	// for read to give as a string, as d8's read does: Octane's zlib, built
	// for d8, takes it to be there.
	Load func(name string) (string, error)
	// Now is the time in milliseconds, to the nanosecond, from a fixed
	// point: what microbench.js's clock, performance.now, reads.
	Now func() float64
}

// A Runtime runs scripts.
type Runtime interface {
	// Run runs a script, named for its stack traces.
	Run(name, src string) error
	// EvalString runs a script and returns its completion value, a string.
	EvalString(src string) (string, error)
}

// A Reporter is a Runtime with something to say of each benchmark the fixed
// mode runs, after its line: go-quickjs's what its JIT did (-jitreport).
type Reporter interface {
	Report(name string)
}

// Main parses the command line and runs the suite on the engine, named in
// what it prints.
func Main(name string, e Engine) {
	dir := flag.String("dir", "", "the directory the suite is in")
	which := flag.String("bench", "v8", "the suite: v8, octane or awfy")
	fetch := flag.Bool("fetch", false, "download the suite into -dir first")
	mode := flag.String("mode", "fixed", "score, fixed, compile or micro")
	n := flag.Int("n", 5, "runs of each benchmark (fixed, micro), or compiles of the suite (compile)")
	only := flag.String("suite", "", "comma-separated suites to run, all by default (fixed); tests by the start of their names (micro)")
	cpu := flag.String("cpuprofile", "", "write a CPU profile to this file")
	mem := flag.String("memprofile", "", "write an allocation profile to this file")
	flag.Parse()
	s := suiteNamed(*which)
	src := map[string]string{}
	if *mode != "micro" {
		if *dir == "" {
			fail(fmt.Errorf("-dir is required"))
		}
		if *fetch {
			if err := download(s, *dir); err != nil {
				fail(err)
			}
		}
		for _, f := range append(s.files, s.extra...) {
			b, err := os.ReadFile(filepath.Join(*dir, f))
			if err != nil {
				fail(fmt.Errorf("%w (run with -fetch to download the suite)", err))
			}
			src[f] = string(b)
		}
	}
	if *mem != "" {
		runtime.MemProfileRate = 64 * 1024
	}
	if *cpu != "" {
		f, err := os.Create(*cpu)
		if err != nil {
			fail(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fail(err)
		}
		defer pprof.StopCPUProfile()
	}

	if *mode == "micro" || s.name == "v8" {
		fmt.Printf("%s, %s\n", name, *mode)
	} else {
		fmt.Printf("%s, %s, %s\n", name, s.name, *mode)
	}
	// The runtime the suite ran in is kept to the end, so that what is live
	// after the run is measured with it: a leak is what a live runtime keeps.
	var rt Runtime
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start, cpu0 := clock(), processCPU()
	switch *mode {
	case "compile":
		for i := 0; i < *n; i++ {
			for _, f := range s.files {
				if err := e.Compile(f, src[f]); err != nil {
					fail(fmt.Errorf("%s: %w", f, err))
				}
			}
		}
	case "score":
		if s.awfy {
			fail(fmt.Errorf("Are We Fast Yet has no scores: run it in the fixed mode"))
		}
		rt = newRuntime(e, src)
		if err := rt.Run("run.js", src["run.js"]); err != nil {
			fail(err)
		}
	case "fixed":
		if s.awfy {
			rt = runAWFY(e, s, src, *n, *only)
		} else {
			rt = runFixed(e, s, src, *n, *only)
		}
	case "micro":
		rt = runMicro(e, *n, *only)
		// What the JIT did over every test run, one report for them all:
		// -suite picks the test it is of.
		if r, ok := rt.(Reporter); ok {
			r.Report("micro")
		}
	default:
		fail(fmt.Errorf("unknown mode %q", *mode))
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	report("TOTAL", clock()-start, processCPU()-cpu0, &before, &after)
	// What is live once a collection has run: after a suite's teardown,
	// anything a run left behind that grows with the runs is a leak. The
	// allocation profile's in-use figures are as of this collection too.
	runtime.GC()
	var settled runtime.MemStats
	runtime.ReadMemStats(&settled)
	fmt.Printf("peak heap %.1f MB, live after GC %.1f MB\n",
		float64(after.HeapSys)/(1<<20), float64(settled.HeapAlloc)/(1<<20))
	runtime.KeepAlive(rt)

	if *mem != "" {
		f, err := os.Create(*mem)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			fail(err)
		}
	}
}

// runFixed loads the suite and runs each benchmark n times, reporting each
// suite as it finishes, and returns the runtime it ran them in.
func runFixed(e Engine, s *suite, src map[string]string, n int, only string) Runtime {
	rt := newRuntime(e, src)
	for _, f := range s.files {
		if err := rt.Run(f, src[f]); err != nil {
			fail(fmt.Errorf("%s: %w", f, err))
		}
	}
	list, err := rt.EvalString(`BenchmarkSuite.suites.map(s => s.name).join()`)
	if err != nil {
		fail(err)
	}
	for _, name := range strings.Split(list, ",") {
		if only != "" && !strings.Contains(","+only+",", ","+name+",") {
			continue
		}
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		t0, c0 := clock(), processCPU()
		_, err := rt.EvalString(fmt.Sprintf(`(() => {
			const s = BenchmarkSuite.suites.find(s => s.name === %q);
			// Octane's Math.random, seeded again for each suite as its
			// harness does, which its checksums assume.
			if (BenchmarkSuite.ResetRNG) BenchmarkSuite.ResetRNG();
			for (const b of s.benchmarks) {
				b.Setup();
				for (let i = 0; i < %d; i++) b.run();
				b.TearDown();
			}
			return "";
		})()`, name, n))
		d, c := clock()-t0, processCPU()-c0
		runtime.ReadMemStats(&m1)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		report(name, d, c, &m0, &m1)
		if r, ok := rt.(Reporter); ok {
			r.Report(name)
		}
	}
	return rt
}

// runAWFY loads Are We Fast Yet and runs each of its benchmarks n times,
// reporting each as it finishes, and returns the runtime it ran them in.
func runAWFY(e Engine, s *suite, src map[string]string, n int, only string) Runtime {
	rt := newRuntime(e, src)
	if err := rt.Run("awfy.js", awfyModules(s, src)); err != nil {
		fail(fmt.Errorf("awfy.js: %w", err))
	}
	for _, w := range awfyWork {
		if only != "" && !strings.Contains(","+only+",", ","+w.name+",") {
			continue
		}
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		t0, c0 := clock(), processCPU()
		_, err := rt.EvalString(awfyRun(w.module, w.inner, n))
		d, c := clock()-t0, processCPU()-c0
		runtime.ReadMemStats(&m1)
		if err != nil {
			fail(fmt.Errorf("%s: %w", w.name, err))
		}
		report(w.name, d, c, &m0, &m1)
		if r, ok := rt.(Reporter); ok {
			r.Report(w.name)
		}
	}
	return rt
}

// report prints a benchmark's time, the CPU time the process spent on it --
// the collector's on other threads included, which the time does not show
// on a machine with cores to spare -- and what it allocated.
func report(name string, d, cpu time.Duration, m0, m1 *runtime.MemStats) {
	fmt.Printf("%-13s %9.1f ms %9.1f cpu %10.1f MB %11d allocs\n", name,
		float64(d.Microseconds())/1000, float64(cpu.Microseconds())/1000,
		float64(m1.TotalAlloc-m0.TotalAlloc)/(1<<20), m1.Mallocs-m0.Mallocs)
}

// newRuntime is a runtime with the print, load and now the suite expects.
func newRuntime(e Engine, src map[string]string) Runtime {
	rt, err := e.NewRuntime(Host{
		Print: func(s string) { fmt.Println(s) },
		Load: func(name string) (string, error) {
			s, ok := src[name]
			if !ok {
				return "", fmt.Errorf("no such file %q", name)
			}
			return s, nil
		},
		Now: func() float64 { return float64(clock().Nanoseconds()) / 1e6 },
	})
	if err != nil {
		fail(err)
	}
	return rt
}

// download fetches the suite's sources into dir.
func download(s *suite, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range append(s.files, s.extra...) {
		resp, err := http.Get(s.url + f)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fetching %s: %s", f, resp.Status)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "v8bench:", err)
	os.Exit(1)
}
