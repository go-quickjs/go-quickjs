//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regWarmGenSrc has a generator doing work between yields, which its own
// loop runs, stepped by for...of; a generator whose parameters have a
// default, run past them at its making; an async function of a loop that
// does not await, every call of it a first run; and one that awaits
// within its loop.
const regWarmGenSrc = `
function* busy(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		s = (s + i * 3) | 0;
		if ((i & 255) == 0) yield s;
	}
	return s;
}
function drive(n) {
	let x = 0;
	for (const v of busy(n)) x ^= v;
	return x;
}
function* pair(a, b = a + 1) { yield a; yield b; }
function pairs(n) {
	let s = 0;
	for (let j = 0; j < n; j++) for (const v of pair(j)) s += v;
	return s;
}
async function sum(n) {
	let s = 0;
	for (let i = 0; i < n; i++) s += i & 7;
	return s;
}
var out = [];
function firsts(n) {
	for (let j = 0; j < n; j++) sum(j).then(function (v) { if (j == n - 1) out.push(v) });
	return n;
}
async function waits(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		s += i;
		if (i % 100 == 0) s += await (i % 7);
	}
	return s;
}
function awaits(n) { waits(n).then(function (v) { out.push(v) }); return n }
`

// TestJITRegSuspendNative runs a generator and async functions whose
// register code suspends at a yield, an await and its parameters' end, with
// the JIT and without, the collector idle and marking: native code suspends
// the frame there itself (jitRegSuspend), where each suspension had been a
// run ended in the interpreter, the frame handed to runReg only to suspend;
// and an async function's every first run enters its code past gen_entry
// (regGenRun).
func TestJITRegSuspendNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regWarmGenSrc)
			rts[0].SetJITReport(true)
			rts[0].jitStress = jitStressConfig{}
			calls := []struct {
				name string
				n    float64
				want string
			}{
				{"drive", 100000, "1938755200"},
				{"pairs", 2000, "4000000"},
				{"firsts", 300, "300"},
				{"awaits", 3000, "3000"},
			}
			var got [2]string
			for i, r := range rts {
				var b strings.Builder
				for _, c := range calls {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
					for k := range 20 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						if s := regNativeShow(r, v, err); s != c.want {
							t.Fatalf("%s, call %d: %s, want %s", c.name, k, s, c.want)
						}
					}
				}
				if err := r.DrainJobs(); err != nil {
					t.Fatal(err)
				}
				v, _ := r.GetProp(Obj(r.Global()), r.Intern("out"))
				b.WriteString(regNativeShow(r, v, nil))
				got[i] = b.String()
			}
			if got[0] != got[1] {
				t.Fatalf("with the JIT %q, without %q", got[0], got[1])
			}
			if want := strings.Repeat("1039,", 20) + strings.Repeat("4498586,", 19) + "4498586"; got[1] != want {
				t.Fatalf("out %q, want %q", got[1], want)
			}
			r := rts[0]
			st := r.RegNativeStats()
			for why, n := range st.Ended {
				if strings.Contains(why, "at yield") || strings.Contains(why, "at end_params") ||
					strings.Contains(why, "at await in waits") {
					t.Errorf("%d runs ended in the interpreter: %s", n, why)
				}
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("sum"))
			e := r.jit.regCache[r.jitRegKeyOf(fn.Object().fn().closure)]
			if e == nil || e.code == nil {
				t.Fatal("sum not compiled")
			}
			if r.jit.regResumes < 1000 {
				t.Errorf("%d first runs and resumptions entered native code", r.jit.regResumes)
			}
			t.Logf("ended: %v; resumes %d", st.Ended, r.jit.regResumes)
		})
	}
}

// TestJITRegGenLearnedJudgedAfresh checks that code compiled again for a
// generator's step it learned (jitRegGenSeen) has the loops it was judged
// slow for judged afresh (jitRegCached): left slow, the consumer's loop,
// whose steps had each been a call of Go, stayed with the interpreter until
// a retry a thousand samples later, generator_next's first round twice the
// stack form's.
func TestJITRegGenLearnedJudgedAfresh(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regWarmGenSrc)
	r := rts[0]
	r.jitStress = jitStressConfig{}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("pairs"))
	for range 4 {
		if v, err := r.Call(fn, Undefined, []Value{Float(2000)}); regNativeShow(r, v, err) != "4000000" {
			t.Fatalf("pairs: %s", regNativeShow(r, v, err))
		}
	}
	cl := fn.Object().fn().closure
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil {
		t.Fatal("pairs not compiled")
	}
	// Judged slow at its loop, then a generator's step learned.
	e.life.loopSlow, e.life.entrySlow = true, true
	r.jitRegGenLearned(cl, e)
	if !e.life.genLearned || !e.life.reopt {
		t.Fatal("the step learned asked for no compile again")
	}
	ne := r.jitRegReoptimize(cl, cl.reg, e, -1)
	if ne == nil || ne.code == nil {
		t.Fatal("not compiled again")
	}
	if ne.life.loopSlow || ne.life.genLearned {
		t.Errorf("compiled again: loopSlow %v, genLearned %v", ne.life.loopSlow, ne.life.genLearned)
	}
	if v, err := r.Call(fn, Undefined, []Value{Float(2000)}); regNativeShow(r, v, err) != "4000000" {
		t.Fatalf("pairs: %s", regNativeShow(r, v, err))
	}
}

