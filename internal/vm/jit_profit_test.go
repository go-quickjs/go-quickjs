//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// The register form's judgements of whether a native callee's code pays: a
// small hot callee its native callers call stays native, or is called
// natively again once its retry is due; a callee that really costs more
// natively is declined.

// regProfitRun runs src on both of rts, calling fn with args times times,
// and returns what the last call answered in each, which must be want.
func regProfitRun(t *testing.T, rts [2]*Runtime, fn string, times int, want string, args ...Value) {
	t.Helper()
	var got [2]string
	for i, rt := range rts {
		f, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(fn))
		for range times {
			v, err := rt.Call(f, Undefined, args)
			got[i] = regNativeShow(rt, v, err)
		}
	}
	if got[0] != got[1] || got[0] != want {
		t.Fatalf("%s: with the JIT %q, without %q, want %q", fn, got[0], got[1], want)
	}
}

// regProfitEntry is the register code of the global function name of r.
func regProfitEntry(t *testing.T, r *Runtime, name string) (*closure, *jitRegEntry) {
	t.Helper()
	fv, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
	cl := fv.Object().fn().closure
	e := cl.closureCode.jitReg
	if e == nil || e.code == nil || e.life == nil {
		t.Fatalf("%s has no register code: %+v", name, e)
	}
	return cl, e
}

// TestJITRegNotNativeRetriedByNativeCallers has a callee its native caller
// no longer calls natively (notNative), judged slow too, which Go does not
// enter (entrySlow), and checks that the caller's calls of it, which Go
// makes, count toward its being called natively again (jitRegRetryNative):
// after jitNativeRetry of them the caller calls it natively. Counted only
// at the calls Go entered it for, which it did not, it stayed in the
// interpreter: AWFY's CD's Vector and append, 700,000 calls a run.
func TestJITRegNotNativeRetriedByNativeCallers(t *testing.T) {
	t.Parallel()
	const src = `
function v(n) { var a = new Array(n & 3); return a.length + 1 }
function run(n) { var s = 0; for (var i = 0; i < n; i++) s += v(i); return s }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		r.SetJITReport(true)
		regProfitRun(t, rts, "run", 4, "2500", Float(1000))
		_, ve := regProfitEntry(t, r, "v")
		l := r.jit.regLinks[ve.key]
		if l == nil || l.entry == 0 {
			t.Fatalf("v is not called natively: link %+v", l)
		}
		// Judged not to pay as a native callee and for Go's entries, as
		// jitRegCalleeProfit leaves it, its retry far off.
		life := ve.life
		life.notNative, life.nativeRetry, life.nativeBackoff = true, 0, 1
		life.entrySlow, life.slowSince, life.slowBackoff = true, life.slowCalls, jitNativeBackoffs
		l.clearCode()
		l.in = 0
		regProfitRun(t, rts, "run", 2, "2500", Float(1000))
		t.Logf("notNative %v, retries %d, native calls since %d", life.notNative, life.nativeRetry, l.in)
		if life.notNative || l.entry == 0 {
			t.Fatalf("v not called natively again after %d calls of it Go made: notNative %v", 2*1000, life.notNative)
		}
		if l.in < 900 {
			t.Errorf("v called natively %d times of the 976 after its retry", l.in)
		}
	})
}

// TestJITRegCalleeJudgedAfresh checks that a native callee's calls of Go
// are counted afresh with its native callers' calls (its link's in) --
// when it is compiled again (jitRegCached), and when native callers call
// it again (jitRegRetryNative): counted over its compiles against
// calls counted only since, the callee was judged not to pay at its next
// look. AWFY's Json's putIntoMap, compiled again, was judged so at once.
func TestJITRegCalleeJudgedAfresh(t *testing.T) {
	t.Parallel()
	const src = `
function v(n) { var a = new Array(n & 3); return a.length + 1 }
function run(n) { var s = 0; for (var i = 0; i < n; i++) s += v(i); return s }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		regProfitRun(t, rts, "run", 4, "2500", Float(1000))
		cl, ve := regProfitEntry(t, r, "v")
		// What earlier code of it did as a native callee: many calls of Go,
		// little work between them.
		ve.life.ssaStats.calleeGoCalls, ve.life.ssaStats.calleeWork = 1<<20, 0
		ne := r.jitRegCompileFor(cl, cl.reg, ve.key, ve.callee)
		if ne == nil || ne.code == nil {
			t.Fatalf("v not compiled again: %+v", ne)
		}
		cl.closureCode.setJITReg(ne)
		if st := ne.life.ssaStats; st.calleeGoCalls != 0 || st.calleeWork != 0 {
			t.Fatalf("compiled again, v's calls of Go as a callee counted on: %d, work %d", st.calleeGoCalls, st.calleeWork)
		}
		ne.life.ssaStats.calleeGoCalls = 1 << 20
		ne.life.notNative, ne.life.nativeRetry, ne.life.nativeBackoff = true, 0, 1
		for range jitNativeRetry {
			r.jitRegRetryNative(ne)
		}
		if ne.life.notNative || ne.life.ssaStats.calleeGoCalls != 0 {
			t.Fatalf("retried, v notNative %v, its calls of Go as a callee counted on: %d", ne.life.notNative, ne.life.ssaStats.calleeGoCalls)
		}
		regProfitRun(t, rts, "run", 4, "2500", Float(1000))
		if ne.life.notNative {
			t.Errorf("v judged not to pay as a native callee: calls of Go %d, work %d, native calls %d",
				ne.life.ssaStats.calleeGoCalls, ne.life.ssaStats.calleeWork, r.jit.regLinks[ne.key].in)
		}
	})
}

