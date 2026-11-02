package vm

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regexpDeadTestSrc calls RegExp.prototype.exec where nothing reads its
// result but, at most, whether it is truthy (zzz_regexpdead.go) -- as a
// statement, in an if, a while, a not, an && and a ternary's arm, a comma's
// left side and a void -- on RegExps global, sticky and neither, held by
// an upvalue, a global, a local, a property of a local and of this, this
// itself, and literals, reading lastIndex and the legacy statics after
// each; and beside them what must not take it: an || that keeps the
// array, an instance's exec of its own, a subclass's exec, a subclass
// without one, an exec read by a getter, a Proxy, frozen RegExps, an
// argument whose toString calls the same RegExp's exec and reads it,
// RegExp.prototype.exec replaced. regexpDeadTestWant is Node's answer
// (v26.10.0, vm.runInThisContext).
const regexpDeadTestSrc = `var calls = 0, inner = []
function st(re) { return [re.lastIndex, RegExp.$1, RegExp.$2, RegExp.lastMatch, RegExp.leftContext, RegExp.rightContext].join('/') }
function g(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
var gre = /(b)(c)?/g
function outer() {
  var up = /(a)(b)/, upg = /(\d)/g, ups = /(x)/y
  return function blk(ss) {
    var out = []
    for (var i = 0; i < ss.length; i++) {
      var s = ss[i]
      up.exec(s); out.push(st(up))
      upg.exec(s); out.push(st(upg))
      ups.exec(s); out.push(st(ups))
      gre.exec(s); out.push(st(gre))
      ;/(e)(f)/g.exec(s); out.push(st(/z/))
      ;/(e)(f)?/.exec(s); out.push(st(/z/))
      up.test(s); upg.test(s); out.push(st(upg))
      if (up.exec(s)) out.push('if')
      if (/(c)/.exec(s)) out.push('lit-if')
      var w = 0
      while (upg.exec(s)) out.push('w' + upg.lastIndex + RegExp.$1 + w++)
      out.push(!ups.exec(s), up.exec(s) && 'and', String(up.exec(s) || 'or'), String(gre.exec(s) || 'or'))
      i % 2 ? up.exec(s) : upg.exec(s)
      out.push(st(upg), (up.exec(s), 5), String(void up.exec(s)))
    }
    return out.join(' ')
  }
}
var blk = outer()
function locs(re, o, ss) {
  var out = []
  for (var i = 0; i < ss.length; i++) {
    re.exec(ss[i]); out.push(st(re))
    o.re.exec(ss[i]); out.push(st(o.re))
    out.push(re.exec(ss[i]) ? 1 : 0)
  }
  return out.join(' ')
}
function Holder(re) { this.re = re }
Holder.prototype.run = function (ss) {
  var out = []
  for (var i = 0; i < ss.length; i++) { this.re.exec(ss[i]); out.push(st(this.re)); if (!this.re.exec(ss[i])) out.push('none') }
  return out.join(' ')
}
function viaThis(ss) { var out = []; for (var i = 0; i < ss.length; i++) { this.exec(ss[i]); out.push(st(this)) } return out.join(' ') }
function others(ss) {
  var out = []
  var own = /(a)/; own.exec = function (s) { calls++; return null }
  class R extends RegExp { exec(s) { calls++; return super.exec(s) } }
  class R2 extends RegExp {}
  var sub = new R('(a)(b)?', 'g'), sub2 = new R2('(b)(.)')
  var orig = RegExp.prototype.exec
  var gett = /(c)(.)/; Object.defineProperty(gett, 'exec', { get() { calls++; return orig } })
  var px = new Proxy(/(a)/, {})
  var fz = Object.freeze(/(a)/g), fzs = /(a)/; Object.freeze(fzs)
  var up = /(a)b/
  var arg = { toString() { calls++; var m = up.exec('zab'); inner.push(m && m.index + m[1]); return 'xab' } }
  for (var i = 0; i < ss.length; i++) {
    var s = ss[i]
    own.exec(s); out.push(st(own))
    sub.exec(s); out.push(sub.lastIndex)
    if (sub.exec(s)) out.push('sub')
    sub2.exec(s); out.push(sub2.lastIndex)
    gett.exec(s); out.push(st(gett))
    out.push(g(() => { px.exec(s); return 'px' }))
    out.push(g(() => { fz.exec(s); return 'fz' }), fz.lastIndex)
    fzs.exec(s); out.push(st(fzs))
    up.exec(arg); out.push(st(up))
    up.exec(42); out.push(st(up))
    RegExp.prototype.exec = function (s) { calls++; return orig.call(this, s) }
    up.exec(s); out.push(st(up))
    if (/(b)/.exec(s)) out.push('replaced')
    RegExp.prototype.exec = orig
  }
  return out.join(' ') + ' calls=' + calls + ' inner=' + inner.join(',')
}
var ss = ['xab1c2', 'ef', 'abcdef9x', '', 'x9bcab']
var all = function () {
  return [blk(ss), blk(ss), locs(/(c)(d)?/g, { re: /(a)/y }, ss), new Holder(/(\d)(\w)/).run(ss), viaThis.call(/(b)/g, ss), others(ss)].join(' | ')
}
all() + ' || ' + all() + ' || ' + all()
`

