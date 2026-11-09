package vm

import (
	"runtime"
	"testing"
	"weak"
)

// spreadShapeSrc spreads sources of every kind into empty literals and into
// literals with properties (zzz_spreadshape.go): {...o}, {...o, x: 1}, {...o,
// a: 9}, {a: 0, ...o} and two spreads, each way round -- a plain object,
// an empty one, frozen, sealed and not extensible ones, one with a read-only
// property, a class's instance, one with a key not enumerable, with a
// symbol enumerable and not, with a getter (logged), with index keys among
// its elements and in its table, with __proto__ an own key by a computed
// key and by JSON.parse, a dictionary, a proxy (its traps logged), an
// object keeping its prototype in its table, one of no prototype, one of
// twelve properties, an array, a string, a number, null, undefined, a
// function -- each thirty times, so that a tier's sites are warm; six
// sites spreading one layout, more than a layout keeps; a source changed
// between spreads, by a key added and deleted, an attribute, a getter and
// a freeze, its spread written; and a spread of twelve written, extended
// and deleted from. d describes an object's keys, values and attributes,
// its prototype and whether it is extensible. spreadShapeWant is Node's
// answer (v26.10.0).
const spreadShapeSrc = `var log = [];
function d(o) {
  if (o === null || typeof o !== 'object') return String(o);
  var s = (Object.getPrototypeOf(o) === Object.prototype ? 'P' : 'p') + (Object.isExtensible(o) ? 'E' : 'e') + '{';
  for (var k of Reflect.ownKeys(o)) {
    var p = Object.getOwnPropertyDescriptor(o, k);
    s += String(k) + ('value' in p ? '=' + String(p.value) : '=get') + ':' + (p.writable ? 'w' : '-') + (p.enumerable ? 'e' : '-') + (p.configurable ? 'c' : '-') + ' ';
  }
  return s + '}';
}
function one(o) { return { ...o } }
function after(o) { return { ...o, x: 1 } }
function over(o) { return { ...o, a: 9 } }
function before(o) { return { a: 0, ...o } }
function twice(o, p) { return { ...o, ...p } }
function s1(o) { return { ...o, k: 1 } }
function s2(o) { return { ...o, k: 2 } }
function s3(o) { return { ...o, k: 3 } }
function s4(o) { return { ...o, k: 4 } }
function s5(o) { return { ...o, k: 5 } }
function s6(o) { return { ...o, k: 6 } }
class Point { constructor(x, y) { this.x = x; this.y = y; this.z = 0 } get len() { return 1 } }
var big = {}; for (var i = 0; i < 12; i++) big['p' + i] = i;
var ro = { a: 1, b: 2, c: 3 }; Object.defineProperty(ro, 'b', { writable: false });
var hidden = { a: 1, b: 2, c: 3 }; Object.defineProperty(hidden, 'a', { enumerable: false });
var symE = { a: 1 }; symE[Symbol('s')] = 2;
var symH = { a: 1, b: 2 }; Object.defineProperty(symH, Symbol('h'), { value: 3, enumerable: false });
var getter = { a: 1, get b() { log.push('get b'); return this.a + 1 }, c: 3 };
var idx = { 2: 'two', a: 1, 0: 'zero' };
var idxT = { a: 1 }; Object.defineProperty(idxT, '5', { value: 'five', enumerable: true, writable: false, configurable: true }); idxT.b = 2;
var protoKey = { ['__proto__']: 'own', a: 1 };
var protoJSON = JSON.parse('{"a":1,"__proto__":{"evil":1},"b":2}');
var dict = {}; for (var i = 0; i < 40; i++) dict['d' + i] = i; for (var i = 0; i < 36; i++) delete dict['d' + i];
var proxy = new Proxy({ a: 1, b: 2 }, {
  ownKeys(t) { log.push('ownKeys'); return Reflect.ownKeys(t) },
  getOwnPropertyDescriptor(t, k) { log.push('gopd ' + String(k)); return Reflect.getOwnPropertyDescriptor(t, k) },
  get(t, k, r) { log.push('get ' + String(k)); return Reflect.get(t, k, r) } });
var polyP = { m() {} }; var poly = Object.create(polyP); poly.a = 1; poly.b = 2;
var nullP = Object.create(null); nullP.a = 1; nullP.b = 2;
var arr = [1, 2, 3]; arr.extra = 'e';
function run(n) {
  var out = [];
  var cases = [
    ['plain', { a: 1, b: 2, c: 3 }], ['empty', {}], ['frozen', Object.freeze({ a: 1, b: 2 })], ['sealed', Object.seal({ a: 1, b: 2 })],
    ['ro', ro], ['class', new Point(1, 2)], ['hidden', hidden], ['symE', symE], ['symH', symH], ['getter', getter],
    ['idx', idx], ['idxT', idxT], ['protoKey', protoKey], ['protoJSON', protoJSON], ['dict', dict], ['proxy', proxy],
    ['poly', poly], ['nullP', nullP], ['big', big], ['arr', arr], ['str', 'ab'], ['num', 5], ['nul', null], ['undef', undefined],
    ['fn', Object.assign(function f() {}, { q: 1 })], ['nonext', Object.preventExtensions({ a: 1, b: 2 })]];
  for (var j = 0; j < n; j++) {
    if (j === n - 1) log.length = 0;
    for (var c of cases) {
      var o = c[1];
      var r = [one(o), after(o), over(o), before(o), twice(o, { y: 2 }), twice({ y: 2 }, o)];
      if (j === n - 1) out.push(c[0] + ': ' + r.map(d).join(' | '));
    }
  }
  var sites = [s1, s2, s3, s4, s5, s6], src = { a: 1, b: 2 }, s = '';
  for (var j = 0; j < n; j++) for (var f of sites) { var r = f(src); if (j === n - 1) s += d(r); }
  out.push('sites: ' + s);
  // A source changed between spreads, and a result changed after.
  var m = { a: 1, b: 2 }, t = '';
  for (var j = 0; j < 6; j++) {
    var r = one(m); r.a = 'changed'; r.n = j; t += d(r) + d(m) + ';';
    if (j === 0) m.c = 3;
    if (j === 1) delete m.a;
    if (j === 2) Object.defineProperty(m, 'b', { enumerable: false });
    if (j === 3) Object.defineProperty(m, 'b', { enumerable: true, get() { return 'g' } });
    if (j === 4) Object.freeze(m);
  }
  out.push('changed: ' + t);
  var b = one(big); b.p3 = 'w'; b.extra = 1; delete b.p0;
  out.push('bigafter: ' + d(b) + ' ' + b.p11 + ' ' + ('p0' in b) + ' ' + big.p3);
  return out.join('\n') + '\nlog: ' + log.join(',');
}
`

