package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// strEqTestSrc branches on === and !== of a local, a tree, an upvalue and a
// loop's test with string constants: the constant, a rope of its length
// with its code units and one with others, strings of other lengths,
// non-ASCII ones, the empty string, a String object, a symbol, a number,
// null and undefined. want is Node's answer.
const strEqTestSrc = `const lit = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyy'
const vals = [lit, 'x'.repeat(40) + 'y'.repeat(24), 'x'.repeat(40) + 'y'.repeat(23) + 'z',
  'x'.repeat(40) + 'y'.repeat(23), 'x', 'été', 'ete', 'et\u00e9', '', 'section', 'sectioN',
  'sec' + 'tion', 1, null, undefined, {}, new String('section'), Symbol('section')]
// Ropes made anew for each call, which a comparison flattens.
function fresh(i) { return i === 1 ? 'x'.repeat(40) + 'y'.repeat(24) : i === 2 ? 'x'.repeat(40) + 'y'.repeat(23) + 'z' : vals[i] }
function a(x) { let _k = 0, s = ''; if (x === 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyy') s += 'a'; if (x !== 'section') s += 'b'; if ('été' === x) s += 'c'; if (x === '') s += 'd'; return s }
function b(i) { let _k = 0, s = ''; if (fresh(i) === 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyy') s += 'a'; if (fresh(i) !== 'section') s += 'b'; if ('été' === fresh(i)) s += 'c'; if (fresh(i) === '') s += 'd'; return s }
function c(x) { let _k = 0; const t = () => { let _j = 0; return (x === 'section' ? 'e' : '') + (x !== 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyy' ? 'f' : '') }; return t() }
function d(x) { let _k = 0, n = 0; while (x !== 'sectioN' && n < 3) n++; return n }
const s = []
for (let round = 0; round < 3; round++) {
  const r = []
  for (let i = 0; i < vals.length; i++) r.push(a(fresh(i)) + '/' + b(i) + '/' + c(fresh(i)) + '/' + d(fresh(i)))
  s.push(r.join(','))
}
s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
`

const strEqTestWant = "ab/ab//3,ab/ab//3,b/b/f/3,b/b/f/3,b/b/f/3,bc/bc/f/3,b/b/f/3,b/b/f/3,bd/bd/f/3,//ef/3,b/b/f/0,//ef/3,b/b/f/3,b/b/f/3,b/b/f/3,b/b/f/3,b/b/f/3,b/b/f/3 | true"

// A strict equality branch with a string constant (strEqJump) answers as
// the interpreter does, and as Node does. Each function has a local of its
// own, so that it is run on its tree rather than by pureCall.
func TestStrictEqualityWithStrings(t *testing.T) {
	for _, c := range compileForTest(t, strEqTestSrc).Constants {
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
		v, err := r.Run(compileForTest(t, strEqTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != strEqTestWant {
			t.Errorf("tree tier %v: got %s, want %s", tree, got, strEqTestWant)
		}
	}
	SetTreeTier(true)
}
