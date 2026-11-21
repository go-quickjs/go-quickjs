package vm

import "testing"

// siteEntry is the read cache entry of fn's site that remembers a chain by
// its validity cell, or nil.
func siteEntry(r *Runtime, fn string) *propCache {
	cl := globalObject(r, fn).fn().closure
	for i := range cl.ic {
		if cl.ic[i].cell != nil {
			return &cl.ic[i]
		}
	}
	return nil
}

// A read of a property found past two prototypes is remembered by the
// validity cell of the receiver's chain, as V8's are, however long the
// chain: a method four prototypes up, which a cache had not held. A change
// of any object's layout on the chain, or of its prototype, invalidates the
// cell, and the read finds what is there now; a change of the property's
// value does not, the value being read where it is.
func TestProtoCellDeepReads(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	run := func(src string) string {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return v.String().Go()
	}
	run(`
		class A { m() { return "A" } }
		class B extends A {}
		class C extends B {}
		class D extends C {}
		var d = new D()
		function call(o) { return o.m() }
		for (var i = 0; i < 5; i++) call(d)`)
	e := siteEntry(r, "call")
	if e == nil || e.p2 != globalObject(r, "d").proto().proto().proto().proto() || !e.cell.valid {
		t.Fatalf("the method four prototypes up is not remembered by the chain's cell: %+v", e)
	}
	cell := e.cell
	if got := run(`A.prototype.m = function () { return "A2" }; call(d)`); got != "A2" || !cell.valid || e.cell != cell {
		t.Fatalf("a value changed: got %s, cell valid %v", got, cell.valid)
	}
	if got := run(`B.prototype.m = function () { return "B" }; call(d)`); got != "B" || cell.valid {
		t.Fatalf("a method added on a prototype between: got %s, old cell valid %v", got, cell.valid)
	}
	if got := run(`delete B.prototype.m; call(d) + call(d)`); got != "A2A2" {
		t.Fatalf("the method deleted again: got %s", got)
	}
	if got := run(`Object.setPrototypeOf(C.prototype, {m() { return "X" }}); call(d) + call(d)`); got != "XX" {
		t.Fatalf("a prototype's prototype changed: got %s", got)
	}
}

// A read of a property nothing on the chain has is remembered too, as V8's
// loads of a nonexistent property are, and answers undefined until
// something on the chain has it -- Object.prototype included, a receiver
// with no prototype's chain the empty one.
func TestProtoCellAbsentReads(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function get(o) { return o.missing }
		var plain = {a: 1}, bare = Object.create(null)
		bare.a = 1
		var out = []
		for (var i = 0; i < 3; i++) out.push(get(plain))
		Object.prototype.missing = 5
		out.push(get(plain), get(plain))
		delete Object.prototype.missing
		out.push(get(plain))
		function getBare(o) { return o.missing }
		for (var i = 0; i < 3; i++) out.push(getBare(bare))
		out.join()`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), ",,,5,5,,,,"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, fn := range []string{"get", "getBare"} {
		if e := siteEntry(r, fn); e == nil || e.p2 != absentHolder {
			t.Errorf("%s: the absent property is not remembered by its chain's cell: %+v", fn, e)
		}
	}
	if e := siteEntry(r, "getBare"); e != nil && e.cell != nullChain {
		t.Error("a receiver with no prototype is not remembered by the empty chain's cell")
	}
}

// A read of a property some of a site's objects have and others have not
// keeps an entry for each: the one it takes, the other among the site's
// others (propCache.poly), as V8's polymorphic cache keeps both maps --
// neither traded for the other at each read, as a monomorphic cache
// would.
func TestProtoCellAbsentKeepsPresent(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		function L(x) { this.x = x; for (var i = 0; i < 1; i++) {} if (x % 2 === 0) this.s = "s" + x }
		function get(o) { return o.s }
		var out = 0
		for (var i = 0; i < 20; i++) if (get(new L(i)) === undefined) out++`)); err != nil {
		t.Fatal(err)
	}
	cl := globalObject(r, "get").fn().closure
	for i := range cl.ic {
		c := &cl.ic[i]
		if c.shape == noShape {
			continue
		}
		if c.fills > 2 {
			t.Fatalf("the site traded its entries: %d fills", c.fills)
		}
		entries := []*propCache{c}
		if c.poly != nil {
			for j := range int(c.poly.n) {
				entries = append(entries, &c.poly.entries[j])
			}
		}
		absent, present := false, false
		for _, e := range entries {
			if e.cell != nil && e.p2 == absentHolder {
				absent = true
			} else if e.cell == nil {
				present = true
			}
		}
		if !absent || !present {
			t.Fatalf("the site keeps an entry for the absent property %v, for the present one %v", absent, present)
		}
	}
	if v := r.global.getOwn(r.atoms.intern("out")).value(); v.Number() != 10 {
		t.Fatalf("%v reads of undefined, want 10", v)
	}
}
