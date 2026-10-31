package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A method call on a receiver read in place whose arguments are one or two
// locals (methodLocals) makes the call methodCallN's node makes, for every
// kind of callee: a method with a tree, a leaf, a pure body, one ending in
// a tail call, an arrow, a bound function, a built-in, one reading its
// arguments object, a getter of the method that assigns to an argument's
// local (which is read after it), a missing method, which throws a
// TypeError, and a string's and a number's; of this and of a local. Each
// function has a local of its own, so that it is run on its tree rather
// than by pureCall.
func TestMethodLocals(t *testing.T) {
	const src = `
		'use strict'
		class A {
		  constructor() { this.n = 0 }
		  tree(a, b) { let _k = 0; this.n++; return 'tree' + a + b }
		  leaf(a) { return this.n + a }
		  pure(a, b) { return a + b }
		  tail(a, b) { return this.tree(b, a) }
		  args(a, b) { let _k = 0; return arguments.length + ':' + a + b }
		  one(a) { let _k = 0; return 'one' + a }
		  self(a, b) { let _k = 0; return this.tree(a, b) + this.one(a) + this.leaf(b) + this.pure(a, b) + this.tail(a, b) + this.args(a, b) }
		}
		A.prototype.arrow = (a, b) => 'arrow' + a + b
		A.prototype.bound = function (a, b) { return 'bound' + this.n + a + b }.bind({ n: 'B' })
		function calls(o, x, y) {
		  let _k = 0, r = []
		  r.push(o.tree(x, y), o.one(x), o.leaf(y), o.pure(x, y), o.tail(x, y), o.args(x, y), o.arrow(x, y), o.bound(x, y), o.self(x, y))
		  const arr = []
		  arr.push(x, y)
		  r.push(arr.join('/'), arr.indexOf(y))
		  try { o.missing(x, y) } catch (err) { r.push(err.constructor.name) }
		  return r.join(' ')
		}
		function order(x) {
		  let _k = 0, a = x
		  const g = { get m() { a = 'changed'; return (v) => 'got' + v } }
		  return g.m(a)
		}
		function prims(s, i, n, d) { let _k = 0; return s.charAt(i) + s.slice(i, d) + n.toFixed(d) + s.at(i) }
		const out = []
		for (let round = 0; round < 3; round++) {
		  out.push(calls(new A(), 1, 2) + ' | ' + calls(new A(), 'p', 'q') + ' | ' + order('x') + ' | ' + prims('hello', 1, 2.5, 3))
		}
		const res = out[0] + ' || ' + (out[0] === out[1] && out[1] === out[2])
		res`
	const want = "tree12 one1 3 3 tree21 2:12 arrow12 boundB12 tree12one153tree212:12 1/2 1 TypeError | treepq onep 1q pq treeqp 2:pq arrowpq boundBpq treepqonep3qpqtreeqp2:pq p/q 1 TypeError | gotchanged | eel2.500e || true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		switch name := c.Fn.Name; name {
		case "calls", "order", "prims":
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", name)
			}
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
