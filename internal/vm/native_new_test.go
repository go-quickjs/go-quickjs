//go:build amd64 || arm64

package vm

import (
	"slices"
	"strings"
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
	// The constructor cache must see changes to the constructor's shape,
	// prototype value and instance chain, including proxy traps. Reuse the
	// exact-answer cases of the Go cache, under both marking states.
	nativeCheck(t, instanceOfChangesSrc, "")
	for _, body := range []string{
		"if (v instanceof c) return true; return false",
		"if (!(v instanceof c)) return false; return true",
	} {
		nativeCheck(t, strings.ReplaceAll(nativeInstanceOfSrc, "return v instanceof c", body), "")
		global := strings.ReplaceAll(nativeInstanceOfSrc, "function at(v, c) { try", "function at(v, c) { currentCtor = c; try")
		global = "var currentCtor;\n" + strings.ReplaceAll(global, "return v instanceof c", strings.ReplaceAll(body, "instanceof c", "instanceof currentCtor"))
		nativeCheck(t, global, "")
	}
	nativeCheck(t, `
		var out = [], reads = 0, values = 0
		function A() {} function B() {}
		Object.defineProperty(globalThis, 'Ctor', { value: A, writable: true, configurable: true })
		var a = new A(), b = new B()
		function site(v) { try { if (v instanceof Ctor) return true; return false } catch (e) { return e.name } }
		function run(v) { var r; for (var i = 0; i < 100; i++) r = site(v); out.push(r) }
		run(a); run(b); Ctor = B; run(b); run(a)
		B.prototype = 3; run(b); run(1); B.prototype = A.prototype; run(a)
		Ctor = A.bind(null); run(a)
		Ctor = new Proxy(A, {}); run(a)
		Ctor = { [Symbol.hasInstance](v) { return v === 7 } }; run(7); run(a)
		Ctor = 3; run(a)
		Object.defineProperty(globalThis, 'Ctor', { configurable: true, get() { reads++; if (reads % 2) return A; throw new RangeError('ctor') } })
		run(a); out.push(reads)
		function value() { values++; return a }
		for (var i = 0; i < 100; i++) { try { if (value() instanceof Ctor) values++ } catch (e) {} }
		out.push(reads, values)
		delete globalThis.Ctor
		run(a)
		function late() { try { if (a instanceof Later) return true; return false } catch (e) { return e.name } }
		out.push(late()); let Later = A; out.push(late())
		out.join()`, "")
	// The constructor's read in a conditional's arm, which the branch
	// joins; Symbol.hasInstance given to the constructor once the site has
	// run; the global's value redefined, and assigned.
	nativeCheck(t, `
		var out = []
		function GA() {} function GB() {} function GH() {} function GS() {} function GL() {}
		var b = new GB(), s = new GS(), l = new GL(), n = 0
		function pick(i) { if (b instanceof (i & 1 ? GA : GB)) return "y"; return "n" }
		var p = ""; for (var i = 0; i < 200; i++) p += pick(i); out.push(p.slice(0, 6), p.length)
		for (var i = 0; i < 300; i++) { if (i == 150) Object.defineProperty(GH, Symbol.hasInstance, { value: function (v) { return v === b } }); if (b instanceof GH) n++ }
		out.push(n); n = 0
		function ks() { if (s instanceof GS) return 1; return 0 }
		for (var i = 0; i < 200; i++) n += ks(); Object.defineProperty(globalThis, "GS", { value: function () {} }); for (var i = 0; i < 200; i++) n += ks()
		out.push(n); n = 0
		function kl() { try { if (l instanceof GL) return 1; return 0 } catch (e) { return e.name } }
		for (var i = 0; i < 200; i++) n += kl(); GL = function () {}; out.push(n, kl())
		out.join()`, "")
	nativeCheck(t, `
		function F() {} var o = new F(), n = 0
		do { if (++n == 20000) o = {} } while (o instanceof F)
		String(n)`, "")
	nativeCheck(t, nativeInstanceOfSrc, `
		class K {} class L extends K {}
		function F() {}
		var o = new L(), f = new F(), s = 0
		for (var i = 0; i < 1000; i++) if (o instanceof K && !(f instanceof K) && f instanceof F && !(i instanceof F)) s++
		String(s)`, bytecode.RGetGlobalInstanceOf, bytecode.RInstanceOfNext)
}

