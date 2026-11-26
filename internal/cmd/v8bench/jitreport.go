//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"time"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// V8BENCH_JITPAD is where the JIT places its first code in its arena
// (jit.FirstOffset), V8BENCH_JITSEED how it varies where it places the
// rest (jit.LayoutSeed), and V8BENCH_JITLINE how far into its cache line
// each function's code starts (jit.LineOffset).
func init() {
	jit.FirstOffset, _ = strconv.Atoi(os.Getenv("V8BENCH_JITPAD"))
	jit.LayoutSeed, _ = strconv.ParseUint(os.Getenv("V8BENCH_JITSEED"), 10, 64)
	jit.LineOffset, _ = strconv.Atoi(os.Getenv("V8BENCH_JITLINE"))
}

// jitHeldLimit is the memory limit -jitheld runs at where GOMEMLIMIT sets
// none: the collector off, a heap grows without bound, and at the limit
// it collects. Two runs of one build at 3 GiB make the same compiles,
// Octane's and Are We Fast Yet's, which pass it.
const jitHeldLimit = 3 << 30

// jitReport is the report's state: what the JIT had done when the last
// benchmark ended, the counters and each function's, in the runtime it ran
// in (rt). A benchmark run in another runtime -- ssr's Render and fresh's
// benchmarks each make their own -- is counted from nothing: counted from
// what the last one's runtime had done, its counters had come out negative,
// ssr's Render's host exits at get_prop -24 after FirstRender.
var jitReport jitReportState

