//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regBothMarking runs f with the collector idle and then, native code
// seeing it mark, marking (jitMarkingForTest), as the JIT's marking rule
// asks of a test of a native path.
func regBothMarking(t *testing.T, f func(t *testing.T)) {
	t.Run("idle", f)
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		f(t)
	})
}

// TestJITRegNoContextDeclines runs a recursion of native calls deeper than
// the JIT has contexts at first, whose deepest levels call a compiled leaf,
// and checks that the contexts grow to its depth and Go never enters code
// past the last context: no run ends in the interpreter for want of one.
// Before, each such call entered its callee's code, found no context for
// its run and finished in the interpreter, counted as an entry that did no
// work: AWFY's Havlak, whose depth-first search recurses past the contexts,
// finished 2.7 million runs so, each judged against its code.
func TestJITRegNoContextDeclines(t *testing.T) {
	const src = `
function leaf(o) { return o.v + 1 }
function rec(d, o) { if (d == 0) return 0; var s = leaf(o); return s + rec(d - 1, o) }
function run() { var o = { v: 1 }, t = 0; for (var i = 0; i < 20; i++) t += rec(300, o); return t }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		r.SetJITReport(true)
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
			for range 6 {
				v, err := rt.Call(fn, Undefined, nil)
				got[i] = regNativeShow(rt, v, err)
			}
		}
		if got[0] != got[1] || got[0] != "12000" {
			t.Fatalf("with the JIT %q, without %q, want 12000", got[0], got[1])
		}
		if len(r.jit.ssaCtxs) < 300 {
			t.Errorf("the recursion 300 deep has %d contexts", len(r.jit.ssaCtxs))
		}
		ended := r.RegNativeStats().Ended
		t.Logf("contexts %d; ended %v", len(r.jit.ssaCtxs), ended)
		for k, n := range ended {
			if strings.HasPrefix(k, "no context") {
				t.Errorf("%d runs entered past the last context: %s", n, k)
			}
		}
	})
}

// regBothMarkingNative is regBothMarking with the native handlers storing
// as while the collector marks too (nativeMarking).
func regBothMarkingNative(t *testing.T, f func(t *testing.T)) {
	t.Run("idle", f)
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		nativeMarking(t, true)
		t.Cleanup(func() { nativeMarking(t, false) })
		f(t)
	})
}

// TestJITRegCountedTierUp calls a function from Go, one call at a time, and
// checks it is compiled once its runs have spent its budget, counted: not
// before jitCallThreshold runs, and at the call after the run that spent
// it -- the second call begins it with the first run counted (jitRegHot's
// warming spends what its first sample counted), the feedback
// dispatcher's returns count the rest down (jitRegArm), and the call after
// the last goes to Go (jitRegCallTier). Before, a sample of the interrupt
// budget stood for 4,096 calls, and a function called less often than
// that between two samples was compiled at its second or third sample, or
// never.
func TestJITRegCountedTierUp(t *testing.T) {
	const src = `
function f(o) { return o[0] + o[1] }
var o = [1, 2];
function g() { return f(o) }
`
	regBothMarkingNative(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		const calls = 50
		r.SetJITWarmUp(calls, calls)
		fv, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
		ov, _ := r.GetProp(Obj(r.Global()), r.Intern("o"))
		cc := fv.Object().fn().closure.closureCode
		at := 0
		for k := 1; k <= calls+4; k++ {
			v, err := r.Call(fv, Undefined, []Value{ov})
			if got := regNativeShow(r, v, err); got != "3" {
				t.Fatalf("call %d: %q, want 3", k, got)
			}
			if at == 0 && cc.jitReg != nil {
				at = k
			}
		}
		if at != calls+1 {
			t.Fatalf("compiled at call %d, want %d: the call after the %d runs of its budget", at, calls+1, calls)
		}
		if e := cc.jitReg; e.life.starts == 0 {
			t.Errorf("never entered: %+v", e.life)
		}
	})
}

// TestJITRegInterpretedCallsEnter compiles a function first, then has a
// caller the interpreter runs -- the JIT refuses it -- call it in a loop,
// and checks that every call goes to its code, from the native call handler
// with no exit (nativeJITCall): the site's cache is filled with the callee
// that has code (nativeCallMiss, regCallSlow), as with any other, and its
// calls go to the JIT. Before, the site was marked for regCallSlow, every
// call of it made by the generic call path, and the call handler left a
// callee with code to rhCallW by an exit.
func TestJITRegInterpretedCallsEnter(t *testing.T) {
	const src = `
function f(o, i) { return o[0] + i }
function g(n) { var o = [1], s = 0; for (var i = 0; i < n; i++) s += f(o, i); return s }
`
	regBothMarkingNative(t, func(t *testing.T) {
		nativeCounting(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		fv, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
		gv, _ := r.GetProp(Obj(r.Global()), r.Intern("g"))
		fcl, gcl := fv.Object().fn().closure, gv.Object().fn().closure
		if fcl.reg == nil {
			r.regOf(fcl)
		}
		e := r.jitRegFor(fcl, fcl.reg)
		if e == nil || e.code == nil {
			t.Fatalf("f not compiled: %+v", e)
		}
		fcl.closureCode.setJITReg(e)
		gcl.closureCode.jitRegRefused = true
		const n = 2000
		exits := atomic.LoadInt64(&nativeExits[bytecode.RCallW])
		v, err := r.Call(gv, Undefined, []Value{Float(n)})
		if got := regNativeShow(r, v, err); got != "2001000" {
			t.Fatalf("g(%d) = %q, want 2001000", n, got)
		}
		if d := atomic.LoadInt64(&nativeExits[bytecode.RCallW]) - exits; d > 2 {
			t.Errorf("%d of %d calls left the native handlers by an exit", d, n)
		}
		// Each call goes to the JIT, which enters the code or, judged
		// slow, declines it (jitRegStretchWork).
		if l := fcl.closureCode.jitReg.life; l.starts+l.slowCalls < n {
			t.Errorf("f's code entered %d times and declined %d for %d calls", l.starts, l.slowCalls, n)
		}
	})
}

// TestJITRegLoopOfGoCallsLeaves runs a loop entered by on-stack replacement
// whose every turn is a call of Go and little else, and checks that once
// its calls are judged not to pay (jitRegRunGoProfit) the run leaves after
// the call it judged at, and the interpreter runs the loop. Before, the
// judgement was made and its result dropped: the loop ran its calls of Go
// natively to its end.
func TestJITRegLoopOfGoCallsLeaves(t *testing.T) {
	const src = `
function f(n) { var s = 0; for (var i = 0; i < n; i++) s += JSON.stringify(i & 7).length; return s }
`
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("f"))
			v, err := rt.Call(fn, Undefined, []Value{Float(200000)})
			got[i] = regNativeShow(rt, v, err)
		}
		if got[0] != got[1] || got[0] != "200000" {
			t.Fatalf("with the JIT %q, without %q, want 200000", got[0], got[1])
		}
		fv, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
		e := fv.Object().fn().closure.closureCode.jitReg
		if e == nil {
			t.Fatalf("f not compiled")
		}
		st := e.life.ssaStats
		t.Logf("loopSlow %v, calls of Go %d, judged %d", e.life.loopSlow, st.goCalls, st.runGoCalls)
		if !e.life.loopSlow {
			t.Skipf("f's calls of Go were judged to pay")
		}
		if st.goCalls > 4*jitSSAProbe {
			t.Errorf("%d calls of Go made natively after the loop was judged slow at %d", st.goCalls, st.runGoCalls)
		}
	})
}

// TestJITRegLoopMeanInProgramUnits checks that the loops' mean length a
// stretch's back edges are counted by is in the program's instructions, the
// units jitSSAMinWork is in: the distance from each backward jump's target
// to it, as lowered (irLoopMean).
func TestJITRegLoopMeanInProgramUnits(t *testing.T) {
	const src = `
function f(a) { var s = 0; for (var i = 0; i < a.length; i++) s += a[i] * 2 + 1; return s }
`
	rts := regNativeRuntimes(t, src)
	r := rts[0]
	fv, _ := r.GetProp(Obj(r.Global()), r.Intern("f"))
	cl := fv.Object().fn().closure
	if cl.reg == nil {
		r.regOf(cl)
	}
	e := r.jitRegFor(cl, cl.reg)
	if e == nil || e.code == nil {
		t.Fatalf("f not compiled: %+v", e)
	}
	bl := cl.reg.rc.BackLive
	if len(bl) != 2 {
		t.Fatalf("f has %d loops, want 1", len(bl)/2)
	}
	q := int(bl[0])
	head := q + 1 + int(regJumpOff(regAt(cl.reg.code, q)))
	want := int(e.at[q+1]) - int(e.at[head])
	if e.loopMean != want {
		t.Errorf("loop mean %d, want %d, the program's instructions from %d to %d (register: %d)", e.loopMean, want, e.at[head], e.at[q+1], q-head+1)
	}
}

// TestJITRegSlowResumptionsSkip resumes, through a yield*, generators whose
// code leaves at each resumption -- at the yield* it does not make
// natively, at a yield with a capture open -- and checks that once judged
// slow their resumptions are no longer entered, the next jitSSAProbe of
// them run in the interpreter without a look at the code (jitResumeSkip),
// until retried. Before, every resumption entered the code, which left at
// once: AWFY-like generator_delegate's 400,000 resumptions each an entry
// and an exit.
func TestJITRegSlowResumptionsSkip(t *testing.T) {
	const src = `
function* count(n) { for (var i = 0; i < n; i++) yield i }
function* delegate(n) { yield* count(n) }
function run(n) { var s = 0; for (var j = 0; j < n; j++) for (var v of delegate(100)) s += v; return s }
`
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITReport(true)
		var got [2]string
		for i, rt := range rts {
			fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
			for range 4 {
				v, err := rt.Call(fn, Undefined, []Value{Float(1000)})
				got[i] = regNativeShow(rt, v, err)
			}
		}
		if got[0] != got[1] || got[0] != "4950000" {
			t.Fatalf("with the JIT %q, without %q, want 4950000", got[0], got[1])
		}
		d, ok := regReportOf(r, "delegate")
		t.Logf("delegate %+v; slow resumptions %d", d, r.jit.slowResumes)
		if !ok || d.Status != "slow" {
			t.Skipf("delegate's code not judged slow: %+v", d)
		}
		// 404,000 resumptions: those after the judgement are entered once
		// in jitSSAProbe+1 at most, until a retry.
		if d.Native > 404000/jitSSAProbe*2 {
			t.Errorf("delegate's code entered %d times", d.Native)
		}
	})
}
