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
// each with the register tier's hot code placed differently. Six pads, of m
// functions that each fill one 32-byte slot, go before the six pieces of
// hot code, in files the linker places there, in this order:
//
//	jit_leaf_pad.go       before jit_leaffast.go             jitLeafGo
//	native_r_pad.go       before native_run.go               nativeDrive
//	reg_ru_pad.go         before reg_run.go                  runReg
//	reg_run_b_pad.go      after reg_run.go, before calls     rhCallW
//	vm_b_pad.go           after vm_amath.go, before vm_call.go callObject
//	zz_pad.go             closures, after every other one    regNativeHandlers
//
// callObject is the calls of functions and built-ins; jitLeafGo the JIT's
// calls of Go leaves (built with the JIT alone: a build without it has no
// such target, and its pad moves what follows); runReg the Go table's loop;
// rhCallW, with regEnterW after it, the register calls; nativeDrive the
// native dispatcher's Go loop, after nativeLoop in its file; and the last
// the assembly after the
// closures -- the JIT's leaf stubs and the native dispatcher's entry, its
// exit's call of Go and its handlers, regNativeHandlers. The assembly is
// laid out after the package's closures, so its pad is closures, function
// literals of the last file's, laid out after every other closure.
// regRunFD and regCallSlow (reg_call.go) move with nativeDrive's pad.
//
// A pad moves what follows it by exactly 32*m bytes. For each placement a
// build with the pads empty says where the six are, and m is chosen so that
// each lands at the phase -- its start mod 64, the one bit a placement can
// vary -- that a 2^(6-3) design gives it: over the eight, each is at 0 and
// at 32 mod 64 four times, each pair of them in each combination twice,
// and each at a distance from the one before it that varies by up to 2 KB.
// The data's phase (dataPhase) is the design's seventh column. Every build
// is checked with go tool nm, and one whose phases are not the design's
// fails. The pads are removed when it is done. What the placements do not
// vary is the layout inside the native handlers' block, which nativegen
// makes as it is.
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
// regression of a later benchmark at all eight. Those move code by whole
// 64-byte lines; placement n also starts each function's code jitLines[n]
// bytes into its line (V8BENCH_JITLINE), where every placement had put
// each JIT loop at the start of one. Each
// benchmark runs once, and one whose run took less than a second runs
// again in a process of its own as many times as fill about one -- 3, 10,
// 30, 100 or 300 -- so that a long one, Octane's zlib, takes its run's
// time and no more, and a short one, the V8 suite's Richards, more than
// the timer's grain.
//
// -k 4 takes placements 0, 3, 5 and 6 of the eight, over which runReg,
// rhCallW and the native block are each at each phase twice and each pair
// of them in each combination once; callObject is at one phase, and the
// rest each follows one of those three (design): a comparison to screen a
// change with, in half the time, before the eight confirm it.
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
                     [-parallel] [-cpus auto|halves|LIST;LIST] [-v] [-out DIR] A B
  (a label NAME@VAR=VALUE runs NAME's build with VAR=VALUE in its environment)`)
	os.Exit(2)
}

// labelExe is placement n of a compared label's build and what its runs add
// to their environment: a label NAME@VAR=VALUE[@VAR=VALUE] runs NAME's
// build with those variables, so that one build can be compared with
// itself under another setting, as QJS_NONATIVE=1 runs the Go table in
// place of the native handlers.
func labelExe(out, label string, n int) (string, []string) {
	parts := strings.Split(label, "@")
	return exePath(out, parts[0], n), parts[1:]
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
	pads = [nPads]string{"internal/vm/vm_b_pad.go", "internal/vm/jit_leaf_pad.go", "internal/vm/reg_ru_pad.go",
		"internal/vm/reg_run_b_pad.go", "internal/vm/native_r_pad.go", "internal/vm/zz_pad.go"}
	targets = [nPads]string{"(*Runtime).callObject", "jitLeafGo", "(*Runtime).runReg",
		"rhCallW", "(*Runtime).nativeDrive", "regNativeHandlers.abi0"}
	// optional are the targets a build may lack: the JIT's leaf calls,
	// without the JIT, and the native handlers, on an architecture that
	// has the Go table alone.
	optional = [nPads]bool{1: true, 5: true}
	// order is the pads' indexes in the order their files sort, which is
	// the order the linker lays them out in. An index is a target's
	// column of the design and its pad's even part (padCounts), whatever
	// its place in the order.
	order = [nPads]int{1, 4, 2, 3, 0, 5}
)

// nPads is how many pads and targets there are; closurePad is the pad
// made of closures (padFile).
const (
	nPads      = 6
	closurePad = 5
)

// placementsOf is the placements -k takes: placements 0, 3, 5 and 6 for
// four, the half of the design whose three bits' sum is even (design);
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
// target: a 2^(6-3) design over n's three bits b0, b1 and b2, saturated
// with the data's phase (dataPhase), so that no two of the seven columns
// are the same or opposite over the eight and each pair is in each
// combination twice. runReg is b0, rhCallW b1 and the native block b2,
// which -k 4's half, where b0^b1^b2 is 0, keeps apart; there callObject,
// b0^b1^b2, is at one phase, jitLeafGo, b0^b1, follows the native block,
// nativeDrive, b1^b2, runReg, and the data, b0^b2, rhCallW.
func design(n int) [nPads]int {
	b0, b1, b2 := n&1, (n>>1)&1, (n>>2)&1
	return [nPads]int{b0 ^ b1 ^ b2, b0 ^ b1, b0, b1, b1 ^ b2, b2}
}

