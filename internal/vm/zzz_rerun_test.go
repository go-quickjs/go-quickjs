package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"weak"

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
// a shared layout. Whether the host runs it as a Program, whose literals'
// roots are kept, or otherwise, whose shapes go with each run.
func TestShapeReruns(t *testing.T) {
	testShapeReruns(t, 2*budgetSlots, func() *Runtime { return New(Config{}) })
}

// testShapeReruns is TestShapeReruns for runtimes newRuntime makes, the tree
// allowed to grow by slack of what a budget's chunk or two can keep.
func testShapeReruns(t *testing.T, slack int, newRuntime func() *Runtime) {
	for _, mode := range rerunModes {
		t.Run(mode.name, func(t *testing.T) {
			for _, s := range rerunScripts {
				t.Run(s.name, func(t *testing.T) {
					r := newRuntime()
					defer r.Close()
					fn := compileForTest(t, s.src)
					run := func(int) error { return mode.run(r, fn) }
					first := liveShapesAfter(t, r, 200, run)
					second := liveShapesAfter(t, r, 800, run)
					third := liveShapesAfter(t, r, 1000, run)
					t.Logf("after 200, 1000 and 2000 runs: %d, %d, %d shapes", first, second, third)
					if third > first+slack || second > first+slack {
						t.Errorf("after 200, 1000 and 2000 runs the tree holds %d, %d and %d shapes; want it flat", first, second, third)
					}
				})
			}
		})
	}
}

// rerunModes are the ways a host runs one compiled script again.
var rerunModes = []struct {
	name string
	run  func(r *Runtime, fn *bytecode.Function) error
}{
	{"Run", func(r *Runtime, fn *bytecode.Function) error {
		if _, err := r.Run(fn); err != nil {
			return err
		}
		return r.DrainJobs()
	}},
	{"RunProgram", func(r *Runtime, fn *bytecode.Function) error {
		if _, err := RunProgram[struct{}](r, fn); err != nil {
			return err
		}
		return r.DrainJobs()
	}},
}

// Programs run in turn keep the tree as flat as one does.
func TestShapeRerunsAlternating(t *testing.T) {
	for _, mode := range rerunModes {
		t.Run(mode.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			var fns []*bytecode.Function
			for _, s := range rerunScripts {
				fns = append(fns, compileForTest(t, s.src))
			}
			run := func(i int) error { return mode.run(r, fns[i%len(fns)]) }
			first := liveShapesAfter(t, r, 200*len(fns), run)
			second := liveShapesAfter(t, r, 800*len(fns), run)
			t.Logf("after 200 and 1000 rounds: %d, %d shapes", first, second)
			if second > first+4*budgetSlots {
				t.Errorf("after 200 and 1000 rounds the tree holds %d and %d shapes; want it flat", first, second)
			}
		})
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

// Past what the tree held on main, a program's runs have left it room: a
// new constructor's objects and a new literal's share a layout, and the
// program's own literals have kept theirs.
func TestShapeAfterReruns(t *testing.T) {
	for _, mode := range rerunModes {
		t.Run(mode.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			fn := compileForTest(t, `var o = {a: 1, b: 2}, p = {c: 3, d: 4}; o.a + p.d`)
			for i := 0; i < 25000; i++ {
				if err := mode.run(r, fn); err != nil {
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
		})
	}
}

// programsForTest is the programs the current realm keeps.
func programsForTest(r *Runtime) *programTable {
	return r.proto.object.shape.side.programs
}

// runProgramForTest runs fn as a Program and drains the jobs it left.
func runProgramForTest(t testing.TB, r *Runtime, fn *bytecode.Function) Value {
	t.Helper()
	v, err := RunProgram[struct{}](r, fn)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.DrainJobs(); err != nil {
		t.Fatal(err)
	}
	return v
}

// A Program's literals start from the roots the realm keeps for it: its
// second run, and each after it, grows the tree by nothing, with no
// collection to give anything back, and its objects have the layouts the
// first run's had. Each run had made its literals' shapes anew, and what any
// cache or native code knew of them was of one run.
func TestProgramRerunsMakeNoShapes(t *testing.T) {
	for _, s := range rerunScripts[:5] {
		t.Run(s.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			fn := compileForTest(t, s.src)
			runProgramForTest(t, r, fn)
			settleSites[struct{}](r)
			count := r.shapes.count
			shapes := map[string]*shape{}
			for _, name := range []string{"o", "a", "b"} {
				if p := r.global.getOwn(r.atoms.intern(name)); p.ok() && p.value().IsObject() {
					shapes[name] = p.value().Object().shape
				}
			}
			for run := 2; run <= 50; run++ {
				runProgramForTest(t, r, fn)
				settleSites[struct{}](r)
				if r.shapes.count != count {
					t.Fatalf("run %d: the tree counts %d shapes, %d after the first", run, r.shapes.count, count)
				}
				for name, sh := range shapes {
					if o := globalObject(r, name); o.shape != sh {
						t.Fatalf("run %d: %s has a layout the first run's had not", run, name)
					}
				}
			}
		})
	}
}

// What a run learned of a literal's objects' room, the next starts with.
func TestProgramRerunsKeepRoom(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `var o = {a: 1}; o.b = 2; o.c = 3; o.d = 4; o.e = 5`)
	runProgramForTest(t, r, fn)
	first := globalObject(r, "o").shape.root()
	if first.slack == 0 {
		t.Fatal("the literal's objects learned no room")
	}
	runProgramForTest(t, r, fn)
	if root := globalObject(r, "o").shape.root(); root != first {
		t.Errorf("the second run's object grew from root %p, the first's from %p", root, first)
	}
}

