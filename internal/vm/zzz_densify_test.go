package vm

import "testing"

// densifySrc fills arrays that are sparse while they are filled
// (zzz_densify.go): from the end down, from 10 to 70,000 elements; new
// Array(n) past the length its holes are written in for, filled up and
// every other element; an array given its last element first; one with
// named properties around its elements; one with an index read-only, and
// one with an index a getter, which stay sparse; one filled every third
// element; and each written past its end, deleted from and shortened
// after. summary is its length, elements, holes, sum, keys and own names;
// then arrays shortened below their dense storage and grown again;
// a sparse array that is a prototype, forEach over holes, reduce,
// slice, map and filter, join with named properties, and a frozen one.
// densifyWant is Node's answer (v26.10.0).
const densifySrc = `function down(n) { var a = []; for (var i = n - 1; i >= 0; i--) a[i] = i * 2; return a }
function upNew(n) { var a = new Array(n); for (var i = 0; i < n; i++) a[i] = i; return a }
function half(n) { var a = new Array(n); for (var i = 0; i < n; i += 2) a[i] = i; return a }
function presize(n) { var a = []; a[n - 1] = -1; for (var i = 0; i < n - 1; i++) a[i] = i; return a }
function named(n) { var a = []; a.first = 'f'; a[n - 1] = 'last'; a.second = 's'; for (var i = n - 2; i >= 0; i--) a[i] = i; a.third = 't'; return a }
function frozenIdx(n) { var a = []; a[n - 1] = 'x'; Object.defineProperty(a, n - 2, { value: 'ro', writable: false, enumerable: true, configurable: true }); for (var i = n - 3; i >= 0; i--) a[i] = i; return a }
function getterIdx(n) { var a = []; a[n - 1] = 'x'; Object.defineProperty(a, n - 2, { get() { return 'g' }, enumerable: true, configurable: true }); for (var i = n - 3; i >= 0; i--) a[i] = i; return a }
function sparseGaps(n) { var a = []; for (var i = n - 1; i >= 0; i -= 3) a[i] = i; return a }
function summary(a) {
  var s = 0, holes = 0, cnt = 0;
  for (var i = 0; i < a.length; i++) { if (i in a) { cnt++; if (typeof a[i] === 'number') s += a[i] } else holes++ }
  var keys = Object.keys(a);
  return a.length + '/' + cnt + '/' + holes + '/' + s + '/' + keys.slice(0, 3).join(',') + '..' + keys.slice(-4).join(',') + '/' + Object.getOwnPropertyNames(a).length;
}
function after(a) {
  // Writes, deletes, length changes after the array is filled.
  a[a.length + 3] = 'past'; delete a[1]; var l = a.length; a.length = 5; return l + ':' + a.length + ':' + JSON.stringify(a) + ':' + (1 in a) + ':' + a.indexOf(4);
}
function protoArr(n) { var p = []; var o = Object.create(p); p[n - 1] = 1; for (var i = n - 2; i >= 0; i--) p[i] = i; return o[5] + ':' + o[n - 1] + ':' + p.length }
function shrunk() {
  var a = []; a[2000] = 1; a.length = 0; a[0] = 'x'; a[1] = 'y';
  var b = []; b[5000] = 1; b.length = 3; for (var i = 0; i < 10; i++) b.push(i);
  var c = new Array(70000); c.length = 2; for (var i = 0; i < 40000; i++) c[i] = i;
  return a.length + ':' + a.join() + '|' + b.length + ':' + b.join() + '|' + c.length + ':' + c[39999]
}
function forEachHoles(n) { var a = half(n); var c = 0; a.forEach(function () { c++ }); return c }
function main() {
  var out = [];
  for (var f of [down, upNew, half, presize, named, frozenIdx, getterIdx, sparseGaps]) {
    for (var n of [10, 1500, 5000, 70000]) {
      var a = f(n);
      out.push(f.name + n + '=' + summary(a) + '|' + after(f(n)));
    }
  }
  out.push(shrunk(), protoArr(3000), forEachHoles(70000), down(3000).reduce(function (x, y) { return x + y }, 0), JSON.stringify(down(2000).slice(1020, 1030)),
    upNew(70000).map(function (x) { return x & 1 }).filter(Boolean).length, named(2000).join('').length, Object.isFrozen(Object.freeze(down(2000))) + '' + Object.keys(Object.freeze(down(2000))).length);
  return out.join('\n');
}
`

