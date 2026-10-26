//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
	"weak"
)

// Objects of a shape, and objects of one made from it by adding a
// property, both live: a function's reads that met both are compiled for
// both -- the descendant, which their caches hold, and the ancestor among
// the shapes they met before -- as V8's inline caches keep a map whose
// objects gained a property elsewhere. Dropped from the cases as stale
// while there was room, the ancestor's objects deoptimized each of the
// function's reads of them in turn, a compile each: Typescript's
// Binder.bindSymbol, of symbols some of which are never given a type,
// nine times (jitAddReadCase). Each answer is the interpreter's.
func TestJITSSAReadsKeepLiveAncestorShapes(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// While the collector marks, native calls leave for Go: held off, so
	// that the rounds' counts are their reads'.
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	src := `function S(){this.a=1;this.b=2;this.c=3}
		var old=[new S,new S,new S,new S], neu=[new S,new S,new S,new S];
		for (const o of neu) o.d=4;
		var mixed=[old[0],neu[0],old[1],neu[1]];
		function f(xs,n){let t=0;for(let i=0;i<n;i++){const o=xs[i&3];t=(t+o.a*3+o.b*5+o.c*7+i)|0}return t}`
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	// The existing tiers meet both shapes at each read, the descendant
	// last; the code is compiled meeting only the descendant; then the
	// ancestor's objects come.
	rounds := []string{`f(mixed,40)`, `f(neu,20000)`, `f(old,20000)`, `f(old,20000)`, `f(mixed,20000)`}
	var deopts, compiles []uint64
	for i, call := range rounds {
		wv, err := want.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		var d, c uint64
		if r.jit != nil {
			d, c = r.jit.shapeDeopts, r.jit.reoptimized
		}
		gv, err := r.Run(compileForTest(t, call))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.Number(), wv.Number(); got != want {
			t.Fatalf("round %d, %s: got %v, interpreter %v", i, call, got, want)
		}
		if r.jit == nil {
			deopts, compiles = append(deopts, 0), append(compiles, 0)
			continue
		}
		deopts, compiles = append(deopts, r.jit.shapeDeopts-d), append(compiles, r.jit.reoptimized-c)
	}
	e := r.jit.cache[weak.Make(cl.fn)]
	if e == nil || e.ssa == nil || e.entrySlow || e.invalid {
		t.Fatalf("f did not run natively: %+v", e)
	}
	if e.ssaStats.entries == 0 {
		t.Fatal("f's code was never entered")
	}
	for i := 2; i < len(rounds); i++ {
		if deopts[i] != 0 || compiles[i] != 0 {
			t.Fatalf("reads of both shapes: map checks deoptimized %v times and f was compiled again %v times, by round", deopts, compiles)
		}
	}
}
