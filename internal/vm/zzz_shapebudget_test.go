package vm

import (
	"runtime"
	"testing"
	"time"
)

// liveShapes is how many shapes r's tree holds, the cleanups of its
// collected prototypes run first: what its count is once it takes back what
// they freed (reclaimShapes).
func liveShapes(r *Runtime) int {
	t := r.shapes
	// The runtime's caches of megamorphic sites hold a bounded number of
	// shapes, as many as their entries, which hashes of their addresses
	// pick: made again when next needed.
	r.shared, r.deep, r.has = nil, nil, nil
	// The chunk of budgets being given out is the tree's, which holds it.
	t.chunk = nil
	freed := int64(-1)
	for i := 0; i < 100; i++ {
		runtime.GC()
		if t.freed == nil {
			break
		}
		// The cleanups run on a goroutine of their own, after the cycle.
		time.Sleep(time.Millisecond)
		if n := t.freed.Load(); n == freed && i > 3 {
			break
		} else {
			freed = n
		}
	}
	reclaimShapes[struct{}](t)
	return t.count
}

// Objects made on prototypes made and dropped, or alternating between
// prototypes, leave the tree no bigger than they found it: a prototype's
// shapes are given back to the budget once it is collected, the first
// ordinary object made on a prototype joins no tree, and a constructor
// meeting a prototype it has met before grows its objects from the root it
// did then.
// With the prototype in the layout, every prototype had roots of its own,
// which counted against the tree for good, and a constructor alternating
// between two made a new root each time: each pattern below made two or
// three shapes an iteration, and once the tree was full every layout after
// it was of its own.
func TestShapeBudgetChurn(t *testing.T) {
	setup := `
		function F() { this.a = 1; this.b = 2 }
		function G() {} function H() {}
		G.prototype = Object.create(F.prototype); H.prototype = Object.create(F.prototype)
		function R() { this.r = 1 }
		var P1 = {m: 1}, P2 = {m: 2}
		function A(v) { this.x = v; this.y = v + 1 }
		function B() {}
		B.prototype = Object.create(A.prototype)
		class Base { constructor() { this.b = 1 } }
		class Sub1 extends Base { constructor() { super(); this.s = 1 } }
		class Sub2 extends Base { constructor() { super(); this.s = 2 } }
		function frame(parent, i) { var s = Object.create(parent); s.i = i; s.v = i; return s }
		var scope = {g: 1}
		var last
	`
	// want is the shapes the tree holds after the pattern's 20,000
	// iterations, and after 20,000 more: its own few, the realm's, those of
	// the first prototypes it made, whose budgets share a chunk with
	// prototypes that live (budgetChunk), and those of the objects whose
	// prototype is in their table, the first made on each prototype
	// (zzz_polyproto.go) -- which, past the first, are all a class made by
	// a factory makes.
	patterns := []struct {
		name, body string
		want       int
	}{
		{"Object.create", `var p = Object.create({x: i}); p.y = 1`, 21},
		{"Object.create twice", `var q = {x: i}, p = Object.create(q), p2 = Object.create(q); p.y = 1; p2.y = 2`, 75},
		{"__proto__ literal", `var o = {__proto__: {z: i}, w: 1}`, 23},
		{"setPrototypeOf", `var o = Object.setPrototypeOf({}, {k: i}); o.v = 1`, 23},
		{"scope chain", `var a = frame(scope, i); var b = frame(a, i)`, 23},
		{"Reflect.construct alternating", `var o = Reflect.construct(F, [], i & 1 ? G : H)`, 25},
		{"prototype reassigned", `R.prototype = i & 1 ? P1 : P2; var o = new R()`, 23},
		{"Babel _createSuper", `var o = i & 1 ? Reflect.construct(A, [i], B) : new A(i)`, 25},
		{"subclasses alternating", `var o = i & 1 ? new Sub1() : new Sub2()`, 24},
		{"class factory", `var K = class { constructor() { this.q = i; this.t = 1 } }; var o = new K()`, 23},
		{"subclass churn", `var D = class extends Base { constructor() { super(); this.d = i } }; var o = new D()`, 23},
	}
	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			src := setup + `function loop(n) { for (var i = 0; i < n; i++) { ` + p.body + ` } }`
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			for run := 1; run <= 2; run++ {
				if _, err := r.Run(compileForTest(t, `loop(20000)`)); err != nil {
					t.Fatal(err)
				}
				if got := liveShapes(r); got != p.want {
					t.Fatalf("after %d iterations the tree holds %d shapes, want %d", run*20000, got, p.want)
				}
			}
		})
	}
}

