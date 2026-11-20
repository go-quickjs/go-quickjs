//go:build amd64 || arm64

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const nativeInitSource = `
function Pair(a, b) { this.a = a; this.b = b }
function Forward() { this.initialize.apply(this, arguments) }
Forward.prototype.initialize = function (a, b) { this.a = a; this.b = b };
function Defaults(x, y, z) {
 if (!x) x = 0; if (!y) y = 0; if (!z) z = 0;
 this.x = x; this.y = y; this.z = z;
}
function ForwardDefaults() { this.initialize.apply(this, arguments) }
ForwardDefaults.prototype.initialize = function (x, y, z) {
 this.x = x ? x : 0; this.y = y ? y : 0; this.z = z ? z : 0;
};
function ForwardCall(a, b) { this.initialize.call(this, a, b) }
ForwardCall.prototype.initialize = Forward.prototype.initialize;
function ForwardCallDefaults(x, y, z) { this.initialize.call(this, z, y, x) }
ForwardCallDefaults.prototype.initialize = ForwardDefaults.prototype.initialize;
function makePairs(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new Pair(i, 1); sum += p.a + p.b }
 return sum;
}
function makeForward(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new Forward(i, 1); sum += p.a + p.b }
 return sum;
}
function makeDefaults(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new Defaults(i, 1); sum += p.x + p.y + p.z }
 return sum;
}
function makeForwardDefaults(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new ForwardDefaults(i, 1); sum += p.x + p.y + p.z }
 return sum;
}
function makeForwardCall(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new ForwardCall(i, 1); sum += p.a + p.b }
 return sum;
}
function makeForwardCallDefaults(n) {
 var sum = 0;
 for (var i = 0; i < n; i++) { var p = new ForwardCallDefaults(i, 1); sum += p.x + p.y + p.z }
 return sum;
}`

func BenchmarkRegNativeInit(b *testing.B) {
	for _, name := range []string{"makePairs", "makeForward", "makeDefaults", "makeForwardDefaults", "makeForwardCall", "makeForwardCallDefaults"} {
		b.Run(name, func(b *testing.B) {
			r := newTestRuntime(b, Config{})
			b.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			if _, err := r.Run(compileForTest(b, nativeInitSource)); err != nil {
				b.Fatal(err)
			}
			fn := compileForTest(b, name+"(3000)")
			if _, err := r.Run(fn); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if v, err := r.Run(fn); err != nil || v.Number() != 4501500 {
					b.Fatalf("got %v, %v", v, err)
				}
			}
		})
	}
}

func TestRegNativeInitFrameless(t *testing.T) {
	nativeCounting(t)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		for _, name := range []string{"makePairs", "makeForward", "makeDefaults", "makeForwardDefaults", "makeForwardCall", "makeForwardCallDefaults"} {
			before := atomic.LoadInt64(&nativeCounter.Load().calls)
			if got := nativeRunSrc(t, nativeInitSource+`String(`+name+`(1000))`, true); got != "500500" {
				t.Fatalf("marking %v, %s: got %s", marking, name, got)
			}
			limit := int64(20)
			if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > limit {
				t.Fatalf("marking %v, %s: entered %d frames, want at most %d", marking, name, n, limit)
			}
		}
	}
}

