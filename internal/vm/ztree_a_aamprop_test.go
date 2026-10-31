package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A method call on a property of this or of a local (methodProp) makes the
// call the spill and callNode made, and reads the receiver's property once:
// for a method with a tree, a leaf, a pure body, a getter of the receiver
// and of the method (each counted), a proxy's, a string's, a number's,
// null's and a missing method's, which throw TypeErrors; for 0 to 4
// arguments, arguments that assign the property the receiver is read from,
// which the call is made on before, ones with a conditional, which ends a
// block and spills the entries, and a nested call; an optional call of a
// method there and not there and of a receiver that is not there, a call
// in tail position, this.k.apply(this, arguments), a template tag,
// charCodeAt and a RegExp literal's argument. Each function has a local of
// its own, so that it is run on its tree rather than by pureCall.
func TestMethodProp(t *testing.T) {
	const src = `
		'use strict'
		let log = ''
		class T {
		  constructor(n) { this.n = n }
		  tree(a, b) { let _k = 0; return 'tree' + this.n + a + b }
		  leaf() { return this.n }
		  pure(a, b, c) { return a + b + c }
		  four(a, b, c, d) { let _k = 0; return [this.n, a, b, c, d].join('.') }
		  get dyn() { log += 'getm;'; return (x) => 'dyn' + this.n + x }
		  tag(s, x) { let _k = 0; return s.raw[0] + x + this.n }
		}
		class H {
		  constructor() { this.a = new T(1); this.s = 'hello'; this.x = 2.5; this.z = null; this.re = 'aXbX' }
		  get g() { log += 'getr;'; return new T('g') }
		  calls(o) {
		    let _k = 0
		    const r = [this.a.tree(1, 2), this.a.leaf(), this.a.pure(1, 2, 3), this.a.four(1, 2, 3, 4), this.a.dyn(5), this.g.tree(3, 4), this.g.leaf(),
		      o.a.tree(5, 6), o.a.leaf(), this.s.slice(1, 3), this.s.charCodeAt(1), this.x.toFixed(2), this.re.replace(/X/g, '-'),
		      this.a.tree((this.a = new T(2), 'r'), this.a.n), this.a.tree(o.a ? 'y' : 'n', this.a.leaf()), this.a.tree(this.a.pure(1, 1, 1), o.a.leaf()),
		      String(this.a.missing?.()), String(this.a.leaf?.()), String(this.z?.leaf()), this.a.tag` + "`q${7}`" + `]
		    for (const f of [() => this.z.leaf(), () => this.a.missing(), () => o.zz.tree(), () => this.a.tree.call()]) {
		      try { r.push(String(f())) } catch (err) { r.push(err.constructor.name) }
		    }
		    return r.join(' ')
		  }
		  tail(x) { return this.a.tree(x, 'tail') }
		}
		function fwd() { this.a.apply(this, arguments) }
		const p = { a: new Proxy(new T('p'), { get(t, k, r) { log += 'trap' + String(k) + ';'; return Reflect.get(t, k, r) } }) }
		const out = []
		for (let round = 0; round < 3; round++) {
		  log = ''
		  const h = new H()
		  const seen = []
		  fwd.call({ a(x, y) { seen.push(this.a === undefined, x, y) } }, 8, 9)
		  out.push(h.calls({ a: new T('o') }) + ' | ' + h.tail(9) + ' | ' + new H().calls(p) + ' | ' + seen.join() + ' | ' + log)
		}
		out[0] + ' || ' + (out[0] === out[1] && out[1] === out[2])`
	const want = "tree112 1 6 1.1.2.3.4 dyn15 treeg34 g treeo56 o el 101 2.50 a-b- tree1r2 tree2y2 tree23o undefined 2 undefined q72 TypeError TypeError TypeError TypeError | tree29tail | tree112 1 6 1.1.2.3.4 dyn15 treeg34 g treep56 p el 101 2.50 a-b- tree1r2 tree2y2 tree23p undefined 2 undefined q72 TypeError TypeError TypeError TypeError | false,8,9 | getm;getr;getr;getm;getr;getr;traptree;trapn;trapleaf;trapn;trapleaf;trapn; || true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil && c.Fn.Name == "fwd" && buildTree(c.Fn) == nil {
			t.Errorf("fwd is not built as a tree")
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}