const regexpDeadTestWant = `0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 3/b//b/xa/1c2 0/b//b/xa/1c2 0/b//b/xa/1c2 6/2//2/xab1c/ if lit-if true and ab,a,b or 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 6/2//2/xab1c/ if lit-if true and ab,a,b b,b, 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 5/c//c/xab1/2 0/c//c/xab1/2 0 0/c//c/xab1/2 0/c//c/xab1/2 0 4/c/d/cd/ab/ef9x 1/a//a//bcdef9x 0 0/a//a//bcdef9x 0/a//a//bcdef9x 0 4/c//c/x9b/ab 0/c//c/x9b/ab 0 | 0/1/c/1c/xab/2 0/1/c/1c/xab/2 none 0/9/x/9x/abcdef/ 0/9/x/9x/abcdef/ none 0/9/b/9b/x/cab | 3/b//b/xa/1c2 0/b//b/xa/1c2 2/b//b/a/cdef9x 0/b//b/a/cdef9x 3/b//b/x9/cab | 0/b//b/x9/cab 3 0 0/c/2/c2/xab1/ TypeError TypeError 0 0/a//a/x/b1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/1c2 replaced 0/b//b/xa/1c2 0 0 0/b//b/xa/1c2 TypeError TypeError 0 0/b//b/xa/1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 2 0 0/c/d/cd/ab/ef9x TypeError TypeError 0 0/a//a//bcdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab//cdef9x replaced 0/b//b/a/cdef9x 0 0 0/b//b/a/cdef9x TypeError TypeError 0 0/b//b/a/cdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 6 0 0/c/a/ca/x9b/b TypeError TypeError 0 0/a//a/x9bc/b 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x9bc/ replaced calls=35 inner=1a,1a,1a,1a,1a || 0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 6/2//2/xab1c/ if lit-if true and ab,a,b b,b, 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 6/2//2/xab1c/ if lit-if true and ab,a,b b,b, 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 5/c//c/xab1/2 0/c//c/xab1/2 0 0/c//c/xab1/2 0/c//c/xab1/2 0 4/c/d/cd/ab/ef9x 1/a//a//bcdef9x 0 0/a//a//bcdef9x 0/a//a//bcdef9x 0 4/c//c/x9b/ab 0/c//c/x9b/ab 0 | 0/1/c/1c/xab/2 0/1/c/1c/xab/2 none 0/9/x/9x/abcdef/ 0/9/x/9x/abcdef/ none 0/9/b/9b/x/cab | 3/b//b/xa/1c2 0/b//b/xa/1c2 2/b//b/a/cdef9x 0/b//b/a/cdef9x 3/b//b/x9/cab | 0/b//b/x9/cab 3 0 0/c/2/c2/xab1/ TypeError TypeError 0 0/a//a/x/b1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/1c2 replaced 0/b//b/xa/1c2 0 0 0/b//b/xa/1c2 TypeError TypeError 0 0/b//b/xa/1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 2 0 0/c/d/cd/ab/ef9x TypeError TypeError 0 0/a//a//bcdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab//cdef9x replaced 0/b//b/a/cdef9x 0 0 0/b//b/a/cdef9x TypeError TypeError 0 0/b//b/a/cdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 6 0 0/c/a/ca/x9b/b TypeError TypeError 0 0/a//a/x9bc/b 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x9bc/ replaced calls=70 inner=1a,1a,1a,1a,1a,1a,1a,1a,1a,1a || 0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 6/2//2/xab1c/ if lit-if true and ab,a,b b,b, 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 0/a/b/ab/x/1c2 4/1//1/xab/c2 1/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 0/x//x//ab1c2 6/2//2/xab1c/ if lit-if true and ab,a,b b,b, 4/1//1/xab/c2 5 undefined 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/a/b/ab/x/1c2 0/e/f/ef// 0/e/f/ef// 0/e/f/ef// true  or or 0/e/f/ef// 5 undefined 0/a/b/ab//cdef9x 7/9//9/abcdef/x 0/9//9/abcdef/x 3/b/c/bc/a/def9x 0/e/f/ef/abcd/9x 0/e/f/ef/abcd/9x 0/a/b/ab//cdef9x if lit-if w790 true and ab,a,b or 7/9//9/abcdef/x 5 undefined 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x 0/a/b/ab//cdef9x true  or or 0/a/b/ab//cdef9x 5 undefined 0/a/b/ab/x9bc/ 2/9//9/x/bcab 1/x//x//9bcab 4/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/b/c/bc/x9/ab 0/a/b/ab/x9bc/ if lit-if w290 true and ab,a,b b,b, 2/9//9/x/bcab 5 undefined | 5/c//c/xab1/2 0/c//c/xab1/2 0 0/c//c/xab1/2 0/c//c/xab1/2 0 4/c/d/cd/ab/ef9x 1/a//a//bcdef9x 0 0/a//a//bcdef9x 0/a//a//bcdef9x 0 4/c//c/x9b/ab 0/c//c/x9b/ab 0 | 0/1/c/1c/xab/2 0/1/c/1c/xab/2 none 0/9/x/9x/abcdef/ 0/9/x/9x/abcdef/ none 0/9/b/9b/x/cab | 3/b//b/xa/1c2 0/b//b/xa/1c2 2/b//b/a/cdef9x 0/b//b/a/cdef9x 3/b//b/x9/cab | 0/b//b/x9/cab 3 0 0/c/2/c2/xab1/ TypeError TypeError 0 0/a//a/x/b1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/1c2 replaced 0/b//b/xa/1c2 0 0 0/b//b/xa/1c2 TypeError TypeError 0 0/b//b/xa/1c2 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 2 0 0/c/d/cd/ab/ef9x TypeError TypeError 0 0/a//a//bcdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab//cdef9x replaced 0/b//b/a/cdef9x 0 0 0/b//b/a/cdef9x TypeError TypeError 0 0/b//b/a/cdef9x 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x/ 6 0 0/c/a/ca/x9b/b TypeError TypeError 0 0/a//a/x9bc/b 0/a//ab/x/ 0/a//ab/x/ 0/a//ab/x9bc/ replaced calls=105 inner=1a,1a,1a,1a,1a,1a,1a,1a,1a,1a,1a,1a,1a,1a,1a`