// TestJITRegCalleeThatLosesDeclined checks the other side of the native
// callee's judgement (jitRegCalleeProfit): heavy, called natively, makes
// ten calls of Go a call with a few instructions between, which cost it
// more than its native callers' calls save -- native callers call it
// through Go (notNative) and Go does not enter it (entrySlow); light makes
// one a call after a loop of forty turns, and its native callers keep
// calling it.
func TestJITRegCalleeThatLosesDeclined(t *testing.T) {
	t.Parallel()
	const src = `
function heavy(x) { var s = 0; for (var k = 0; k < 10; k++) s += JSON.stringify((x + k) & 7).length; return s }
function light(x) { var s = 0; for (var k = 0; k < 40; k++) s = (s + x * k) & 1023; return s + JSON.stringify(x & 7).length }
function run(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + heavy(i) + light(i)) & 65535; return s }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		want := ""
		{
			fv, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern("run"))
			v, err := rts[1].Call(fv, Undefined, []Value{Float(5000)})
			want = regNativeShow(rts[1], v, err)
		}
		regProfitRun(t, rts, "run", 3, want, Float(5000))
		_, he := regProfitEntry(t, r, "heavy")
		_, le := regProfitEntry(t, r, "light")
		t.Logf("heavy: notNative %v slow %v calls of Go %d work %d; light: notNative %v calls of Go %d work %d",
			he.life.notNative, he.life.entrySlow, he.life.ssaStats.calleeGoCalls, he.life.ssaStats.calleeWork,
			le.life.notNative, le.life.ssaStats.calleeGoCalls, le.life.ssaStats.calleeWork)
		if !he.life.notNative || !he.life.entrySlow {
			t.Errorf("heavy, ten calls of Go a call, still called natively: notNative %v slow %v", he.life.notNative, he.life.entrySlow)
		}
		if le.life.notNative {
			t.Errorf("light, a loop of forty turns a call of Go, judged not to pay as a native callee")
		}
	})
}

// TestJITRegInlinedShapeMissCompiledAgain inlines a callee whose method
// read met two classes' objects when its caller was compiled, then runs
// the caller over a third's, and checks that the caller does not leave
// native code at every call, nor do its native callers stop calling it;
// and that a host exit at the read, its site not megamorphic, is a shape
// miss (jitRegShapeMiss), which has the caller compiled again for the new
// shape. AWFY's Dictionary.at, compiled while DeltaBlue's keys
// were hashed, left so at each of Havlak's two million calls.
func TestJITRegInlinedShapeMissCompiledAgain(t *testing.T) {
	t.Parallel()
	const src = `
class A1 { constructor(v) { this.v = v } h() { return this.v } }
class A2 { constructor(v) { this.u = 0; this.v = v } h() { return this.v } }
class B { constructor(v) { this.w = 0; this.x = 0; this.v = v } h() { return this.v * 2 } }
function hash(k) { if (!k) return 0; var x = k.h(); return x ^ (x >>> 16) }
function at(k) { var x = hash(k); return x + 1 }
function run(o, n) { var s = 0; for (var i = 0; i < n; i++) s += at(o); return s }
function runA1(n) { return run(new A1(1), n) }
function runA2(n) { return run(new A2(1), n) }
function runB(n) { return run(new B(2), n) }
`
	regBothMarking(t, func(t *testing.T) {
		// at called natively by run, hash inlined in it, its method
		// called natively, as Havlak's Dictionary.at and customHash are.
		jitTestSwitch(t, func(jc *jitTestConfig) {
			jc.noInline = func(fn *bytecode.Function) bool { return fn.Name == "at" || fn.Name == "h" }
		})
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		r.SetJITReport(true)
		// A1's objects, then A2's, whose read deoptimizes the code at it,
		// which is compiled again with the read's two cases and leaves
		// for Go at a third.
		regProfitRun(t, rts, "runA1", 6, "4000", Float(2000))
		regProfitRun(t, rts, "runA2", 6, "4000", Float(2000))
		left := func() uint64 {
			n := uint64(0)
			for k, c := range r.RegNativeStats().Ended {
				if strings.HasPrefix(k, "inlined callee left") {
					n += c
				}
			}
			return n
		}
		before := left()
		regProfitRun(t, rts, "runB", 40, "10000", Float(2000))
		n := left() - before
		_, ae := regProfitEntry(t, r, "at")
		t.Logf("inlined callee left %d times; at notNative %v, reopts %d", n, ae.life.notNative, ae.life.reopts)
		if n > 200 {
			t.Errorf("the inlined read left native code %d times of %d calls over B's objects", n, 40*2000)
		}
		if ae.life.notNative {
			t.Errorf("at's native callers stopped calling it")
		}
		// The miss, a host exit at hash's method read inlined: a map
		// check's failure while the read's site is not megamorphic, Go's
		// own read once it is (jitRegShapeMiss).
		hv, _ := r.GetProp(Obj(r.Global()), r.Intern("hash"))
		hcl := hv.Object().fn().closure
		read := -1
		for pc, w := range hcl.reg.rc.Code {
			if w.Op() == bytecode.RGetMethodW {
				read = pc
			}
		}
		if read < 0 {
			t.Fatalf("hash has no get_method_w")
		}
		x := jitRegInlineLevel{cl: hcl, pc: read, kind: abi.ExitHost}
		if !r.jitRegShapeMiss(&x) {
			t.Errorf("a host exit at hash's read, its site not megamorphic, is no shape miss")
		}
		if x.kind = abi.ExitPoll; r.jitRegShapeMiss(&x) {
			t.Errorf("a poll at hash's read is a shape miss")
		}
		x.kind = abi.ExitHost
		site := &hcl.ic[int(hcl.reg.rc.Code[read].C())]
		fills := site.fills
		site.fills = maxCacheFills
		if r.jitRegShapeMiss(&x) {
			t.Errorf("a host exit at hash's read, its site megamorphic, is a shape miss")
		}
		site.fills = fills
	})
}

// TestJITRegGenericCalleeByCodeLinked passes five functions to a loop's
// call, the fifth an arrow made anew at each call of its maker, and checks
// that the call, past the four it calls natively, calls the arrow through
// its link (ssa.CallSite's Generic), which a function known by its code is
// given as one known by its object is (jitRegCallSeenOf) -- not by Go at
// every call: AWFY's Havlak's Vector.forEach of stepD's callback, 86,000
// calls a run.
func TestJITRegGenericCalleeByCodeLinked(t *testing.T) {
	t.Parallel()
	const src = `
var acc = 0;
function each(a, f) { for (var i = 0; i < a.length; i++) f(a[i]) }
function m1(k) { each([1, 2], (v) => { acc += v + k }) }
function m2(k) { each([1, 2], (v) => { acc -= v + k }) }
function m3(k) { each([1, 2], (v) => { acc ^= v + k }) }
function f5(v) { acc |= v }
function mk(k) { var s = 0; each([1, 2, 3], (v) => { s += v * k }); return s }
function warm(n) { for (var i = 0; i < n; i++) { m1(i); m2(i); m3(i); mk(i); each([1], f5) } return acc }
function run(n) { var t = 0; for (var i = 0; i < n; i++) t += mk(i & 7); return t }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		r.SetJITReport(true)
		for _, rt := range rts {
			fv, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("warm"))
			if _, err := rt.Call(fv, Undefined, []Value{Float(2000)}); err != nil {
				t.Fatal(err)
			}
		}
		regProfitRun(t, rts, "run", 4, "42000", Float(2000))
		before := r.RegNativeStats().GoCallOps["call_w->function with code"]
		regProfitRun(t, rts, "run", 4, "42000", Float(2000))
		n := r.RegNativeStats().GoCallOps["call_w->function with code"] - before
		t.Logf("calls of Go of functions with code: %d of %d calls", n, 4*2000*3)
		if n > 1000 {
			t.Errorf("Go made %d of %d calls of the arrow", n, 4*2000*3)
		}
	})
}

