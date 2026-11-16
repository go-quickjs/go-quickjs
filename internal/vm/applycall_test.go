package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

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

// TestForwarderMethodsByShape pins what leafForward answers once its
// method's read is cached by the receiver's shape in the forwarder's own
// table (fwdSite) -- twenty classes made by one Class.create, each built
// many times -- and the changes that table must see: the method replaced
// on the prototype, an own method, Function.prototype.apply patched and
// restored, a method with an apply of its own, a bound method, a proxy as
// the prototype, the prototype replaced, and a throw's stack through the
// forwarder. The answers are Node's; the stack interpreter, the tree tier
// and register code each give them.
func TestForwarderMethodsByShape(t *testing.T) {
	const src = `
		var Class = { create: function() { return function() { this.initialize.apply(this, arguments); }; } };
		function mk(init) { var C = Class.create(); C.prototype = { initialize: init }; return C; }
		var r = [], cs = [], s = 0;
		for (var k = 0; k < 20; k++) cs.push(mk((function (k) { return function (a, b) { this.v = a + b + k } })(k)));
		for (var i = 0; i < 200; i++) for (var k = 0; k < 20; k++) s += new cs[k](i, 1).v;
		r.push(s);
		var A = mk(function (x) { this.x = x });
		r.push(new A(1).x);
		A.prototype.initialize = function (x) { this.x = x * 10 };
		r.push(new A(2).x);
		var B = mk(function (x) { this.y = "proto" + x });
		B.prototype = Object.create(B.prototype);
		r.push(new B(3).y);
		var ap = Function.prototype.apply;
		Function.prototype.apply = function (t, a) { t.patched = a.length; return ap.call(this, t, a) };
		r.push(new A(4).patched + ":" + new A(4).x);
		Function.prototype.apply = ap;
		r.push(new A(5).patched + ":" + new A(5).x);
		var own = function (x) { this.z = x };
		own.apply = function (t, a) { t.z = "own" + a[0] };
		r.push(new (mk(own))(6).z);
		var E = mk(function (x) { this.w = this === undefined ? "u" : typeof this.w + x }.bind({ w: 1 }));
		r.push(typeof new E(7).w);
		var F = Class.create();
		F.prototype = new Proxy({ initialize: function (x) { this.p = x } }, {});
		r.push(new F(8).p);
		var G = mk(function (x) { this.g = "one" + x });
		r.push(new G(1).g);
		G.prototype = { initialize: function (x) { this.g = "two" + x } };
		r.push(new G(2).g);
		var H = mk(function (x) { if (x) throw new Error("boom"); this.h = 1 });
		try { new H(1) } catch (e) { r.push(e.stack.split("\n").slice(0, 3).map(function (l) { return l.trim().replace(/ \(.*$/, "") }).join(" | ")) }
		r.join("; ")`
	const want = "440000; 1; 20; proto3; 1:40; undefined:50; own6; undefined; 8; one1; two2; Error: boom | at Object.initialize | at new <anonymous>"
	for _, mode := range []string{"stack", "tree", "reg"} {
		compiler.SetRegisterForm(mode == "reg")
		fn := compileForTest(t, src)
		compiler.SetRegisterForm(false)
		SetTreeTier(mode != "stack")
		SetRegTier(mode == "reg")
		r := New(Config{})
		v, err := r.Run(fn)
		SetRegTier(false)
		SetTreeTier(true)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("%s:\n got %s\nwant %s", mode, got, want)
		}
		r.Close()
	}
}
