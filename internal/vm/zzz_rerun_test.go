package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// rerunScripts are programs a host runs again and again in one realm, each
// made for what a run of it leaves in the shape tree.
var rerunScripts = []struct{ name, src string }{
	{"two literals", `var o = {a: 1, b: 2}, p = {c: 3, d: 4}; o.a + p.d`},
	{"literals of several shapes", `
		var a = {x: 1}, b = {x: 1, y: 2}, c = {y: 1, x: 2}, d = {}, e = {"k-1": 1, 2: 2}, f = {get g() { return 1 }, set g(v) {}}
		d.later = a.x + b.y + c.x; f.g`},
	{"nested literals", `var o = {a: {b: {c: {d: 1}}, e: [{f: 1}, {f: 2}]}, g: {h: 1}}; o.a.b.c.d + o.a.e[1].f`},
	{"spread and rest", `var a = {x: 1, y: 2}, b = {...a, z: 3}, {x, ...c} = b; c.z`},
	{"literal in a function", `function f(i) { return {i, j: i + 1} } var s = 0; for (var i = 0; i < 10; i++) s += f(i).j; s`},
	{"classes", `{
		class A { constructor() { this.x = 1 } m() { return this.x } static s = 2 }
		class B extends A { #p = 3; constructor() { super(); this.y = 2 } get p() { return this.#p } }
		var o = new B(); o.m() + o.y + o.p + A.s }`},
	{"functions", `
		function F(a) { this.a = a; this.b = a + 1 }
		F.prototype.get = function () { return this.a }
		F.tag = "f"
		var o = new F(1); o.get() + new F(2).b`},
	{"closures", `
		function counter() { var n = 0; return { inc: () => ++n, get: function () { return n } } }
		var c = counter(); c.inc(); c.inc(); c.get()`},
	{"regexps", `var m = /(?<y>\d+)-(\d+)/.exec("10-20"), r = /a/g; m.groups.y + "abab".replace(/b/g, "c") + r.lastIndex`},
	{"arguments", `function f() { return arguments.length + arguments[0] } function g(a) { "use strict"; arguments[0] = 2; return arguments[0] } f(1, 2) + g(1)`},
	{"generators and promises", `
		function* g() { yield {v: 1}; yield {v: 2} }
		var s = 0; for (var x of g()) s += x.v
		Promise.resolve({w: 1}).then(o => ({w: o.w + 1})); s`},
	{"prototypes of literals", `
		var base = {m() { return 1 }}, o = {__proto__: base, a: 1}, p = Object.create(base), q = Object.create({n: 1})
		p.b = 2; q.c = 3; o.m() + p.b + q.n`},
}

// liveShapesAfter runs run n times in r and reports the tree's count.
func liveShapesAfter(t *testing.T, r *Runtime, n int, run func(i int) error) int {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := run(i); err != nil {
			t.Fatal(err)
		}
	}
	return liveShapes(r)
}

// A program run again and again in one realm leaves the tree no bigger than
// its first few runs did: each run prepared its code anew, so that each of
// its object literals had a new root and new shapes grown from it, which
// went with the run's closures, but were counted against the realm's
// Object.prototype, which in a runtime's only realm is never collected. Two
// literals reached maxShapes in 20,000 runs, and from then on no object had
// a shared layout.
func TestShapeReruns(t *testing.T) {
	for _, s := range rerunScripts {
		t.Run(s.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			fn := compileForTest(t, s.src)
			run := func(int) error {
				if _, err := r.Run(fn); err != nil {
					return err
				}
				return r.DrainJobs()
			}
			first := liveShapesAfter(t, r, 200, run)
			second := liveShapesAfter(t, r, 800, run)
			third := liveShapesAfter(t, r, 1000, run)
			t.Logf("after 200, 1000 and 2000 runs: %d, %d, %d shapes", first, second, third)
			if third > first+2*budgetSlots || second > first+2*budgetSlots {
				t.Errorf("after 200, 1000 and 2000 runs the tree holds %d, %d and %d shapes; want it flat", first, second, third)
			}
		})
	}
}

// Programs run in turn keep the tree as flat as one does.
func TestShapeRerunsAlternating(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	var fns []*bytecode.Function
	for _, s := range rerunScripts {
		fns = append(fns, compileForTest(t, s.src))
	}
	run := func(i int) error {
		if _, err := r.Run(fns[i%len(fns)]); err != nil {
			return err
		}
		return r.DrainJobs()
	}
	first := liveShapesAfter(t, r, 200*len(fns), run)
	second := liveShapesAfter(t, r, 800*len(fns), run)
	t.Logf("after 200 and 1000 rounds: %d, %d shapes", first, second)
	if second > first+4*budgetSlots {
		t.Errorf("after 200 and 1000 rounds the tree holds %d and %d shapes; want it flat", first, second)
	}
}

// A host that compiles what it runs each time -- Eval, for each request --
// or code that evaluates source has no program to run again, and each run a
// new literal: the tree stays as flat as for a Program.
func TestShapeFreshCodeRuns(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	run := func(i int) error {
		_, err := r.Run(compileForTest(t, fmt.Sprintf(`var o = {a: %d, b: {c: 1}}, e = {}; o.a`, i)))
		return err
	}
	first := liveShapesAfter(t, r, 200, run)
	second := liveShapesAfter(t, r, 1800, run)
	t.Logf("after 200 and 2000 runs: %d, %d shapes", first, second)
	if second > first+2*budgetSlots {
		t.Errorf("after 200 and 2000 runs the tree holds %d and %d shapes; want it flat", first, second)
	}
}

// Past what the tree holds on main, a program's runs have left it room: a
// new constructor's objects and a new literal's share a layout, and the
// program's own literals have kept theirs.
func TestShapeAfterReruns(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `var o = {a: 1, b: 2}, p = {c: 3, d: 4}; o.a + p.d`)
	for i := 0; i < 25000; i++ {
		if _, err := r.Run(fn); err != nil {
			t.Fatal(err)
		}
	}
	if got := liveShapes(r); got > 64 {
		t.Fatalf("after 25,000 runs the tree holds %d shapes", got)
	}
	v, err := r.Run(compileForTest(t, `
		function Q(a) { this.a = a; this.b = a + 1 }
		function make(a) { return new Q(a) }
		function lit(a) { return {u: a, v: 2} }
		// The first object made on a prototype keeps it in its table
		// (zzz_polyproto.go): the ones compared are made after it.
		make(0)
		var q1 = make(1), q2 = make(2), l1 = lit(1), l2 = lit(2)
		String(q1.a + q2.b + l1.u + l2.v)`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "7" {
		t.Fatalf("got %s, want 7", got)
	}
	for _, pair := range [][2]string{{"q1", "q2"}, {"l1", "l2"}} {
		a, b := globalObject(r, pair[0]), globalObject(r, pair[1])
		if a.shape != b.shape || a.shape.unique || a.shape.loose {
			t.Errorf("%s and %s: shapes %p and %p, unique %v; want one shared layout", pair[0], pair[1], a.shape, b.shape, a.shape.unique)
		}
	}
	if o := globalObject(r, "o"); o.shape.unique || o.shape.loose {
		t.Errorf("the program's literal: unique %v, loose %v; want a shared layout", o.shape.unique, o.shape.loose)
	}
}
