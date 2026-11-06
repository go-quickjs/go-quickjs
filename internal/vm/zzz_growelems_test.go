package vm

import "testing"

// growElems keeps what an array has and its length, gives it the room asked
// for, and from 16,384 elements half as much again. What it adds is clear.
func TestGrowElems(t *testing.T) {
	for _, tc := range []struct{ n, more, least int }{
		{0, 1, 1}, {1, 1, 2}, {15, 1, 16}, {255, 1, 256}, {256, 1, 257}, {1024, 5000, 6024},
		{16383, 1, 16384}, {16384, 1, 24592}, {16384, 50000, 66384}, {100000, 1, 150016},
	} {
		els := make([]Value, tc.n, tc.n)
		for i := range els {
			els[i] = Float(float64(i))
		}
		g := growElems[struct{}](els, tc.more)
		if len(g) != tc.n || cap(g) < tc.least {
			t.Errorf("%d and %d more: length %d, capacity %d, want at least %d", tc.n, tc.more, len(g), cap(g), tc.least)
		}
		for i := range g {
			if g[i].Number() != float64(i) {
				t.Fatalf("%d and %d more: element %d is %v", tc.n, tc.more, i, g[i])
			}
		}
		for i, v := range g[len(g):cap(g)] {
			if v != (Value{}) {
				t.Fatalf("%d and %d more: slack %d is %v", tc.n, tc.more, i, v)
			}
		}
	}
}

// arrayGrowthTestSrc grows arrays past where growth changes, by push of one
// value and of several, by a spread, by a[a.length] = x and by a[i] = x, and
// reads them back, with pops, a shorter length and a frozen array between.
const arrayGrowthTestSrc = `
const out = []
function sum(a) { let s = 0; for (let i = 0; i < a.length; i++) s += a[i]; return s }
function pushes(n) { const a = []; for (let i = 0; i < n; i++) a.push(i); return a }
function pushes3(n) { const a = []; for (let i = 0; i < n; i++) a.push(i, 1, 2); return a }
function appends(n) { const a = []; for (let i = 0; i < n; i++) a[a.length] = i; return a }
function indexed(n) { const a = []; for (let i = 0; i < n; i++) a[i] = i; return a }
for (let r = 0; r < 3; r++) {
  for (const n of [100, 257, 1025, 5000, 16384, 16385, 18433, 40000]) {
    const a = pushes(n), b = pushes3(n), c = appends(n), d = indexed(n)
    out.push(a.length, sum(a), b.length, sum(b), c.length, sum(c), d.length, sum(d))
  }
  const a = pushes(2000)
  a.push(...pushes(3000))
  out.push(a.length, sum(a), a[1999], a[2000], a[4999], a[5000])
  while (a.length > 1000) a.pop()
  a.push('x')
  out.push(a.length, a[999], a[1000], a[1001])
  a.length = 10
  for (let i = 0; i < 2000; i++) a[a.length] = i
  out.push(a.length, a[9], a[10], a[2009], sum(a))
  const f = Object.freeze(pushes(17000))
  try { 'use strict'; f.push(1) } catch (e) { out.push(e.constructor.name) }
  out.push(f.length)
  const s = pushes(18430)
  s[18440] = 7
  out.push(s.length, s[18435] === undefined, 18435 in s, s[18440])
}
out.join('|')
`

// arrayGrowthTestWant is Node's answer.
const arrayGrowthTestWant = "100|4950|300|5250|100|4950|100|4950|257|32896|771|33667|257|32896|257|32896|1025|524800|3075|527875|1025|524800|1025|524800|5000|12497500|15000|12512500|5000|12497500|5000|12497500|16384|134209536|49152|134258688|16384|134209536|16384|134209536|16385|134225920|49155|134275075|16385|134225920|16385|134225920|18433|169878528|55299|169933827|18433|169878528|18433|169878528|40000|799980000|120000|800100000|40000|799980000|40000|799980000|5000|6497500|1999|0|2999||1001|999|x||2010|9|0|1999|1999045|TypeError|17000|18441|true|false|7|100|4950|300|5250|100|4950|100|4950|257|32896|771|33667|257|32896|257|32896|1025|524800|3075|527875|1025|524800|1025|524800|5000|12497500|15000|12512500|5000|12497500|5000|12497500|16384|134209536|49152|134258688|16384|134209536|16384|134209536|16385|134225920|49155|134275075|16385|134225920|16385|134225920|18433|169878528|55299|169933827|18433|169878528|18433|169878528|40000|799980000|120000|800100000|40000|799980000|40000|799980000|5000|6497500|1999|0|2999||1001|999|x||2010|9|0|1999|1999045|TypeError|17000|18441|true|false|7|100|4950|300|5250|100|4950|100|4950|257|32896|771|33667|257|32896|257|32896|1025|524800|3075|527875|1025|524800|1025|524800|5000|12497500|15000|12512500|5000|12497500|5000|12497500|16384|134209536|49152|134258688|16384|134209536|16384|134209536|16385|134225920|49155|134275075|16385|134225920|16385|134225920|18433|169878528|55299|169933827|18433|169878528|18433|169878528|40000|799980000|120000|800100000|40000|799980000|40000|799980000|5000|6497500|1999|0|2999||1001|999|x||2010|9|0|1999|1999045|TypeError|17000|18441|true|false|7"

// Arrays grown past where their growth changes answer in the tree tier as in
// the interpreter, as Node answers.
func TestArrayGrowth(t *testing.T) {
	var answers [2]string
	for i, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, arrayGrowthTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		answers[i] = v.String().Go()
	}
	SetTreeTier(true)
	if answers[0] != answers[1] {
		t.Errorf("tree tier:\n%s\ninterpreter:\n%s", answers[0], answers[1])
	}
	if answers[0] != arrayGrowthTestWant {
		t.Errorf("got\n%s\nwant\n%s", answers[0], arrayGrowthTestWant)
	}
}

// A large array grows by half on every path that grows one by its own
// operation: 30,000 elements end past Go's capacity of 37,376 by half and 16
// from 18,432's.
func TestArrayGrowthRoom(t *testing.T) {
	for _, src := range []string{
		`var a = []; for (var i = 0; i < 30000; i++) a.push(i); a`,
		`var a = []; for (var i = 0; i < 30000; i++) a[a.length] = i; a`,
		`var a = []; for (var i = 0; i < 30000; i++) a[i] = i; a`,
	} {
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		els := v.Object().elemsOf()
		if n := len(els); n != 30000 || cap(els) > 2*n {
			t.Errorf("%s: length %d, capacity %d", src, n, cap(els))
		}
		if cap(els) < 41000 {
			t.Errorf("%s: capacity %d, not half again's", src, cap(els))
		}
		r.Close()
	}
}