func TestRegNativeInitFallback(t *testing.T) {
	const constructed = `
function Pair(a, b) { "use strict"; this.a = a; this.b = b }
var p, object = { tag: 7 }, out = [], held;
for (var i = 0; i < 100; i++) p = new Pair(object, object);
out.push(p.a === object, p.b === object, Object.keys(p).join());
p = new Pair(object); out.push(p.a === object, p.b === undefined);
function Repeated(a, b) { this.a = a; this.b = b; this.a = b; this.c = a }
for (var i = 0; i < 100; i++) p = new Repeated(object, 9);
out.push(p.a, p.b, p.c === object, Object.keys(p).join());
for (var i = 0; i < 100; i++) Repeated.call(p, object, 10);
Repeated.call(p, object); out.push(p.a === undefined, p.b === undefined, p.c === object);
Repeated.apply(p, [object, 11]); out.push(p.a, p.b, p.c === object);
Object.defineProperty(Pair.prototype, "b", {
 set(v) { held = this; throw new RangeError("store:" + v) }, configurable: true
});
try { new Pair(9, 10) } catch (e) { out.push(e.name, e.message, held.a, Object.hasOwn(held, "b")) }
delete Pair.prototype.b;
Object.defineProperty(Pair.prototype, "a", { value: 1, writable: false });
try { new Pair(2, 3) } catch (e) { out.push(e.name) }
out.join("|")`
	const constructedWant = "true|true|a,b|true|true|9|9|true|a,b,c|true|true|true|11|11|true|RangeError|store:10|9|false|TypeError"
	const src = nativeInitSource + `
var out = [], log = [];
makePairs(100); makeForward(100);
function init(a, b) { this.a = a; this.b = b }
function via(a, b) { return init.apply(this, arguments) }
var o = {};
for (var i = 0; i < 100; i++) via.call(o, i, i + 1);
out.push(via.call(o, 7), o.a, String(o.b), Object.keys(o).join());
Object.defineProperty(o, "a", { set(v) { log.push("set" + v) }, configurable: true });
via.call(o, 8, 9); out.push(o.b);
Object.defineProperty(o, "a", { set(v) { throw new TypeError("setter") }, configurable: true });
try { via.call(o, 1, 2) } catch (e) { out.push(e.name, e.message, o.b) }
delete o.a;
Object.defineProperty(o, "a", { value: 4, writable: false, configurable: true });
via.call(o, 5, 6); out.push(o.a, o.b);
var empty = Object.preventExtensions({}); via.call(empty, 1, 2); out.push(Object.keys(empty).length);
var inherited = Object.create({ set a(v) { log.push("proto" + v) } });
via.call(inherited, 10, 11); out.push(inherited.b, Object.keys(inherited).join());
var proxy = new Proxy({}, { set(t, k, v) { log.push(k + v); return Reflect.set(t, k, v) } });
via.call(proxy, 12, 13); out.push(proxy.a, proxy.b);
function dup(a, b) { this.x = a; this.x = b; this.y = a }
var d; for (var i = 0; i < 100; i++) d = new dup(2, 3);
out.push(d.x, d.y, Object.keys(d).join());
function wide(a, b) {
 this.a = a; this.b = b; this.c = a; this.d = b; this.e = a; this.f = b;
 this.g = a; this.h = b; this.i = a; this.j = b; this.k = a; this.l = b;
}

for (var i = 0; i < 100; i++) d = new wide(4, 5);
out.push(d.a + d.l, Object.keys(d).join(), log.join());
out.join("|")`
	const want = "|7|undefined|a,b|9|TypeError|setter|9|4|6|0|11|b|12|13|3|2|x,y|9|a,b,c,d,e,f,g,h,i,j,k,l|set8,proto10,a12,b13"
	nativeCounting(t)
	if got := nativeRunSrc(t, constructed, false); got != constructedWant {
		t.Fatalf("Go table construction: got %q, want %q", got, constructedWant)
	}
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	var exits [2]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, constructed, true); got != constructedWant {
			t.Fatalf("marking %v construction: got %q, want %q", marking, got, constructedWant)
		}
		before := atomic.LoadInt64(&nativeCounter.Load().enters)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		exits[i] = atomic.LoadInt64(&nativeCounter.Load().enters) - before
	}
	if exits[0] != exits[1] {
		t.Fatalf("%d exits, %d while marking", exits[0], exits[1])
	}
	const forwarding = `
function factory() { return function Forward(a, b) { FORWARD } }
function init(a, b) { this.a = a; this.b = b }
var cs = [], out = [], log = [];
for (var k = 0; k < 20; k++) {
 var C = factory(); C.prototype.initialize = init; cs.push(C);
}
for (var i = 0; i < 100; i++) for (var C of cs) new C(i, 1);
out.push(new cs[19](7, 8).a);
var F = cs[0];
F.prototype.initialize = function (a, b) { this.a = b; this.b = a };
out.push(new F(2, 3).a);
Object.defineProperty(F.prototype, "initialize", {
 get() { log.push("method"); return init }, configurable: true
});
out.push(new F(4, 5).b);
Object.defineProperty(F.prototype, "initialize", { value: init, writable: true, configurable: true });
var original = Function.prototype.OP;
Object.defineProperty(init, "OP", {
 get() { log.push("OP"); return original }, configurable: true
});
out.push(new F(6, 7).a); delete init.OP;
init.OP = function (t, a, b) { t.a = 90; return 123 };
out.push(new F(8, 9).a); delete init.OP;
Function.prototype.OP = function () { this.seen = true; return Reflect.apply(original, this, arguments) };
out.push(new F(10, 11).b, init.seen); Function.prototype.OP = original;
out.push(new F(12).a, String(new F(12).b));
F.prototype.initialize = init.bind({}); out.push(String(new F(13, 14).a));
F.prototype.initialize = new Proxy(init, { apply(t, o, a) { log.push("proxy"); return Reflect.apply(t, o, a) } });
out.push(new F(15, 16).b);
F.prototype.initialize = function (a) { this.a = arguments.length; this.b = arguments[1] };
out.push(new F(17, 18).a, new F(17, 18).b);
F.prototype.initialize = function boom() { throw new RangeError("forward") };
try { new F(19, 20) } catch (e) { out.push(e.name, e.message, /boom/.test(e.stack), /Forward/.test(e.stack)) }
F.prototype.initialize = init;
Object.defineProperty(F.prototype, "b", { set(v) { throw new TypeError("store") }, configurable: true });
try { new F(21, 22) } catch (e) { out.push(e.name, e.message, /Forward/.test(e.stack)) }
delete F.prototype.b;
F.prototype = new Proxy({ initialize: init }, { get(t, k, r) { log.push(String(k)); return Reflect.get(t, k, r) } });
out.push(new F(23, 24).b);
function constants(a, b) { this.initialize.call(this, b, a, 3, null) }
constants.prototype.initialize = function (a, b, c, d) { this.a = a; this.b = b; this.c = c; this.d = d };
var p; for (var i = 0; i < 100; i++) p = new constants(25, 26);
out.push(p.a, p.b, p.c, p.d === null, log.join());
out.join("|")`
	for _, op := range []string{"apply", "call"} {
		t.Run(op, func(t *testing.T) {
			body := "this.initialize.apply(this, arguments)"
			if op == "call" {
				body = "this.initialize.call(this, a, b)"
			}
			src := strings.ReplaceAll(strings.ReplaceAll(forwarding, "FORWARD", body), "OP", op)
			want := "7|3|5|6|90|11|true|12|undefined|undefined|16|2|18|RangeError|forward|true|true|TypeError|store|true|24|26|25|3|true|method," + op + ",proxy,initialize"
			if got := nativeRunSrc(t, src, false); got != want {
				t.Fatalf("Go table: got %q, want %q", got, want)
			}
			for _, marking := range []bool{false, true} {
				nativeMarking(t, marking)
				if got := nativeRunSrc(t, src, true); got != want {
					t.Fatalf("marking %v: got %q, want %q", marking, got, want)
				}
			}
		})
	}
}

