// Command v8bench runs the V8 version 7 benchmark suite, as AreWeFastYet
// publishes it, on go-quickjs, for measuring and profiling the engine.
//
// The suite is not part of this repository. -fetch downloads it, at the
// revision the README's figures were measured at, into -dir:
//
//	go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch
//
// Three modes measure three things:
//
//   - score runs the suite as its own run.js does, and prints its scores:
//     each benchmark runs for at least a second, so the work done depends
//     on the speed, and the scores are what compare with other engines.
//   - fixed runs each benchmark -n times after its setup, and prints the
//     time and the allocations of each suite: the same work every time,
//     which is what compares one build of the engine with another.
//   - compile parses and compiles the suite's sources -n times, which is
//     the parser and the compiler alone.
//
// -cpuprofile and -memprofile write profiles for go tool pprof.
package main

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

	quickjs "github.com/go-quickjs/go-quickjs"
)

// suiteURL is where the suite is fetched from: AreWeFastYet at the revision
// the README's figures were measured at.
const suiteURL = "https://raw.githubusercontent.com/mozilla/arewefastyet/0e21608/benchmarks/v8-v7/"

// files are the suite's sources, in the order run.js loads them.
var files = []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
	"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js"}

func main() {
	dir := flag.String("dir", "", "the directory the suite is in")
	fetch := flag.Bool("fetch", false, "download the suite into -dir first")
	mode := flag.String("mode", "fixed", "score, fixed or compile")
	n := flag.Int("n", 5, "runs of each benchmark (fixed), or compiles of the suite (compile)")
	only := flag.String("suite", "", "comma-separated suites to run, all by default (fixed)")
	cpu := flag.String("cpuprofile", "", "write a CPU profile to this file")
	mem := flag.String("memprofile", "", "write an allocation profile to this file")
	flag.Parse()
	if *dir == "" {
		fail(fmt.Errorf("-dir is required"))
	}
	if *fetch {
		if err := download(*dir); err != nil {
			fail(err)
		}
	}

	src := map[string]string{}
	for _, f := range append(files, "run.js") {
		b, err := os.ReadFile(filepath.Join(*dir, f))
		if err != nil {
			fail(fmt.Errorf("%w (run with -fetch to download the suite)", err))
		}
		src[f] = string(b)
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

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	switch *mode {
	case "compile":
		for i := 0; i < *n; i++ {
			for _, f := range files {
				if _, err := quickjs.Compile(f, src[f]); err != nil {
					fail(fmt.Errorf("%s: %w", f, err))
				}
			}
		}
	case "score":
		rt := newRuntime(src)
		if _, err := rt.EvalFile("run.js", src["run.js"]); err != nil {
			fail(err)
		}
	case "fixed":
		runFixed(src, *n, *only)
	default:
		fail(fmt.Errorf("unknown mode %q", *mode))
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	report("TOTAL", time.Since(start), &before, &after)
	fmt.Printf("peak heap %.1f MB\n", float64(after.HeapSys)/(1<<20))

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
// suite as it finishes.
func runFixed(src map[string]string, n int, only string) {
	rt := newRuntime(src)
	for _, f := range files {
		if _, err := rt.EvalFile(f, src[f]); err != nil {
			fail(fmt.Errorf("%s: %w", f, err))
		}
	}
	suites, err := rt.Eval(`BenchmarkSuite.suites.map(s => s.name)`)
	if err != nil {
		fail(err)
	}
	var names []string
	if err := suites.Decode(&names); err != nil {
		fail(err)
	}
	for _, name := range names {
		if only != "" && !strings.Contains(","+only+",", ","+name+",") {
			continue
		}
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		t0 := time.Now()
		_, err := rt.Eval(fmt.Sprintf(`(() => {
			const s = BenchmarkSuite.suites.find(s => s.name === %q);
			for (const b of s.benchmarks) {
				b.Setup();
				for (let i = 0; i < %d; i++) b.run();
				b.TearDown();
			}
		})()`, name, n))
		d := time.Since(t0)
		runtime.ReadMemStats(&m1)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		report(name, d, &m0, &m1)
	}
}

func report(name string, d time.Duration, m0, m1 *runtime.MemStats) {
	fmt.Printf("%-13s %9.1f ms %10.1f MB %11d allocs\n", name,
		float64(d.Microseconds())/1000, float64(m1.TotalAlloc-m0.TotalAlloc)/(1<<20), m1.Mallocs-m0.Mallocs)
}

// newRuntime is a runtime with the print and load the suite expects.
func newRuntime(src map[string]string) *quickjs.Runtime {
	rt := quickjs.New()
	rt.Set("print", func(s string) { fmt.Println(s) })
	rt.Set("load", func(r *quickjs.Runtime, name string) error {
		_, err := r.EvalFile(name, src[name])
		return err
	})
	return rt
}

// download fetches the suite's sources into dir.
func download(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range append(files, "run.js") {
		resp, err := http.Get(suiteURL + f)
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
