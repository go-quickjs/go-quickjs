//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// polyWriteSetup's writes stores s into objects of two shapes, which its
// code's exits note as cases (jitPolyWriteSeen); then into dictionaries,
// whose cache is dictShape's, a shape native code is compiled for none of;
// then into objects of no properties, Object.create(null)'s, whose shape is
// none. The FormatJS Intl polyfill's pattern parser writes so into the
// options objects it builds.
const polyWriteSetup = `
	function dict(i) { var d = {a: i, b: i, s: 0}; delete d.a; return d }
	function pick(phase, i) {
		if (phase === 0) return i % 2 ? {x: i, s: 0} : {y: i, s: 0};
		if (phase === 1) return i % 3 ? dict(i) : {x: i, s: 0};
		return i % 3 === 0 ? Object.create(null) : dict(i);
	}
	function writes(n, phase) {
		let t = 0;
		for (let i = 0; i < n; i++) { const o = pick(phase, i); o.s = i; t = (t + o.s) | 0 }
		return t;
	}`

// polyWriteLearn are the rounds that teach writes' store its cases and then
// dictionaries, and polyWriteEmpty those that give it objects of no
// properties.
var polyWriteLearn = []string{`writes(5000, 0)`, `writes(5000, 1)`, `writes(5000, 0)`, `writes(5000, 1)`, `writes(5000, 0)`, `writes(5000, 1)`}

var polyWriteEmpty = []string{`writes(5000, 2)`, `writes(5000, 2)`}

// A write whose cache holds no shape native code is compiled for -- a
// dictionary's -- and which met objects of other shapes is compiled for
// the first of those, the rest its cases (ssa's PropertyWrite). It had been
// compiled for its cache's shape, none, first among the cases: an object of
// no properties, whose shape is none, matched it, and the store wrote
// through the object's table, also none, at address 8 -- the FormatJS Intl
// polyfill's top level faulted so under the JIT every time, and every page
// of a React bundle that loaded it. Each answer is the interpreter's.
func TestJITPolyWriteNoShape(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitPolyWriteNoShape(t)
}

// TestJITPolyWriteNoShapeMarking is TestJITPolyWriteNoShape while the
// collector marks, its stores through Go's write barrier.
func TestJITPolyWriteNoShapeMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	jitMarkingForTest(t)
	jitClobberStoresForTest(t)
	jitPolyWriteNoShape(t)
}

// jitPolyWriteNoShape runs polyWriteLearn, checks that writes' store got
// to what the test is about, then runs polyWriteEmpty, each round's answer
// the interpreter's.
func jitPolyWriteNoShape(t *testing.T) {
	r := jitAgreesForTest(t, polyWriteSetup, polyWriteLearn...)
	jitPolyWriteNoShapeReached(t, r)
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	if _, err := want.Run(compileForTest(t, polyWriteSetup)); err != nil {
		t.Fatal(err)
	}
	for _, src := range polyWriteEmpty {
		wv, werr := want.Run(compileForTest(t, "''+("+src+")"))
		gv, gerr := r.Run(compileForTest(t, "''+("+src+")"))
		if werr != nil || gerr != nil || gv.String().Go() != wv.String().Go() {
			t.Fatalf("%s: %v, %v; the interpreter's %v, %v", src, gv, gerr, wv, werr)
		}
	}
}

// jitPolyWriteNoShapeReached checks that the rounds so far made what the
// test is about: writes ran natively, its store's cache a dictionary's,
// and cases its exits noted, which its code is compiled for.
func jitPolyWriteNoShapeReached(t *testing.T, r *Runtime) {
	t.Helper()
	if st := r.JITStats(); st.SSAEntries == 0 {
		t.Fatalf("nothing ran natively: %+v", st)
	}
	cl := globalObject(r, "writes").fn().closure
	dict := false
	for i := range cl.ic {
		dict = dict || cl.ic[i].shape == dictShape
	}
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn == cl.fn {
			e = x
		}
	}
	cases := 0
	if e != nil {
		for _, p := range e.poly {
			cases += len(p.cases)
		}
	}
	if !dict || cases == 0 {
		t.Fatalf("writes' store: a dictionary's cache %v, %d cases noted; want both", dict, cases)
	}
}
