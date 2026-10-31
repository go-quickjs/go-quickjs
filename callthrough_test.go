package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// f.call(this, ...args) from a tree's call node is f called nested, under
// call's frame (callThroughTree): a base constructor's, an exception thrown
// through it and caught by the caller and its stack, a sloppy and a strict
// body's this of a primitive, null and undefined, a recursion through call
// and one past the stack's limit, a native target, and call replaced on
// Function.prototype. Against Node's answer.
func TestCallThroughTree(t *testing.T) {
	src := `
		var out = [];
		function Base(x) { this.x = x; }
		function Derived(x, y) { Base.call(this, x); this.y = y; }
		function thrower(m) { throw new Error(m + ":" + (this && this.tag)); }
		function sloppyThis() { return typeof this + ":" + (this === globalThis); }
		function strictThis() { "use strict"; return this; }
		function rec(n) { return n == 0 ? 0 : 1 + rec.call(null, n - 1); }
		for (var r = 0; r < 3; r++) {
		  var d = new Derived(1, 2); out.push(d.x + d.y);
		  try { thrower.call({ tag: "t" }, "boom"); } catch (e) { out.push(e.message); }
		  try { (function () { return thrower.call(null, "deep"); })(); } catch (e) { out.push(e.message, /thrower/.test(e.stack)); }
		  out.push(sloppyThis.call(5), sloppyThis.call(null), strictThis.call(5), strictThis.call(undefined));
		  out.push(rec(500), Math.max.call(null, 3, 9), [1, 2].concat.call([0], 7).join("-"));
		}
		try { (function inf() { return inf.call(null); })(); } catch (e) { out.push(e instanceof RangeError); }
		var saved = Function.prototype.call;
		Function.prototype.call = function () { return "patched"; };
		out.push(Base.call({}, 1));
		Function.prototype.call = saved;
		out.join()`
	const want = "3,boom:t,deep:undefined,true,object:false,object:true,5,,500,9,0-7,3,boom:t,deep:undefined,true,object:false,object:true,5,,500,9,0-7,3,boom:t,deep:undefined,true,object:false,object:true,5,,500,9,0-7,true,patched"
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
