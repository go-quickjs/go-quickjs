//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A compile in the background builds from the snapshot what the compile on
// the spot builds from the feedback: every question the build asks was
// asked ahead, and answers as the feedback does. The functions ask the
// questions the snapshot had not asked ahead -- a keyed store's site
// (KeyedStore), at a store and at an object literal's computed key; a call
// of a leaf through Function.prototype.call (ViaLeaf) and its leaf's kind
// (LeafKind); a leaf's call's pool (CreatePool) and its array made again
// (ExecReuse), whose second argument is the leaf -- each a miss, the compile
// built on the runtime's goroutine at its install from the feedback as it
// was then: AWFY's DeltaBlue's append, chainTest and projectionTest, Octane's
// PdfJS and TypeScript and the V8 suite's RegExp, 20 compiles in the
// three suites.
func TestJITBackgroundSnapshotBuildsAsTheFeedback(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := newTestRuntime(t, Config{JIT: true})
	r.jitStress = jitStressConfig{}
	r.SetJITBackground(false)
	src := `
		function keyed(o, keys, n) {
			for (let i = 0; i < n; i++) o[keys[i & 3]] = i
			return o.a + o.b
		}
		function defined(keys, n) {
			let s = 0
			for (let i = 0; i < n; i++) { const o = {[keys[i & 3]]: i}; s += o[keys[i & 3]] }
			return s
		}
		function owns(objs, n) {
			const has = Object.prototype.hasOwnProperty
			let s = 0
			for (let i = 0; i < n; i++) if (has.call(objs[i & 1], "a")) s++
			return s
		}
		const proto = {get k() { return 1 }}
		function creates(n) {
			let s = 0
			for (let i = 0; i < n; i++) { const o = Object.create(proto); s += o.k }
			return s
		}
		function execs(re, strs, n) {
			let s = 0
			for (let i = 0; i < n; i++) { const m = re.exec(strs[i & 1]); if (m) s += m[1].length }
			return s
		}
		const keys = ["a", "b", "c", "d"]
		for (let k = 0; k < 40; k++) {
			keyed({a: 0, b: 0, c: 0, d: 0}, keys, 500)
			defined(keys, 500)
			owns([{a: 1}, {b: 2}], 500)
			creates(500)
			execs(/(b+)c/, ["abbbc", "xx"], 500)
		}`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keyed", "defined", "owns", "creates", "execs"} {
		t.Run(name, func(t *testing.T) {
			cl := jitGlobalClosureForTest(r, name)
			j := jitRegJobForTest(t, r, cl)
			defer jit.PutWorkspace(j.w)
			snap := jitSnapBuildForTest(t, j, j.snap)
			live := jitSnapBuildForTest(t, j, r.jitRegFeedbackFor(cl, j.p))
			if snap != live {
				t.Errorf("built from the snapshot:\n%s\nfrom the feedback:\n%s", snap, live)
			}
		})
	}
}

// jitSnapBuildForTest is j's function built, as its compile builds it, from
// fb and optimized: its listing, or the miss of a snapshot's question.
func jitSnapBuildForTest(t *testing.T, j *jitRegJob, fb ssa.Feedback) (listing string) {
	t.Helper()
	w := jit.GetWorkspace()
	defer jit.PutWorkspace(w)
	defer func() {
		if v := recover(); v != nil {
			miss, ok := v.(jitSnapMiss)
			if !ok {
				panic(v)
			}
			listing = fmt.Sprint("a miss: ", miss)
		}
	}()
	f, err := ssa.BuildIn(&w.SSA, j.p, fb)
	if err != nil {
		t.Fatal(err)
	}
	f.FrameAt = j.frameAt
	jitRegShape(f, j.p, j.cl, j.c)
	ssa.Optimize(f)
	return f.String()
}

