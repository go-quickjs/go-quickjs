package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regexpLeafTestSrc calls RegExp.prototype's exec and test, native leaves
// where the match runs nothing and cannot fail (zzz_regexpleaf.go), and
// everywhere around them: patterns of one class, folded, negated, read as
// code points, and of a literal, global, sticky or neither, and one the
// matcher matches; ASCII subjects and others; arguments of every kind,
// one whose conversion is counted and a symbol; lastIndex an object
// (counted), a string, negative, past the subject, NaN, a fraction;
// frozen RegExps and a lastIndex that cannot be written; receivers that
// are no RegExp; test of a RegExp with an exec of its own, of a
// subclass's, and with the prototype's replaced; the legacy statics; the
// d flag; a subject past ASCII too long for the matcher's budget's
// check. loop calls a leaf's exec, a global pattern's test and a literal's
// test in a loop of its own, and escs an HTML escaper's exec from a
// function its loop calls. regexpLeafTestWant is Node's answer
// (v26.10.0).
const regexpLeafTestSrc = `var calls = 0
function g(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
function show(m) { return m === null ? 'null' : m.index + ':' + m[0].length + '/' + m[0].charCodeAt(0) + ':' + m.length + ':' + m.input.length + ':' + (m.groups === undefined) }
function table() {
  var o = []
  var res = [/["'&<>]/, /["'&<>]/g, /[<>]/y, /ab/, /ab/g, /ab/y, /[é<]/u, /[A-Z]/i, /[^\w ]/, /a+b/, /[k]/iu, /\W/iu]
  var subjects = ['plain', 'Tom & Jerry', '<a>', '', 'xabab', 'café <b>', '😀<', 'ÉTÉ', 'Kelvin K ſ']
  for (var s of subjects) {
    for (var re of res) {
      re.lastIndex = 1
      o.push(show(re.exec(s)) + ',' + re.lastIndex + ',' + re.test(s) + ',' + re.lastIndex)
    }
  }
  var args = [42, 1.5, true, undefined, null, { toString() { calls++; return '<' } }, Symbol('s'), 10n]
  for (var a of args) o.push(g(() => show(/[<1nu]/.exec(a))) + ',' + g(() => /[<1nu]/g.test(a)))
  o.push(show(/[<un]/.exec()) + ',' + /d/.test())
  var lis = [{ valueOf() { calls++; return 2 } }, '3', -1, 2 ** 53, NaN, 1e300, 0.5, 5]
  for (var li of lis) {
    var r = /[<>]/g; r.lastIndex = li; o.push(g(() => show(r.exec('<a><b>'))) + ',' + r.lastIndex)
    var t = /[<>]/; t.lastIndex = li; o.push(g(() => t.test('<a>')) + ',' + String(t.lastIndex))
  }
  var fz = Object.freeze(/[<>]/g); o.push(g(() => fz.exec('<')) + ',' + g(() => fz.test('x')) + ',' + fz.lastIndex)
  var fn = Object.freeze(/[<>]/); o.push(show(fn.exec('a<')) + ',' + fn.test('a'))
  var nw = /[<>]/y; Object.defineProperty(nw, 'lastIndex', { value: 0, writable: false }); o.push(g(() => nw.exec('<')) + ',' + g(() => nw.test('a')))
  o.push(g(() => RegExp.prototype.exec.call({}, 'a')) + ',' + g(() => RegExp.prototype.test.call({ exec() { calls++; return null } }, 'a')) + ',' +
    g(() => RegExp.prototype.exec.call(RegExp.prototype, 'a')) + ',' + g(() => RegExp.prototype.test.call(1, 'a')))
  var own = /[<>]/; own.exec = function (s) { calls++; return null }; o.push(own.test('<') + ',' + show(RegExp.prototype.exec.call(own, '<')))
  class R extends RegExp { exec(s) { calls++; return super.exec(s) } }
  var sub = new R('[<>]'); o.push(sub.test('a<') + ',' + show(sub.exec('<')) + ',' + show(RegExp.prototype.exec.call(sub, 'b>')))
  var orig = RegExp.prototype.exec; RegExp.prototype.exec = function (s) { calls++; return orig.call(this, s) }
  o.push(/[<>]/.test('<') + ',' + show(/[<>]/.exec('<'))); RegExp.prototype.exec = orig;
  ;/[<>]/.exec('ab<cd'); o.push(RegExp.lastMatch + RegExp.leftContext + RegExp.rightContext + RegExp.input)
  ;/cd/.test('xxcdyy'); o.push(RegExp.lastMatch + RegExp.rightContext + RegExp.leftContext)
  o.push(JSON.stringify(/[<>]/d.exec('a<').indices))
  var long = 'é'.repeat(5000) + '<'; o.push(show(/[<]/u.exec(long)) + ',' + /[<]/.test(long))
  return o.join(' ') + ' calls=' + calls
}
var esre = /["'&<>]/
function esc(n) {
  if (typeof n == 'boolean' || typeof n == 'number' || typeof n == 'bigint') return '' + n
  n = '' + n
  var e = esre.exec(n)
  if (e) {
    var t = '', l, r = 0
    for (l = e.index; l < n.length; l++) {
      switch (n.charCodeAt(l)) {
      case 34: e = '&quot;'; break
      case 38: e = '&amp;'; break
      case 39: e = '&#x27;'; break
      case 60: e = '&lt;'; break
      case 62: e = '&gt;'; break
      default: continue
      }
      r !== l && (t += n.slice(r, l)), r = l + 1, t += e
    }
    n = r !== l ? t + n.slice(r, l) : t
  }
  return n
}
function escs(n) {
  var s = '', words = ['article', 'Tom & Jerry', '<a title="x">sale</a>', '', 42, true, 'plain text', "q's"]
  for (var i = 0; i < n; i++) s = esc(words[i % 8]) + s.length
  return s
}
function loop(n) {
  let _k = 0
  var s = 0, rs = /["'&<>]/, rg = /[<>]/g, rt = /["&]/, words = ['plain', 'a<b', '"q"', 'x&y', 'none', 'ab']
  for (var i = 0; i < n; i++) {
    var w = words[i % 6], m = rs.exec(w)
    s += (m ? m.index + 1 : 0) + (rg.test(w) ? 10 * rg.lastIndex : 0) + (/ab/.test(w) ? 100 : 0) + (rt.test(w) ? 1000 : 0)
    _k++
  }
  return s + _k
}
table() + ' | ' + loop(50) + ' | ' + loop(200) + ' | ' + escs(50) + ' | ' + escs(200)
`