const spreadShapeWant = `plain: PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec x=1:wec } | PE{a=9:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec c=3:wec }
empty: PE{} | PE{x=1:wec } | PE{a=9:wec } | PE{a=0:wec } | PE{y=2:wec } | PE{y=2:wec }
frozen: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
sealed: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
ro: PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec x=1:wec } | PE{a=9:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec c=3:wec }
class: PE{x=1:wec y=2:wec z=0:wec } | PE{x=1:wec y=2:wec z=0:wec } | PE{x=1:wec y=2:wec z=0:wec a=9:wec } | PE{a=0:wec x=1:wec y=2:wec z=0:wec } | PE{x=1:wec y=2:wec z=0:wec } | PE{y=2:wec x=1:wec z=0:wec }
hidden: PE{b=2:wec c=3:wec } | PE{b=2:wec c=3:wec x=1:wec } | PE{b=2:wec c=3:wec a=9:wec } | PE{a=0:wec b=2:wec c=3:wec } | PE{b=2:wec c=3:wec y=2:wec } | PE{y=2:wec b=2:wec c=3:wec }
symE: PE{a=1:wec Symbol(s)=2:wec } | PE{a=1:wec x=1:wec Symbol(s)=2:wec } | PE{a=9:wec Symbol(s)=2:wec } | PE{a=1:wec Symbol(s)=2:wec } | PE{a=1:wec y=2:wec Symbol(s)=2:wec } | PE{y=2:wec a=1:wec Symbol(s)=2:wec }
symH: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
getter: PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec x=1:wec } | PE{a=9:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec } | PE{a=1:wec b=2:wec c=3:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec c=3:wec }
idx: PE{0=zero:wec 2=two:wec a=1:wec } | PE{0=zero:wec 2=two:wec a=1:wec x=1:wec } | PE{0=zero:wec 2=two:wec a=9:wec } | PE{0=zero:wec 2=two:wec a=1:wec } | PE{0=zero:wec 2=two:wec a=1:wec y=2:wec } | PE{0=zero:wec 2=two:wec y=2:wec a=1:wec }
idxT: PE{5=five:wec a=1:wec b=2:wec } | PE{5=five:wec a=1:wec b=2:wec x=1:wec } | PE{5=five:wec a=9:wec b=2:wec } | PE{5=five:wec a=1:wec b=2:wec } | PE{5=five:wec a=1:wec b=2:wec y=2:wec } | PE{5=five:wec y=2:wec a=1:wec b=2:wec }
protoKey: PE{__proto__=own:wec a=1:wec } | PE{__proto__=own:wec a=1:wec x=1:wec } | PE{__proto__=own:wec a=9:wec } | PE{a=1:wec __proto__=own:wec } | PE{__proto__=own:wec a=1:wec y=2:wec } | PE{y=2:wec __proto__=own:wec a=1:wec }
protoJSON: PE{a=1:wec __proto__=[object Object]:wec b=2:wec } | PE{a=1:wec __proto__=[object Object]:wec b=2:wec x=1:wec } | PE{a=9:wec __proto__=[object Object]:wec b=2:wec } | PE{a=1:wec __proto__=[object Object]:wec b=2:wec } | PE{a=1:wec __proto__=[object Object]:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec __proto__=[object Object]:wec b=2:wec }
dict: PE{d36=36:wec d37=37:wec d38=38:wec d39=39:wec } | PE{d36=36:wec d37=37:wec d38=38:wec d39=39:wec x=1:wec } | PE{d36=36:wec d37=37:wec d38=38:wec d39=39:wec a=9:wec } | PE{a=0:wec d36=36:wec d37=37:wec d38=38:wec d39=39:wec } | PE{d36=36:wec d37=37:wec d38=38:wec d39=39:wec y=2:wec } | PE{y=2:wec d36=36:wec d37=37:wec d38=38:wec d39=39:wec }
proxy: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
poly: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
nullP: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
big: PE{p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec } | PE{p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec x=1:wec } | PE{p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec a=9:wec } | PE{a=0:wec p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec } | PE{p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec y=2:wec } | PE{y=2:wec p0=0:wec p1=1:wec p2=2:wec p3=3:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec }
arr: PE{0=1:wec 1=2:wec 2=3:wec extra=e:wec } | PE{0=1:wec 1=2:wec 2=3:wec extra=e:wec x=1:wec } | PE{0=1:wec 1=2:wec 2=3:wec extra=e:wec a=9:wec } | PE{0=1:wec 1=2:wec 2=3:wec a=0:wec extra=e:wec } | PE{0=1:wec 1=2:wec 2=3:wec extra=e:wec y=2:wec } | PE{0=1:wec 1=2:wec 2=3:wec y=2:wec extra=e:wec }
str: PE{0=a:wec 1=b:wec } | PE{0=a:wec 1=b:wec x=1:wec } | PE{0=a:wec 1=b:wec a=9:wec } | PE{0=a:wec 1=b:wec a=0:wec } | PE{0=a:wec 1=b:wec y=2:wec } | PE{0=a:wec 1=b:wec y=2:wec }
num: PE{} | PE{x=1:wec } | PE{a=9:wec } | PE{a=0:wec } | PE{y=2:wec } | PE{y=2:wec }
nul: PE{} | PE{x=1:wec } | PE{a=9:wec } | PE{a=0:wec } | PE{y=2:wec } | PE{y=2:wec }
undef: PE{} | PE{x=1:wec } | PE{a=9:wec } | PE{a=0:wec } | PE{y=2:wec } | PE{y=2:wec }
fn: PE{q=1:wec } | PE{q=1:wec x=1:wec } | PE{q=1:wec a=9:wec } | PE{a=0:wec q=1:wec } | PE{q=1:wec y=2:wec } | PE{y=2:wec q=1:wec }
nonext: PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec x=1:wec } | PE{a=9:wec b=2:wec } | PE{a=1:wec b=2:wec } | PE{a=1:wec b=2:wec y=2:wec } | PE{y=2:wec a=1:wec b=2:wec }
sites: PE{a=1:wec b=2:wec k=1:wec }PE{a=1:wec b=2:wec k=2:wec }PE{a=1:wec b=2:wec k=3:wec }PE{a=1:wec b=2:wec k=4:wec }PE{a=1:wec b=2:wec k=5:wec }PE{a=1:wec b=2:wec k=6:wec }
changed: PE{a=changed:wec b=2:wec n=0:wec }PE{a=1:wec b=2:wec };PE{a=changed:wec b=2:wec c=3:wec n=1:wec }PE{a=1:wec b=2:wec c=3:wec };PE{b=2:wec c=3:wec a=changed:wec n=2:wec }PE{b=2:wec c=3:wec };PE{c=3:wec a=changed:wec n=3:wec }PE{b=2:w-c c=3:wec };PE{b=g:wec c=3:wec a=changed:wec n=4:wec }PE{b=get:-ec c=3:wec };PE{b=g:wec c=3:wec a=changed:wec n=5:wec }Pe{b=get:-e- c=3:-e- };
bigafter: PE{p1=1:wec p2=2:wec p3=w:wec p4=4:wec p5=5:wec p6=6:wec p7=7:wec p8=8:wec p9=9:wec p10=10:wec p11=11:wec extra=1:wec } 11 false 3
log: get b,get b,get b,get b,get b,get b,ownKeys,gopd a,get a,gopd b,get b,ownKeys,gopd a,get a,gopd b,get b,ownKeys,gopd a,get a,gopd b,get b,ownKeys,gopd a,get a,gopd b,get b,ownKeys,gopd a,get a,gopd b,get b,ownKeys,gopd a,get a,gopd b,get b`

