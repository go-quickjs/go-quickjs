package vm

import "testing"

// polyProtoSemantics exercises objects whose prototype is in their table
// (zzz_polyproto.go) -- the first ordinary object made on a prototype, by
// Object.create, a literal's __proto__, setPrototypeOf, and a class made
// for each call -- through everything that reads or changes a prototype
// or lists keys: the slot is no property a script sees, a new prototype is
// seen by every read after it, a cycle through it is refused, attributes
// and extensibility are as for any object, and such an object that comes to
// be a shared prototype is read through as one. polyProtoWant is Node's
// answer (vm.runInThisContext), main's too.
const polyProtoSemantics = `var out = [];
function F() {}
var P = { x: 1, get g() { return this.y * 10; }, set s(v) { this.sv = v; } };
var o = Object.create(P);
o.y = 2;
o.s = 5;
out.push(Object.getPrototypeOf(o) === P, o.x, o.g, o.sv, P.sv, Object.keys(o).join(), Reflect.ownKeys(o).join(),
  Object.getOwnPropertyNames(o).join(), JSON.stringify(o), JSON.stringify(Object.assign({}, o)), JSON.stringify({ ...o }),
  Object.entries(o).join(";"), Object.values(o).join(), JSON.stringify(Object.getOwnPropertyDescriptors(o)),
  o.hasOwnProperty("x"), "x" in o, "y" in o, P.isPrototypeOf(o), Object.prototype.isPrototypeOf(o), o.__proto__ === P);
var forin = [];
for (var k in o) forin.push(k);
out.push(forin.join());
// a new prototype, through each way there is to give one
var Q = { x: 3, z: 4 };
Object.setPrototypeOf(o, Q);
out.push(o.x, o.z, o.g, P.isPrototypeOf(o), Q.isPrototypeOf(o));
o.__proto__ = P;
out.push(o.x, o.z, o.g);
Reflect.setPrototypeOf(o, null);
out.push(Object.getPrototypeOf(o), o.x, "toString" in o, typeof o.toString);
Reflect.setPrototypeOf(o, F.prototype);
out.push(o instanceof F, o.x);
// a cycle through the slot is refused
var c1 = Object.create({ t: 1 });
var c2 = Object.create(c1);
try { Object.setPrototypeOf(c1, c2); out.push("no error"); } catch (e) { out.push(e instanceof TypeError); }
out.push(Reflect.setPrototypeOf(c1, c2), Object.getPrototypeOf(Object.getPrototypeOf(c2)) === Object.getPrototypeOf(c1));
// attributes: freeze, seal, delete, define, preventExtensions
var f = Object.create({ q: 1 });
f.a = 1; f.b = 2;
delete f.a;
Object.defineProperty(f, "c", { value: 3, enumerable: false, writable: true, configurable: true });
out.push(Object.keys(f).join(), Object.getOwnPropertyNames(f).join(), f.q);
Object.freeze(f);
out.push(Object.isFrozen(f), Object.isSealed(f), f.q, Reflect.set(f, "b", 9), f.b);
try { Object.setPrototypeOf(f, {}); out.push("set"); } catch (e) { out.push(e.constructor.name); }
var g = Object.create({ q: 2 });
Object.preventExtensions(g);
out.push(Object.isFrozen(g), Object.isExtensible(g), Reflect.setPrototypeOf(g, {}), Reflect.setPrototypeOf(g, Object.getPrototypeOf(g)));
var se = Object.seal(Object.create({ w: 1 }));
out.push(Object.isSealed(se), Object.isFrozen(se));
// a literal's __proto__, with super in its methods
var base = { hi() { return "base"; } };
var lit = { __proto__: base, hi() { return "lit+" + super.hi(); }, n: 1 };
out.push(lit.hi(), Object.keys(lit).join(), lit.n);
Object.setPrototypeOf(base, { other: 5 });
out.push(lit.other);
// chains of objects made each on the one before
var top = { base: 7 }, cur = top, chain = [];
for (var i = 0; i < 6; i++) { cur = Object.create(cur); cur["k" + i] = i; chain.push(cur); }
var last = chain[chain.length - 1];
out.push(last.base, last.k0, last.k5, "k3" in last, top.isPrototypeOf(last), Object.keys(last).join());
Object.setPrototypeOf(chain[2], { base: 8 });
out.push(last.base, last.k0, last.k2, last.k3);
// a read site that meets objects on many prototypes, then one twice
function rd(o) { return o.v + o.w; }
var s = 0;
for (var i = 0; i < 200; i++) s += rd(Object.create({ v: i, w: 1 }));
var shared = { v: 100, w: 1 }, a1 = Object.create(shared), a2 = Object.create(shared);
for (var i = 0; i < 50; i++) s += rd(a1) + rd(a2);
shared.v = 200;
s += rd(a1) + rd(a2);
Object.setPrototypeOf(a1, { v: 1000, w: 0 });
s += rd(a1) + rd(a2);
out.push(s);
// a write site adding past a setter on a fresh prototype
var log = 0;
function wr(o) { o.m = 1; return o.hasOwnProperty("m"); }
var own = 0;
for (var i = 0; i < 100; i++) {
  var pp = i % 10 == 0 ? { set m(v) { log += v; } } : { m0: 1 };
  if (wr(Object.create(pp))) own++;
}
out.push(own, log);
// a frozen prototype's read-only property is not shadowed
var ro = Object.freeze({ m: 5 });
var oo = Object.create(ro);
oo.m = 6;
out.push(oo.m, oo.hasOwnProperty("m"));
// classes made per call, and their subclasses
var total = 0;
for (var i = 0; i < 50; i++) {
  var K = class { constructor(a) { this.a = a; } get() { return this.a; } static s() { return 2; } };
  var k1 = new K(i), k2 = new K(i + 1);
  total += k1.get() + k2.get() + K.s() + (k1 instanceof K ? 1 : 0) + (Object.getPrototypeOf(k1) === K.prototype ? 1 : 0);
  var D = class extends K { constructor(a) { super(a); this.b = 1; } get() { return super.get() + this.b; } };
  var d = new D(i);
  total += d.get() + (d instanceof K ? 1 : 0) + (d instanceof D ? 1 : 0) + D.s();
}
out.push(total);
// a factory's prototype that becomes a shared one later
var made = [];
for (var i = 0; i < 20; i++) made.push(Object.create({ id: i }));
var later = Object.create(made[3]);
var later2 = Object.create(made[3]);
out.push(later.id, later2.id, made[3].isPrototypeOf(later2));
function H() { this.h = 1; }
H.prototype = made[5];
var hs = 0;
for (var i = 0; i < 30; i++) { var hh = new H(); hs += hh.id + hh.h; }
Object.setPrototypeOf(made[5], { id: 50 });
for (var i = 0; i < 30; i++) { var hh = new H(); hs += hh.id + hh.h; }
out.push(hs, made[5].id);
// proxies and functions as prototypes of a first object
var px = new Proxy({ pv: 1 }, { get(t, k, r) { return k === "pv" ? 42 : Reflect.get(t, k, r); } });
var op = Object.create(px);
out.push(op.pv, "pv" in op);
var fp = function () {};
fp.fx = 3;
var of = Object.create(fp);
out.push(of.fx, typeof of.call, of instanceof Function);
// Object.prototype.toString, String, and spread over a first object
var t1 = Object.create({ [Symbol.toStringTag]: "Tag" });
out.push(Object.prototype.toString.call(t1), String(Object.create({ toString() { return "ts"; } })));
// structured data
out.push(JSON.stringify([Object.create({ a: 1 }), Object.create(null)]), JSON.parse(JSON.stringify(Object.create({ a: 1 }))).a);
// getters on the first object itself, and Object.defineProperties
var dp = Object.create({ base: 1 }, { own: { value: 2, enumerable: true }, acc: { get() { return 3; }, enumerable: true } });
out.push(JSON.stringify(dp), dp.base, Object.keys(dp).join());
out.join("|");
`

