package vm

import "testing"

// A site whose objects are of a few shapes in turn answers each from an
// entry of its own, as V8's polymorphic inline caches hold four maps: it
// fills once a shape, not at each change of shape, which had counted
// against its fills until it gave its cache up (maxCacheFills).
func TestPolymorphicSites(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function A() { this.x = 1 }
		function B() { this.y = 0; this.x = 2 }
		function C() { this.z = 0; this.w = 0; this.x = 3 }
		function D() {}
		D.prototype.x = 4
		function E() {}
		Object.defineProperty(E.prototype, "x", { get() { return 5 } })
		var stored = 0
		function F() {}
		Object.defineProperty(F.prototype, "v", { set(v) { stored += v } })
		function read(o) { return o.x }
		function write(o, v) { o.v = v }
		var objs = [new A(), new B(), new C(), new D(), new E()], made = [A, B, C, F]
		var sum = 0, added = 0
		for (var i = 0; i < 200; i++) {
			sum += read(objs[i % 4])
			var o = new made[i % 4]()
			write(o, 1)
			if (o.hasOwnProperty("v")) added += o.v
		}
		var before = sum
		for (var i = 0; i < 4; i++) sum += read(objs[i])
		D.prototype.x = 40
		var after = read(objs[3])
		read(objs[4]) + "," + before + "," + sum + "," + after + "," + stored + "," + added`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "5,500,510,40,50,150"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// A fill for each of the read's four shapes, for D's again once its
	// prototype changed, and for E's getter; for each of the write's.
	for fn, most := range map[string]uint8{"read": 6, "write": 4} {
		cl := globalObject(r, fn).fn().closure
		for i := range cl.ic {
			if c := &cl.ic[i]; c.shape != noShape && c.fills > most {
				t.Errorf("%s: a site of four shapes filled %d times, not %d", fn, c.fills, most)
			}
		}
	}
}
