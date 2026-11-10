package vm

import "testing"

// instanceOfChangesSrc tests constructors instanceof remembers
// (instanceOfEntry) before and after each change that must be seen: the
// prototype property assigned; a Symbol.hasInstance of the constructor's
// own, by definition and through a new prototype of the constructor; the
// instance's chain changed, at its object and above it; a proxy in the
// chain; a primitive prototype, a TypeError; a bound function, a class, and
// a constructor with a property deleted. Each test runs warm, many times, so
// that the remembered answer is the one given.
const instanceOfChangesSrc = `
const out = []
function test(name, f) {
  let r
  for (let i = 0; i < 50; i++) {
    try { r = f() } catch (e) { r = e.constructor.name }
  }
  out.push(name + '=' + r)
}
function P() {}
const p = new P(), q = {}
test('plain', () => [p instanceof P, q instanceof P, 1 instanceof P, 'x' instanceof P].join())
P.prototype = {}
test('reassigned', () => [p instanceof P, new P() instanceof P].join())
const old = P.prototype
Object.defineProperty(P, Symbol.hasInstance, { value: v => v === q })
test('own hasInstance', () => [p instanceof P, q instanceof P].join())

function Q() {}
const qi = new Q()
test('before proto', () => qi instanceof Q)
const hp = Object.create(Function.prototype, { [Symbol.hasInstance]: { value: () => 'yes' } })
Object.setPrototypeOf(Q, hp)
test('ctor proto', () => qi instanceof Q)

function R() {}
const ri = new R(), other = {}
test('chain before', () => ri instanceof R)
Object.setPrototypeOf(ri, other)
test('chain object', () => ri instanceof R)
Object.setPrototypeOf(other, R.prototype)
test('chain above', () => ri instanceof R)
const deep = Object.create(Object.create(Object.create(R.prototype)))
test('deep', () => deep instanceof R)

function S() {}
const si = new S()
test('proxy before', () => si instanceof S)
let traps = 0
const viaProxy = Object.create(new Proxy(Object.create(S.prototype), { getPrototypeOf(t) { traps++; return Object.getPrototypeOf(t) } }))
test('proxy', () => [viaProxy instanceof S, traps > 0].join())
const proxied = new Proxy(si, { getPrototypeOf() { traps = -1; return null } })
test('proxy object', () => [proxied instanceof S, traps].join())

function T() {}
const ti = new T()
test('prim before', () => ti instanceof T)
T.prototype = 3
test('prim prototype', () => ti instanceof T)
test('prim prototype, primitive', () => 1 instanceof T)

function U() {}
const ui = new U()
test('bound target', () => ui instanceof U)
const B = U.bind(null)
test('bound', () => [ui instanceof B, {} instanceof B].join())
B.prototype = {}
test('bound with prototype', () => [ui instanceof B, Object.create(B.prototype) instanceof B].join())

class C {}
class D extends C {}
test('class', () => [new D() instanceof C, new C() instanceof D, new D() instanceof D].join())

function W() {}
const wi = new W()
W.extra = 1
test('before delete', () => wi instanceof W)
delete W.extra
test('after delete', () => wi instanceof W)
test('not callable', () => wi instanceof {})
out.join('\n')
`

const instanceOfChangesWant = `plain=true,false,false,false
reassigned=false,true
own hasInstance=false,true
before proto=true
ctor proto=true
chain before=true
chain object=false
chain above=true
deep=true
proxy before=true
proxy=true,true
proxy object=false,-1
prim before=true
prim prototype=TypeError
prim prototype, primitive=false
bound target=true
bound=true,false
bound with prototype=true,false
class=true,false,true
before delete=true
after delete=true
not callable=TypeError`

func TestInstanceOfKnown(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, instanceOfChangesSrc))
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		if got := v.String().Go(); got != instanceOfChangesWant {
			t.Errorf("tree tier %v:\n%s", tree, firstDifference(got, instanceOfChangesWant))
		}
		known := 0
		for _, e := range r.ctorsKnown {
			if e.ctor != nil {
				known++
			}
		}
		if known == 0 {
			t.Errorf("tree tier %v: no constructor remembered", tree)
		}
		r.Close()
	}
	SetTreeTier(true)
}

// A remembered constructor is answered from its entry: a test of one that
// stops being ordinary is not.
func TestInstanceOfKnownEntry(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `function F() {}; const o = new F(); for (let i = 0; i < 3; i++) o instanceof F; F`))
	if err != nil {
		t.Fatal(err)
	}
	c := v.Object()
	e := r.ctorsKnown[instanceOfSlot(c)]
	if e.ctor != c || e.shape != c.shape {
		t.Fatalf("F not remembered: %+v", e)
	}
	if _, err := r.Run(compileForTest(t, `Object.defineProperty(F, Symbol.hasInstance, { value: () => false })`)); err != nil {
		t.Fatal(err)
	}
	if c.shape == e.shape {
		t.Fatal("an own Symbol.hasInstance left F's layout as it was")
	}
	if _, ok := r.instanceOfKnown(c, Obj(c)); ok {
		t.Error("answered from an entry F's new layout does not prove")
	}
}