// regexpExecLeafForTest is exec's leaf, whose dead r.regexpExecDead is.
func regexpExecLeafForTest(r *Runtime) *regexpLeaf[struct{}] {
	return (*regexpLeaf[struct{}])(unsafe.Add(unsafe.Pointer(r.regexpExecDead), -int(unsafe.Offsetof(regexpLeaf[struct{}]{}.dead))))
}

// A call of exec whose result nothing reads makes the match, the legacy
// statics and lastIndex as any other: each answer is Node's, in the tree
// tier and in the interpreter, with the collector idle.
func TestRegExpDeadExec(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		gc := debug.SetGCPercent(-1)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, regexpDeadTestSrc))
		dummy := regexpExecLeafForTest(r).dummy
		r.Close()
		debug.SetGCPercent(gc)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpDeadTestWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, regexpDeadTestWant)
		}
		// The tree tier's calls gave back the dummy; the interpreter's are
		// made as before.
		if (dummy != nil) != tree {
			t.Errorf("tree tier %v: exec's dummy made: %v", tree, dummy != nil)
		}
	}
	SetTreeTier(true)
}

// regexpDeadKindsSrc has a function for each way the tree tier builds a
// call of exec whose result nothing reads: on a RegExp an upvalue holds,
// a global, a literal, global or not, a local, a property of a local and
// of this, this itself, and the calls tested for truth -- an if, a while,
// a not, an &&, a ternary.
const regexpDeadKindsSrc = `var kgre = /(b)/g, y, h
function kOuter() { var up = /(a)(b)/, upg = /(b)/g; return [function (s) { for (var i = 0; i < 3; i++) up.exec(s) }, function (s) { for (var i = 0; i < 3; i++) upg.exec(s) }] }
var kUp = kOuter()[0], kUpG = kOuter()[1]
function kGlobal(s) { for (var i = 0; i < 3; i++) kgre.exec(s) }
function kLit(s) { for (var i = 0; i < 3; i++) /(a)(b)/.exec(s) }
function kLitG(s) { for (var i = 0; i < 3; i++) /(a)(b)/g.exec(s) }
function kLocal(s, re) { for (var i = 0; i < 3; i++) re.exec(s) }
function kProp(s, o) { for (var i = 0; i < 3; i++) o.re.exec(s) }
function kThisProp(s) { for (var i = 0; i < 3; i++) this.re.exec(s) }
function kThis(s) { for (var i = 0; i < 3; i++) this.exec(s) }
function kIf(s, re) { var n = 0; for (var i = 0; i < 3; i++) if (re.exec(s)) n++; return n }
function kWhile(s, re) { var n = 0; while (re.exec(s)) n++; return n }
function kNot(s, re) { var n = 0; for (var i = 0; i < 3; i++) n += !re.exec(s); return n }
function kAnd(s, re) { var n = 0; for (var i = 0; i < 3; i++) n += re.exec(s) && 1; return n }
function kTernary(s, re) { var n = 0; for (var i = 0; i < 3; i++) n += re.exec(s) ? 2 : 1; return n }
function many(s, n) { for (var i = 0; i < n; i++) kgre.lastIndex = 0, kUp(s) }`

