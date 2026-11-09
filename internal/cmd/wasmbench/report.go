package main

import (
	"fmt"
	"io"
	"math"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/internal/worker"
)

// A cell is what the samples of one workload on one engine came to. Times
// are in milliseconds.
type cell struct {
	Suite    string `json:"suite"`
	Workload string `json:"workload"`
	Engine   string `json:"engine"`
	Samples  int    `json:"samples"`
	// Compile and Instantiate are the samples' medians; Instantiate is a
	// micro-benchmark's, a program being instantiated by every run.
	Compile     float64 `json:"compile_ms"`
	Instantiate float64 `json:"instantiate_ms,omitempty"`
	// First is the median of the samples' first runs; Best and Median are
	// over every timed run, and CPU is the median of their CPU times.
	First  float64   `json:"first_ms"`
	Best   float64   `json:"best_ms"`
	Median float64   `json:"median_ms"`
	CPU    float64   `json:"cpu_ms"`
	Runs   []float64 `json:"runs_ms"`
	// AllocBytes and Allocs are the Go heap's per timed run, the samples'
	// median, for the Go engines; PeakRSS is the largest sample's.
	AllocBytes uint64 `json:"alloc_bytes,omitempty"`
	Allocs     uint64 `json:"allocs,omitempty"`
	PeakRSS    uint64 `json:"peak_rss,omitempty"`
	// Stage and Error are the first failure: a compile error is a feature
	// the engine lacks; any other is a wrong or failed run.
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
}

// summarize makes w's cell for e from its samples, err being the first
// failure.
func summarize(w workload, e engine, samples []worker.Result, err error) *cell {
	c := &cell{Suite: w.suite, Workload: w.name, Engine: e.name}
	if err != nil {
		c.Stage, c.Error = "worker", err.Error()
	}
	var compile, inst, first, cpu []float64
	var allocBytes, allocs []uint64
	for _, s := range samples {
		if s.Error != "" {
			c.Stage, c.Error = s.Stage, s.Error
			continue
		}
		c.Samples++
		compile = append(compile, ms(s.Compile))
		inst = append(inst, ms(s.Instantiate))
		if len(s.Runs) > 0 {
			first = append(first, ms(s.Runs[0]))
		}
		for i := *warmupFlag; i < len(s.Runs); i++ {
			c.Runs = append(c.Runs, ms(s.Runs[i]))
			cpu = append(cpu, ms(s.RunsCPU[i]))
		}
		allocBytes = append(allocBytes, s.AllocBytes)
		allocs = append(allocs, s.Allocs)
		c.PeakRSS = max(c.PeakRSS, s.PeakRSS)
	}
	c.Compile = median(compile)
	c.Instantiate = median(inst)
	c.First = median(first)
	c.Median = median(c.Runs)
	if len(c.Runs) > 0 {
		c.Best = slices.Min(c.Runs)
	}
	c.CPU = median(cpu)
	c.AllocBytes = medianUint(allocBytes)
	c.Allocs = medianUint(allocs)
	return c
}

func ms(ns int64) float64 { return float64(ns) / 1e6 }

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func medianUint(xs []uint64) uint64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// textRow is the text table's layout, a line printed as each workload ends.
const textRow = "%-20s %-16s %9s %9s %9s %9s %9s %9s %10s %10s %9s\n"

func printTextHeader() {
	fmt.Printf(textRow, "workload", "engine", "compile", "inst", "first", "best", "median", "cpu", "alloc/run", "allocs/run", "peak rss")
}

// printTextRow prints c as a line of the text table.
func printTextRow(c *cell) {
	name := c.Suite + "/" + c.Workload
	if c.Error != "" {
		fmt.Printf("%-20s %-16s %s failed: %s\n", name, c.Engine, c.Stage, firstLine(c.Error))
		return
	}
	inst := "-"
	if c.Suite == suiteMicro {
		inst = fmtMS(c.Instantiate)
	}
	allocs := "-"
	if c.Allocs > 0 || c.AllocBytes > 0 {
		allocs = strconv.FormatUint(c.Allocs, 10)
	}
	fmt.Printf(textRow, name, c.Engine, fmtMS(c.Compile), inst, fmtMS(c.First), fmtMS(c.Best), fmtMS(c.Median),
		fmtMS(c.CPU), fmtBytes(c.AllocBytes), allocs, fmtBytes(c.PeakRSS))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// fmtMS formats a time in milliseconds to three significant digits or so.
func fmtMS(v float64) string {
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64) + "ms"
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64) + "ms"
	case v >= 1:
		return strconv.FormatFloat(v, 'f', 2, 64) + "ms"
	default:
		return strconv.FormatFloat(v, 'f', 3, 64) + "ms"
	}
}