// TestSpreadByLayout pins that an object spread made by its layout makes
// what the definitions made: Node's answer, in the tree tier and in the
// interpreter.
func TestSpreadByLayout(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, spreadShapeSrc+";run(1)")); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, "run(30)"))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != spreadShapeWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, spreadShapeWant)
		}
	}
}

// TestSpreadAdoptsLayout pins the mechanism: a spread of a plain source
// into an empty literal takes the layout the definitions reach from the
// literal's root, the same for each object the literal makes, kept on the
// source's layout's side; the source's values copied as one block where
// they are its first, and gathered past a key not enumerable. A spread of
// such a source into a literal with a property first, or of a source of
// another realm's prototype, is made by the definitions.
func TestSpreadAdoptsLayout(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, `
		var src = {a: 1, b: 2, c: 3}
		var hid = {a: 1, b: 2, c: 3}; Object.defineProperty(hid, 'a', {enumerable: false})
		function one(o) { return {...o} }
		function first(o) { return {z: 0, ...o} }
		var x = one(src), y = one(src), h = one(hid), f = first(src)
		;[x, y, h, f, src, hid]`))
		if err != nil {
			t.Fatal(err)
		}
		get := func(i int) *Object {
			e, err := r.getIndexed(v, Int(i))
			if err != nil {
				t.Fatal(err)
			}
			return e.Object()
		}
		x, y, h, f, src, hid := get(0), get(1), get(2), get(3), get(4), get(5)
		if x.shape != y.shape || x.shape.unique || x.shape.loose || x.shape.n != 3 {
			t.Fatalf("tree %v: spreads of one layout by one literal have layouts %p and %p", tree, x.shape, y.shape)
		}
		root := x.shape.ancestorAt(0)
		if root.parent != nil || root.proto != r.proto.object {
			t.Errorf("tree %v: the spread's layout is not grown from a root of Object.prototype", tree)
		}
		sp := src.shape.side.spread
		if sp == nil || !sp.block || sp.to[0] != x.shape || sp.from[0] != root {
			t.Errorf("tree %v: the source's layout keeps %+v", tree, sp)
		}
		if hs := hid.shape.side.spread; hs == nil || hs.block || h.shape.n != 2 {
			t.Errorf("tree %v: a source with a key not enumerable keeps %+v, its spread has %d keys", tree, hs, h.shape.n)
		}
		if f.shape.n != 4 || f.shape.ancestorAt(1).key != r.atoms.intern("z") {
			t.Errorf("tree %v: {z: 0, ...src} is not z then src's keys", tree)
		}
		r.Close()
	}
}

