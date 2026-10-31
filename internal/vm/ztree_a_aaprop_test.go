package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A local given a property of a local, of this or of an upvalue (storeProp)
// gets what the read gives in every case the read has: an own property of
// one shape and then of others, one of the prototype, a getter, one that
// throws, a getter that writes the local being stored to through a closure
// (the read's value is stored after it), a proxy's, a primitive's -- a
// string's length, a number's method -- a missing one, and null's, which
// throws a TypeError. Each function has a local of its own beyond the one
// it stores to, so that it is run on its tree rather than by pureCall.
func TestStoreProp(t *testing.T) {
	const src = `
		class P { constructor(v) { this.v = v } get g() { return 'g' + this.v } walk() { let _k = 0, x = this.v; x = this.v; return x } }
		P.prototype.q = 'proto'
		let w = 0
		const vals = [
		  { v: 1 }, { a: 0, v: 2 }, new P(3), { get v() { return 'getter' } },
		  new Proxy({}, { get(t, k) { return 'proxy:' + String(k) } }),
		  'str', 5, {}, null, { get v() { throw new RangeError('no') } },
		]
		function loc(o) { let _k = 0, x = 0; x = o.v; return x }
		function locGet(o) { let _k = 0, x = 0, y = (x = o.v); return x + '/' + y }
		function len(o) { let _k = 0, x = 0; x = o.length; return x }
		function proto(o) { let _k = 0, x = 0; x = o.q; return x }
		function up(o) { let _k = 0; const f = () => { let _j = 0, x = 0; x = o.v; return x }; return f() }
		function own(o) {
		  let _k = 0, x = 'before'
		  const set = () => { x = 'set' }
		  const g = { get v() { set(); return 'after' } }
		  x = g.v
		  return x
		}
		const s = []
		for (let round = 0; round < 3; round++) {
		  const r = []
		  for (const o of vals) {
		    let k = ''
		    for (const fn of [loc, locGet, len, proto, up]) {
		      try { k += String(fn(o)) + ';' } catch (err) { k += err.constructor.name + ';' }
		    }
		    r.push(k)
		  }
		  r.push(new P(7).walk(), new P('x').g, own())
		  s.push(r.join(','))
		}
		const out = s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
		out`
	const want = "1;1/1;undefined;undefined;1;,2;2/2;undefined;undefined;2;,3;3/3;undefined;proto;3;,getter;getter/getter;undefined;undefined;getter;,proxy:v;proxy:v/proxy:v;proxy:length;proxy:q;proxy:v;,undefined;undefined/undefined;3;undefined;undefined;,undefined;undefined/undefined;undefined;undefined;undefined;,undefined;undefined/undefined;undefined;undefined;undefined;,TypeError;TypeError;TypeError;TypeError;TypeError;,RangeError;RangeError;undefined;undefined;RangeError;,7,gx,after | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		switch name := c.Fn.Name; name {
		case "loc", "locGet", "len", "proto", "up", "own":
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
