package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regexpReuseTestSrc calls exec where its array and its pieces may be made
// again (zzz_regexpreuse.go) -- Octane's Exec, of patterns of 1 to 18
// elements, named groups, the d flag, global ones, subjects past ASCII and
// empty ones; Exec again from a valueOf of its sum while it still reads
// its array; a loop that reads a piece of the last match after the next;
// the array's index, input and a constant element, under let and const --
// and where they may not: arrays and pieces kept or returned. leaks gives
// a getter of Array.prototype and of Object.prototype, defined while an
// array is held, and a Proxy and an object of such a getter made
// Array.prototype's prototype, the arrays as their receiver, and calls the
// same sites after, while they are there and once they are gone. regexpReuseTestWant is Node's answer (v26.10.0).
const regexpReuseTestSrc = `function Exec(re, string) {
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
// The same, its sum an object whose valueOf makes the same call again,
// while the outer array is still being read.
function ExecDeep(re, string, depth) {
  var sum = depth > 0 ? { valueOf: function () { return ExecDeep(re, string.slice(1), depth - 1).length } } : 0
  re.lastIndex = 0
  var array = re.exec(string)
  var t = ''
  if (array) {
    for (var i = 0; i < array.length; i++) {
      var substring = array[i]
      if (substring) { sum += substring.length; t += substring.length + '.' }
    }
  }
  return sum + '/' + t
}
// Its pieces kept in a local across the loop's next match.
function carry(re, ss) {
  var n = '', sub
  for (var j = 0; j < ss.length; j++) {
    var m = re.exec(ss[j])
    if (sub) n += sub.length
    if (m) sub = m[2]
  }
  return n
}
// Its index, input, a constant element, and comparisons with null.
function fields(re, s) {
  var m = re.exec(s)
  if (m !== null) return m.index + ':' + m.input.length + ':' + (m[1] ? m[1].length : -1) + ':' + m.length
  return 'null'
}
function fieldsLet(re, s) {
  let r = 0
  for (let k = 0; k < 3; k++) {
    const m = re.exec(s)
    if (m != null) r += m[0].length + m.length
  }
  return r
}
// Arrays and pieces that do leave the function.
var kept = []
function keepArray(re, s) { var m = re.exec(s); kept.push(m); return m ? m.length : 0 }
function keepPiece(re, s) { var m = re.exec(s); if (m) kept.push(m[2]); return m ? m.length : 0 }
function retArray(re, s) { var m = re.exec(s); return m }
var pats = [/^ba/, /(((\w+):\/\/)([^\/:]*)(:(\d+))?)?([^#?]*)(\?([^#]*))?(#(.*))?/, /(a)|(b)/, /([a-z]+)(\d+)/, /(\d*)(\D*)/g,
  /(x)(y)?(z)?(w)?(v)?(u)?/, /(?<n>a)(b)/, /(a)(b)/d, /é(.)/, /()/, /(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)(.)/, /qqqq|qqq|qq|q|ZZZZ|ZZZ|ZZ|Z/g, /(=)(.*)/]
var subjects = ['uggc://jjj.snprobbx.pbz/ybtva.cuc', 'bark', 'a1', 'zz99x', '528.9', 'xyz', 'xyzwvu', 'ab', 'éé', 'qqqq, ZZZ', 'k=v', 'abcdefghijklmnopqrstu', '', 'a'.repeat(3) + 'é' + '12']
function all() {
  var o = []
  for (var r = 0; r < 3; r++) {
    var t = 0
    for (var i = 0; i < pats.length; i++) for (var j = 0; j < subjects.length; j++) t = t * 31 + Exec(pats[i], subjects[j]) | 0
    o.push(t)
  }
  for (var i = 0; i < 4; i++) o.push(ExecDeep(pats[1], 'uggc://jjj.snprobbx.pbz/' + 'p'.repeat(i) + '?x#y', i))
  o.push(carry(/([a-z]+)(\d+)/, ['a1', 'x', 'bbbb22', 'y', 'cc3', 'q', 'dddddd4', 'zz']))
  o.push(carry(/(\w)(\w+)(\d)(\d)(\d)/, ['ab123', '--', 'cdefg456', '..', 'h12345', ',']))
  for (var j = 0; j < subjects.length; j++) o.push(fields(pats[1], subjects[j]) + ',' + fields(pats[3], subjects[j]) + ',' + fieldsLet(pats[5], subjects[j]))
  kept = []
  for (var j = 0; j < 6; j++) { keepArray(pats[1], subjects[j % 3]); keepPiece(pats[5], subjects[6 + j % 2]); keepPiece(pats[3], subjects[2 + j % 2]) }
  o.push(kept.map(function (k) { return k === null ? 'null' : typeof k == 'string' ? k : k.join('|') }).join(','))
  o.push(kept[0] === kept[6] ? 'same' : 'distinct')
  var a1 = retArray(pats[1], subjects[0]), a2 = retArray(pats[1], subjects[1])
  o.push(a1 === a2 ? 'same' : a1.join('|') + ';' + a2.join('|'))
  return o.join(' ')
}
// A getter of the array's prototypes, given the array as its receiver --
// defined while the array is held, by the call between the match and the
// read, or before the match -- and a Proxy made Array.prototype's
// prototype: the arrays they were given are left as they were by the
// matches after, while the getter is there and once it is gone.
var leaked = []
function nop() {}
function leakMid(re, s, f) { var m = re.exec(s); f(); return m ? (m[15] ? 1 : 0) + m.length : -1 }
function leakMid2(re, s, f) { var m = re.exec(s); f(); return m ? (m[15] === undefined ? 0 : 1) + m.length : -1 }
function getter(o) {
  return function () { Object.defineProperty(o, '15', { get: function () { leaked.push(this); return undefined }, configurable: true }) }
}
function many(f, k, g) { for (var i = 0; i < 6; i++) f(pats[1], 'uggc://' + k + i + '.pbz/k', g) }
function leaks() {
  var o = []
  for (var f of [leakMid, leakMid2]) {
    many(f, 'a', nop)
    f(pats[1], 'uggc://b.pbz/k', getter(Array.prototype))
    many(f, 'c', nop)
    delete Array.prototype[15]
    many(f, 'd', nop)
    f(pats[1], 'uggc://e.pbz/k', getter(Object.prototype))
    many(f, 'g', nop)
    delete Object.prototype[15]
    many(f, 'h', nop)
    var proto = Object.getPrototypeOf(Array.prototype)
    f(pats[1], 'uggc://i.pbz/k', function () {
      Object.setPrototypeOf(Array.prototype, new Proxy(proto, { get: function (t, k, rcv) { if (k === '15') leaked.push(rcv); return Reflect.get(t, k, rcv) } }))
    })
    many(f, 'j', nop)
    Object.setPrototypeOf(Array.prototype, proto)
    many(f, 'k', nop)
    f(pats[1], 'uggc://l.pbz/k', function () {
      Object.setPrototypeOf(Array.prototype, Object.create(proto, { 15: { get: function () { leaked.push(this); return undefined } } }))
    })
    many(f, 'm', nop)
    Object.setPrototypeOf(Array.prototype, proto)
    many(f, 'n', nop)
  }
  o.push(leaked.length)
  o.push(leaked.map(function (a) { return a[1] + '@' + a.index }).join(' '))
  return o.join(' ')
}
all() + ' # ' + all() + ' # ' + leaks()
`