// A run of code compiled again in the background, installed while the run
// went on in the code it replaced, leaves that code at its next call of Go
// and goes on in the new code (jitRegCompileDueNow, jitRegGoOnAt), as it
// goes on in a compile made on the spot at its leaving for it; and a run
// whose own compile is done installs it at its call of Go, a check, as a
// native caller's ask for its callee's code does. A loop run once had gone
// on in its first code to its end -- AWFY DeltaBlue's chainTest made 12,000
// constructions through Go its code compiled again made natively -- and a
// constructor called natively from a loop had waited for a check with its
// compile done, its Vector making 7,000 arrays through Go meanwhile: the
// suite's time 1.24 times the foreground's.
//
// Each compile handed to the workers is waited for as it is handed over
// (jitTestConfig's bgDone), so that what is made through Go is what waits
// for the install and the run's leaving, not for the workers: on arm64's
// runners, their queue shared with the package's other tests, a compile
// had taken long enough for 9,000 to 100,000 constructions through Go.
func TestJITBackgroundRunsGoOnInTheirNewCode(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.bgDone = true })
	cases := []struct {
		name, src, op string
		n             uint64
	}{
		{"replaced", `
			class Variable { constructor() { this.value = 0; this.mark = 0 } }
			class Base { constructor(a, b) { this.a = a; this.b = b } }
			class Eq extends Base { constructor(a, b) { super(a, b); this.c = 1 } }
			function chain(n) {
				const vars = new Array(n + 1)
				for (let i = 0; i <= n; i++) vars[i] = new Variable()
				let s = 0
				for (let i = 0; i < n; i++) { const e = new Eq(vars[i], vars[i + 1]); s += e.c }
				return s
			}
			for (let k = 0; k < 4; k++) chain(50000)`, "new_w->function", 200000},
		{"done", `
			class V { constructor() { this.s = new Array(4); this.n = 0 } }
			class W { constructor(i) { this.v = new V(); this.i = i } }
			function make(n) { let s = 0; for (let i = 0; i < n; i++) s += new W(i).i & 1; return s }
			for (let k = 0; k < 4; k++) make(50000)`, "new_w->Array", 200000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			made := func(background bool) uint64 {
				r := newTestRuntime(t, Config{JIT: true})
				r.jitStress = jitStressConfig{}
				r.SetJITBackground(background)
				r.SetJITReport(true)
				if _, err := r.Run(compileForTest(t, tc.src)); err != nil {
					t.Fatal(err)
				}
				var n uint64
				for k, v := range r.RegNativeStats().GoCallOps {
					if strings.HasPrefix(k, tc.op) {
						n += v
					}
				}
				return n
			}
			fg, bg := made(false), made(true)
			t.Logf("%s: %d in the background, %d on the spot", tc.op, bg, fg)
			if bg > fg+tc.n/40 {
				t.Errorf("%s: %d made through Go in the background, %d on the spot, of %d", tc.op, bg, fg, tc.n)
			}
		})
	}
}

// Every method of a PC the register feedback has is asked ahead at the ops
// the build asks it at (jitSnapOps), but those asked at every op (Generic)
// or by a state's PC (jitRegSnapStatePC): one with no ops is a miss at every
// compile that asks it, as KeyedStore and ViaLeaf were.
func TestJITSnapshotAsksEveryPCMethod(t *testing.T) {
	t.Parallel()
	for m := range jitSnapNames {
		if !jitSnapAtPC[m] || jitRegSnapMethods&(1<<m) == 0 || jitRegSnapStatePC&(1<<m) != 0 || jitSnapMethod(m) == snapGeneric {
			continue
		}
		if m >= len(jitSnapOps) || len(jitSnapOps[m]) == 0 {
			t.Errorf("%s: asked at no op (jitSnapOps)", jitSnapNames[m])
		}
	}
}

// A native callee that calls functions natively is not judged by its calls
// of Go alone (jitRegCalleeProfit), as a run is not (jitRegRunGoProfit):
// the work of the functions it calls natively, which its calls of Go are
// measured against, is not counted in its own. A loop over a callback's
// calls whose callbacks were called through Go while they warmed up again
// after a deoptimization -- AWFY DeltaBlue's Vector.forEach -- was judged
// not to pay at each such spell, its native callers calling it through Go
// and the interpreter running it: 9,000 calls on the spot, up to 54,000 in
// the background.
func TestJITCalleeCallingNativelyIsNotJudgedByItsCallsOfGo(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `
		class Vec {
			constructor() { this.s = []; this.n = 0 }
			push(x) { this.s[this.n++] = x }
			forEach(fn) { for (let i = 0; i < this.n; i++) fn(this.s[i]) }
		}
		function make(k) {
			const v = new Vec()
			for (let i = 0; i < 6; i++) {
				const o = {a: i, m() { return this.a + k }}
				for (let j = 0; j < k; j++) o["p" + j] = j
				v.push(o)
			}
			return v
		}
		function sum(v, k) { let s = 0; v.forEach((o) => { s += o.m() + k }); return s }
		function xor(v) { let s = 0; v.forEach((o) => { s ^= o.a }); return s }
		function count(v) { let s = 0; v.forEach((o) => { if (o.m() > 2) s++ }); return s }
		let total = 0
		for (let k = 0; k < 8; k++) {
			const v = make(k)
			for (let r = 0; r < 10000; r++) total += sum(v, r) + xor(v) + count(v)
		}`
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			t.Parallel()
			r := newTestRuntime(t, Config{JIT: true})
			r.jitStress = jitStressConfig{}
			r.SetJITBackground(background)
			r.SetJITReport(true)
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			// Before, 820 on the spot and 1,025 in the background: the
			// retry after a judgment (jitNativeRetry).
			if n := r.RegNativeStats().GoCallOps["call_method_w->function with code"]; n > 100 {
				t.Errorf("forEach called through Go %d times of 240,000, its code native callers may call", n)
			}
		})
	}
}

