package vm

import (
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestRegCallCachesReleasedAtTurnEnd: a call site's cache in a template
// the program shares -- a library's each -- holds the closure it called
// last, and that closure what it captured. Once the turn that made them
// has ended, and the program let go of what it captured, it is
// collected: the turn's end empties the caches (releaseRegCalls). Held,
// a finished benchmark's whole graph had stayed live through the next.
func TestRegCallCachesReleasedAtTurnEnd(t *testing.T) {
	steps := []string{
		`function each(f) { return f(); }
		 var keep = { graph: [1, 2, 3] };
		 function use() { var o = keep; return each(() => o); }
		 use(); use(); use();
		 keep`,
		`keep = undefined; 0`,
	}
	compiler.SetRegisterForm(true)
	fns := make([]*bytecode.Function, len(steps))
	for i, s := range steps {
		fns[i] = compileForTest(t, s)
	}
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	before := RegsBuilt()
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(fns[0])
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsObject() {
		t.Fatalf("got %v, want the object", v)
	}
	ref := weak.Make(v.Object())
	v = Undefined
	if built := RegsBuilt() - before; built < 2 {
		t.Fatalf("only %d functions ran as register code", built)
	}
	if n := len(r.weakMaps.calls.held); n != 0 {
		t.Errorf("%d templates' call caches held after their turn ended", n)
	}
	for _, name := range []string{"each", "use"} {
		fv, err := r.getProp(r.global, r.atoms.intern(name), Obj(r.global))
		if err != nil || !fv.IsObject() {
			t.Fatalf("%s = %v, %v", name, fv, err)
		}
		cc := fv.Object().fn().closure.closureCode
		if len(cc.regCalls) == 0 {
			t.Fatalf("%s has no call caches", name)
		}
		for i, e := range cc.regCalls {
			if e.callee != nil {
				t.Errorf("%s's call site %d holds its callee after its turn ended", name, i)
			}
		}
		if cc.regCallsHeld {
			t.Errorf("%s is still on the turn's list after its end", name)
		}
	}
	if _, err := r.Run(fns[1]); err != nil {
		t.Fatal(err)
	}
	if !collected(r, ref) {
		t.Errorf("the object the last turn's closure captured survived it")
	}
}

// TestRegCallFeedbackOutlivesTurn: what a call site called is kept, held
// weakly, past the turn whose end let its cache go (regCallSeen): a
// function warmed in one turn and compiled in the next finds its calls'
// targets where they were (regCallsRestored), as V8's call feedback holds
// a target while it lives -- and a site whose callee is gone finds none.
func TestRegCallFeedbackOutlivesTurn(t *testing.T) {
	steps := []string{
		`function g() { return 1; }
		 var h = function () { return 2; };
		 function f() { return g() + h(); }
		 for (var i = 0; i < 10; i++) f();
		 f`,
		`h = undefined; 0`,
	}
	compiler.SetRegisterForm(true)
	fns := make([]*bytecode.Function, len(steps))
	for i, s := range steps {
		fns[i] = compileForTest(t, s)
	}
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(fns[0])
	if err != nil {
		t.Fatal(err)
	}
	cl := v.Object().fn().closure
	if len(cl.regCalls) < 2 {
		t.Fatalf("f has %d call caches, want 2", len(cl.regCalls))
	}
	for i, e := range cl.regCalls {
		if e.callee != nil {
			t.Fatalf("site %d's cache holds its callee after its turn ended", i)
		}
	}
	gv, err := r.getProp(r.global, r.atoms.intern("g"), Obj(r.global))
	if err != nil || !gv.IsObject() {
		t.Fatalf("g = %v, %v", gv, err)
	}
	hv, err := r.getProp(r.global, r.atoms.intern("h"), Obj(r.global))
	if err != nil || !hv.IsObject() {
		t.Fatalf("h = %v, %v", hv, err)
	}
	h := weak.Make(hv.Object())
	hv = Undefined
	if _, err := r.Run(fns[1]); err != nil {
		t.Fatal(err)
	}
	if !collected(r, h) {
		t.Fatal("h, which nothing but f's call site held, survived")
	}
	calls := r.regCallsRestored(cl, -1)
	var got []*Object
	for _, e := range calls {
		if e.callee != nil {
			got = append(got, e.callee)
		}
	}
	if len(got) != 1 || got[0] != gv.Object() {
		t.Errorf("f's call sites restored to %v, want g's alone", got)
	}
	if e := calls[0]; e.callee == gv.Object() && (e.cl == nil || e.rc == nil || e.kind&^regCallMarks != regCallThis) {
		t.Errorf("g's site restored as %+v, want its closure, code and a plain call's kind", e)
	}
}

// TestRegCallsReleaseTheTurnsOwn: a turn's end lets go of the call caches
// of the templates the turn filled, and visits no other. After a thousand
// scripts, each a template of its own with a call site, run one a turn,
// the next turn's list holds the one template whose caches it fills, its
// own. Its end had gone through every template whose caches any turn had
// made, alive or collected but not yet found gone: a test running twenty
// thousand scripts one a turn spent three quarters of its time there,
// each turn's end longer than the last until the collector came.
func TestRegCallsReleaseTheTurnsOwn(t *testing.T) {
	compiler.SetRegisterForm(true)
	setup := compileForTest(t, `function f() { return 1; }`)
	scripts := make([]*bytecode.Function, 1000)
	for i := range scripts {
		scripts[i] = compileForTest(t, `f() + f()`)
	}
	last := compileForTest(t, `f() + held()`)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	defer SetRegTier(bytecode.RegDefault())
	r := New(Config{})
	defer r.Close()
	r.global.setOwnRaw(r.atoms.intern("held"), r.NewFunction("held", 0, func(r *Runtime, _ Value, _ []Value) (Value, error) {
		return Float(float64(len(r.weakMapRegistry().calls.held))), nil
	}), propDefault)
	if _, err := r.Run(setup); err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		if v, err := r.Run(s); err != nil || v != Float(2) {
			t.Fatalf("got %v, %v, want 2", v, err)
		}
		if n := len(r.weakMaps.calls.held); n != 0 {
			t.Fatalf("%d templates held after their turn ended", n)
		}
	}
	if v, err := r.Run(last); err != nil || v != Float(2) {
		t.Fatalf("got %v, %v, want 2: the turn held one template, its own", v, err)
	}
}
