//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// instanceof of a constructor a captured binding holds -- a module's
// closures', as Box2D's a instanceof S -- is answered natively, as one of
// a global's is: the binding's value is what the operator checks it has.
// Every answer is the interpreter's: of the constructor's instances, its
// subclasses', others', primitives, null; after the binding is assigned
// another constructor, or the constructor given a Symbol.hasInstance of
// its own, or another prototype.
func TestJITInstanceOfCaptured(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var m=(function(){let S=function(){this.v=1},T=function(){this.v=2};
		function U(){S.call(this)};U.prototype=Object.create(S.prototype);
		function test(xs,n){let c=0;for(let i=0;i<n;i++){const x=xs[i%xs.length];if(x instanceof S)c++;if(x instanceof T)c+=10}return c}
		const xs=[new S(),new T(),new U(),{v:3},5,"s",null,undefined,Object.create(T.prototype)];
		function swap(){const s=S;S=T;T=s}
		function own(){Object.defineProperty(S,Symbol.hasInstance,{value:x=>x===5,configurable:true})}
		function unown(){delete S[Symbol.hasInstance]}
		function reproto(){S.prototype={k:1}}
		return {test,xs,swap,own,unown,reproto}})();
		function run(n){return m.test(m.xs,n)}`
	cases := []string{`run(30)`, `run(45)`, `(m.swap(),run(30))`, `(m.swap(),run(30))`, `(m.own(),run(30))`, `(m.unown(),run(30))`,
		`run(27)`, `(m.reproto(),run(30))`, `run(18)`}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cases {
		wv, werr := want.Run(compileForTest(t, "String("+c+")"))
		gv, gerr := r.Run(compileForTest(t, "String("+c+")"))
		if werr != nil || gerr != nil {
			t.Fatalf("%s: %v, interpreter's %v", c, gerr, werr)
		}
		if got, w := gv.String().Go(), wv.String().Go(); got != w {
			t.Fatalf("%s: got %s, interpreter %s", c, got, w)
		}
	}
	// In a runtime of its own, nothing changed: the tests never leave
	// native code.
	r = jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Run(compileForTest(t, `run(36)`)); err != nil {
			t.Fatal(err)
		}
	}
	test := r.global.getOwn(r.atoms.intern("m")).value().Object().getOwnVisible(r.atoms.intern("test")).value().Object().fn().closure
	e := r.jit.hint(test)
	if e == nil || e.ssa == nil {
		t.Fatalf("test was not compiled: %+v", e)
	}
	before := e.ssaStats
	if v, err := r.Run(compileForTest(t, `run(900)`)); err != nil || v.Number() != 2200 {
		t.Fatal(v, err)
	}
	st := e.ssaStats
	if r.jit.hint(test) != e || st.work-before.work < 900 {
		t.Fatalf("run(900) did not run natively: %+v, before %+v", st, before)
	}
	if left := st.hosts - before.hosts; left != 0 {
		t.Errorf("run(900) left native code %d times", left)
	}
}
