package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// noKeysHash is the hash a literal of no keys has.
func TestNoKeysHash(t *testing.T) {
	if noKeysHash != bytecode.HashLiteralKeys([]string{}) || noKeysHash == 0 {
		t.Fatalf("noKeysHash %#x", noKeysHash)
	}
}

// namespaceSrc is code new for each k, as Octane's CodeLoad evaluates a
// library again with its names changed: a namespace made of {} and kept by a
// global of that name, given the library's keys after it is made.
func namespaceSrc(k, keys int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "var g%d = g%d || {};\n", k, k)
	for i := 0; i < keys; i++ {
		fmt.Fprintf(&b, "g%d.p%d = %d;\n", k, i, i)
	}
	fmt.Fprintf(&b, "g%d.p0", k)
	return b.String()
}

// The namespaces of code evaluated anew, each {} given the same keys and
// kept, share one layout, as V8's empty literals share the Object function's
// initial map and the maps its transitions lead to: the tree stays as it was
// after the first, rather than growing a chain for each site's objects until
// it is full and each object made after it has a layout of its own, as
// Octane's CodeLoad left it for Box2D and Typescript.
func TestEmptyLiteralsShareAcrossCode(t *testing.T) {
	t.Parallel()
	const runs, keys = 1500, 51
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			r := newTestRuntime(t, nativeConfig(native))
			defer r.Close()
			if _, err := r.Run(compileForTest(t, namespaceSrc(0, keys))); err != nil {
				t.Fatal(err)
			}
			settleSites[struct{}](r)
			count := r.shapes.count
			first := globalObject(r, "g0").shape
			if first == nil || first.unique || first.loose || first.n != keys {
				t.Fatalf("the first namespace's layout is no shared one: %+v", first)
			}
			for k := 1; k < runs; k++ {
				if _, err := r.Run(compileForTest(t, namespaceSrc(k, keys))); err != nil {
					t.Fatal(err)
				}
			}
			settleSites[struct{}](r)
			if r.shapes.count != count {
				t.Errorf("after %d namespaces the tree counts %d shapes, %d after the first", runs, r.shapes.count, count)
			}
			for _, k := range []int{1, runs / 2, runs - 1} {
				if s := globalObject(r, fmt.Sprintf("g%d", k)).shape; s != first {
					t.Errorf("namespace %d has a layout of its own (unique %v, n %d)", k, s.unique, s.n)
				}
			}
			// What is made after them is of shared layouts still.
			v, err := r.Run(compileForTest(t, `function P(x) { this.x = x; this.y = x; } var p1 = new P(1), p2 = new P(2); p1.y + p2.y`))
			if err != nil || v.Number() != 3 {
				t.Fatalf("got %v, %v", v, err)
			}
			if a, b := globalObject(r, "p1").shape, globalObject(r, "p2").shape; a != b || a.unique {
				t.Errorf("a constructor's objects after them: layouts %p and %p, unique %v", a, b, a.unique)
			}
		})
	}
}

// KI-66's two literals, one of them {} given its keys after it, run 20,000
// times in one realm, as new code each run and as one program run again,
// make no shape after the first run: the tree counts what it did then,
// without a collection to give back what a run made.
func TestFreshRunsOfTwoLiteralsStayBounded(t *testing.T) {
	t.Parallel()
	holdCollector(t)
	const runs = 20000
	src := func(i int) string {
		return fmt.Sprintf(`var o = {}; o.a = %d; o.b = 2; var p = {c: 3, d: 4}; o.a + p.d`, i)
	}
	t.Run("fresh", func(t *testing.T) {
		r := newTestRuntime(t, Config{})
		defer r.Close()
		if _, err := r.Run(compileForTest(t, src(0))); err != nil {
			t.Fatal(err)
		}
		settleSites[struct{}](r)
		count := r.shapes.count
		for i := 1; i < runs; i++ {
			if _, err := r.Run(compileForTest(t, src(i))); err != nil {
				t.Fatal(err)
			}
			settleSites[struct{}](r)
			if r.shapes.count != count {
				t.Fatalf("run %d: the tree counts %d shapes, %d after the first", i, r.shapes.count, count)
			}
		}
	})
	t.Run("program", func(t *testing.T) {
		r := newTestRuntime(t, Config{})
		defer r.Close()
		fn := compileForTest(t, src(7))
		runProgramForTest(t, r, fn)
		settleSites[struct{}](r)
		count := r.shapes.count
		for i := 1; i < runs; i++ {
			runProgramForTest(t, r, fn)
			settleSites[struct{}](r)
			if r.shapes.count != count {
				t.Fatalf("run %d: the tree counts %d shapes, %d after the first", i, r.shapes.count, count)
			}
		}
	})
}