// The native initializer writes every store at its slot and gives the
// object the layout the last leaves once: a name stored twice keeps the
// last value and its first place; stores over an object's own properties
// (apply_args on a receiver that has them) leave its layout; a default
// made 0 is written with the rest; and the object made goes on as any
// other -- its keys, a property added, deleted and redefined after, and
// another object of the same constructor sharing its layout. Each with
// the body made without a frame, while the collector is idle and while
// it marks.
func TestRegNativeInitFinalLayout(t *testing.T) {
	const src = `
var object = { tag: 7 };
function Repeated(a, b) { this.a = a; this.b = b; this.a = b; this.c = a }
function Zeroes(x, y) { if (!x) x = 0; this.x = x; this.y = y; this.x = y; this.z = x }
function via(a, b) { Repeated.apply(this, arguments) }
function run(n) {
 var s = 0, p, z;
 for (var i = 0; i < n; i++) {
  p = new Repeated(object, i); z = new Zeroes(-0, i);
  s += p.a + p.b + (p.c === object) + z.x + z.y + z.z;
 }
 return s;
}
function reuse(o, n) { for (var i = 0; i < n; i++) via.call(o, i, i + 1); return o }
run(100);
var out = [String(run(1000))];
var p = new Repeated(object, 9), q = new Repeated(1, 2), z = new Zeroes(-0, 5);
out.push(p.a, p.b, p.c === object, Object.keys(p).join(), Object.keys(q).join());
out.push(Object.is(z.x, 5), Object.is(z.z, 0), Object.keys(z).join());
var o = reuse({ a: 0, b: 0, c: 0 }, 1000);
out.push(o.a, o.b, o.c, Object.keys(o).join());
p.d = 4; delete p.b; p.b = 5;
Object.defineProperty(q, "a", { get() { return "got" } });
out.push(Object.keys(p).join(), p.a, p.b, p.d, q.a, q.b, new Repeated(3, 4).a);
out.join("|")`
	const want = "1999000|9|9|true|a,b,c|a,b,c|true|true|x,y,z|1000|1000|999|a,b,c|a,c,d,b|9|5|4|got|2|4"
	nativeCounting(t)
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		// reuse's 1,000 calls of via have frames, the Repeated each applies
		// none; nor have run's 2,000 constructions but the first few.
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 1000+20 {
			t.Fatalf("marking %v: entered %d frames", marking, n)
		}
	}
}