const densifyWant = `down10=10/10/0/90/0,1,2..6,7,8,9/11|14:5:[0,null,4,6,8]:false:2
down1500=1500/1500/0/2248500/0,1,2..1496,1497,1498,1499/1501|1504:5:[0,null,4,6,8]:false:2
down5000=5000/5000/0/24995000/0,1,2..4996,4997,4998,4999/5001|5004:5:[0,null,4,6,8]:false:2
down70000=70000/70000/0/4899930000/0,1,2..69996,69997,69998,69999/70001|70004:5:[0,null,4,6,8]:false:2
upNew10=10/10/0/45/0,1,2..6,7,8,9/11|14:5:[0,null,2,3,4]:false:4
upNew1500=1500/1500/0/1124250/0,1,2..1496,1497,1498,1499/1501|1504:5:[0,null,2,3,4]:false:4
upNew5000=5000/5000/0/12497500/0,1,2..4996,4997,4998,4999/5001|5004:5:[0,null,2,3,4]:false:4
upNew70000=70000/70000/0/2449965000/0,1,2..69996,69997,69998,69999/70001|70004:5:[0,null,2,3,4]:false:4
half10=10/5/5/20/0,2,4..2,4,6,8/6|14:5:[0,null,2,null,4]:false:4
half1500=1500/750/750/561750/0,2,4..1492,1494,1496,1498/751|1504:5:[0,null,2,null,4]:false:4
half5000=5000/2500/2500/6247500/0,2,4..4992,4994,4996,4998/2501|5004:5:[0,null,2,null,4]:false:4
half70000=70000/35000/35000/1224965000/0,2,4..69992,69994,69996,69998/35001|70004:5:[0,null,2,null,4]:false:4
presize10=10/10/0/35/0,1,2..6,7,8,9/11|14:5:[0,null,2,3,4]:false:4
presize1500=1500/1500/0/1122750/0,1,2..1496,1497,1498,1499/1501|1504:5:[0,null,2,3,4]:false:4
presize5000=5000/5000/0/12492500/0,1,2..4996,4997,4998,4999/5001|5004:5:[0,null,2,3,4]:false:4
presize70000=70000/70000/0/2449895000/0,1,2..69996,69997,69998,69999/70001|70004:5:[0,null,2,3,4]:false:4
named10=10/10/0/36/0,1,2..9,first,second,third/14|14:5:[0,null,2,3,4]:false:4
named1500=1500/1500/0/1122751/0,1,2..1499,first,second,third/1504|1504:5:[0,null,2,3,4]:false:4
named5000=5000/5000/0/12492501/0,1,2..4999,first,second,third/5004|5004:5:[0,null,2,3,4]:false:4
named70000=70000/70000/0/2449895001/0,1,2..69999,first,second,third/70004|70004:5:[0,null,2,3,4]:false:4
frozenIdx10=10/10/0/28/0,1,2..6,7,8,9/11|14:5:[0,null,2,3,4]:false:4
frozenIdx1500=1500/1500/0/1121253/0,1,2..1496,1497,1498,1499/1501|1504:5:[0,null,2,3,4]:false:4
frozenIdx5000=5000/5000/0/12487503/0,1,2..4996,4997,4998,4999/5001|5004:5:[0,null,2,3,4]:false:4
frozenIdx70000=70000/70000/0/2449825003/0,1,2..69996,69997,69998,69999/70001|70004:5:[0,null,2,3,4]:false:4
getterIdx10=10/10/0/28/0,1,2..6,7,8,9/11|14:5:[0,null,2,3,4]:false:4
getterIdx1500=1500/1500/0/1121253/0,1,2..1496,1497,1498,1499/1501|1504:5:[0,null,2,3,4]:false:4
getterIdx5000=5000/5000/0/12487503/0,1,2..4996,4997,4998,4999/5001|5004:5:[0,null,2,3,4]:false:4
getterIdx70000=70000/70000/0/2449825003/0,1,2..69996,69997,69998,69999/70001|70004:5:[0,null,2,3,4]:false:4
sparseGaps10=10/4/6/18/0,3,6..0,3,6,9/5|14:5:[0,null,null,3,null]:false:-1
sparseGaps1500=1500/500/1000/375250/2,5,8..1490,1493,1496,1499/501|1504:5:[null,null,2,null,null]:false:-1
sparseGaps5000=5000/1667/3333/4167500/1,4,7..4990,4993,4996,4999/1668|5004:5:[null,null,null,null,4]:false:4
sparseGaps70000=70000/23334/46666/816678333/0,3,6..69990,69993,69996,69999/23335|70004:5:[0,null,null,3,null]:false:-1
2:x,y|13:,,,0,1,2,3,4,5,6,7,8,9|40000:39999
5:1:3000
35000
8997000
[2040,2042,2044,2046,2048,2050,2052,2054,2056,2058]
35000
6890
true2000`

// A sparse array made dense again answers as it did: Node's answers, in the
// tree tier and in the interpreter.
func TestDensify(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, densifySrc)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, "main()"))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != densifyWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, densifyWant)
		}
	}
}

// The mechanism: an array filled from the end down, one made by new Array(n)
// past its holes' length and filled, and one given its last element first
// and filled, are dense once filled -- every element in dense storage, none
// in the table, not sparse -- and keep their named properties; one whose
// table keeps an index of other attributes, or a prototype, stays sparse.
func TestDensifyMakesDense(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, densifySrc+`;
		var p = []; Object.create(p); p[2999] = 1; for (var i = 2998; i >= 0; i--) p[i] = i;
		[down(5000), upNew(70000), presize(3000), named(3000), frozenIdx(3000), getterIdx(3000), p]`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		dense    bool
		n, named int
	}{{true, 5000, 0}, {true, 70000, 0}, {true, 3000, 0}, {true, 3000, 3}, {false, 0, 0}, {false, 0, 0}, {false, 0, 0}} {
		e, err := r.getIndexed(v, Int(i))
		if err != nil {
			t.Fatal(err)
		}
		o := e.Object()
		sparse := o.flags&objHasSparseElements != 0
		if sparse == want.dense {
			t.Errorf("array %d: sparse %v", i, sparse)
			continue
		}
		if want.dense && (len(o.elemsOf()) != want.n || int(o.valn) != want.named) {
			t.Errorf("array %d: %d dense elements and %d properties, want %d and %d", i, len(o.elemsOf()), o.valn, want.n, want.named)
		}
	}
}
