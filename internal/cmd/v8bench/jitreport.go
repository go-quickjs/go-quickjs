//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// V8BENCH_JITPAD is where the JIT places its first code in its arena
// (jit.FirstOffset), and V8BENCH_JITSEED how it varies where it places the
// rest (jit.LayoutSeed).
func init() {
	jit.FirstOffset, _ = strconv.Atoi(os.Getenv("V8BENCH_JITPAD"))
	jit.LayoutSeed, _ = strconv.ParseUint(os.Getenv("V8BENCH_JITSEED"), 10, 64)
}

// jitReport is the report's state: what the JIT had done when the last
// benchmark ended, the counters and each function's.
var jitReport struct {
	stats vm.JITStats
	funcs map[string]vm.JITFunction
}

// reportStart has the runtime's JIT note what it skips, for the report,
// and write its code, for -jitcode.
func reportStart(r runtimeOf) {
	if *jitReportFlag {
		hostaccess.VM(r.rt).SetJITReport(true)
	}
	if *jitCodeFlag != "" && vm.JITCodeHook == nil {
		startCodeDump(*jitCodeFlag)
	}
	if *jitTimeFlag > 0 && mir.Timing == nil {
		mir.Timing = &mir.Times{Clock: func() int64 { return int64(v8bench.Clock()) }}
		vm.JITCompileRepeat = *jitTimeFlag - 1
	}
}

// reportTimes prints, for -jitcompile, how long the compiles since the last
// benchmark took, phase by phase, in milliseconds per run of them all.
func reportTimes() {
	t := mir.Timing
	if t == nil {
		return
	}
	runs := float64(*jitTimeFlag)
	var total int64
	fmt.Printf("  jit compile, ms per run (%d runs, %d compiles each):", *jitTimeFlag, t.Compiles / *jitTimeFlag)
	for p := range mir.NumPhases {
		fmt.Printf(" %s %.2f", p, float64(t.Ticks[p])/runs/1e6)
		total += t.Ticks[p]
	}
	fmt.Printf(" total %.2f\n", float64(total)/runs/1e6)
	*t = mir.Times{Clock: t.Clock}
}

// jitCode is a line of -jitcode's file: a function's native code as it was
// placed -- where it runs (Addr), its bytes, how many of them are
// instructions (Text) -- and what the backend made each stretch of it for
// (mir.Mark), with the SSA it compiled. Seq orders the compiles: code
// flushed leaves its addresses to code placed after it.
type jitCode struct {
	Seq          int
	Name, Script string
	Line         int
	Addr         uint64
	Text         int
	Bytes        []byte
	Marks        []mir.Mark
	Listing      string
	// Spills counts the values the allocation spilled (mir.Code's
	// Spilled).
	Spills int
}

// startCodeDump has the JIT write every function's code to path as it is
// compiled, a JSON line each, until the process exits.
func startCodeDump(path string) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v8bench:", err)
		os.Exit(1)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	mir.Annotate = true
	seq := 0
	vm.JITCodeHook = func(fn vm.JITFunction, text []byte, c *mir.Code) {
		seq++
		rec := jitCode{Seq: seq, Name: fn.Name, Script: fn.Script, Line: fn.Line, Text: c.Text, Bytes: text, Marks: c.Marks, Listing: c.Listing, Spills: c.Spilled()}
		if len(text) > 0 {
			rec.Addr = uint64(uintptr(unsafe.Pointer(&text[0])))
		}
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintln(os.Stderr, "v8bench: -jitcode:", err)
			os.Exit(1)
		}
		// Flushed at each, so that what a run that ends by os.Exit wrote
		// is all there.
		w.Flush()
	}
}

// Report prints what the JIT did while the benchmark ran: its counters,
// then the functions it compiled that were used, the busiest first, and
// those it refused or did not try, with why -- what a benchmark's coverage
// is made of. A function is named by its script and line.
func (r runtimeOf) Report(name string) {
	reportTimes()
	if !*jitReportFlag {
		return
	}
	v := hostaccess.VM(r.rt)
	st := v.JITStats()
	was := jitReport.stats
	fmt.Printf("  jit: %d compiled, %d compiled again, %d entries, %d guard failures, %d exits to Go, %d finished in the interpreter\n",
		st.Compiled-was.Compiled, st.Reoptimized-was.Reoptimized, st.Entries-was.Entries, st.Guards-was.Guards, st.Hosts-was.Hosts, st.Interpreted-was.Interpreted)
	fmt.Printf("  jit: %d dropped to make room, %d compiles refused it, %d of them once compiled; holds %.1f of %.1f MiB\n",
		st.Evicted-was.Evicted, st.Refused-was.Refused, st.Discarded-was.Discarded, float64(st.Held)/(1<<20), float64(st.Budget)/(1<<20))
	jitReport.stats = st
	var used, other []vm.JITFunction
	now := map[string]vm.JITFunction{}
	for _, f := range v.JITReport() {
		key := fmt.Sprintf("%s:%d %s", f.Script, f.Line, f.Name)
		now[key] = f
		before, seen := jitReport.funcs[key]
		switch {
		case f.Status == "native" || f.Status == "slow":
			if f.Native+f.Declined > before.Native+before.Declined || !seen || before.Status != f.Status {
				f.Native -= before.Native
				f.Declined -= before.Declined
				f.Hosts -= before.Hosts
				f.Guards -= before.Guards
				f.Ended -= before.Ended
				used = append(used, f)
			}
		case !seen || before.Status != f.Status:
			other = append(other, f)
		}
	}
	jitReport.funcs = now
	slices.SortStableFunc(used, func(a, b vm.JITFunction) int {
		return cmp.Compare(b.Native+b.Declined, a.Native+a.Declined)
	})
	for i, f := range used {
		if i == 15 {
			fmt.Printf("    ... %d more compiled\n", len(used)-i)
			break
		}
		fmt.Printf("    %-8s %10d native %10d not entered %8d exits  %s:%d %s\n",
			f.Status, f.Native, f.Declined, f.Hosts+f.Guards+f.Ended, f.Script, f.Line, f.Name)
	}
	for _, f := range other {
		fmt.Printf("    %-8s %s:%d %s: %s\n", f.Status, f.Script, f.Line, f.Name, f.Reason)
	}
}
