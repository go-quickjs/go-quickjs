//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeNewSrc constructs with functions and classes of every kind a
// site meets, each enough times for its site's cache to hold it
// (regCallConstruct, past slackTracking objects): bodies returning an
// object, a primitive and nothing, one storing its parameters alone
// (leafCall), one reading new.target and arguments past its parameters,
// one capturing its this and a local in closures, one throwing, one
// constructing itself deeper than the native frame stack goes, one whose
// call in tail position returns what the construction must not, a
// class's and one with fields, a prototype replaced, made a primitive and
// given an accessor the body's store meets, a bound constructor, and a
// stack trace taken in a constructor.
const nativeNewSrc = `
	var out = []
	function P(x, y) { this.x = x; this.y = y; this.z = x + y }
	function RO(x) { this.x = x; return { o: x } }
	function RP(x) { this.x = x; return x }
	function RU(x) { this.x = x; return }
	function L(a, b) { this.a = a; this.b = b }
	function NT(a) { this.t = new.target === NT; this.n = arguments.length; this.l = arguments[arguments.length - 1]; this.a = a }
	function CL(x) { var k = x * 2; this.get = function () { return k + this.x }; this.x = x; this.inc = () => { k++; return this } }
	function TH(x) { if (x % 5 == 3) throw new RangeError("t" + x); this.x = x }
	function DEEP(n) { this.n = n; this.next = n > 0 ? new DEEP(n - 1) : null }
	function TC(x) { "use strict"; this.x = x; return tcv(x) }
	function tcv(x) { return x & 1 ? { odd: x } : x }
	class C { constructor(a) { this.a = a; this.k = a + 1 } m() { return this.a * 10 } }
	class F { f = 7; constructor(a) { this.a = a } }
	function PR(x) { this.x = x }
	function ST(x) { this.s = new Error("e").stack.split("\n").length; this.x = x }
	function depth(o) { var d = 0; while (o) { d++; o = o.next } return d }
	var BP = P.bind(null, 100)
	for (var j = 0; j < 40; j++) {
		var p = new P(j, 2), ro = new RO(j), rp = new RP(j), ru = new RU(j), l = new L(j, "b")
		out.push(p.z, p instanceof P, ro.o, ro instanceof RO, rp.x, ru.x, l.a + l.b)
		var nt = new NT(j, 1, 2, "last"), nt0 = new NT()
		out.push(nt.t, nt.n, nt.l, nt.a, nt0.n, nt0.a, nt0.l)
		var cl = new CL(j); out.push(cl.get(), cl.inc() === cl, cl.get())
		try { out.push(new TH(j).x) } catch (e) { out.push(e.name + e.message) }
		var tc = new TC(j); out.push(tc.x, tc.odd)
		var c = new C(j); out.push(c.k, c.m(), c instanceof C)
		var f = new F(j); out.push(f.f + f.a)
		if (j == 12) PR.prototype = { tag: "second" }
		if (j == 20) PR.prototype = 5
		if (j == 28) { PR.prototype = { set x(v) { this.got = v * 3 } } }
		var pr = new PR(j); out.push(pr.tag, pr.x, pr.got, Object.getPrototypeOf(pr) === Object.prototype)
		out.push(new BP(j).z)
		if (j % 13 == 0) out.push(depth(new DEEP(300)))
		var st = new ST(j); out.push(st.s > 1)
	}
	out.join()`

// TestRegNativeNew checks new_w's native handler against the Go table:
// the same answers, marking or not, and a construction of a function or a
// class its site holds made with no exit once its objects' room is
// settled.
func TestRegNativeNew(t *testing.T) {
	was := nativeWarmExits
	nativeWarmExits = 16
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, nativeNewSrc, `
		function P(x) { this.x = x; this.y = x + 1; if (x < 0) throw x }
		class K { constructor(a) { this.a = a; this.b = [a] } }
		var s = 0
		for (var i = 0; i < 1000; i++) s += new P(i).y + new K(i).b[0]
		String(s)`, bytecode.RNewW)
}

