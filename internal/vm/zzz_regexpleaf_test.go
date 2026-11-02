package vm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

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
		// The matcher's, of a string: a match that fails errs with nothing
		// made, and is made again with a frame (errRegExpLeafAgain).
		{`/[é<]/u`, `'café'`, true, true},
		{`/a+b/`, `'aab'`, true, true},
		{`/(a)/`, `'a'`, true, true},
		// The matcher's, of another primitive, which the built-in converts
		// and matches itself; and a sticky pattern's.
		{`/a+b/`, `42`, false, false},
		{`/(a)/`, `undefined`, false, false},
		{`/ab/y`, `'ab'`, false, false},
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
	// The built-ins' one-pass function answers as their own does, and
	// leaves the legacy statics as it does, whatever it is given.
	recvs, err := r.Run(compileForTest(t, `[/["'&<>]/, /["'&<>]/g, /[<>]/y, /ab/, /ab/g, /ab/y, /[é<]/u, /[A-Z]/i, /a+b/, /(a)(b)?/,
		/[<]/d, Object.freeze(/[<>]/), (function () { var x = /[<>]/; x.lastIndex = { valueOf() { return 0 } }; return x })(),
		(function () { var x = /[<>]/; x.lastIndex = 3; return x })(), (function () { var x = /[<>]/; x.exec = function () { return null }; return x })(),
		new (class extends RegExp {})('[<>]'), {}, 'str', RegExp.prototype, Object.freeze(/[<>]/g),
		(function () { var x = /a/y; Object.defineProperty(x, 'lastIndex', { value: 1, writable: false }); return x })(),
		(function () { var x = /b/g; x.lastIndex = 1e300; return x })(), (function () { var x = /\u{1F600}/gu; x.lastIndex = 1; return x })()]`))
	if err != nil {
		t.Fatal(err)
	}
	subjects, err := r.Run(compileForTest(t, `['', 'plain', 'Tom & Jerry', '<a>', 'xabab', 'café <b>', 'ab'.repeat(40) + '<', 42, null, undefined, '\u{1F600}\u{1F600}b', 'aaab']`))
	if err != nil {
		t.Fatal(err)
	}
	statics := compileForTest(t, `RegExp.lastMatch + '|' + RegExp.input + '|' + RegExp.leftContext`)
	// A match of its own before each call, whose statics each call's own
	// replace: a global pattern's, which the built-in's own function makes.
	reset := compileForTest(t, `new RegExp('q', 'g').exec('qq')`)
	describe := func(v Value, err error) string {
		if err != nil {
			return "throws " + err.Error()
		}
		if !v.IsObject() {
			return r.describe(v)
		}
		o := v.Object()
		idx, _ := r.getValueProp(v, r.atoms.intern("index"))
		m0, _ := r.getValueProp(v, r.atoms.intern("0"))
		n, _ := r.getValueProp(v, atomLength)
		return r.describe(idx) + ":" + r.describe(m0) + ":" + r.describe(n) + ":" + strconv.Itoa(int(o.class))
	}
	for _, op := range []uint8{elemRegExpExec, elemRegExpTest} {
		fn := r.regexpExecFn
		if op == elemRegExpTest {
			fn = r.regexpTestFn
		}
		fast := fn.fn().native
		// The built-ins' own functions, as initRegExpBuiltins makes them.
		slow := func(rt *Runtime, this Value, args []Value) (Value, error) {
			s, err := rt.toString(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			return rt.regexpExec(this, s)
		}
		if op == elemRegExpTest {
			slow = func(rt *Runtime, this Value, args []Value) (Value, error) {
				if !this.IsObject() {
					return Undefined, rt.throwTypeError("RegExp.prototype.test called on a non-object")
				}
				s, err := rt.toString(arg(args, 0))
				if err != nil {
					return Undefined, err
				}
				if rt.execIsBuiltin(this) {
					caps, _, err := rt.regexpMatch(this, s)
					if err != nil {
						return Undefined, err
					}
					return Bool(caps != nil), nil
				}
				res, err := rt.regExpExec(this, s)
				if err != nil {
					return Undefined, err
				}
				return Bool(!res.IsNull()), nil
			}
		}
		for _, recv := range recvs.Object().elemsOf() {
			// Each call from the receiver's own lastIndex, which a global
			// or sticky pattern's match moves.
			var li Value
			if recv.IsObject() && len(recv.Object().vals) > 0 {
				li = recv.Object().vals[0]
			}
			for _, a := range subjects.Object().elemsOf() {
				for _, args := range [][]Value{{a}, nil, {a, a}} {
					if recv.IsObject() && len(recv.Object().vals) > 0 {
						recv.Object().vals[0] = li
					}
					if _, err := r.Run(reset); err != nil {
						t.Fatal(err)
					}
					want := describe(slow(r, recv, args))
					ws := describe(r.Run(statics))
					if recv.IsObject() && len(recv.Object().vals) > 0 {
						// And lastIndex after it.
						ws += " " + r.describe(recv.Object().vals[0])
					}
					if recv.IsObject() && len(recv.Object().vals) > 0 {
						recv.Object().vals[0] = li
					}
					if _, err := r.Run(reset); err != nil {
						t.Fatal(err)
					}
					got := describe(fast(r, recv, args))
					gs := describe(r.Run(statics))
					if recv.IsObject() && len(recv.Object().vals) > 0 {
						gs += " " + r.describe(recv.Object().vals[0])
					}
					if got != want || ws != gs {
						t.Errorf("op %d on %s of %d arguments: %s, statics %s; the built-in's %s, statics %s",
							op, r.describe(recv), len(args), got, gs, want, ws)
					}
				}
			}
		}
	}
	// With nothing to match, the subject is undefined's string.
	if recv, _ := r.Run(compileForTest(t, `/[<>]/`)); !leafChecks[elemRegExpExec](r, recv, nil) {
		t.Error("exec's check of no argument fails")
	}
	_ = bytecode.OpCall
}

