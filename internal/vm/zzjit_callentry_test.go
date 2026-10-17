//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// A callee's locals past its parameters are undefined when a native call
// enters it, though its code's start, a loop's target, is no place to make
// them so (compile.StartOnly): its call entry makes them undefined (mir's
// callEntry), and Go, which runs a callee with no call entry from its
// start (abi.ExitEnter), does (jitStartLocals) -- no call writes them
// now. Each callee here reads such a local before it assigns it, in a
// frame where the call before wrote its third argument, the same slot,
// past the VM's stack's top: left as it was, it reads 777.
func TestJITCallEntryClearsLocals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Native calls are Go's while the collector marks.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function g(a, b, c) { return a + b + c }
		function f(a, b) { for (;;) { if (b) return y; var y = a; b = 1 } }
		function run(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + g(i, 0, 777)) | 0
				const v = f(i, 1)
				if (v !== undefined) return "stale " + v
			}
			return s
		}
		function g16(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q) { return a + q }
		function f16(a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p) { for (;;) { if (b) return y; var y = a; b = 1 } }
		function run16(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				s = (s + g16(i,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,777)) | 0
				const v = f16(i,1,0,0,0,0,0,0,0,0,0,0,0,0,0,0)
				if (v !== undefined) return "stale " + v
			}
			return s
		}`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup+`; run(300); run(300); run16(300); run16(300)`)); err != nil {
		t.Fatal(err)
	}
	fnOf := func(name string) *bytecode.Function {
		return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
	}
	for _, tc := range []struct {
		run, callee string
		// enter has the callee have no code native callers may call, which
		// Go runs from its start (abi.ExitEnter): its operands are more than
		// a call of Go takes (abi.MaxGoArgs).
		enter bool
	}{
		{"String(run(300))", "f", false},
		{"String(run16(300))", "f16", false},
		{"String(run16(300))", "f16", true},
	} {
		fn := fnOf(tc.callee)
		e := r.jit.cache[weak.Make(fn)]
		if e == nil || e.ssa == nil || e.link.in == 0 {
			t.Fatalf("%s is not called natively: %+v", tc.callee, e)
		}
		if e.ssa.CallEntry() == 0 || jitcompile.StartOnly(fn) {
			t.Fatalf("%s: call entry %#x, start only %v", tc.callee, e.ssa.CallEntry(), jitcompile.StartOnly(fn))
		}
		if tc.enter {
			e.notNative, e.link.entry, e.link.body, e.nativeRetry = true, 0, 0, 0
		}
		in, entered := e.link.in, r.jit.entered
		wv, err := want.Run(compileForTest(t, tc.run))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, tc.run))
		if err != nil {
			t.Fatal(err)
		}
		if gv.String().Go() != wv.String().Go() {
			t.Fatalf("%s = %s; interpreter %s", tc.run, gv.String().Go(), wv.String().Go())
		}
		switch {
		case tc.enter && r.jit.entered == entered:
			t.Fatalf("%s: Go never ran %s from its start", tc.run, tc.callee)
		case !tc.enter && e.link.in-in < 300:
			t.Fatalf("%s: %s was called natively %d times of 300", tc.run, tc.callee, e.link.in-in)
		}
	}
}