// A base class's constructor that only stores its parameters has a plan
// (RegCode.Stores) the native interpreter makes at a construction, without
// the constructor's frame -- and only there: the class called, applied,
// or the method a forwarder calls throws the TypeError a class called
// without new throws, as the Go table does, marking or not.
func TestRegNativeInitClass(t *testing.T) {
	const src = `
class K { constructor(a, b) { if (!b) b = 0; this.a = a; this.b = b } }
var object = { tag: 7 }, out = [];
function run(n) { var s = 0, k; for (var i = 0; i < n; i++) { k = new K(i, object); s += k.a + (k.b === object) } return s }
run(100);
out.push(run(1000));
var k = new K(5); out.push(Object.keys(k).join(), k.a, Object.is(k.b, 0), k instanceof K);
var o = {};
function via(a, b) { return K.apply(this, arguments) }
function call() { try { return K(1, 2) } catch (e) { return e.name } }
function callOn() { try { return K.call(o, 1, 2) } catch (e) { return e.name } }
function applyOn() { try { return K.apply(o, [1, 2]) } catch (e) { return e.name } }
function viaOn() { try { return via.call(o, 1, 2) } catch (e) { return e.name } }
for (var i = 0; i < 100; i++) out.push(call(), callOn(), applyOn(), viaOn());
out.push(Object.keys(o).join());
function init(a, b) { this.a = a; this.b = b }
function Fwd(a, b) { this.initialize.apply(this, arguments) }
function FwdCall(a, b) { this.initialize.call(this, a, b) }
Fwd.prototype.initialize = FwdCall.prototype.initialize = init;
function make(C) { try { return new C(1, 2).a } catch (e) { return e.name } }
for (var i = 0; i < 100; i++) make(Fwd), make(FwdCall);
Fwd.prototype.initialize = FwdCall.prototype.initialize = K;
for (var i = 0; i < 100; i++) out.push(make(Fwd), make(FwdCall));
class S extends K { constructor(a) { super(a, 2) } }
out.push(new S(3).a, new S(3).b, new S(3) instanceof S);
var R = Reflect.construct(K, [4, 5], S); out.push(R.a, R.b, Object.getPrototypeOf(R) === S.prototype);
Object.defineProperty(K.prototype, "b", { set(v) { out.push("set:" + v) }, configurable: true });
k = new K(6, 7); out.push(Object.keys(k).join());
out.join("|")`
	want := "500500|a,b|5|true|true|" + strings.Repeat("TypeError|", 400) + "|" + strings.Repeat("TypeError|", 200) +
		"3|2|true|4|5|true|set:7|a"
	nativeCounting(t)
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	const constructions = `
class K { constructor(a, b) { if (!b) b = 0; this.a = a; this.b = b } }
function run(n) { var s = 0, k; for (var i = 0; i < n; i++) { k = new K(i); s += k.a + k.b } return s }
run(100); String(run(1000))`
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		// The constructions but the first few have no frames.
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := nativeRunSrc(t, constructions, true); got != "499500" {
			t.Fatalf("marking %v: constructions gave %s", marking, got)
		}
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 20 {
			t.Fatalf("marking %v: constructions entered %d frames", marking, n)
		}
	}
}

