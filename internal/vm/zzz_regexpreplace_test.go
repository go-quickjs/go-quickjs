package vm

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"testing"
)

// replaceFlatTestSrc replaces with patterns global and not, sticky, folded,
// unicode, of a literal and not, of groups, of empty matches and anchors,
// with the d flag and named groups; subjects empty, short, of 80 bytes and
// more, past ASCII and of lines; and templates empty, short, long, with a $
// and past ASCII: table hashes each result with lastIndex and the legacy
// statics. others replaces by a frozen RegExp, one whose lastIndex cannot
// be written, one with an exec of its own, a subclass's, one whose lastIndex
// is an object, with RegExp.prototype's exec replaced, and to a result too
// long for a string. run replaces in a loop, as Octane's RegExp does.
// replaceFlatTestWant is Node's answer (v26.10.0).
const replaceFlatTestSrc = `function hash(h, s) {
  for (var i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619) >>> 0
  return h
}
var long = 'd1 xx d1d1 ab_cd '.repeat(9) + 'tail', huge = 'x'.repeat(3000) + 'b'
var subjects = ['', 'a', 'ab', 'aaa', 'abab', 'xabx', 'b', long, huge, 'caf\xe9 ab', '\ud83d\ude00ab', 'a\nb\nc', '  trim me  ']
var templates = ['', 'X', 'xyz', '--long replacement--', 'Z'.repeat(70), '$', '$&', 'caf\xe9', 'a$1b', '$$']
var res = [/a/, /a/g, /ab/g, /aa/g, /d1/g, /_/g, /x/y, /a/gy, /b/gi, /a/gu, /(a)(b)?/g, /(a)|(b)/g, /x*/g, /^/g, /$/g, /\s+|^\s*|\s*$/g,
  /^\s*|\s*$/g, /\w+/, /[ab]/g, /z/g, /(?:)/g, /\n/gm, /^b/gm, /é/g, /a/gd, /(?<n>a)/g]
function state(re) { return re.lastIndex + '/' + RegExp.lastMatch + '/' + RegExp.leftContext.length + '/' + RegExp['$1'] }
function table() {
  var h = 2166136261, k = 0
  ;/(?:)/.exec('')
  for (var re of res) for (var s of subjects) for (var t of templates) {
    re.lastIndex = 2
    var r
    try { r = s.replace(re, t) } catch (e) { r = e.name }
    h = hash(h, r + '|' + state(re)); k++
  }
  return h + ' ' + k
}
// The replace is made in one pass where its subject and template are ASCII
// and the template has no $; anything else, and a RegExp that is not as the
// built-in has it, is replace's own way.
function others() {
  var o = []
  var fz = Object.freeze(/a/g); o.push((function () { try { return 'aa'.replace(fz, 'b') } catch (e) { return e.name } })())
  var nw = /a/g; Object.defineProperty(nw, 'lastIndex', { value: 3, writable: false }); o.push((function () { try { return 'aa'.replace(nw, 'b') } catch (e) { return e.name } })() + nw.lastIndex)
  var own = /a/g; own.exec = function (s) { return null }; o.push('aa'.replace(own, 'b'))
  class R extends RegExp { [Symbol.replace](s, t) { return 'sub:' + s + t } }
  o.push('aa'.replace(new R('a', 'g'), 'b'))
  var calls = 0, li = /a/g; li.lastIndex = { valueOf() { calls++; return 1 } }; o.push('aaa'.replace(li, 'b') + li.lastIndex + calls)
  var orig = RegExp.prototype.exec; RegExp.prototype.exec = function (s) { calls++; return orig.call(this, s) }
  o.push('abab'.replace(/b/g, 'c') + calls); RegExp.prototype.exec = orig
  var big
  try { big = 'a'.repeat(1 << 16).replace(/a/g, 'x'.repeat(1 << 14)).length } catch (e) { big = e.name }
  o.push(big)
  return o.join(' ')
}
// Replace in a loop: a literal's, a pattern's, global and not, a long
// subject's, as Octane's RegExp makes them, hashed.
function run(n) {
  var h = 2166136261
  for (var i = 0; i < n; i++) {
    var s = subjects[i % subjects.length]
    h = hash(h, s.replace(/d1/g, '') + s.replace(/ab/g, 'Q') + s.replace(res[i % res.length], '-') + s.replace(/^\s*|\s*$/g, '') + RegExp.lastMatch)
  }
  return h
}
table() + ' ' + others() + ' ' + run(500) + ' ' + run(2000)
`

const replaceFlatTestWant = "2637138712 3380 TypeError TypeError3 aa sub:aab bbb00 acac3 RangeError 3570225015 202452282"

// A string's replace by a template without a $, both ASCII, is made in one
// pass (zzz_regexpreplace.go): each answer is Node's, in the tree tier and
// in the interpreter, with the collector idle and running.
func TestRegExpReplaceFlat(t *testing.T) {
	for _, tree := range []bool{true, false} {
		for _, idle := range []bool{true, false} {
			SetTreeTier(tree)
			gc := debug.SetGCPercent(-1)
			if !idle {
				debug.SetGCPercent(1)
			}
			r := New(Config{})
			v, err := r.Run(compileForTest(t, replaceFlatTestSrc))
			r.Close()
			debug.SetGCPercent(gc)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != replaceFlatTestWant {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, replaceFlatTestWant)
			}
		}
	}
	SetTreeTier(true)
}

// A replace in one pass stops where an interrupt stops it as replace's
// own way does (regExpReplace), at each of the ticks it is made at:
// lastIndex and the legacy statics are those of the last match it made.
func TestRegExpReplaceFlatInterrupt(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	for _, re := range []string{"/b/g", "/b(a)?/g", "/b/", "/b/y"} {
		for ticks := 1; ticks <= 12; ticks++ {
			var got [2]string
			for i := range got {
				r := New(Config{})
				rx, err := r.Run(compileForTest(t, "var re = "+re+"; re"))
				if err != nil {
					t.Fatal(err)
				}
				s, tv := NewString(strings.Repeat("ab", 8)+strings.Repeat("c", 100)), NewString("x")
				fl, ok := r.replaceDirect(rx)
				if !ok {
					t.Fatal("not a direct replace")
				}
				r.abort, r.interruptCounter = stopped, ticks
				var v Value
				if i == 0 {
					v, err = replaceFlat[struct{}](r, rx, s, tv, fl)
				} else {
					v, err = r.regExpReplace(rx, []Value{Str(s), Str(tv)}, fl, true)
				}
				r.abort, r.stopped = nil, nil
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("%s at %d ticks: %v", re, ticks, err)
				}
				st, err := r.Run(compileForTest(t, "re.lastIndex + '/' + RegExp.lastMatch + '/' + RegExp.leftContext.length + '/' + RegExp['$1']"))
				if err != nil {
					t.Fatal(err)
				}
				got[i] = st.String().Go()
				if v.IsString() {
					got[i] += " " + v.String().Go()
				}
				r.Close()
			}
			if got[0] != got[1] {
				t.Errorf("%s at %d ticks: %s, replace's own way %s", re, ticks, got[0], got[1])
			}
		}
	}
}
