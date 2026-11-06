package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// shapeOfNew is the layout of the object src, compiled for this run alone,
// evaluates to.
func shapeOfNew(t *testing.T, r *Runtime, src string) *shape {
	t.Helper()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	return v.Object().shape
}

// A literal of the same keys in the same order has the same layout in any code
// of a realm: a script compiled anew, a function's and a script's, and two
// sites of one function.
func TestLiteralKeysShareALayout(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	want := shapeOfNew(t, r, `({a: 1, b: 2})`)
	if want == nil || want.unique || want.loose || want.n != 2 {
		t.Fatalf("no shared layout: %+v", want)
	}
	for _, src := range []string{
		`// again
		({a: 3, b: 4})`,
		`({"a": 1, 'b': "x"})`,
		`(function () { return {a: 1, b: {c: 1}} })()`,
		`(() => { let o; for (let i = 0; i < 50; i++) o = i & 1 ? {a: i, b: i} : {a: 0, b: 0}; return o })()`,
		`({a: [1, 2], b() { return 1 }})`,
		`({a: 1, b: (function () { try { throw 1 } catch (e) { return e } })()})`,
	} {
		if got := shapeOfNew(t, r, src); got != want {
			t.Errorf("%s: a layout of its own", src)
		}
	}
	for _, src := range []string{
		`({b: 1, a: 2})`,
		`({a: 1})`,
		`({a: 1, b: 2, c: 3})`,
		`({a: 1, get b() { return 1 }})`,
		`({a: 1, ["b"]: 2})`,
		`({a: 1, a: 2, b: 3})`,
		`({a: 1, ...{b: 2}})`,
	} {
		if got := shapeOfNew(t, r, src); got == want {
			t.Errorf("%s: the layout of {a, b}", src)
		}
	}
}

// Each realm has its own, of its own Object.prototype.
func TestLiteralKeysByRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `({a: 1, b: 2})`)
	run := func(re *Realm) *shape {
		t.Helper()
		v, err := r.RunIn(re, fn)
		if err != nil {
			t.Fatal(err)
		}
		return v.Object().shape
	}
	a, b := r.NewRealm(), r.NewRealm()
	x, y := run(a), run(b)
	if x == y || x.proto != a.proto.object || y.proto != b.proto.object {
		t.Fatalf("realms' layouts %p, %p of prototypes %p, %p", x, y, x.proto, y.proto)
	}
	if run(a) != x {
		t.Error("a realm's second run had another layout")
	}
}

// New code makes no shape for a literal whose keys the realm has met, and
// nothing a run keeps of it holds the code.
func TestFreshCodeMakesNoLiteralShapes(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	src := func(i int) string {
		return fmt.Sprintf(`var o = {a: %d, b: {c: 1}}, f = function () { return {d: 1, e: [{f: 2}]} }; f().e[0].f + o.b.c`, i)
	}
	if _, err := r.Run(compileForTest(t, src(0))); err != nil {
		t.Fatal(err)
	}
	settleSites[struct{}](r)
	count := r.shapes.count
	for i := 1; i <= 500; i++ {
		if _, err := r.Run(compileForTest(t, src(i))); err != nil {
			t.Fatal(err)
		}
		settleSites[struct{}](r)
		if r.shapes.count != count {
			t.Fatalf("run %d: the tree counts %d shapes, %d after the first", i, r.shapes.count, count)
		}
	}
}

