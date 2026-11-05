// Command placements compares two builds of go-quickjs on the V8 suite over
// several placements of package vm's code, so that a change is not mistaken
// for where the linker happened to put it.
//
// Package vm's speed depends on where its hot code lands. Go aligns each
// function to 32 bytes and lays out a package's functions in file-name
// order and then all its closures, so code added anywhere moves the code
// after it, and the V8 suite moves with it by 5 to 10% -- for code that no
// benchmark runs as much as for the change being measured. One build of
// each side compares two placements as much as two versions of the code.
//
// build makes eight builds of internal/cmd/v8bench from the working tree,
// each with the hot code placed differently. Four pads, of m functions
// that each fill one 32-byte slot, go before the four pieces of hot code,
// in files the linker places there:
//
//	aaa_pad.go    before everything in the package    executeAt
//	vm_b_pad.go   after vm.go, before vm_call.go      callDirect
//	vm_sz_pad.go  after vm_shape.go, before vm_tree.go runTree
//	zzz_pad.go    after every file, before the closures binaryNode.func1
//
// A pad moves what follows it by exactly 32*m bytes. For each placement a
// build with the pads empty says where the four are, and m is chosen so
// that each lands at the phase -- its start mod 64, the one bit a
// placement can vary -- that a 2^(4-1) design gives it: over the eight,
// each is at 0 and at 32 mod 64 four times, each pair of them in each
// combination twice, and each at a distance from the one before it that
// varies by up to 2 KB. Placement n also puts 3n never-taken tests in
// executeAt's case for nop, which the compiler lays out ahead of the
// others, so that its cases move against its dispatch. Every build is
// checked with go tool nm, and one whose phases are not the design's
// fails. The pads and the tests are removed when it is done.
//
// compare runs two labels' builds, placement by placement, for several
// rounds: each placement of each label is taken at its fastest round,
// since what the machine adds only ever adds time, and a label's figure is
// the mean over its placements, in milliseconds a run. Placement n runs
// n*5 frames of 64 bytes deeper in the stack (V8BENCH_STACKPAD), and with
// -jit has the JIT place its code from jitPads[n] into its arena
// (V8BENCH_JITPAD), and lay out its later chunks and the gaps between its
// functions by seed n+1 (V8BENCH_JITSEED): code compiled late in a suite
// had been where the code before it ended, at one place in every
// placement, and a change that resized earlier code had looked like a
// regression of a later benchmark at all eight. Each
// benchmark runs once, and one whose run took less than a second runs
// again in a process of its own as many times as fill about one -- 3, 10,
// 30, 100 or 300 -- so that a long one, Octane's zlib, takes its run's
// time and no more, and a short one, the V8 suite's Richards, more than
// the timer's grain.
//
// -k 4 takes placements 0, 3, 5 and 6 of the eight, over which the first
// three targets are each at each phase twice (the closures' at one): a
// comparison to screen a change with, in half the time, before the eight
// confirm it.
//
// -parallel runs both labels at once, each on a group of CPUs of its own,
// the groups swapped from one placement and round to the next: by default
// the groups that share a level 3 cache, as the OS reports them -- a
// Ryzen's core complexes, whose caches are their own -- else the CPUs in
// halves, of the fastest cores alone where the cores differ, a hybrid's
// P-cores; -cpus gives them ("0-5;6-11"). Each run has GOMAXPROCS as many
// as its group's CPUs. Windows and Linux give a process its CPUs; macOS
// does not, and there the two run unpinned. It halves a comparison's time:
// compare a label with itself so, first, to see that the groups measure
// alike on the machine.
//
// -micro runs v8bench's micro mode instead of a suite: QuickJS's
// micro-benchmarks and ours, each test the same work every run, -suite
// naming tests by the start of their names. It reads each test's time of
// one operation, in nanoseconds, takes each placement's best round, and
// prints, beside each label's mean over the placements, at how many the
// second label was more than 2% slower; then the geometric mean over the
// tests, and those 3% or more slower at every placement. Micro-benchmarks
// move with where the code lands more than suites do, so a test is slower
// only where it is slower at every placement.
//
// Build both sides in the same directory, one after the other: the same
// source built in another checkout has measured differently.
//
//	git checkout base && go run ./internal/cmd/v8bench/placements build -label base
//	git checkout change && go run ./internal/cmd/v8bench/placements build -label change
//	go run ./internal/cmd/v8bench/placements compare -dir /tmp/v8-v7 base change
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/cmd/v8bench/internal/cpuset"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = buildCmd(os.Args[2:])
	case "compare":
		err = compareCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "placements:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  placements build -label NAME [-k 8|4] [-out DIR]
  placements compare -dir SUITE [-bench NAME] [-k 8|4] [-rounds 3] [-suite NAMES] [-jit]
  placements compare -micro [-k 8|4] [-rounds 3] [-suite TESTS] [-jit]
                     [-parallel] [-cpus auto|halves|LIST;LIST] [-v] [-out DIR] A B`)
	os.Exit(2)
}

// defaultOut is where the builds go unless -out says otherwise.
func defaultOut() string { return filepath.Join(os.TempDir(), "go-quickjs-placements") }

// exePath is placement n of label's build.
func exePath(out, label string, n int) string {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	return filepath.Join(out, fmt.Sprintf("vb-%s-%d%s", label, n, suffix))
}

// pads are the files the pads go in, each before its target; targets are
// the targets, as go tool nm names them after the package's path.
var (
	pads    = [4]string{"internal/vm/aaa_pad.go", "internal/vm/vm_b_pad.go", "internal/vm/vm_sz_pad.go", "internal/vm/zzz_pad.go"}
	targets = [4]string{"(*Runtime).executeAt", "(*Runtime).callDirect", "(*Runtime).runTree", "binaryNode.func1"}
)

const (
	vmGo = "internal/vm/vm.go"
	nop  = "\t\tcase bytecode.OpNop:\n"
	// inner is how many never-taken tests each placement adds to nop's
	// case, times its number.
	inner = 3
)

// placementsOf is the placements -k takes: placements 0, 3, 5 and 6 for
// four, over which the first three targets are each at each phase twice;
// otherwise the first k.
func placementsOf(k int) []int {
	if k == 4 {
		return []int{0, 3, 5, 6}
	}
	ns := make([]int, k)
	for i := range ns {
		ns[i] = i
	}
	return ns
}

// design is the phase, in units of 32 bytes, that placement n gives each
// target: a 2^(4-1) design, the fourth the parity of the other three.
func design(n int) [4]int {
	b0, b1, b2 := n&1, (n>>1)&1, (n>>2)&1
	return [4]int{b0, b1, b2, b0 ^ b1 ^ b2}
}

// padSlots is the most functions a pad has: 2*31 and one.
const padSlots = 64

// dataPhase is how many bytes placement n adds to the initialized data,
// which moves the zeroed data after it -- the Go runtime's hot variables
// among it, runtime.writeBarrier and gcController -- within their cache
// lines: 0 or 32, half the placements each, as the code's phases are
// balanced. Builds of two revisions differ in their data as well as their
// code -- a few package variables, the build's own information (a tree
// with changes records it, 32 bytes more) -- which no placement had
// varied: where a variable fell in its cache line had shown, for a
// collector-bound benchmark, as a difference at every placement.
func dataPhase(n int) int {
	d := design(n)
	return 32 * (d[0] ^ d[1])
}

// padDataBase is padData's size at a phase of 0: big enough to be laid
// out on a 32-byte boundary at either size.
const padDataBase = 64

// padCounts is how many one-slot functions each pad needs for placement
// n, given where the targets are with the pads empty. Each pad moves its
// target and everything after it, and the targets come in this order, so
// each is chosen given the ones before; a pad is an even number of slots,
// varying with n and the pad, plus the one that sets its target's phase.
func padCounts(ref [4]uint64, n int) [4]int {
	want := design(n)
	var counts [4]int
	shift := 0
	for i := range 4 {
		base := 2 * ((n*37 + i*11) % 32)
		phase := int((ref[i]/32 + uint64(shift)) % 2)
		m := base + ((want[i]-phase)%2+2)%2
		counts[i] = m
		shift += m
	}
	return counts
}

// phases is each target's start, in units of 32 bytes, mod 2.
func phases(addrs [4]uint64) [4]int {
	var p [4]int
	for i, a := range addrs {
		p[i] = int(a/32) % 2
	}
	return p
}

func buildCmd(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	label := fs.String("label", "", "the name the builds are kept under")
	k := fs.Int("k", 8, "how many placements")
	out := fs.String("out", defaultOut(), "where the builds go")
	fs.Parse(args)
	if *label == "" {
		return errors.New("-label is required")
	}
	if *k < 1 || *k > 8 {
		return errors.New("-k is from 1 to 8, the design's placements")
	}
	src, err := os.ReadFile(vmGo)
	if err != nil {
		return fmt.Errorf("%w: run from the repository's root", err)
	}
	if bytes.Count(src, []byte(nop)) != 1 {
		return fmt.Errorf("%s has no case for nop to put the tests in", vmGo)
	}
	for _, p := range pads {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists: a build was interrupted? remove it", p)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	// Whatever happens, vm.go is put back and the pads go. An interrupt
	// does the same before it ends the program.
	restore := func() {
		os.WriteFile(vmGo, src, 0o644)
		for _, p := range pads {
			os.Remove(p)
		}
	}
	defer restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		restore()
		os.Exit(1)
	}()

	for _, n := range placementsOf(*k) {
		exe := exePath(*out, *label, n)
		ref, err := buildAt(src, [4]int{}, inner*n, dataPhase(n), exe)
		if err != nil {
			return err
		}
		counts := padCounts(ref, n)
		got, err := buildAt(src, counts, inner*n, dataPhase(n), exe)
		if err != nil {
			return err
		}
		if p, want := phases(got), design(n); p != want {
			return fmt.Errorf("placement %d: phases %v, want %v (pads %v)", n, p, want, counts)
		}
		fmt.Printf("%s-%d: pads %v, phases %v, distances %d %d %d\n", *label, n, counts, phases(got),
			got[1]-got[0], got[2]-got[1], got[3]-got[2])
	}
	fmt.Printf("built %d placements of %s in %s\n", *k, *label, *out)
	return nil
}

// buildAt writes the pads and the tests, builds v8bench to exe, and says
// where the targets are in it.
func buildAt(src []byte, counts [4]int, tests, data int, exe string) ([4]uint64, error) {
	var addrs [4]uint64
	var body strings.Builder
	for i := range tests {
		fmt.Fprintf(&body, "\t\t\tif in.A == padKeys[%d] {\n\t\t\t\tpadSink += %d\n\t\t\t}\n", i, i+1)
	}
	patched := strings.Replace(string(src), nop, nop+body.String(), 1)
	if err := os.WriteFile(vmGo, []byte(patched), 0o644); err != nil {
		return addrs, err
	}
	for p, m := range counts {
		if err := os.WriteFile(pads[p], padFile(p, m, tests, data), 0o644); err != nil {
			return addrs, err
		}
	}
	cmd := exec.Command("go", "build", "-o", exe, "./internal/cmd/v8bench")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return addrs, fmt.Errorf("go build: %w", err)
	}
	nm, err := exec.Command("go", "tool", "nm", exe).Output()
	if err != nil {
		return addrs, fmt.Errorf("go tool nm: %w", err)
	}
	found := [4]bool{}
	sc := bufio.NewScanner(bytes.NewReader(nm))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[1] != "T" {
			continue
		}
		for i, t := range targets {
			if strings.HasSuffix(f[2], "/internal/vm."+t) {
				a, err := strconv.ParseUint(f[0], 16, 64)
				if err != nil {
					return addrs, err
				}
				addrs[i], found[i] = a, true
			}
		}
	}
	for i, ok := range found {
		if !ok {
			return addrs, fmt.Errorf("%s is not in the build: has it been renamed?", targets[i])
		}
	}
	return addrs, nil
}

// padFile is pad p with m functions. The first holds the one init, which
// keeps every pad's functions reachable, and the tests' keys: it is before
// the first target, where its size is the same in every placement.
func padFile(p, m, tests, data int) []byte {
	var b strings.Builder
	b.WriteString("package vm\n\n")
	if p == 0 {
		fmt.Fprintf(&b, "var padSink int\n\nvar padKeys = [%d]uint32{}\n\n", max(tests, 1))
		// The data's phase: the initialized data, which the zeroed data
		// follows, data bytes more (dataPhase).
		fmt.Fprintf(&b, "var padData = [%d]byte{1}\n\n", padDataBase+data)
		b.WriteString("func init() {\n\tfor i := range padKeys {\n\t\tpadKeys[i] = ^uint32(i)\n\t}\n" +
			"\tpadSink += int(padData[len(padData)-1])\n" +
			"\tfor _, fs := range [][]func() int{padFuncs0[:], padFuncs1[:], padFuncs2[:], padFuncs3[:]} {\n" +
			"\t\tfor _, f := range fs {\n\t\t\tif f != nil {\n\t\t\t\tpadSink += f()\n\t\t\t}\n\t\t}\n\t}\n}\n\n")
	}
	names := make([]string, m)
	for i := range m {
		names[i] = fmt.Sprintf("padF%d_%d", p, i)
		// A constant-return leaf occupies only 16 bytes on arm64. Loading a
		// global and computing from it fills a 32-byte slot on both CPUs.
		fmt.Fprintf(&b, "//go:noinline\nfunc %s() int { return padSink*3 + %d }\n\n", names[i], i+1)
	}
	// An array of padSlots whatever m is, so that the pads' data is the
	// same size in every placement and in both labels' builds and only
	// the code moves: a slice of m had moved the data after it by 8*m
	// bytes, the Go runtime's hot variables (runtime.writeBarrier,
	// gcController) among them, differently for each label, whose pads
	// differ.
	fmt.Fprintf(&b, "var padFuncs%d = [%d]func() int{%s}\n", p, padSlots, strings.Join(names, ", "))
	return []byte(b.String())
}

var timeLine = regexp.MustCompile(`(?m)^(\w+)\s+([\d.]+) ms`)

// microLine is a line of v8bench's micro mode: a test, its count and the
// time of one operation, in nanoseconds.
var microLine = regexp.MustCompile(`(?m)^(\w+)\s+\d+\s+([\d.]+)\s*$`)

// microSlower is how much slower a placement of the second label must be
// than the same placement of the first to count as slower at it, and
// microReport how much slower a test must be at every placement to be
// listed after the table.
const (
	microSlower = 0.02
	microReport = 0.03
)

func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	dir := fs.String("dir", "", "the directory the suite is in")
	k := fs.Int("k", 8, "how many placements")
	rounds := fs.Int("rounds", 3, "runs of each placement of each label")
	suite := fs.String("suite", "", "comma-separated suites to run, all by default")
	bench := fs.String("bench", "", "the benchmark collection, as v8bench's -bench: v8 by default, octane, awfy, ssr, fresh")
	jitEnabled := fs.Bool("jit", false, "enable the optional native tier in both tagged builds")
	parallel := fs.Bool("parallel", false, "run both labels at once, each on a group of CPUs (-cpus)")
	cpus := fs.String("cpus", "auto", "the groups of CPUs -parallel runs on: auto, halves, or lists separated by ';' (0-5;6-11)")
	verbose := fs.Bool("v", false, "print each placement's figure")
	out := fs.String("out", defaultOut(), "where the builds are")
	micro := fs.Bool("micro", false, "run v8bench's micro mode, QuickJS's micro-benchmarks and ours, rather than a suite: -suite names tests by the start of their names, and -dir is not needed")
	fs.Parse(args)
	if (*dir == "" && !*micro) || fs.NArg() != 2 {
		usage()
	}
	labels := [2]string{fs.Arg(0), fs.Arg(1)}
	if *k < 1 || *k > 8 {
		return errors.New("-k is from 1 to 8, the design's placements")
	}
	ns := placementsOf(*k)
	var groups []cpuset.Set
	if *parallel {
		g, how, err := cpuset.Parse(*cpus)
		if err != nil {
			return err
		}
		if len(g) < 2 || len(g[0]) == 0 || len(g[1]) == 0 {
			return fmt.Errorf("-parallel needs two groups of CPUs, -cpus %q gives %v", *cpus, g)
		}
		groups = g[:2]
		pinned := "each pinned to its group"
		if !cpuset.CanPin {
			pinned = "unpinned: this OS gives a process no CPUs of its own"
		}
		fmt.Printf("in parallel, on CPUs %s and %s -- %s; %s\n", groups[0], groups[1], how, pinned)
	}
	// res[label][i][benchmark] is each round's time of placement ns[i].
	res := [2][]map[string][]float64{}
	for l := range labels {
		res[l] = make([]map[string][]float64, len(ns))
		for i, n := range ns {
			res[l][i] = map[string][]float64{}
			if _, err := os.Stat(exePath(*out, labels[l], n)); err != nil {
				return fmt.Errorf("no placement %d of %s: build it first", n, labels[l])
			}
		}
	}
	var names []string
	var reps repsTable
	add := func(l, i int, got []named) {
		for _, nv := range got {
			if !slices.Contains(names, nv.name) {
				names = append(names, nv.name)
			}
			res[l][i][nv.name] = append(res[l][i][nv.name], nv.ms)
		}
	}
	for round := range *rounds {
		for i, n := range ns {
			if !*parallel {
				for l := range labels {
					got, err := runPlacement(exePath(*out, labels[l], n), *dir, *bench, *suite, n, *jitEnabled, *micro, nil, &reps)
					if err != nil {
						return err
					}
					add(l, i, got)
				}
				continue
			}
			// Each label on each group as often: the groups swap with the
			// placement and with the round.
			var got [2][]named
			var errs [2]error
			done := make(chan struct{}, 2)
			for l := range labels {
				g := groups[(l+i+round)%2]
				go func() {
					got[l], errs[l] = runPlacement(exePath(*out, labels[l], n), *dir, *bench, *suite, n, *jitEnabled, *micro, g, &reps)
					done <- struct{}{}
				}()
			}
			<-done
			<-done
			for l := range labels {
				if errs[l] != nil {
					return errs[l]
				}
				add(l, i, got[l])
			}
		}
	}
	if *micro {
		reportMicro(os.Stdout, labels, names, res, len(ns), *verbose)
		return nil
	}
	fmt.Printf("%-14s%12s%12s   change  (spread of %s over placements; ms a run)\n", "", labels[0], labels[1], labels[1])
	for _, name := range names {
		var per [2][]float64
		for l := range labels {
			for i := range ns {
				per[l] = append(per[l], slices.Min(res[l][i][name]))
			}
		}
		ma, mb := mean(per[0]), mean(per[1])
		fmt.Printf("%-14s%12.1f%12.1f  %+6.1f%%   %.1f..%.1f\n", name, ma, mb, 100*(mb-ma)/ma,
			slices.Min(per[1]), slices.Max(per[1]))
		if *verbose {
			for l := range labels {
				fmt.Printf("    %s:", labels[l])
				for _, v := range per[l] {
					fmt.Printf(" %.0f", v)
				}
				fmt.Println()
			}
		}
	}
	return nil
}

// longRun is the time, in milliseconds, past which a benchmark's one run
// is its figure; repeats are the runs a shorter one is run again with, as
// many as fill about a second of it (runPlacement).
const longRun = 1000

var repeats = []int{3, 10, 30, 100, 300}

// repsFor is the runs a benchmark whose one run took ms runs again with:
// the fewest of repeats that fill longRun, at most the last, or 0 for one
// that is long already.
func repsFor(ms float64) int {
	if ms >= longRun {
		return 0
	}
	for _, r := range repeats {
		if ms*float64(r) >= longRun {
			return r
		}
	}
	return repeats[len(repeats)-1]
}

type named struct {
	name string
	ms   float64
}

// repsTable is how many runs each benchmark is given again, decided once
// for a comparison: by the first run that timed it, of whichever label and
// placement, and kept for every other. A benchmark whose one run is near a
// boundary of repeats would otherwise be run 10 times by one label and 3
// by the other, and one whose first runs warm up -- Octane's Gameboy,
// about 231 ms a run over 3 and 150 over 10 -- compared by how its warm-up
// weighs, not by its speed.
type repsTable struct {
	mu   sync.Mutex
	reps map[string]int
}

// of is the runs name is given again, decided by ms, its one run, if no
// run has decided it yet.
func (t *repsTable) of(name string, ms float64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reps == nil {
		t.reps = map[string]int{}
	}
	r, ok := t.reps[name]
	if !ok {
		r = repsFor(ms)
		t.reps[name] = r
	}
	return r
}

// runPlacement runs one build and reads its times, in milliseconds a run,
// in the order it printed them, then the total of theirs: every benchmark
// once, then those reps says are short again, in a process for each of
// repeats that one needs. With micro, it runs v8bench's micro mode once,
// whose tests each do the same work every run, and reads each test's time
// of one operation, in nanoseconds, with no total. On cpus, if any, with
// GOMAXPROCS as many.
func runPlacement(exe, dir, bench, suite string, n int, jitEnabled, micro bool, cpus cpuset.Set, reps *repsTable) ([]named, error) {
	run := func(reps int, only string) ([]named, error) {
		args := []string{"-dir", dir, "-n", strconv.Itoa(reps)}
		if micro {
			args = []string{"-mode", "micro", "-n", "1"}
		}
		if bench != "" && !micro {
			args = append(args, "-bench", bench)
		}
		if jitEnabled {
			args = append(args, "-jit")
		}
		if only != "" {
			args = append(args, "-suite", only)
		}
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "V8BENCH_STACKPAD="+strconv.Itoa(5*n), "V8BENCH_JITPAD="+strconv.Itoa(jitPads[n%len(jitPads)]), "V8BENCH_JITSEED="+strconv.Itoa(n+1))
		var outb bytes.Buffer
		cmd.Stdout = &outb
		if cpus != nil {
			cmd.Env = append(cmd.Env, "GOMAXPROCS="+strconv.Itoa(len(cpus)))
			if err := cpuset.Start(cmd, cpus); err != nil {
				return nil, fmt.Errorf("%s: %w", exe, err)
			}
		} else if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("%s: %w", exe, err)
		}
		if err := cmd.Wait(); err != nil {
			return nil, fmt.Errorf("%s: %w", exe, err)
		}
		var got []named
		if micro {
			return parseMicro(outb.String()), nil
		}
		for _, m := range timeLine.FindAllStringSubmatch(outb.String(), -1) {
			if m[1] == "TOTAL" {
				continue
			}
			v, _ := strconv.ParseFloat(m[2], 64)
			got = append(got, named{m[1], v / float64(reps)})
		}
		return got, nil
	}
	got, err := run(1, suite)
	if err != nil || micro {
		return got, err
	}
	// The short ones again, grouped by the runs each needs.
	byReps := map[int][]string{}
	for _, g := range got {
		if r := reps.of(g.name, g.ms); r > 0 {
			byReps[r] = append(byReps[r], g.name)
		}
	}
	for _, r := range repeats {
		names := byReps[r]
		if len(names) == 0 {
			continue
		}
		again, err := run(r, strings.Join(names, ","))
		if err != nil {
			return nil, err
		}
		for _, a := range again {
			if !slices.Contains(names, a.name) {
				continue
			}
			for i := range got {
				if got[i].name == a.name {
					got[i].ms = a.ms
				}
			}
		}
	}
	total := 0.0
	for _, g := range got {
		total += g.ms
	}
	return append(got, named{"TOTAL", total}), nil
}

// jitPads is where each placement has the JIT place its first code in its
// arena (V8BENCH_JITPAD, jit.FirstOffset), which decides how the hot code
// falls on the op cache's sets: AWFY's Towers, its code all native, ran 20%
// faster at some offsets than at others, its ops from the op cache 81%
// against 43% (jit.FirstOffset). Towers' 60 KB fills a 64 KB chunk, so an
// offset of 32 KiB or more puts what comes last in a second chunk: bit 15
// is set at half of the eight, and at half of -k 4's (0, 3, 5, 6), and
// bits 10 to 14 vary.
var jitPads = [8]int{0, 44 << 10, 10 << 10, 33 << 10, 21 << 10, 50 << 10, 27 << 10, 37 << 10}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	if len(xs) == 0 {
		return math.NaN()
	}
	return s / float64(len(xs))
}

// parseMicro reads v8bench's micro mode's output: each test's time of one
// operation, in nanoseconds, in the order it printed them.
func parseMicro(out string) []named {
	var got []named
	for _, m := range microLine.FindAllStringSubmatch(out, -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err == nil {
			got = append(got, named{m[1], v})
		}
	}
	return got
}

// microRow is a test's figures over the placements: each label's mean of
// its placements' best rounds, and at how many placements the second label
// was slower than the first by more than microSlower.
type microRow struct {
	name   string
	a, b   float64
	per    [2][]float64
	slower int
}

// microRows is each test's row, in names' order: res[label][i][test] is
// each round's time of placement i, of which the best is taken.
func microRows(names []string, res [2][]map[string][]float64, placements int) []microRow {
	var rows []microRow
	for _, name := range names {
		row := microRow{name: name}
		complete := true
		for l := range 2 {
			for i := range placements {
				times := res[l][i][name]
				if len(times) == 0 {
					complete = false
					break
				}
				row.per[l] = append(row.per[l], slices.Min(times))
			}
		}
		if !complete {
			continue
		}
		row.a, row.b = mean(row.per[0]), mean(row.per[1])
		for i := range placements {
			if row.per[1][i] > row.per[0][i]*(1+microSlower) {
				row.slower++
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// reportMicro prints each test's mean time of one operation for both
// labels, the change, the second label's spread over the placements and at
// how many it was slower; then the geometric mean of the changes, and the
// tests slower by microReport or more at every placement.
func reportMicro(w io.Writer, labels [2]string, names []string, res [2][]map[string][]float64, placements int, verbose bool) {
	rows := microRows(names, res, placements)
	fmt.Fprintf(w, "%-28s%12s%12s   change  slower at  (spread of %s over placements; ns an operation)\n", "", labels[0], labels[1], labels[1])
	logs := 0.0
	for _, r := range rows {
		fmt.Fprintf(w, "%-28s%12.2f%12.2f  %+6.1f%%  %4d/%-4d  %.2f..%.2f\n", r.name, r.a, r.b, 100*(r.b-r.a)/r.a,
			r.slower, placements, slices.Min(r.per[1]), slices.Max(r.per[1]))
		if verbose {
			for l := range labels {
				fmt.Fprintf(w, "    %s:", labels[l])
				for _, v := range r.per[l] {
					fmt.Fprintf(w, " %.2f", v)
				}
				fmt.Fprintln(w)
			}
		}
		logs += math.Log(r.b / r.a)
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "%d tests, %s / %s %.3f by the geometric mean\n", len(rows), labels[1], labels[0], math.Exp(logs/float64(len(rows))))
	fmt.Fprintf(w, "slower by %.0f%% or more at every placement:\n", 100*microReport)
	for _, r := range rows {
		every := true
		for i := range placements {
			if r.per[1][i] < r.per[0][i]*(1+microReport) {
				every = false
			}
		}
		if every {
			fmt.Fprintf(w, "  %-28s %+6.1f%%\n", r.name, 100*(r.b-r.a)/r.a)
		}
	}
}
