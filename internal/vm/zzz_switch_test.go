package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// switchTestSrc runs switches whose cases are literals, all or some, on
// values of every kind: strings made at run time -- a concatenation, a
// join, a slice -- and non-ASCII ones, a surrogate pair and a lone
// surrogate, the empty string and a long string against a rope, numbers
// with +0 and -0, NaN, Infinity and fractions, 1 against "1", booleans,
// null and undefined, objects; duplicate selectors, the first winning;
// default first, in the middle and last, with cases after it; fall-through
// and break, continue in a loop, a body that throws inside try, one that
// returns through finally and a discriminant that throws inside try; a
// discriminant evaluated once (discs), a case not a literal, its
// evaluations counted (evals); switch (typeof x) with a name of no type
// and a duplicate; a generator's switch; forty string cases, sparse
// numbers and integers dense enough for an array. want is Node's answer.
const switchTestSrc = `const part = 'sec'
const long1 = 'x'.repeat(40), long2 = 'y'.repeat(40)
const vals = ['a', 'section', part + 'tion', ['sec', 'tion'].join(''), ('section?').slice(0, 7), 'été', '😀', '\ud800', '',
  'unknown', long1 + long2, 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyz', 0, -0, 1, '1', 1.5, 2, 3, 16, 17, -1, NaN, Infinity,
  true, false, null, undefined, {}, [], Symbol('a'), 1n, new String('a'), 'b', 'c', 'd', 'z', 'tpl', 40, 255, 256]
function s1(x) {
  let _k = 0
  switch (x) {
  case 'a': return 'A'
  case 'section': return 'S'
  case 'été': return 'E'
  case '😀': return 'F'
  case '\ud800': return 'L'
  case '': return 'Z'
  case 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy': return 'R'
  case ` + "`" + `tpl` + "`" + `: return 'T'
  case 'a': return 'dup'
  }
  return '-'
}
function s2(x) {
  let _k = 0, out = ''
  switch (x) {
  case 1: out += '1'
  case 2: out += '2'; break
  case 0: out += '0'; break
  case -0: out += 'm'; break
  case 1.5: out += 'h'; break
  case '1': out += 's'; break
  case 16: out += 'x'
  default: out += 'd'
  case 3: out += '3'; break
  case Infinity: out += 'i'
  }
  return out
}
function s3(x) {
  let _k = 0
  switch (x) {
  case true: return 't'
  case false: return 'f'
  case null: return 'n'
  case undefined: return 'u'
  case 0: return '0'
  case 'null': return 'N'
  default: return '-'
  }
}
let evals = 0
function sel(v) { evals++; return v }
function s4(x) {
  let _k = 0
  switch (x) {
  case 'a': return 'A'
  case sel('b'): return 'B'
  case 'c': return 'C'
  case 'a': return 'dup'
  case sel('d'): return 'D'
  case 1: return '1'
  default: return '-'
  }
}
function s5(x) {
  let _k = 0, n = 0
  for (let i = 0; i < 3; i++) {
    switch (x) {
    case 'a': n += 1; continue
    case 'section': n += 10; break
    case 2: n += 100
    case 3: n += 1000; break
    default: n += 7
    }
    n += 2
  }
  return n
}
function s6(x) {
  let _k = 0
  switch (typeof x) {
  case 'number': return 'n'
  case 'string': return 's'
  case 'number': return 'dup'
  case 'object': return 'o'
  case 'strnig': return '!'
  case 'function': return 'f'
  case 'undefined': return 'u'
  default: return '-'
  case 'symbol': return 'y'
  }
}
function s7(x) {
  let _k = 0, r = ''
  try {
    switch (x) {
    case 'a': r += 'A'; throw new Error('e')
    case 1: r += '1'; return r
    case null: r += 'n'
    }
  } catch (e) { r += 'c' } finally { r += 'f' }
  return r
}
let discs = 0
function disc(x) { discs++; return x }
function s8(x) {
  let _k = 0
  switch (disc(x)) {
  case 'a': case 'b': return 'AB'
  case 'c': return 'C'
  case 'section': return 'S'
  }
  return '-'
}
function s9(x) {
  let _k = 0
  switch (x) {
  case 'a': x = 'b'
  case 'b': return x + '!'
  }
  return '-'
}
function* g(x) {
  let _k = 0
  switch (x) {
  case 'a': yield 1; yield 2; break
  case 0: yield 3
  default: yield 4
  }
}
function sg(x) { let _k = 0, s = ''; for (const v of g(x)) s += v; return s }
function s10(x) {
  let _k = 0
  switch (x) {
  case 'break': return 1; case 'case': return 2; case 'catch': return 3; case 'class': return 4
  case 'const': return 5; case 'continue': return 6; case 'debugger': return 7; case 'default': return 8
  case 'delete': return 9; case 'do': return 10; case 'else': return 11; case 'export': return 12
  case 'extends': return 13; case 'finally': return 14; case 'for': return 15; case 'function': return 16
  case 'if': return 17; case 'import': return 18; case 'in': return 19; case 'instanceof': return 20
  case 'new': return 21; case 'return': return 22; case 'super': return 23; case 'switch': return 24
  case 'a': return 25; case 'b': return 26; case 'c': return 27; case 'd': return 28
  }
  return 0
}
function s11(x) {
  let _k = 0
  switch (x) {
  case 40: return 'a'; case 255: return 'b'; case 256: return 'c'; case 1e9: return 'd'; case -1: return 'e'
  case 0.5: return 'f'; case 3: return 'g'; case 7: return 'h'; case 8: return 'i'; case 9: return 'j'; case 10: return 'k'
  }
  return '-'
}
function s12(x) {
  let _k = 0
  switch (x) {
  case 3: return 'c'; case 0: return 'z'; case 1: return 'a'; case 2: return 'b'; case 16: return 'p'
  case -0: return 'dup'; case 17: return 'q'; case 1: return 'dup'; case -1: return 'm'
  }
  return '-'
}
const boom = { get k() { if (this.x === 'a') throw new Error('boom'); return this.x }, x: '' }
function s13(x) {
  let _k = 0
  boom.x = x
  try {
    switch (boom.k) {
    case 'a': return 'A'
    case 'b': return 'B'
    case 1: return '1'
    }
  } catch (e) { return 'caught:' + e.message }
  return '-'
}
const s = []
for (let round = 0; round < 3; round++) {
  const r = []
  evals = 0; discs = 0
  for (const x of vals) r.push([s1(x), s2(x), s3(x), s4(x), s5(x), s6(x), s7(x), s8(x), s9(x), sg(x), s10(x), s11(x), s12(x), s13(x)].join('.'))
  r.push(evals, discs)
  s.push(r.join(','))
}
s[0] + ' | ' + (s[0] === s[1] && s[1] === s[2])
`