const regexpReuseTestWant = `-384824464 -384824464 -384824464 85/28.23.7.4.16.1.2.1.2.1. 113/29.23.7.4.16.2.2.1.2.1. 116/30.23.7.4.16.3.2.1.2.1. 118/31.23.7.4.16.4.2.1.2.1. 1122111 11442 0:33:23:12,null,24 0:4:-1:12,null,0 0:2:-1:12,0:2:1:3,0 0:5:-1:12,0:5:2:3,24 0:5:-1:12,null,0 0:3:-1:12,null,30 0:6:-1:12,null,39 0:2:-1:12,null,0 0:2:-1:12,null,0 0:9:-1:12,null,0 0:3:-1:12,null,0 0:21:-1:12,null,0 0:0:-1:12,null,0 0:6:-1:12,null,0 uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||,y,1,bark|||||||bark||||,99,a1|||||||a1||||,y,1,uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||,99,bark|||||||bark||||,y,1,a1|||||||a1||||,99 distinct uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||;bark|||||||bark|||| # -384824464 -384824464 -384824464 85/28.23.7.4.16.1.2.1.2.1. 113/29.23.7.4.16.2.2.1.2.1. 116/30.23.7.4.16.3.2.1.2.1. 118/31.23.7.4.16.4.2.1.2.1. 1122111 11442 0:33:23:12,null,24 0:4:-1:12,null,0 0:2:-1:12,0:2:1:3,0 0:5:-1:12,0:5:2:3,24 0:5:-1:12,null,0 0:3:-1:12,null,30 0:6:-1:12,null,39 0:2:-1:12,null,0 0:2:-1:12,null,0 0:9:-1:12,null,0 0:3:-1:12,null,0 0:21:-1:12,null,0 0:0:-1:12,null,0 0:6:-1:12,null,0 uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||,y,1,bark|||||||bark||||,99,a1|||||||a1||||,y,1,uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||,99,bark|||||||bark||||,y,1,a1|||||||a1||||,99 distinct uggc://jjj.snprobbx.pbz/ybtva.cuc|uggc://jjj.snprobbx.pbz|uggc://|uggc|jjj.snprobbx.pbz|||/ybtva.cuc||||;bark|||||||bark|||| # 56 uggc://b.pbz@0 uggc://c0.pbz@0 uggc://c1.pbz@0 uggc://c2.pbz@0 uggc://c3.pbz@0 uggc://c4.pbz@0 uggc://c5.pbz@0 uggc://e.pbz@0 uggc://g0.pbz@0 uggc://g1.pbz@0 uggc://g2.pbz@0 uggc://g3.pbz@0 uggc://g4.pbz@0 uggc://g5.pbz@0 uggc://i.pbz@0 uggc://j0.pbz@0 uggc://j1.pbz@0 uggc://j2.pbz@0 uggc://j3.pbz@0 uggc://j4.pbz@0 uggc://j5.pbz@0 uggc://l.pbz@0 uggc://m0.pbz@0 uggc://m1.pbz@0 uggc://m2.pbz@0 uggc://m3.pbz@0 uggc://m4.pbz@0 uggc://m5.pbz@0 uggc://b.pbz@0 uggc://c0.pbz@0 uggc://c1.pbz@0 uggc://c2.pbz@0 uggc://c3.pbz@0 uggc://c4.pbz@0 uggc://c5.pbz@0 uggc://e.pbz@0 uggc://g0.pbz@0 uggc://g1.pbz@0 uggc://g2.pbz@0 uggc://g3.pbz@0 uggc://g4.pbz@0 uggc://g5.pbz@0 uggc://i.pbz@0 uggc://j0.pbz@0 uggc://j1.pbz@0 uggc://j2.pbz@0 uggc://j3.pbz@0 uggc://j4.pbz@0 uggc://j5.pbz@0 uggc://l.pbz@0 uggc://m0.pbz@0 uggc://m1.pbz@0 uggc://m2.pbz@0 uggc://m3.pbz@0 uggc://m4.pbz@0 uggc://m5.pbz@0`