type jitReportState struct {
	rt                 weak.Pointer[vm.Runtime]
	stats              vm.JITStats
	flushed            uint64
	funcs              map[string]vm.JITFunction
	hostOps            map[string]uint64
	goCallOps          map[string]uint64
	nested, levels     uint64
	levelsMade, putOff uint64
	ended              map[string]uint64
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
	if *jitTierFlag != "" && vm.JITTierHook == nil {
		startTierTrace(*jitTierFlag)
	}
	if *jitHeldFlag && vm.JITCompileRepeat == 0 {
		// Held for the first runtime: the arm64 compiles are its repeats.
		debug.SetGCPercent(-1)
		if debug.SetMemoryLimit(-1) == math.MaxInt64 {
			// GOMEMLIMIT sets none: the collector off held Octane's Game
			// Boy at a 6.75 GB heap, 159 MB of it live, where a limit
			// collects only near it.
			debug.SetMemoryLimit(jitHeldLimit)
		}
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
	// Arch is arm64 for native arm64 code and what -jitheld's repeated
	// compile made for arm64 (vm.JITCrossHook), and otherwise empty. States
	// hashes what the code's calls and exits are described by, and its
	// entries, with addresses left out (codeStates).
	Arch   string `json:",omitempty"`
	States string
}

// exitView is what an exit description says, however it is kept: its
// slots and its inlined callees' frames listed in it, as they are
// written, not where the frame the descriptions share keeps them
// (abi.ExitDescriptor's Slots and Inlines).
type exitView struct {
	Kind, PC, Depth     uint32
	Site                int32
	Frame               *exitFrameView
	Slots               []exitSlotView
	LocalsReg, StackReg uint8
	Direct              bool
	Inline              []abi.ExitInline
}

// exitFrameView is the layout of the frame an exitView's slots are of.
// The slots are listed in each view, as the exit writes them, however the
// frame keeps them.
type exitFrameView struct {
	FrameLocals, ThisSlot, Locals int32
	Enc                           *abi.Encoding
}

// exitViews are c's exit descriptions as exitViews.
func exitViews(exits []*abi.ExitDescriptor) []exitView {
	views := make([]exitView, len(exits))
	frames := map[*abi.ExitFrame]*exitFrameView{}
	for i, d := range exits {
		fv := frames[d.Frame]
		f := d.Frame
		if fv == nil && f != nil {
			fv = &exitFrameView{f.FrameLocals, f.ThisSlot, f.Locals, f.Enc}
			frames[d.Frame] = fv
		}
		var slots []exitSlotView
		d.Slots(func(e *abi.ExitSlot) { slots = append(slots, exitSlotOf(f, e)) })
		views[i] = exitView{d.Kind, d.PC, d.Depth, d.Site, fv, slots, d.LocalsReg, d.StackReg, d.Direct, d.Inlines()}
	}
	return views
}

// callView is what a native call's state says, as exitView an exit's:
// the frame's layout, and the records' constant words where they are.
type callView struct {
	PC, Depth uint32
	Site      int32
	Frame     *exitFrameView
	Records   []callRecordView
}

// callRecordView is a call's record, its constant word in it.
type callRecordView struct {
	abi.CallRecord
	NumWord uint64
}

// callViews are calls as callViews.
func callViews(calls []*abi.CallState) []callView {
	views := make([]callView, len(calls))
	for i, st := range calls {
		v := callView{PC: st.PC, Depth: st.Depth, Site: st.Site}
		if f := st.Frame; f != nil {
			v.Frame = &exitFrameView{f.FrameLocals, f.ThisSlot, f.Locals, f.Enc}
		}
		for _, r := range st.Records {
			rv := callRecordView{CallRecord: r}
			if r.Num.Kind == abi.ExitConst && r.Num.N >= 0 && st.Frame != nil {
				rv.NumWord, rv.Num.N = st.Frame.Words[r.Num.N], 0
			}
			v.Records = append(v.Records, rv)
		}
		views[i] = v
	}
	return views
}

// exitSlotView is a slot an exit writes, its constant words in it.
type exitSlotView struct {
	abi.ExitSlot
	ValueWord, ShadowWord uint64
}

// exitSlotOf is e as an exitSlotView: a constant's word where it is, not
// its index among the frame's words.
func exitSlotOf(f *abi.ExitFrame, e *abi.ExitSlot) exitSlotView {
	v := exitSlotView{ExitSlot: *e}
	word := func(l *abi.ExitLoc) uint64 {
		if l.Kind != abi.ExitConst || l.N < 0 {
			return 0
		}
		w := f.Words[l.N]
		l.N = 0
		return w
	}
	v.ValueWord, v.ShadowWord = word(&v.Value), word(&v.Shadow)
	return v
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
		Calls                       []callView
		GoCalls                     []goCallView
		Exits                       []exitView
	}{c.Entries, c.CallEntry, c.Resume, c.GenEntry, c.CallRefs, callViews(c.Calls), goCallViews(c.GoCalls), exitViews(c.Exits)})
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
func startCodeDump(path string) *os.File {
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
		if runtime.GOARCH == "arm64" {
			rec.Arch = "arm64"
			// arm64 has no constant pool after Text. Arena alignment is
			// not code, and may differ with the addresses in the code.
			rec.Bytes = text[:c.Text]
		}
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
	return f
}

