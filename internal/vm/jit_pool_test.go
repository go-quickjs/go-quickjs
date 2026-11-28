//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Pool objects have the interpreter's allocation layout, including its
// inline-property capacities and growth into separate property storage.
func TestJITObjectPoolLayouts(t *testing.T) {
	t.Parallel()
	proto := newObject(nil, ClassObject)
	root := &shape{proto: proto}
	for n := 0; n <= 16; n++ {
		pool := &abi.ObjectPool{Size: abi.PoolSize}
		jitFillObjectPool(pool, proto, root, n)
		want := newLiteralObject(proto, ClassObject, n)
		if pool.Count != pool.Size || pool.Proto != unsafe.Pointer(proto) {
			t.Fatalf("size %d: count %d, prototype %p", n, pool.Count, pool.Proto)
		}
		for i := range int(pool.Count) {
			o := (*Object)(pool.Objects[i])
			if o.proto() != proto || o.shape != root || o.class != want.class || o.flags != want.flags || o.valn != 0 || int(o.valc) != int(want.valc) {
				t.Fatalf("size %d, object %d: wrong initial layout", n, i)
			}
			o.shape = nil
			for j := 0; j <= int(want.valc); j++ {
				o.appendProp(Property{key: Atom(100 + j), value: Int(i + j)})
			}
			if o.flags&objInlineProps != 0 {
				t.Fatalf("size %d, object %d: growth did not leave inline storage", n, i)
			}
		}
		for i := range int(pool.Count) {
			if got := (*Object)(pool.Objects[i]).vals()[0]; got != Int(i) {
				t.Fatalf("size %d, object %d: property storage shared", n, i)
			}
		}
	}
}

// Keeping one pool object must not retain any other object from its refill,
// or the graph reachable from that other object, as a slab allocation would.
func TestJITObjectPoolIndependentLifetimes(t *testing.T) {
	for n := 0; n <= 9; n++ {
		pool := &abi.ObjectPool{Size: abi.PoolSize}
		jitFillObjectPool(pool, nil, nil, n)
		kept := (*Object)(pool.Objects[0])
		other := weak.Make((*Object)(pool.Objects[1]))
		clear(pool.Objects[:])
		runtime.GC()
		if other.Value() != nil {
			t.Fatalf("size %d: keeping one object retains another", n)
		}
		runtime.KeepAlive(kept)
	}
}

// jitFullTreeSrc builds, in functions the JIT compiles, objects of
// constructors -- one that hands its arguments on to an initializer, as
// Box2D's classes do -- and of a literal given properties after it is
// made, all made from pools, and keeps some of each; then says each kept
// object's keys and classes, which a pool that gave its objects one layout
// of their own had mixed up: each object's keys added to the others'.
const jitFullTreeSrc = `
function P(x, y) { this.x = x; this.y = y; }
function F() { F.init.apply(this, arguments); }
F.init = function (a) { this.a = a; this.b = a + 1; this.c = new P(a, -a); };
function lit(i) { var o = {}; o.u = i; o.v = i * 2; return o; }
var kept = [], bad = 0;
for (var i = 0; i < 3000; i++) {
  var p = new P(i, i + 1), f = new F(i), l = lit(i);
  if (i % 2) { p.odd = i; f.odd = i; l.odd = i; } else { p.even = i; f.even = i; l.even = i; }
  if (Object.keys(p).join() !== (i % 2 ? "x,y,odd" : "x,y,even") || p.x !== i || p.y !== i + 1 || !(p instanceof P)) bad++;
  if (Object.keys(f).join() !== (i % 2 ? "a,b,c,odd" : "a,b,c,even") || f.b !== i + 1 || !(f instanceof F) || !(f.c instanceof P) || f.c.y !== -i) bad++;
  if (Object.keys(l).join() !== (i % 2 ? "u,v,odd" : "u,v,even") || l.v !== 2 * i) bad++;
  if (i % 501 === 0) kept.push(p, f, l);
}
var out = [];
for (var j = 0; j < kept.length; j++) {
  var o = kept[j];
  out.push(Object.keys(o).join("") + (o instanceof P ? "P" : o instanceof F ? "F" : "-") + (o.odd !== undefined ? o.odd : o.even));
}
bad + " " + out.join(" ")
`