// nativeInstanceOfSrc tests instanceof of functions and classes against
// objects of their chains and of others, primitives, objects made with no
// prototype or given one after (objPolyProto), a proxy in the chain, a
// class with a Symbol.hasInstance of its own, a bound function, an arrow,
// a constructor whose prototype is replaced by a primitive and one that is
// no object, every answer and error at one site.
const nativeInstanceOfSrc = `
	var out = []
	function A() {} function B() {} B.prototype = Object.create(A.prototype)
	class K {} class L extends K {} class M extends L {}
	class H { static [Symbol.hasInstance](v) { return v === 7 } }
	var BA = A.bind(null), arrow = () => 1, P = function () {}
	var poly = Object.setPrototypeOf({}, M.prototype), bare = Object.create(null)
	var px = new Proxy(new L(), {}), pxIn = Object.create(new Proxy(new K(), {}))
	function at(v, c) { try { return v instanceof c } catch (e) { return e.name } }
	var vals = [new A(), new B(), new K(), new L(), new M(), poly, bare, px, pxIn, 7, "s", null, undefined, {}, [], P]
	var ctors = [A, B, K, L, M, Object, Array, H, BA, Function, P]
	for (var r = 0; r < 30; r++) {
		if (r == 10) P.prototype = 3
		if (r == 20) P.prototype = new M()
		for (var i = 0; i < vals.length; i++)
			for (var j = 0; j < ctors.length; j++) out.push(at(vals[i], ctors[j]))
		out.push(at(vals[r % vals.length], arrow), at(1, 2))
	}
	out.join()`

// TestRegNativeInstanceOf checks instanceof's native handler against the
// Go table: the same answers and errors, marking or not, and a site of
// one constructor answered with no exit once its cache holds it.
func TestRegNativeInstanceOf(t *testing.T) {
	nativeCheck(t, nativeInstanceOfSrc, `
		class K {} class L extends K {}
		function F() {}
		var o = new L(), f = new F(), s = 0
		for (var i = 0; i < 1000; i++) if (o instanceof K && !(f instanceof K) && f instanceof F && !(i instanceof F)) s++
		String(s)`, bytecode.RInstanceOf)
}

// nativeDerivedSrc constructs derived classes of every kind their
// constructions and super() calls meet: a super() of arguments and one
// handing on the frame's (a class with no constructor), chains two and
// three deep, a parent returning an object of its own, one throwing, one
// reading new.target, a derived constructor returning an object, a
// primitive, nothing before super(), super() twice, super() in an arrow,
// this captured by a closure before and after, fields of the derived
// class and of the parent, a parent replaced by changing the class's
// prototype, arguments read, and a stack trace taken in the parent.
const nativeDerivedSrc = `
	var out = []
	class A { constructor(x, y) { this.x = x; this.y = y; this.t = new.target.name } sum() { return this.x + (this.y | 0) } }
	class B extends A { constructor(x) { super(x, x * 2); this.b = 1 } }
	class C extends B { constructor(x, z) { super(x); this.c = z } }
	class D extends C {}
	class R { constructor(v) { if (v % 4 == 1) return { own: v }; this.r = v } }
	class RS extends R { constructor(v) { super(v); this.s = 2 } }
	class T { constructor(v) { if (v % 5 == 2) throw new Error("t" + v); this.v = v } }
	class TS extends T { constructor(v) { super(v); this.w = 1 } }
	class Obj extends A { constructor(v) { super(v); return { replaced: v } } }
	class Prim extends A { constructor(v) { super(v); return v } }
	class NoSuper extends A { constructor(v) { if (v > 100) super(v) } }
	class Twice extends A { constructor(v) { super(v); super(v) } }
	class Arrow extends A { constructor(v) { var f = () => super(v, 1); f(); this.k = this.x } }
	class Cap extends A { constructor(v) { var g = () => this; super(v, 3); this.g = g } }
	class F extends A { f = 5; constructor(v) { super(v) } }
	class PF { p = 7; constructor(v) { this.v = v } }
	class GF extends PF { constructor(v) { super(v); this.q = this.p + 1 } }
	class Args extends A { constructor() { super(arguments.length, arguments[1]) } }
	class St extends A { constructor(v) { super(v); this.n = new Error().stack.split("\n").length > 2 } }
	class Swap extends A { constructor(v) { super(v) } }
	function at(f) { try { return f() } catch (e) { return e.name + ":" + e.message } }
	for (var j = 0; j < 40; j++) {
		var b = new B(j), c = new C(j, "z"), d = new D(j, "d")
		out.push(b.sum(), b.b, b.t, c.c, c.t, d.c, d.t, d instanceof A, Object.getPrototypeOf(d) === D.prototype)
		var rs = new RS(j); out.push(rs.own, rs.r, rs.s, rs instanceof RS)
		out.push(at(() => new TS(j).w))
		out.push(at(() => new Obj(j).replaced), at(() => new Prim(j)), at(() => new NoSuper(j)), at(() => new Twice(j).x))
		out.push(at(() => new Arrow(j).k), at(() => { var o = new Cap(j); return o.g() === o && o.y }))
		out.push(new F(j).f, new GF(j).q, new Args(j, "a", 3).y, new Args().x, new St(j).n)
		if (j == 20) Object.setPrototypeOf(Swap, R)
		out.push(at(() => { var o = new Swap(j); return [o.x, o.r, o.own].join("/") }))
	}
	out.join()`

