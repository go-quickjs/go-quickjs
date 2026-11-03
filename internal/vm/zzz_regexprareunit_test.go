package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// regexpRareUnitTestSrc matches Octane's pattern for a date in JSON, whose
// required literal begins with a quotation mark, against JSON past ASCII
// -- matched by its code units -- full of quotation marks and of Q, the
// literal's rarest unit, which its search looks for (regexp's rareUnit):
// replaces whose result is measured, and a non-global pattern's exec.
// regexpRareUnitTestWant is Node's answer (v26.10.0).
const regexpRareUnitTestSrc = `var re = /(^|[^\\])"\\\/Qngr\((-?[0-9]+)\)\\\/"/g, re1 = /"\\\/Qngr\((\d+)\)/;
function variants(s) {
  var o = [s];
  for (var i = 1; i < 40; i++) o.push(s.slice(0, i * 3 % s.length) + 'Q' + s.slice(i * 3 % s.length));
  return o;
}
function run() {
  var base = '{"Q":"\u2030","QQ":"Q\\"","x":"\\/Qngr(12)\\/","Qngr":"\\/Qngr","y":"\\/Qngr(345)\\/"}';
  var vs = variants(base + base), sum = 0, out = [];
  for (var i = 0; i < vs.length; i++) {
    sum += vs[i].replace(re, '').length;
    var m = re1.exec(vs[i]);
    if (m) sum += m.index * 7 + m[1].length;
  }
  out.push(sum, base.replace(re, '#'), JSON.stringify(re1.exec(base)));
  return out.join(' ');
}
`

const regexpRareUnitTestWant = `11374 {"Q":"‰","QQ":"Q\"","x"#,"Qngr":"\/Qngr","y"#} ["\"\\/Qngr(12)","12"]`

// A search for a pattern's required literal over a subject's code units
// finds Node's matches, in the tree tier and the interpreter, with the
// collector idle and running.
func TestRegExpRareUnit(t *testing.T) {
	for _, idle := range []bool{true, false} {
		for _, tree := range []bool{true, false} {
			if idle {
				runtime.GC()
				debug.SetGCPercent(-1)
			}
			SetTreeTier(tree)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, regexpRareUnitTestSrc+"run() + ' # ' + run()"))
			r.Close()
			debug.SetGCPercent(100)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := v.String().Go(), regexpRareUnitTestWant+" # "+regexpRareUnitTestWant; got != want {
				t.Errorf("tree tier %v, collector idle %v:\ngot  %s\nwant %s", tree, idle, got, want)
			}
		}
	}
	SetTreeTier(true)
}