func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// printMatrix prints, for each suite, a Markdown table of the median run
// times with the engines side by side and go-quickjs's ratios to Node and to
// wazero's compiler, and a table of the compile times.
func printMatrix(w io.Writer, es []engine, ws []workload, cells []*cell) {
	at := map[[2]string]*cell{}
	for _, c := range cells {
		at[[2]string{c.Suite + "/" + c.Workload, c.Engine}] = c
	}
	has := func(name string) bool {
		return slices.ContainsFunc(es, func(e engine) bool { return e.name == name })
	}
	type ratio struct{ num, den, title string }
	var ratios []ratio
	for _, den := range []struct{ name, short string }{{"node", "Node"}, {"wazero-compiler", "wazero-c"}} {
		for _, num := range []struct{ name, short string }{{"interp", "interp"}, {"jit", "JIT"}} {
			if has(num.name) && has(den.name) {
				ratios = append(ratios, ratio{num.name, den.name, num.short + "/" + den.short})
			}
		}
	}

	fmt.Fprintf(w, "wasmbench -matrix: %s/%s, %d CPUs, %s, %s; -n %d -reps %d -warmup %d.\n",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), time.Now().Format("2006-01-02"),
		*nFlag, *repsFlag, *warmupFlag)
	if v := nodeVersion(es); v != "" {
		fmt.Fprintf(w, "Node %s.\n", v)
	}
	for _, suite := range []string{suiteMicro, suitePrograms} {
		var rows []workload
		for _, wl := range ws {
			if wl.suite == suite {
				rows = append(rows, wl)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n## %s: median run, ms\n\n", suite)
		fmt.Fprint(w, "| workload |")
		for _, e := range es {
			fmt.Fprintf(w, " %s |", e.title)
		}
		for _, r := range ratios {
			fmt.Fprintf(w, " %s |", r.title)
		}
		fmt.Fprint(w, "\n|---|")
		fmt.Fprint(w, strings.Repeat("---:|", len(es)+len(ratios)))
		fmt.Fprintln(w)
		logs := make([][]float64, len(ratios))
		for _, wl := range rows {
			fmt.Fprintf(w, "| %s |", wl.name)
			for _, e := range es {
				fmt.Fprintf(w, " %s |", matrixValue(at[[2]string{wl.fullName(), e.name}], func(c *cell) float64 { return c.Median }))
			}
			for i, r := range ratios {
				num, den := at[[2]string{wl.fullName(), r.num}], at[[2]string{wl.fullName(), r.den}]
				if !usable(num) || !usable(den) || den.Median == 0 {
					fmt.Fprint(w, " |")
					continue
				}
				q := num.Median / den.Median
				logs[i] = append(logs[i], math.Log(q))
				fmt.Fprintf(w, " %s |", fmtRatio(q))
			}
			fmt.Fprintln(w)
		}
		if len(ratios) > 0 {
			fmt.Fprint(w, "| geomean |", strings.Repeat(" |", len(es)))
			for _, l := range logs {
				if len(l) == 0 {
					fmt.Fprint(w, " |")
					continue
				}
				var s float64
				for _, x := range l {
					s += x
				}
				fmt.Fprintf(w, " %s |", fmtRatio(math.Exp(s/float64(len(l)))))
			}
			fmt.Fprintln(w)
		}

		fmt.Fprintf(w, "\n## %s: compile, ms\n\n", suite)
		fmt.Fprint(w, "| workload |")
		for _, e := range es {
			fmt.Fprintf(w, " %s |", e.title)
		}
		fmt.Fprint(w, "\n|---|", strings.Repeat("---:|", len(es)), "\n")
		for _, wl := range rows {
			fmt.Fprintf(w, "| %s |", wl.name)
			for _, e := range es {
				fmt.Fprintf(w, " %s |", matrixValue(at[[2]string{wl.fullName(), e.name}], func(c *cell) float64 { return c.Compile }))
			}
			fmt.Fprintln(w)
		}
	}

	var notes []string
	for _, c := range cells {
		if c.Error != "" {
			notes = append(notes, fmt.Sprintf("- %s/%s on %s: %s: %s", c.Suite, c.Workload, c.Engine, c.Stage, firstLine(c.Error)))
		}
	}
	if len(notes) > 0 {
		fmt.Fprintf(w, "\nn/a is a module the engine does not compile, FAIL a run that failed:\n\n%s\n", strings.Join(notes, "\n"))
	}
}

// usable reports whether c has times.
func usable(c *cell) bool { return c != nil && c.Error == "" && c.Samples > 0 }

// matrixValue is f of c for a table, or what kept c from having one.
func matrixValue(c *cell, f func(*cell) float64) string {
	switch {
	case c == nil:
		return ""
	case c.Error != "" && c.Stage == "compile":
		return "n/a"
	case !usable(c):
		return "FAIL"
	}
	v := f(c)
	switch {
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	case v >= 1:
		return strconv.FormatFloat(v, 'f', 2, 64)
	default:
		return strconv.FormatFloat(v, 'f', 3, 64)
	}
}

func fmtRatio(q float64) string {
	if q >= 10 {
		return strconv.FormatFloat(q, 'f', 0, 64) + "x"
	}
	return strconv.FormatFloat(q, 'f', 2, 64) + "x"
}

// nodeVersion is node --version, when a Node engine ran.
func nodeVersion(es []engine) string {
	for _, e := range es {
		if e.kind == "node" {
			out, err := exec.Command(*nodeFlag, "--version").Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}
