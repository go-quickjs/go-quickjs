//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Runtimes on goroutines of their own compile at once, each in a workspace
// of the pool's no other compile uses meanwhile: their code computes what
// the interpreter does.
func TestJITWorkspacesConcurrentRuntimes(t *testing.T) {
	t.Parallel()
	const src = `
		function f(n, k) { let s = 0; for (let i = 0; i < n; i++) s = (s + (i * k) % 13) | 0; return s }
		function g(n, k) { const o = {a: k}; let s = 0; for (let i = 0; i < n; i++) s = (s + o.a * i) % 1000003; return s }
		function h(n, k) { let s = ""; for (let i = 0; i < n; i++) s = (s + (i % k)).slice(-8); return s }
		function all(k) { let r = 0; for (let j = 0; j < 30; j++) r = (r + f(2000, k) + g(2000, k) + h(500, k).length) | 0; return r }`
	want := make([]string, 8)
	for k := range want {
		r := newTestRuntime(t, Config{})
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, fmt.Sprintf("''+all(%d)", k+3)))
		if err != nil {
			t.Fatal(err)
		}
		want[k] = v.String().Go()
	}
	errs := make(chan error, len(want))
	for k := range want {
		go func() {
			r := newTestRuntime(t, Config{JIT: true})
			defer r.Close()
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				errs <- err
				return
			}
			for range 3 {
				v, err := r.Run(compileForTest(t, fmt.Sprintf("''+all(%d)", k+3)))
				if err != nil {
					errs <- err
					return
				}
				if got := v.String().Go(); got != want[k] {
					errs <- fmt.Errorf("all(%d) = %s, the interpreter's %s", k+3, got, want[k])
					return
				}
			}
			if r.JITStats().Compiled == 0 {
				errs <- fmt.Errorf("all(%d) compiled nothing", k+3)
				return
			}
			errs <- nil
		}()
	}
	for range want {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// Code the cache drops to make room is code nothing runs, calls or returns
// to: compiled in a budget far too small for a program's hot functions,
// each calling others natively and compiled again as it learns, a run
// gives what the interpreter gives, and every function's link calls the
// code of the entry the cache holds. Before, the cache dropped the entry a
// compile for native callers was being made for (jitRegNativeCallee made
// room before it was busy), whose code its link then called with nothing to
// keep it; Go ran code the cache had dropped since it was found; a
// function was compiled again while a native call of it had not returned,
// which then returned into the new code's place, or had its frame read by
// the new code's entry; and a native callee's caller went
// on at the place the old code had returned to in new code put there
// (jitRegUnwind): Octane's Typescript failed, a different way each run.
func TestJITEvictionKeepsRunningCode(t *testing.T) {
	// Leaves the JIT refuses on their own -- property reads, no loop --
	// which it compiles for the native callers it then meets: each
	// caller's loop calls several, and is compiled again as it learns.
	var b strings.Builder
	const leaves, callers = 120, 60
	for i := 0; i < leaves; i++ {
		fmt.Fprintf(&b, "function g%d(o) { return o.a * %d + o.b }\n", i, i%7+1)
	}
	for i := 0; i < callers; i++ {
		fmt.Fprintf(&b, "function c%d(os, d) { let s = 0; for (let k = 0; k < os.length; k++) { s = (s + g%d(os[k]) + g%d(os[k]) + g%d(os[k])) %% 1000003",
			i, i%leaves, (i*7+1)%leaves, (i*11+2)%leaves)
		fmt.Fprintf(&b, "; if (d > 0 && k === 0) s = (s + c%d(os, d - 1)) %% 1000003 } return s }\n", (i*5+3)%callers)
	}
	b.WriteString("const os = []; for (let i = 0; i < 20; i++) os.push({a: i, b: i * 3});\n")
	fmt.Fprintf(&b, "let r = 0; for (let k = 0; k < 30; k++) { for (let i = 0; i < %d; i++) r = (r + this[\"c\" + i](os, 2)) %% 1000003; check() } r", callers)
	src := b.String()
	plain := newTestRuntime(t, Config{})
	plain.global.setOwnRaw(plain.atoms.intern("check"), Obj(plain.newNativeFunc("check", 0, func(*Runtime, Value, []Value) (Value, error) {
		return Undefined, nil
	})), propDefault)
	want, err := plain.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	// The cache's clock a tick at every look: code is old at once, and the
	// cache drops it to make room for whatever is compiled next.
	var tick uint64
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.epochNow = func() uint64 { tick++; return tick } })
	jitNoFlushForTest(t)
	r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: 1 << 20})
	// Between rounds, every link holds, and the collector runs, releasing
	// the code nothing holds -- which native code then must not run.
	var broken error
	r.global.setOwnRaw(r.atoms.intern("check"), Obj(r.newNativeFunc("check", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
		if err := regLinksHeldForTest(rt.jit); err != nil && broken == nil {
			broken = err
		}
		if err := regEntriesFoundForTest(rt.jit); err != nil && broken == nil {
			broken = err
		}
		runtime.GC()
		return Undefined, nil
	})), propDefault)
	got, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if !got.sameBits(want) {
		t.Fatalf("jit: %v, interpreter: %v", got, want)
	}
	if r.jit == nil || r.jit.evicted == 0 || r.jit.compiled < 20 {
		t.Fatalf("the cache dropped no code: %+v", r.JITStats())
	}
	if broken != nil {
		t.Fatal(broken)
	}
}

// A program with more hot functions than the JIT's budget holds, run in
// turn: a function whose code was dropped to make room waits, before it
// is compiled again, twice as long each time (jitGhost), and the cache
// stops trading one function's code for another's. Each had been
// compiled again at its next call, dropping the next one's.
func TestJITEvictedWaitToCompileAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var b strings.Builder
	const funcs, rounds = 80, 40
	for i := 0; i < funcs; i++ {
		fmt.Fprintf(&b, "function f%d(a) { let s = 0; for (let k = 0; k < a.length; k++) s = (s + a[k] * %d + (a[k] >> 1)) | 0; return s }\n", i, i+1)
	}
	b.WriteString("const a = []; for (let i = 0; i < 50; i++) a.push(i);\n")
	fmt.Fprintf(&b, "let r = 0; for (let k = 0; k < %d; k++) for (let i = 0; i < %d; i++) r = (r + this[\"f\" + i](a)) | 0; r", rounds, funcs)
	var tick uint64
	jitTestSwitch(t, func(jc *jitTestConfig) { jc.epochNow = func() uint64 { tick++; return tick } })
	r := newTestRuntime(t, Config{JIT: true, JITMemoryLimit: 256 << 10})
	jitDefaultsForTest(r)
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if _, err := r.Run(compileForTest(t, b.String())); err != nil {
		t.Fatal(err)
	}
	st := r.JITStats()
	t.Logf("%d compiled, %d dropped", st.Compiled, st.Evicted)
	if st.Compiled > 4*funcs {
		t.Errorf("%d compiles of %d functions in %d rounds", st.Compiled, funcs, rounds)
	}
}