// A literal keeps its object's meaning however many runs share its root:
// each run's objects are its own, what it does to them is not seen by the
// next, and a run that keeps one sees it unchanged.
func TestProgramRerunsAnswers(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `
		var kept = this.kept || (this.kept = {n: 0, list: []})
		var o = {a: kept.n, b: {c: kept.n * 2}, __proto__: {p: 1}}
		function make(i) { return {i, get twice() { return this.i * 2 }, ["k" + (i & 1)]: i} }
		var m = make(kept.n)
		delete o.a
		o.d = 1
		Object.freeze(m)
		kept.list.push(m)
		kept.n++
		JSON.stringify([Object.keys(o), o.p, o.b.c, Object.keys(m), m.twice, kept.list.length, Object.getPrototypeOf({}) === Object.prototype])`)
	for run := 0; run < 6; run++ {
		want := fmt.Sprintf(`[["b","d"],1,%d,["i","twice","k%d"],%d,%d,true]`, run*2, run&1, run*2, run+1)
		if got := runProgramForTest(t, r, fn).String().Go(); got != want {
			t.Fatalf("run %d: got %s, want %s", run, got, want)
		}
	}
}

// Each realm has roots of its own for a program, of its own Object.prototype:
// a realm's objects never have another's layouts.
func TestProgramRerunsRealmsApart(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `var o = {a: 1}; o instanceof Object && Object.getPrototypeOf(o) === Object.prototype`)
	inRealm := func(re *Realm) *Object {
		t.Helper()
		var o *Object
		r.InRealm(re, func() {
			if v := runProgramForTest(t, r, fn); !v.Truthy() {
				t.Fatal("an object of another realm's prototype")
			}
			o = globalObject(r, "o")
		})
		return o
	}
	a, b := r.NewRealm(), r.NewRealm()
	for range 3 {
		x, y := inRealm(a), inRealm(b)
		if x.shape == y.shape || x.shape.root() == y.shape.root() {
			t.Fatal("two realms share a literal's layout")
		}
		if x.shape.proto != a.proto.object || y.shape.proto != b.proto.object {
			t.Fatal("a literal's layout is of another realm's prototype")
		}
	}
}