// Objects built alike at two sites of different code share each layout:
// {} given the same keys after it, as V8's empty literals do from one map,
// and literals of the same keys, as V8's from the map of their count. A
// read meeting both stays monomorphic: one layout, one cache entry.
func TestLiteralLayoutsShareAcrossSites(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		r := newTestRuntime(t, nativeConfig(native))
		for _, pair := range [][2]string{
			{`(function () { var o = {}; o.a = 1; o.b = 2; return o })()`,
				`(function (x) { var q = {}; q.a = x; q.b = "y"; return q })(3)`},
			{`(function () { return {a: 1, b: 2} })()`,
				`(function (x) { return {a: x, b: [x]} })(3)`},
		} {
			a, b := shapeOfNew(t, r, pair[0]), shapeOfNew(t, r, pair[1])
			if a != b || a == nil || a.unique || a.loose || a.n != 2 {
				t.Errorf("native %v: %s and %s: layouts %p and %p", native, pair[0], pair[1], a, b)
			}
		}
		// {} and {a, b} given no further keys are not the same layout: the
		// {} ones were given theirs one at a time, from another root.
		if a, b := shapeOfNew(t, r, `(function () { var o = {}; o.a = 1; o.b = 2; return o })()`),
			shapeOfNew(t, r, `({a: 1, b: 2})`); a == b {
			t.Errorf("native %v: {} given a and b has the layout of {a, b}", native)
		}
		r.Close()
	}
}

// The room of {}'s objects is fixed, as V8's Object function's initial map
// fixes its in-object properties: a site whose objects grow to a table of
// twenty keys leaves every {} made with the room of four, its own next ones
// too, where a root learning it had sized every {} of the realm by them.
func TestEmptyLiteralRoomIsFixed(t *testing.T) {
	t.Parallel()
	for _, native := range []bool{false, true} {
		r := newTestRuntime(t, nativeConfig(native))
		var grow strings.Builder
		for i := 0; i < 20; i++ {
			fmt.Fprintf(&grow, "o.k%d = %d; ", i, i)
		}
		if _, err := r.Run(compileForTest(t, `function big() { var o = {}; `+grow.String()+`return o }
			for (var j = 0; j < 50; j++) big(); var a = {}, b = big()`)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a"} {
			o := globalObject(r, name)
			if o.valc != emptyLiteralRoom || o.shape.slack != emptyLiteralRoom || o.shape.rootClass != emptyRootClass {
				t.Errorf("native %v: %s: room %d, root room %d, root class %d", native, name, o.valc, o.shape.slack, o.shape.rootClass)
			}
		}
		if o := globalObject(r, "b"); o.shape.unique || o.shape.n != 20 || o.shape.root().slack != emptyLiteralRoom {
			t.Errorf("native %v: the grown {}'s root learned room %d", native, o.shape.root().slack)
		}
		r.Close()
	}
}

// A literal that says no list of keys -- a spread, a computed key, an
// accessor, __proto__ -- is no {}: its objects start from a root of their
// site's own, as before, and neither from the realm's root for {} nor of
// its fixed room.
func TestKeylessLiteralsKeepTheirRoots(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	for _, src := range []string{
		`(function (x) { return {...x} })({a: 1})`,
		`(function (k) { return {[k]: 1, a: 2} })("z")`,
		`(function () { return {get a() { return 1 }} })()`,
		`(function (x) { return {a: 1, ...x} })({b: 2})`,
	} {
		if s := shapeOfNew(t, r, src); s != nil && s.root().rootClass == emptyRootClass {
			t.Errorf("%s: of the realm's root for {}", src)
		}
	}
	if s := shapeOfNew(t, r, `(function () { var o = {}; o.a = 1; return o })()`); s.root().rootClass != emptyRootClass {
		t.Errorf("{}: not of the realm's root for {}")
	}
}