// An array exec makes where only a local of its function can reach it is
// made again in the memory of its site's last (zzz_regexpreuse.go): each
// answer is Node's, in the interpreter and the tree tier, with the
// collector idle and running -- the arrays a valueOf's call of the same
// site made while the outer one was read, the pieces a loop read after the
// next match, and the arrays and pieces kept, returned or handed to a
// getter left as they were.
func TestRegExpReuse(t *testing.T) {
	for _, idle := range []bool{true, false} {
		for _, tree := range []bool{true, false} {
			if idle {
				runtime.GC()
				debug.SetGCPercent(-1)
			}
			SetTreeTier(tree)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, regexpReuseTestSrc))
			r.Close()
			debug.SetGCPercent(100)
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != regexpReuseTestWant {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, regexpReuseTestWant)
			}
		}
	}
	SetTreeTier(true)
}

// regexpReuseSiteOf is the site execReuseOf finds for the first call of
// exec in function name of fn, a script, or nil.
func regexpReuseSiteOf(t *testing.T, fn *bytecode.Function, name string) *execReuseSite {
	t.Helper()
	for _, c := range fn.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil || c.Fn.Name != name {
			continue
		}
		f := c.Fn
		for pc, in := range f.Code {
			if in.Op != bytecode.OpGetPropThis || f.Names[in.A] != "exec" {
				continue
			}
			for q := pc + 1; q+1 < len(f.Code); q++ {
				if f.Code[q].Op == bytecode.OpCallMethod {
					switch st := f.Code[q+1]; st.Op {
					case bytecode.OpSetLocal, bytecode.OpPutLocal, bytecode.OpSetLocalGet, bytecode.OpInitLocal:
						return execReuseOf[struct{}](f, q+1, int(st.A))
					}
					return nil
				}
			}
		}
	}
	t.Fatalf("no call of exec in %s", name)
	return nil
}