// A source spread in a realm of another prototype keeps none of that
// realm's layouts (spreadLayout): a realm a host dropped, whose code
// spread the main realm's objects, is collected; the realm's own spreads
// of its own sources are kept by them, which go with it.
func TestSpreadKeepsNoRealm(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	src, err := r.Run(compileForTest(t, `class P { constructor() { this.x = 1; this.y = 2 } }; [{a: 1, b: 2}, new P]`))
	if err != nil {
		t.Fatal(err)
	}
	fn := compileForTest(t, `var own = {c: 3, d: 4}; for (var i = 0; i < 20; i++) ({...own, e: i});
		(function (o) { var r; for (var i = 0; i < 20; i++) r = {...o}; return r })`)
	var refs []weak.Pointer[Realm]
	for range 10 {
		re := r.NewRealm()
		refs = append(refs, weak.Make(re))
		f, err := r.RunIn(re, fn)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 2 {
			o, err := r.getIndexed(src, Int(i))
			if err != nil {
				t.Fatal(err)
			}
			v, err := r.call(f, Undefined, []Value{o})
			if err != nil {
				t.Fatal(err)
			}
			if got := v.Object().shape.n; got != 2 {
				t.Fatalf("the spread has %d keys", got)
			}
		}
	}
	if n := realmsAlive(r, refs); n != 0 {
		t.Errorf("%d of %d realms alive", n, len(refs))
	}
	runtime.KeepAlive(src)
}