// After the budget was spent on the shapes of prototypes since collected,
// a constructor and an object literal met for the first time still share
// layouts in the tree, and a read of their objects stays monomorphic.
func TestShapeBudgetAfterChurn(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	// Three shapes an iteration: more than the tree holds.
	if _, err := r.Run(compileForTest(t, `
		for (var i = 0; i < 65536; i++) { var q = {x: i}, p = Object.create(q), p2 = Object.create(q); p.y = 1; p.z = 2 }`)); err != nil {
		t.Fatal(err)
	}
	if got := liveShapes(r); got != 22 {
		t.Fatalf("after the churn the tree holds %d shapes, want 22", got)
	}
	v, err := r.Run(compileForTest(t, `
		function Q(a) { this.a = a; this.b = a + 1 }
		function make(a) { return new Q(a) }
		function lit(a) { return {u: a, v: 2} }
		// The first object made on a prototype keeps it in its table
		// (zzz_polyproto.go): the ones compared are made after it.
		make(0)
		var q1 = make(1), q2 = make(2), l1 = lit(1), l2 = lit(2)
		function A(v) { this.x = v; this.y = v + 1 }
		function B() {}
		B.prototype = Object.create(A.prototype)
		for (var i = 0; i < 2000; i++) { var o = i & 1 ? Reflect.construct(A, [i], B) : new A(i); if (i == 3) var b1 = o; if (i == 1999) var b2 = o }
		String(q1.a + q2.b + l1.u + l2.v + b1.x + b2.y)`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "2010" {
		t.Fatalf("got %s, want 2010", got)
	}
	for _, pair := range [][2]string{{"q1", "q2"}, {"l1", "l2"}, {"b1", "b2"}} {
		a, b := globalObject(r, pair[0]), globalObject(r, pair[1])
		if a.shape != b.shape || a.shape.unique || a.shape.loose {
			t.Errorf("%s and %s: shapes %p and %p, unique %v; want one shared layout", pair[0], pair[1], a.shape, b.shape, a.shape.unique)
		}
	}
}

// The first ordinary object made on a prototype keeps it in its table, of
// a layout in the tree the runtime keeps for such objects, which a cache
// may remember, whatever their prototypes (zzz_polyproto.go); the objects
// after it start from the root the prototype's layout keeps, and so does
// the first once a cache finds a property past it. Objects of other classes
// start from the prototype's root from the first.
func TestShapeFirstObjectOfPrototype(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var p = {m: 1}, p2 = {m: 2}
		var a = Object.create(p), b = Object.create(p), a2 = Object.create(p2)
		var ap = []
		var arr1 = Object.setPrototypeOf([], ap), arr2 = Object.setPrototypeOf([], ap)`)); err != nil {
		t.Fatal(err)
	}
	a, b, a2 := globalObject(r, "a"), globalObject(r, "b"), globalObject(r, "a2")
	if a.flags&objPolyProto == 0 || a.shape.unique || a.shape.loose || a.shape != a2.shape || a.proto() != globalObject(r, "p") || a2.proto() != globalObject(r, "p2") {
		t.Errorf("first objects of p and p2: poly %v, unique %v, loose %v, shapes %p and %p; want one layout of their prototypes' tables",
			a.flags&objPolyProto != 0, a.shape.unique, a.shape.loose, a.shape, a2.shape)
	}
	if b.flags&objPolyProto != 0 || b.shape.unique || b.shape.loose || b.shape.tree != r.shapes || b.proto() != globalObject(r, "p") {
		t.Errorf("second object: unique %v, loose %v, tree %p; want the prototype's root", b.shape.unique, b.shape.loose, b.shape.tree)
	}
	if arr1, arr2 := globalObject(r, "arr1"), globalObject(r, "arr2"); arr1.shape != arr2.shape || arr1.shape.loose {
		t.Errorf("arrays on a fresh prototype: shapes %p and %p, loose %v; want one root", arr1.shape, arr2.shape, arr1.shape.loose)
	}
	v, err := r.Run(compileForTest(t, `
		function set(o, v) { o.x = v; o.y = v }
		set(a, 1); set(b, 2); set(a2, 3)
		var keys = Object.keys(a).join() + ";" + Reflect.ownKeys(a2).join() + ";" + JSON.stringify(a)
		Object.setPrototypeOf(a2, p)
		keys + ";" + a.m + a2.m + (Object.getPrototypeOf(a2) === p)`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), `x,y;x,y;{"x":1,"y":1};11true`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if a.flags&objPolyProto == 0 || a.shape.unique || a.shape != a2.shape || a.shape.n != 3 {
		t.Errorf("first objects given x and y: poly %v, unique %v, %d keys; want one layout, of the slot and two keys", a.flags&objPolyProto != 0, a.shape.unique, a.shape.n)
	}
	if b.shape.unique || b.shape.loose || b.shape.parent == nil {
		t.Errorf("second object given x and y: unique %v, loose %v; want a layout in the tree", b.shape.unique, b.shape.loose)
	}
	if _, err := r.Run(compileForTest(t, `var c = Object.create(a2); function rd(o) { return o.m } rd(c) + rd(c)`)); err != nil {
		t.Fatal(err)
	}
	// c is the first made on a2, and a read of it finds nothing past it;
	// a2 becomes a prototype of its own layout once one made after c is
	// read through a cache.
	if _, err := r.Run(compileForTest(t, `var d = Object.create(a2); rd(d) + rd(d)`)); err != nil {
		t.Fatal(err)
	}
	if a2.flags&objPolyProto != 0 || !a2.shape.unique || a2.shape.proto != globalObject(r, "p") || !a2.vals()[0].IsUndefined() {
		t.Errorf("a2 as a prototype: poly %v, unique %v; want a layout of its own of prototype p, its slot cleared", a2.flags&objPolyProto != 0, a2.shape.unique)
	}
}

// A singleton made on a prototype of its own -- by Object.create, a
// literal's __proto__, setPrototypeOf -- is read through a cache that
// remembers its layout, as any object's: the first object made on a
// prototype had had a loose layout, which no cache remembers, and each read
// of it was a lookup in its table.
func TestShapeSingletonOnOwnPrototype(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var c = Object.create({k: 1}); c.a = 1; c.b = 2
		var l = {__proto__: {k: 2}, a: 1, b: 2}
		var s = Object.setPrototypeOf({a: 1, b: 2}, {k: 3})
		function readC(o) { return o.a + o.b + o.k }
		function readL(o) { return o.a + o.b + o.k }
		function readS(o) { return o.a + o.b + o.k }
		var sum = 0
		for (var i = 0; i < 100; i++) sum += readC(c) + readL(l) + readS(s)
		String(sum)`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "1500" {
		t.Fatalf("sum %s, want 1500", got)
	}
	for _, n := range []string{"c", "l", "s"} {
		o := globalObject(r, n)
		if o.shape.loose || !o.shape.seen {
			t.Errorf("%s: loose %v, seen %v; want a layout a cache remembers", n, o.shape.loose, o.shape.seen)
		}
	}
}

// Classes made to extend one base, as a mixin or a factory makes one for
// each call, share a layout of the base's after the first, whose own it is:
// each had had a layout of its own made as it was given its prototype.
// Static inheritance reads through it as through any.
func TestShapeSubclassesShareLayout(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		class Base { static make() { return new this() } static get kind() { return "base" } }
		Base.tag = 7
		var B = Base, made = []
		for (var i = 0; i < 4; i++) made.push(class extends Base { static extra() { return i } })
		var d1 = made[1], d3 = made[3]
		var first = made[0]
		var sum = 0
		for (var i = 0; i < 4; i++) sum += made[i].tag + (made[i].make() instanceof made[i] ? 1 : 0) + made[i].extra()
		Base.tag = 1
		;[sum, d3.tag, d3.kind, Object.getPrototypeOf(d3) === Base, d1.hasOwnProperty("tag")].join()`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "38,1,base,true,false"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	d1, d3, first := globalObject(r, "d1"), globalObject(r, "d3"), globalObject(r, "first")
	if d1.shape != d3.shape || d1.shape.unique || d1.shape.loose || d1.proto() != globalObject(r, "B") {
		t.Errorf("second and fourth subclass: shapes %p and %p, unique %v; want one layout of Base's", d1.shape, d3.shape, d1.shape.unique)
	}
	if !first.shape.unique || first.proto() != globalObject(r, "B") {
		t.Errorf("first subclass: unique %v; want a layout of its own", first.shape.unique)
	}
}
