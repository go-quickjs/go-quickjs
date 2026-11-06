package vm

import (
	"runtime"
	"testing"
	"weak"
)

// forInCursorTestSrc runs loops around, inside and after one another, which
// a used cursor must not confuse: one in another, over the same object and
// over others; over null, undefined, an array, a string and a number; left
// by a break, a return and a throw; with keys deleted and added as they go;
// stopped in a generator while others run; with a proxy's traps running
// loops at their start and in their steps, and throwing; and in recursion.
// Three rounds, so that the tree tier and native code meet them too.
const forInCursorTestSrc = `
const out = []
function keys(o) { const r = []; for (const k in o) r.push(k); return r.join() }
const a = { x: 1, y: 2, z: 3 }, b = { p: 1, q: 2 }
for (let r = 0; r < 3; r++) {
  out.push(keys(a), keys(b), keys(null), keys(undefined), keys(a), keys([7, 8, 9]), keys('hi'), keys(5))
  // One inside another, over the same object and over another.
  let s = ''
  for (const i in a) for (const j in b) s += i + j + ' '
  for (const i in a) for (const j in a) s += i + j + ' '
  out.push(s)
  // A loop inside a loop's body, finished, and the outer one going on.
  s = ''
  for (const i in a) { s += i + ':' + keys(b) + ':' + keys(null) + ';' }
  out.push(s)
  // A loop left by a break, the next taking none of its keys.
  s = ''
  for (const i in a) { s += i; break }
  for (const i in b) { s += i }
  for (const i in a) { for (const j in b) { s += j; break } s += i }
  out.push(s)
  // Left by a return.
  function first(o) { for (const k in o) return k }
  out.push(first(a), first(b), first({}), keys(a))
  // Left by a throw, and caught in the loop around it.
  s = ''
  for (const i in a) {
    try { for (const j in b) { s += j; throw new Error(i) } } catch (e) { s += e.message }
  }
  out.push(s)
  // A loop over null inside one, and one started before null's first step.
  s = ''
  for (const i in a) { for (const j in null) s += 'no'; for (const j in b) s += j; s += i }
  out.push(s)
  // Deleted and added as the loop goes.
  const d = { a: 1, b: 2, c: 3, d: 4 }
  s = ''
  for (const k in d) { s += k; delete d.c; d.e = 5 }
  out.push(s, keys(d))
  // A generator stopped in its loop while others run, and left there.
  function* g(o) { for (const k in o) yield k }
  const g1 = g(a), g2 = g(b)
  s = g1.next().value + g2.next().value + keys(a) + g1.next().value + keys(b) + g2.next().value
  s += g1.next().value + g1.next().done + g2.next().done + keys(a)
  const g3 = g(a); g3.next(); g3.return(1)
  s += keys(b)
  out.push(s)
  // A proxy whose traps run loops, in the loop's start and in its steps.
  const seen = []
  const p = new Proxy({ m: 1, n: 2 }, {
    ownKeys(t) { seen.push(keys(b)); return Reflect.ownKeys(t) },
    getOwnPropertyDescriptor(t, k) { keys(a); return Reflect.getOwnPropertyDescriptor(t, k) },
  })
  for (const k in p) seen.push(k + keys(b))
  out.push(seen.join('/'))
  // A trap that throws in a step.
  const q = new Proxy({ m: 1 }, { getOwnPropertyDescriptor() { throw new RangeError('trap') } })
  try { for (const k in q) out.push('no') } catch (e) { out.push(e.name) }
  out.push(keys(a))
  // Recursion, each level in its loop.
  function walk(o) { let s = ''; for (const k in o) s += typeof o[k] === 'object' ? k + '(' + walk(o[k]) + ')' : k; return s }
  out.push(walk({ a: { b: { c: 1, d: 2 }, e: 3 }, f: { g: 4 } }))
  // A prototype's keys, shadowed and not.
  function P() { this.own = 1 }
  P.prototype.up = 1; P.prototype.own = 2
  out.push(keys(new P()))
}
out.join('|')
`

