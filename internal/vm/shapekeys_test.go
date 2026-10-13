package vm

import "testing"

// A shape's keys are shared: by every object of a shared layout, by a
// layout and the first one grown from it (whose keys extend its array in
// place), and by the arguments objects made from the first one's template.
// What one object does to its own layout -- a key deleted, an attribute
// changed, a key added past a sibling's -- must reach no other object, and
// each must go on reading its keys where its values are.
func TestShapeKeysShared(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function make(i) { return {a: i, b: i + 1, c: i + 2} }
		var x = make(1), y = make(10), z = make(100)
		delete x.b
		Object.defineProperty(y, "a", {writable: false})
		y.a = 99
		var s1 = {p: 1}, s2 = {p: 2}
		s1.q = 3; s2.r = 4; s1.t = 5
		function args() { return arguments }
		var a1 = args(1, 2), a2 = args(3), a3 = args(4, 5, 6)
		delete a1.length
		a2.extra = 7
		Object.defineProperty(a3, "length", {enumerable: true})
		;[
			JSON.stringify([x, y, z]),
			Object.getOwnPropertyDescriptor(y, "a").writable + "," + Object.getOwnPropertyDescriptor(z, "a").writable,
			Object.keys(s1) + "|" + Object.keys(s2),
			[Object.keys(a1), Object.keys(a2), Object.keys(a3)].join("|"),
			[a1.length, a2.length, a3.length, a2.extra].join(),
		].join(" ")`))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"a":1,"c":3},{"a":10,"b":11,"c":12},{"a":100,"b":101,"c":102}] false,true p,q,t|p,r 0,1|0,extra|0,1,2,length ,1,3,7`
	if got := v.String().Go(); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	for _, name := range []string{"x", "y", "z", "s1", "s2", "a1", "a2", "a3"} {
		checkShape(r.global.getOwn(r.atoms.intern(name)).value().Object())
	}
}
