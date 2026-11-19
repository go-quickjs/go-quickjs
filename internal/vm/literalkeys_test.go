package vm

import (
	"fmt"
	"runtime"
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

// Register code's literals made at once (new_object_lit) take the realm's
// root for their keys: one layout for the same keys in
// any function, a closure made anew or two of one function's sites, and
// one of their own for other keys.
func TestRegLiteralKeysShareALayout(t *testing.T) {
	for _, native := range []bool{false, true} {
		was := SetRegNative(native)
		r := New(Config{})
		want := shapeOfNew(t, r, `(function () { return {a: 1, b: 2} })()`)
		if want == nil || want.unique || want.loose || want.n != 2 {
			t.Fatalf("native %v: no shared layout: %+v", native, want)
		}
		for _, src := range []string{
			`(function (x) { return {a: x, b: "y"} })(3)`,
			`(function () { var f = function (i) { return {"a": i, 'b': i} }; f(1); return f(2) })()`,
			`(function () { let o; for (let i = 0; i < 50; i++) o = i & 1 ? {a: i, b: i} : {a: 0, b: 0}; return o })()`,
			`(function () { return {a: [1, 2], b: {c: 1}} })()`,
		} {
			if got := shapeOfNew(t, r, src); got != want {
				t.Errorf("native %v: %s: a layout of its own", native, src)
			}
		}
		for _, src := range []string{
			`(function () { return {b: 1, a: 2} })()`,
			`(function () { return {a: 1, b: 2, c: 3} })()`,
		} {
			if got := shapeOfNew(t, r, src); got == want {
				t.Errorf("native %v: %s: the layout of {a, b}", native, src)
			}
		}
		r.Close()
		SetRegNative(was)
	}
}

// Each realm has its own, of its own Object.prototype.
func TestLiteralKeysByRealm(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	v, err = r.Run(compileForTest(t, `
		function literalPair(first, second) { return {first: first, second: second} }
		var pair;
		for (var i = 0; i < 50; i++) pair = literalPair({tag: "first"}, {tag: "second"});
		pair`))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	pair := v.Object()
	if pair.valn != 2 || pair.valc != 2 {
		t.Fatalf("pair has %d values in room for %d", pair.valn, pair.valc)
	}
	for i, want := range []string{"first", "second"} {
		if got := pair.valAt(i); !got.IsObject() || got.Object().valAt(0).String().Go() != want {
			t.Fatalf("pair value %d did not retain its object", i)
		}
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
	t.Parallel()
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

// The register compiler says the keys of a literal that has nothing else
// (RegCode.ObjKeysHash): alike for the same keys, in order, however
// written, and for no others.
func TestLiteralKeysTable(t *testing.T) {
	t.Parallel()
	keys := func(src string) uint64 {
		t.Helper()
		f := compileForTest(t, src)
		rc := f.RegCode()
		// The last literal made, the outermost.
		for pc := len(rc.Code) - 1; pc >= 0; pc-- {
			if w := rc.Code[pc]; w.Op() == bytecode.RNewObject || w.Op() == bytecode.RNewObjectLit {
				return keysOf[struct{}](rc, pc)
			}
		}
		t.Fatalf("%s: no literal", src)
		return 0
	}
	says := []string{`({x: 1, y: {z: 2}})`, `({"n-1": 2, n: 4})`, `({"x": 3, y: 4})`, `({y: 1, x: 2})`,
		`({ab: 1, c: 2})`, `({a: 1, bc: 2})`, `({z: 2})`}
	// A literal with a method is made a property at a time, the method
	// given its home object: new_object_lit is of values alone.
	none := []string{`var a = {}; ({...a})`, `({})`, `({q: 1, q: 2})`, `var k = "k"; ({[k]: 1})`,
		`({get g() { return 1 }})`, `({__proto__: null})`, `var a = {}; ({r: 1, ...a})`, `({m() {}, n: 2})`}
	got := map[string]uint64{}
	for _, src := range says {
		if got[src] = keys(src); got[src] == 0 {
			t.Errorf("%s says no keys", src)
		}
	}
	for _, src := range none {
		if keys(src) != 0 {
			t.Errorf("%s says its keys", src)
		}
	}
	if got[says[0]] != got[says[2]] {
		t.Error("{x, y} and {\"x\", y} are told apart")
	}
	if got[says[0]] == got[says[3]] || got[says[4]] == got[says[5]] || got[says[0]] == got[says[6]] {
		t.Errorf("different keys told alike: %x", got)
	}
}

// The same keys are told alike from one function to another, and from one
// compile to another, runtimes sharing what is compiled: nothing seeds it.
func TestLiteralKeysAcrossCompiles(t *testing.T) {
	t.Parallel()
	a := compileForTest(t, `({one: 1, two: 2})`).RegCode()
	b := compileForTest(t, `function f() { return [{"one": {}, "two": 1}] }`).Constants[0].Fn.RegCode()
	if a.ObjKeysHash[0] != b.ObjKeysHash[0] {
		t.Errorf("hashes %x and %x", a.ObjKeysHash[0], b.ObjKeysHash[0])
	}
}

func testLiteralKeysCollision(t *testing.T, r *Runtime) {
	const collide = 0x1234
	// Register code's literals' keys' hashes (RegCode's ObjKeysHash),
	// which a runtime running it starts them from (regLiteralKeyedRoot),
	// made to collide.
	regKeys := func(fn *bytecode.Function) {

		if rc := fn.RegCode(); rc != nil {
			for i, h := range rc.ObjKeysHash {
				if h != 0 {
					rc.ObjKeysHash[i] = collide
				}
			}
		}
	}
	force := func(fn *bytecode.Function) *bytecode.Function {
		regKeys(fn)
		for i := range fn.Constants {
			if c := &fn.Constants[i]; c.Fn != nil {
				regKeys(c.Fn)
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
