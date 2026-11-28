//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// keyAtomsHoldForTest reports whether r's table of key strings (keyAtoms)
// has an entry for a flat string of text.
func keyAtomsHoldForTest(r *Runtime, text string) bool {
	for i := range r.keyAtoms {
		if s := r.keyAtoms[i].s; s != nil && s.left == nil && s.s == text {
			return true
		}
	}
	return false
}

// A keyed store of a string constant, this["constructor"] = v, is compiled
// as the store of the key its site met, with no test of the key: the test
// had asked the VM's table of key strings, whose entries a string's
// address slots, and once other strings had evicted the constant's, the
// store took the way of a key the site had never met, left native code
// and was compiled again for what that taught -- at a call that varied
// from run to run with where the strings were, Octane's Typescript's
// BlockIntrinsics among them, so that two runs of one build compiled
// differently from there on.
func TestJITKeyedStoreConstantKeyEvicted(t *testing.T) {
	events := jitTierTraceForTest(t)
	r, run := jitTierRuntimeForTest(t, 8, `function B() { this.a = undefined; this["constructor"] = undefined; }
		function flood(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return 0; }
		function f() { var b = new B(); return b.a === undefined && b.hasOwnProperty("constructor") ? 1 : 0; }`)
	r.jitStress = jitStressConfig{}
	for range 20 {
		run("f()")
	}
	run("flood(4096)")
	if keyAtomsHoldForTest(r, "constructor") {
		t.Fatal(`"constructor" is still in the table of key strings: the flood evicted nothing, and the test tests nothing`)
	}
	for range 20 {
		run("f()")
	}
	for _, e := range *events {
		if e.Func.Name == "B" && (e.Deopt || e.Compiles != 0) {
			t.Fatalf("B left its code or was compiled again after its key's entry was evicted:\n%s", jitTierLines(*events))
		}
	}
}

// A keyed store of a key that is no constant, o[k] = v, tests its key
// against each key its site met by the VM's table of key strings, whose
// entries a string's address slots: a string another had evicted is
// looked up and its entry filled by Go from native code, and tested
// again, so that what the store does is what its key is, not what the
// table holds. An evicted key had been one the site never met:
// the store left native code, and the code was compiled again for what
// that taught. The key the test floods out is one the site met, and one
// it did not, which deoptimizes, entry or none.
func TestJITKeyedStoreKeyEvicted(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprint("marking=", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			events := jitTierTraceForTest(t)
			r, run := jitTierRuntimeForTest(t, 8, `var keys = ["x", "y"], other = "z", made = { z: 0 }
				function set(o, k, v) { o[k] = v; return o }
				function flood(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return 0 }`)
			r.jitStress = jitStressConfig{}
			for i := range 20 {
				run(fmt.Sprintf("set({ a: 1 }, keys[%d], %d)[keys[%d]]", i&1, i, i&1))
			}
			cl := jitGlobalClosure(r, "set")
			e := cl.closureCode.jitReg
			if e == nil || e.code == nil {
				t.Fatalf("set has no native code: no test of its keyed store\n%s", jitTierLines(*events))
			}
			flood := func() {
				run("flood(4096)")
				for _, k := range []string{"x", "y", "z"} {
					if keyAtomsHoldForTest(r, k) {
						t.Fatalf("%q is still in the table of key strings: the flood evicted nothing, and the test tests nothing", k)
					}
				}
			}
			flood()
			*events = (*events)[:0]
			st := r.RegNativeStats()
			for i := range 20 {
				run(fmt.Sprintf("set({ a: 1 }, keys[%d], %d)[keys[%d]]", i&1, i, i&1))
			}
			after := r.RegNativeStats()
			if !keyAtomsHoldForTest(r, "x") || !keyAtomsHoldForTest(r, "y") {
				t.Errorf("the stores' keys are not in the table of key strings again")
			}
			if after.Hosts != st.Hosts || after.Interpreted != st.Interpreted {
				t.Errorf("the evicted keys' stores left native code: %d exits to Go, %d invocations finished in the interpreter", after.Hosts-st.Hosts, after.Interpreted-st.Interpreted)
			}
			if e.life.reopt || e.invalid || cl.closureCode.jitReg != e || len(*events) != 0 {
				t.Fatalf("the evicted keys' stores taught set's code (reopt %v, invalid %v, replaced %v):\n%s", e.life.reopt, e.invalid, cl.closureCode.jitReg != e, jitTierLines(*events))
			}
			// A key the site did not meet, its entry evicted: the store
			// deoptimizes, as it does with the entry there.
			flood()
			run("set({ a: 1 }, other, 1).z")
			if got := jitTierLines(*events); !strings.HasPrefix(got, "D set ") {
				t.Errorf("a key the site never met, its entry evicted, did not deoptimize the store:\n%s", got)
			}
		})
	}
}

