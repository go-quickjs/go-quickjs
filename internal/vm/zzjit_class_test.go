//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A base class's constructor is compiled, and constructed with natively
// as a function is: fields set in its body, a returned object in place of
// this, new.target, instanceof, methods and getters of its prototype. A
// call of one without new throws, in a loop native code runs, at a
// method's call of it, through call and apply, a forwarding constructor's
// apply of it, and at its own call of itself in tail position; a class
// with fields, and a derived class, are constructed by Go. Each answer is
// the interpreter's, in every tier and under every stress setting.
func TestJITClassConstructors(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		class P { constructor(x, y) { this.x = x; this.y = y + 1 } get sum() { return this.x + this.y } scale(k) { return new P(this.x * k, this.y * k) } }
		class O { constructor(v) { if (v > 3) return { other: v }; this.v = v } }
		class T { constructor() { this.t = new.target === T } }
		class R { constructor(n) { if (n > 0) return R(n - 1); this.n = n } }
		class F { f = 7; constructor(x) { this.x = x } }
		class D extends P { constructor(x) { super(x, x); this.d = 1 } }
		// A function constructor forwarding to a class: its apply throws.
		function Fwd() { this.init.apply(this, arguments) }
		Fwd.prototype.init = P
		const log = []
		function build(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const p = new P(i, i)
				s += p.sum + p.scale(2).x
				const o = new O(i % 6)
				s += o.v !== undefined ? o.v : o.other * 10
				s += new T().t ? 1 : 0
				s += new F(i).f + new D(i).d
				s += (p instanceof P) ? 1 : 0
			}
			return s
		}
		function callIt(n) {
			let thrown = 0
			for (let i = 0; i < n; i++) {
				try { P(i, i) } catch (e) { if (e instanceof TypeError) thrown++ }
				try { P.call({}, i, i) } catch (e) { if (e instanceof TypeError) thrown++ }
				try { new R(i % 3) } catch (e) { if (e instanceof TypeError) thrown++ }
				try { P.apply(null, [i, i]) } catch (e) { if (e instanceof TypeError) thrown++ }
				try { new Fwd(i) } catch (e) { if (e instanceof TypeError) thrown++ }
			}
			return thrown
		}
		for (let r = 0; r < 20; r++) log.push(build(40), callIt(30))
		log.join()`
	jitDifferential(t, src)
}

// The constructor of a class constructed in a loop runs natively, called
// by the loop's own native code: it had been refused (a special function
// kind), and AWFY's Havlak and CD made every object in the tree tier.
func TestJITClassConstructorsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const setup = `
		var Node = class { constructor(id) { this.id = id; this.next = null; this.mark = 0 } }
		function chain(n) { let head = null; for (let i = 0; i < n; i++) { const x = new Node(i); x.next = head; head = x } let s = 0; for (let x = head; x; x = x.next) s += x.id; return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `chain(500)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "chain")
	ctor := jitNativeOrCalleeForTest(t, r, "Node")
	in := ctor.link.in
	if _, err := r.Run(compileForTest(t, `chain(500)`)); err != nil {
		t.Fatal(err)
	}
	if n := ctor.link.in - in; n < 500 && len(e.ssaInlined) == 0 {
		t.Errorf("Node was constructed natively %d times of 500, inlined nowhere", n)
	}
}

// jitNativeOrCalleeForTest is the entry of the global name's function,
// with code, whether Go enters it or only native callers call it.
func jitNativeOrCalleeForTest(t *testing.T, r *Runtime, name string) *jitEntry {
	t.Helper()
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("%s has no code: %+v", name, e)
	}
	return e
}
