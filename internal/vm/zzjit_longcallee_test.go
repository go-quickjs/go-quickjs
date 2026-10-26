//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A call of a function too long to inline, warming up when its caller is
// compiled, is made through the callee's link: by Go from native code while
// the link has no code, then natively once the callee's own budget has it
// compiled -- the caller is not compiled again for it, as V8's code calls a
// function's current code, its interpreter's until it is optimized. Left to
// Go at the compile, the call had been decided at its exits once the callee
// had code, and the caller compiled again: Typescript's compiler, whose
// methods' callers are compiled before the methods are. Each answer is the
// interpreter's.
func TestJITLongCalleeCalledThroughLink(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function long(x) {
			let s = x
			s = (s * 3 + 1) | 0; s = (s ^ (s >>> 3)) | 0; s = (s * 5 + 7) | 0; s = (s ^ (s >>> 5)) | 0
			s = (s * 3 + 1) | 0; s = (s ^ (s >>> 3)) | 0; s = (s * 5 + 7) | 0; s = (s ^ (s >>> 5)) | 0
			s = (s * 3 + 1) | 0; s = (s ^ (s >>> 3)) | 0; s = (s * 5 + 7) | 0; s = (s ^ (s >>> 5)) | 0
			s = (s * 3 + 1) | 0; s = (s ^ (s >>> 3)) | 0; s = (s * 5 + 7) | 0; s = (s ^ (s >>> 5)) | 0
			return s
		}
		function caller(i) { return i % 10 === 0 ? long(i) : (i * 7) | 0 }
		function run(n) { let t = 0; for (let i = 0; i < n; i++) t = (t + caller(i)) | 0; return t }`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	fnOf := func(name string) *closure { return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure }
	caller, long := fnOf("caller"), fnOf("long")
	if len(long.fn.Code) <= ssa.MaxInline {
		t.Fatalf("long is %d instructions, short enough to inline", len(long.fn.Code))
	}
	for i := range 6 {
		call := `run(20000)`
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
	ce, le := r.jit.cache[weak.Make(caller.fn)], r.jit.cache[weak.Make(long.fn)]
	if ce == nil || ce.ssa == nil || le == nil || le.ssa == nil || le.link == nil {
		t.Fatalf("caller or long has no code: %v, %v", ce, le)
	}
	if ce.reopts+ce.inlineReopts+ce.upgradeReopts+ce.polyReopts != 0 {
		t.Fatalf("caller compiled again: %d for what failed, %d for its calls, %d for its callees, %d for shapes", ce.reopts, ce.inlineReopts, ce.upgradeReopts, ce.polyReopts)
	}
	if le.link.in == 0 {
		t.Fatal("long's calls were never made natively")
	}
	// While long warmed up, Go made its calls from native code: none left
	// native code (abi.ExitEnter), which would have been a host exit or a
	// finish in the interpreter at each of some 2,000 calls a round.
	if st := r.JITStats(); st.Hosts > 4 || st.Interpreted > 4 || ce.ssaStats.goCalls == 0 {
		t.Fatalf("calls of long while it warmed: %d exits to Go, %d finished in the interpreter, %d calls of Go", st.Hosts, st.Interpreted, ce.ssaStats.goCalls)
	}
}
