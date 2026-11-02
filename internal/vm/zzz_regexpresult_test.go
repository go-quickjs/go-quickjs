package vm

import (
	"runtime/debug"
	"testing"
)

// matchResultTestSrc makes exec's arrays of patterns of no group to
// eighteen, groups that take part, are empty or not, and patterns of them
// global, sticky, unicode, folded and with the d flag, on ASCII subjects and
// others, empty, whole and past a pair; patterns whose groups take part in
// turn, are the whole subject or named; and arrays then changed. table
// hashes what each array is -- its elements, undefined told from empty,
// index, input, its own keys in order, their attributes, groups and indices
// -- with lastIndex and the legacy statics, and says where it differs from
// the array of exec called with a String object, which the built-in makes
// its own way (buildMatchResult). Exec reads every element, as Octane's
// RegExp does, and run hashes what it read in a loop. matchResultTestWant
// is Node's answer (v26.10.0).
const matchResultTestSrc = `function dump(m) {
  if (m === null) return 'null'
  var out = []
  for (var i = 0; i < m.length; i++) out.push(m[i] === undefined ? 'U' : JSON.stringify(m[i]))
  var d = Object.getOwnPropertyDescriptor(m, 'index'), d0 = Object.getOwnPropertyDescriptor(m, 0)
  var attrs = [d.writable, d.enumerable, d.configurable, d0.writable, d0.enumerable, d0.configurable, Object.isExtensible(m), Array.isArray(m)].map(Number).join('')
  var g = m.groups === undefined ? 'U' : JSON.stringify(m.groups) + Object.getPrototypeOf(m.groups)
  var ind = m.indices === undefined ? '' : ' d=' + JSON.stringify(m.indices) + (m.indices.groups ? JSON.stringify(m.indices.groups) : '')
  return '[' + out.join(',') + '] i=' + m.index + ' in=' + m.input.length + ' k=' + Object.getOwnPropertyNames(m) + ' a=' + attrs + ' g=' + g + ind
}
function hash(h, s) {
  for (var i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619) >>> 0
  return h
}
function groups(n, opt) {
  var p = ''
  for (var i = 0; i < n; i++) p += opt && i % 3 == 1 ? '(x)?' : i % 4 == 2 ? '()' : '(\\w)'
  return p
}
var subjects = ['abcdefghijklmnopqrstuvwxyz0123', 'ab', '', '\xe91bcdefghijklmnopqrstuvwxyz\xe9', '\ud83d\ude00abcdefghijklmnopqrstuv', 'abcdefghijklmnopqrstuvwxyz0123\ud800']
var res = []
for (var n = 0; n <= 18; n++) for (var opt = 0; opt < 2; opt++) for (var flags of ['', 'g', 'y', 'u', 'd', 'i']) res.push(new RegExp(groups(n, opt), flags))
res.push(/^(.*)$/, /(a)|(b)/, /(?:)/, /((((((a))))))/, /(?<x>a)(?<y>b)?(?<z>c)/, /(?<x>a)|(?<x>b)/, /(\w+)(\d)?(.)(.)(.)/,
  /(((\w+):\/\/)([^\/:]*)(:(\d+))?)?([^#?]*)(\?([^#]*))?(#(.*))?/, /(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)/)
subjects.push('abc', 'a', 'b', 'ac', 'uggc://jjj.snprobbx.pbz/ybtva.cuc', 'uggc://n.o:80/c?d#r', 'xyz')
// table: every pattern on every subject, its hash and how many, and where
// exec of the string differs from exec of a String object, which is the
// built-in's own path.
function table() {
  var h = 2166136261, k = 0, bad = []
  for (var re of res) for (var s of subjects) {
    re.lastIndex = 1
    var a = dump(re.exec(s)) + ' li=' + re.lastIndex + ' lm=' + RegExp.lastMatch + RegExp['$1'] + RegExp.lastParen
    re.lastIndex = 1
    var b = dump(re.exec(new String(s))) + ' li=' + re.lastIndex + ' lm=' + RegExp.lastMatch + RegExp['$1'] + RegExp.lastParen
    if (a !== b) bad.push(re + ' ' + JSON.stringify(s) + ': ' + a + ' vs ' + b)
    h = hash(h, a); k++
  }
  var m = /(a)(b)(c)(d)(e)(f)/.exec('xabcdefy')
  m.push('p'); m[0] = 'q'; m.length = 3; h = hash(h, dump(m) + ' ' + m.join('+'))
  var m2 = /(a)(b)?/.exec('ab'); m2.extra = 1; m2.index = 9; h = hash(h, dump(m2) + Object.keys(m2))
  return h + ' ' + k + (bad.length ? ' ' + bad.join(' | ') : '')
}
// Exec reads every element, as Octane's RegExp does; run calls it in a
// loop, over patterns of each size, and hashes what it read.
function Exec(re, string) {
  var sum = 0, t = ''
  re.lastIndex = 0
  var array = re.exec(string)
  if (array) {
    for (var i = 0; i < array.length; i++) {
      var substring = array[i]
      if (substring) sum += substring.length, t += substring
      else t += substring === undefined ? 'U' : 'E'
    }
    t += array.index + ',' + array.input.length + (array.groups ? JSON.stringify(array.groups) : '')
  }
  return t + sum
}
function run(k) {
  var h = 2166136261
  for (var i = 0; i < k; i++) h = hash(h, Exec(res[i % res.length], subjects[i % subjects.length]))
  return h
}
table() + " " + run(1000) + " " + run(3000)
`

const matchResultTestWant = "2661856178 3081 1410175481 853667658"

// A match's array and its strings are made together (zzz_regexpresult.go):
// each answer is Node's, in the tree tier and in the interpreter, with the
// collector idle and running.
func TestRegExpMatchResults(t *testing.T) {
	for _, tree := range []bool{true, false} {
		for _, idle := range []bool{true, false} {
			SetTreeTier(tree)
			gc := debug.SetGCPercent(-1)
			if !idle {
				debug.SetGCPercent(1)
			}
			r := New(Config{})
			v, err := r.Run(compileForTest(t, matchResultTestSrc))
			r.Close()
			debug.SetGCPercent(gc)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != matchResultTestWant {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, matchResultTestWant)
			}
		}
	}
	SetTreeTier(true)
}