// Keeping a program's roots keeps neither a realm a host has dropped nor a
// program it has.
func TestProgramRerunsLetGo(t *testing.T) {
	t.Run("realms", func(t *testing.T) {
		r := New(Config{})
		defer r.Close()
		fn := compileForTest(t, `var o = {a: 1, b: 2}, e = {}; function f() { return {c: 1, d: 2} } f()`)
		const realms = 200
		var refs []weak.Pointer[Realm]
		for range realms {
			re := r.NewRealm()
			refs = append(refs, weak.Make(re))
			r.InRealm(re, func() {
				runProgramForTest(t, r, fn)
				runProgramForTest(t, r, fn)
			})
		}
		if n := realmsAlive(r, refs); n != 0 {
			t.Fatalf("%d realms alive", n)
		}
		if n := liveShapes(r); n > 2*budgetSlots*8 {
			t.Errorf("%d shapes left of %d realms'", n, realms)
		}
	})
	t.Run("programs", func(t *testing.T) {
		r := New(Config{})
		defer r.Close()
		const programs = 2000
		var last weak.Pointer[bytecode.Function]
		for i := range programs {
			fn := compileForTest(t, fmt.Sprintf(`var o = {a: %d, b: {c: 1}}`, i))
			last = weak.Make(fn)
			runProgramForTest(t, r, fn)
			if i%500 == 0 {
				runtime.GC()
			}
		}
		runtime.GC()
		runtime.GC()
		if last.Value() != nil {
			t.Error("the last program, dropped, is alive")
		}
		if n := len(programsForTest(r).byCode); n > 1024 {
			t.Errorf("the realm keeps %d programs' roots of %d, each dropped", n, programs)
		}
		if n := liveShapes(r); n > 1024*4 {
			t.Errorf("the tree holds %d shapes", n)
		}
	})
}

// A program whose literals' objects are given keys it makes up grows what
// its roots hold at each run, which nothing gives back while they are kept:
// past programShapes they are let go, and go, and the tree has room.
func TestProgramRerunsMakingUpKeys(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	// Keys from a few, in orders of their own: each level of an object's
	// path has room for a transition no run has taken.
	fn := compileForTest(t, `
		var x = this.x = this.x || 1, o = {}
		for (var i = 0; i < 6; i++) {
			x = (Math.imul(x, 1103515245) + 12345) & 0x7fffffff
			o["k" + (x >> 8) % 60] = i
		}
		this.x = x`)
	var kept *programSites
	lets := 0
	for run := 0; run < 60000; run++ {
		runProgramForTest(t, r, fn)
		if p := programsForTest(r).byCode[weak.Make(fn)]; p != kept {
			kept = p
			lets++
		}
	}
	t.Logf("roots made %d times; %d shapes of the last; count %d", lets, kept.budget.b.n.Load(), r.shapes.count)
	if lets < 2 {
		t.Errorf("roots made %d times; the program's were not let go", lets)
	}
	if n := liveShapes(r); n > programShapes+1024 {
		t.Errorf("the tree holds %d shapes of a program that made up its keys", n)
	}
	if r.shapes.count >= maxShapes {
		t.Error("the tree is full")
	}
}

// A shape takes its budget from its parent, which is what a literal's root
// took at its first child: nothing it costs grows with its depth. Asked of
// the root, the shapes of an object of a thousand properties had walked half
// a million parents, and each new shape of any kind paid for its own.
func TestShapeChargedFromItsParent(t *testing.T) {
	r := New(Config{})
	var b strings.Builder
	b.WriteString("var o = {")
	for i := range 1000 {
		fmt.Fprintf(&b, "k%d: %d,", i, i)
	}
	b.WriteString("}; o")
	v, err := r.Run(compileForTest(t, b.String()))
	if err != nil {
		t.Fatal(err)
	}
	s := v.Object().shape
	if s == nil || s.unique || s.n != 1000 {
		t.Fatalf("no shared layout of a thousand keys: %+v", s)
	}
	root := s.root()
	if root.budget == nil {
		t.Fatal("the literal's root has no budget")
	}
	for c := s; c != root; c = c.parent {
		if c.budget != root.budget {
			t.Fatalf("the shape of %d keys has not its root's budget", c.n)
		}
	}
}