// Objects that grow teach the room of the literals of their keys, and of no
// others, however many they have.
func TestLiteralKeysRoom(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function grown() { var o = {a: 1, b: 2}; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; return o }
		function same() { return {a: 1, b: 2} }
		function other() { return {x: 1, y: 2} }
		for (var i = 0; i < 300; i++) { grown(); same(); other() }
		[same(), other()]`))
	if err != nil {
		t.Fatal(err)
	}
	objs := v.Object().elemsOf()
	if c := objs[0].Object().valc; c != 7 {
		t.Errorf("a literal of the grown literal's keys has room for %d, want 7", c)
	}
	if c := objs[1].Object().valc; c != 2 {
		t.Errorf("a literal of other keys has room for %d, want 2", c)
	}
}

// Code that makes up its literals' keys, new for each run, grows what the realm
// holds for as long as it lives: past literalShapes it starts again, and what
// no code has goes, so that the tree has room for what comes after.
func TestLiteralKeysMadeUp(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	roots := func() *literalRoots { return r.proto.object.shape.side.programs.roots }
	if _, err := r.Run(compileForTest(t, `({first: 1})`)); err != nil {
		t.Fatal(err)
	}
	first := roots()
	const runs = literalShapes * 3 / 4
	var src strings.Builder
	for i := 0; i < runs; {
		src.Reset()
		src.WriteString("[")
		for range 500 {
			fmt.Fprintf(&src, "{key%d: 1, other%d: 2},", i, i)
			i++
		}
		src.WriteString("]")
		if _, err := r.Run(compileForTest(t, src.String())); err != nil {
			t.Fatal(err)
		}
	}
	if roots() == first {
		t.Fatal("the realm's roots grew past their budget and were kept")
	}
	first = nil
	if n := liveShapes(r); n > literalShapes+1024 {
		t.Errorf("the tree holds %d shapes", n)
	}
	if r.shapes.count >= maxShapes {
		t.Error("the tree is full")
	}
	if s := shapeOfNew(t, r, `({after: 1, all: 2})`); s == nil || s.unique || s.n != 2 {
		t.Errorf("a literal after them has no shared layout: %+v", s)
	}
}

// With no room in the tree a literal does as it did: its objects are in no
// layout of a root, and are right.
func TestLiteralKeysTreeFull(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	r.shapes.count, r.shapes.settled = maxShapes, maxShapes
	v, err := r.Run(compileForTest(t, `var o = {new1: 1, new2: 2}; o.new1 + o.new2`))
	if err != nil {
		t.Fatal(err)
	}
	if v.Number() != 3 {
		t.Errorf("got %v, want 3", v)
	}
}

// The compiler says the keys of a literal that has nothing else, in the order
// of the literals' sites, a literal inside another after it: alike for the same
// keys, in order, however written, and for no others.
func TestLiteralKeysTable(t *testing.T) {
	fn := compileForTest(t, `
		var k = "k", a = {x: 1, y: {z: 2}}, b = {...a}, c = {}, d = {q: 1, q: 2}, e = {[k]: 1},
			f = {get g() { return 1 }}, h = {m() {}, "n-1": 2, 3: 4}, i = {__proto__: null}, j = {r: 1, ...a},
			l = {"x": 3, y() {}}, m = {y: 1, x: 2}, n = {ab: 1, c: 2}, o = {a: 1, bc: 2}`)
	var got []uint64
	for _, in := range fn.Code {
		if in.Op == bytecode.OpNewObject {
			got = append(got, keysOf[struct{}](fn, in.B))
		}
	}
	const xy, z, methods, again, yx, abc, abc2 = 0, 1, 7, 10, 11, 12, 13
	if len(got) != 14 {
		t.Fatalf("%d literals, want 14", len(got))
	}
	// A spread, {}, a key twice, a computed key, an accessor, __proto__ and
	// a spread after a key.
	for _, i := range []int{2, 3, 4, 5, 6, 8, 9} {
		if got[i] != 0 {
			t.Errorf("literal %d says its keys", i)
		}
	}
	for _, i := range []int{xy, z, methods, again, yx, abc, abc2} {
		if got[i] == 0 {
			t.Errorf("literal %d says no keys", i)
		}
	}
	if got[xy] != got[again] {
		t.Error("{x, y} and {\"x\", y() {}} are told apart")
	}
	if got[xy] == got[yx] || got[abc] == got[abc2] || got[xy] == got[z] {
		t.Errorf("different keys told alike: %x", got)
	}
	for i := 1; i < len(fn.LiteralKeys); i++ {
		if fn.LiteralKeys[i-1].Site >= fn.LiteralKeys[i].Site {
			t.Fatalf("the table is not in the order of its sites: %+v", fn.LiteralKeys)
		}
	}
}

// The same keys are told alike from one function to another, and from one
// compile to another, runtimes sharing what is compiled: nothing seeds it.
func TestLiteralKeysAcrossCompiles(t *testing.T) {
	a := compileForTest(t, `({one: 1, two: 2})`)
	b := compileForTest(t, `function f() { return [{"one": {}, "two": 1}] }`)
	inner := b.Constants[0].Fn
	if a.LiteralKeys[0].Keys != inner.LiteralKeys[0].Keys {
		t.Errorf("hashes %x and %x", a.LiteralKeys[0].Keys, inner.LiteralKeys[0].Keys)
	}
}

// Two lists of keys of one hash share a root, and it costs them no more than
// the first transition: each literal's objects have their keys, in their
// order, and what reads or adds to them finds them, in the interpreter, a tree
// and, under the JIT's tests, native code.
func TestLiteralKeysCollision(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	testLiteralKeysCollision(t, r)
}

func testLiteralKeysCollision(t *testing.T, r *Runtime) {
	const collide = 0x1234
	force := func(fn *bytecode.Function) *bytecode.Function {
		for i := range fn.LiteralKeys {
			fn.LiteralKeys[i].Keys = collide
		}
		for i := range fn.Constants {
			if c := &fn.Constants[i]; c.Fn != nil {
				for j := range c.Fn.LiteralKeys {
					c.Fn.LiteralKeys[j].Keys = collide
				}
			}
		}
		return fn
	}
	run := func(src string) string {
		t.Helper()
		v, err := r.Run(force(compileForTest(t, src)))
		if err != nil {
			t.Fatal(err)
		}
		return v.String().Go()
	}
	src := `
		function a(i) { return {p: i, q: 2} }
		function b(i) { return {q: i, p: 2, r: 3} }
		function c(i) { return {s: i} }
		var top1 = {p: 1, q: 2}, top2 = {q: 1, p: 2, r: 3}, top3 = {z: 1, y: 2}, out = []
		for (var i = 0; i < 300; i++) {
			var x = a(i), y = b(i), z = c(i)
			x.later = 1
			if (x.p !== i || x.q !== 2 || y.q !== i || y.p !== 2 || y.r !== 3 || z.s !== i) throw new Error("read " + i)
		}
		JSON.stringify([top1, top2, top3, a(1), b(2), c(3), Object.keys(x), Object.keys(y), Object.keys(z)])`
	want := `[{"p":1,"q":2},{"q":1,"p":2,"r":3},{"z":1,"y":2},{"p":1,"q":2},{"q":2,"p":2,"r":3},{"s":3},["p","q","later"],["q","p","r"],["s"]]`
	for range 3 {
		if got := run(src); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	x, y := globalObject(r, "top1"), globalObject(r, "top2")
	if x.shape.root() != y.shape.root() {
		t.Error("two lists of one hash have two roots")
	}
	if x.shape == y.shape {
		t.Error("two lists of keys have one layout")
	}
}