// instanceOfBranchSrc is instanceof consumed by a branch alone
// (instanceof_next), forward both ways and as a loop's back edge, each
// case run until its sites' caches hold it: primitives on the left, a
// derived class, Symbol.hasInstance of the constructor's own and
// inherited, defined after the site warmed and deleted again, put on the
// constructor's prototype chain; bound functions, proxies as the
// constructor (its get trap's keys) and as the instance, a proxy's
// getPrototypeOf trap, a right side that is no function or no object, an
// object with a Symbol.hasInstance, a prototype that is no object, an
// arrow, a throwing getter, and the instance's chain and the prototype
// changed after the sites warmed. instanceOfBranchWant is Node's answer
// (vm.runInThisContext); the messages are the engine's own.
const instanceOfBranchSrc = `
	var out = [], keys = []
	function br(v, C) { if (v instanceof C) return "T"; return "F" }
	function nbr(v, C) { if (!(v instanceof C)) return "f"; return "t" }
	function loop(v, C) { var i = 0; do { if (++i >= 3) break } while (v instanceof C); return i }
	function at(f, v, C) { try { return f(v, C) } catch (e) { return e.name } }
	function run(name, v, C) {
		var r
		for (var i = 0; i < 40; i++) r = at(br, v, C) + at(nbr, v, C) + at(loop, v, C)
		out.push(name + "=" + r)
	}
	function F() {} var f = new F()
	class K {} class L extends K {} var l = new L()
	run("plain", f, F); run("object", {}, F); run("int", 1, F); run("string", "s", F); run("null", null, F)
	run("undefined", undefined, F); run("symbol", Symbol(), F); run("bigint", 1n, F); run("derived", l, K); run("base", new K(), L)
	class H { static [Symbol.hasInstance](v) { return v === 7 } } class H2 extends H {}
	run("own-has", 7, H); run("own-has-no", f, H); run("inherited-has", 7, H2); run("inherited-has-no", 8, H2)
	function G() {} var g = new G()
	run("before-replace", g, G)
	Object.defineProperty(G, Symbol.hasInstance, { value: v => v === 1, configurable: true })
	run("replaced", g, G); run("replaced-int", 1, G)
	delete G[Symbol.hasInstance]
	run("restored", g, G)
	function G2() {} var g2 = new G2()
	run("before-ctor-proto", g2, G2)
	Object.setPrototypeOf(G2, Object.create(Function.prototype, { [Symbol.hasInstance]: { value: () => "yes" } }))
	run("ctor-proto", 0, G2)
	var BF = F.bind(null), BBF = BF.bind(null)
	run("bound", f, BF); run("bound-bound", f, BBF); run("bound-no", {}, BF); run("bound-int", 1, BF)
	var PF = new Proxy(F, {}), PT = new Proxy(F, { get(t, k, r) { keys.push(typeof k === "symbol" ? k.description : k); return Reflect.get(t, k, r) } })
	run("proxy-ctor", f, PF); run("proxy-trap", f, PT)
	out.push("keys=" + keys.slice(0, 4).join(","))
	var px = new Proxy(new F(), {}), pxo = new Proxy(new F(), { getPrototypeOf() { return Object.prototype } })
	run("proxy-instance", px, F); run("proxy-instance-trap", pxo, F); run("proxy-in-chain", Object.create(px), F)
	run("plain-object-right", f, {}); run("int-right", f, 1); run("null-right", f, null); run("string-right", f, "F")
	run("has-on-object", f, { [Symbol.hasInstance]: () => 1 })
	function P() {} P.prototype = 3
	run("nonobject-proto", {}, P); run("nonobject-proto-int", 1, P)
	var arrow = () => 1
	run("arrow", {}, arrow); run("arrow-int", 1, arrow)
	function T() {} Object.defineProperty(T, Symbol.hasInstance, { get() { throw new RangeError("x") } })
	run("throwing-has", f, T)
	function R() {} var ri = new R(), other = {}
	run("chain-before", ri, R)
	Object.setPrototypeOf(ri, other)
	run("chain-cut", ri, R)
	Object.setPrototypeOf(other, Object.create(R.prototype))
	run("chain-deep", ri, R)
	R.prototype = {}
	run("ctor-proto-reassigned", ri, R); run("new-after-reassign", new R(), R)
	function msg(v, C) { try { if (v instanceof C) return "T"; return "F" } catch (e) { return e.message } }
	out.push(msg(f, {}), msg(f, 1), msg({}, P))
	out.join("\n")`

