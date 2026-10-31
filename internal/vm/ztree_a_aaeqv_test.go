package vm

import (
	"testing"
)

// An equality as a value against a literal, a string constant or a number
// (eqValue) gives what compareNode's test gave: for ==, !=, === and !== of
// null, undefined, true, false, a string constant and the numbers 0, 1 and
// NaN, against values of every kind -- numbers, -0, NaN, strings that are
// numbers, booleans, BigInts, document.all, objects whose valueOf logs and
// one whose valueOf throws -- read as a local, an upvalue, a property of
// this and of a local, and a tree. Each function has a local of its own, so
// that it is run on its tree rather than by pureCall.
func TestEqualityValues(t *testing.T) {
	const src = `
		let log = ''
		const vals = [0, -0, 1, NaN, '0', '1', '', 'go', true, false, null, undefined, 0n, 1n, dda,
		  { valueOf() { log += 'v'; return 1 } }, { valueOf() { throw new RangeError('no') } }]
		function local(x) {
		  let _k = 0
		  return [x == null, x != null, x === null, x !== undefined, x == undefined, x === true, x !== false,
		    x == 0, x != 1, x === 0, x !== 1, x == NaN, x === 'go', x !== 'go', 0 == x, null === x].map(Number).join('')
		}
		function up(x) {
		  return () => { let _j = 0; return [x == null, x === undefined, x !== null, x == 1, x === 0, 'go' === x].map(Number).join('') }
		}
		class O {
		  constructor(v) { this.v = v }
		  self() { let _k = 0; return [this.v == null, this.v !== null, this.v === 'go', this.v == 0, this.v !== 1].map(Number).join('') }
		}
		function prop(o) { let _k = 0; return [o.v === null, o.v != undefined, o.v === 'go', o.v === 1].map(Number).join('') }
		function tree(x) { let _k = 0; return [(x & 3) != 0, (x | 0) === 1, typeof x == 'string', -x == 0].map(Number).join('') }
		const out = []
		for (let round = 0; round < 3; round++) {
		  log = ''
		  const r = []
		  for (const v of vals) {
		    for (const f of [local, (x) => up(x)(), (x) => new O(x).self(), (x) => prop({ v: x }), tree]) {
		      try { r.push(f(v)) } catch (err) { r.push(err.constructor.name) }
		    }
		  }
		  out.push(r.join(',') + ' ' + log)
		}
		out[0] + ' | ' + (out[0] === out[1] && out[1] === out[2])`
	const want = "0101001111100110,001010,01011,0100,0001,0101001111100110,001010,01011,0100,0001,0101001000000100,001100,01000,0101,1100,0101001010100100,001000,01001,0100,0000,0101001110100110,001000,01011,0100,0011,0101001000100100,001100,01001,0100,1110,0101001110100110,001000,01011,0100,0011,0101001010101000,001001,01101,0110,0010,0101011000100100,001100,01001,0100,1100,0101000110100110,001000,01011,0100,0001,1011101010100101,100000,10001,1000,0001,1000101010100100,111000,11001,0000,0000,0101001110100110,001000,01011,0100,TypeError,0101001000100100,001100,01001,0100,TypeError,1001101010100100,101000,11001,0000,0000,0101001000100100,001100,01001,0100,1100,RangeError,RangeError,RangeError,0100,RangeError vvvvvvvvv | true"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		dda := r.NewObject()
		dda.MarkHTMLDDA()
		r.global.setOwnRaw(r.atoms.intern("dda"), Obj(dda), propDefault)
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
