//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// regChainCtorSrc constructs with a namespace's constructor, the
// property chain NS.Geo.P read before each construction -- in run's loop,
// and in make, which run2's loop calls and inlines -- as RayTrace's
// new Flog.RayTracer.Vector(...) does; swap replaces the constructor.
const regChainCtorSrc = `
var NS = { Geo: { P: function (x, y) { this.x = x; this.y = y } } };
function run(n) { var s = 0; for (var i = 0; i < n; i++) { var p = new NS.Geo.P(i, i + 1); s += p.x + p.y } return s }
function make(i) { return new NS.Geo.P(2 * i, 1) }
function run2(n) { var s = 0; for (var i = 0; i < n; i++) { var p = make(i); s += p.x - p.y } return s }
function swap() { NS.Geo.P = function (x, y) { this.x = -x; this.y = y } }
`

// TestJITRegChainConstructionSeeded runs loops that construct with a
// function read through a property chain, NS.Geo.P, and checks that each is
// compiled once, its constructions native from that compile: the
// construction is seeded from the function the chain reads now
// (regDefined's get_prop), not learned at a call of Go and compiled again
// for it -- RayTrace's every construction had cost a compile more, 49
// compiles at -n 3. The constructor replaced, the construction checks it.
func TestJITRegChainConstructionSeeded(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regChainCtorSrc)
		r := rts[0]
		r.SetJITReport(true)
		call := func(name string, n float64) [2]string {
			var got [2]string
			for i, rt := range rts {
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(name))
				v, err := rt.Call(fn, Undefined, []Value{Float(n)})
				got[i] = regNativeShow(rt, v, err)
			}
			return got
		}
		for range 30 {
			if got := call("run", 1000); got[0] != got[1] || got[0] != "1000000" {
				t.Fatalf("run: with the JIT %q, without %q, want 1000000", got[0], got[1])
			}
			if got := call("run2", 1000); got[0] != got[1] || got[0] != "998000" {
				t.Fatalf("run2: with the JIT %q, without %q, want 998000", got[0], got[1])
			}
		}
		st, js := r.RegNativeStats(), r.JITStats()
		var made uint64
		for k, n := range st.GoCallOps {
			t.Logf("call of Go %s %d", k, n)
			if strings.HasPrefix(k, "new_w") {
				made += n
			}
		}
		made += st.HostOps["new_w"]
		if made != 0 {
			t.Errorf("Go made %d of the 60,000 constructions; want them native from the first compile", made)
		}
		for _, name := range []string{"run", "run2"} {
			if e := regCodeNamed(t, r, name); e == nil || e.code == nil {
				t.Errorf("%s has no code", name)
			}
		}
		t.Logf("%d compiled, %d compiled again", js.Compiled, js.Reoptimized)
		if js.Reoptimized != 0 {
			t.Errorf("%d compiled again; want each function compiled once", js.Reoptimized)
		}
		for _, rt := range rts {
			sw, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("swap"))
			if _, err := rt.Call(sw, Undefined, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := call("run", 1000); got[0] != got[1] || got[0] != "1000" {
			t.Errorf("P replaced, run: with the JIT %q, without %q, want 1000", got[0], got[1])
		}
		if got := call("run2", 1000); got[0] != got[1] || got[0] != "-1000000" {
			t.Errorf("P replaced, run2: with the JIT %q, without %q, want -1000000", got[0], got[1])
		}
	})
}

// regEmptyPoolSrc constructs in a loop, its objects from a pool.
const regEmptyPoolSrc = `
function P(x) { this.x = x }
function run(n) { var s = 0; for (var i = 0; i < n; i++) s += new P(i).x; return s }
`