const instanceOfBranchWant = `plain=Tt3
object=Ff1
int=Ff1
string=Ff1
null=Ff1
undefined=Ff1
symbol=Ff1
bigint=Ff1
derived=Tt3
base=Ff1
own-has=Tt3
own-has-no=Ff1
inherited-has=Tt3
inherited-has-no=Ff1
before-replace=Tt3
replaced=Ff1
replaced-int=Tt3
restored=Tt3
before-ctor-proto=Tt3
ctor-proto=Tt3
bound=Tt3
bound-bound=Tt3
bound-no=Ff1
bound-int=Ff1
proxy-ctor=Tt3
proxy-trap=Tt3
keys=Symbol.hasInstance,prototype,Symbol.hasInstance,prototype
proxy-instance=Tt3
proxy-instance-trap=Ff1
proxy-in-chain=Tt3
plain-object-right=TypeErrorTypeErrorTypeError
int-right=TypeErrorTypeErrorTypeError
null-right=TypeErrorTypeErrorTypeError
string-right=TypeErrorTypeErrorTypeError
has-on-object=Tt3
nonobject-proto=TypeErrorTypeErrorTypeError
nonobject-proto-int=Ff1
arrow=TypeErrorTypeErrorTypeError
arrow-int=Ff1
throwing-has=RangeErrorRangeErrorRangeError
chain-before=Tt3
chain-cut=Ff1
chain-deep=Tt3
ctor-proto-reassigned=Ff1
new-after-reassign=Tt3
the right operand of "instanceof" is not callable
the right operand of "instanceof" must be an object
the prototype is not an object`

// TestRegNativeInstanceOfBranch runs instanceOfBranchSrc's branches on
// an instanceof, which the register compiler fuses (instanceof_next), by
// the Go table and natively, marking and not, with Node's answers.
func TestRegNativeInstanceOfBranch(t *testing.T) {
	prog := compileForTest(t, instanceOfBranchSrc)
	n := 0
	for _, c := range prog.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		if rc := c.Fn.RegCode(); rc != nil && slices.ContainsFunc(rc.Code, func(w bytecode.RInstr) bool { return w.Op() == bytecode.RInstanceOfNext }) {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("%d functions branch on a fused instanceof, want br, nbr, loop and msg", n)
	}
	if got := nativeRunSrc(t, instanceOfBranchSrc, false); got != instanceOfBranchWant {
		t.Errorf("Go table:\n%s\nwant\n%s", got, instanceOfBranchWant)
	}
	nativeCheck(t, instanceOfBranchSrc, "")
	global := "var globalCtor;\n" + strings.ReplaceAll(instanceOfBranchSrc, "instanceof C", "instanceof globalCtor")
	for _, signature := range []string{"br(v, C)", "nbr(v, C)", "loop(v, C)", "msg(v, C)"} {
		global = strings.ReplaceAll(global, "function "+signature+" {", "function "+signature+" { globalCtor = C;")
	}
	if got := nativeRunSrc(t, global, false); got != instanceOfBranchWant {
		t.Errorf("global Go table:\n%s\nwant\n%s", got, instanceOfBranchWant)
	}
	nativeCheck(t, global, "")
}