// regWarmCallSrc has a call of eight functions, a table's, by a loop, as
// literal_sites_read has it; and a call of four that a loop makes only
// past its first half, its site's cache empty when the loop is compiled.
const regWarmCallSrc = `
var fs8 = [
	function (j) { return { x: j, y: 1 } }, function (j) { return { x: j, y: 2 } },
	function (j) { return { x: j, y: 3 } }, function (j) { return { x: j, y: 4 } },
	function (j) { return { x: j, y: 5 } }, function (j) { return { x: j, y: 6 } },
	function (j) { return { x: j, y: 7 } }, function (j) { return { x: j, y: 8 } },
];
function rd(o) { return o.x; }
function sites(n) {
	var s = 0;
	for (var j = 0; j < n; j++) s += rd(fs8[j & 7](j));
	return s;
}
var fs4 = [function (a) { return a + 1 }, function (a) { return a + 2 }, function (a) { return a * 3 }, function (a) { return a - 4 }];
function late(n) {
	var s = 0;
	for (var j = 0; j < n; j++) {
		if (j >= n / 2) s += fs4[j & 3](j);
		else s += j & 1;
	}
	return s;
}
`

// TestJITRegCallRepeatsInvalidates runs a call of eight functions in a
// loop, with the JIT and without, the collector idle and marking: a call
// that leaves at a host exit for a function an earlier exit there taught
// its code, which the code was not compiled for, invalidates it
// (jitRegCallRepeats), the interpreter running the function until it is
// compiled again with it. Left valid, the code left at every other turn
// until its loop was judged slow for it, and the interpreter ran the loop
// for a thousand samples.
func TestJITRegCallRepeatsInvalidates(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regWarmCallSrc)
			rts[0].SetJITReport(true)
			rts[0].jitStress = jitStressConfig{}
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("sites"))
				for k := range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(100000)})
					if got := regNativeShow(r, v, err); got != "4999950000" {
						t.Fatalf("runtime %d, call %d: %s, want 4999950000", i, k, got)
					}
				}
			}
			r := rts[0]
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("sites"))
			cl := fn.Object().fn().closure
			l := r.jit.regLearnedOf(r.jitRegKeyOf(cl))
			st := r.RegNativeStats()
			for why, n := range st.Ended {
				if strings.HasPrefix(why, "slow loop") && strings.HasSuffix(why, " in sites") {
					t.Errorf("%d runs ended: %s", n, why)
				}
			}
			if l.loopSlow {
				t.Error("sites' loop judged slow")
			}
			if n := st.HostOps["call_method_w"]; n > 20 {
				t.Errorf("%d host exits at the call", n)
			}
			t.Logf("ended %v, hosts %v", st.Ended, st.HostOps)
		})
	}
}

