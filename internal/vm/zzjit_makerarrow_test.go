//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// An arrow's super reads the home object of the method it is in, which the
// arrow takes from its maker's function object as it is made: a frame Go
// made of a native callee had its function object only where the callee's
// own code read it, so an arrow such a frame made -- after the callee left
// native code -- had no home object, and its super threw "super is only
// valid inside a method": a method's, an object literal's, a static one's,
// one through an arrow between, after a few calls with the JIT's default
// thresholds too (bytecode.Function's FrameCallee). Each answer Node's,
// compiled at the first call and at the second, idle and while the
// collector marks, and under forced deoptimizations.
func TestJITArrowSuperOfNativeCallee(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `'use strict'
		function method() { class A { m() { return 'A' } } class B extends A { m(x) { const g = () => super.m() + x; return g() } } return new B().m(1) }
		var kept = new (class extends (class { m() { return 'K' } }) { m() { const g = () => super.m(); return g() } })()
		function shared() { return kept.m() }
		function literal() { const p = { m() { return 'P' } }; const o = { __proto__: p, m() { const g = () => super.m(); return g() } }; return o.m() }
		function statics() { class A { static s() { return 'S' } } class B extends A { static s() { const g = () => super.s(); return g() } } return B.s() }
		function nested() { class A { m() { return 'A' } } class B extends A { m() { const g = () => () => super.m(); return g()() } } return new B().m() }
		function inner() { class A { m() { return 'A' } } class B extends A { m() { const mk = () => { const h = () => super.m(); return h() }; return mk() } } return new B().m() }
		function target() { function F() { const g = () => new.target; this.v = g() === F } return new F().v }
		function derived() { class A {} class C extends A { constructor() { super(); const g = () => new.target; this.v = g() === C } } return new C().v }
		function all() { const out = []; for (const f of [method, shared, literal, statics, nested, inner, target, derived]) { try { out.push(f()) } catch (e) { out.push(e.name) } } return out.join() }`
	const want = "A1,K,P,S,A,A,true,true"
	for _, mode := range []string{"idle", "marking", "stress", "second"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "marking" {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			switch mode {
			case "stress":
				r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			case "second":
				r.jitCallThreshold = 2
			}
			if _, err := r.Run(compileForTest(t, setup)); err != nil {
				t.Fatal(err)
			}
			var got []string
			for range 30 {
				v, err := r.Run(compileForTest(t, `all()`))
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, v.String().Go())
			}
			for i, g := range got {
				if g != want {
					t.Fatalf("call %d: %s, want %s (all: %s)", i, g, want, strings.Join(got, " | "))
				}
			}
			if e := r.jit.hint(r.global.getOwn(r.atoms.intern("all")).value().Object().fn().closure); e == nil || e.ssa == nil {
				t.Fatal(fmt.Sprintf("all is not native: %+v", e))
			}
		})
	}
}