// nativeInitCacheSrc constructs through the initializer cache (initCache)
// and makes each change it must see between constructions, every answer
// exact against the Go table: a hit with a parameter made 0 and one
// whose argument the body decides; the constructor's prototype replaced
// by one with a setter; a setter and a read-only property added to the
// prototype; the prototype frozen over a property the stores name, and
// made non-extensible; Object.prototype given a read-only property; the
// prototype made a dictionary and given a setter; a chain past two
// prototypes (its cell) given a setter at its top; apply_args on a
// receiver of the cached layout made non-extensible; a constructor that
// returns an object; another new.target; classes a factory makes on
// fresh prototypes; and a forwarder's method over more classes than the
// cache refills for.
const nativeInitCacheSrc = `
var out = [], log = [];
function run(C, n, a, b) { var o; for (var i = 0; i < n; i++) o = new C(a + i, b); return o }
function show(o) { return Object.keys(o).join() + ":" + Object.keys(o).map(function (k) { return String(o[k]) }).join() }

function P(a, b) { this.a = a; this.b = b }
out.push(show(run(P, 50, 1, 2)));
P.prototype = { set b(v) { log.push("replaced:" + v) } };
out.push(show(run(P, 3, 10, 20)));
Object.defineProperty(P.prototype, "a", { set(v) { log.push("added:" + v) }, configurable: true });
out.push(show(run(P, 3, 30, 40)));

function Q(a, b) { this.a = a; this.b = b }
run(Q, 50, 1, 2);
Object.defineProperty(Q.prototype, "b", { value: "ro", writable: false });
out.push(show(run(Q, 3, 5, 6)));

function F(a, b) { this.a = a; this.b = b }
F.prototype.a = "proto";
run(F, 50, 1, 2);
Object.freeze(F.prototype);
out.push(show(run(F, 3, 7, 8)));

function N(a, b) { this.a = a; this.b = b }
run(N, 50, 1, 2);
Object.preventExtensions(N.prototype);
out.push(show(run(N, 3, 9, 10)));

function O(a, zz) { this.a = a; this.zz = zz }
run(O, 50, 1, 2);
Object.defineProperty(Object.prototype, "zz", { value: "oro", writable: false, configurable: true });
out.push(show(run(O, 3, 11, 12)));
delete Object.prototype.zz;

function D(a, b) { this.a = a; this.b = b }
for (var k = 0; k < 40; k++) D.prototype["k" + k] = k;
run(D, 50, 1, 2);
for (var k = 0; k < 40; k++) delete D.prototype["k" + k];
run(D, 50, 1, 2);
Object.defineProperty(D.prototype, "b", { set(v) { log.push("dict:" + v) }, configurable: true });
out.push(show(run(D, 3, 13, 14)));

var top = {};
function Deep(a, b) { this.a = a; this.b = b }
Deep.prototype = Object.create(Object.create(Object.create(top)));
run(Deep, 50, 1, 2);
Object.defineProperty(top, "a", { set(v) { log.push("deep:" + v) }, configurable: true });
out.push(show(run(Deep, 3, 15, 16)));

function Z(x, y) { if (!x) x = 0; this.x = x; this.y = y }
run(Z, 50, 1, 2);
out.push(show(run(Z, 1, -1, 3)), show(new Z("", 4)), show(new Z(-0, 5)), Object.is(new Z(-0, 5).x, 0), show(new Z(NaN, 6)));

function init(a, b) { this.a = a; this.b = b }
function via(a, b) { init.apply(this, arguments) }
for (var i = 0; i < 50; i++) via.call({}, i, i);
var sealed = Object.preventExtensions({});
via.call(sealed, 1, 2); out.push(show(sealed), Object.isExtensible(sealed));
var mixed = { b: 0 };
for (var i = 0; i < 50; i++) via.call({ b: 0 }, i, i);
via.call(mixed, 3, 4); out.push(show(mixed));

function R(a) { this.a = a; return { r: a } }
out.push(show(run(R, 50, 1, 0)));

function T(a, b) { this.a = a; this.b = b }
function U() {}
run(T, 50, 1, 2);
var t = Reflect.construct(T, [17, 18], U);
out.push(show(t), Object.getPrototypeOf(t) === U.prototype, show(run(T, 1, 19, 20)));

function factory() { return function (a, b) { this.a = a; this.b = b } }
var made = [];
for (var k = 0; k < 20; k++) made.push(factory());
for (var i = 0; i < 3; i++) for (var k = 0; k < made.length; k++) new made[k](i, k);
Object.defineProperty(made[7].prototype, "b", { set(v) { log.push("fresh:" + v) }, configurable: true });
out.push(show(new made[7](21, 22)), show(new made[8](23, 24)));

function create() { return function (a, b) { this.initialize.apply(this, arguments) } }
function initialize(a, b) { this.a = a; this.b = b }
var cs = [];
for (var k = 0; k < 24; k++) { var C = create(); C.prototype.initialize = initialize; cs.push(C) }
var sum = 0;
for (var i = 0; i < 40; i++) for (var k = 0; k < cs.length; k++) { var c = new cs[k](i, k); sum += c.a + c.b }
Object.defineProperty(cs[23].prototype, "a", { set(v) { log.push("many:" + v) }, configurable: true });
out.push(sum, show(new cs[23](25, 26)), show(new cs[0](27, 28)));
out.push(log.join());
out.join("|")`

