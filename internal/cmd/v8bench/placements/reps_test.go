package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fakeEnv, set in a process's environment, makes the test binary a fake
// build of v8bench (fakeV8bench): "new" one with -perrun, "old" one from
// before it. fakeFirstEnv is how much longer than its others Long's first
// run is, 300 ms if it is not set.
const (
	fakeEnv      = "PLACEMENTS_FAKE_V8BENCH"
	fakeFirstEnv = "PLACEMENTS_FAKE_FIRST"
)

func TestMain(m *testing.M) {
	if kind := os.Getenv(fakeEnv); kind != "" {
		os.Exit(fakeV8bench(kind, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeWarmUp is what each of Warm's first runs takes past its steady
// 20 ms: a warm-up over four runs, as a JIT's compiles are, its first run
// 100 ms, which repsFor gives 10 runs.
var fakeWarmUp = []float64{80, 40, 20, 10}

// fakeRun is the time of benchmark name's run i, from 0: Warm's warms up
// over its first four runs, and Long's first, past a second as Octane's
// Typescript is, takes fakeFirstEnv's milliseconds more than its others'
// 1500.
func fakeRun(name string, i int) float64 {
	if name == "Long" {
		if i > 0 {
			return 1500
		}
		first, err := strconv.ParseFloat(os.Getenv(fakeFirstEnv), 64)
		if err != nil {
			first = 300
		}
		return 1500 + first
	}
	if i < len(fakeWarmUp) {
		return 20 + fakeWarmUp[i]
	}
	return 20
}

// fakeV8bench prints what v8bench's fixed mode prints for two benchmarks,
// Warm and Long (fakeRun), without running anything; kind "old" has no
// -perrun.
func fakeV8bench(kind string, args []string) int {
	fs := flag.NewFlagSet("v8bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.String("dir", "", "the directory the suite is in")
	fs.String("bench", "v8", "the suite")
	fs.String("mode", "fixed", "the mode")
	fs.Bool("jit", false, "the JIT")
	fs.Bool("jitbackground", false, "compile in the background")
	n := fs.Int("n", 5, "runs of each benchmark")
	only := fs.String("suite", "", "suites to run")
	perRun := new(bool)
	if kind != "old" {
		fs.BoolVar(perRun, "perrun", false, "print each run's time")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fmt.Println("go-quickjs, octane, fixed")
	all := 0.0
	for _, name := range []string{"Warm", "Long"} {
		if *only != "" && !strings.Contains(","+*only+",", ","+name+",") {
			continue
		}
		var runs []string
		total := 0.0
		for i := range *n {
			v := fakeRun(name, i)
			total += v
			runs = append(runs, fmt.Sprintf(" %.3f", v))
		}
		all += total
		fmt.Printf("%-13s %9.1f ms %9.1f cpu %10.1f MB %11d allocs\n", name, total, total, 1.0, 1)
		if *perRun {
			fmt.Printf("%s runs ms:%s\n", name, strings.Join(runs, ""))
		}
	}
	fmt.Printf("%-13s %9.1f ms %9.1f cpu %10.1f MB %11d allocs\n", "TOTAL", all, all, 1.0, 1)
	fmt.Println("peak heap 1.0 MB, live after GC 1.0 MB")
	return 0
}

// fakeBuild puts the test binary where placement 0 of label's build goes,
// in a directory of its own, and has it run as a fake build of kind.
func fakeBuild(t *testing.T, kind string) string {
	t.Helper()
	t.Setenv(fakeEnv, kind)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(exePath(out, "fake", 0), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// figures runs placement 0 of the fake build in out, with the runs pin
// gives (-reps), and returns each benchmark's figure.
func figures(t *testing.T, out, pin string, c runConfig) map[string]named {
	t.Helper()
	var reps repsTable
	if pin != "" {
		if err := reps.pin(pin); err != nil {
			t.Fatal(err)
		}
	}
	c.dir, c.bench = t.TempDir(), "octane"
	got, err := runPlacement("fake", out, 0, c, nil, &reps)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]named{}
	for _, g := range got {
		m[g.name] = g
	}
	if len(m) != len(got) {
		t.Fatalf("a benchmark twice: %v", got)
	}
	return m
}

// A short benchmark's figure is its steady time a run, whatever runs it
// is given again: Box2D with the JIT, warm after ten runs or so, was 43 ms
// a run over 10 runs and 28.5 over 30 when the figure was the runs' mean,
// and the runs, decided by its first run of about 100 ms, were 10 in one
// comparison and 30 in another. A long one's is its one run.
func TestFigureIndependentOfRuns(t *testing.T) {
	out := fakeBuild(t, "new")
	if !printsRuns(exePath(out, "fake", 0)) {
		t.Fatal("a build with -perrun is taken for one without")
	}
	for _, r := range []int{0, 10, 30, 100} {
		pin := ""
		if r > 0 {
			pin = fmt.Sprintf("Warm=%d", r)
		}
		got := figures(t, out, pin, runConfig{perRun: true})
		w := got["Warm"]
		want := r
		if r == 0 {
			want = 10 // its first run's 100 ms
		}
		if w.reps != want || w.ms != 20 || w.first != 100 || w.dropped != want/2 {
			t.Errorf("Warm pinned to %d runs: %d runs, figure %v, first %v, %d dropped; want %d, 20, 100, %d", r, w.reps, w.ms, w.first, w.dropped, want, want/2)
		}
		if l := got["Long"]; l.ms != 1800 || l.reps != 0 {
			t.Errorf("Long: %v ms over %d runs again, want its one run's 1800 and none", l.ms, l.reps)
		}
		if tot := got["TOTAL"]; tot.ms != 1820 {
			t.Errorf("TOTAL %v, want 1820", tot.ms)
		}
		if _, ok := got["Long"+firstSuffix]; ok {
			t.Error("a first+steady figure without -steady")
		}
	}
}

// -steady runs a long benchmark again too, and its steady figure is its
// runs after its first, whatever its runs and however long its first run
// was; its first+steady figure is its one run, as is a short one's.
func TestSteadyFigures(t *testing.T) {
	out := fakeBuild(t, "new")
	for _, first := range []float64{300, 900} {
		t.Setenv(fakeFirstEnv, strconv.FormatFloat(first, 'f', -1, 64))
		for _, r := range []int{0, 3, 10} {
			pin := ""
			if r > 0 {
				pin = fmt.Sprintf("Long=%d", r)
			}
			got := figures(t, out, pin, runConfig{perRun: true, steady: true})
			want := r
			if r == 0 {
				want = 2 // a long one's, at least twice
			}
			l := got["Long"]
			if l.ms != 1500 || l.reps != want || l.first != 1500+first {
				t.Errorf("Long, first run %v ms, pinned to %d: steady %v over %d runs, first %v; want 1500 over %d, %v", 1500+first, r, l.ms, l.reps, l.first, want, 1500+first)
			}
			if w := got["Warm"]; w.ms != 20 || w.reps != 10 {
				t.Errorf("Warm: steady %v over %d runs, want 20 over 10", w.ms, w.reps)
			}
			if tot := got["TOTAL"]; tot.ms != 1520 {
				t.Errorf("TOTAL %v, want 1520", tot.ms)
			}
			for name, want := range map[string]float64{"Long": 1500 + first, "Warm": 100, "TOTAL": 1600 + first} {
				if f := got[name+firstSuffix]; f.ms != want {
					t.Errorf("%s first+steady %v, want its one run's %v", name, f.ms, want)
				}
			}
		}
	}
}

// A build from before -perrun prints no runs' times: compare sees it, and
// its figure is the process's total a run, which depends on the runs.
func TestFigureOfABuildWithoutRuns(t *testing.T) {
	out := fakeBuild(t, "old")
	if printsRuns(exePath(out, "fake", 0)) {
		t.Fatal("a build without -perrun is taken for one with")
	}
	for _, r := range []int{10, 30} {
		w := figures(t, out, fmt.Sprintf("Warm=%d", r), runConfig{})["Warm"]
		want := 20.0
		for _, v := range fakeWarmUp {
			want += v / float64(r)
		}
		if w.reps != r || w.dropped != -1 || w.ms != want || w.mean != want {
			t.Errorf("Warm over %d runs: %+v, want the runs' mean %v", r, w, want)
		}
	}
}

// -reps pins a benchmark's runs, 0 for its one run, over its first run's.
func TestRepsPinned(t *testing.T) {
	var reps repsTable
	if err := reps.pin("Box2D=30, Splay=0"); err != nil {
		t.Fatal(err)
	}
	if err := reps.pin("Richards=100"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"Box2D": 30, "Splay": 0, "Richards": 100, "Crypto": 10} {
		if got := reps.of(name, 101); got != want {
			t.Errorf("%s: %d runs, want %d", name, got, want)
		}
	}
	for _, bad := range []string{"Box2D", "Box2D=1", "Box2D=-3", "=10", "Box2D=x"} {
		if err := reps.pin(bad); err == nil {
			t.Errorf("-reps %q passed", bad)
		}
	}
}

// -v says what each row's figures were taken from: the runs, those the
// figure is the mean of, and each placement's first run and all runs'
// mean.
func TestPrintInputs(t *testing.T) {
	out := fakeBuild(t, "new")
	got := figures(t, out, "Warm=30", runConfig{perRun: true, steady: true})
	var best [2][]map[string]named
	for l := range 2 {
		best[l] = []map[string]named{got}
	}
	for name, want := range map[string]string{
		"Warm": "    r 30: the mean of runs 16 to 30\n" +
			"    base: 20.00   (first run 100.00; all runs' mean 25.00)\n" +
			"    change: 20.00   (first run 100.00; all runs' mean 25.00)\n",
		"Long": "    r 2: the mean of runs 2 to 2\n" +
			"    base: 1500.00   (first run 1800.00; all runs' mean 1650.00)\n" +
			"    change: 1500.00   (first run 1800.00; all runs' mean 1650.00)\n",
		"Long" + firstSuffix: "    r 0: its one run in the suite's process\n" +
			"    base: 1800.00\n" +
			"    change: 1800.00\n",
	} {
		v := got[name].ms
		var b strings.Builder
		printInputs(&b, [2]string{"base", "change"}, name, [2][]float64{{v}, {v}}, best, 1)
		if b.String() != want {
			t.Errorf("printInputs of %s:\n%s\nwant\n%s", name, b.String(), want)
		}
	}
}