// TestRegNativeDerived checks constructions of derived classes and their
// super() calls, which the native handlers enter with their this
// bindings, against the Go table: the same answers and errors, marking
// or not, and new_w and super_call made with no exit once their caches
// hold them.
func TestRegNativeDerived(t *testing.T) {
	was := nativeWarmExits
	nativeWarmExits = 16
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, nativeDerivedSrc, `
		class A { constructor(x) { this.x = x; this.y = x + 1 } }
		class B extends A { constructor(x) { super(x); this.z = 2 } }
		class C extends B {}
		var s = 0
		for (var i = 0; i < 1000; i++) s += new B(i).z + new C(i).y
		String(s)`, bytecode.RNewW, bytecode.RSuperCall, bytecode.RSuperForward, bytecode.RDerivedRet)
}

// nativeThroughSrc calls functions through Function.prototype.call: a
// method of this given, a strict function given primitives and nothing,
// a sloppy one given a primitive and an object, an arrow, several
// functions at one site in turn, built-ins, a bound function, one that
// reads arguments, a leaf, a class constructor, a function that throws,
// a recursion through call, call called through call, and call replaced
// by a script's function and put back, a stack trace taken in a callee.
const nativeThroughSrc = `
	var out = []
	function m(a, b) { return this.k + a + (b | 0) }
	function strict(a) { "use strict"; return typeof this + ":" + this + ":" + a }
	function sloppy(a) { return (this === globalThis ? "G" : typeof this) + a }
	var arrow = (a) => typeof this + a
	function args() { return arguments.length + ":" + arguments[arguments.length - 1] }
	function leaf(a) { return a }
	function thrower(a) { if (a % 7 == 3) throw new TypeError("t" + a); return a }
	function rec(n) { return n == 0 ? 0 : 1 + rec.call(this, n - 1) }
	function st() { return new Error().stack.split("\n").length }
	class K { constructor() {} }
	var fs = [m, strict, sloppy, arrow, args, leaf, thrower, Math.max, m.bind({ k: 9 }), K, st, [].push, Object.prototype.hasOwnProperty]
	function at(f) { try { return f() } catch (e) { return e.name + ":" + e.message } }
	var realCall = Function.prototype.call
	for (var j = 0; j < 40; j++) {
		var o = { k: j }
		out.push(m.call(o, 1, 2), m.call(o, 3), strict.call(5, j), strict.call(undefined, j), at(() => strict.call()))
		out.push(sloppy.call(4, j), sloppy.call(o, j), sloppy.call(null, j), arrow.call(o, j), args.call(o, 1, 2, j))
		out.push(leaf.call(o, j), at(() => thrower.call(o, j)), rec.call(o, 140), realCall.call(m, o, 5))
		var f = fs[j % fs.length]
		out.push(at(() => f.call(o, j, 1)))
		if (j == 25) Function.prototype.call = function (t, a) { return "mine" + a }
		if (j == 30) Function.prototype.call = realCall
		out.push(at(() => m.call(o, 7)), at(() => st.call(o) > 1))
	}
	out.join()`