// A megamorphic read's miss of the megamorphic cache (abi.MegaCache) --
// an entry another has displaced from its set, which the shapes' addresses
// choose -- is filled and gone on from as a hit is: no exit, no compile
// again, nothing learned, no call of Go counted in the code's profit. Each
// miss had been a call of Go that learned its receiver's case, read
// through the site's cache and counted as a stretch, so that where the
// misses came -- which differs from one process to the next -- decided
// what the code was compiled for and when: two runs of one build of
// Octane's Typescript compiled differently from their 769th compile on.
// Own properties, a prototype's and one none of them has, each in an
// object too large for native code to search.
func TestJITMegaMissTeachesNothing(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, c := range []struct {
		key     string
		marking bool
	}{{"v", false}, {"p", false}, {"none", false}, {"v", true}, {"none", true}} {
		t.Run(fmt.Sprintf("%s/marking=%v", c.key, c.marking), func(t *testing.T) {
			key := c.key
			if c.marking {
				// Native code seeing the collector mark, as it does the
				// same work then.
				jitMarkingForTest(t)
			}
			events := jitTierTraceForTest(t)
			// get called natively, its own code's misses those of a
			// native callee.
			jitNoInlineForTest(t, "get")
			holdCollector(t)
			r, run := jitTierRuntimeForTest(t, 8, `
				var proto = { p: 3 }
				function make(i) {
					const o = Object.create(proto)
					o.v = i
					for (let k = 0; k < 12; k++) o["f" + k] = k
					o["s" + i] = i
					return o
				}
				var objs = []
				for (let i = 0; i < 40; i++) objs.push(make(i))
				function get(o) { return o.`+key+` }
				function sum() { let t = 0; for (let i = 0; i < objs.length; i++) t += get(objs[i]) === undefined ? 1 : get(objs[i]); return t }`)
			r.jitStress = jitStressConfig{}
			for range 30 {
				run("sum()")
			}
			cl := jitGlobalClosure(r, "get")
			e := cl.closureCode.jitReg
			if e == nil || e.code == nil || r.jit.mega == nil {
				t.Fatalf("get has no native code, or the runtime no megamorphic cache: no test of its misses\n%s", jitTierLines(*events))
			}
			atom := r.atoms.intern(key)
			// Each receiver's set filled with entries of another shape, the
			// prototype's, as the entries of other shapes and keys that
			// share it fill it.
			other := unsafe.Pointer(r.global.getOwn(r.atoms.intern("proto")).value().Object().shape)
			arr := r.global.getOwn(r.atoms.intern("objs")).value()
			var objs []*Object
			for i := range 40 {
				v, err := r.getValueProp(arr, Atom(atomIndexTag|uint32(i)))
				if err != nil || !v.IsObject() {
					t.Fatal(v, err)
				}
				objs = append(objs, v.Object())
			}
			for _, o := range objs {
				set := r.jit.mega[abi.MegaSet(uintptr(unsafe.Pointer(o.shape)), uint32(atom)):][:abi.MegaWays]
				for w := range set {
					set[w] = abi.MegaEntry{Shape: other, Key: uint32(atom)}
				}
			}
			*events = (*events)[:0]
			goCalls, st := e.life.ssaStats.goCalls, r.RegNativeStats()
			run("sum()")
			after := r.RegNativeStats()
			// The receivers of shapes the code has no case for missed, and
			// their entries are filled again.
			filled := 0
			for _, o := range objs {
				if r.jit.megaAnswers(o, atom) {
					filled++
				}
			}
			if filled < len(objs)/2 {
				t.Fatalf("%d of %d receivers' entries filled again: too few misses of the megamorphic cache to test", filled, len(objs))
			}
			if e.life.ssaStats.goCalls != goCalls {
				t.Errorf("the misses counted %d calls of Go in get's profit", e.life.ssaStats.goCalls-goCalls)
			}
			if after.Hosts != st.Hosts || after.Interpreted != st.Interpreted {
				t.Errorf("the misses left native code: %d exits to Go, %d invocations finished in the interpreter", after.Hosts-st.Hosts, after.Interpreted-st.Interpreted)
			}
			if e.life.reopt || e.invalid || cl.closureCode.jitReg != e {
				t.Errorf("the misses taught get's code (reopt %v, invalid %v, replaced %v)", e.life.reopt, e.invalid, cl.closureCode.jitReg != e)
			}
			for _, ev := range *events {
				if ev.Func.Name == "get" || ev.Func.Name == "sum" {
					t.Fatalf("the misses compiled or deoptimized:\n%s", jitTierLines(*events))
				}
			}
		})
	}
}