// The initializer cache answers as the Go table does through every change
// nativeInitCacheSrc makes, marking or not, with the same exits.
func TestRegNativeInitCache(t *testing.T) {
	const want = "a,b:50,2|a:12|:|a:7|b:8|a,b:11,10|a:13|a:15|b:16|x,y:-1,3|x,y:0,4|x,y:0,5|true|x,y:0,6|" +
		":|false|b,a:4,3|r:50|a,b:17,18|true|a,b:19,20|a:21|a,b:23,24|29760|b:26|a,b:27,28|" +
		"replaced:20,replaced:20,replaced:20,added:30,replaced:40,added:31,replaced:40,added:32,replaced:40," +
		"dict:14,dict:14,dict:14,deep:15,deep:16,deep:17,fresh:22,many:25"
	if got := nativeRunSrc(t, nativeInitCacheSrc, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	nativeCheck(t, nativeInitCacheSrc, "")
}

// initCacheOf is the initializer cache of the global function name.
func initCacheOf(r *Runtime, name string) *initCache {
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.ctorInit
}

// The cache's state: none before the constructor has register code; filled
// once, from the
// constructor's root to its objects' layout, with each store's slot, by
// constructions that go on hitting; emptied and refilled when the
// prototype's layout changes; and, over more layouts than maxCacheFills,
// no longer refilled. Each with the constructions frameless but the
// first few.
func TestRegNativeInitCacheState(t *testing.T) {
	nativeCounting(t)
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		was := regNativeOn
		regNativeOn = true
		r := newTestRuntime(t, Config{})
		run := func(src string) string {
			t.Helper()
			v, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if !v.IsString() {
				return ""
			}
			return v.String().Go()
		}
		run(`function P(a, b, c) { this.a = a; this.b = b; this.a = c }
function make(C, n) { var s = 0; for (var i = 0; i < n; i++) { var o = new C(i, 1, 2); s += o.a + o.b } return s }`)
		if c := initCacheOf(r, "P"); c != nil {
			t.Fatalf("marking %v: a cache before P has register code: %+v", marking, c)
		}
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := run(`String(make(P, 1000))`); got != "3000" {
			t.Fatalf("marking %v: got %s", marking, got)
		}
		c := initCacheOf(r, "P")
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 20 {
			t.Fatalf("marking %v: %d frames", marking, n)
		}
		o := run(`var o = new P(1, 2, 3); Object.keys(o).join()`)
		obj := r.global.getOwn(r.atoms.intern("o")).value().Object()
		if o != "a,b" || c.fills != 1 || c.to != obj.shape || c.from == nil || c.from.n != 0 || c.slots[0] != 0 || c.slots[1] != 1 || c.slots[2] != 0 || c.plain != 3 {
			t.Fatalf("marking %v: after constructions: keys %s, %+v", marking, o, c)
		}
		if got := run(`
function One(a) { this.a = a }
function Four(a, b, c, d) { this.a = a; this.b = b; this.c = c; this.d = d }
function Many(a, b) { this.a = a; this.b = b; this.c = a; this.d = b; this.e = a }
function Need(a, b, c, d) { this.a = d; this.b = a }
function Defaulted(a, b) { this.a = a ? a : 0; this.b = b }
var pointer = {}, f, m, n, one;
for (var i = 0; i < 100; i++) {
 one = new One(pointer); f = new Four(pointer, i, pointer, i); m = new Many(pointer, i);
 n = new Need(pointer, i, 0, pointer); new Defaulted(pointer, i);
}
n = new Need(pointer);
[one.a === pointer, f.a === pointer, f.d === 99, m.e === pointer, n.a === undefined,
 n.b === pointer, Object.keys(f).join()].join("|")`); got != "true|true|true|true|true|true|a,b,c,d" {
			t.Fatalf("marking %v: short plans: %s", marking, got)
		}
		for _, tc := range []struct {
			name  string
			plain uint8
		}{{"One", 1}, {"Four", 4}, {"Many", 0}, {"Need", 2}, {"Defaulted", 0}} {
			if ic := initCacheOf(r, tc.name); ic == nil || ic.plain != tc.plain {
				t.Fatalf("marking %v: %s's short plan: %+v", marking, tc.name, ic)
			}
		}
		if ic := initCacheOf(r, "Need"); ic.need != 4 || ic.args[0] != 3 || ic.args[1] != 0 {
			t.Fatalf("marking %v: remapped arguments: %+v", marking, ic)
		}
		run(`
f = new Four({ tag: 17 }, "retained" + 23, 1n << 80n, function () { return 41 });
one = new One(-0);`)
		runtime.GC()
		if got := run(`[f.a.tag, f.b, String(f.c), f.d(), Object.is(one.a, -0)].join("|")`); got != "17|retained23|1208925819614629174706176|41|true" {
			t.Fatalf("marking %v: retained short-plan values: %s", marking, got)
		}
		run(`P.prototype.x = 1; make(P, 100)`)
		if c.fills != 2 || c.from == nil {
			t.Fatalf("marking %v: after the prototype's change: %+v", marking, c)
		}
		run(`for (var k = 0; k < 40; k++) { P.prototype["y" + k] = k; make(P, 2) }`)
		if c.fills != maxCacheFills {
			t.Fatalf("marking %v: %d fills, want %d", marking, c.fills, maxCacheFills)
		}
		if got := run(`String(make(P, 100))`); got != "300" {
			t.Fatalf("marking %v: past the fills: got %s", marking, got)
		}
		r.Close()
		regNativeOn = was
	}
}