// With the tree of shapes full, an object a pool holds starts from a
// layout of its own, as each the interpreter makes does (newOrdinary): a
// pool had given all of its objects the one unique root it was refilled
// with, so that each object built natively added its keys to the others'
// table -- a constructor's field lost or another's in its place, a slot
// read past an object's values. Each object answers as the interpreter's,
// natively built, under every stress setting, with the collector idle and
// while it marks.
func TestJITPoolObjectsTreeFull(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const want = "0 xyevenP0 abcevenF0 uveven-0 xyoddP501 abcoddF501 uvodd-501 xyevenP1002 abcevenF1002 uveven-1002 " +
		"xyoddP1503 abcoddF1503 uvodd-1503 xyevenP2004 abcevenF2004 uveven-2004 xyoddP2505 abcoddF2505 uvodd-2505"
	run := func(t *testing.T, jit bool, c jitStressConfig) (string, JITStats) {
		r := newTestRuntime(t, Config{JIT: jit})
		defer r.Close()
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = c
		r.shapes.count, r.shapes.settled = maxShapes, maxShapes
		got, _ := jitRunWithin(t, r, jitFullTreeSrc, jitTestWindow)
		return got, r.JITStats()
	}
	if got, _ := run(t, false, jitStressConfig{}); got != want {
		t.Fatalf("interpreter: got %q, want %q", got, want)
	}
	stress := []jitStressConfig{{}, {threshold: true}, {threshold: true, budget: 3, deopt: 7},
		{threshold: true, background: true, checks: 1}}
	for _, marking := range jitMarkingModes() {
		for _, c := range stress {
			t.Run(fmt.Sprintf("marking=%v/%+v", marking, c), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				got, st := run(t, true, c)
				if got != want {
					t.Errorf("got %q, want %q", got, want)
				}
				if st.SSAEntries == 0 {
					t.Errorf("no native entry: %+v", st)
				}
			})
		}
	}
}

// Native code's {} at two sites of different functions makes its objects
// from pools of the realm's root for {}, as the interpreter's first object
// takes it: built alike, they share each layout, with the room of four.
func TestJITEmptyLiteralsShareAcrossSites(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function A(i) { var o = {}; o.a = i; o.b = i + 1; return o; }
function B(i) { var q = {}; q.a = -i; q.b = "b"; return q; }
var s = 0, x, y;
for (var i = 0; i < 3000; i++) { x = A(i); y = B(i); s += x.b; }
s + ""
`
	r := newTestRuntime(t, Config{JIT: true})
	defer r.Close()
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	got, _ := jitRunWithin(t, r, src, jitTestWindow)
	if got != "4501500" {
		t.Fatalf("got %q", got)
	}
	if st := r.JITStats(); st.SSAEntries == 0 {
		t.Fatalf("no native entry: %+v", st)
	}
	x, y := globalObject(r, "x"), globalObject(r, "y")
	if x.shape != y.shape || x.shape.unique || x.shape.n != 2 || x.shape.root().rootClass != emptyRootClass {
		t.Errorf("layouts %p and %p (unique %v, n %d)", x.shape, y.shape, x.shape.unique, x.shape.n)
	}
	if x.valc != emptyLiteralRoom || y.valc != emptyLiteralRoom {
		t.Errorf("room %d and %d, want %d", x.valc, y.valc, emptyLiteralRoom)
	}
}

// Native code's pools of a {} learn into its site's record as the
// interpreter's executions do: each fill reads the sample before it and
// samples the object it gives last, so that a loop compiled from its first
// turns, whose objects come from pools from then on, has its objects made
// with the room of the twenty properties they are given, where the realm's
// root's fixed room had each grow its table to 9, 18 and 39.
func TestJITEmptySitePoolLearns(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function mk(i) {
	var o = {};
	o.a = i; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
	for (var k = 0; k < 10; k++) o[k] = k;
	return o;
}
var s = 0, x;
for (var i = 0; i < 3000; i++) { x = mk(i); s += x.j; }
s + ""
`
	r := newTestRuntime(t, Config{JIT: true})
	defer r.Close()
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	got, _ := jitRunWithin(t, r, src, jitTestWindow)
	if got != "30000" {
		t.Fatalf("got %q", got)
	}
	if st := r.JITStats(); st.SSAEntries == 0 {
		t.Fatalf("no native entry: %+v", st)
	}
	x := globalObject(r, "x")
	if x.valn != 20 || x.valc != 20 || x.shape.unique || x.shape.root().rootClass != emptyRootClass {
		t.Errorf("the last object: %d properties in room for %d (unique %v)", x.valn, x.valc, x.shape.unique)
	}
	if ic := emptySiteOf(t, r, "mk"); ic.idx != 20 || ic.fills != 0 {
		t.Errorf("the site's record: room %d, %d samples left, want 20 and settled", ic.idx, ic.fills)
	}
	if r.emptySamples != nil {
		t.Errorf("the turn's end left %d samples", len(r.emptySamples))
	}
	// A {} the interpreter never ran: native code compiled before the site
	// first made an object makes them all from its pool, whose fills alone
	// teach the site.
	const late = `
function mk2(i, go) {
	if (!go) return null;
	var o = {};
	o.a = i; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
	for (var k = 0; k < 10; k++) o[k] = k;
	return o;
}
for (var i = 0; i < 200; i++) mk2(i, false);
var y, n = 0;
for (var i = 0; i < 3000; i++) { y = mk2(i, true); n += y.j; }
n + ""
`
	if got, _ := jitRunWithin(t, r, late, jitTestWindow); got != "30000" {
		t.Fatalf("got %q", got)
	}
	y := globalObject(r, "y")
	if y.valn != 20 || y.valc != 20 {
		t.Errorf("the last object of a site native code ran first: %d properties in room for %d", y.valn, y.valc)
	}
	if ic := emptySiteOf(t, r, "mk2"); ic.idx != 20 || ic.fills != 0 {
		t.Errorf("the site native code ran first: room %d, %d samples left, want 20 and settled", ic.idx, ic.fills)
	}
}
