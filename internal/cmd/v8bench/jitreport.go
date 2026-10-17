//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/jit"
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

// reportStart has the runtime's JIT note what it skips, for the report.
func reportStart(r runtimeOf) {
	if *jitReportFlag {
		hostaccess.VM(r.rt).SetJITReport(true)
	}
}

// Report prints what the JIT did while the benchmark ran: its counters,
// then the functions it compiled that were used, the busiest first, and
// those it refused or did not try, with why -- what a benchmark's coverage
// is made of. A function is named by its script and line.
func (r runtimeOf) Report(name string) {
	if !*jitReportFlag {
		return
	}
	v := hostaccess.VM(r.rt)
	st := v.JITStats()
	was := jitReport.stats
	fmt.Printf("  jit: %d compiled, %d entries, %d guard failures, %d exits to Go, %d finished in the interpreter\n",
		st.Compiled-was.Compiled, st.Entries-was.Entries, st.Guards-was.Guards, st.Hosts-was.Hosts, st.Interpreted-was.Interpreted)
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