// Runtimes on goroutines of their own run one compiled function, whose
// bytecode they share: each template's closureCode, and so its
// initializer cache, is its runtime's, filled from its own layouts and
// prototypes. Run under the race detector in the gates.
func TestRegNativeInitCacheRuntimes(t *testing.T) {
	fn := compileForTest(t, `
function P(a, b) { this.a = a; this.b = b }
var s = 0;
for (var i = 0; i < 2000; i++) { var o = new P(i, 1); s += o.a + o.b }
P.prototype.x = 1;
for (var i = 0; i < 100; i++) { var o = new P(i, 2); s += o.a + o.b + o.x }
String(s)`)
	was := regNativeOn
	regNativeOn = true
	defer func() { regNativeOn = was }()
	var wg sync.WaitGroup
	errs := make([]string, 4)
	for g := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newTestRuntime(t, Config{})
			defer r.Close()
			v, err := r.Run(fn)
			switch {
			case err != nil:
				errs[g] = err.Error()
			case v.String().Go() != "2006250":
				errs[g] = v.String().Go()
			default:
				c := initCacheOf(r, "P")
				if c == nil || c.fills != 2 || c.to == nil || c.to.tree != r.shapes {
					errs[g] = fmt.Sprintf("cache %+v", c)
				}
			}
		}()
	}
	wg.Wait()
	for g, e := range errs {
		if e != "" {
			t.Errorf("runtime %d: %s", g, e)
		}
	}
}

func TestRegNativeInitDefaults(t *testing.T) {
	const src = nativeInitSource + `
makeDefaults(100); makeForwardDefaults(100);
var out = [], log = [], pointer = { tag: 7 };
function show(v) { return typeof v + ":" + (Object.is(v, -0) ? "-0" : String(v)) }
for (var v of [undefined, null, false, 0, -0, NaN, "", 0n, true, 2, -3, "s", 1n, pointer]) {
 var a = new Defaults(v, v, v), b = new ForwardDefaults(v, v, v);
 out.push(show(a.x), show(b.x), a.x === b.x, a.y === b.y, a.z === b.z);
}
function mixed(x) { this.a = x; if (!x) x = 0; this.b = x; this.c = x ? x : 0; this.d = x }
function conditional(x) { this.a = x ? x : 0; this.b = x }
var m, c;
for (var i = 0; i < 100; i++) { m = new mixed(-0); c = new conditional(-0) }
out.push(Object.is(m.a, -0), Object.is(m.b, 0), Object.is(m.c, 0), Object.is(m.d, 0),
 Object.is(c.a, 0), Object.is(c.b, -0));
Object.defineProperty(Defaults.prototype, "x", { set(v) { log.push("set:" + v); if (v === 9) throw new RangeError("x") }, configurable: true });
var d = new Defaults(-0, 2, 3); out.push(d.y, d.z);
try { new Defaults(9, 5, 6) } catch (e) { out.push(e.name, e.message) }
delete Defaults.prototype.x;
function strict(a, b) { "use strict"; this.a = a; this.b = b }
function forward(a, b) { return strict.apply(this, arguments) }
var o = {}; for (var i = 0; i < 100; i++) forward.call(o, i, i);
Object.defineProperty(o, "b", { value: 4, writable: false });
try { forward.call(o, 7, 8) } catch (e) { out.push(e.name, o.a, o.b) }
function getArgs(a, b) { this.a = a; this.b = arguments.length }
for (var i = 0; i < 100; i++) d = new getArgs(2, 3, 4); out.push(d.a, d.b);
function computed(a) { this.a = a; this.b = a + 1 }
for (var i = 0; i < 100; i++) d = new computed(2); out.push(d.a, d.b);
out.push(log.join()); out.join("|")`
	nativeCounting(t)
	want := strings.Repeat("number:0|number:0|true|true|true|", 8) +
		"boolean:true|boolean:true|true|true|true|number:2|number:2|true|true|true|number:-3|number:-3|true|true|true|string:s|string:s|true|true|true|bigint:1|bigint:1|true|true|true|object:[object Object]|object:[object Object]|true|true|true|true|true|true|true|true|true|2|3|RangeError|x|TypeError|7|4|2|3|2|3|set:0,set:9"
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
	}
}

// A construction the native initializer answers makes no frame, but the
// frames its bodies would have made still meet the limit: where the
// constructor's frame, or the forwarder's and its method's, would be past
// it, the bodies run and throw the RangeError the frames would. probe
// records how far below the deepest frame a construction first throws:
// at it for a constructor that stores, one below for a forwarder or an
// apply_args of an initializer; and
// never for one that only stores its parameters (LeafSetThis), which
// leafCall answers without a frame, as the Go table does, past the limit.
func TestRegNativeInitFrameLimit(t *testing.T) {
	const src = nativeInitSource + `
function probe(C) {
 var first = -1, deepest = 0;
 function down(d) {
  try { new C(1, 2) } catch (e) { if (first < 0) first = d; if (!(e instanceof RangeError)) throw e }
  deepest = d;
  try { down(d + 1) } catch (e) {}
 }
 for (var i = 0; i < 100; i++) new C(1, 2);
 down(0);
 return first < 0 ? "none" : deepest - first;
}
function Via(a, b) { Defaults.apply(this, arguments) }
[probe(Pair), probe(Defaults), probe(Forward), probe(ForwardCall), probe(ForwardCallDefaults), probe(Via)].join()`
	// Via's apply_args: the Go table counts apply's frame
	// (regApplyArgs), which the native handler, entering the target
	// itself (nativeApply), makes none of, with the plan or without.
	const table, native = "none,0,1,1,1,2", "none,0,1,1,1,1"
	if got := nativeRunSrc(t, src, false); got != table {
		t.Fatalf("Go table: got %q, want %q", got, table)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != native {
			t.Fatalf("marking %v: got %q, want %q", marking, got, native)
		}
	}
}

