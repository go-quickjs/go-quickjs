//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitWorkRecord is the compile work TestJITCompileWork records.
const jitWorkRecord = "testdata/jit_work.txt"

// jitWorkUpdate is the environment variable that has TestJITCompileWork
// write its record anew, where a change means to move it.
const jitWorkUpdate = "QUICKJS_JIT_WORK"

// A compile's work, by measure, for a fixed corpus -- a function of each
// of TestJITCompileLinear's shapes at a few hundred values, and the
// micro-benchmarks' asm.js kernels -- holds to the figures recorded
// (testdata/jit_work.txt), so that a change that makes the compiler do
// much more work fails here without anyone timing it: the spill classes
// had made the allocator's hints four to six times as long on Octane's
// asm.js suites, which only a run of -jitcompile showed.
//
// The measures are work, not time, the same on every machine: of the SSA,
// the bytes its build and Optimize allocate and how many values and blocks
// Optimize leaves; of the backend, for amd64 and for arm64 both, each
// compiled on any host from the same SSA, each phase's steps in the loops
// whose length is not the item's own (mir.Times' Steps) and the bytes it
// allocates. The collector is off while they are counted. A count of
// steps, values or blocks is the record's within 10% either way -- they do
// not vary from one run to the next, so a change that moves one by more
// is a change of the compiler's work, to record if it is meant -- and one
// of bytes no more than 25% above it, a Go release's allocations
// differing from another's. The bytes are held only in the build the
// record is made in (jitWorkPlainBuild): one with the verifier's checks,
// the race detector or checkptr allocates as it goes, and its steps,
// values and blocks are held still. Where a change means to move them,
// the record is written anew by QUICKJS_JIT_WORK=update, in that build,
// and the change's commit says why.
func TestJITCompileWork(t *testing.T) {
	if !jitSSABackend || testing.Short() {
		t.Skip("no SSA backend on this architecture, or short")
	}
	holdCollector(t)
	got := map[string]int64{}
	var order []string
	put := func(item, what string, n int64) {
		k := item + " " + what
		if _, ok := got[k]; !ok {
			order = append(order, k)
		}
		got[k] = n
	}
	for _, it := range jitWorkCorpus(t) {
		jitWorkOf(t, it, put)
	}
	plain, why := jitWorkPlainBuild()
	if os.Getenv(jitWorkUpdate) == "update" {
		if !plain {
			t.Fatalf("the record is written in the plain build, not one %s", why)
		}
		var b strings.Builder
		b.WriteString("# The compile work of TestJITCompileWork's corpus, written by\n")
		b.WriteString("# QUICKJS_JIT_WORK=update go test -tags quickjs_jit ./internal/vm -run '^TestJITCompileWork$'\n")
		for _, k := range order {
			fmt.Fprintf(&b, "%s %d\n", k, got[k])
		}
		if err := os.WriteFile(jitWorkRecord, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readJITWorkRecord(t)
	if !plain {
		t.Logf("bytes not held: a build %s", why)
	}
	for _, k := range order {
		w, ok := want[k]
		if !ok {
			t.Errorf("%s: %d, not in %s: write it with %s=update", k, got[k], jitWorkRecord, jitWorkUpdate)
			continue
		}
		g := got[k]
		if strings.HasSuffix(k, " bytes") {
			if plain && float64(g) > 1.25*float64(w)+4096 {
				t.Errorf("%s: %d, %.2f times the %d recorded: a change that means it records it anew (%s=update)", k, g, float64(g)/float64(max(w, 1)), w, jitWorkUpdate)
			}
			continue
		}
		if math.Abs(float64(g-w)) > 0.10*float64(w)+16 {
			t.Errorf("%s: %d, %.2f times the %d recorded: a change that means it records it anew (%s=update)", k, g, float64(g)/float64(max(w, 1)), w, jitWorkUpdate)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: recorded, not measured: write the record anew (%s=update)", k, jitWorkUpdate)
		}
	}
}

// jitWorkPlainBuild reports whether the test is built as the record is
// made, with nothing that changes what the compiler allocates, or else
// with what: the verifier's tags or the race detector's (jitWorkPlainTags),
// or compiler flags -- checkptr's instrumentation -- or a sanitizer, as
// the binary's build settings name them.
func jitWorkPlainBuild() (bool, string) {
	if !jitWorkPlainTags {
		return false, "with the verifier's or the race detector's tags"
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false, "whose settings are unknown"
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "-gcflags":
			if s.Value != "" {
				return false, "with -gcflags " + s.Value
			}
		case "-race", "-msan", "-asan":
			if s.Value == "true" {
				return false, "with " + s.Key
			}
		}
	}
	return true, ""
}

// readJITWorkRecord reads jitWorkRecord: a line of an item, a measure and
// its figure each, past comments.
func readJITWorkRecord(t *testing.T) map[string]int64 {
	t.Helper()
	f, err := os.Open(jitWorkRecord)
	if err != nil {
		t.Fatalf("%v: write it with %s=update", err, jitWorkUpdate)
	}
	defer f.Close()
	m := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		n, err := strconv.ParseInt(line[i+1:], 10, 64)
		if i < 0 || err != nil {
			t.Fatalf("%s: a malformed line %q", jitWorkRecord, line)
		}
		m[line[:i]] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

// jitWorkItem is a function of the corpus, by name: build makes its SSA,
// from its lowered program and its feedback as a compile of it would.
type jitWorkItem struct {
	name  string
	build func() *ssa.Func
}

// jitWorkCorpus is TestJITCompileWork's functions: one of each of
// TestJITCompileLinear's shapes at 400 values, run once in a runtime with
// the JIT, and the micro-benchmarks' asm.js kernels, compiled at their
// module's link.
func jitWorkCorpus(t *testing.T) []jitWorkItem {
	t.Helper()
	plain := func(*Runtime) []Value { return []Value{Float(3), Float(2)} }
	shapes := []struct {
		name string
		src  func(string, int) string
		args func(r *Runtime) []Value
	}{
		{"blocks", linearBlocksSource, plain},
		{"guards", linearGuardsSource, plain},
		{"spill", func(name string, n int) string { return spillMoreHelpers + spillMoreSource(name, n, 150) }, func(r *Runtime) []Value {
			return []Value{Float(1.25), Float(0.5), Float(7), spillMoreObject(r, 0), Str(NewString("pp")), Float(2)}
		}},
		{"loops", linearLoopsSource, plain},
		{"groups", linearGroupsSource, plain},
	}
	var items []jitWorkItem
	for _, sh := range shapes {
		name := sh.name + "400"
		rts := regNativeRuntimes(t, sh.src(name, 400))
		r := rts[0]
		r.jitMemoryLimit = 1 << 36
		spillMoreCollect(r)
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		r.RegNativeCall(fn, sh.args(r), false)
		cl := fn.Object().fn().closure
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		if e == nil || e.p == nil {
			t.Fatalf("%s: not lowered", name)
		}
		items = append(items, jitWorkItem{name, jitWorkBuild(t, r, e.p, cl, cl.reg)})
	}
	src, err := os.ReadFile("../v8bench/microbench_extra.js")
	if err != nil {
		t.Fatal(err)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	// Each micro-benchmark called once, as TestAsmJITMicroKernels calls
	// them: its kernel linked, and its typed code compiled at the link.
	var calls []string
	for _, name := range []string{"adler", "crc32", "copy", "fnv", "dot", "fib", "call_leaf", "switch", "table"} {
		calls = append(calls, "asm_"+name+"(8192)")
	}
	prog := "var global_res;\n" + string(src) + "\nvar asmMicro = extra_asm_kernels();\n" + strings.Join(calls, ";\n")
	if _, err := r.Run(compileForTest(t, prog)); err != nil {
		t.Fatal(err)
	}
	m, _ := r.GetProp(Obj(r.Global()), r.Intern("asmMicro"))
	for _, name := range []string{"adler", "crc", "copy", "fnv", "dot", "fib", "callLeaf", "table"} {
		f, _ := r.GetProp(m, r.Intern(name))
		if !f.IsObject() || f.Object().fn() == nil {
			t.Fatalf("asm %s: no function", name)
		}
		cl := f.Object().fn().closure
		tc := cl.regTyped
		if tc == nil || tc.typedExt().twin == nil {
			t.Fatalf("asm %s: no typed code", name)
		}
		tw := tc.typedExt().twin
		e := r.jit.regCache[r.jitRegKeyOf(tw)]
		if e == nil || e.p == nil {
			t.Fatalf("asm %s: not lowered", name)
		}
		items = append(items, jitWorkItem{"asm_" + name, jitWorkBuild(t, r, e.p, tw, tc)})
	}
	return items
}

// jitWorkBuild is a build of p, cl's register code c lowered, as the JIT
// builds it: its feedback asked anew at each.
func jitWorkBuild(t *testing.T, r *Runtime, p *ir.Program, cl *closure, c *regCode) func() *ssa.Func {
	return func() *ssa.Func {
		f, err := ssa.BuildWith(p, r.jitRegFeedbackFor(cl, p))
		if err != nil {
			t.Fatal(err)
		}
		jitRegShape(f, p, cl, c)
		return f
	}
}

// jitWorkOf measures it's compile work (TestJITCompileWork), each figure
// given to put by measure.
func jitWorkOf(t *testing.T, it jitWorkItem, put func(item, what string, n int64)) {
	t.Helper()
	var ms runtime.MemStats
	alloc := func() int64 {
		runtime.ReadMemStats(&ms)
		return int64(ms.TotalAlloc)
	}
	opts := mir.Settings()
	opts.CheckAllocations = false
	for _, arch := range []string{"amd64", "arm64"} {
		a0 := alloc()
		f := it.build()
		a1 := alloc()
		ssa.Optimize(f)
		a2 := alloc()
		if arch == "amd64" {
			put(it.name, "ssa build bytes", a1-a0)
			put(it.name, "ssa optimize bytes", a2-a1)
			put(it.name, "ssa values", int64(f.NumValues()))
			put(it.name, "ssa blocks", int64(len(f.Blocks)))
		}
		tm := &mir.Times{Clock: alloc}
		o := opts
		o.Timing = tm
		tm.Start()
		var err error
		if arch == "amd64" {
			_, err = mir.CompileAMD64With(f, jitEncoding, o)
		} else {
			_, err = mir.CompileARM64With(f, jitEncoding, o)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", it.name, arch, err)
		}
		for p := mir.PhasePrepare; p < mir.NumPhases; p++ {
			put(it.name, fmt.Sprintf("%s %s steps", arch, p), tm.Steps[p])
			put(it.name, fmt.Sprintf("%s %s bytes", arch, p), tm.Ticks[p])
		}
	}
}