// TestRegNativeInstanceOfRealms branches on instanceof between two realms'
// constructors and objects, by the Go table and natively, marking and
// not: an object is an instance of its own realm's constructors alone,
// Symbol.hasInstance being each realm's own.
func TestRegNativeInstanceOfRealms(t *testing.T) {
	t.Parallel()
	const foreign = `function FB() {} [FB, new FB(), [], {}]`
	const src = `
	function FA() {} var a = new FA(), out = []
	function br(v, C) { if (v instanceof C) return "T"; return "F" }
	function loop(v, C) { var i = 0; do { if (++i >= 3) break } while (v instanceof C); return i }
	var FB = foreign[0], b = foreign[1]
	for (var i = 0; i < 40; i++) {
		out = [br(a, FA), br(b, FB), br(a, FB), br(b, FA), br(foreign[2], Array), br(foreign[2], foreign[2].constructor),
			br(foreign[3], Object), br(b, foreign[3].constructor), loop(b, FB), loop(b, FA)]
	}
	out.join()`
	// Node, the foreign values from vm.runInNewContext.
	const want = "T,T,F,F,F,T,F,T,3,1"
	for _, native := range []bool{false, true} {
		for _, marking := range []bool{false, true} {
			if marking && !native {
				continue
			}
			nativeMarking(t, marking)
			r := newTestRuntime(t, nativeConfig(native))
			other := r.NewRealm()
			fv, err := r.RunIn(other, compileForTest(t, foreign))
			if err == nil {
				err = r.SetProp(Obj(r.Global()), r.Intern("foreign"), fv)
			}
			var v Value
			if err == nil {
				v, err = r.Run(compileForTest(t, src))
			}
			if err != nil {
				t.Fatalf("native %v, marking %v: %v", native, marking, err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("native %v, marking %v: %s, want %s", native, marking, got, want)
			}
			r.Close()
		}
	}
	nativeMarking(t, false)
}

// nativeInstanceOfSlotSrc runs one instanceof site through the prototype
// slot its cache keeps (propCache.instanceProto) and each change that
// must be seen there: another function of the constructor's shape, whose
// own prototype is read; the prototype made a primitive, a TypeError; the
// prototype assigned another object; the instance's chain changed; and a
// Symbol.hasInstance of the constructor's own, which changes its shape.
const nativeInstanceOfSlotSrc = `
	function A() {} function B() {}
	var a = new A(), b = new B(), out = []
	function site(v, c) { try { return v instanceof c } catch (e) { return e.name } }
	function run(v, c) { var r; for (var i = 0; i < 100; i++) r = site(v, c); out.push(r) }
	run(a, A); run(b, A); run(b, B); run(a, B); run(1, A)
	A.prototype = 3; run(a, A); run(1, A); run(b, B)
	A.prototype = B.prototype; run(b, A); run(a, A)
	Object.setPrototypeOf(a, B.prototype); run(a, A); run(a, B)
	Object.defineProperty(A, Symbol.hasInstance, { value: v => v === 1 }); run(1, A); run(b, A)
	out.join()`

// The prototype slot instanceof's site keeps answers as the Go table
// does, through every change nativeInstanceOfSlotSrc makes, marking or
// not.
func TestRegNativeInstanceOfSlot(t *testing.T) {
	const want = "true,false,true,false,false,TypeError,false,true,true,false,true,true,true,false"
	if got := nativeRunSrc(t, nativeInstanceOfSlotSrc, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	nativeCheck(t, nativeInstanceOfSlotSrc, `
		function A() {} function B() {}
		var a = new A(), b = new B(), s = 0
		function site(v, c) { return v instanceof c }
		for (var i = 0; i < 1000; i++) if (site(a, A) && site(b, B) && !site(a, B)) s++
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
	function alias() { var a = arguments; return a[0] + ":" + a.length + ":" + a[3] }
	function mappedAlias(x) { var a = arguments; x = "m"; return a[0] + ":" + a.length }
	var ks = [0, 1, 2, 5, -1, 0.5, NaN, "1", "x", 1e20, -0]
	for (var j = 0; j < 30; j++) {
		out.push(a0(j, "b"), a0(), s0(j, 1, 2), p2(j, 2, 3), sp2(j, 2, 3, 4), whole(j, 9), alias(j), mappedAlias(j))
		out.push(alias.call(null, j, 2), alias.apply(null, [j, 2]))
		var k = ks[j % ks.length]
		out.push(keys(k, "p", "q"), String(keys.apply(null, [k, 7])))
	}
	out.join()`

// TestRegNativeArgsIndex checks arguments[i] read natively from a
// frame's kept arguments against the Go table: the same answers, marking
// or not, and a forwarder's reads with no exit.
func TestRegNativeArgsIndex(t *testing.T) {
	nativeCheck(t, nativeArgsSrc, `
		function f() { var a = arguments; return a[0] + a[1] + a[2] }
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
	before := nativeExitOps(t)
	if got := nativeRunSrc(t, src, true); got != "400020" {
		t.Fatalf("got %s, want 400020", got)
	}
	after := nativeExitOps(t)
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
