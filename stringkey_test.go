package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// o[k] of a string key is read by the walk of ordinary objects
// (stringKeyRead) where the key's atom is known and no index, and as it
// was otherwise: an own and an inherited data property, a getter, a key no
// object has, an index-like key, Object.prototype's method, a deleted key,
// __proto__'s accessor, an array's, a string's, a proxy's, a function's
// and a String object's, a key made at run time, and an inherited property
// made a getter after the reads. Against Node's answer.
func TestStringKeyRead(t *testing.T) {
	src := `
		var out = [];
		function read(o, k) { return o[k]; }
		var proto = { inherited: "p", get viaGetter() { return "g"; } };
		var o = Object.create(proto); o.own = 1; o["7"] = "seven"; o.gone = 2; delete o.gone;
		var keys = ["own", "inherited", "viaGetter", "missing", "7", "toString", "gone", "__proto__", "length", "0"];
		for (var r = 0; r < 3; r++) {
		  for (var i = 0; i < keys.length; i++) {
		    var v = read(o, keys[i]);
		    out.push(typeof v === "function" ? "fn" : v === proto ? "proto" : String(v));
		  }
		  out.push(read([5, 6], "0"), read([5, 6], "length"), read("str", "length"), read(new Proxy({}, { get: function (t, k) { return "px:" + k; } }), "own"));
		  out.push(read(function f(a, b) {}, "length"), read(new String("ab"), "1"));
		  var fresh = "k" + r; o[fresh] = r; out.push(read(o, fresh), read(o, "nope" + r));
		}
		Object.defineProperty(proto, "inherited", { get: function () { return "now a getter"; } });
		out.push(read(o, "inherited"));
		out.join()`
	const want = "1,p,g,undefined,seven,fn,undefined,proto,undefined,undefined,5,2,3,px:own,2,b,0,,1,p,g,undefined,seven,fn,undefined,proto,undefined,undefined,5,2,3,px:own,2,b,1,,1,p,g,undefined,seven,fn,undefined,proto,undefined,undefined,5,2,3,px:own,2,b,2,,now a getter"
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