// A stretch that returns counts its work to its return's block, which the
// code writes as the exit's site, not to the exit PC, which a return does
// not write: that is the last call of Go's, or an earlier exit's, of
// whatever code ran in the context before. Counted from it, a miss of the
// megamorphic cache -- a call of Go, at a PC of its own -- changed what
// the next return counted, and the code's profit with it.
func TestJITReturnWorkIsItsSite(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	at := make([]int32, 64)
	for i := range at {
		at[i] = int32(i)
	}
	e := &jitRegEntry{life: &jitEntry{}, at: at}
	ctx := &abi.Context{ExitKind: abi.ExitReturn, ExitPC: 60, ExitSite: 9}
	r.jitRegStretchWork(e, ctx, 2, r.backEdges-r.jitState().pollCredit, false)
	if got := e.life.ssaStats.work; got != 7 {
		t.Fatalf("a return at instruction 9 of a stretch begun at 2 counted %d instructions of work, want 7", got)
	}
}

// jitReadCompiledMegamorphicForTest reports whether a property read of
// cl's code is megamorphic in the feedback its code is compiled from
// (ssa.PropertySite's Megamorphic): its misses Go's from native code. Its
// interpreter's cache need not say so (jitSiteMegamorphicForTest): a
// site compiled for four shapes is megamorphic, and the megamorphic
// cache's misses fill no site's cache.
func jitReadCompiledMegamorphicForTest(t *testing.T, r *Runtime, cl *closure) bool {
	t.Helper()
	cv, ok := jitViewForTest(r, cl)
	if !ok {
		t.Fatal("no code to look at")
	}
	p := cv.reg.p
	fb := r.jitRegFeedbackFor(cl, p)
	for pc, in := range p.Code {
		if in.Op != ir.PropertyRead && in.Op != ir.ReferenceRead {
			continue
		}
		if site, ok := fb.Property(pc); ok && site.Megamorphic {
			return true
		}
	}
	return false
}

// The cache lets go of the lowerings used at one epoch in the order they
// were made, whatever order its map gives them, which is the process's:
// ordered by their epoch alone, which went first had a budget's compiles
// differ from one run to the next.
func TestJITVictimsTieByMade(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	s := r.jitState()
	fns := make([]*bytecode.Function, 12)
	for i := range fns {
		fns[i] = &bytecode.Function{Name: fmt.Sprint(i)}
		s.lowering(fns[i])
	}
	for _, l := range s.lowered {
		l.used = 7
	}
	s.budget = 1 << 40
	for range 20 {
		if !s.listVictims(8, 0) {
			t.Fatal("nothing listed")
		}
		var made []uint64
		for _, v := range s.victims {
			if v.class == jitVictimLowering && !v.reg {
				made = append(made, v.l.made)
			}
		}
		if len(made) != len(fns) || !slices.IsSorted(made) {
			t.Fatalf("lowerings of one epoch listed in the order %v, not as they were made", made)
		}
	}
	runtime.KeepAlive(fns)
}

// A parking's candidates used at one epoch go in the order their
// functions' states were made, whatever order the cache's map listed them
// in, which is the process's.
func TestJITParkCandsTieBySerial(t *testing.T) {
	var c jitParkCands
	for _, s := range []uint64{9, 3, 12, 1, 7, 5, 11, 2, 8, 4, 10, 6} {
		c = append(c, jitParkCand{at: 3, serial: s})
	}
	c = append(c, jitParkCand{at: 1, serial: 99})
	sort.Sort(c)
	if c[0].serial != 99 {
		t.Fatalf("the least recently used, %+v, is not first", c[0])
	}
	for i := 2; i < len(c); i++ {
		if c[i-1].serial > c[i].serial {
			t.Fatalf("candidates of one epoch in the order %v, not their states'", c)
		}
	}
}