// TestJITRegConstructionEmptyPool compiles a loop's construction again
// while the pool its objects come from is empty, as its callers had just
// run it out, and checks that the construction stays native, the pool
// filled again by the code (ssa's GoRefill), the site compiled whatever
// its pool holds. A site refused for an empty pool was
// compiled as a call of Go, which fills no pool, and every construction
// was Go's from then on: RayTrace's IntersectionInfo's, hundreds of
// thousands of times, in some runs.
func TestJITRegConstructionEmptyPool(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regEmptyPoolSrc)
		r := rts[0]
		r.SetJITReport(true)
		run := func() {
			for i, rt := range rts {
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
				v, err := rt.Call(fn, Undefined, []Value{Float(1000)})
				if got := regNativeShow(rt, v, err); got != "499500" {
					t.Fatalf("runtime %d: run gave %q, want 499500", i, got)
				}
			}
		}
		for range 10 {
			run()
		}
		e := regCodeNamed(t, r, "run")
		if e == nil || e.code == nil {
			t.Fatal("run has no code")
		}
		emptied := 0
		for _, x := range e.life.nativeCalls {
			if x.pool != nil {
				x.pool.Count = 0
				emptied++
			}
		}
		if emptied == 0 {
			t.Fatal("run constructs from no pool")
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("run"))
		cl := fn.Object().fn().closure
		if ne := r.jitRegCompileFor(cl, cl.reg, e.key, false); ne == nil || ne.code == nil {
			t.Fatal("run not compiled again")
		}
		before := r.RegNativeStats()
		for range 10 {
			run()
		}
		after := r.RegNativeStats()
		for k, n := range after.GoCallOps {
			if strings.HasPrefix(k, "new_w") && n != before.GoCallOps[k] {
				t.Errorf("%d constructions made by Go (%s) once compiled with the pool empty; want none", n-before.GoCallOps[k], k)
			}
		}
		if n := after.HostOps["new_w"] - before.HostOps["new_w"]; n != 0 {
			t.Errorf("%d constructions left native code; want none", n)
		}
	})
}

// regForwardCtorSrc is a class library's forwarding constructors
// (Prototype's Class.create), six classes' made by one forwarder's
// closures: its this.initialize read meets six prototypes.
const regForwardCtorSrc = `
var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
var A = Class.create(); A.prototype = { initialize: function (x) { this.v = x } };
var B = Class.create(); B.prototype = { initialize: function (x) { this.v = 2 * x } };
var C = Class.create(); C.prototype = { initialize: function (x) { this.v = 3 * x } };
var D = Class.create(); D.prototype = { initialize: function (x) { this.v = 4 * x } };
var E = Class.create(); E.prototype = { initialize: function (x) { this.v = 5 * x } };
var F = Class.create(); F.prototype = { initialize: function (x) { this.v = 6 * x } };
function run(n) {
	var s = 0;
	for (var i = 0; i < n; i++) s += new A(i).v + new B(i).v + new C(i).v + new D(i).v + new E(i).v + new F(i).v;
	return s;
}
`

// TestJITRegForwardingConstructionReads runs constructions of six classes
// made by one forwarding constructor, each inlined with the method it
// forwards to, and checks that none leaves native code: the inlined
// forwarder's reads, this.initialize and its apply, are compiled for the
// construction's object and the method its prototype holds
// (forwardProperty). Compiled for what the forwarder's own cache met, the
// read left at each class it had not, and once a construction had left
// often enough it was made by a native call of the forwarder, whose
// apply_args calls four methods natively and the rest by Go.
func TestJITRegForwardingConstructionReads(t *testing.T) {
	regBothMarking(t, func(t *testing.T) {
		rts := regNativeRuntimes(t, regForwardCtorSrc)
		r := rts[0]
		r.SetJITReport(true)
		for range 40 {
			var got [2]string
			for i, rt := range rts {
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern("run"))
				v, err := rt.Call(fn, Undefined, []Value{Float(500)})
				got[i] = regNativeShow(rt, v, err)
			}
			if got[0] != got[1] || got[0] != "2619750" {
				t.Fatalf("run: with the JIT %q, without %q, want 2619750", got[0], got[1])
			}
		}
		if e := regCodeNamed(t, r, "run"); e == nil || e.code == nil {
			t.Fatal("run has no code")
		}
		st := r.RegNativeStats()
		for k, n := range st.Ended {
			t.Logf("ended: %s %d", k, n)
			if strings.Contains(k, "left at get_prop") {
				t.Errorf("an inlined forwarder's read left native code %d times (%s); want none", n, k)
			}
		}
		for k, n := range st.GoCallOps {
			t.Logf("call of Go %s %d", k, n)
			if strings.HasPrefix(k, "apply_args") || strings.HasPrefix(k, "new_w") {
				t.Errorf("%d calls of Go %s; want the constructions inlined", n, k)
			}
		}
	})
}
