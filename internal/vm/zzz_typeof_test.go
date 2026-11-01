package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// typeofTestSrc tests typeof's answer of every kind of value against each
// type's name and names of none, ===, !==, == and != either way round, as a
// branch and as a value, of a local, an upvalue, a call and a name nothing
// declares; and a typeof whose answer is kept. want is Node's answer, with
// document.all (dda) undefined there, which typeof answers alike.
const typeofTestSrc = `
const dda = globalThis.dda
const vals = [undefined, null, true, false, 0, -0, NaN, 1.5, '', 'str', Symbol('s'), 1n, 2n ** 70n,
  {}, [], function f() {}, () => 1, class K {}, Math.max, Math.max.bind(null), async function () {}, function* () {},
  new Proxy(function () {}, {}), new Proxy({}, {}), new String('s'), new Date(0), /r/, dda]
function a(x) {
  let _k = 0, s = ''
  if (typeof x === 'undefined') s += 'u'
  if (typeof x === 'object') s += 'o'
  if (typeof x === 'boolean') s += 'b'
  if (typeof x === 'number') s += 'n'
  if (typeof x === 'string') s += 's'
  if (typeof x === 'symbol') s += 'y'
  if (typeof x === 'bigint') s += 'g'
  if (typeof x === 'function') s += 'f'
  if (typeof x === 'strnig') s += '!'
  if (typeof x !== 'object') s += 'O'
  if (typeof x != 'function') s += 'F'
  if (typeof x == 'number') s += 'N'
  if ('string' === typeof x) s += 'S'
  if ('undefined' !== typeof x) s += 'U'
  if (typeof x !== 'Number') s += '?'
  return s
}
function b(x) {
  let _k = 0
  return [typeof x === 'undefined', typeof x == 'object', typeof x !== 'function', 'bigint' === typeof x,
    'symbol' != typeof x, typeof x === '', typeof x !== 'undefined ', typeof x === 'boolean' && typeof x !== 'number']
    .map(Number).join('')
}
let up = 0
function c(x) {
  let _k = 0
  const t = () => { let _j = 0; return (typeof x === 'string' ? 's' : '') + (typeof x === 'object' ? 'o' : '') + (typeof up === 'number' ? 'n' : '') }
  return t()
}
let calls = 0
function side(x) { calls++; return x }
function d(x) {
  let _k = 0, s = ''
  if (typeof side(x) === 'strnig') s += '!'
  if (typeof side(x) !== 'nope') s += 'p'
  s += typeof side(x) === 'function' ? 'f' : '-'
  s += (typeof side(x) == 'whatever') ? '!' : '-'
  return s
}
function e() {
  let _k = 0
  return [typeof undeclaredXyz === 'undefined', typeof undeclaredXyz !== 'undefined', typeof undeclaredXyz,
    typeof undeclaredXyz == 'object'].join()
}
function g(x) {
  let _k = 0, n = 0
  for (let i = 0; i < 3; i++) { if (typeof x === 'number') n++; while (typeof x === 'string' && n < 5) n++ }
  return n
}
function h(x) { let _k = 0; const t = typeof x; return t + ':' + (t === 'object') }
const s = []
for (let round = 0; round < 3; round++) {
  const r = []
  calls = 0
  for (const x of vals) r.push(a(x) + '/' + b(x) + '/' + c(x) + '/' + d(x) + '/' + g(x) + '/' + h(x))
  r.push(e(), calls)
  s.push(r.join(','))
}
s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
`

const typeofTestWant = "uOF?/10101010/n/p--/0/undefined:false,oFU?/01101010/on/p--/0/object:true,bOFU?/00101011/n/p--/0/boolean:false,bOFU?/00101011/n/p--/0/boolean:false,nOFNU?/00101010/n/p--/3/number:false,nOFNU?/00101010/n/p--/3/number:false,nOFNU?/00101010/n/p--/3/number:false,nOFNU?/00101010/n/p--/3/number:false,sOFSU?/00101010/sn/p--/5/string:false,sOFSU?/00101010/sn/p--/5/string:false,yOFU?/00100010/n/p--/0/symbol:false,gOFU?/00111010/n/p--/0/bigint:false,gOFU?/00111010/n/p--/0/bigint:false,oFU?/01101010/on/p--/0/object:true,oFU?/01101010/on/p--/0/object:true,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,fOU?/00001010/n/pf-/0/function:false,oFU?/01101010/on/p--/0/object:true,oFU?/01101010/on/p--/0/object:true,oFU?/01101010/on/p--/0/object:true,oFU?/01101010/on/p--/0/object:true,uOF?/10101010/n/p--/0/undefined:false,true,false,undefined,false,112 | true"

// typeof's tests (zzz_typeof.go) answer as the interpreter does, and
// as Node does, for every kind of value, document.all among them; a test
// against a name of no type is false with its operand still evaluated
// (calls counts them); typeof of a name nothing declares throws nothing.
// Each function has a local of its own, so that it is run on its tree
// rather than by pureCall.
func TestTypeofTests(t *testing.T) {
	for _, c := range compileForTest(t, typeofTestSrc).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		if name := c.Fn.Name; len(name) == 1 {
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", name)
			}
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		dda := r.NewObject()
		dda.MarkHTMLDDA()
		r.global.setOwnRaw(r.atoms.intern("dda"), Obj(dda), propDefault)
		v, err := r.Run(compileForTest(t, typeofTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != typeofTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, typeofTestWant)
		}
	}
	SetTreeTier(true)
}