// TestJITRegCallSettlesTogether checks that a call whose code knows none of
// its functions decides those it meets in its first jitCallsToInline calls
// of Go together (jitRegCallSettling), the code compiled again once for
// them all: learned one at a time, each compile again was put off by the
// next function met.
func TestJITRegCallSettlesTogether(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rts := regNativeRuntimes(t, regWarmCallSrc)
	var got [2]string
	for i, r := range rts {
		r.jitStress = jitStressConfig{}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("late"))
		for range 4 {
			v, err := r.Call(fn, Undefined, []Value{Float(400000)})
			got[i] = regNativeShow(r, v, err)
		}
	}
	if got[0] != got[1] || got[1] != "89999950000" {
		t.Fatalf("late: with the JIT %s, without %s, want 89999950000", got[0], got[1])
	}
	r := rts[0]
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("late"))
	cl := fn.Object().fn().closure
	rpc := -1
	for pc, w := range cl.reg.rc.Code {
		if w.Op() == bytecode.RCallMW {
			rpc = pc
		}
	}
	arr, _ := r.GetProp(Obj(r.Global()), r.Intern("fs4"))
	var xs []jitInline
	for k := range 4 {
		o, _ := r.GetProp(arr, r.Intern(fmt.Sprint(k)))
		xs = append(xs, jitInline{pc: int32(rpc), cl: o.Object().fn().closure, obj: o.Object()})
	}
	l := &jitEntry{jitLearned: &jitLearned{}}
	// Met twice, the first: the count goes on, kept once.
	for i, x := range []jitInline{xs[0], xs[0], xs[1]} {
		if met, settled := r.jitRegCallSettling(l, cl.reg, rpc, x); settled || met != nil {
			t.Fatalf("call %d settled", i)
		}
	}
	if len(l.settling) != 2 {
		t.Fatalf("%d settling, want 2", len(l.settling))
	}
	met, settled := r.jitRegCallSettling(l, cl.reg, rpc, xs[2])
	if !settled || len(met) != 2 || met[0].obj != xs[0].obj || met[1].obj != xs[1].obj || len(l.settling) != 0 {
		t.Fatalf("fourth call: settled %v, met %d, %d left settling", settled, len(met), len(l.settling))
	}
	if l.callSites[rpc] != jitCallNative {
		t.Errorf("call's state %d, want %d", l.callSites[rpc], jitCallNative)
	}
	// Settled: later functions are learned as they come.
	if met, settled := r.jitRegCallSettling(l, cl.reg, rpc, xs[3]); !settled || met != nil {
		t.Errorf("after settling: settled %v, met %d", settled, len(met))
	}
}

// regWarmBuiltinSrc calls built-ins with frames of their own from native
// code's calls of Go: JSON.parse with and without a reviver, one that
// throws, Object.create and Object.setPrototypeOf, a built-in called in
// tail position, and one whose call reads the caller's frame (a stack
// trace).
const regWarmBuiltinSrc = `
"use strict";
function parses(n) {
	let s = 0, bad = 0;
	for (let j = 0; j < n; j++) {
		const o = JSON.parse('{"a":' + j + ',"b":[1,2,' + (j & 7) + ']}');
		s += o.a + o.b[2];
		if ((j & 15) == 0) s += JSON.parse('[1,2,3]', function (k, v) { return typeof v == "number" ? v * 2 : v })[2];
		try { JSON.parse(j & 31 ? '1' : '{') } catch (e) { if (e instanceof SyntaxError) bad++ }
	}
	return s + ":" + bad;
}
function creates(n) {
	let s = 0;
	for (let j = 0; j < n; j++) {
		const a = Object.create({ base: j });
		a.i = 1;
		const b = Object.setPrototypeOf({ v: 2 }, a);
		s += b.base + b.i + b.v;
	}
	return s;
}
function keys(o) { return Object.keys(o) }
function tails(n) {
	let s = 0;
	for (let j = 0; j < n; j++) s += keys({ a: j, b: 2 }).length;
	return s;
}
function traces(n) {
	let s = 0;
	for (let j = 0; j < n; j++) {
		const e = Reflect.construct(Error, ["x" + (j & 3)]);
		s += e.message.length + (e.stack.indexOf("traces") >= 0 ? 1 : 0);
	}
	return s;
}
`

// TestJITRegBuiltinCalls runs calls of built-ins with frames of their own
// that native code leaves to Go, with the JIT and without, the collector
// idle and marking: Go calls such a built-in directly (jitRegBuiltin,
// jitRegNativeMade), rather than through regCallSlow's search of the site's
// cache and the generic call.
func TestJITRegBuiltinCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regWarmBuiltinSrc)
			rts[0].SetJITReport(true)
			rts[0].jitStress = jitStressConfig{}
			calls := []struct {
				name string
				n    float64
				want string
			}{
				{"parses", 3000, "4510128:94"},
				{"creates", 3000, "4507500"},
				{"tails", 3000, "6000"},
				{"traces", 3000, "9000"},
			}
			for _, c := range calls {
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(c.name))
					for k := range 6 {
						v, err := r.Call(fn, Undefined, []Value{Float(c.n)})
						if got := regNativeShow(r, v, err); got != c.want {
							t.Fatalf("runtime %d: %s, call %d: %s, want %s", i, c.name, k, got, c.want)
						}
					}
				}
			}
			st := rts[0].RegNativeStats()
			if st.GoCallOps["call_method_w->parse"] == 0 {
				t.Errorf("JSON.parse not called from native code: %v", st.GoCallOps)
			}
			t.Logf("calls of Go: %v", st.GoCallOps)
		})
	}
}