// A compile in the background is built from feedback asked when it was
// prepared; what the code it replaces learns meanwhile at its exits it was
// not built for, and its install leaves the function marked to be
// compiled again for it (jitRegJob's asks), as code that learned is. React's
// createElement, compiled again in the background, met "div" at its read
// of a.defaultProps while the compile was under way: its site learned the
// primitive for good (jitPolySite's prim), the install cleared the mark,
// and its new code, built without the primitive, left native code at every
// such read and learned nothing more -- ssr's Render 348,000 host exits
// at get_prop in one run of three, each invocation finished in the
// interpreter, the run 1.43 times the one on the spot.
func TestJITBackgroundInstallKeepsWhatTheOldCodeLearned(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn.Name == "el" } })
	r := newTestRuntime(t, Config{JIT: true})
	r.jitStress = jitStressConfig{}
	r.SetJITBackground(false)
	r.SetJITReport(true)
	src := `
		const types = []
		for (let k = 0; k < 12; k++) { const o = {}; o["p" + k] = k; if (k & 1) o.defaultProps = {k}; types.push(o) }
		const ones = new Array(32).fill(1)
		function el(a, xs, n) {
			for (let i = 0; i < xs.length; i++) n += xs[i]
			if (a && a.defaultProps) n++
			return n
		}
		function objects(n) { let s = 0; for (let i = 0; i < n; i++) s = el(types[i % 12], ones, s); return s }
		function strings(n) { let s = 0; for (let i = 0; i < n; i++) s = el("div", ones, s); return s }
		for (let k = 0; k < 20; k++) objects(1200)`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "el")
	e := jitRegOfForTest(r, cl)
	if e == nil || e.code == nil || e.invalid {
		t.Fatalf("el has no valid code: %+v", e)
	}
	// Compiled again in the background, its install held off.
	r.SetJITBackground(true)
	r.jitStress = jitStressConfig{background: true, checks: 1000}
	if ne := r.jitRegReoptimize(cl, r.regOf(cl), e, -1); ne != nil {
		t.Fatal("compiled again on the spot")
	}
	j := r.jit.regPendingJobs[e.key]
	if j == nil {
		t.Fatal("not compiled again in the background")
	}
	// The snapshot answers as the feedback does when it is asked. Asked
	// once its worker is done with it, and the questions it notes the
	// build asked (jitSnapshot's uses) left as the worker's build left
	// them, for the install's replay.
	<-j.done
	uses := len(j.snap.s.uses)
	if snap, live := jitSnapBuildForTest(t, j, j.snap), jitSnapBuildForTest(t, j, r.jitRegFeedbackFor(cl, j.p)); snap != live {
		t.Fatalf("built from the snapshot:\n%s\nfrom the feedback:\n%s", snap, live)
	}
	j.snap.s.uses = j.snap.s.uses[:uses]
	// The code it replaces meets a string at a.defaultProps, and learns it.
	jitCallForTest(t, r, `strings(50)`, 50*32)
	if r.jit.regPendingJobs[e.key] != j || jitRegOfForTest(r, cl) != e {
		t.Fatal("installed before its Kth check")
	}
	if e.life.reoptAsks == j.asks {
		t.Fatal("the old code learned nothing from a string at a.defaultProps")
	}
	// A compile on the spot reads it as a primitive's too, which the
	// snapshot, asked before, does not.
	read := slices.IndexFunc(j.p.Code, func(in ir.Instruction) bool { return in.Op == ir.PropertyRead || in.Op == ir.ReferenceRead })
	if read < 0 {
		t.Fatal("el reads no property")
	}
	if site, _ := r.jitRegFeedbackFor(cl, j.p).Property(read); !site.Primitives {
		t.Fatal("the feedback does not answer the primitive the old code met at a.defaultProps")
	}
	if site, _ := j.snap.Property(read); site.Primitives {
		t.Fatal("the snapshot answers what the old code learned after it was asked")
	}
	j.snap.s.uses = j.snap.s.uses[:uses]
	// Installed at the next check, it is compiled again for the string.
	r.jitStress.checks = 1
	jitCallForTest(t, r, `objects(12)`, 12*32+6)
	ne := jitRegOfForTest(r, cl)
	if r.jit.regPendingJobs[e.key] != nil || ne == e || ne == nil || ne.code == nil {
		t.Fatalf("not installed: pending %v, entry %p, was %p", r.jit.regPendingJobs[e.key] != nil, ne, e)
	}
	if !ne.life.reopt {
		t.Error("installed with no mark to be compiled again for what the old code learned meanwhile")
	}
	before := r.RegNativeStats().HostOps["get_prop"]
	const calls = 20000
	jitCallForTest(t, r, fmt.Sprintf(`strings(%d)`, calls), calls*32)
	if n := r.RegNativeStats().HostOps["get_prop"] - before; n > calls/10 {
		t.Errorf("%d host exits at get_prop of %d reads of a string's a.defaultProps", n, calls)
	}
}
