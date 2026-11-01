package vm

import "testing"

// pushTestSrc pushes and pops in a pure body's fast path (pureElemOp):
// one element of every kind and none, two, four at once, past a few
// growths and 5000 elements, onto an array with a hole; and what the
// path leaves to the built-in -- a frozen array, a length not writable,
// an array not extensible, an index with a setter on Array.prototype --
// and pops to empty and past it. Each function has a local of its own,
// so that it is run on its tree rather than by pureCall.
const pushTestSrc = `
const out = []
const O = { v: 1 }
function fill(a, n) { let _k = 0; for (let i = 0; i < n; i++) a.push(i % 3 ? i : (i % 2 ? O : 's' + i)); return a.length }
function many(a) { let _k = 0; return [a.push(), a.push(1), a.push(2, 3), a.push(O, 'x', null, undefined)] }
function drain(a) { let _k = 0, s = 0; while (a.length) { const v = a.pop(); s += typeof v === 'number' ? v : 1 } return s + ':' + a.pop() }
function show(a) { return a.map(v => v === O ? 'O' : v === null ? 'null' : String(v)).join() }
for (let r = 0; r < 3; r++) {
  const a = []
  out.push(fill(a, 300), a.length, show(a.slice(0, 7)), show(a.slice(-4)), drain(a))
  const b = [1, , 3]
  out.push(many(b).join(), show(b), b.length)
  const c = Object.freeze([1, 2])
  try { c.push(3) } catch (e) { out.push(e.constructor.name, c.length) }
  const d = [1, 2]
  Object.defineProperty(d, 'length', { writable: false })
  try { d.push(3) } catch (e) { out.push(e.constructor.name, d.length) }
  const e = [1]
  Object.preventExtensions(e)
  try { e.push(2) } catch (x) { out.push(x.constructor.name, e.length) }
  const f = [9, 8]
  out.push(f.pop(), f.pop(), f.pop(), f.length)
  const big = []
  for (let i = 0; i < 5000; i++) big.push(i)
  out.push(big.length, big[4999], big.pop(), big.length)
}
Object.defineProperty(Array.prototype, 2, { set(v) { out.push('setter ' + v) }, configurable: true })
const g = [0, 1]
out.push(g.push('two'), g.length, g[2])
delete Array.prototype[2]
const res = out.join('|')
res
`

// pushTestWant is Node's answer.
const pushTestWant = "300|300|s0,1,2,O,4,5,s6|296,O,298,299|30100:undefined|3,4,6,10|1,,3,1,2,3,O,x,null,undefined|10|TypeError|2|TypeError|2|TypeError|1|8|9||0|5000|4999|4999|4999|300|300|s0,1,2,O,4,5,s6|296,O,298,299|30100:undefined|3,4,6,10|1,,3,1,2,3,O,x,null,undefined|10|TypeError|2|TypeError|2|TypeError|1|8|9||0|5000|4999|4999|4999|300|300|s0,1,2,O,4,5,s6|296,O,298,299|30100:undefined|3,4,6,10|1,,3,1,2,3,O,x,null,undefined|10|TypeError|2|TypeError|2|TypeError|1|8|9||0|5000|4999|4999|4999|setter two|3|3|"

// push and pop answer in the tree tier as in the interpreter, as Node
// answers.
func TestPurePushPop(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, pushTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != pushTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, pushTestWant)
		}
	}
	SetTreeTier(true)
}
