package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An element read and write of a property of this or of a local, keyed by
// a local or by an expression (propIndexGet, propIndexSet), does what the
// separate nodes did: for an array's element, a hole, one past the end, an
// append at the length, a typed array's, a string's character, an object
// with numeric keys, an element with a getter and a setter, a frozen
// array, which a strict write throws for, a proxy, a property of the
// prototype, a missing property, which throws a TypeError, and a getter of
// the property that assigns to the key, which is read after it. Each
// function has a local of its own, so that it is run on its tree rather
// than by pureCall.
func TestPropIndex(t *testing.T) {
	const src = `
		'use strict'
		class Box {
		  constructor(a) { this.a = a; this.n = 0 }
		  get(i) { let _k = 0; return this.a[i] }
		  getNext(i) { let _k = 0; return this.a[i + 1] }
		  put(i, v) { let _k = 0; this.a[i] = v; return this.a[i] }
		  putNext(i, v) { let _k = 0; this.a[i + 1] = v; return this.a[i + 1] }
		}
		function loc(o, i) { let _k = 0; return o.a[i] }
		function locPut(o, i, v) { let _k = 0; o.a[i] = v; return o.a[i] }
		const out = []
		for (let round = 0; round < 3; round++) {
		  const acc = [0, 0]
		  Object.defineProperty(acc, 1, { get() { return 'getter' }, set(v) { acc[0] = 'set:' + v } })
		  const proxy = new Proxy([5, 6], { get(t, k) { return typeof k === 'string' ? 'p' + k : undefined } })
		  const vals = [[1, 2, 3], [1, , 3], new Int8Array([7, 8, 9]), 'str', { 0: 'zero', 1: 'one', length: 2 },
		    acc, Object.freeze([4, 5]), proxy, null, undefined]
		  const r = []
		  for (const v of vals) {
		    let k = ''
		    const b = new Box(v)
		    for (const f of [() => b.get(0), () => b.get(1), () => b.get(3), () => b.getNext(1), () => b.put(1, 'x'), () => b.put(3, 'y'),
		                     () => b.putNext(-2, 'z'), () => loc(b, 2), () => locPut(b, 0, 'w')]) {
		      try { k += String(f()) + ';' } catch (err) { k += err.constructor.name + ';' }
		    }
		    r.push(k + (v === acc ? acc[0] : ''))
		  }
		  const P = class extends Box { }
		  const q = Object.create({ a: ['proto'] })
		  r.push(String(Box.prototype.get.call(q, 0)))
		  try { loc({}, 0) } catch (err) { r.push(err.constructor.name) }
		  const order = { i: 0, get a() { this.i = 1; return ['first', 'second'] } }
		  function ord(o) { let _k = 0, i = 0; const g = { get a() { i = 1; return ['first', 'second'] } }; return g.a[i] }
		  r.push(ord(order), String(new P([9]).get(0)))
		  out.push(r.join(','))
		}
		const res = out[0] + ' | ' + (out[0] === out[1] && out[1] === out[2])
		res`
	const want = "1;2;undefined;3;x;y;z;3;w;,1;undefined;undefined;3;x;y;z;3;w;,7;8;undefined;9;0;undefined;undefined;9;0;,s;t;undefined;r;TypeError;TypeError;TypeError;r;TypeError;,zero;one;undefined;undefined;x;y;z;undefined;w;,0;getter;undefined;undefined;getter;y;z;undefined;w;w,4;5;undefined;undefined;TypeError;TypeError;TypeError;undefined;TypeError;,p0;p1;p3;p2;p1;p3;p-1;p2;p0;,TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;,TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;TypeError;,proto,TypeError,second,9 | true"
	for _, c := range compileForTest(t, src).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		switch name := c.Fn.Name; name {
		case "loc", "locPut":
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
