package vm

import "testing"

// TestOwnCacheOnlyForPlainOwnData pins what a read or write site's own fast
// path (propCache.own, storeOwn) answers: a plain data property the object
// has itself. An entry for a property found on a prototype, an accessor of
// the object's own or a prototype's, or a write that adds the property is
// of the same receiver shape, and must still be left to the full cache,
// which reads the holder or calls the accessor -- in either tier, and from a
// pure body, which reads through own too.
func TestOwnCacheOnlyForPlainOwnData(t *testing.T) {
	const src = `
		var log = [];
		function P() {}
		P.prototype.x = "proto";
		Object.defineProperty(P.prototype, "y", { get() { return "pget" }, set(v) { log.push("pset " + v) } });
		function readX(o) { return o.x }
		function readY(o) { return o.y }
		function writeY(o, v) { o.y = v }
		function C() { this.z = 1 }
		C.prototype = Object.create(P.prototype);
		C.prototype.getZ = function () { return this.z };
		C.prototype.getX = function () { return this.x };
		function make(i) {
			return { get x() { return "own getter " + i }, set x(v) { log.push("own setter " + v) } };
		}
		var out = [];
		for (var i = 0; i < 40; i++) {
			var p = new P(), q = make(i), c = new C();
			out.push(readX(p), readX(q), readY(p), c.getZ(), c.getX());
			writeY(p, i); q.x = i;
			c.z = i;
		}
		var plain = { x: "own" };
		for (var i = 0; i < 3; i++) out.push(readX(plain));
		[out.slice(0, 10).join(), out.slice(-8).join(), log.length, log.slice(-2).join(), c.getZ()].join("|")`
	const want = "proto,own getter 0,pget,1,proto,proto,own getter 1,pget,1,proto|" +
		"proto,own getter 39,pget,1,proto,own,own,own|80|pset 39,own setter 39|39"
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatalf("tree=%v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree=%v:\n got %s\nwant %s", tree, got, want)
		}
		r.Close()
	}
	SetTreeTier(true)
}
