package vm

import "testing"

// TestApplyArgumentsCallsDirectly pins f.apply(this, arguments) -- and a
// constructor that is only this.initialize.apply(this, arguments), which
// leafForward answers -- calling the target as a call from the loop does:
// the result, this, the arguments, a primitive this boxed for a sloppy
// target, a class constructor refused, an exception's stack, and, in the
// tree tier, the target given its tree's call (planTreeCall), which r.call
// never gave it.
func TestApplyArgumentsCallsDirectly(t *testing.T) {
	const src = `
		var Class = { create: function () { return function () { this.initialize.apply(this, arguments) } } };
		var Point = Class.create();
		Point.prototype.initialize = function (x, y) { this.x = x; this.y = y; this.n = arguments.length };
		function sum() { var s = 0; for (var i = 0; i < arguments.length; i++) s += arguments[i]; return s }
		function fwd() { return sum.apply(this, arguments) }
		function kind() { return typeof this }
		function fwdKind() { return kind.apply(5, arguments) }
		class K { constructor() {} }
		function fwdClass() { return K.apply(null, arguments) }
		function thrower() { throw new Error("boom") }
		function fwdThrow() { return thrower.apply(this, arguments) }
		var out = [], p;
		for (var i = 0; i < 300; i++) { p = new Point(i, i + 1); out[0] = p.x + p.y + p.n; out[1] = fwd(i, 1, 2) }
		out.push(fwdKind(), p instanceof Point);
		try { fwdClass() } catch (e) { out.push(e instanceof TypeError) }
		try { fwdThrow() } catch (e) { out.push(/thrower/.test(e.stack), /fwdThrow/.test(e.stack)) }
		[out.join(), Point.prototype.initialize]`
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatalf("tree=%v: %v", tree, err)
		}
		res := v.Object().elemsOf()
		if got, want := res[0].String().Go(), "601,302,object,true,true,true,true"; got != want {
			t.Errorf("tree=%v: got %s, want %s", tree, got, want)
		}
		if fd := res[1].Object().fn(); tree && fd.treeCall == nil {
			t.Errorf("initialize, called by apply, was not given its tree's call")
		}
		r.Close()
	}
	SetTreeTier(true)
}
