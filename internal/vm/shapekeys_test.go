package vm

import (
	"fmt"
	"strings"
	"testing"
)

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

// Objects a constructor builds alike share a layout past 64 properties, as
// V8's share a map up to its 1,020 descriptors: Octane's Gameboy emulator,
// of 215, had had a shape of its own at each construction, which every
// cache met anew. The layouts along the chain share one index, as V8's
// maps share a descriptor array, each the prefix of its own count: an
// object stopped part way finds none of the keys past its own, and a
// layout branching off the chain -- its keys a copy -- an index of its
// own, which leaves the chain's as it was.
func TestShapeChainsPastSixtyFour(t *testing.T) {
	var src strings.Builder
	src.WriteString("function C(stop, alt) {\n")
	for i := 0; i < 215; i++ {
		if i == 50 {
			src.WriteString("if (alt) this.q = -1;\n")
		}
		fmt.Fprintf(&src, "if (stop === %d) return; this.p%d = %d;\n", i, i, i)
	}
	src.WriteString(`}
		var a = new C(-1, false), b = new C(-1, false), y = new C(100, false), z = new C(-1, true)
		;[y.p99, y.p100, y.p150, "p150" in y, Object.keys(y).length,
			z.q, z.p50, z.p214, Object.keys(z)[50], a.q,
			a["p" + 50], a["p" + 214], Object.keys(a).length].join()`)
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "99,,,false,100,-1,50,214,q,,50,214,215"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	obj := func(name string) *Object { return r.global.getOwn(r.atoms.intern(name)).value().Object() }
	a, b, y, z := obj("a"), obj("b"), obj("y"), obj("z")
	if a.shape != b.shape || a.shape.unique || a.shape.n != 215 {
		t.Fatalf("a and b do not share a layout of 215: %p %p unique %v n %d", a.shape, b.shape, a.shape.unique, a.shape.n)
	}
	indexes := func(s *shape) int {
		seen := map[*propIndex]bool{}
		for ; s != nil; s = s.parent {
			if s.index != nil {
				seen[s.index] = true
			}
		}
		return len(seen)
	}
	onChain := false
	for s := a.shape; s != nil; s = s.parent {
		onChain = onChain || s == y.shape
	}
	if !onChain || y.shape.unique {
		t.Fatal("an object stopped part way is not on its chain")
	}
	if n := indexes(a.shape); n != 1 {
		t.Fatalf("a's chain has %d indexes, not one", n)
	}
	if n := indexes(z.shape); z.shape.unique || n != 2 {
		t.Fatalf("the branch has %d indexes along its chain, unique %v: not its own after the chain's", n, z.shape.unique)
	}
}