// padSlots is the most functions a pad has: 2*31 and one.
const padSlots = 64

// dataPhase is how many bytes placement n adds to the initialized data,
// which moves the zeroed data after it -- the Go runtime's hot variables
// among it, runtime.writeBarrier and gcController -- within their cache
// lines: 0 or 32, half the placements each, as the code's phases are
// balanced, by the design's last column, b0^b2. Builds of two revisions
// differ in their data as well as their code -- a few package variables,
// the build's own information (a tree with changes records it, 32 bytes
// more) -- which no placement had varied: where a variable fell in its
// cache line had shown, for a collector-bound benchmark, as a difference
// at every placement.
func dataPhase(n int) int {
	return 32 * (n&1 ^ (n>>2)&1)
}

// padDataBase is padData's size at a phase of 0: big enough to be laid
// out on a 32-byte boundary at either size.
const padDataBase = 64

// padCounts is how many one-slot functions each pad needs for placement
// n, given where the targets are with the pads empty. Each pad moves its
// target and everything after it, and the targets come in the pads'
// order, so each is chosen given the ones before; a pad is an even number
// of slots, varying with n and the pad, plus the one that sets its
// target's phase. A target the build lacks (0 in ref) takes the even part
// alone.
func padCounts(ref [nPads]uint64, n int) [nPads]int {
	want := design(n)
	var counts [nPads]int
	shift := 0
	for _, i := range order {
		m := 2 * ((n*37 + i*11) % 32)
		if ref[i] != 0 {
			phase := int((ref[i]/32 + uint64(shift)) % 2)
			m += ((want[i]-phase)%2 + 2) % 2
		}
		counts[i] = m
		shift += m
	}
	return counts
}

// phases is each target's start, in units of 32 bytes, mod 2.
func phases(addrs [nPads]uint64) [nPads]int {
	var p [nPads]int
	for i, a := range addrs {
		p[i] = int(a/32) % 2
	}
	return p
}

// phasesMatch reports whether the targets at addrs are at the design's
// phases for placement n, each that the build has.
func phasesMatch(addrs [nPads]uint64, n int) bool {
	got, want := phases(addrs), design(n)
	for i := range nPads {
		if addrs[i] != 0 && got[i] != want[i] {
			return false
		}
	}
	return true
}

func buildCmd(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	label := fs.String("label", "", "the name the builds are kept under")
	k := fs.Int("k", 8, "how many placements")
	out := fs.String("out", defaultOut(), "where the builds go")
	tags := fs.String("tags", "", "build tags, as go build's -tags: quickjs_jit for the JIT (GOFLAGS' are taken too)")
	fs.Parse(args)
	if *label == "" {
		return errors.New("-label is required")
	}
	if *k < 1 || *k > 8 {
		return errors.New("-k is from 1 to 8, the design's placements")
	}
	if _, err := os.Stat("internal/vm/vm_call.go"); err != nil {
		return fmt.Errorf("%w: run from the repository's root", err)
	}
	for _, p := range pads {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists: a build was interrupted? remove it", p)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}

	// Whatever happens, the pads go. An interrupt does the same before it
	// ends the program.
	restore := func() {
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
		ref, err := buildAt([nPads]int{}, dataPhase(n), *tags, exe)
		if err != nil {
			return err
		}
		counts := padCounts(ref, n)
		got, err := buildAt(counts, dataPhase(n), *tags, exe)
		if err != nil {
			return err
		}
		if !phasesMatch(got, n) {
			return fmt.Errorf("placement %d: phases %v, want %v (pads %v)", n, phases(got), design(n), counts)
		}
		fmt.Printf("%s-%d: pads %v, phases %v, at %s\n", *label, n, counts, phases(got), mod4K(got))
	}
	fmt.Printf("built %d placements of %s in %s\n", *k, *label, *out)
	return nil
}

// mod4K is the targets' addresses mod 4,096, the offsets Zen 2's op cache
// sets by, "-" for one the build lacks.
func mod4K(addrs [nPads]uint64) string {
	parts := make([]string, nPads)
	for i, a := range addrs {
		parts[i] = "-"
		if a != 0 {
			parts[i] = fmt.Sprintf("%#x", a%4096)
		}
	}
	return strings.Join(parts, " ")
}

// buildAt writes the pads, builds v8bench to exe, and says where the
// targets are in it, 0 for one the build lacks.
func buildAt(counts [nPads]int, data int, tags, exe string) ([nPads]uint64, error) {
	var addrs [nPads]uint64
	for p, m := range counts {
		if err := os.WriteFile(pads[p], padFile(p, m, data), 0o644); err != nil {
			return addrs, err
		}
	}
	// Built as the layout lock's targets are measured
	// (internal/cmd/layoutpad): without VCS stamping, whose build
	// information is longer by a tree with changes -- the pads make one --
	// and moves the data after it, and without cgo.
	args := []string{"build", "-buildvcs=false"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	cmd := exec.Command("go", append(args, "-o", exe, "./internal/cmd/v8bench")...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return addrs, fmt.Errorf("go build: %w", err)
	}
	nm, err := exec.Command("go", "tool", "nm", exe).Output()
	if err != nil {
		return addrs, fmt.Errorf("go tool nm: %w", err)
	}
	return targetAddrs(nm)
}

// targetAddrs is where go tool nm's output says the targets are, 0 for an
// optional one the build lacks.
func targetAddrs(nm []byte) ([nPads]uint64, error) {
	var addrs [nPads]uint64
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
				addrs[i] = a
			}
		}
	}
	for i, a := range addrs {
		if a == 0 && !optional[i] {
			return addrs, fmt.Errorf("%s is not in the build: has it been renamed?", targets[i])
		}
	}
	return addrs, nil
}