// TestRegNativeThrough checks calls through Function.prototype.call,
// which the call handler enters in the window one register on, against
// the Go table: the same answers and errors, marking or not, and a site of
// one function called with no exit once its cache holds it.
func TestRegNativeThrough(t *testing.T) {
	nativeCheck(t, nativeThroughSrc, `
		function m(a, b) { return this.k + a + b }
		var o = { k: 1 }, s = 0, own = Object.prototype.hasOwnProperty, keys = ["k", "j"]
		for (var i = 0; i < 1000; i++) s += m.call(o, i, 2)
		for (var i = 0; i < 1000; i++) s += own.call(o, keys[i & 1]) ? 1 : 0
		String(s)`, bytecode.RCallMW)
}

// nativeApplySrc forwards arguments by f.apply(this, arguments): to
// strict and sloppy functions given objects, primitives and nothing, to
// an arrow, built-ins, a bound function, a class constructor, a function
// that throws, one that forwards again in a recursion, a sloppy
// forwarder whose parameter is assigned before (its mapped arguments), a
// strict one's, apply replaced by a script's function and put back; and
// constructions of class libraries' forwarding constructors, which hand
// theirs to the prototype's initialize, with the prototype replaced and an
// initialize that throws.
const nativeApplySrc = `
	var out = []
	function tgt(a, b) { return (this && this.k) + ":" + a + ":" + b + ":" + arguments.length }
	function st(a) { "use strict"; return typeof this + a }
	function sl(a) { return (this === globalThis ? "G" : typeof this) + a }
	var arrow = (a) => typeof this + a
	function thr(a) { if (a % 6 == 1) throw new RangeError("r" + a); return a }
	class K {}
	var targets = [tgt, st, sl, arrow, thr, Math.max, tgt.bind({ k: "b" }), K]
	function fwd(f, t) { var g = f; return g.apply(t, arguments) }
	function fwd2() { return tgt.apply(this, arguments) }
	function fwdS() { "use strict"; return tgt.apply(this, arguments) }
	function mapped(a, b) { a = "x" + a; return tgt.apply(this, arguments) }
	function depth(n) { return n == 0 ? 0 : 1 + depth.apply(this, [n - 1]) }
	function rec() { return arguments[0] == 0 ? "end" : rec.apply(this, [arguments[0] - 1]) }
	function cls() { return function () { this.initialize.apply(this, arguments) } }
	var P = cls(); P.prototype.initialize = function (x, y) { this.x = x; this.y = y }
	var Q = cls(); Q.prototype.initialize = function (x) { if (x % 9 == 4) throw new Error("q" + x); this.q = x }
	function at(f) { try { return f() } catch (e) { return e.name + ":" + e.message } }
	var realApply = Function.prototype.apply
	for (var j = 0; j < 40; j++) {
		var o = { k: j }
		out.push(fwd2.call(o, j, 1), fwd2.call(o), fwdS.call(o, j, 2, 3), fwdS(j), mapped.call(o, j, 5), mapped.call(o))
		var f = targets[j % targets.length]
		out.push(at(() => fwd.call(o, f, j)), at(() => fwd.call(7, f, j, 2)))
		out.push(rec(j % 5), depth(20))
		var p = new P(j, "y"), q = at(() => new Q(j).q)
		out.push(p.x, p.y, p instanceof P, q)
		if (j == 15) P.prototype = { initialize: function (x) { this.x = -x } }
		if (j == 22) Function.prototype.apply = function (t, a) { return "mine" + a.length }
		if (j == 26) Function.prototype.apply = realApply
	}
	out.join()`