// Report prints what the JIT did while the benchmark ran: its counters,
// then the functions it compiled that were used, the busiest first, and
// those it refused or did not try, with why -- what a benchmark's coverage
// is made of. A function is named by its script and line.
func (r runtimeOf) Report(name string) {
	reportTimes()
	reportTier(r)
	// The register tier's crossings, where QJS_REGREPORT asks for them.
	vm.RegReport()
	if !*jitReportFlag {
		return
	}
	v := hostaccess.VM(r.rt)
	st := v.JITStats()
	if was := jitReport.stats; jitReport.rt.Value() != v || st.Compiled < was.Compiled || st.Entries < was.Entries {
		// Another runtime, or one whose JIT was let go of and made
		// again: counted from nothing.
		jitReport = jitReportState{rt: weak.Make(v)}
	}
	was := jitReport.stats
	fmt.Printf("  jit: %d compiled, %d compiled again, %d entries, %d guard failures, %d exits to Go, %d finished in the interpreter\n",
		st.Compiled-was.Compiled, st.Reoptimized-was.Reoptimized, st.Entries-was.Entries, st.Guards-was.Guards, st.Hosts-was.Hosts, st.Interpreted-was.Interpreted)
	fmt.Printf("  jit: %d callees inlined, %d instructions of them\n", st.Inlined-was.Inlined, st.InlinedCode-was.InlinedCode)
	flushed := v.JITCacheCounts()
	fmt.Printf("  jit: %d dropped to make room, %d compiles refused it; %d flushed unused; holds %.1f of %.1f MiB\n",
		st.Evicted-was.Evicted, st.Refused-was.Refused, flushed-jitReport.flushed,
		float64(st.Held)/(1<<20), float64(st.Budget)/(1<<20))
	fmt.Printf("  jit: %d contexts, %d at most, given back %d times\n", st.Contexts, st.ContextsMost, st.ContextsShrunk-was.ContextsShrunk)
	fmt.Printf("  jit: parked %d times as the runtime returned to its host, %d functions' code, %d put back as it was\n",
		st.Parkings-was.Parkings, st.Parked-was.Parked, st.Unparked-was.Unparked)
	jitReport.stats, jitReport.flushed = st, flushed
	// Register code's host exits by the register instruction Go ran
	// whole, the most first.
	rs := v.RegNativeStats()
	type hostOp struct {
		op string
		n  uint64
	}
	var ops []hostOp
	for k, n := range rs.HostOps {
		if d := n - jitReport.hostOps[k]; d != 0 {
			ops = append(ops, hostOp{k, d})
		}
	}
	jitReport.hostOps = rs.HostOps
	slices.SortFunc(ops, func(a, b hostOp) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.op, b.op)) })
	if len(ops) > 8 {
		ops = ops[:8]
	}
	if len(ops) != 0 {
		fmt.Printf("  jit: register host exits:")
		for _, o := range ops {
			fmt.Printf(" %s %d", o.op, o.n)
		}
		fmt.Println()
	}
	// Its calls of Go that may run script, by the register instruction
	// and what it called, the most first.
	ops = ops[:0]
	for k, n := range rs.GoCallOps {
		if d := n - jitReport.goCallOps[k]; d != 0 {
			ops = append(ops, hostOp{k, d})
		}
	}
	jitReport.goCallOps = rs.GoCallOps
	slices.SortFunc(ops, func(a, b hostOp) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.op, b.op)) })
	if len(ops) > 8 {
		ops = ops[:8]
	}
	if len(ops) != 0 {
		fmt.Printf("  jit: register calls of Go:")
		for _, o := range ops {
			fmt.Printf(" %s %d;", o.op, o.n)
		}
		fmt.Println()
	}
	// Its calls of Go from native callees' code, and the levels of native
	// calls Go found the code of: each such call's calling level, and the
	// levels it unwinds.
	if n := rs.NestedGoCalls - jitReport.nested; n != 0 {
		fmt.Printf("  jit: register calls of Go from native callees %d, levels found %d, put off %d, made for a read %d times\n",
			n, rs.Levels-jitReport.levels, rs.LevelsPutOff-jitReport.putOff, rs.LevelsMade-jitReport.levelsMade)
	}
	jitReport.nested, jitReport.levels, jitReport.levelsMade, jitReport.putOff = rs.NestedGoCalls, rs.Levels, rs.LevelsMade, rs.LevelsPutOff
	// The runs of register code that finished in the interpreter, by why
	// they ended there, where they left and in what function, the most
	// first.
	ops = ops[:0]
	for k, n := range rs.Ended {
		if d := n - jitReport.ended[k]; d != 0 {
			ops = append(ops, hostOp{k, d})
		}
	}
	jitReport.ended = rs.Ended
	slices.SortFunc(ops, func(a, b hostOp) int { return cmp.Or(cmp.Compare(b.n, a.n), cmp.Compare(a.op, b.op)) })
	if len(ops) > 8 {
		ops = ops[:8]
	}
	if len(ops) != 0 {
		fmt.Printf("  jit: register runs ended in the interpreter:")
		for _, o := range ops {
			fmt.Printf(" %s %d;", o.op, o.n)
		}
		fmt.Println()
	}
	var used, other []vm.JITFunction
	now, keys := reportFunctions(v.JITReport())
	for _, key := range keys {
		f := now[key]
		before, seen := jitReport.funcs[key]
		switch {
		case f.Status == "native" || f.Status == "slow":
			f.Native = countSince(f.Native, before.Native)
			f.Declined = countSince(f.Declined, before.Declined)
			if f.Native+f.Declined > 0 || !seen || before.Status != f.Status {
				f.Hosts = countSince(f.Hosts, before.Hosts)
				f.Guards = countSince(f.Guards, before.Guards)
				f.Ended = countSince(f.Ended, before.Ended)
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

// reportFunctions are the functions of a JITReport by where they start
// and their names, in its order (keys): the functions of one source -- a
// script run again, a realm's copy -- summed, as one, and described by the
// busiest. Named by their line alone, the functions of a minified line --
// Box2D's, each "(anonymous)" -- had each been measured against another's
// counts at the last report, whichever the map met last.
func reportFunctions(fs []vm.JITFunction) (map[string]vm.JITFunction, []string) {
	now := map[string]vm.JITFunction{}
	var keys []string
	for _, f := range fs {
		key := fmt.Sprintf("%s:%d:%d %s", f.Script, f.Line, f.Column, f.Name)
		g, seen := now[key]
		if !seen {
			now[key] = f
			keys = append(keys, key)
			continue
		}
		if f.Native+f.Declined > g.Native+g.Declined {
			g.Status, g.Reason = f.Status, f.Reason
		}
		g.Native += f.Native
		g.Declined += f.Declined
		g.Hosts += f.Hosts
		g.Guards += f.Guards
		g.Ended += f.Ended
		g.GoCalls += f.GoCalls
		g.Work += f.Work
		g.Reopts += f.Reopts
		now[key] = g
	}
	return now, keys
}

// countSince is a function's count n since the last report, which counted
// before: all of n where it is less, the JIT having let go of what it
// learned of the function since and counted afresh (vm.JITFunction).
func countSince(n, before uint64) uint64 {
	if n < before {
		return n
	}
	return n - before
}

// tierCounts are the tier trace's counts since the last benchmark ended:
// compiles by what they were for, deoptimizations, and those of code
// younger than tierEarly invocations' worth.
var tierCounts struct {
	compiles map[string]int
	deopts   map[string]int
	early    int
	// byFunc and bySite count the deoptimizations of each function and
	// at each of its sites, an instruction of it or of a callee inlined
	// in it by its register pc, whose largest the report prints: each
	// deoptimization widens what it failed on, so that a site's are
	// bounded by how far it can widen. Both count from a function's first
	// deoptimization in what the JIT learned of it (JITTierEvent's Deopts
	// 1), so that runtimes made one after another -- ssr's FirstRender's
	// -- count apart.
	byFunc map[string]int
	bySite map[string]map[string]int
	// mostSite and mostAt are the largest of bySite's counts and its site,
	// kept as a function's counts begin again.
	mostSite int
	mostAt   string
	// top counts the deoptimizations at a site already at the top of its
	// ladder (JITTierEvent's Top), which none should be.
	top int
}

// tierEarly is V8's invocation_count_for_maglev_with_delay: a
// deoptimization of code that ran less is an early one.
const tierEarly = 600

// startTierTrace has the JIT write its tier trace to path, a line for each
// compile and each deoptimization, tab-separated: C or D, the function,
// the reason, its length, its invocations, then for a deoptimization the
// code's age and the budget before the next compile, both in invocations'
// worth, the function's deoptimizations and compiles so far, and last, for
// a deoptimization, the function whose instruction failed -- its own or a
// callee inlined in its code -- and that instruction's register pc, with
// "top" after them where the site was at the top of its ladder.
func startTierTrace(path string) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "v8bench:", err)
		os.Exit(1)
	}
	w := bufio.NewWriter(f)
	tierCounts.compiles, tierCounts.deopts = map[string]int{}, map[string]int{}
	tierCounts.byFunc, tierCounts.bySite = map[string]int{}, map[string]map[string]int{}
	vm.JITCompileClock = v8bench.Clock
	vm.JITTierHook = func(e vm.JITTierEvent) {
		fn := fmt.Sprintf("%s %s:%d", e.Func.Name, e.Func.Script, e.Func.Line)
		kind, site := "C", ""
		if e.Deopt {
			kind = "D"
			tierCounts.deopts[e.Reason]++
			if e.Age < tierEarly {
				tierCounts.early++
			}
			site = fmt.Sprintf("\t%s %s:%d\t%d", e.At.Name, e.At.Script, e.At.Line, e.PC)
			if e.Top {
				site += "\ttop"
			}
			// A function by where it starts, which tells apart those of
			// one name on a line.
			key := fmt.Sprintf("%s:%d", fn, e.Func.Column)
			sites := tierCounts.bySite[key]
			if sites == nil || e.Deopts == 1 {
				sites = map[string]int{}
				tierCounts.bySite[key] = sites
			}
			tierCounts.byFunc[key] = max(tierCounts.byFunc[key], e.Deopts)
			if e.Top {
				tierCounts.top++
			}
			at := fmt.Sprintf("%s, %s:%d:%d at %d", key, e.At.Name, e.At.Line, e.At.Column, e.PC)
			if sites[at]++; sites[at] > tierCounts.mostSite {
				tierCounts.mostSite, tierCounts.mostAt = sites[at], at
			}
		} else {
			tierCounts.compiles[e.Reason]++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d%s\n", kind, fn,
			e.Reason, e.Length, e.Invocations, e.Age, e.Budget, e.Deopts, e.Compiles, site)
		// Flushed at each, as -jitcode's is.
		w.Flush()
	}
}