// TestJITRegPolymorphicCallSeeded runs a method call over four classes'
// objects in the interpreter, then has a hot caller compile the function
// making it, and checks that its first code calls each class's method
// natively, the four seeded from what the method read's cache saw
// (jitRegSeenCallees): not one, the site's call cache's, the others learned
// an exit at a time, a budget waited for each compile -- V8's Richards'
// TaskControlBlock.run left native code at 344 of its first run's calls.
func TestJITRegPolymorphicCallSeeded(t *testing.T) {
	t.Parallel()
	const src = `
class T1 { run(p) { return p + 1 } }
class T2 { run(p) { return p + 2 } }
class T3 { run(p) { return p + 3 } }
class T4 { run(p) { return p + 4 } }
var ts = [new T1(), new T2(), new T3(), new T4()];
function step(t, p) { return t.run(p) }
function warm() { var s = 0; for (var i = 0; i < 8; i++) s += step(ts[i & 3], i); return s }
function run(n) { var s = 0; for (var i = 0; i < n; i++) s = (s + step(ts[i & 3], i)) & 65535; return s }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITReport(true)
		// The interpreter's runs first: the method read meets the four
		// classes before anything is compiled.
		r.SetJITWarmUp(1<<15, 1<<15)
		regProfitRun(t, rts, "warm", 1, "48")
		r.SetJITWarmUp(1, 1)
		want := ""
		{
			fv, _ := rts[1].GetProp(Obj(rts[1].Global()), rts[1].Intern("run"))
			v, err := rts[1].Call(fv, Undefined, []Value{Float(4000)})
			want = regNativeShow(rts[1], v, err)
		}
		before := r.RegNativeStats().HostOps["call_method_w"]
		regProfitRun(t, rts, "run", 1, want, Float(4000))
		_, se := regProfitEntry(t, r, "step")
		exits := r.RegNativeStats().HostOps["call_method_w"] - before
		t.Logf("step compiled %d times, %d exits at its call, learned %d targets", se.life.ssaSerial, exits, len(se.life.nativeCalls))
		if exits > 8 {
			t.Errorf("step's call of run left native code %d times of 4000", exits)
		}
		if se.life.ssaSerial > 1 {
			t.Errorf("step compiled %d times for its call's targets, not once", se.life.ssaSerial)
		}
	})
}
