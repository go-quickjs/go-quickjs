//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

// A call that left native code for a target its code was not compiled
// for teaches the code that target at its first exit; at the next, the
// exit teaches nothing, and the code, which would leave at every such call
// until its budget for what it learned was spent -- the interpreter
// finishing each iteration -- is invalidated (jitCallRepeats), as V8's code
// deoptimizes at a call's wrong target: the existing tiers run the
// function until it is compiled again with the target. PDF.js's
// Lexer.getNumber, whose stream.lookChar() met each stream's class in turn,
// had left at 951 calls so. Each answer is the interpreter's.
func TestJITCallRepeatsInvalidate(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function A(k) { this.k = k }
		A.prototype.step = function (i) { let s = 0; for (let j = 0; j < 3; j++) s = (s + i * this.k + j) | 0; return s }
		function B(k) { this.k = k; this.z = 1 }
		B.prototype.step = function (i) { let s = 1; for (let j = 0; j < 3; j++) s = (s ^ (i + this.k + j)) | 0; return s }
		var a = new A(3), b = new B(5)
		function run(o, n) { let t = 0; for (let i = 0; i < n; i++) t = (t + o.step(i)) | 0; return t }
		function each(o, n) { let t = 0; for (let i = 0; i < n; i++) t = (t + run(o, 4)) | 0; return t }`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	runCl := r.global.getOwn(r.atoms.intern("run")).value().Object().fn().closure
	var hosts []uint64
	for i, call := range []string{`each(a, 2000)`, `each(a, 2000)`, `each(b, 2000)`, `each(b, 2000)`, `each(a, 2000)`} {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		var h uint64
		if r.jit != nil {
			h = r.jit.hosts
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d, %s: got %v, interpreter %v", i, call, got, want)
		}
		if r.jit != nil {
			h = r.jit.hosts - h
		}
		hosts = append(hosts, h)
	}
	e := r.jit.cache[weak.Make(runCl.fn)]
	if e == nil || e.ssa == nil || e.invalid {
		t.Fatalf("run has no code: %+v", e)
	}
	// B's first round: the call meets B, learned at its first exits, and
	// the code is invalidated at the next -- three in all, where it had left
	// at 487 calls.
	if hosts[2] > 3 || hosts[3] != 0 || hosts[4] != 0 {
		t.Fatalf("exits to Go by round: %v", hosts)
	}
}
