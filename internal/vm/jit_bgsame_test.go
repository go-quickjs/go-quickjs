//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
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
func TestJITBackgroundRunsGoOnInTheirNewCode(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
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