// reportTier prints, for -jittier, the compiles and deoptimizations since
// the last benchmark, by reason, and the runtime goroutine's part of the
// compiles (vm.JITGoroutineTimes).
func reportTier(r runtimeOf) {
	if vm.JITTierHook == nil || tierCounts.compiles == nil {
		return
	}
	line := func(m map[string]int) (int, string) {
		keys := slices.Sorted(maps.Keys(m))
		total, s := 0, ""
		for _, k := range keys {
			total += m[k]
			s += fmt.Sprintf(" %s %d", k, m[k])
		}
		return total, s
	}
	n, cs := line(tierCounts.compiles)
	d, ds := line(tierCounts.deopts)
	fmt.Printf("  jit tier: %d compiles:%s\n", n, cs)
	fmt.Printf("  jit tier: %d deoptimizations, %d of code younger than %d invocations:%s\n", d, tierCounts.early, tierEarly, ds)
	gt := hostaccess.VM(r.rt).TakeJITGoroutineTimes()
	ms := func(t time.Duration) float64 { return float64(t.Microseconds()) / 1e3 }
	each := func(t time.Duration, n int) float64 {
		if n == 0 {
			return 0
		}
		return float64(t.Microseconds()) / float64(n)
	}
	fmt.Printf("  jit tier: the runtime goroutine's part: %d compiles on the spot %.1f ms (%.0f us each); in the background %d prepared %.1f ms (%.0f us each), %d installed %.1f ms (%.0f us each), %d taken back %.1f ms\n",
		gt.Spots, ms(gt.Spot), each(gt.Spot, gt.Spots), gt.Prepares, ms(gt.Prepared), each(gt.Prepared, gt.Prepares),
		gt.Installs, ms(gt.Installed), each(gt.Installed, gt.Installs), gt.TakeBacks, ms(gt.TakenBack))
	most := func(m map[string]int) (string, int) {
		k, n := "", 0
		for _, x := range slices.Sorted(maps.Keys(m)) {
			if m[x] > n {
				k, n = x, m[x]
			}
		}
		return k, n
	}
	if fn, n := most(tierCounts.byFunc); n > 0 {
		fmt.Printf("  jit tier: most deoptimizations of a function %d (%s), at a site %d (%s); %d at a site at its top\n", n, fn, tierCounts.mostSite, tierCounts.mostAt, tierCounts.top)
	}
	clear(tierCounts.compiles)
	clear(tierCounts.deopts)
	clear(tierCounts.byFunc)
	clear(tierCounts.bySite)
	tierCounts.early, tierCounts.mostSite, tierCounts.mostAt, tierCounts.top = 0, 0, "", 0
}
