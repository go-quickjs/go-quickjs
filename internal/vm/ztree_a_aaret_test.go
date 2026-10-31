package vm

import (
	"testing"
)

// A return of nothing, of a local, of this or of a constant (returnAt)
// gives what buildBlock's return gives: in a function, a method and a
// generator, and in a base and a derived constructor, whose result is the
// object made where the body returns undefined, the object returned
// otherwise, a TypeError for anything else, and a ReferenceError where
// super() has not been called. Each function has a local of its own, so
// that it is run on its tree rather than by pureCall.
func TestReturnAt(t *testing.T) {
	const src = `
		function nothing(x) { let _k = 0; if (x) return; }
		function local(x) { let _k = x + 1; return _k }
		function constant(x) { let _k = 0; if (x) return null; if (x === 0) return 7; return true }
		function self() { let _k = 0; return this }
		function* gen(x) { let _k = 0; yield x; return x }
		class Base { constructor(v) { let _k = 0; this.v = v; if (v === 1) return 5; if (v === 2) return { other: true }; return } }
		class D extends Base {
		  constructor(v, how) {
		    let _k = 0, o = { mine: v }
		    if (how === 'before') return
		    super(v)
		    if (how === 'obj') return o
		    if (how === 'prim') return 3
		    if (how === 'null') return null
		    return
		  }
		}
		const r = []
		for (let round = 0; round < 3; round++) {
		  const s = [String(nothing(1)), String(nothing(0)), local(1), String(constant(1)), constant(0), constant(''),
		    self.call(5) == 5, typeof self.call(undefined), JSON.stringify([...gen(4)]), gen(4).next(), JSON.stringify(gen(4).return(9))]
		  for (const v of [0, 1, 2]) s.push(JSON.stringify(new Base(v)))
		  for (const how of ['undef', 'obj', 'prim', 'null', 'before']) {
		    try { s.push(JSON.stringify(new D(1, how))) } catch (err) { s.push(err.constructor.name) }
		  }
		  r.push(s.map(String).join(','))
		}
		r[0] + ' | ' + (r[0] === r[1] && r[1] === r[2])`
	const want = "undefined,undefined,2,null,7,true,true,object,[4],[object Object],{\"value\":9,\"done\":true},{\"v\":0},{\"v\":1},{\"other\":true},{\"v\":1},{\"mine\":1},TypeError,TypeError,ReferenceError | true"
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
