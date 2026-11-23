//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"strings"
	"testing"
)

// A fresh invocation has no live locals from the previous invocation.
// Its native exec site can keep the array owned by the same stack window:
// each of eight fresh calls, entered natively, makes its match in the
// array the call before made. Exec counts to n first, so that its code is
// worth entering (jitSSAMinWork): without the loop it was judged slow
// after its warm-up (entrySlow), the eight calls ran in the interpreter,
// and the test checked a table entry they did not touch -- which the
// interpreter's own site's entry replaced where the two shared a slot.
func TestJITRegExpReuseFreshCalls(t *testing.T) {
	for _, marking := range []bool{false, true} {
		name := "idle"
		if marking {
			name = "marking"
		}
		t.Run(name, func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `
				function Exec(re, s, n) {
					var k = 0
					for (var i = 0; i < n; i++) k = (k + i) | 0
					var m = re.exec(s)
					return m ? m.length + m[1].length + (k & 0) : 0
				}
				var re = /(a)(b)(c)/;
			`)); err != nil {
				t.Fatal(err)
			}
			fn := r.global.getOwn(r.atoms.intern("Exec")).value()
			re := r.global.getOwn(r.atoms.intern("re")).value()
			args := []Value{re, Str(NewString("abc")), Int32(64)}
			call := func() {
				v, err := r.Call(fn, Undefined, args)
				if err != nil || !v.IsNumber() || v.Number() != 5 {
					t.Fatalf("Exec: %v, %v; want 5", v, err)
				}
			}
			for range 64 {
				call()
			}
			jitNativeForTest(t, r, "Exec")
			if !jitReuseNativeForTest(r, 4) {
				t.Fatal("no native reusable array")
			}
			tb := execReuseLeaf[struct{}](r).reuseTable
			var saved *execReuseEntry
			for i := range tb.e {
				if e := &tb.e[i]; e.site != nil && e.n == 4 {
					own := false
					for _, site := range fn.Object().fn().closure.reg.reuse {
						own = own || e.site == site
					}
					if !own {
						saved = e
					}
				}
			}
			if saved == nil {
				t.Fatal("native site's entry not found")
			}
			site, arr := saved.site, saved.arr
			life := jitRegLifeForTest(r, "Exec")
			for i := range 8 {
				entries := life.ssaStats.entries
				call()
				if life.ssaStats.entries != entries+1 {
					t.Fatalf("call %d was not entered natively (slow %v)", i, life.entrySlow)
				}
				var now *execReuseEntry
				for k := range tb.e {
					if tb.e[k].site == site {
						now = &tb.e[k]
					}
				}
				if now == nil || now.arr != arr {
					t.Fatalf("fresh call %d discarded its reusable array", i)
				}
			}
		})
	}
}

