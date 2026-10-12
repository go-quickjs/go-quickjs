package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestGeneratorEvalVars pins where a direct eval in a generator or an
// async function puts its vars, now that a generator keeps them apart from
// what every generator has: in the function's own scope, across
// suspensions, seen by a function the body makes, and not on the global
// object.
func TestGeneratorEvalVars(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	for _, c := range []struct{ src, want string }{
		{`function* f(a) { eval('var v = a + 1'); yield v; yield typeof v } [...f(1)].join() + typeof v`, "2,numberundefined"},
		{`function* f() { eval('var v = 7'); yield* (function* () { yield v })() } [...f()].join()`, "7"},
		{`var out = 'pending'; async function f() { eval('var w = 2'); await 0; return w * 3 } f().then(v => out = v); 'started'`, "started"},
		{`out + ',' + typeof w`, "6,undefined"},
	} {
		v, err := rt.Eval(c.src)
		if err != nil || v.String() != c.want {
			t.Errorf("%s: got %v, %v; want %s", c.src, v, err, c.want)
		}
	}
}
