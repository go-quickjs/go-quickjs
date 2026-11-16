package vm

import "testing"

// A pure body's closure that register code makes -- a function
// declaration, a callback -- enters its register code at once (regPlan),
// as any other does: a built-in calling it runs that code, which the JIT
// learns from, rather than the stack form's evaluator (pureCall) at every
// call. The removal of the tree tier had left pure bodies to pureCall, and
// their callbacks' loops were never compiled natively.
func TestRegPureClosuresPlanned(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	src := `function f(x) { return x * 2 }
	function g(x) { s += x }
	function h(x) { return x.length }
	var s = 0, out = []
	for (var i = 0; i < 3; i++) { out.push([1, 2, 3].map(f).join()); [1, 2].forEach(g); out.push(["ab"].map(h)[0]) }
	out.join(";") + ";" + s`
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "2,4,6;2;2,4,6;2;2,4,6;2;9"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, n := range []string{"f", "g", "h"} {
		fd := r.global.getOwn(r.atoms.intern(n)).value().Object().fn()
		if fd.closure.reg == nil || fd.regCall == nil {
			t.Errorf("%s: register code bound %v, entered at once %v", n, fd.closure.reg != nil, fd.regCall != nil)
		}
	}
}
