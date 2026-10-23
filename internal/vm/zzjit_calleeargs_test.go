//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// calleeArgsSetup's callees read their call's arguments past their
// parameters: rest parameters, after parameters and after a default;
// strict functions' arguments objects, read by index and counted, and
// made and let out; a getter's and a setter's; a class with fields and no
// constructor of its own, whose super() hands its call's arguments on;
// sum called through Function.prototype.call; a call of more arguments
// than a context holds; and a callee whose arguments object is made after
// a collection, its extra arguments, objects made for the call, kept alive
// by its caller alone.
const calleeArgsSetup = `
	'use strict'
	function sum(...xs) { let s = 0; for (let i = 0; i < xs.length; i++) s += xs[i]; return s * 10 + xs.length }
	function tail(k, ...xs) { return k * 100 + xs.length * 10 + (xs.length ? xs[xs.length - 1] : 0) }
	function dflt(a = 7, ...r) { return a + r.length * 1000 }
	function count(a) { return arguments.length * 100 + (arguments.length > 1 ? arguments[1] : -1) + a }
	function made() { return arguments }
	class P { constructor(a, b) { this.a = a; this.b = b } }
	class D extends P { f = 3 }
	const acc = { get g() { return arguments.length }, set s(v) { this.v = v + arguments.length * 10 } }
	function late(a) { gc(); const o = arguments; return a + o[1].v + o[2].v + o.length }
	function many(...xs) { return xs.length }
	function runRest(n) { let s = 0; for (let i = 0; i < n; i++) s += sum() + sum(i) + sum(i, 1, 2) + tail(1) + tail(2, i, 3) + dflt() + dflt(i, 1, 2); return s }
	function runArgs(n) { let s = 0; for (let i = 0; i < n; i++) s += count(i) + count(i, 2, 3) + made(i, 1)[1] + (made()[0] === undefined ? 1 : 0); return s }
	function runSuper(n) { let s = 0; for (let i = 0; i < n; i++) { const d = new D(i, 2); s += d.a + d.b + d.f } return s }
	function runVia(n) { let s = 0; for (let i = 0; i < n; i++) s += sum.call(null, i, 1) + sum.call(null); return s }
	function runAcc(n) { let s = 0; for (let i = 0; i < n; i++) { acc.s = i; s += acc.v + acc.g } return s }
	function runLate(n) { let s = 0; for (let i = 0; i < n; i++) s += late(i, { v: i }, { v: 1 }); return s }
	function runMany(n) { let s = 0; for (let i = 0; i < n; i++) s += many(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, i); return s }`

// A native call of a function whose code reads its call's arguments past
// its parameters writes them all, and their count, to its callee's context
// (ssa.CallSite's FrameArgs, abi.Context's Args), as V8's call pushes them
// all: Go makes the rest parameter's array and the unmapped arguments
// object of them without a frame (jitGoHostPlain), reads arguments[k] and
// arguments.length there (jitArgument), and gives them to every frame it
// makes of the callee's level -- a super() of them, a frame the interpreter
// finishes (jitCallArgs). Such callees had been refused for native
// callers, each call a call of Go: npm's 144 implicit derived
// constructors, 77 functions of rest parameters, 17 of arguments objects.
// Every answer is the interpreter's, at every stress setting, and the
// callees are called natively, the loop making no call of Go for them.
func TestJITCalleeArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	gcs := 0
	gc := func(r *Runtime) {
		r.global.setOwnRaw(r.atoms.intern("gc"), r.NewFunction("gc", 0, func(r *Runtime, _ Value, _ []Value) (Value, error) {
			if gcs++; gcs%8 == 0 {
				runtime.GC()
			}
			return Undefined, nil
		}), propDefault)
	}
	loops := []string{"runRest", "runArgs", "runSuper", "runVia", "runAcc"}
	rounds := []string{}
	for range 6 {
		for _, l := range loops {
			rounds = append(rounds, l+`(200)`)
		}
		rounds = append(rounds, `runLate(100)`, `runMany(50)`)
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		gc(rt)
		if _, err := rt.Run(compileForTest(t, calleeArgsSetup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		wv, werr := want.Run(compileForTest(t, `''+`+src))
		gv, gerr := r.Run(compileForTest(t, `''+`+src))
		if werr != nil || gerr != nil {
			t.Fatalf("round %d (%s): %v, interpreter's %v", i, src, gerr, werr)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
		}
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	// Each loop's callee called natively at each call, the loop making no
	// call of Go: D's code too, its super() of the call's arguments Go's.
	for _, c := range []struct {
		loop, callee string
		calls        uint64
	}{
		{"runRest", "sum", 300}, {"runRest", "tail", 200}, {"runRest", "dflt", 200},
		{"runArgs", "count", 200}, {"runArgs", "made", 200},
		{"runVia", "sum", 200},
	} {
		m, e := jitEntryForTest(t, r, c.loop), jitEntryForTest(t, r, c.callee)
		in, goCalls := e.link.in, m.ssaStats.goCalls
		if _, err := r.Run(compileForTest(t, c.loop+`(100)`)); err != nil {
			t.Fatal(err)
		}
		if d := e.link.in - in; d != c.calls {
			t.Errorf("%s: %s was called natively %d times, want %d", c.loop, c.callee, d, c.calls)
		}
		if d := m.ssaStats.goCalls - goCalls; d != 0 {
			t.Errorf("%s made %d calls of Go, want none", c.loop, d)
		}
	}
	for _, loop := range []string{"runSuper"} {
		m := jitEntryForTest(t, r, loop)
		goCalls, hosts := m.ssaStats.goCalls, m.ssaStats.hosts
		if _, err := r.Run(compileForTest(t, loop+`(100)`)); err != nil {
			t.Fatal(err)
		}
		if d, h := m.ssaStats.goCalls-goCalls, m.ssaStats.hosts-hosts; d != 0 || h != 0 {
			t.Errorf("%s made %d calls of Go and left native code %d times, want none", loop, d, h)
		}
	}
	d := jitDerivedForTest(t, r, "D")
	if in := jitDerivedCalls(d); in == 0 {
		t.Errorf("D's code was never called natively")
	}
}

// The same, under every stress setting the differential tests use, gc a
// function of the script's own.
func TestJITCalleeArgumentsStress(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitDifferential(t, `'use strict'; function gc() {}`+calleeArgsSetup[len("\n\t'use strict'"):]+`
		;[runRest(100), runArgs(100), runSuper(100), runVia(100), runAcc(100), runLate(50), runMany(20), runRest(50)].join()`)
}

// A native callee that reads its arguments past its parameters is called
// natively, or by Go, from a call in tail position deep in the stack, its
// frame given up for the callee's only where its arguments are its
// parameters: Go makes such a call, whose callee's frame its context's
// arguments would not outlive. Each answer is the interpreter's.
func TestJITCalleeArgumentsTailCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitAgreesForTest(t, `'use strict'
		function last(...xs) { return xs.length * 1000 + xs[xs.length - 1] }
		function hop(n, a, b) { return last(n, a, b, n + 1) }
		function deep(d, n) { return d === 0 ? hop(n, 1, 2) : deep(d - 1, n) + 0 }`,
		`deep(40, 8)`, `deep(40, 20)`, `deep(4, 4)`, `deep(40, 20)`, `deep(5, 20)`, `deep(40, 30)`, `deep(40, 30)`)
}