// A match the matcher makes past its step budget, with nothing to stop
// the script, throws its SyntaxError as regexpMatch throws it, through
// exec's and test's one-pass function (regexpLeaf's match), in the tree
// tier and in the interpreter, where a RegExp neither global nor sticky
// takes it.
func TestRegExpLeafComplexity(t *testing.T) {
	for _, c := range []struct {
		tree bool
		call string
	}{{true, "exec"}, {false, "test"}} {
		SetTreeTier(c.tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `function f(re) { let _k = 0; try { return String(re.`+c.call+`('a'.repeat(40))) } catch (e) { return e.name + ': ' + e.message } }
f(/(a+)+b/)`))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), "SyntaxError: regular expression is too complex"; got != want {
			t.Errorf("tree tier %v, %s: %s, want %s", c.tree, c.call, got, want)
		}
	}
	SetTreeTier(true)
}

// A leaf's call of exec or test whose match fails -- spends the step
// budget -- throws what a call with a frame throws: the SyntaxError, its
// stack, and Error.stackTraceLimit's getter run as often, as for the call
// with a frame of a global pattern, which is never a leaf's. The leaf's own
// call erred with nothing made, and its call made again found the match's
// failure without matching again.
func TestRegExpLeafFailsAgainWithFrame(t *testing.T) {
	src := `var gets = 0, limit = Error.stackTraceLimit;
Object.defineProperty(Error, 'stackTraceLimit', { get() { gets++; return limit }, set(v) { limit = v }, configurable: true });
function call(re, how) { gets = 0; try { return String(re[how]('a'.repeat(32) + 'cb')) } catch (e) { return [e.name, e.message, e.stack, gets].join(',') } }
var out = [];
for (var how of ['exec', 'test']) {
  var leaf = call(/(a+)+b/, how), framed = call(/(a+)+b/g, how);
  out.push(leaf === framed ? leaf : leaf + ' != ' + framed);
}
out.join('|')`
	want := "SyntaxError,regular expression is too complex,,0|SyntaxError,regular expression is too complex,,0"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, want)
		}
	}
	SetTreeTier(true)

	// The leaf's call itself, without a frame: an error that is no
	// exception, and the match remembered for the call made again.
	r := New(Config{})
	defer r.Close()
	recv, err := r.Run(compileForTest(t, `/(a+)+b/`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Run(compileForTest(t, `'a'.repeat(32) + 'cb'`))
	if err != nil {
		t.Fatal(err)
	}
	args := []Value{s}
	fd := r.regexpExecFn.fn()
	if !leafChecks[elemRegExpExec](r, recv, args) {
		t.Fatal("exec's check fails")
	}
	if _, err := fd.native(r, recv, args); err != errRegExpLeafAgain {
		t.Fatalf("the leaf's call: %v, want errRegExpLeafAgain", err)
	}
	if _, err := r.callObject(r.regexpExecFn, recv, args, Undefined); err == nil || err == errRegExpLeafAgain {
		t.Fatalf("the call with a frame: %v, want the SyntaxError", err)
	}

	// The call made again finds the failure without matching again: a
	// leaf's failed call costs what a call with a frame of a global
	// pattern, never a leaf's, does -- one match spending the budget, not
	// two.
	if testing.Short() {
		return
	}
	if _, err := r.Run(compileForTest(t, `function g(re, s) { try { re.exec(s) } catch (e) {} }`)); err != nil {
		t.Fatal(err)
	}
	best := func(src string) time.Duration {
		b := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			b = min(b, time.Since(start))
		}
		return b
	}
	leaf := best(`g(/(a+)+b/, 'a'.repeat(32) + 'cb')`)
	framed := best(`g(/(a+)+b/g, 'a'.repeat(32) + 'cb')`)
	if leaf > framed*3/2 {
		t.Errorf("a leaf's failed call took %v, one with a frame %v: the match made twice", leaf, framed)
	}
}

