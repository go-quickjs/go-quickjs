//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// privateMemberScript's reads and writes, in code the JIT compiles round
// after round, its members' sites the tree tier's where the JIT leaves
// them to Go, answer what Node's do.
func TestJITPrivateMembersAgreeWithNode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, "function run() { return "+privateMemberScript+"; }", "run()", "run()", "run()", "run()")
	for i := range 2 {
		v, err := r.Run(compileForTest(t, "run()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != privateMemberWant {
			diffLinesForTest(t, i, got, privateMemberWant)
		}
	}
}

// A private member's site, shared by the closures of every evaluation of
// its class, learns the shapes of each one's objects; the code compiled
// for one evaluation's key reads only the cases whose shape holds that key
// where the case says, so that another evaluation's object meets the
// brand check: a TypeError, as in the interpreter. Each evaluation's
// closure is called once on its own object, then on the other's, as
// test262's private-method-brand-check-multiple-evaluations-of-class
// does: a function with a call in tail position, compiled with both
// cases, had read c2's #m through c1's code.
func TestJITPrivateCasesOfOneKey(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function g() { return "g"; }
		var mk = function () {
			class C {
				#m() { return "m"; }
				#v = 1;
				tail(o) { o.#m(); return g(); }
				read(o) { return o.#v; }
				write(o) { o.#v = 2; return 0; }
			}
			return new C();
		};
		function cross(name) {
			var r = [];
			var c1 = mk(), c2 = mk();
			c1[name](c1);
			c2[name](c2);
			try { r.push(c1[name](c2)); } catch (e) { r.push(e.constructor.name); }
			try { r.push(c2[name](c1)); } catch (e) { r.push(e.constructor.name); }
			return r.join();
		}`
	r := jitAgreesForTest(t, setup, `cross("tail")`, `cross("read")`, `cross("write")`, `cross("tail")`)
	for _, name := range []string{"tail", "read", "write"} {
		v, err := r.Run(compileForTest(t, `cross("`+name+`")`))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "TypeError,TypeError" {
			t.Errorf("%s: got %s, want TypeError,TypeError", name, got)
		}
	}
}