const switchTestWant = "A.d3.-.A.3.s.Acf.AB.b!.12.25.-.-.caught:boom,S.d3.-.-.36.s.f.S.-.4.0.-.-.-,S.d3.-.-.36.s.f.S.-.4.0.-.-.-,S.d3.-.-.36.s.f.S.-.4.0.-.-.-,S.d3.-.-.36.s.f.S.-.4.0.-.-.-,E.d3.-.-.27.s.f.-.-.4.0.-.-.-,F.d3.-.-.27.s.f.-.-.4.0.-.-.-,L.d3.-.-.27.s.f.-.-.4.0.-.-.-,Z.d3.-.-.27.s.f.-.-.4.0.-.-.-,-.d3.-.-.27.s.f.-.-.4.0.-.-.-,R.d3.-.-.27.s.f.-.-.4.0.-.-.-,-.d3.-.-.27.s.f.-.-.4.0.-.-.-,-.0.0.-.27.n.f.-.-.34.0.-.z.-,-.0.0.-.27.n.f.-.-.34.0.-.z.-,-.12.-.1.27.n.1.-.-.4.0.-.a.1,-.s.-.-.27.s.f.-.-.4.0.-.-.-,-.h.-.-.27.n.f.-.-.4.0.-.-.-,-.2.-.-.3306.n.f.-.-.4.0.-.b.-,-.3.-.-.3006.n.f.-.-.4.0.g.c.-,-.xd3.-.-.27.n.f.-.-.4.0.-.p.-,-.d3.-.-.27.n.f.-.-.4.0.-.q.-,-.d3.-.-.27.n.f.-.-.4.0.e.m.-,-.d3.-.-.27.n.f.-.-.4.0.-.-.-,-.i.-.-.27.n.f.-.-.4.0.-.-.-,-.d3.t.-.27.-.f.-.-.4.0.-.-.-,-.d3.f.-.27.-.f.-.-.4.0.-.-.-,-.d3.n.-.27.o.nf.-.-.4.0.-.-.-,-.d3.u.-.27.u.f.-.-.4.0.-.-.-,-.d3.-.-.27.o.f.-.-.4.0.-.-.-,-.d3.-.-.27.o.f.-.-.4.0.-.-.-,-.d3.-.-.27.y.f.-.-.4.0.-.-.-,-.d3.-.-.27.-.f.-.-.4.0.-.-.-,-.d3.-.-.27.o.f.-.-.4.0.-.-.-,-.d3.-.B.27.s.f.AB.b!.4.26.-.-.B,-.d3.-.C.27.s.f.C.-.4.27.-.-.-,-.d3.-.D.27.s.f.-.-.4.28.-.-.-,-.d3.-.-.27.s.f.-.-.4.0.-.-.-,T.d3.-.-.27.s.f.-.-.4.0.-.-.-,-.d3.-.-.27.n.f.-.-.4.0.a.-.-,-.d3.-.-.27.n.f.-.-.4.0.b.-.-,-.d3.-.-.27.n.f.-.-.4.0.c.-.-,78,41 | true"

// A run of a switch's literal cases is one lookup (zzz_switch.go),
// which answers as the interpreter's chain of comparisons does, and as
// Node does. Each function has a local of its own, so that it is run on
// its tree rather than by pureCall.
func TestSwitchLookups(t *testing.T) {
	for _, c := range compileForTest(t, switchTestSrc).Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		if name := c.Fn.Name; len(name) >= 2 && name[0] == 's' && name[1] != 'e' {
			if buildTree(c.Fn) == nil {
				t.Errorf("%s is not built as a tree", name)
			}
		}
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, switchTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != switchTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, switchTestWant)
		}
	}
	SetTreeTier(true)
}