// padFile is pad p with m functions. The first in the order holds the one
// init, which keeps every pad's functions reachable, and the data's pad:
// it is before the first target, where its size is the same in every
// placement. The
// closures' pad (closurePad) is m function literals, closures laid out
// after every other closure of the package and before its assembly.
func padFile(p, m, data int) []byte {
	var b strings.Builder
	b.WriteString("package vm\n\n")
	if p == order[0] {
		b.WriteString("var padSink int\n\n")
		// The data's phase: the initialized data, which the zeroed data
		// follows, data bytes more (dataPhase).
		fmt.Fprintf(&b, "var padData = [%d]byte{1}\n\n", padDataBase+data)
		var all []string
		for q := range nPads {
			all = append(all, fmt.Sprintf("padFuncs%d[:]", q))
		}
		fmt.Fprintf(&b, "func init() {\n\tpadSink += int(padData[len(padData)-1])\n"+
			"\tfor _, fs := range [][]func() int{%s} {\n"+
			"\t\tfor _, f := range fs {\n\t\t\tif f != nil {\n\t\t\t\tpadSink += f()\n\t\t\t}\n\t\t}\n\t}\n}\n\n",
			strings.Join(all, ", "))
	}
	if p == closurePad {
		// Function literals in the array's initializer, as below an array
		// of padSlots whatever m is: data, which no init's code makes,
		// so that what grows with m is the closures alone, laid out
		// after every other closure. Assigned in an init, the init had
		// grown with them, and moved the assembly by more than 32*m.
		lits := make([]string, m)
		for i := range m {
			lits[i] = fmt.Sprintf("func() int { return padSink*3 + %d }", i+1)
		}
		fmt.Fprintf(&b, "var padFuncs%d = [%d]func() int{%s}\n", p, padSlots, strings.Join(lits, ", "))
		return []byte(b.String())
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
	jitBackground := fs.Bool("jitbackground", false, "with -jit, compile in the background, as qjs --jit does")
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
			exe, _ := labelExe(*out, labels[l], n)
			if _, err := os.Stat(exe); err != nil {
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
					got, err := runPlacement(labels[l], *out, *dir, *bench, *suite, n, *jitEnabled, *jitBackground, *micro, nil, &reps)
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
					got[l], errs[l] = runPlacement(labels[l], *out, *dir, *bench, *suite, n, *jitEnabled, *jitBackground, *micro, g, &reps)
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
					fmt.Printf(" %.2f", v)
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
func runPlacement(label, out, dir, bench, suite string, n int, jitEnabled, jitBackground, micro bool, cpus cpuset.Set, reps *repsTable) ([]named, error) {
	exe, env := labelExe(out, label, n)
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
		if jitEnabled && jitBackground {
			args = append(args, "-jitbackground")
		}
		if only != "" {
			args = append(args, "-suite", only)
		}
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "V8BENCH_STACKPAD="+strconv.Itoa(5*n), "V8BENCH_JITPAD="+strconv.Itoa(jitPads[n%len(jitPads)]), "V8BENCH_JITSEED="+strconv.Itoa(n+1), "V8BENCH_JITLINE="+strconv.Itoa(jitLines[n%len(jitLines)]))
		cmd.Env = append(cmd.Env, env...)
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

// jitLines is how many bytes into its 64-byte line each placement starts
// each JIT function's code (V8BENCH_JITLINE, jit.LineOffset). JITPAD and
// the seeds move code by whole lines, and the backend starts each loop's
// header a line, so every placement had had each loop at offset 0 of its
// line, its branches and fused pairs where they fall from there, in the
// same 32-byte fetch windows: a change that was 5% slower than its base on
// bigint64_fnv1a at every placement was 10% faster at four of eight such
// offsets and 3 to 5% slower at three, 0 among them. The eight are 0 to 56
// by 8; -k 4's (0, 3, 5, 6) are 0, 16, 32 and 48, each half of a 32-byte
// window twice; -k 2's are 0 and 40.
var jitLines = [8]int{0, 40, 8, 16, 24, 32, 48, 56}

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
