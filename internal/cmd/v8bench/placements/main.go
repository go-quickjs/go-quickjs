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
		ref, err := buildAt(src, [4]int{}, inner*n, exe)
		if err != nil {
			return err
		}
		counts := padCounts(ref, n)
		got, err := buildAt(src, counts, inner*n, exe)
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
func buildAt(src []byte, counts [4]int, tests int, exe string) ([4]uint64, error) {
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
		if err := os.WriteFile(pads[p], padFile(p, m, tests), 0o644); err != nil {
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
func padFile(p, m, tests int) []byte {
	var b strings.Builder
	b.WriteString("package vm\n\n")
	if p == 0 {
		fmt.Fprintf(&b, "var padSink int\n\nvar padKeys = [%d]uint32{}\n\n", max(tests, 1))
		b.WriteString("func init() {\n\tfor i := range padKeys {\n\t\tpadKeys[i] = ^uint32(i)\n\t}\n" +
			"\tfor _, fs := range [][]func() int{padFuncs0, padFuncs1, padFuncs2, padFuncs3} {\n" +
			"\t\tfor _, f := range fs {\n\t\t\tpadSink += f()\n\t\t}\n\t}\n}\n\n")
	}
	names := make([]string, m)
	for i := range m {
		names[i] = fmt.Sprintf("padF%d_%d", p, i)
		// A constant-return leaf occupies only 16 bytes on arm64. Loading a
		// global and computing from it fills a 32-byte slot on both CPUs.
		fmt.Fprintf(&b, "//go:noinline\nfunc %s() int { return padSink*3 + %d }\n\n", names[i], i+1)
	}
	fmt.Fprintf(&b, "var padFuncs%d = []func() int{%s}\n", p, strings.Join(names, ", "))
	return []byte(b.String())
}

var timeLine = regexp.MustCompile(`(?m)^(\w+)\s+([\d.]+) ms`)

func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	dir := fs.String("dir", "", "the directory the suite is in")
	k := fs.Int("k", 8, "how many placements")
	rounds := fs.Int("rounds", 3, "runs of each placement of each label")
	suite := fs.String("suite", "", "comma-separated suites to run, all by default")
	bench := fs.String("bench", "", "the benchmark collection, as v8bench's -bench: v8 by default, octane, awfy")
	jitEnabled := fs.Bool("jit", false, "enable the optional native tier in both tagged builds")
	parallel := fs.Bool("parallel", false, "run both labels at once, each on a group of CPUs (-cpus)")
	cpus := fs.String("cpus", "auto", "the groups of CPUs -parallel runs on: auto, halves, or lists separated by ';' (0-5;6-11)")
	verbose := fs.Bool("v", false, "print each placement's figure")
	out := fs.String("out", defaultOut(), "where the builds are")
	fs.Parse(args)
	if *dir == "" || fs.NArg() != 2 {
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
					got, err := runPlacement(exePath(*out, labels[l], n), *dir, *bench, *suite, n, *jitEnabled, nil)
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
					got[l], errs[l] = runPlacement(exePath(*out, labels[l], n), *dir, *bench, *suite, n, *jitEnabled, g)
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

// runPlacement runs one build and reads its times, in milliseconds a run,
// in the order it printed them, then the total of theirs: every benchmark
// once, then those shorter than longRun again, in a process for each of
// repeats that one needs. On cpus, if any, with GOMAXPROCS as many.
func runPlacement(exe, dir, bench, suite string, n int, jitEnabled bool, cpus cpuset.Set) ([]named, error) {
	run := func(reps int, only string) ([]named, error) {
		args := []string{"-dir", dir, "-n", strconv.Itoa(reps)}
		if bench != "" {
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
	if err != nil {
		return nil, err
	}
	// The short ones again, grouped by the runs each needs.
	byReps := map[int][]string{}
	for _, g := range got {
		if reps := repsFor(g.ms); reps > 0 {
			byReps[reps] = append(byReps[reps], g.name)
		}
	}
	for _, reps := range repeats {
		names := byReps[reps]
		if len(names) == 0 {
			continue
		}
		again, err := run(reps, strings.Join(names, ","))
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
