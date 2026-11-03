package vm

import "testing"

// An arrow captures its maker's new.target only where its body, or an
// arrow in it, can read it -- by name or by a direct eval (TestArrowNewTargetEval,
// in the package that has eval) -- in its rare
// part (funcExtra's newTarget); any other arrow, the commonest of
// closures, carries no room for it.
func TestArrowNewTarget(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `
			function F() { this.direct = () => new.target; this.nested = () => () => new.target;
				this.plain = () => 1 }
			class C { constructor() { this.arrow = () => new.target } }
			class D extends C { constructor() { super(); this.mine = () => new.target } }
			var f = new F(), c = new C(), d = new D(), g = (function () { return () => new.target })()
			var out = []
			for (var i = 0; i < 3; i++)
				out.push([f.direct() === F, f.nested()() === F, f.plain(), c.arrow() === C,
					d.arrow() === D, d.mine() === D, g() === undefined].join())
			out.join(' ')`))
		if err != nil {
			t.Fatal(err)
		}
		want := "true,true,1,true,true,true,true"
		if got := v.String().Go(); got != want+" "+want+" "+want {
			t.Fatalf("tree %v: got %s", tree, got)
		}
		f := r.global.getOwn(r.atoms.intern("f")).value().Object()
		plain := f.getOwn(r.atoms.intern("plain")).value().Object().fn()
		if plain.extra.rare != nil {
			t.Errorf("tree %v: an arrow that reads no new.target kept it", tree)
		}
		direct := f.getOwn(r.atoms.intern("direct")).value().Object().fn()
		if direct.extra.rare == nil {
			t.Errorf("tree %v: an arrow that reads new.target did not keep it", tree)
		}
		r.Close()
	}
}