// Each of the tree tier's ways gives back the dummy for the array, and the
// statics and lastIndex are what the call made, as Node has them; in the
// interpreter, which makes every call as before, the dummy is never made.
// A loop of such calls makes nothing for each.
func TestRegExpDeadExecKinds(t *testing.T) {
	for _, c := range []struct{ call, want string }{
		{"kUp('xab'), RegExp.lastMatch + RegExp.$2", "abb"},
		{"kUpG('xbxb'), RegExp.lastMatch + RegExp.leftContext", "bxbx"},
		{"kgre.lastIndex = 0, kGlobal('bxbb'), kgre.lastIndex + RegExp.leftContext", "4bxb"},
		{"kLit('zab'), RegExp.$1 + RegExp.leftContext", "az"},
		{"kLitG('zab'), RegExp.$2 + RegExp.rightContext", "b"},
		{"kLocal('qab', /(q)(a)/), RegExp.$1 + RegExp.$2", "qa"},
		{"y = /(a)/y, kLocal('aaa', y), y.lastIndex + RegExp.leftContext", "3aa"},
		{"kProp('cab', { re: /(c)/ }), RegExp.$1", "c"},
		{"kThisProp.call({ re: /(d)(.)/ }, 'xdz'), RegExp.$2", "z"},
		{"h = /(e)/g, kThis.call(h, 'eeee'), h.lastIndex + RegExp.lastMatch", "3e"},
		{"kIf('ab', /a/) + RegExp.lastMatch", "3a"},
		{"kWhile('a1a2a3', /a(.)/g) + RegExp.$1", "33"},
		{"kNot('ab', /b/) + ',' + kNot('ab', /c/)", "0,3"},
		{"kAnd('ab', /b/) + ',' + kAnd('ab', /c/)", "3,0"},
		{"kTernary('ab', /b/) + ',' + kTernary('ab', /c/)", "6,3"},
	} {
		for _, tree := range []bool{true, false} {
			SetTreeTier(tree)
			r := New(Config{})
			if _, err := r.Run(compileForTest(t, regexpDeadKindsSrc)); err != nil {
				t.Fatal(err)
			}
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			dummy := regexpExecLeafForTest(r).dummy
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("tree tier %v: %s = %s, want Node's %s", tree, c.call, got, c.want)
			}
			if (dummy != nil) != tree {
				t.Errorf("tree tier %v: %s made exec's dummy: %v", tree, c.call, dummy != nil)
			}
		}
	}
	SetTreeTier(true)

	// What a loop of them makes: nothing for each call, where an array
	// and two strings had been made.
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, regexpDeadKindsSrc)); err != nil {
		t.Fatal(err)
	}
	none, lots := compileForTest(t, "many('xab', 0)"), compileForTest(t, "many('xab', 2000)")
	mallocs := func(f *bytecode.Function) uint64 {
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if _, err := r.Run(f); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&b)
		return b.Mallocs - a.Mallocs
	}
	mallocs(lots)
	if base, got := mallocs(none), mallocs(lots); got > base+600 {
		t.Errorf("6000 calls whose results nothing reads made %d objects, %d more than none", got, got-base)
	}
}

