//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
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
	stats               vm.JITStats
	flushed, recompiled uint64
	funcs               map[string]vm.JITFunction
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
	if *jitHeldFlag && vm.JITCompileRepeat == 0 {
		// Held for the first runtime: the arm64 compiles are its repeats.
		debug.SetGCPercent(-1)
		vm.JITHoldClocks()
		vm.JITCompileRepeat = 1
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
	if n := runs - 1; n > 0 {
		fmt.Printf("  jit compile, heap per run: %.2f MB, %.0f objects\n", float64(t.Bytes)/n/(1<<20), float64(t.Objects)/n)
	}
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
	// Arch is arm64 for what -jitheld's repeated compile made for arm64
	// (vm.JITCrossHook), which has no Marks, and otherwise empty. States
	// hashes what the code's calls and exits are described by, and its
	// entries, with addresses left out (codeStates).
	Arch   string `json:",omitempty"`
	States string
}

// exitView is what an exit description says, however it is kept: its
// inlined callees' frames listed in it, as they are written, not where
// the frame the descriptions share keeps them (abi.ExitDescriptor's
// Inlines).
type exitView struct {
	Kind, PC, Depth     uint32
	Site                int32
	Frame               *exitFrameView
	Start               uint32
	Count               uint16
	LocalsReg, StackReg uint8
	Direct              bool
	Inline              []abi.ExitInline
}

// exitFrameView is the frame an exitView's slots are of, without the
// inlined callees' frames, which each view lists.
type exitFrameView struct {
	FrameLocals, ThisSlot, Locals int32
	Enc                           *abi.Encoding
	Slots                         []abi.ExitSlot
	Index                         []uint16
	Words                         []uint64
}

// exitViews are c's exit descriptions as exitViews.
func exitViews(exits []*abi.ExitDescriptor) []exitView {
	views := make([]exitView, len(exits))
	frames := map[*abi.ExitFrame]*exitFrameView{}
	for i, d := range exits {
		fv := frames[d.Frame]
		if fv == nil && d.Frame != nil {
			f := d.Frame
			fv = &exitFrameView{f.FrameLocals, f.ThisSlot, f.Locals, f.Enc, f.Slots, f.Index, f.Words}
			frames[d.Frame] = fv
		}
		views[i] = exitView{d.Kind, d.PC, d.Depth, d.Site, fv, d.Start, d.Count, d.LocalsReg, d.StackReg, d.Direct, d.Inlines()}
	}
	return views
}

// goCallView is what a call of Go's state says of the call: not the
// VM's own (LeafForm, which the VM makes at the call's first, and which
// JSON has no form for: every code with a call of Go had hashed to the
// error's text), nor the encoding's constants, which every call shares.
type goCallView struct {
	Op, Keep  uint64
	PC, Depth uint32
	SetsPC    bool
	Fail      int32
	Args      []abi.GoCallArg
	BigScore  int32
	BigOff    bool
	LeafSeen  bool
	Inline    uintptr
	InlinePC  uint32
	Leaf      uintptr
	Unused    bool
}

// goCallViews are c's calls of Go as goCallViews.
func goCallViews(calls []*abi.GoCallState) []goCallView {
	views := make([]goCallView, len(calls))
	for i, st := range calls {
		views[i] = goCallView{st.Op, st.Keep, st.PC, st.Depth, st.SetsPC, st.Fail, st.Args, st.BigScore, st.BigOff, st.LeafSeen, st.Inline, st.InlinePC, st.Leaf, st.Unused}
	}
	return views
}

// codeStates is a hash of what c's code is described by. Numbers that may
// be addresses are left out, and an empty list is a nil one.
func codeStates(c *mir.Code) string {
	b, err := json.Marshal(struct {
		Entries                     map[int]int
		CallEntry, Resume, GenEntry int
		CallRefs                    int
		Calls                       []*abi.CallState
		GoCalls                     []goCallView
		Exits                       []exitView
	}{c.Entries, c.CallEntry, c.Resume, c.GenEntry, c.CallRefs, c.Calls, goCallViews(c.GoCalls), exitViews(c.Exits)})
	if err != nil {
		return err.Error()
	}
	s := addressLike.ReplaceAllFunc(b, func(n []byte) []byte {
		if v, err := strconv.ParseUint(string(n), 10, 64); err == nil && v >= 1<<32 && v < 1<<47 {
			return []byte("0")
		}
		return n
	})
	h := fnv.New64a()
	h.Write(bytes.ReplaceAll(s, []byte("null"), []byte("[]")))
	return strconv.FormatUint(h.Sum64(), 16)
}

var addressLike = regexp.MustCompile(`[0-9]{10,15}`)

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
		if *jitHeldFlag {
			rec.States = codeStates(c)
		}
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
	if *jitHeldFlag {
		vm.JITCrossHook = func(fn vm.JITFunction, c *mir.Code, err error) {
			seq++
			rec := jitCode{Seq: seq, Name: fn.Name, Script: fn.Script, Line: fn.Line, Arch: "arm64"}
			if err != nil {
				rec.Listing = err.Error()
			} else {
				rec.Text, rec.Bytes, rec.States = c.Text, c.Bytes, codeStates(c)
			}
			if err := enc.Encode(rec); err != nil {
				fmt.Fprintln(os.Stderr, "v8bench: -jitcode:", err)
				os.Exit(1)
			}
			w.Flush()
		}
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
	fmt.Printf("  jit: %d callees inlined, %d instructions of them\n", st.Inlined-was.Inlined, st.InlinedCode-was.InlinedCode)
	flushed, recompiled := v.JITCacheCounts()
	fmt.Printf("  jit: %d dropped to make room, %d compiles refused it, %d of them once compiled; %d flushed unused, %d compiles of code dropped; holds %.1f of %.1f MiB\n",
		st.Evicted-was.Evicted, st.Refused-was.Refused, st.Discarded-was.Discarded, flushed-jitReport.flushed, recompiled-jitReport.recompiled,
		float64(st.Held)/(1<<20), float64(st.Budget)/(1<<20))
	jitReport.stats, jitReport.flushed, jitReport.recompiled = st, flushed, recompiled
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
