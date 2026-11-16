//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// jitMarkingWork is what one run of a test of the micro-benchmarks did that
// a breach of the marking rule changes: its calls of Go, the times native
// code left for Go, the runs the interpreter finished, and the records of
// exits Go applied (jitApplyRecords, jit.ApplyExit), which native code
// applies itself as it leaves (mir's recordsTail).
type jitMarkingWork struct{ calls, hosts, interpreted, records uint64 }

// TestJITSameWorkWhileMarking holds native code to internal/jit/README.md's
// rule for the write barrier: a path compiled natively does the same work
// whether the collector marks or not. Every test of the micro-benchmarks
// (internal/v8bench's microbench.js and microbench_extra.js, which between
// them reach most of what the JIT makes natively) is warmed up and then run
// once, in a runtime whose native code sees the collector idle and in one
// whose native code sees it marking throughout (jitMarkingForTest), the
// collector itself held off in both so that only the flag differs; each
// test must make as many calls of Go, leave native code as often, have
// the interpreter finish as many runs and have Go apply as many records in
// the one as in the other. A test that differs names a path that leaves
// native code, or calls Go, for the marking's sake, which is to be brought
// to the rule, not listed here.
func TestJITSameWorkWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src, names := jitMarkingCorpus(t)
	// Each run holds the collector's count and the cache's clocks, in a
	// JIT memory that holds all it compiles (jitMarkingHeldForTest,
	// jitMarkingBudget), so that its cache keeps, flushes and reclaims
	// alike whether native code sees the collector marking or not.
	plain, plainCache := jitMarkingRun(t, src, names, false)
	marking, markingCache := jitMarkingRun(t, src, names, true)
	// And each run by native code made from its register code
	// (jitRegRun), named "reg:" and the test's name.
	regPlain, regPlainCache := jitMarkingRegRun(t, src, names, false)
	regMarking, regMarkingCache := jitMarkingRegRun(t, src, names, true)
	for _, c := range [][2]jitMarkingCache{{plainCache, markingCache}, {regPlainCache, regMarkingCache}} {
		switch p, m := c[0], c[1]; {
		case p.evicted != m.evicted || p.refused != m.refused || p.flushed != m.flushed:
			t.Errorf("the JIT's cache differs from run to run (%s): %+v, and while marking %+v: a run's work depends on more than the marking", p.form, p, m)
		case p.evicted != 0 || p.refused != 0:
			t.Errorf("the JIT's cache ran out of room (%s): %+v; jitMarkingBudget is to hold all the runs compile", p.form, p)
		default:
			t.Logf("the JIT's cache, %s: %+v, and while marking %+v", p.form, p, m)
		}
	}
	all := names
	for _, name := range names {
		all = append(all, "reg:"+name)
		plain["reg:"+name], marking["reg:"+name] = regPlain[name], regMarking[name]
	}
	differ := 0
	for _, name := range all {
		p, m := plain[name], marking[name]
		why, known := jitMarkingBreaches[name]
		switch {
		case p != m && !known:
			differ++
			t.Errorf("%s: %d calls of Go, %d exits, %d finished in the interpreter, %d records Go applied; while marking %d, %d, %d, %d: a path leaves native code or calls Go for the marking's sake (internal/jit/README.md, the write barrier's rule)",
				name, p.calls, p.hosts, p.interpreted, p.records, m.calls, m.hosts, m.interpreted, m.records)
		case p != m:
			differ++
			t.Logf("%s, a known breach (%s): %d calls of Go, %d exits; while marking %d, %d", name, why, p.calls, p.hosts, m.calls, m.hosts)
		case known:
			t.Errorf("%s does the same work while marking now: take it out of jitMarkingBreaches", name)
		}
	}
	t.Logf("%d tests of the micro-benchmarks, in both forms, %d differ while marking, %d of them known", len(names), differ, len(jitMarkingBreaches))
}

// jitMarkingBreaches are the tests of the micro-benchmarks whose paths still
// leave native code, or call Go, while the collector marks, each with what
// does. The list only shrinks: a path brought to the rule is taken out of it
// (the test fails while it is listed and keeps the rule), and no path is
// added to it -- a new native path keeps the rule or is not compiled
// natively.
var jitMarkingBreaches = map[string]string{}

