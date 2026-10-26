//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

// A call of a closure read from a captured binding -- a module's
// function calling another of its scope -- is decided at the caller's
// compile from the function the binding holds then (jitCalleeObjectAt),
// as a global's call is: a function compiled while its first loop runs
// inlines the calls of its later loops, which have not run yet, and is
// not compiled again for them. Decided one call at a time at their exits,
// RegExp's runBlock0, whose two dozen loops each call Exec, was compiled
// three times a run. Each answer is the interpreter's.
func TestJITCapturedCalleeSeeded(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `var run = (function () {
			function leaf(x) { return x * 2 + 1 }
			function twice(x) { return leaf(x) + leaf(x + 1) }
			return function run(n) {
				let s = 0
				for (let i = 0; i < n; i++) s = (s + leaf(i)) | 0
				for (let i = 0; i < n; i++) s = (s + twice(i)) | 0
				for (let i = 0; i < n; i++) s = (s - leaf(i + 2)) | 0
				return s
			}
		})()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	for i := range 3 {
		call := `run(50000)`
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d: got %v, interpreter %v", i, got, want)
		}
	}
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.ssa == nil || e.entrySlow || e.invalid {
		t.Fatalf("run did not run natively: %+v", e)
	}
	if r.jit.reoptimized != 0 || e.ssaStats.goCalls != 0 || len(e.inlines) != 3 {
		t.Fatalf("run compiled again %d times, %d calls of Go, %d calls inlined (want 0, 0, 3)", r.jit.reoptimized, e.ssaStats.goCalls, len(e.inlines))
	}
}

// A captured binding that the call was compiled for holds another
// function later: the call checks the function it calls, and calls the
// one the binding holds. Each answer is the interpreter's.
func TestJITCapturedCalleeReassigned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := `var m = (function () {
			let f = function (x) { return x + 1 }
			return {
				run(n) { let s = 0; for (let i = 0; i < n; i++) s = (s * 3 + f(i)) | 0; return s },
				set(k) { f = k === 0 ? function (x) { return x * 5 } : function (x) { return x - k } },
			}
		})()`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for i, call := range []string{`m.run(30000)`, `m.run(30000)`, `m.set(0), m.run(30000)`, `m.run(30000)`, `m.set(7), m.run(30000)`, `m.run(300)`} {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d, %s: got %v, interpreter %v", i, call, got, want)
		}
	}
	if r.jit == nil || r.jit.ssaEntries == 0 {
		t.Fatal("run never ran natively")
	}
}