// A forwarder made anew for each class, as Class.create makes one, is one
// site of more shapes than a site keeps: the method is read in the
// forwarder's table (fwdSite), which nativeNew fills, and every
// construction is still made without a frame.
func TestRegNativeInitForwardTable(t *testing.T) {
	nativeCounting(t)
	const src = `
function create() { return function () { this.initialize.apply(this, arguments) } }
function init(a, b) { this.a = a; this.b = b }
var cs = [];
for (var k = 0; k < 24; k++) { var C = create(); C.prototype.initialize = init; cs.push(C) }
function run(n) {
 var s = 0;
 for (var i = 0; i < n; i++) for (var j = 0; j < cs.length; j++) { var p = new cs[j](i, 1); s += p.a + p.b }
 return s;
}
run(50);
String(run(100))`
	const want = "121200"
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeCounter.Load().calls)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		// The warm-up's constructions fill the sites and the table.
		if n := atomic.LoadInt64(&nativeCounter.Load().calls) - before; n > 2*24*20 {
			t.Fatalf("marking %v: forwarded constructions entered %d frames", marking, n)
		}
	}
}

// The register compiler's plans (RegCode.Stores and Forward), by body:
// the forms it takes, and bodies that are none of them.
func TestRegInitPlans(t *testing.T) {
	const src = `
function stores(a, b) { this.a = a; this.b = b }
function defaults(x, y) { if (!x) x = 0; this.x = x; this.y = y ? y : 0; this.z = x }
function method(a) { this.m = a }
function apply() { this.k.apply(this, arguments) }
function call(a, b) { this.k.call(this, b, 3, a, null) }
function computed(a) { this.a = a + 1 }
function args(a) { this.a = arguments.length }
function nine(a) { this.a = a; this.b = a; this.c = a; this.d = a; this.e = a; this.f = a; this.g = a; this.h = a; this.i = a }
function five(a) { if (!a) a = 5; this.a = a }
function other(a) { other.k.call(this, a) }
function returns(a) { this.a = a; return a }
var K = class { constructor(a) { this.a = a } };
var KReturns = class { constructor(a) { this.a = a; return a } };
var KDerived = class extends K { constructor(a) { super(a) } };
var KForward = class { constructor(a) { this.k.call(this, a) } };
var o = { k() {} };
for (var f of [stores, defaults, method, apply, call, computed, args, nine, five, other, returns]) {
	try { f.call(o, 1, 2) } catch (e) {}
}
new K(1); new KReturns(1); new KDerived(1);
try { new KForward(1) } catch (e) {}`
	r := newTestRuntime(t, Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	plan := func(name string) string {
		v := r.global.getOwn(r.atoms.intern(name)).value()
		rc := v.Object().fn().closure.reg.rc
		s := fmt.Sprint(len(rc.Stores) > 0)
		for _, st := range rc.Stores {
			s += fmt.Sprintf(" %d:%v", st.Param, st.Zero)
		}
		if f := rc.Forward; f != nil {
			s += fmt.Sprintf(" forward call=%v", f.Call)
			for _, a := range f.Args {
				if a.Param < 0 {
					s += " k:" + a.Load.Op().String()
				} else {
					s += fmt.Sprintf(" p%d", a.Param)
				}
			}
		}
		return s
	}
	for _, tc := range []struct{ name, want string }{
		{"stores", "true 0:false 1:false"},
		{"defaults", "true 0:true 1:true 0:true"},
		{"method", "true 0:false"},
		{"apply", "false forward call=false"},
		{"call", "false forward call=true p1 k:load_int p0 k:load_null"},
		{"computed", "false"},
		{"args", "false"},
		{"nine", "false"},
		{"five", "false"},
		{"other", "false"},
		{"returns", "false"},
		{"K", "true 0:false"},
		{"KReturns", "false"},
		{"KDerived", "false"},
		{"KForward", "false"},
	} {
		if got := plan(tc.name); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
