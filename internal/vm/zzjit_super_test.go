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