// forInCursorTestWant is Node's answer.
const forInCursorTestWant = "x,y,z|p,q|||x,y,z|0,1,2|0,1||xp xq yp yq zp zq xx xy xz yx yy yz zx zy zz |x:p,q:;y:p,q:;z:p,q:;|xpqpxpypz|x|p||x,y,z|pxpypz|pqxpqypqz|abd|a,b,d,e|xpx,y,zyp,qqztruetruex,y,zp,q|p,q/mp,q/np,q|RangeError|x,y,z|a(b(cd)e)f(g)|own,up|x,y,z|p,q|||x,y,z|0,1,2|0,1||xp xq yp yq zp zq xx xy xz yx yy yz zx zy zz |x:p,q:;y:p,q:;z:p,q:;|xpqpxpypz|x|p||x,y,z|pxpypz|pqxpqypqz|abd|a,b,d,e|xpx,y,zyp,qqztruetruex,y,zp,q|p,q/mp,q/np,q|RangeError|x,y,z|a(b(cd)e)f(g)|own,up|x,y,z|p,q|||x,y,z|0,1,2|0,1||xp xq yp yq zp zq xx xy xz yx yy yz zx zy zz |x:p,q:;y:p,q:;z:p,q:;|xpqpxpypz|x|p||x,y,z|pxpypz|pqxpqypqz|abd|a,b,d,e|xpx,y,zyp,qqztruetruex,y,zp,q|p,q/mp,q/np,q|RangeError|x,y,z|a(b(cd)e)f(g)|own,up"

// Loops answer as Node does, whichever cursor they are given.
func TestForInCursor(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, forInCursorTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != forInCursorTestWant {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, forInCursorTestWant)
		}
	}
	SetTreeTier(true)
}

// A loop takes the last loop's cursor once that has finished: loops run one
// after another make one cursor between them, not one each.
func TestForInCursorUsedAgain(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var o = { a: 1, b: 2, c: 3 }
		function loops(n) {
			var c = 0
			for (var i = 0; i < n; i++)
				for (var k in o) c++
			return c
		}
		loops(100)`)); err != nil {
		t.Fatal(err)
	}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	v, err := r.Run(compileForTest(t, `loops(10000)`))
	runtime.ReadMemStats(&b)
	if err != nil || v.Number() != 30000 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := b.Mallocs - a.Mallocs; n > 1000 && allocsOrdinary() {
		t.Errorf("10,000 loops made %d allocations", n)
	}
}

// A cursor made done, of null or undefined, is done before its loop's first
// step, so it is never there for another loop to take. A cursor that is
// not done is not taken: the next loop has one of its own.
func TestForInCursorKept(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.startForIn(Null); err != nil {
		t.Fatal(err)
	}
	if r.forInLast != nil {
		t.Errorf("a cursor of null was kept")
	}
	o := Obj(r.NewObject())
	first, err := r.startForIn(o)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.startForIn(o)
	if err != nil {
		t.Fatal(err)
	}
	if first.Object() == second.Object() {
		t.Errorf("a loop took a cursor whose loop had not finished")
	}
	if _, ok, _ := r.iterNext(second); ok {
		t.Fatalf("an empty object's loop made a step")
	}
	third, err := r.startForIn(o)
	if err != nil {
		t.Fatal(err)
	}
	if third.Object() != second.Object() {
		t.Errorf("a loop did not take the cursor of a loop that had finished")
	}
	if st := cursorOf(third); st.done || st.idx != 0 || !st.forIn {
		t.Errorf("the cursor taken is done %v at %d, for-in %v", st.done, st.idx, st.forIn)
	}
}

// What a loop walked goes when its turn ends and each time the collector has
// run, though no loop follows it.
func TestForInCursorLetsGo(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var big = { a: 1, b: 2 }
		for (var k in big);
		big`))
	if err != nil {
		t.Fatal(err)
	}
	if r.forInLast != nil {
		t.Errorf("the turn's end left the cursor")
	}
	p := weak.Make(v.Object())
	v = Undefined
	if _, err := r.Run(compileForTest(t, `big = null`)); err != nil {
		t.Fatal(err)
	}
	if !collected(r, p) {
		t.Errorf("the object a loop walked was not collected")
	}
	r.forInLast = new(forInObject)
	r.sweptEpoch = gcEpoch.Load()
	r.sweepStaleSlots(0)
	if r.forInLast == nil {
		t.Errorf("the cursor went with no collection since the last sweep")
	}
	r.sweptEpoch = gcEpoch.Load() - 1
	r.sweepStaleSlots(0)
	if r.forInLast != nil {
		t.Errorf("a collection left the cursor")
	}
}
