//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// superSetup's methods read and write their home objects' prototypes'
// properties through super, in loops: a method's call, a getter's and a
// setter's (the receiver the instance), a computed key's, an object
// literal's method's, a static method's and an async method's; nul's class
// extends null, whose super read throws.
const superSetup = `
	class A {
		constructor() { this.n = 1 }
		m(x) { return x + this.n }
		get g() { return this.n * 10 }
		set s(v) { this.seen = v }
		static st(x) { return x * 2 }
	}
	class B extends A {
		m(x) { let s = 0; for (let i = 0; i < 4; i++) s += super.m(i + x); return s + super.g }
		k(x) { let s = 0; const name = 'm'; for (let i = 0; i < 3; i++) s += super[name](x); super.s = s; return s + this.seen }
		w(x) { for (let i = 0; i < 2; i++) super[x] = i; return this[x] }
		static st(x) { let s = 0; for (let i = 0; i < 3; i++) s += super.st(x + i); return s }
		async am(x) { let s = 0; for (let i = 0; i < 2; i++) s += super.m(await x + i); return s }
	}
	const lit = { __proto__: { f(x) { return x * 3 } }, f(x) { let s = 0; for (let i = 0; i < 3; i++) s += super.f(x + i); return s } }
	class N extends null { r() { for (let i = 0; i < 2; i++) super.x; return 1 } }
	var out = []
	function run(n) {
		const b = new B()
		let s = 0
		for (let i = 0; i < n; i++) {
			s += b.m(i) + b.k(i) + b.w('p' + (i & 3)) + B.st(i) + lit.f(i)
			b.am(i).then(v => out.push(v))
		}
		return s
	}
	function nul() { try { return N.prototype.r() } catch (e) { return e.constructor.name + ': ' + e.message } }`

// A super reference's read or write -- super.k, super[k], and its base --
// is Go's from native code (ssa.GoHost) on the frame Go entered the code
// with, which has the function object whose home object it reads through,
// and the this it reads with: a method's loop of super calls stays native.
// Such methods -- 117 of npm's own functions -- had been refused whole.
// Their code has no call entry (ir.Program's FrameCallee), as a native
// callee's frame has no function object. Each answer is the
// interpreter's, the throws included.
func TestJITSuperReferences(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, superSetup)
	for range 3 {
		both.agree(t, `run(200)`, `nul()`, `out.length + ':' + out.slice(-3).join()`)
	}
	r := both.jit
	for _, src := range []string{`B.prototype.m`, `B.prototype.k`, `B.prototype.w`, `B.st`, `lit.f`} {
		cl := (*closure)(nil)
		if v, err := r.Run(compileForTest(t, src)); err == nil && v.IsObject() && v.Object().fn() != nil {
			cl = v.Object().fn().closure
		}
		e := r.jit.entryFor(cl.fn)
		if e == nil || e.ssa == nil || e.ssaStats.entries == 0 {
			t.Fatalf("%s never ran natively: %+v", src, e)
		}
		if e.ssa.CallEntry() != 0 {
			t.Errorf("%s has a call entry", src)
		}
		if e.ssaStats.hosts > 8 {
			t.Errorf("%s left native code %d times", src, e.ssaStats.hosts)
		}
	}
	jitAgreesForTest(t, superSetup, `run(20)`, `nul()`, `run(30)`, `out.join()`)
}

// definitionSetup's callees, called natively by loops, make classes and
// object literals with methods, accessors and private members: point's
// literal has a method and an accessor, which its home object is given;
// cls makes a class extending Root, with a method, a getter and a static
// method; mixin's has a private field and a private method too, whose
// methods' object it keeps in a local, which a native callee's frame Go
// makes does not have, so it stays Go's to call; counter's literal method
// reads super.
const definitionSetup = `
	function point(x, y) { return { x, y, len() { return this.x + this.y }, get twice() { return this.len() * 2 } } }
	function mixin(Base, k) {
		return class extends Base {
			#v = k
			#bump() { return this.#v + 1 }
			get v() { return this.#bump() }
			static make() { return new this() }
		}
	}
	class Root { constructor() { this.r = 1 } }
	function cls(k) { return class extends Root { m() { return k + this.r } get g() { return k * 2 } static s() { return k } } }
	function counter(n) { return { __proto__: { base() { return n } }, get() { return super.base() + 1 } } }
	function run(n) {
		let s = 0
		for (let i = 0; i < n; i++) {
			const p = point(i, 2)
			s += p.len() + p.twice
			const C = mixin(Root, i)
			s += new C().v + C.make().r
			s += counter(i).get()
			const D = cls(i)
			s += new D().m() + new D().g + D.s()
		}
		return s
	}
	function ext(p) { return class extends p { q() { return 1 } } }
	function runExt(n) { let s = 0, errs = ''; for (let i = 0; i < n; i++) { try { const C = ext(i % 50 === 49 ? 5 : Root); s += typeof C === 'function' ? 1 : 0 } catch (e) { errs = e.constructor.name + ': ' + e.message } } return s + ':' + errs }
	function runLit(n) { let s = 0; for (let i = 0; i < n; i++) { const p = point(i, 2); s += p.x + p.y } return s }
	function runCls(n) { let s = 0; for (let i = 0; i < n; i++) { const D = cls(i); s += typeof D === 'function' ? 1 : 0 } return s }`

// A native callee's definitions of class members and object literals'
// methods -- new_class, set_home_object, define_method, a getter's, a
// private name's and member's, install_private_methods -- are Go's from
// native code, on the frame Go makes of its level (jitGoCallNested), which
// has its closure and this, or with no frame where they run no script and
// throw nothing (jitGoHostPlain): they read no local, which native code
// holds, nor the function object, which it has not (compile's
// frameDefinition).
// Such callees -- 161 functions of npm's own code making classes, 124
// literals' methods, 190 private members' -- had been called by Go.
func TestJITDefinitionCallees(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	jitNoInlineForTest(t, "point")
	both := jitBothForTest(t, definitionSetup)
	for range 6 {
		both.agree(t, `run(100)`, `runLit(200)`, `runCls(200)`, `runExt(100)`)
	}
	r := both.jit
	for _, name := range []string{"point", "cls", "ext"} {
		e := jitEntryForTest(t, r, name)
		in := e.link.in
		both.agree(t, `runLit(200)`, `runCls(200)`, `runExt(200)`)
		if e.link.in-in < 150 {
			t.Errorf("%s was called natively %d times of 200", name, e.link.in-in)
		}
	}
	jitAgreesForTest(t, definitionSetup, `run(10)`, `run(30)`, `run(10)`, `runExt(30)`, `runLit(30)`, `runCls(30)`)
}