// Where a runtime runs the JIT, exec's leaf is told of the arrays it may
// make again by native code's calls too (jit_regexpreuse.go), each array
// made again only by the frame that made the last. The calls are made
// natively and in the interpreter as before: each answer is the
// interpreter's and Node's, with the collector idle and while it marks,
// and native frames made arrays again.
func TestJITRegExpReuse(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	src := strings.TrimSpace(regexpReuseTestSrc)
	setup := src[:strings.LastIndex(src, "\n")]
	want := strings.Split(regexpReuseTestWant, " # ")
	for _, marking := range []bool{false, true} {
		runtime.GC()
		release := holdCollectorNow(t)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 4 {
			rounds = append(rounds, "all()")
		}
		r := jitAgreesForTest(t, setup, rounds...)
		jitNativeForTest(t, r, "Exec")
		if !jitReuseNativeForTest(r, 0) {
			t.Errorf("marking %v: no array made again by a native frame in a runtime running the JIT", marking)
		}
		v, err := r.Run(compileForTest(t, "all() + ' # ' + all() + ' # ' + leaks()"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), want[0]+" # "+want[1]+" # "+want[2]; got != want {
			t.Errorf("marking %v:\ngot  %s\nwant %s", marking, got, want)
		}
		release()
	}
}

// jitReuseNativeForTest reports whether r's table holds an array a native
// frame made, of n elements or, for 0, of any: one of a site native code's
// calls are told (jitReuseForm), which is none of the register code's of
// the functions of r's global object.
func jitReuseNativeForTest(r *Runtime, n int) bool {
	own := map[*execReuseSite]bool{}
	for _, k := range r.global.ownKeys(false, r.atoms) {
		v := r.global.getOwn(k).value()
		if !v.IsObject() || v.Object().fn() == nil || v.Object().fn().closure == nil {
			continue
		}
		if c := firstReg(v.Object().fn().closure.fn); c != nil {
			for _, s := range c.reuse {
				own[s] = true
			}
		}
	}
	tb := execReuseLeaf[struct{}](r).reuseTable
	for i := 0; tb != nil && i < len(tb.e); i++ {
		if e := &tb.e[i]; e.site != nil && !own[e.site] && (n == 0 || e.n == n) {
			return true
		}
	}
	return false
}

// A site of exec that native code calls first on a RegExp whose array
// would be new however the leaf is told -- one with named groups -- and
// then on one whose array may be made again tells the leaf of the later
// calls (jitReuseForm asks execReuseFits at each call): a site may call
// exec on several RegExps, as a function's parameter's.
func TestJITRegExpReuseAfterUnfit(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `function Exec(re, s) {
  var m = re.exec(s), n = 0
  if (m) for (var i = 0; i < m.length; i++) if (m[i]) n += m[i].length
  return n
}
var named = /(?<n>a)(b)/, six = /(x)(y)(z)(w)(v)(u)/
function first() { var t = 0; for (var k = 0; k < 3000; k++) t += Exec(named, 'ab'); return t }
function then() { var t = 0; for (var k = 0; k < 3000; k++) t += Exec(six, 'xyzwvu'); return t }`
	r := jitAgreesForTest(t, setup, "first()", "first()", "then()")
	jitNativeForTest(t, r, "Exec")
	if v, err := r.Run(compileForTest(t, "first() + ' ' + then()")); err != nil {
		t.Fatal(err)
	} else if got := v.String().Go(); got != "12000 36000" {
		t.Fatalf("got %s, want 12000 36000", got)
	}
	if !jitReuseNativeForTest(r, 7) {
		t.Errorf("no array of six's made again by a native frame after named's calls")
	}
}

// jitReuseSrc calls exec's sites that make their arrays again from native
// frames and frames the interpreter runs: a loop entered at its header
// while a local holds a piece of the last match, its function called again
// beneath it by a valueOf; a callee with such a site inlined into a hot
// loop, left by a deoptimization when its receiver is another; and the
// first loop over subjects of which one is a number and one an object, its
// native code left for the interpreter in the frame there; an array a
// closure captures, read once the loop is done, and one kept from a loop's
// last match and returned -- neither a site made again. jitReuseWant is
// Node's answer (vm.runInThisContext).
const jitReuseSrc = `
// A loop native code goes on with at its header (on-stack replacement)
// while a piece of its last match is in a local, its function called
// again beneath it from the loop by a valueOf; and a callee inlined into
// a hot loop that its exec's site is in, then left by a deoptimization.
function carryDeep(re, ss, depth) {
  var n = '', sub
  for (var j = 0; j < ss.length; j++) {
    var m = re.exec(ss[j])
    if (depth > 0 && j % 97 == 5) n += { valueOf: function () { return carryDeep(re, ss.slice(0, 40), depth - 1).length } } + ','
    if (sub) n += sub.length
    if (m) sub = m[2]
  }
  return n
}
function piece(re, s) { var m = re.exec(s); return m ? m.length + (m[1] ? m[1].length : 0) : -1 }
function hot(re, ss, k) {
  var t = 0
  for (var j = 0; j < ss.length; j++) t = t * 7 + piece(j === k ? { exec: function () { return ['q', 'qq'] } } : re, ss[j]) | 0
  return t
}
// The array captured by a closure, which reads it later, and returned
// from a loop's last turn: neither made again.
function capt(re, ss) {
  var m, fs = []
  for (var j = 0; j < ss.length; j++) { m = re.exec(ss[j]); if (j % 500 == 0) fs.push(function () { return m ? m[1] : '-' }) }
  return fs.map(function (f) { return f() }).join() + ':' + (m ? m[1] : '-')
}
function last(re, ss) { var m; for (var j = 0; j < ss.length; j++) { var t = re.exec(ss[j]); if (t) m = t } return m }
function jitReuse() {
  var ss = []
  for (var i = 0; i < 3000; i++) ss.push(['a1', 'bbbb22', 'x', 'cc3', 'dddddd4', 'zz'][i % 6] + i)
  var o = []
  o.push(carryDeep(/([a-z]+)(\d+)/, ss, 2).length, carryDeep(/(\w)(\w+)(\d)/, ss, 1).slice(0, 60))
  o.push(hot(/([a-z]+)(\d+)/, ss, -1), hot(/([a-z]+)(\d+)/, ss, 2500), hot(/(x)?([a-z]+)/, ss, 10))
  var mixed = ss.slice()
  mixed[2000] = 12345
  mixed[2500] = { toString: function () { return 'qq7' } }
  o.push(carryDeep(/([a-z]*)(\d+)/, mixed, 1).slice(-60), carryDeep(/([a-z]*)(\d+)/, ss, 0).length)
  var l1 = last(/([a-z]+)(\d+)/, ss), l2 = last(/([a-z]+)(\d+)/, ss.slice(0, 100))
  o.push(capt(/([a-z]+)(\d+)/, ss), l1 === l2, l1.join('|'), l2.join('|'))
  return o.join(' ')
}
`

const jitReuseWant = "3092 155239,61155272261372261372261372261372261372261372261372261 747508448 -849150803 -354450553 456455456455456455456455456455456455456455456455456455456455 2999 zz,zz,zz,zz,zz,zz:zz false zz2999|zz|2999 cc399|cc|399"

// TestJITRegExpReuseFrames runs jitReuseSrc with the JIT and without, the
// collector idle and as while it marks: the interpreter's answer and
// Node's, the arrays the frames native code went on with held forgotten
// (execReuseForget) and none made again by a call Go made for native code
// (Runtime's reuseQuiet).
func TestJITRegExpReuseFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, jitReuseSrc, "jitReuse()", "jitReuse()", "jitReuse()", "jitReuse()")
		v, err := r.Run(compileForTest(t, "jitReuse()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != jitReuseWant {
			t.Errorf("marking %v: %s, want %s", marking, got, jitReuseWant)
		}
	}
}