// TestRegNativeApply checks f.apply(this, arguments), which apply_args's
// handler enters, and constructions with forwarding constructors, which
// new_w's enters with a frame, against the Go table: the same answers
// and errors, marking or not, and both made with no exit once settled.
func TestRegNativeApply(t *testing.T) {
	was := nativeWarmExits
	nativeWarmExits = 16
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, nativeApplySrc, `
		function tgt(a, b) { return a + b }
		function fwd() { return tgt.apply(this, arguments) }
		var P = function () { this.initialize.apply(this, arguments) }
		P.prototype.initialize = function (x, y) { this.x = x; this.y = y }
		var s = 0
		for (var i = 0; i < 1000; i++) s += fwd(i, 2) + new P(i, 1).y
		String(s)`, bytecode.RApplyArgs, bytecode.RNewW)
}

// nativeArgsSrc reads arguments[i] in functions that never make their
// arguments object: sloppy and strict, of no parameter and of some, an
// index past the arguments, negative, fractional, NaN, a string, an index
// of a parameter assigned in a sloppy function (its mapped object's), and
// after the object is made by a read of arguments as a whole.
const nativeArgsSrc = `
	var out = []
	function a0() { return arguments[0] + ":" + arguments[1] + ":" + arguments[2] }
	function s0() { "use strict"; return arguments[0] + ":" + arguments[2] }
	function p2(x, y) { x = "m" + x; return arguments[0] + ":" + arguments[1] + ":" + arguments[2] }
	function sp2(x, y) { "use strict"; x = "m"; return arguments[0] + ":" + arguments[3] }
	function keys(k) { return arguments[k] }
	function whole() { var n = arguments[1]; var o = arguments; return n + ":" + o.length + ":" + arguments[0] }
	var ks = [0, 1, 2, 5, -1, 0.5, NaN, "1", "x", 1e20, -0]
	for (var j = 0; j < 30; j++) {
		out.push(a0(j, "b"), a0(), s0(j, 1, 2), p2(j, 2, 3), sp2(j, 2, 3, 4), whole(j, 9))
		var k = ks[j % ks.length]
		out.push(keys(k, "p", "q"), String(keys.apply(null, [k, 7])))
	}
	out.join()`

// TestRegNativeArgsIndex checks arguments[i] read natively from a
// frame's kept arguments against the Go table: the same answers, marking
// or not, and a forwarder's reads with no exit.
func TestRegNativeArgsIndex(t *testing.T) {
	nativeCheck(t, nativeArgsSrc, `
		function f() { return arguments[0] + arguments[1] + arguments[2] }
		function g() { "use strict"; return arguments[0] * arguments[1] }
		var s = 0
		for (var i = 0; i < 1000; i++) s += f(i, i, 1) + g(i, 2)
		String(s)`, bytecode.RArgsIndex)
}

// TestRegNativeLeafInterrupt pins a register leaf's calls, which the call
// handler enters with a frame, through the interrupt's checks: a frameless
// run of the leaf in Go counts no interrupt, and once one was due every
// later call had been left to Go, the counter never reaching its check.
// Each check is one exit, not every call after it.
func TestRegNativeLeafInterrupt(t *testing.T) {
	nativeCounting(t)
	src := `
		function run(n) {
			function f(a) { sum++ }
			var j, sum = 0
			for (j = 0; j < n; j++) { f(j); f(j) }
			return sum
		}
		String(run(10) + run(200000))`
	before := nativeExitOps()
	if got := nativeRunSrc(t, src, true); got != "400020" {
		t.Fatalf("got %s, want 400020", got)
	}
	after := nativeExitOps()
	if n := after[bytecode.RCallW] - before[bytecode.RCallW]; n > 1000 {
		t.Errorf("call_w exited %d times of 400020 calls", n)
	}
}

