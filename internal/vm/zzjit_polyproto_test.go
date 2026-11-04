//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Native code reads the prototype of an object that keeps it in its table
// (zzz_polyproto.go) there, not in its layout (mir's loadProto): instanceof,
// the in operator and reads up the chain of the first objects made on
// prototypes made and dropped, and of objects made on them, answer as the
// interpreter does, before and after their prototypes change; and a loop
// of instanceof over such objects, made before it, runs natively.
func TestJITPolyProto(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `function F() { this.f = 1 }
		F.prototype = {m: 2}
		function make(n) {
			var xs = []
			for (var i = 0; i < n; i++) {
				var mid = Object.create(i % 3 ? F.prototype : {m: 5})
				var o = Object.create(mid)
				o.own = i
				xs.push(o)
			}
			return xs
		}
		var xs = make(64)
		function count(xs, n) {
			var c = 0
			for (var i = 0; i < n; i++) {
				var x = xs[i % xs.length]
				if (x instanceof F) c++
				if ("m" in x) c += 10
				c += x.m + x.own * 100
			}
			return c
		}
		function churn(n) {
			var s = 0
			for (var i = 0; i < n; i++) {
				var o = Object.create({v: i})
				var p = Object.create(o)
				p.w = 1
				s += p.v + p.w + (p instanceof F ? 1000 : 0) + ("v" in p ? 1 : 0)
			}
			return s
		}
		function reproto(k) { for (var i = 0; i < xs.length; i += k) Object.setPrototypeOf(Object.getPrototypeOf(xs[i]), i & 1 ? F.prototype : {m: 7}) }
		function run(n) { return count(xs, n) + churn(n) }`
	cases := []string{`run(64)`, `run(200)`, `(reproto(5), run(200))`, `run(300)`, `(reproto(3), run(64))`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	// The objects the loop reads, the first of their prototypes, keep their prototypes in their tables.
	xs := r.global.getOwn(r.atoms.intern("xs")).value().Object()
	if o := xs.elemsOf()[1].Object(); o.flags&objPolyProto == 0 || o.proto().flags&objPolyProto == 0 {
		t.Fatalf("xs[1] and its prototype: poly %v and %v; want both", o.flags&objPolyProto != 0, o.proto().flags&objPolyProto != 0)
	}
	for _, c := range cases {
		wv, werr := want.Run(compileForTest(t, "String("+c+")"))
		gv, gerr := r.Run(compileForTest(t, "String("+c+")"))
		if werr != nil || gerr != nil {
			t.Fatalf("%s: %v, interpreter's %v", c, gerr, werr)
		}
		if got, w := gv.String().Go(), wv.String().Go(); got != w {
			t.Fatalf("%s: got %s, interpreter %s", c, got, w)
		}
	}
	// In a runtime of its own, an instanceof loop over them runs natively.
	r = jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function F() {}
		F.prototype = {}
		var xs = []
		for (var i = 0; i < 64; i++) xs.push(Object.create(Object.create(i & 1 ? F.prototype : Object.prototype)))
		function isF(xs, n) { var c = 0; for (var i = 0; i < n; i++) if (xs[i & 63] instanceof F) c++; return c }`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Run(compileForTest(t, `isF(xs, 640)`)); err != nil {
			t.Fatal(err)
		}
	}
	fn := r.global.getOwn(r.atoms.intern("isF")).value().Object().fn().closure
	e := r.jit.hint(fn)
	if e == nil || e.ssa == nil {
		t.Fatalf("isF was not compiled: %+v", e)
	}
	before := e.ssaStats
	if v, err := r.Run(compileForTest(t, `isF(xs, 6400)`)); err != nil || v.Number() != 3200 {
		t.Fatal(v, err)
	}
	st := e.ssaStats
	if r.jit.hint(fn) != e || st.work-before.work < 6400 {
		t.Fatalf("isF(xs, 6400) did not run natively: %+v, before %+v", st, before)
	}
	if left := st.hosts - before.hosts; left != 0 {
		t.Errorf("isF(xs, 6400) left native code %d times", left)
	}
}

// The in operator asked in a loop of an object made by Object.create of a
// prototype made for it, which keeps the prototype in its table, is
// answered natively after its first turns: the Go call native code makes
// for an answer its probe of hasCache does not find (jitHas) gives such an
// object a layout of its own when asked again, as the tree's in node does,
// and the entry then made for it is native code's. It had called Go at
// every turn, each run of the loop making the object anew (prop_in, five
// times main's time over ten runs); and the additions it makes to the
// object before the loop are compiled to write, not to deoptimize.
func TestJITPolyProtoIn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function probe(n) {
			var proto = {inherited: 1}, obj = Object.create(proto), s = 0
			obj.a = 1
			obj.b = 2
			obj.c = 3
			for (var j = 0; j < n; j++) {
				if ("a" in obj) s++
				if ("z" in obj) s += 10
				if ("inherited" in obj) s += 100
				if ("toString" in obj) s += 1000
			}
			return s
		}`)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if v, err := r.Run(compileForTest(t, `probe(2000)`)); err != nil || v.Number() != 2000*1101 {
			t.Fatal(v, err)
		}
	}
	v, calls := jitCountGoCalls(t, r, `probe(20000)`)
	if v.Number() != 20000*1101 {
		t.Fatalf("probe(20000) = %v", v)
	}
	if calls > 100 {
		t.Errorf("probe(20000) called Go %d times", calls)
	}
	// Nor are its writes before the loop, additions to such an object,
	// which no cache remembers, sites that know nothing, which the code
	// deoptimizes at (ssa's unfedSite) -- each run, the loop then run on from
	// the interpreter -- but megamorphic ones (jitFeedback.megamorphic),
	// which Go makes from native code.
	fn := r.global.getOwn(r.atoms.intern("probe")).value().Object().fn().closure
	e := r.jit.hint(fn)
	if e == nil || e.ssa == nil {
		t.Fatalf("probe was not compiled: %+v", e)
	}
	fb := &jitFeedback{r: r, fn: fn.fn, cl: fn, e: e}
	writes := 0
	for pc, in := range fn.fn.Code {
		if in.Op != bytecode.OpSetProp {
			continue
		}
		writes++
		if site, ok := fb.Property(pc); !ok || !site.Megamorphic {
			t.Errorf("write at %d: %+v, ok %v; want a megamorphic site", pc, site, ok)
		}
	}
	if writes != 3 {
		t.Fatalf("%d writes, want 3", writes)
	}
}
