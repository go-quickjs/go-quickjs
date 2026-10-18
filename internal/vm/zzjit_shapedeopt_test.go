//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
		e := r.jit.hint(cl)
		if e == nil || e.ssa == nil {
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
	e := r.jit.hint(cl)
	for pc, in := range cl.fn.Code {
		if (in.Op == bytecode.OpGetProp || in.Op == bytecode.OpGetPropThis) && int(in.A) < len(cl.names) && cl.names[in.A] == r.atoms.intern(key) {
			fb := &jitFeedback{r: r, fn: cl.fn, cl: cl, e: e}
			site, ok := fb.Property(pc)
			if !ok {
				t.Fatalf("%s's read of %s has no site", name, key)
			}
			return site.Megamorphic, &cl.ic[in.B]
		}
	}
	t.Fatalf("%s reads no %s", name, key)
	return false, nil
}

// A property read meeting a receiver of a shape its code was not compiled
// for deoptimizes, as V8's map checks do: Go writes the frame, and the
// interpreter makes the read and goes on to the loop's next header. The
// site's cache learns the shape there -- the interpreter reads an object's
// small table without it, so the deoptimization fills it -- and the code is
// compiled again with it, reading it natively from then on. So code
// compiled at its first call, before the read ran, deoptimizes once; met
// with a second shape, once more; and never leaves after, each answer the
// interpreter's.
func TestJITShapeMissesDeoptimize(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function A(){this.x=1} function B(){this.y=0;this.x=2}
		var as=[new A(),new A()], bs=[new A(),new B()];
		function sum(os,n){let s=0;for(let i=0;i<n;i++)s=(s+os[i&1].x)|0;return s}`
	r, rounds := jitShapeRunForTest(t, setup, "sum", `sum(as,100)`, `sum(as,100)`, `sum(bs,100)`, `sum(bs,100)`)
	for i, want := range []uint64{1, 0, 1, 0} {
		if got := rounds[i]; got.deopts != want || got.hosts != 0 {
			t.Errorf("round %d: %d map checks deoptimized and %d exits left to Go, want %d and none", i, got.deopts, got.hosts, want)
		}
		if got := rounds[i]; want == 0 && got.compiles != 0 {
			t.Errorf("round %d: compiled again %d times", i, got.compiles)
		}
	}
	mega, c := jitReadSite(t, r, "sum", "x")
	if mega || c.shape == nil || c.shape == noShape || c.seen == nil || c.seen.n == 0 {
		t.Fatalf("the read's cache did not learn its two shapes: megamorphic %v, %+v", mega, c)
	}
}

// What a site's cache cannot hold makes it megamorphic, once, rather than
// deoptimize at every receiver: a primitive receiver; an object's own
// getter, which the cache holds but the code does not call -- deoptimized
// for a second time, a shape the code was compiled again for since; and
// receivers of more shapes than a site's cases hold. A megamorphic read
// looks in the receiver's own table and the megamorphic cache, and leaves
// for Go what neither has (a host exit), never deoptimizing: so each
// function is compiled again a bounded number of times, then never.
func TestJITShapeMissesSettleMegamorphic(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function A(){this.x=1}
		var prims=[new A(),7], owns=[new A(),{get x(){return 5}}];
		var many=[];for(let k=0;k<8;k++){const o={};o['f'+k]=k;o.x=k;many.push(o)}
		function sum(os,n){let s=0;for(let i=0;i<n;i++)s=(s+(os[i%os.length].x|0))|0;return s}`
	for _, c := range []struct {
		arg     string
		compile uint64
	}{{"prims", 2}, {"owns", 3}, {"many", 6}} {
		t.Run(c.arg, func(t *testing.T) {
			run := `sum(` + c.arg + `,64)`
			r, rounds := jitShapeRunForTest(t, setup, "sum", run, run, run, run)
			var compiles uint64
			for _, rd := range rounds {
				compiles += rd.compiles
			}
			last := rounds[len(rounds)-1]
			if last.deopts != 0 || last.compiles != 0 || compiles > c.compile {
				t.Fatalf("%v: compiled again %d times, want at most %d, and settled", rounds, compiles, c.compile)
			}
			if mega, _ := jitReadSite(t, r, "sum", "x"); !mega {
				t.Fatalf("%v: the read is not megamorphic", rounds)
			}
		})
	}
}
