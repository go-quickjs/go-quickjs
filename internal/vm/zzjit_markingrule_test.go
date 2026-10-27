//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
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
	plain := jitMarkingRun(t, src, names, false)
	marking := jitMarkingRun(t, src, names, true)
	differ := 0
	for _, name := range names {
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
	t.Logf("%d tests of the micro-benchmarks, %d differ while marking, %d of them known", len(names), differ, len(jitMarkingBreaches))
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
// n, but the async ones, whose work runs in jobs after their call returns.
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
		if name := m[1]; !seen[name] && !strings.HasPrefix(name, "async_") {
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
func jitMarkingRun(t *testing.T, src string, names []string, marking bool) map[string]jitMarkingWork {
	t.Helper()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	if marking {
		jitMarkingForTest(t)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	calls := uint64(0)
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
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
	return work
}
