//go:build amd64 || arm64

package vm

import (
	"strings"
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
			r := New(Config{})
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
			before := atomic.LoadInt64(&nativeCalls)
			if got := nativeRunSrc(t, nativeInitSource+`String(`+name+`(1000))`, true); got != "500500" {
				t.Fatalf("marking %v, %s: got %s", marking, name, got)
			}
			limit := int64(20)
			if n := atomic.LoadInt64(&nativeCalls) - before; n > limit {
				t.Fatalf("marking %v, %s: entered %d frames, want at most %d", marking, name, n, limit)
			}
		}
	}
}

func TestRegNativeInitFallback(t *testing.T) {
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
	if got := nativeRunSrc(t, src, false); got != want {
		t.Fatalf("Go table: got %q, want %q", got, want)
	}
	var exits [2]int64
	for i, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		before := atomic.LoadInt64(&nativeEnters)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %q, want %q", marking, got, want)
		}
		exits[i] = atomic.LoadInt64(&nativeEnters) - before
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