// Two runtimes in one process running one program compile the same, at
// the same points of it: their tier traces are the same event by event,
// with the invocations, the work and the budgets of each. The runtimes'
// objects, and so their layouts, are at addresses of their own -- the
// second's moved by what it made first -- so a cache slotted by an
// address fills and misses at other points in each: a megamorphic read
// over hundreds of AST-like nodes of layouts of their own, which the
// megamorphic cache's sets hold most of, and a constructor's keyed store
// of a constant key, Typescript's BlockIntrinsics'. With
// QUICKJS_JIT_CORPUS, Octane's benchmarks of QUICKJS_JIT_CORPUS_SUITES
// (Typescript where it names none) too, run as v8bench runs them: their
// AST walks are where the misses had come at other points of the run.
func TestJITTwoRuntimesCompileAlike(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function BlockIntrinsics() { this.prototype = undefined; this["constructor"] = undefined; this.toString = undefined }
		function Node(kind, i) { this.kind = kind; this.minChar = i; this.limChar = i + 1 }
		const kinds = []
		for (let k = 0; k < 40; k++) kinds.push(function (i) { Node.call(this, k, i); this["f" + k] = i })
		const nodes = []
		for (let i = 0; i < 400; i++) {
			const n = new kinds[i % 40](i)
			n["u" + i] = i
			nodes.push(n)
		}
		function pre(n) { return n.kind + (n.parent === undefined ? 1 : 0) }
		function walk(round) {
			let t = 0
			for (let i = 0; i < nodes.length; i++) t = (t + pre(nodes[(i * 7 + round) % nodes.length])) | 0
			new BlockIntrinsics()
			return t
		}`
	traces := make([][]JITTierEvent, 2)
	var events []JITTierEvent
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.tierHook = func(e JITTierEvent) { events = append(events, e) }
	})
	jitNoInlineForTest(t, "pre")
	holdCollector(t)
	var hold []any
	for k := range traces {
		events = nil
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 8, 8
		r.jitStress = jitStressConfig{}
		if k == 1 {
			// The second runtime's heap moved on by what it made first.
			for i := range 5000 {
				hold = append(hold, make([]byte, 16+i%200))
			}
		}
		if _, err := r.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
		for i := range 40 {
			if _, err := r.Run(compileForTest(t, fmt.Sprintf("walk(%d)", i))); err != nil {
				t.Fatal(err)
			}
		}
		traces[k] = events
	}
	runtime.KeepAlive(hold)
	if len(traces[0]) == 0 {
		t.Fatal("nothing compiled: the test tests nothing")
	}
	jitSameTraces(t, traces)
	dir := os.Getenv("QUICKJS_JIT_CORPUS")
	if dir == "" {
		return
	}
	suites := os.Getenv("QUICKJS_JIT_CORPUS_SUITES")
	if suites == "" {
		suites = "Typescript"
	}
	files := []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
		"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js", "pdfjs.js",
		"box2d.js", "typescript.js", "typescript-input.js", "typescript-compiler.js"}
	for k := range traces {
		events = nil
		r := jitRuntimeForTest(t, Config{JIT: true})
		if k == 1 {
			for i := range 5000 {
				hold = append(hold, make([]byte, 16+i%200))
			}
		}
		if _, err := r.Run(compileForTest(t, `var print = function () {}, alert = function () {};`)); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			src, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(compileForTest(t, string(src))); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
		if _, err := r.Run(compileForTest(t, fmt.Sprintf(`(() => {
			for (const s of BenchmarkSuite.suites) {
				if (!(",%s,").includes(","+s.name+",")) continue;
				if (BenchmarkSuite.ResetRNG) BenchmarkSuite.ResetRNG();
				for (const b of s.benchmarks) { b.Setup(); b.run(); b.TearDown(); }
			}
		})()`, suites))); err != nil {
			t.Fatal(err)
		}
		traces[k] = events
	}
	runtime.KeepAlive(hold)
	jitSameTraces(t, traces)
}

// jitSameTraces fails t where two runtimes' tier traces differ, at the
// first event that does.
func jitSameTraces(t *testing.T, traces [][]JITTierEvent) {
	t.Helper()
	a, b := strings.Split(jitTierLinesFull(traces[0]), "\n"), strings.Split(jitTierLinesFull(traces[1]), "\n")
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			t.Fatalf("the two runtimes' tier traces differ at event %d of %d and %d:\n%s\nagainst\n%s", i, len(a)-1, len(b)-1, a[i], b[i])
		}
	}
	if len(a) != len(b) {
		t.Fatalf("one runtime's tier trace has %d events, the other's %d", len(a)-1, len(b)-1)
	}
}

// jitTierLinesFull is events a line each, with every count an event has.
func jitTierLinesFull(events []JITTierEvent) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%v %s %s %d %d %d %d %d %d %d\n", e.Deopt, e.Func.Name, e.Reason, e.Length, e.Invocations, e.Age, e.Budget, e.Deopts, e.Compiles, e.PC)
	}
	return b.String()
}