// Where the match fails -- spends the step budget -- a call whose result
// nothing reads throws what a call whose result is read throws: the
// SyntaxError, its stack, Error.stackTraceLimit's getter run as often, and
// lastIndex as it was, global or not, in the tree tier and in the
// interpreter. (Each match spends the budget: half a second.)
func TestRegExpDeadExecComplexity(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the step budget")
	}
	src := `var gets = 0, limit = Error.stackTraceLimit;
Object.defineProperty(Error, 'stackTraceLimit', { get() { gets++; return limit }, set(v) { limit = v }, configurable: true });
var bad = 'a'.repeat(32) + 'cb';
function fail(re, e) { return [e.name, e.message, e.stack, gets, re.lastIndex].join(',') }
function dead(re) { gets = 0; for (var k = 0; k < 1; k++) try { re.exec(bad); return 'none' } catch (e) { return fail(re, e) } }
function deadIf(re) { gets = 0; for (var k = 0; k < 1; k++) try { if (re.exec(bad)) return 'm'; return 'none' } catch (e) { return fail(re, e) } }
function used(re) { gets = 0; for (var k = 0; k < 1; k++) try { var m = re.exec(bad); return String(m) } catch (e) { return fail(re, e) } }
var out = [];
for (var re of RES) {
  var u = used(re), d = dead(re), i = RES.length > 1 ? deadIf(re) : d;
  out.push(u === d && u === i ? u : [u, d, i].join(' != '));
}
out.join('|')`
	fail := "SyntaxError,regular expression is too complex,,0,0"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		// The interpreter, which makes these calls as before, of one.
		res, want := "var RES = [/(a+)+b/, /(a+)+b/g];", fail+"|"+fail
		if !tree {
			res, want = "var RES = [/(a+)+b/];", fail
		}
		v, err := r.Run(compileForTest(t, res+src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// A deadline stops a call of exec whose result nothing reads on a match
// that would run for ever, and the runtime runs on, its calls made as
// before.
func TestRegExpDeadExecDeadline(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function f(re, s) { for (var i = 0; i < 2; i++) re.exec(s) }`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	if _, err := r.Run(compileForTest(t, `f(/(a+)+b/, 'a'.repeat(64) + 'cb')`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v, want the deadline", err)
	}
	r.SetContext(context.Background())
	if v, err := r.Run(compileForTest(t, `f(/(a+)b/, 'xaab'), RegExp.$1 + RegExp.leftContext`)); err != nil || v.String().Go() != "aax" {
		t.Fatalf("after: %v, %v", v, err)
	}
	if regexpExecLeafForTest(r).dead != nil {
		t.Error("exec's leaf still told of a call")
	}
}

// regexpDeadReplaceTestSrc replaces by RegExps where nothing reads the
// result (zzz_regexpdead.go) -- as a statement, in an if, a not, an &&,
// a ternary's arm, a comma's left side and a void -- global, sticky,
// neither and with named groups, held by an upvalue, a global, this's
// property and literals, of subjects in locals, elements and properties,
// by templates with $&, $1, $<n>, $` and $', by ” and by a function,
// reading lastIndex and the legacy statics after each; and beside them
// what must not take the dead replace: an || that keeps the string, a
// subclass's Symbol.replace and exec, an instance's Symbol.replace,
// frozen RegExps and one whose lastIndex cannot be written, a template
// that could make a string too long (replaced as before), a template
// that is an object, a pattern that is no RegExp, String.prototype.replace
// and RegExp.prototype[Symbol.replace] replaced. regexpDeadReplaceTestWant
// is Node's answer (v26.10.0, vm.runInThisContext).
const regexpDeadReplaceTestSrc = `var BT = String.fromCharCode(96)
var calls = 0, seen = []
function c(x) { return x.length < 40 ? x : '#' + x.length }
function st(re) { return [re.lastIndex, RegExp.$1, RegExp.$2, RegExp.lastMatch, c(RegExp.leftContext), c(RegExp.rightContext), RegExp.input.length].join('/') }
var gre = /(b)(c)?/g
function router() {
  var up = /(a)(b)/, upg = /(\d)/g, ups = /(x)/y, upn = /(?<n>o)/g
  return function blk(ss) {
    var out = []
    for (var i = 0; i < ss.length; i++) {
      var s = ss[i]
      s.replace(up, ''); out.push(st(up))
      ss[i].replace(upg, '<$1>'); out.push(st(upg))
      ups.lastIndex = 1; ss[i].replace(ups, '$&$&'); out.push(st(ups))
      s.replace(gre, '$' + BT + "$'"); out.push(st(gre))
      s.replace(upn, '$<n>'); out.push(st(upn))
      ss[i].replace(/(e)(f)/g, ''); out.push(st(/z/))
      ss[i].replace(/(e)(f)?/, 'x'); out.push(st(/z/))
      if (s.replace(upg, '')) out.push('if')
      out.push(!s.replace(up, ''), s.replace(gre, '') && 'and', s.replace(up, '-') || 'or')
      ss[i].replace(upg, function (m, g, at, all) { calls++; seen.push(m + g + at + all.length); return 'F' }); out.push(st(upg))
      s.replace('b', ''); s.replace(/a/g, 1); out.push(st(/z/))
      i % 2 ? s.replace(up, '') : s.replace(upg, '')
      out.push(st(upg), (s.replace(up, ''), 5), String(void s.replace(up, '')))
    }
    return out.join(' ')
  }
}
var blk = router()
function Holder(re) { this.re = re }
Holder.prototype.run = function (ss) {
  var out = []
  for (var i = 0; i < ss.length; i++) { ss[i].replace(this.re, '_'); out.push(st(this.re)); var o = { s: ss[i] }; o.s.replace(this.re, ''); out.push(st(this.re)) }
  return out.join(' ')
}
function others(ss) {
  var out = []
  class R extends RegExp { [Symbol.replace](s, t) { calls++; return super[Symbol.replace](s, t) } }
  class R2 extends RegExp { exec(s) { calls++; return super.exec(s) } }
  var sub = new R('(a)', 'g'), sub2 = new R2('(b)', 'g')
  var own = /(c)/g; own[Symbol.replace] = function (s, t) { calls++; return 'own' }
  var fz = Object.freeze(/(a)/g), fzs = Object.freeze(/(a)/)
  var nw = /(a)/g; Object.defineProperty(nw, 'lastIndex', { value: 0, writable: false })
  var big = 'x'.repeat(30000) + 'b', bre = /b/g, many = ('$' + BT).repeat(10)
  var orig = String.prototype.replace, rorig = RegExp.prototype[Symbol.replace]
  var up = /(a)(b)/g
  var tpl = { toString() { calls++; return '' } }
  for (var i = 0; i < ss.length; i++) {
    var s = ss[i]
    s.replace(sub, ''); out.push(sub.lastIndex)
    s.replace(sub2, ''); out.push(sub2.lastIndex)
    ;/()/.exec('')
    s.replace(own, ''); out.push(st(own))
    try { s.replace(fz, ''); out.push('fz') } catch (e) { out.push(e.name) }
    s.replace(fzs, ''); out.push(st(fzs))
    try { s.replace(nw, ''); out.push('nw') } catch (e) { out.push(e.name) }
    big.replace(bre, many); out.push(bre.lastIndex, RegExp.leftContext.length)
    s.replace(up, tpl); out.push(st(up))
    s.replace(42, ''); s.replace(up)
    out.push(st(up))
    String.prototype.replace = function (re, t) { calls++; return orig.call(this, re, t) }
    s.replace(up, ''); out.push(st(up))
    String.prototype.replace = orig
    RegExp.prototype[Symbol.replace] = function (s, t) { calls++; return rorig.call(this, s, t) }
    s.replace(up, ''); out.push(st(up))
    RegExp.prototype[Symbol.replace] = rorig
  }
  return out.join(' ') + ' calls=' + calls + ' seen=' + seen.join(',')
}
var ss = ['xab1c2', 'ef', 'abcdef9x', '', 'x9bcab', 'foo1o', 'xxab']
var all = function () {
  return [blk(ss), blk(ss), new Holder(/(\d)(\w)/).run(ss), new Holder(/(a)/y).run(ss), others(ss)].join(' | ')
}
all() + ' || ' + all() + ' || ' + all()
`

const regexpDeadReplaceTestWant = `0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/b/9b/x/cab/6 0/9/b/9b/x/cab/6 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 | 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 1/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 | 0 0 0//////0 TypeError 0/a//a/x/b1c2/6 TypeError 0 30000 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a//bcdef9x/8 TypeError 0 30000 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/x9bc/b/6 TypeError 0 30000 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/xx/b/4 TypeError 0 30000 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 calls=57 seen=1136,2256,9968,9916,1135,1136,2256,9968,9916,1135 || 0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/b/9b/x/cab/6 0/9/b/9b/x/cab/6 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 | 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 1/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 | 0 0 0//////0 TypeError 0/a//a/x/b1c2/6 TypeError 0 30000 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a//bcdef9x/8 TypeError 0 30000 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/x9bc/b/6 TypeError 0 30000 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/xx/b/4 TypeError 0 30000 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 calls=114 seen=1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135 || 0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/a/b/ab/x/1c2/6 0/2//2/xab1c//6 0/2//2/xab1c//6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 0/b//b/xa/1c2/6 if false and x-1c2 0/2//2/xab1c//6 0///a/x/b1c2/6 0/2//2/xab1c//6 5 undefined 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/e/f/ef///2 0/e/f/ef///2 if false and ef 0/e/f/ef///2 0/e/f/ef///2 0/e/f/ef///2 5 undefined 0/a/b/ab//cdef9x/8 0/9//9/abcdef/x/8 0/9//9/abcdef/x/8 0/b/c/bc/a/def9x/8 0/b/c/bc/a/def9x/8 0/e/f/ef/abcd/9x/8 0/e/f/ef/abcd/9x/8 if false and -cdef9x 0/9//9/abcdef/x/8 0///a//bcdef9x/8 0/9//9/abcdef/x/8 5 undefined 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 true  or 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 5 undefined 0/a/b/ab/x9bc//6 0/9//9/x/bcab/6 0/9//9/x/bcab/6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 0/b//b/x9bca//6 if false and x9bc- 0/9//9/x/bcab/6 0///a/x9bc/b/6 0/9//9/x/bcab/6 5 undefined 0/a/b/ab/x9bc//6 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 0/o//o/foo1//5 0/o//o/foo1//5 0/o//o/foo1//5 if false and foo1o 0/1//1/foo/o/5 0/1//1/foo/o/5 0/1//1/foo/o/5 5 undefined 0/a/b/ab/xx//4 0/a/b/ab/xx//4 2/x//x/x/ab/4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 0/b//b/xxa//4 if false and xx- 0/a/b/ab/xx//4 0///a/xx/b/4 0///a/xx/b/4 5 undefined | 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/1/c/1c/xab/2/6 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/x/9x/abcdef//8 0/9/b/9b/x/cab/6 0/9/b/9b/x/cab/6 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 | 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 0/1/o/1o/foo//5 1/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 0/a//a//bcdef9x/8 | 0 0 0//////0 TypeError 0/a//a/x/b1c2/6 TypeError 0 30000 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0/a/b/ab/x/1c2/6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a//bcdef9x/8 TypeError 0 30000 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0/a/b/ab//cdef9x/8 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/x9bc/b/6 TypeError 0 30000 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0/a/b/ab/x9bc//6 0 0 0//////0 TypeError 0//////0 TypeError 0 30000 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0///b/#30000//30001 0 0 0//////0 TypeError 0/a//a/xx/b/4 TypeError 0 30000 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 0/a/b/ab/xx//4 calls=171 seen=1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135,1136,2256,9968,9916,1135`

// regexpDeadReplaceKindsSrc has a function for each way the tree tier
// builds a replace whose result nothing reads: of a RegExp an upvalue
// holds, a global, a local, of a subject that is an element, a local and
// a property, a RegExp literal given, a sticky RegExp, a template, and
// one tested for truth.
const regexpDeadReplaceKindsSrc = `var rkg = /(b)/g, rs = /(a)/y
function rOuter() { var up = /(a)(b)/g; return function (ss, n) { for (var i = 0; i < n; i++) ss[i % 2].replace(up, '') } }
var rUp = rOuter()
function rGlobal(ss, n) { for (var i = 0; i < n; i++) ss[i % 2].replace(rkg, '') }
function rLocal(s, re, n) { for (var i = 0; i < n; i++) s.replace(re, '') }
function rProp(o, re, n) { for (var i = 0; i < n; i++) o.s.replace(re, '') }
function rLit(ss, n) { for (var i = 0; i < n; i++) ss[i % 2].replace(/(a)(b)/g, '') }
function rIf(s, re, n) { var k = 0; for (var i = 0; i < n; i++) if (s.replace(re, '')) k++; return k }
function rSticky(s, re, n) { for (var i = 0; i < n; i++) { re.lastIndex = 1; s.replace(re, 'x') } }
function rTpl(s, re, n) { for (var i = 0; i < n; i++) s.replace(re, '[$1]') }
`

// A replace whose result nothing reads makes the matches, the legacy
// statics and lastIndex as any other, and calls a replacement function as
// it would: each answer is Node's, in the tree tier and in the
// interpreter, with the collector idle.
func TestRegExpDeadReplace(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		gc := debug.SetGCPercent(-1)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, regexpDeadReplaceTestSrc))
		r.Close()
		debug.SetGCPercent(gc)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != regexpDeadReplaceTestWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, regexpDeadReplaceTestWant)
		}
	}
	SetTreeTier(true)
}