// jitMarkingCorpus is the micro-benchmarks' source, without microbench.js's
// own driver, and the names of their tests: each function of one argument,
// n, but the async ones, whose work runs in jobs after their call returns,
// and those that compile functions anew, with the Function constructor this
// package leaves to its host: carried_short's loop is the code they run.
func jitMarkingCorpus(t *testing.T) (string, []string) {
	t.Helper()
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("no micro-benchmarks: %v", err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	bench := read("../v8bench/microbench.js")
	if end := strings.LastIndex(bench, "\nif (typeof scriptArgs === \"undefined\")"); end > 0 {
		bench = bench[:end+1]
	}
	src := bench + read("../v8bench/microbench_extra.js")
	seen := map[string]bool{}
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^function (\w+)\s*\(n\)`).FindAllStringSubmatch(src, -1) {
		if name := m[1]; !seen[name] && !strings.HasPrefix(name, "async_") && !strings.HasPrefix(name, "carried_fresh") {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) < 100 {
		t.Fatalf("only %d tests found in the micro-benchmarks", len(names))
	}
	return src, names
}

// jitMarkingRun warms each test up and counts what its next run does, in a
// fresh runtime, native code seeing the collector marking if marking.
func jitMarkingRun(t *testing.T, src string, names []string, marking bool) (map[string]jitMarkingWork, jitMarkingCache) {
	t.Helper()
	defer jitMarkingHeldForTest()()
	if marking {
		jitMarkingForTest(t)
	}
	r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: jitMarkingBudget})
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	calls := uint64(0)
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	work := map[string]jitMarkingWork{}
	for _, name := range names {
		for range 3 {
			if _, err := r.Run(compileForTest(t, name+"(64)")); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		calls0, hosts0, interpreted0, records0 := calls, uint64(0), uint64(0), uint64(0)
		if r.jit != nil {
			hosts0, interpreted0, records0 = r.jit.hosts, r.jit.interpreted, r.jit.ssaRecords
		}
		if _, err := r.Run(compileForTest(t, name+"(256)")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := jitMarkingWork{calls: calls - calls0}
		if r.jit != nil {
			w.hosts, w.interpreted, w.records = r.jit.hosts-hosts0, r.jit.interpreted-interpreted0, r.jit.ssaRecords-records0
		}
		work[name] = w
	}
	// Nothing is collected here, so what the tests allocate is what the
	// process takes: a test whose n is not a count of operations, each of
	// which takes little, had this at 24 GB, and a runner of CI's has 16.
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > jitMarkingAllocLimit {
		t.Errorf("the micro-benchmarks allocated %d MB with the collector off: a test's n should be small work", n>>20)
	}
	return work, jitMarkingCacheOf(r, "stack form")
}

// jitMarkingRegRun is jitMarkingRun for native code made from register
// code: the corpus compiled in register form, each test's function called
// natively (RegNativeCall) three times from its first call, from its start,
// and then once more counted.
func jitMarkingRegRun(t *testing.T, src string, names []string, marking bool) (map[string]jitMarkingWork, jitMarkingCache) {
	t.Helper()
	defer jitMarkingHeldForTest()()
	if marking {
		jitMarkingForTest(t)
	}
	compiler.SetRegisterForm(true)
	prog := compileForTest(t, src)
	compiler.SetRegisterForm(false)
	defer compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	defer SetJITRegTier(SetJITRegTier(true))
	r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: jitMarkingBudget})
	if _, err := r.Run(prog); err != nil {
		t.Fatal(err)
	}
	calls := uint64(0)
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	work := map[string]jitMarkingWork{}
	for _, name := range names {
		fn, err := r.GetProp(Obj(r.Global()), r.Intern(name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Less work than the stack form's runs: both forms' runs, marking
		// and not, under checkptr take CI's job past its time.
		for range 2 {
			if res := r.RegNativeCall(fn, []Value{Float(32)}, false); res.Err != nil {
				t.Fatalf("%s: %v", name, res.Err)
			}
		}
		calls0, st0 := calls, r.RegNativeStats()
		if res := r.RegNativeCall(fn, []Value{Float(96)}, false); res.Err != nil {
			t.Fatalf("%s: %v", name, res.Err)
		}
		st := r.RegNativeStats()
		work[name] = jitMarkingWork{calls: calls - calls0, hosts: st.Hosts - st0.Hosts + st.Guards - st0.Guards,
			interpreted: st.Interpreted - st0.Interpreted, records: st.Records - st0.Records}
	}
	if st := r.RegNativeStats(); st.Entries == 0 {
		t.Error("no test ran natively from its register code")
	} else {
		t.Logf("register code, marking %v: %d entries, %d host exits, %d deoptimizations, %d polls", marking, st.Entries, st.Hosts, st.Guards, st.Polls)
	}
	return work, jitMarkingCacheOf(r, "register code")
}

// jitMarkingAllocLimit is how much jitMarkingRun's tests may allocate: about
// 1 GB is usual, and 5.7 GB under checkptr, which CI runs them with and which
// keeps far less on the stack.
const jitMarkingAllocLimit = 8 << 30

// jitMarkingHeldForTest holds what else than its program decides a run's
// work, so that jitMarkingRun's two runs differ in the marking alone, until
// the restore it returns is called: the collector off once its cycles'
// count (jitGCEpoch) has settled -- a finalizer counts a cycle at some time
// after it, and a count landing in one run emptied the megamorphic cache
// there (flush) -- and the clocks the JIT's cache ages and flushes code by
// held (JITHoldClocks), which otherwise run on the program's time, and the
// marking runs' calls of the barrier lengthen it: code flushed, or
// reclaimed for room, in one run and not the other was compiled again in
// that run alone. Run so on arm64 under emulation, two rounds of the
// register runs differed, in the first in carried_exit_500 and
// carried_exit_mixed by a call of Go, in the second in nothing; CI's
// arm64 runner under checkptr had typed_sort make 372 calls of Go while
// marking against 371, every other count the same; a flush forced into
// typed_sort's warm-up in one run makes it 374. With the clocks held no
// code ages, and the runtimes' JIT memory (jitMarkingBudget) holds all the
// tests compile: at the default 8 MiB the cache, full, refused compiles
// from the middle of the run on, typed_sort's among them.
func jitMarkingHeldForTest() (restore func()) {
	jitEpochOnce.Do(jitArmEpoch)
	gc := debug.SetGCPercent(-1)
	for epoch := jitGCEpoch.Load(); jitGCEpoch.Load() == epoch; {
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	clocks := JITHoldClocks()
	return func() {
		clocks()
		debug.SetGCPercent(gc)
	}
}

// jitMarkingBudget is the JIT's memory for jitMarkingRun's runtimes. The
// corpus fills the default budget -- register code's state is counted in
// it (regAccount), the top-level code's literal pools among it -- and
// which code makes room for a compile, or whether there is room, then
// depends on the clocks; with them held none is old, and a compile
// refused room in one run and not the other is no path's work while
// marking. So each run's cache must not run out (jitMarkingCacheOf).
const jitMarkingBudget = 256 << 20

// jitMarkingCache is what a run's JIT cache did that decides what it
// compiled: the code it reclaimed for room and the compiles it found none
// for, and the code it flushed. Two runs that differ in it do not differ
// in the marking alone. What it held at the end is noted, not compared:
// jitMarkingForTest has native code read a flag on the heap in place of
// the runtime's, in its static data, and an address's encoding depends on
// its value: amd64 makes one below 4 GB an immediate and one above a
// constant of the code's pool, and arm64 takes an instruction for each
// 16 bits that are not zero. Where the binary's data lies low -- Linux,
// Go 1.24's Windows binaries -- the stack form's code held 2,880 bytes
// less while marking, and on arm64 either form's differed by tens of
// bytes, the work the same in every test.
type jitMarkingCache struct {
	form                      string
	held                      int64
	evicted, refused, flushed uint64
}

// jitMarkingCacheOf is r's jitMarkingCache.
func jitMarkingCacheOf(r *Runtime, form string) jitMarkingCache {
	c := jitMarkingCache{form: form}
	if s := r.jit; s != nil {
		c.held, c.evicted, c.refused, c.flushed = int64(s.held()), s.evicted, s.refusedRoom, s.flushed
	}
	return c
}