// A deadline stops a leaf's call of exec on a match that would run for
// ever, and the runtime runs on.
func TestRegExpLeafDeadline(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function f(re, s) { return re.exec(s) }`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	if _, err := r.Run(compileForTest(t, `f(/(a+)+b/, 'a'.repeat(64) + 'cb')`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v, want the deadline", err)
	}
	r.SetContext(context.Background())
	if v, err := r.Run(compileForTest(t, `f(/(a+)b/, 'xaab')[1]`)); err != nil || v.String().Go() != "aa" {
		t.Fatalf("after: %v, %v", v, err)
	}
}

// A leaf's call whose match spends the step budget, in a pure body nested in
// pure bodies, is matched once: each body that gives up is evaluated again
// with a frame, its pure callee's body again without one, the leaf's call in
// it made anew, which finds the match remembered rather than making it
// again. It had been made once for each level the pure calls nested it, and
// once more: a call of f2 below took four times what a call of a global
// pattern's exec, never a leaf's, takes. (A body pureCall has found closed
// often enough is no longer tried: the warm-up calls f2 with a pattern
// whose leaf's call passes.)
func TestRegExpLeafSpentOnceNested(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the step budget")
	}
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		src := `function f0(re, s) { return re.exec(s) }
function f1(re, s) { return f0(re, s) }
function f2(re, s) { return f1(re, s) }
var s = 'a'.repeat(32) + 'cb'
function warm() { for (var i = 0; i < 300; i++) f2(/(a+)+b/, 'aab') }
warm(); warm()`
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		best := func(src string) time.Duration {
			b := time.Duration(1 << 62)
			for range 2 {
				start := time.Now()
				v, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				if got := v.String().Go(); got != "SyntaxError: regular expression is too complex" {
					t.Fatalf("tree tier %v: %s = %s", tree, src, got)
				}
				b = min(b, time.Since(start))
			}
			return b
		}
		call := "var out; try { %s } catch (e) { out = e.name + ': ' + e.message } out"
		leaf, framed := best(fmt.Sprintf(call, "f2(/(a+)+b/, s)")), best(fmt.Sprintf(call, "/(a+)+b/g.exec(s)"))
		r.Close()
		if leaf > 2*framed {
			t.Errorf("tree tier %v: the leaf's failed call took %v, one with a frame %v: the match made %.1f times",
				tree, leaf, framed, float64(leaf)/float64(framed))
		}
	}
	SetTreeTier(true)
}
