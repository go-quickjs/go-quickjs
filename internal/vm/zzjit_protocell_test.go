//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A read whose cache remembers it by a chain's validity cell -- a property
// nothing on the chain has, an option left out, or a method four
// prototypes up -- is read natively at a megamorphic site, from the
// megamorphic cache, which the interpreter's cache filled for it as it
// remembered the read (jitNoteChainRead): the loop's code neither leaves
// nor deoptimizes there, from its first compile on. Each answer is the
// interpreter's.
func TestJITChainCellReadsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `class A { m() { return 1 } } class B extends A {} class C extends B {} class D extends C {}
		var opts = {verbose: false}, d = new D()
		function absent(n) { let c = 0; for (let i = 0; i < n; i++) if (opts.depth === undefined) c++; return c }
		function deep(n) { let c = 0; for (let i = 0; i < n; i++) c += d.m(); return c }`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"absent", "deep"} {
		src := name + `(2000)`
		reoptimized := r.jit.reoptimized
		var stats jitSSAStats
		for round := 0; round < 6; round++ {
			wv, err := want.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			e := r.jit.hint(globalObject(r, name).fn().closure)
			if e != nil {
				stats = e.ssaStats
			}
			gv, err := r.Run(compileForTest(t, src))
			if err != nil || gv.Number() != wv.Number() {
				t.Fatalf("%s round %d: got %v, %v; interpreter %v", name, round, gv, err, wv)
			}
			if e == nil || e.ssa == nil || round < 3 {
				continue
			}
			if e != r.jit.hint(globalObject(r, name).fn().closure) || e.ssaStats.hosts != stats.hosts || e.ssaStats.guards != stats.guards {
				t.Fatalf("%s round %d: left native code: %d exits, %d guards failed", name, round, e.ssaStats.hosts-stats.hosts, e.ssaStats.guards-stats.guards)
			}
		}
		// Once compiled, compiled again at most once, for the loop's own
		// speculations: not for what the read meets.
		e := r.jit.hint(globalObject(r, name).fn().closure)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s was not compiled", name)
		}
		if n := r.jit.reoptimized - reoptimized; e.deopts > 1 || n > 1 {
			t.Fatalf("%s: %d deoptimizations, compiled again %d times", name, e.deopts, n)
		}
	}
}