// regexpDeadReplaceKinds are calls of regexpDeadReplaceKindsSrc's
// functions, each three times round its loop, with Node's answers, and the
// call that makes the loop's count n.
var regexpDeadReplaceKinds = []struct{ call, want, many string }{
	{"rUp(['xabab', 'zab'], 3), RegExp.lastMatch + RegExp.leftContext", "abxab", "rUp(['xabab', 'zab'], n)"},
	{"rkg.lastIndex = 5, rGlobal(['bxb', 'cbbc'], 3), rkg.lastIndex + RegExp.leftContext + RegExp.$1", "0bxb", "rGlobal(['bxb', 'cbbc'], n)"},
	{"rLocal('qab', /(q)(a)/, 3), RegExp.$1 + RegExp.$2 + RegExp.rightContext", "qab", "rLocal('qab', /(q)(a)/, n)"},
	{"rProp({ s: 'cabc' }, /(c)/g, 3), RegExp.leftContext + RegExp.$1", "cabc", "rProp({ s: 'cabc' }, /(c)/g, n)"},
	{"rLit(['xabab', 'zab'], 3), RegExp.$2 + RegExp.leftContext", "bxab", "rLit(['xabab', 'zab'], n)"},
	{"rIf('ab', /a/, 3) + RegExp.lastMatch + rIf('', /a/, 3)", "3a0", "rIf('ab', /a/, n)"},
	{"rSticky('aaa', rs, 3), rs.lastIndex + RegExp.leftContext", "2a", "rSticky('aaa', rs, n)"},
	{"rTpl('a1b2', /([0-9])/g, 3), RegExp.$1 + RegExp.leftContext", "2a1b", "rTpl('a1b2', /([0-9])/g, n)"},
}