// Where a call's arrays may be made again, and its pieces' Strings too, is
// what its function's code says it reads of them; and the tree tier makes
// Exec's again, as many as the patterns' numbers of elements, the same
// arrays on another run.
func TestRegExpReuseSites(t *testing.T) {
	fn := compileForTest(t, regexpReuseTestSrc)
	cases := []struct {
		name       string
		reused     bool
		strs, kept bool
	}{
		{"Exec", true, true, true},
		{"ExecDeep", true, true, true},
		{"carry", true, true, true},
		{"fields", true, true, false},
		{"fieldsLet", true, true, false},
		{"leakMid", true, true, false},
		{"leakMid2", true, false, false},
		{"keepPiece", true, false, false},
		{"keepArray", false, false, false},
		{"retArray", false, false, false},
	}
	for _, c := range cases {
		site := regexpReuseSiteOf(t, fn, c.name)
		if (site != nil) != c.reused {
			t.Errorf("%s: site %+v, want one %v", c.name, site, c.reused)
			continue
		}
		if site == nil {
			continue
		}
		if site.strs != c.strs || (site.s >= 0) != c.kept {
			t.Errorf("%s: site %+v, want strs %v and a local of the pieces %v", c.name, site, c.strs, c.kept)
		}
	}
	SetTreeTier(true)
	r := New(Config{})
	defer r.Close()
	src := strings.TrimSpace(regexpReuseTestSrc)
	if _, err := r.Run(compileForTest(t, src[:strings.LastIndex(src, "\n")]+"\nall()")); err != nil {
		t.Fatal(err)
	}
	tb := execReuseLeaf[struct{}](r).reuseTable
	if tb == nil {
		t.Fatal("no array made again")
	}
	type key struct {
		site *execReuseSite
		n    int
	}
	arrays := map[key]*Object{}
	for _, e := range tb.e {
		if e.arr != nil {
			arrays[key{e.site, e.n}] = e.arr
		}
	}
	if len(arrays) < 5 {
		t.Fatalf("%d arrays kept to make again, want 5 or more", len(arrays))
	}
	if _, err := r.Run(compileForTest(t, "for (var i = 0; i < 10; i++) Exec(pats[1], subjects[i % 3]) + Exec(pats[3], subjects[2]) + Exec(pats[5], subjects[6])")); err != nil {
		t.Fatal(err)
	}
	same := 0
	for _, e := range tb.e {
		if a, ok := arrays[key{e.site, e.n}]; ok && e.arr != nil && (e.n == 12 || e.n == 3 || e.n == 7) {
			if a != e.arr {
				t.Errorf("an array of %d elements made anew", e.n)
			}
			same++
		}
	}
	if same < 3 {
		t.Errorf("%d of Exec's arrays made again, want 3", same)
	}
}
