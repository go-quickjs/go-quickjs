//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// jitShapeRunForTest runs setup in an interpreter and in a runtime that
// compiles at the first call, then each round in both, failing at the
// first answer that differs; for each round it reports how many map
// checks deoptimized (jitState's shapeDeopts), how many exits left an
// operation to Go, and how many times the function name was compiled
// again.
func jitShapeRunForTest(t *testing.T, setup, name string, rounds ...string) (*Runtime, []jitShapeRound) {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	var out []jitShapeRound
	for i, src := range rounds {
		wv, err := want.Run(compileForTest(t, "String("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		var deopts, hosts, reopts uint64
		if r.jit != nil {
			deopts, hosts, reopts = r.jit.shapeDeopts, r.jit.hosts, r.jit.reoptimized
		}
		gv, err := r.Run(compileForTest(t, "String("+src+")"))
		if err != nil {
			t.Fatal(err)
		}
		if got, w := gv.String().Go(), wv.String().Go(); got != w {
			t.Fatalf("round %d, %s: got %s, interpreter %s", i, src, got, w)
		}
		if _, ok := jitViewForTest(r, cl); !ok {
			t.Fatalf("round %d, %s: %s has no code", i, src, name)
		}
		out = append(out, jitShapeRound{r.jit.shapeDeopts - deopts, r.jit.hosts - hosts, r.jit.reoptimized - reopts})
	}
	return r, out
}

type jitShapeRound struct{ deopts, hosts, compiles uint64 }

// jitReadSite is the site of the property read of key in cl's function
// as its code is compiled now (jitFeedback.Property), and its cache.
func jitReadSite(t *testing.T, r *Runtime, name, key string) (siteMega bool, c *propCache) {
	t.Helper()
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	cv, ok := jitViewForTest(r, cl)
	if !ok || cv.reg == nil {
		t.Fatalf("%s has no code", name)
	}
	// The read, by its program's pc, its cache the one its register
	// instruction reads (jitRegFeedback's propertyAt).
	fb := r.jitRegFeedbackFor(cl, cv.reg.p)
	for pc, in := range cv.reg.p.Code {
		if in.Op != ir.PropertyRead {
			continue
		}
		if _, k, c, write, _, _, ok := fb.propertyAt(pc); ok && !write && k == r.atoms.intern(key) && c != nil {
			site, ok := fb.Property(pc)
			if !ok {
				t.Fatalf("%s's read of %s has no site", name, key)
			}
			return site.Megamorphic, c
		}
	}
	t.Fatalf("%s reads no %s", name, key)
	return false, nil
}

// A property read meeting a receiver of a shape its code was not compiled
// for deoptimizes, as V8's map checks do: Go writes the frame, and the
// interpreter makes the read and the rest of the invocation. The site's
// cache learns the shape there, and the code, invalidated, is compiled
// again with it once the existing tiers have spent their budget -- at the
// next call, a runtime that compiles at the first -- reading it natively
// from then on. So code compiled at its first call, before the read ran,
// deoptimizes once at a prototype's property, which a site that knew
// nothing searches no receiver's own table for; met with a second shape,
// once more; and never leaves after, its receivers of the two shapes mixed
// too, each answer the interpreter's. (TestJITShapesMixedDeoptimizeOnce
// mixes them from the first call.)
func TestJITShapeMissesDeoptimize(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function A(){} A.prototype.x=1; function B(){this.y=0} B.prototype.x=2;
		var as=[new A(),new A()], bs=[new B(),new B()], mixed=[new A(),new B()];
		function sum(os,n){let s=0;for(let i=0;i<n;i++)s=(s+os[i&1].x)|0;return s}`
	r, rounds := jitShapeRunForTest(t, setup, "sum", `sum(as,100)`, `sum(as,100)`, `sum(bs,100)`, `sum(bs,100)`, `sum(mixed,100)`, `sum(mixed,100)`)
	// Register code is compiled once its first run has returned, a budget
	// of one (regTickRet, jitRegCallTier), its read's cache knowing A's
	// shape by then: no deoptimization at the first runs. B's deoptimizes
	// once, and the interpreter's finishing that run spends the fresh
	// budget, compiling the code again in the same round (jitRegTierUp).
	wants := []jitShapeRound{{0, 0, 0}, {0, 0, 0}, {1, 0, 1}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}}
	for i, want := range wants {
		if got := rounds[i]; got != want {
			t.Errorf("round %d: %d map checks deoptimized, %d exits left to Go and %d compiles, want %+v", i, got.deopts, got.hosts, got.compiles, want)
		}
	}
	mega, c := jitReadSite(t, r, "sum", "x")
	if mega || c.shape == nil || c.shape == noShape || c.seenOf() == nil || c.seenOf().n == 0 {
		t.Fatalf("the read's cache did not learn its two shapes: megamorphic %v, %+v", mega, c)
	}
}

// Receivers of two shapes mixed from the first call: the code, compiled
// before the read ran, deoptimizes once, and the interpreter, running the
// rest of that invocation, keeps an entry for each shape in the read's
// cache, as V8's polymorphic inline caches hold four maps, rather than
// trading one for the other at each receiver until the cache gave up. The
// second entry, made after the deoptimization, has the code wait a budget
// more (jitTierUp), as V8's NotifyICChanged does; compiled again, it reads
// both natively and never leaves.
func TestJITShapesMixedDeoptimizeOnce(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function A(){} A.prototype.x=1; function B(){this.y=0} B.prototype.x=2;
		var mixed=[new A(),new B()];
		function sum(os,n){let s=0;for(let i=0;i<n;i++)s=(s+os[i&1].x)|0;return s}`
	r, rounds := jitShapeRunForTest(t, setup, "sum", `sum(mixed,100)`, `sum(mixed,100)`, `sum(mixed,100)`, `sum(mixed,100)`)
	// Register code is compiled once its first run has returned
	// (regTickRet, jitRegCallTier), its read's cache holding both shapes
	// by then: it reads both natively from its first compile, and never
	// leaves nor is compiled again.
	wants := []jitShapeRound{{0, 0, 0}, {0, 0, 0}, {0, 0, 0}, {0, 0, 0}}
	for i, want := range wants {
		if got := rounds[i]; got != want {
			t.Errorf("round %d: %d map checks deoptimized, %d exits left to Go and %d compiles, want %+v", i, got.deopts, got.hosts, got.compiles, want)
		}
	}
	if mega, c := jitReadSite(t, r, "sum", "x"); mega || c.fills > 2 {
		t.Fatalf("the read's cache did not keep its two shapes: megamorphic %v, %d fills", mega, c.fills)
	}
}