const regexpLeafTestWant = `null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 0:1/112:1:5:true,1,true,1 null,1,false,1 null,1,false,1 null,1,false,1 null,1,false,1 4:1/38:1:11:true,1,true,1 4:1/38:1:11:true,5,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 0:1/84:1:11:true,1,true,1 4:1/38:1:11:true,1,true,1 null,1,false,1 null,1,false,1 3:1/32:1:11:true,1,true,1 0:1/60:1:3:true,1,true,1 2:1/62:1:3:true,3,false,0 null,0,true,1 null,1,false,1 null,0,false,0 null,0,false,0 0:1/60:1:3:true,1,true,1 1:1/97:1:3:true,1,true,1 0:1/60:1:3:true,1,true,1 null,1,false,1 null,1,false,1 0:1/60:1:3:true,1,true,1 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 null,1,false,1 null,1,false,1 null,1,false,1 null,1,false,1 null,1,false,1 null,1,false,1 null,0,false,0 null,0,false,0 1:2/97:1:5:true,1,true,1 1:2/97:1:5:true,3,true,5 1:2/97:1:5:true,3,true,5 null,1,false,1 0:1/120:1:5:true,1,true,1 null,1,false,1 1:2/97:1:5:true,1,true,1 null,1,false,1 null,1,false,1 5:1/60:1:8:true,1,true,1 5:1/60:1:8:true,6,true,8 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 3:1/233:1:8:true,1,true,1 0:1/99:1:8:true,1,true,1 3:1/233:1:8:true,1,true,1 null,1,false,1 null,1,false,1 3:1/233:1:8:true,1,true,1 2:1/60:1:3:true,1,true,1 2:1/60:1:3:true,3,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 2:1/60:1:3:true,1,true,1 null,1,false,1 0:1/55357:1:3:true,1,true,1 null,1,false,1 null,1,false,1 0:2/55357:1:3:true,1,true,1 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 1:1/84:1:3:true,1,true,1 0:1/201:1:3:true,1,true,1 null,1,false,1 null,1,false,1 0:1/201:1:3:true,1,true,1 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 null,0,false,0 null,0,false,0 null,1,false,1 0:1/75:1:10:true,1,true,1 9:1/383:1:10:true,1,true,1 null,1,false,1 0:1/75:1:10:true,1,true,1 6:1/32:1:10:true,1,true,1 null,false 0:1/49:1:3:true,true 2:1/117:1:4:true,true 0:1/117:1:9:true,true 0:1/110:1:4:true,true 0:1/60:1:1:true,true TypeError,TypeError 0:1/49:1:2:true,true 0:1/117:1:9:true,true 2:1/62:1:6:true,3 true,[object Object] 3:1/60:1:6:true,4 true,3 0:1/60:1:6:true,1 true,-1 null,0 true,9007199254740992 0:1/60:1:6:true,1 true,NaN null,0 true,1e+300 0:1/60:1:6:true,1 true,0.5 5:1/62:1:6:true,6 true,5 TypeError,TypeError,0 1:1/60:1:2:true,false TypeError,TypeError TypeError,false,TypeError,TypeError false,0:1/60:1:1:true true,0:1/60:1:1:true,1:1/62:1:2:true true,0:1/60:1:1:true <abcdab<cd cdyyxx [[1,2]] 5000:1/60:1:5001:true,true calls=10 | 17072 | 70347 | Tom &amp; Jerry9 | q&#x27;s11`