// Each of the tree tier's ways makes the replace's matches, statics and
// lastIndex as Node has them, in the tree tier and in the interpreter; and
// in the tree tier a loop of them makes no string for each.
func TestRegExpDeadReplaceKinds(t *testing.T) {
	for _, c := range regexpDeadReplaceKinds {
		for _, tree := range []bool{true, false} {
			SetTreeTier(tree)
			r := New(Config{})
			if _, err := r.Run(compileForTest(t, regexpDeadReplaceKindsSrc)); err != nil {
				t.Fatal(err)
			}
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("tree tier %v: %s = %s, want Node's %s", tree, c.call, got, c.want)
			}
			if tree {
				if made := regexpDeadMallocs(t, r, c.many, 2000); made > 400 {
					t.Errorf("%s: 2000 replaces whose results nothing reads made %d objects", c.many, made)
				}
			}
			r.Close()
		}
	}
	SetTreeTier(true)
}

// regexpDeadMallocs is how many more objects r makes running call, a
// source of n, with n of count than with n of 0.
func regexpDeadMallocs(t *testing.T, r *Runtime, call string, count int) uint64 {
	t.Helper()
	none := compileForTest(t, "var n = 0; "+call)
	lots := compileForTest(t, "var n = "+strconv.Itoa(count)+"; "+call)
	mallocs := func(f *bytecode.Function) uint64 {
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		if _, err := r.Run(f); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&b)
		return b.Mallocs - a.Mallocs
	}
	mallocs(lots)
	base, got := mallocs(none), mallocs(lots)
	if got < base {
		return 0
	}
	return got - base
}

