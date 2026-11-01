package vm

import "testing"

// upvalueAddTestSrc adds to captured bindings, as values and as
// statements, what the tree tier's upvalue addition (zzz_zupvstr.go) joins
// or adds itself -- strings, from a local and a constant, an unpaired
// surrogate's halves, numbers -- and what it leaves to rmwOp: a string and
// a number either way round, objects with valueOf or toString, BigInts, a
// BigInt and a string, undefined, a symbol, which throws; an operand that
// assigns the binding before the addition; a string doubled past the
// longest, which throws; a constant, which throws. Three rounds of fresh
// bindings, each method with a local of its own, so that it is run on its
// tree rather than by pureCall.
const upvalueAddTestSrc = `
const out = []
function maker() {
  let s = '', n = 0, mixed = 1, big = 1n, order = 'a', obj = 'o', u
  const o = { valueOf() { return 5 }, toString() { return 'T' } }
  const sink = {
    push(x) { let _k = 0; return x !== null && (s += x), true },
    stmt(x) { let _k = 0; s += x; s += '-'; return s.length },
    num(x) { let _k = 0; n += x; return n },
    mix(x) { let _k = 0; return mixed += x },
    bigs(x) { let _k = 0; return typeof (big += x) + ':' + big },
    ord() { let _k = 0; order += (() => { order = 'z'; return 'c' })(); return order },
    objs(x) { let _k = 0; obj += x; return obj },
    undef(x) { let _k = 0; u += x; return u },
    get() { return s },
  }
  return sink
}
for (let r = 0; r < 3; r++) {
  const k = maker()
  for (let i = 0; i < 5; i++) k.push(i % 2 ? '<x>' : null)
  for (let i = 0; i < 3; i++) k.push('é' + i)
  k.push('\ud83d'); k.push('\ude00')
  out.push(k.get(), k.get().length, k.get().lastIndexOf('<x>'), k.stmt('y'), k.stmt(7), k.get())
  out.push(k.num(1), k.num(2.5), k.num('s'), k.num(1))
  out.push(k.mix(1), k.mix('a'), k.mix(2), k.mix({ toString() { return 'O' } }))
  out.push(k.bigs(2n), k.bigs('x'))
  try { k.bigs(1) } catch (e) { out.push(e.constructor.name) }
  out.push(k.ord(), k.objs({ valueOf() { return 1 } }), k.objs(Symbol.iterator.description), k.undef('u'), k.undef(1))
  try { k.objs(Symbol('q')) } catch (e) { out.push(e.constructor.name) }
}
let d = 'x'
function dbl() { let _k = 0; d += d; return d.length }
try { for (let i = 0; i < 40; i++) dbl() } catch (e) { out.push(e.constructor.name, d.length) }
function constUp() { const c = 'c'; return () => { let _k = 0; c += 'd'; return c } }
try { constUp()() } catch (e) { out.push(e.constructor.name) }
const res = out.join('|')
res
`

// upvalueAddTestWant is Node's answer.
const upvalueAddTestWant = "<x><x>é0é1é2😀|14|3|16|18|<x><x>é0é1é2😀y-7-|1|3.5|3.5s|3.5s1|2|2a|2a2|2a2O|bigint:3|string:3x|ac|o1|o1Symbol.iterator|undefinedu|undefinedu1|TypeError|<x><x>é0é1é2😀|14|3|16|18|<x><x>é0é1é2😀y-7-|1|3.5|3.5s|3.5s1|2|2a|2a2|2a2O|bigint:3|string:3x|ac|o1|o1Symbol.iterator|undefinedu|undefinedu1|TypeError|<x><x>é0é1é2😀|14|3|16|18|<x><x>é0é1é2😀y-7-|1|3.5|3.5s|3.5s1|2|2a|2a2|2a2O|bigint:3|string:3x|ac|o1|o1Symbol.iterator|undefinedu|undefinedu1|TypeError|RangeError|268435456|TypeError"

// An addition to a captured binding answers in the tree tier as in the
// interpreter, as Node answers.
func TestTreeUpvalueAdd(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, upvalueAddTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != upvalueAddTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, upvalueAddTestWant)
		}
	}
	SetTreeTier(true)
}
