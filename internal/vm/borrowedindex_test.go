package vm

import "testing"

// TestDeleteBorrowsIndex pins what an object of a shared layout large enough
// to have an index sees once it deletes a key: it leaves the layout and
// borrows its index rather than copying it, and the index, which still has
// the key, must not find it -- the empty string's included, a deleted
// slot's key being the empty atom -- nor lead the layout's other objects
// astray. Adding a key gives the object an index of its own.
func TestDeleteBorrowsIndex(t *testing.T) {
	const src = `
	function make() {
		var o = {};
		var keys = ["a", "b", "c", "d", "e", "f", "g", "h", "i", "j", ""];
		for (var n = 0; n < keys.length; n++) o[keys[n]] = keys[n] + "!";
		return o;
	}
	var out = [];
	var x = make(), y = make();
	out.push(x.a, y.a);
	delete x.c;
	delete x[""];
	out.push("c" in x, x.c, "" in x, x[""], "c" in y, y.c, y[""], Object.keys(x).join(), Object.keys(y).join());
	x.c = 1;
	out.push(x.c, Object.keys(x).join(), "c" in x, x.hasOwnProperty("d"), x.d, "" in x);
	delete x.c;
	out.push("c" in x, x.c);
	var z = make();
	out.push(z.c, z[""], "c" in z);
	var w = make();
	delete w.b;
	Object.defineProperty(w, "d", {enumerable: false});
	out.push(Object.keys(w).join(), w.d, w.b, "b" in w);
	var v = make();
	delete v.e;
	var cnt = 0;
	for (var i = 0; i < 50; i++) {
		if ("e" in v) cnt++;
		if (v.f === "f!") cnt += 2;
		if (v.e === undefined) cnt += 4;
	}
	out.push(cnt);
	var u = make();
	out.push(u.e, "e" in u, u.f);
	var spread = {...make()};
	delete spread.a;
	out.push(Object.keys(spread).join(), spread.a, spread.b);
	JSON.stringify(out);
	`
	const want = `["a!","a!",false,null,false,null,true,"c!","!","a,b,d,e,f,g,h,i,j","a,b,c,d,e,f,g,h,i,j,",` +
		`1,"a,b,d,e,f,g,h,i,j,c",true,true,"d!",false,false,null,"c!","!",true,"a,c,e,f,g,h,i,j,",` +
		`"d!",null,false,300,"e!",true,"f!","b,c,d,e,f,g,h,i,j,",null,"b!"]`
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v:\n got %s\nwant %s", tree, got, want)
		}
		r.Close()
	}
}

// TestDeleteBorrowedIndexShape pins the layouts: the object that deleted a
// key has the index of the layout it left, which stays as it was, and an
// object that then adds a key has one of its own without the deleted key.
func TestDeleteBorrowedIndexShape(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
	function make() { var o = {}; for (var n = 0; n < 12; n++) o["k" + n] = n; return o }
	var a = make(), b = make(), c = make();
	delete a.k3;
	delete b.k3;
	b.k20 = 20;
	[a, b, c]`))
	if err != nil {
		t.Fatal(err)
	}
	get := func(i int) *Object {
		e, err := r.getIndexed(v, Int(i))
		if err != nil {
			t.Fatal(err)
		}
		return e.Object()
	}
	a, b, c := get(0), get(1), get(2)
	if c.shape.unique || c.shape.index == nil {
		t.Fatalf("an object of the shared layout has shape unique %v, index %v", c.shape.unique, c.shape.index != nil)
	}
	if !a.shape.unique || !a.shape.borrowed || a.shape.index != c.shape.index {
		t.Errorf("deleting gave unique %v, borrowed %v, layout's index %v", a.shape.unique, a.shape.borrowed, a.shape.index == c.shape.index)
	}
	if b.shape.borrowed || b.shape.index == c.shape.index {
		t.Errorf("adding after deleting kept the borrowed index")
	}
	if _, ok := b.shape.index.slots[r.atoms.intern("k3")]; ok {
		t.Errorf("an index of its own has the deleted key")
	}
	if _, ok := c.shape.index.slots[r.atoms.intern("k3")]; !ok {
		t.Errorf("the layout's index lost a key an object of it deleted")
	}
}