// RegExp.prototype's exec and test are native leaves (zzz_regexpleaf.go),
// applied without a frame where nothing they do runs script, throws or
// writes lastIndex: each answer is Node's, in the tree tier and in the
// interpreter, and the check passes exactly where it says.
func TestRegExpLeaves(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, regexpLeafTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpLeafTestWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, regexpLeafTestWant)
		}
	}
	SetTreeTier(true)
	r := New(Config{})
	defer r.Close()
	if fd := r.regexpExecFn.fn(); fd.elemOp != elemRegExpExec || leafChecks[elemRegExpExec] == nil {
		t.Fatal("exec is not a leaf")
	}
	if fd := r.regexpTestFn.fn(); fd.elemOp != elemRegExpTest || leafChecks[elemRegExpTest] == nil {
		t.Fatal("test is not a leaf")
	}
	// Each case is a receiver and an argument, made by a script, and
	// whether exec's check and test's pass on them.
	cases := []struct {
		recv, arg  string
		exec, test bool
	}{
		{`/["'&<>]/`, `'plain'`, true, true},
		{`/["'&<>]/`, `'café <b>'`, true, true},
		{`/[<>]/i`, `42`, true, true},
		{`/ab/`, `'xab'`, true, true},
		{`/ab/`, `'éab'`, true, true},
		{`/[é<]/u`, `'plain'`, true, true},
		{`/[<>]/`, `undefined`, true, true},
		{`/[<>]/`, `null`, true, true},
		{`/[<>]/`, `true`, true, true},
		{`(function () { var x = /[<>]/; x.lastIndex = 1.5; return x })()`, `'a'`, true, true},
		// The matcher's.
		{`/[é<]/u`, `'café'`, false, false},
		{`/a+b/`, `'aab'`, false, false},
		{`/ab/y`, `'ab'`, false, false},
		{`/(a)/`, `'a'`, false, false},
		// lastIndex written, or converted by a call.
		{`/[<>]/g`, `'a'`, false, false},
		{`/[<>]/y`, `'a'`, false, false},
		{`(function () { var x = /[<>]/; x.lastIndex = { valueOf() { return 0 } }; return x })()`, `'a'`, false, false},
		{`(function () { var x = /[<>]/; x.lastIndex = '0'; return x })()`, `'a'`, false, false},
		// Arguments converted by a call, or thrown on.
		{`/[<>]/`, `({ toString() { return '<' } })`, false, false},
		{`/[<>]/`, `Symbol()`, false, false},
		{`/[<>]/`, `1n`, false, false},
		// Receivers no RegExp, and tests that call an exec of the script's.
		{`({})`, `'a'`, false, false},
		{`RegExp.prototype`, `'a'`, false, false},
		{`(function () { var x = /[<>]/; x.exec = function () { return null }; return x })()`, `'a'`, true, false},
		{`new (class extends RegExp {})('[<>]')`, `'a'`, true, false},
	}
	for _, c := range cases {
		recv, err := r.Run(compileForTest(t, c.recv))
		if err != nil {
			t.Fatal(err)
		}
		a, err := r.Run(compileForTest(t, c.arg))
		if err != nil {
			t.Fatal(err)
		}
		args := []Value{a}
		if got := leafChecks[elemRegExpExec](r, recv, args); got != c.exec {
			t.Errorf("exec's check on %s of %s: %v, want %v", c.recv, c.arg, got, c.exec)
		}
		if got := leafChecks[elemRegExpTest](r, recv, args); got != c.test {
			t.Errorf("test's check on %s of %s: %v, want %v", c.recv, c.arg, got, c.test)
		}
	}
	// With nothing to match, the subject is undefined's string.
	if recv, _ := r.Run(compileForTest(t, `/[<>]/`)); !leafChecks[elemRegExpExec](r, recv, nil) {
		t.Error("exec's check of no argument fails")
	}
	_ = bytecode.OpCall
}