// Where the match fails -- spends the step budget -- a replace whose result
// nothing reads throws what one whose result is read throws: the
// SyntaxError, its stack, Error.stackTraceLimit's getter run as often, and
// lastIndex, global or not, in the tree tier and in the interpreter.
// (Each match spends the budget: half a second.)
func TestRegExpDeadReplaceComplexity(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the step budget")
	}
	src := `var gets = 0, limit = Error.stackTraceLimit;
Object.defineProperty(Error, 'stackTraceLimit', { get() { gets++; return limit }, set(v) { limit = v }, configurable: true });
var bad = 'a'.repeat(32) + 'cb';
function fail(re, e) { return [e.name, e.message, e.stack, gets, re.lastIndex].join(',') }
function dead(re) { gets = 0; for (var k = 0; k < 1; k++) try { bad.replace(re, ''); return 'none' } catch (e) { return fail(re, e) } }
function used(re) { gets = 0; for (var k = 0; k < 1; k++) try { var m = bad.replace(re, ''); return m } catch (e) { return fail(re, e) } }
var out = [];
for (var re of RES) {
  re.lastIndex = 3;
  var u = used(re); re.lastIndex = 3; var d = dead(re);
  out.push(u === d ? u : u + ' != ' + d);
}
out.join('|')`
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		res, want := "var RES = [/(a+)+b/, /(a+)+b/g];", "SyntaxError,regular expression is too complex,,0,3|SyntaxError,regular expression is too complex,,0,0"
		if !tree {
			res, want = "var RES = [/(a+)+b/g];", "SyntaxError,regular expression is too complex,,0,0"
		}
		v, err := r.Run(compileForTest(t, res+src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// A deadline stops a replace whose result nothing reads on a match that
// would run for ever, and the runtime runs on, its replaces made as
// before.
func TestRegExpDeadReplaceDeadline(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function f(re, s) { for (var i = 0; i < 2; i++) s.replace(re, '') }`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	if _, err := r.Run(compileForTest(t, `f(/(a+)+b/g, 'a'.repeat(64) + 'cb')`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v, want the deadline", err)
	}
	r.SetContext(context.Background())
	if v, err := r.Run(compileForTest(t, `f(/(a+)b/g, 'xaab'), RegExp.$1 + RegExp.leftContext`)); err != nil || v.String().Go() != "aax" {
		t.Fatalf("after: %v, %v", v, err)
	}
	if *r.strReplaceDead != nil {
		t.Error("replace still told of a call")
	}
}

// The replace without its string refuses, having done nothing, where its
// string could be too long to be one -- which the replace that makes it
// throws for -- and makes the matches otherwise.
func TestRegExpDeadReplaceTooLong(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	rx, err := r.Run(compileForTest(t, `var re = /b/g; re.lastIndex = 7; re`))
	if err != nil {
		t.Fatal(err)
	}
	s := NewString(strings.Repeat("x", 30000) + "b")
	fl, ok := r.replaceDirect(rx)
	if !ok {
		t.Fatal("not a direct replace")
	}
	if _, ok, _ := regexpReplaceDead[struct{}](r, rx, s, NewString(strings.Repeat("$`", 10)), fl); ok {
		t.Error("a replace that could be too long was made without its string")
	}
	if v, err := r.Run(compileForTest(t, `'' + re.lastIndex`)); err != nil || v.String().Go() != "7" {
		t.Errorf("lastIndex %v, %v after the refusal, want 7", v, err)
	}
	if _, ok, err := regexpReplaceDead[struct{}](r, rx, s, emptyString, fl); !ok || err != nil {
		t.Errorf("the replace by '' refused: %v", err)
	}
	if v, err := r.Run(compileForTest(t, `re.lastIndex + ',' + RegExp.leftContext.length`)); err != nil || v.String().Go() != "0,30000" {
		t.Errorf("after the replace: %v, %v, want 0,30000", v, err)
	}
}