const polyProtoWant = `true|1|20|5||y,sv|y,sv|y,sv|{"y":2,"sv":5}|{"y":2,"sv":5}|{"y":2,"sv":5}|y,2;sv,5|2,5|{"y":{"value":2,"writable":true,"enumerable":true,"configurable":true},"sv":{"value":5,"writable":true,"enumerable":true,"configurable":true}}|false|true|true|true|true|true|y,sv,x,g,s|3|4||false|true|1||20|||false|undefined|true||true|false|true|b|b,c|1|true|true|1|false|2|TypeError|true|false|false|true|true|true|lit+base|hi,n|1|5|7|0|5|true|true|k5|8||2|3|31803|90|10|5|false|4175|3|3|true|1710|50|42|true|3|function|true|[object Tag]|ts|[{},{}]||{"own":2,"acc":3}|1|own,acc`

func TestPolyProtoSemantics(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, polyProtoSemantics))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != polyProtoWant {
			t.Errorf("tree %v:\n got %s\nwant %s", tree, got, polyProtoWant)
		}
		r.Close()
	}
}

// The first object made on a prototype keeps it in its table, the slot a
// private property under atomProtoSlot at its first position, and its
// layout one of the runtime's for such objects, which says no prototype;
// a cache remembers it for the object's own properties, and for nothing
// found past it. A second object made on the prototype makes it one of its
// own layout, with the prototype in it, its slot cleared; so does a cache
// finding a property past it, and the layout of an object of another kind
// taking it as its prototype.
func TestPolyProtoLayout(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		var p = {k: 1}, q = {k: 2}
		var a = Object.create(p), b = Object.create(q)
		a.x = 1; b.x = 2
		function own(o) { return o.x }
		function up(o) { return o.k }
		var sum = 0
		for (var i = 0; i < 20; i++) sum += own(a) + own(b) + up(a) + up(b)
		var lit = {__proto__: {k: 3}, w: 1}
		var sp = Object.setPrototypeOf({}, {k: 4})
		var shared = {k: 5}, s1 = Object.create(shared), s2 = Object.create(shared)
		var mid = Object.create({k: 6}), deep = Object.create(mid), deep2 = Object.create(mid)
		function G() {}
		G.prototype = Object.create({k: 7})
		var g = new G()
		for (var i = 0; i < 4; i++) sum += up(deep) + up(g)
		String(sum)`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "172" {
		t.Fatalf("sum %s, want 172", got)
	}
	a, b := globalObject(r, "a"), globalObject(r, "b")
	for _, n := range []string{"a", "b", "lit", "sp", "s1", "deep"} {
		o := globalObject(r, n)
		if o.flags&objPolyProto == 0 || o.shape.keys[0] != (propKey{atomProtoSlot, propPrivate}) || o.vals()[0].Object() != o.proto() {
			t.Errorf("%s, the first object of its prototype: poly %v; want its prototype in its first slot", n, o.flags&objPolyProto != 0)
		}
	}
	if a.shape != b.shape || a.shape.unique || a.shape.loose || a.shape.proto != nil || !a.shape.seen {
		t.Errorf("a and b: shapes %p and %p; want one shared layout, of no prototype, a cache remembers", a.shape, b.shape)
	}
	for _, fn := range []string{"own", "up"} {
		for i, c := range globalObject(r, fn).fn().closure.ic {
			if c.shape == a.shape && (c.p1 != nil || c.p2 != nil || c.cell != nil) {
				t.Errorf("%s's site %d remembers prototypes of objects whose layout says none: %+v", fn, i, c)
			}
		}
	}
	if s2 := globalObject(r, "s2"); s2.flags&objPolyProto != 0 || s2.proto() != globalObject(r, "shared") {
		t.Errorf("s2, the second object of its prototype: poly %v", s2.flags&objPolyProto != 0)
	}
	// mid, the first of {k: 6}'s, is deep2's prototype, the second made on
	// it, and G.prototype, the first of {k: 7}'s, is read past by up's cache
	// for g, whose constructor's root says it: each has a layout of its own
	// now, which holds its prototype.
	for _, o := range []*Object{globalObject(r, "mid"), globalObject(r, "g").proto()} {
		if o.flags&objPolyProto != 0 || !o.shape.unique || o.shape.proto == nil || !o.vals()[0].IsUndefined() {
			t.Errorf("a prototype: poly %v, unique %v; want a layout of its own holding its prototype, its slot cleared", o.flags&objPolyProto != 0, o.shape.unique)
		}
	}
	if d := globalObject(r, "deep"); d.flags&objPolyProto == 0 || d.proto() != globalObject(r, "mid") {
		t.Errorf("deep: poly %v; want its prototype still in its slot", d.flags&objPolyProto != 0)
	}
}

// Objects made on prototypes made and dropped -- by Object.create, a
// literal's __proto__, setPrototypeOf, a class made by a factory, a
// subclass of one base made by each call -- make no shape after the first
// of each loop: what they need is the runtime's layouts of such objects.
func TestPolyProtoNoShapes(t *testing.T) {
	for _, body := range []string{
		`var o = Object.create({x: i}); o.y = 1`,
		`var o = {__proto__: {z: i}, w: 1}`,
		`var o = Object.setPrototypeOf({}, {k: i}); o.v = 1`,
		`var C = class { constructor(a) { this.a = a } get() { return this.a } }; var o = new C(i); o.get()`,
		`var D = class extends Base { constructor(a) { super(a); this.b = 1 } }; var o = new D(i)`,
		`var a = Object.create({base: i}); a.i = i; var b = Object.create(a); b.v = 1; b.base + b.i + b.v`,
	} {
		r := New(Config{})
		src := "class Base { constructor(a) { this.a = a } }\nfunction loop(n) { for (var i = 0; i < n; i++) { " + body + " } }"
		if _, err := r.Run(compileForTest(t, src+"\nloop(100)")); err != nil {
			t.Fatal(err)
		}
		before := r.shapes.count
		if _, err := r.Run(compileForTest(t, "loop(10000)")); err != nil {
			t.Fatal(err)
		}
		if after := r.shapes.count; after != before {
			t.Errorf("%s: 10,000 iterations made %d shapes", body, after-before)
		}
		r.Close()
	}
}