// regWarmLoopsSrc is EarleyBoyer's deriv_trees' loop2 and loop3 in
// small: a closure made anew at each turn of a loop, whose own loop calls
// a function and a closure it makes anew at each turn.
const regWarmLoopsSrc = `
function deriv(a, b) { return a * 3 + b; }
function outer(n) {
	var total = 0;
	for (var j = 0; j < n; j++) {
		var loop2 = function (k, acc) {
			while (true) {
				if (k < 0) return acc;
				var t = deriv(k, j);
				var loop3 = function (m, s) { return m > 0 ? s + m : s; };
				acc = loop3(t & 7, acc);
				k--;
			}
		};
		total += loop2(j & 15, 0);
	}
	return total;
}
`

// TestJITRegMadeAnewCallsSettle runs regWarmLoopsSrc, with the JIT and
// without, the collector idle and marking: the calls of the closures made
// anew, whose functions the code learns at its exits, leave at no more
// than a few host exits once learned (jitRegCallRepeats) -- EarleyBoyer's
// loop2 had left at 4674 of them, 7316 exits in all, each run ending in
// the interpreter.
func TestJITRegMadeAnewCallsSettle(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regWarmLoopsSrc)
			rts[0].SetJITReport(true)
			rts[0].jitStress = jitStressConfig{}
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("outer"))
				for k := range 10 {
					v, err := r.Call(fn, Undefined, []Value{Float(20000)})
					if got := regNativeShow(r, v, err); got != "570000" {
						t.Fatalf("runtime %d, call %d: %s, want 570000", i, k, got)
					}
				}
			}
			st := rts[0].RegNativeStats()
			if n := st.HostOps["call_w"]; n > 10 {
				t.Errorf("%d host exits at call_w: %v", n, st.Ended)
			}
			t.Logf("hosts %v, ended %v", st.HostOps, st.Ended)
		})
	}
}

// regWarmSmallSrc calls a tiny callee and a larger one at calls made in one
// of thirty-two of a function's runs, past an early return, below V8's
// minimum frequency as it runs (0.05); and the larger one at a call made at
// every run.
const regWarmSmallSrc = `
function Pt(x, y) { this.x = x; this.y = y }
function tiny(o) { return o.a }
function big(o) { return new Pt(o.a + o.b, o.a - o.b) }
function rare(o, k) {
	if (k & 31) return 0;
	var t = tiny(o);
	return t + big(o).x;
}
function often(o, k) { return big(o).y + k }
function drive(n) {
	var s = 0, o = { a: 1, b: 2 };
	for (var k = 0; k < n; k++) s += rare(o, k) + often(o, k);
	return s;
}
`

// TestJITRegSmallCalleeAtColdCall pins V8's rule as Node runs it,
// Turbofan's: at a call made in one of thirty-two of its function's runs,
// below minInliningFrequency, no callee is inlined, however small -- tiny,
// within V8's max_inlined_bytecode_size_small, nor big -- the frequency
// asked before a small function is forced in (JSInliningHeuristic); big is
// inlined where its call is made at every run. The exemption of a small
// callee at a cold call is Turbolev's, which Node does not run. Exact
// answers with the JIT and without, the collector idle and marking.
func TestJITRegSmallCalleeAtColdCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regWarmSmallSrc)
			rts[0].jitStress = jitStressConfig{}
			for i, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
				for k := range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(4000)})
					if got := regNativeShow(r, v, err); got != "7994500" {
						t.Fatalf("runtime %d, call %d: %s, want 7994500", i, k, got)
					}
				}
			}
			r := rts[0]
			if !regNativeOn {
				t.Skip("the Go dispatcher counts no calls: no frequency is known")
			}
			inlined := func(name string) map[string]bool {
				fv, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				cl := fv.Object().fn().closure
				e := r.jit.regCache[r.jitRegKeyOf(cl)]
				if e == nil || e.p == nil {
					t.Fatalf("%s not compiled", name)
				}
				rf := r.jitRegFeedbackFor(cl, e.p)
				for pc, in := range e.p.Code {
					if in.Op == ir.Host && in.Window && cl.reg.rc.Code[e.p.Origin[pc]].Op() == bytecode.RCallW {
						if f, ok := rf.Frequency(pc); name == "rare" && (!ok || f >= ssa.MinInliningFrequency) {
							t.Fatalf("rare's call at %d: frequency %v (%v), want one below %v", pc, f, ok, ssa.MinInliningFrequency)
						}
					}
				}
				f, err := ssa.BuildWith(e.p, rf)
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]bool{}
				for _, callee := range []string{"tiny", "big"} {
					if slices.Contains(f.Inlined, jitClosureForTest(t, r, callee)) {
						got[callee] = true
					}
				}
				return got
			}
			if got := inlined("rare"); len(got) != 0 {
				t.Errorf("rare inlined %v, want neither tiny nor big", got)
			}
			if got := inlined("often"); len(got) != 1 || !got["big"] {
				t.Errorf("often inlined %v, want big alone", got)
			}
		})
	}
}
