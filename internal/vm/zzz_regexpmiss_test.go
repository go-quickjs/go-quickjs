package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// regexpMissTestSrc matches, with exec and test, patterns a look at the
// subject can say miss (regexp's MissesASCII) -- anchored at the start, of
// a required literal, of one first unit or two -- and patterns it cannot
// -- the m flag, the empty string matched, global and sticky ones, which
// it is not asked of -- against subjects that match, that miss, and
// empty ones; and calls Octane's Exec, whose first RegExp misses nearly
// always, in a loop. The legacy statics after a miss are the last match's.
// regexpMissTestWant is Node's answer (v26.10.0).
const regexpMissTestSrc = `function show(m) { return m === null ? 'null' : m.index + ':' + m.join('|') + ':' + m.length }
function misses(n) {
  var o = []
  var res = [/^ba/, /^[ab]c/, /^(?:x|y)z/, /^\s*(\S+)/, /^a*/, /^ba/m, /^ba|c/, /\bQBZ\b/, /x.{0,3}abc/, /a\d+/, /[qz]\d/, /^ba/i, /^(?=b)ba/, /a\d+/u, /^ba/g, /a\d/g, /^ba/y]
  var subjects = ['pyvpx', 'bax', '', 'bc', 'cab', 'zz', 'xz', '  ab c', 'x\nba', 'xc', 'a QBZ b', 'vachggrkg', 'x12abc', 'xxxabd', 'a12', 'bcd', 'abz9', 'BA', 'q1', 'aa1']
  for (var k = 0; k < n; k++) {
    for (var i = 0; i < res.length; i++) {
      var re = res[i]
      for (var j = 0; j < subjects.length; j++) {
        var s = subjects[j]
        re.lastIndex = 0
        var m = re.exec(s)
        var li = re.lastIndex
        re.lastIndex = 0
        var t = re.test(s)
        if (k == n - 1) o.push(show(m) + ',' + li + ',' + t + ',' + re.lastIndex + ',' + RegExp.lastMatch + ',' + RegExp['$1'])
      }
    }
  }
  return o.join(' ')
}
function Exec(re, string) {
  var sum = 0
  re.lastIndex = 0
  var array = re.exec(string)
  if (array) {
    for (var i = 0; i < array.length; i++) {
      var substring = array[i]
      if (substring) sum += substring.length
    }
  }
  return sum
}
function execs(n) {
  var re0 = /^ba/, re3 = /\bQBZ\b/, re9 = /a\d+/, sum = 0
  var words = ['pyvpx', 'bax', 'a QBZ b', 'QBZ', '', 'a1', 'ba1', 'xxQBZ']
  for (var i = 0; i < n; i++) {
    var w = words[i % words.length]
    sum += Exec(re0, w) + 10 * Exec(re3, w) + 100 * Exec(re9, w)
  }
  ;/(b)(a)/.exec('xbay'); var before = RegExp.lastMatch + RegExp['$2']
  ;/^ba/.exec('pyvpx'); /\bQBZ\b/.test('none')
  return sum + ',' + before + ',' + RegExp.lastMatch + RegExp['$2'] + RegExp.leftContext
}
misses(3) + ' | ' + execs(100) + ' | ' + execs(400)
`

const regexpMissTestWant = `null,0,false,0,ba, 0:ba:1,0,true,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 0:bc:1,0,true,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, 0:bc:1,0,true,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, null,0,false,0,bc, 0:xz:1,0,true,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, null,0,false,0,xz, 0:pyvpx|pyvpx:2,0,true,0,pyvpx,pyvpx 0:bax|bax:2,0,true,0,bax,bax null,0,false,0,bax,bax 0:bc|bc:2,0,true,0,bc,bc 0:cab|cab:2,0,true,0,cab,cab 0:zz|zz:2,0,true,0,zz,zz 0:xz|xz:2,0,true,0,xz,xz 0:  ab|ab:2,0,true,0,  ab,ab 0:x|x:2,0,true,0,x,x 0:xc|xc:2,0,true,0,xc,xc 0:a|a:2,0,true,0,a,a 0:vachggrkg|vachggrkg:2,0,true,0,vachggrkg,vachggrkg 0:x12abc|x12abc:2,0,true,0,x12abc,x12abc 0:xxxabd|xxxabd:2,0,true,0,xxxabd,xxxabd 0:a12|a12:2,0,true,0,a12,a12 0:bcd|bcd:2,0,true,0,bcd,bcd 0:abz9|abz9:2,0,true,0,abz9,abz9 0:BA|BA:2,0,true,0,BA,BA 0:q1|q1:2,0,true,0,q1,q1 0:aa1|aa1:2,0,true,0,aa1,aa1 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0:a:1,0,true,0,a, 0::1,0,true,0,, 0::1,0,true,0,, 0::1,0,true,0,, 0:a:1,0,true,0,a, 0::1,0,true,0,, 0:a:1,0,true,0,a, 0::1,0,true,0,, 0::1,0,true,0,, 0:aa:1,0,true,0,aa, null,0,false,0,aa, 0:ba:1,0,true,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 2:ba:1,0,true,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 0:ba:1,0,true,0,ba, null,0,false,0,ba, 1:c:1,0,true,0,c, 0:c:1,0,true,0,c, null,0,false,0,c, null,0,false,0,c, 5:c:1,0,true,0,c, null,0,false,0,c, 1:c:1,0,true,0,c, null,0,false,0,c, 2:c:1,0,true,0,c, 5:c:1,0,true,0,c, null,0,false,0,c, null,0,false,0,c, 1:c:1,0,true,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, null,0,false,0,c, 2:QBZ:1,0,true,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, null,0,false,0,QBZ, 0:x12abc:1,0,true,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, null,0,false,0,x12abc, 0:a12:1,0,true,0,a12, null,0,false,0,a12, null,0,false,0,a12, null,0,false,0,a12, null,0,false,0,a12, 1:a1:1,0,true,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, 2:z9:1,0,true,0,z9, null,0,false,0,z9, 0:q1:1,0,true,0,q1, null,0,false,0,q1, null,0,false,0,q1, 0:ba:1,0,true,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 0:BA:1,0,true,0,BA, null,0,false,0,BA, null,0,false,0,BA, null,0,false,0,BA, 0:ba:1,0,true,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 0:a12:1,0,true,0,a12, null,0,false,0,a12, null,0,false,0,a12, null,0,false,0,a12, null,0,false,0,a12, 1:a1:1,0,true,0,a1, null,0,false,0,a1, 0:ba:1,2,true,2,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, 0:a1:1,2,true,2,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, null,0,false,0,a1, 1:a1:1,3,true,3,a1, null,0,false,0,a1, 0:ba:1,2,true,2,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, null,0,false,0,ba, | 5630,baa,baax | 23200,baa,baax`

// A match a look at its ASCII subject says finds nothing is not made
// (regexpLeaf's match, regexp's MissesASCII): exec's and test's answers,
// lastIndex and the legacy statics are Node's, in the interpreter and the
// tree tier, with the collector idle and running.
func TestRegExpLeafMisses(t *testing.T) {
	for _, idle := range []bool{true, false} {
		for _, tree := range []bool{true, false} {
			if idle {
				runtime.GC()
				debug.SetGCPercent(-1)
			}
			SetTreeTier(tree)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, regexpMissTestSrc))
			r.Close()
			debug.SetGCPercent(100)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != regexpMissTestWant {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, regexpMissTestWant)
			}
		}
	}
	SetTreeTier(true)
}
