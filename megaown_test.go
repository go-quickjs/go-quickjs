package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A read at a site that has given up remembers where its key is by shape
// in a table of its own (megaOwn): forty layouts read again and again, a
// key deleted and added back, one made a getter, an object given another
// key, one frozen; an array's, a function's, an inherited, a missing and a
// string's at the same site, and a dictionary's. Both tiers against Node's
// answer.
func TestMegamorphicOwnTable(t *testing.T) {
	src := `
		var out = [];
		function read(o) { return o.k; }
		var objs = [];
		for (var s = 0; s < 40; s++) { var o = {}; for (var j = 0; j < s % 13; j++) o["p" + s + "_" + j] = j; o.k = s; objs.push(o); }
		for (var r = 0; r < 4; r++) { var t = 0; for (var i = 0; i < objs.length; i++) t += read(objs[i]); out.push(t); }
		delete objs[3].k; objs[3].k = "re"; out.push(read(objs[3]));
		Object.defineProperty(objs[5], "k", { get: function () { return "getter"; } }); out.push(read(objs[5]));
		objs[7].extra = 1; out.push(read(objs[7]));
		Object.freeze(objs[9]); out.push(read(objs[9]));
		var arr = [1]; arr.k = "arr"; var fn = function () {}; fn.k = "fn";
		out.push(read(arr), read(fn), read(Object.create(objs[11])), read({}), read("str"));
		var dict = {}; for (var q = 0; q < 100; q++) dict["d" + q] = q; dict.k = "dict"; delete dict.d5; out.push(read(dict));
		for (var r = 0; r < 2; r++) { var t = 0; for (var i = 0; i < objs.length; i++) { var v = read(objs[i]); t += typeof v === "number" ? v : 0; } out.push(t); }
		out.join()`
	const want = "780,780,780,780,re,getter,7,9,arr,fn,11,,,dict,772,772"
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(src)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
