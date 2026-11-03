package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// regexpExecDirectTestSrc calls exec where its array may be made again
// (zzz_regexpreuse.go), as Octane's Exec does: on global, sticky, global
// and sticky, and unicode patterns from lastIndex at, which it writes
// before, reads after, before and past the subject's end and between the
// halves of a pair; on a frozen global RegExp, whose lastIndex cannot be
// written; and on one whose lastIndex is an object, whose valueOf is
// counted. regexpExecDirectTestWant is Node's answer (v26.10.0).
const regexpExecDirectTestSrc = `function E(re, s, at) {
  re.lastIndex = at
  var a = re.exec(s)
  var t = re.lastIndex + ':'
  if (a) {
    for (var i = 0; i < a.length; i++) {
      var x = a[i]
      if (x) t += x.length + ','
      else t += '-,'
    }
  } else t += 'null'
  return t
}
function F(re, s) {
  var a = re.exec(s)
  var t = String(re.lastIndex) + ':'
  if (a) for (var i = 0; i < a.length; i++) { var x = a[i]; if (x) t += x.length + ',' }
  return t
}
function tryE(re, s, at) { try { return E(re, s, at) } catch (e) { return e.name } }
function tryF(re, s) { try { return F(re, s) } catch (e) { return e.name } }
function run() {
  var o = []
  var g = /(\d*)(\D*)/g, y = /(a)(b)?/y, gy = /a|(b)/gy, gu = /(.)(é)?/gu, n = /(\d+)/
  for (var at of [0, 2, 3, 7, 100]) o.push(E(g, '528.9xy', at))
  for (var at of [0, 1, 2, 3, 4, 9]) o.push(E(y, 'abaab', at))
  for (var at of [0, 1, 2, 3]) o.push(E(gy, 'abab', at))
  for (var at of [0, 1, 2, 3]) o.push(E(gu, 'aé\u{1F600}é', at))
  o.push(E(n, 'x12y', 5))
  var fz = Object.freeze(/(x)/g)
  o.push(tryE(fz, 'axb', 0), tryF(fz, 'axb'), String(fz.lastIndex))
  var calls = 0, li = /(x)(y)/g
  li.lastIndex = { valueOf: function () { calls++; return 1 } }
  o.push(F(li, 'axyxy'), calls, li.lastIndex)
  var cnt = 0
  for (var k = 0; k < 50; k++) cnt += E(g, '1a2b3c', k % 7).length
  o.push(cnt)
  return o.join(' ')
}
run() + ' # ' + run()`

const regexpExecDirectTestWant = "4:4,3,1, 4:2,1,1, 4:1,-,1, 7:-,-,-, 0:null 2:2,1,1, 0:null 3:1,1,-, 5:2,1,1, 0:null 0:null 1:1,-, 2:1,1, 3:1,-, 4:1,1, 2:2,1,1, 2:1,1,-, 5:3,2,1, 5:3,2,1, 5:2,2, TypeError TypeError 0 3:2,1,1, 1 3 400"

// A tree's call of exec whose array may be made again calls exec's leaf
// itself (reuseCallRest), on a global or a sticky RegExp too: each answer
// is Node's, in the tree tier and the interpreter, with the collector idle
// and running.
func TestRegExpExecDirect(t *testing.T) {
	want := regexpExecDirectTestWant + " # " + regexpExecDirectTestWant
	for _, idle := range []bool{true, false} {
		for _, tree := range []bool{true, false} {
			if idle {
				runtime.GC()
				debug.SetGCPercent(-1)
			}
			SetTreeTier(tree)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, regexpExecDirectTestSrc))
			r.Close()
			debug.SetGCPercent(100)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, want)
			}
		}
	}
	SetTreeTier(true)
}

// The call of a global pattern's exec is made without a frame, as a
// non-global one's is: where the frames a runtime may have leave room for
// E's call of a non-global pattern's exec and no more, a global one's is
// made too, where a frame of its own would have been past the limit. A
// match that spends the step budget throws what the call with a frame
// throws, its stack and Error.stackTraceLimit's getter run as often.
func TestRegExpExecDirectFrameless(t *testing.T) {
	SetTreeTier(true)
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, regexpExecDirectTestSrc+`; var rg = /(\d*)(\D*)/g, rp = /(\d*)(\D*)/`)); err != nil {
		t.Fatal(err)
	}
	max := r.maxFrames
	call := `E(rp, '1a2b', 0)`
	k := 1
	for ; k < 64; k++ {
		r.maxFrames = k
		if _, err := r.Run(compileForTest(t, call)); err == nil {
			break
		}
	}
	for _, src := range []string{call, `E(rg, '1a2b', 0)`, `E(rg, '1a2b', 2)`} {
		v, err := r.Run(compileForTest(t, src))
		if err != nil || v.String().Go() == "" {
			t.Errorf("%s with room for %d frames: %v, %v", src, k, v, err)
		}
	}
	r.maxFrames = max

	src := `var gets = 0, limit = Error.stackTraceLimit
Object.defineProperty(Error, 'stackTraceLimit', { get() { gets++; return limit }, set(v) { limit = v }, configurable: true })
function bad(re) { gets = 0; try { return E(re, 'a'.repeat(32) + 'cb', 0) } catch (e) { return [e.name, e.message, e.stack, gets].join(',') } }
function framed(re) { gets = 0; try { re.lastIndex = 0; return String(re.exec('a'.repeat(32) + 'cb')) } catch (e) { return [e.name, e.message, e.stack, gets].join(',') } }
var out = []
for (var re of [/(a+)+b/g, /(a+)+b/y, /(a+)+b/]) { var a = bad(re), b = framed(re); out.push(a === b ? a : a + ' != ' + b) }
out.join('|')`
	if testing.Short() {
		return
	}
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	const one = "SyntaxError,regular expression is too complex,,0"
	if got, want := v.String().Go(), one+"|"+one+"|"+one; got != want {
		t.Errorf("a match past the budget:\ngot  %s\nwant %s", got, want)
	}
}