// nativeReflectSrc constructs by Reflect.construct: a function with
// alternating new.targets, as Babel's _createSuper does, a class, a
// derived class, a body that only stores its parameters, arguments of an
// array with holes and of an array-like object, a new.target that is no
// constructor, a bound one and a proxy, a prototype replaced and made a
// primitive, and Reflect.construct replaced by a script's function.
const nativeReflectSrc = `
	var out = []
	function A(x) { this.x = x; this.y = 1; this.t = new.target === A ? "A" : new.target.name }
	function B() {} function C() {}
	B.prototype = Object.create(A.prototype); C.prototype = Object.create(A.prototype)
	class K { constructor(a, b) { this.a = a; this.b = b } }
	class D extends K { constructor(a) { super(a, 2); this.d = 3 } }
	function L(p, q) { this.p = p; this.q = q }
	var bound = A.bind(null), px = new Proxy(function () {}, {})
	function at(f) { try { return f() } catch (e) { return e.name } }
	var real = Reflect.construct
	for (var j = 0; j < 40; j++) {
		var args = [j]
		var o = Reflect.construct(A, args, (j & 1) ? B : C)
		out.push(o.x + o.y, o.t, o instanceof B, at(() => o instanceof C), o instanceof A)
		var k = Reflect.construct(K, [j, "k"]); out.push(k.a, k.b, k instanceof K)
		var d = Reflect.construct(D, [j], K); out.push(d.a, d.b, d.d, Object.getPrototypeOf(d) === K.prototype)
		var l = Reflect.construct(L, [j, 2]); out.push(l.p + l.q)
		out.push(Reflect.construct(A, [, 2]).x, Reflect.construct(A, { length: 1, 0: "al" }).x)
		out.push(at(() => Reflect.construct(A, [1], {})), at(() => Reflect.construct(A, [1], bound).x), at(() => Reflect.construct(A, [1], px).x))
		if (j == 15) C.prototype = { tag: "c2" }
		if (j == 22) C.prototype = 7
		out.push(Reflect.construct(A, [j], C).tag)
		if (j == 30) Reflect.construct = function () { return "mine" }
		if (j == 35) Reflect.construct = real
		out.push(String(Reflect.construct(A, [j]).x))
	}
	out.join()`

// TestRegNativeReflect checks Reflect.construct's constructions, which
// the call handler enters, against the Go table: the same answers and
// errors, marking or not, and a construction with new.targets in turn
// made with no exit.
func TestRegNativeReflect(t *testing.T) {
	was := nativeWarmExits
	nativeWarmExits = 16
	defer func() { nativeWarmExits = was }()
	nativeCheck(t, nativeReflectSrc, `
		function A(x) { this.x = x; this.y = 1 }
		function B() {} function C() {}
		B.prototype = Object.create(A.prototype); C.prototype = Object.create(A.prototype)
		var s = 0, args = [0]
		for (var j = 0; j < 1000; j++) { args[0] = j; s += Reflect.construct(A, args, (j & 1) ? B : C).x }
		String(s)`, bytecode.RCallMW)
}

// TestRegNativeDeepConstructions recurses past the stack's depth through
// each entry the handlers make -- a construction, super(), apply,
// call and Reflect.construct -- each a RangeError, caught, and the
// runtime fit to go on, natively as by the Go table.
func TestRegNativeDeepConstructions(t *testing.T) {
	nativeCheck(t, `
		var out = []
		function R(n) { this.c = new R(n + 1) }
		class A { constructor(n) { this.n = n } }
		class B extends A { constructor(n) { super(n); this.b = new B(n + 1) } }
		function f() { return f.apply(this, arguments) }
		function g(x) { return g.call(this, x + 1) }
		function h() { return Reflect.construct(H, [1]) } function H() { this.x = h() }
		for (var k = 0; k < 3; k++)
			for (var t of [() => new R(0), () => new B(0), () => f(1, 2), () => g(0), () => h()]) {
				try { t(); out.push("none") } catch (e) { out.push(e.name) }
			}
		out.push(new A(5).n)
		out.join()`, "")
}
