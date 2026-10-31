package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A read of undefined is undefined wherever no binding of that name, no
// with object and no direct eval's var can stand in front of the global
// object's, which is neither writable nor configurable (the compiler then
// pushes it); and the binding's, the object's or the eval's value where one
// does. Against Node's answer.
func TestUndefinedRead(t *testing.T) {
	src := `
		var out = [];
		function a(x) { return x === undefined; }
		function b(undefined) { return undefined; }
		function c() { var undefined = 3; return undefined; }
		function d() { eval("var undefined = 4"); return undefined; }
		function e() { with ({ undefined: 5 }) { return undefined; } }
		function f() { with ({}) { return undefined; } }
		function g() { return eval("undefined"); }
		function h() { "use strict"; return eval("var undefined = 6; undefined"); }
		function k() { eval("var undefined = 7"); return (function () { return undefined; })(); }
		function m() { return typeof undefined; }
		var n = (function () { try { undefined = 9; return undefined; } catch (e) { return e.name; } })();
		var p = (function () { "use strict"; try { undefined = 9; return "no"; } catch (e) { return e.name; } })();
		out.push(a(), a(1), b(2), c(), d(), e(), f(), g(), h(), k(), m(), n, p, undefined, void 0 === undefined);
		var q; try { eval("let undefined = 1"); q = "ok"; } catch (e) { q = e.name; }
		out.push(q);
		out.join()`
	const want = "true,false,2,3,4,5,,,6,7,undefined,,TypeError,,true,ok"
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
