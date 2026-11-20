//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// objectCreateSrc calls Object.create every way it can be called -- a
// prototype alone, an object or null, which its leaf makes without a frame
// (createleaf.go); with properties, or of no prototype, which the built-in
// makes -- of prototypes of every kind: an array, a function, a frozen
// object, a proxy, whose traps making an object never runs; one changed
// after, an object given another; and in loops of a prototype made anew at
// each turn or at each call. objectCreateWant is Node's answer
// (vm.runInThisContext).
const objectCreateSrc = `
var out = [];
function show(o) { var p = Object.getPrototypeOf(o); return [p === null ? "null" : typeof p, Object.keys(o).join(), Object.isExtensible(o)].join("/") }
var P = { x: 1 };
var a = Object.create(P); a.y = 2;
out.push(show(a), a.x, a.y, P.isPrototypeOf(a));
var n = Object.create(null); n.k = 1;
out.push(show(n), "toString" in n, n.k);
var d = Object.create(P, { z: { value: 3, enumerable: true }, w: { get() { return 4 } } });
out.push(show(d), d.z, d.w, Object.getOwnPropertyNames(d).join());
var d2 = Object.create(P, undefined);
out.push(show(d2), Object.create(P, {}).x);
for (var bad of [undefined, 1, "s", true, Symbol.iterator, 1n]) { try { Object.create(bad); out.push("no error") } catch (e) { out.push(e.constructor.name) } }
try { Object.create(P, null); out.push("no error") } catch (e) { out.push(e.constructor.name) }
try { Object.create(); out.push("no error") } catch (e) { out.push(e.constructor.name) }
var arr = [1, 2, 3], f = function () { return 9 }, frozen = Object.freeze({ q: 5 });
var traps = [];
var px = new Proxy({ r: 6 }, { get(t, k, rcv) { traps.push(String(k)); return Reflect.get(t, k, rcv) }, getPrototypeOf(t) { traps.push("gpo"); return null } });
var ca = Object.create(arr), cf = Object.create(f), cz = Object.create(frozen), cp = Object.create(px);
out.push(ca.length, ca[1], Array.isArray(ca), cf.call === Function.prototype.call, typeof cf, cz.q, Object.isFrozen(cz));
cz.q = 7;
out.push(cz.q, Object.keys(cz).join());
out.push(traps.join(), cp.r, traps.join(), Object.getPrototypeOf(cp) === px);
var Q = { x: 10 }, b = Object.create(Q);
Object.setPrototypeOf(Q, { up: 1 });
out.push(b.up, b.x);
Object.setPrototypeOf(b, P);
out.push(b.x, b.up);
Q.x = 11;
out.push(Object.create(Q).x, Object.create.call(null, Q).x, Reflect.apply(Object.create, undefined, [Q]).x);
function loop(n) { var s = 0; for (var i = 0; i < n; i++) { var p = { x: i }; var o = Object.create(i % 3 ? p : null); if (o.x !== undefined) s += o.x; o.z = i; s += o.z } return s }
function shared(n) { var proto = { x: 1 }, s = 0, o; for (var j = 0; j < n; j++) { o = Object.create(proto); o.y = j; s += o.x + o.y } return s }
for (var k = 0; k < 3; k++) out.push(loop(300), shared(300));
out.join("|")
`

const objectCreateWant = "object/y/true|1|2|true|null/k/true|false|1|object/z/true|3|4|z,w|object//true|1|TypeError|TypeError|TypeError|TypeError|TypeError|TypeError|TypeError|TypeError|3|2|false|true|object|5|false|5|||6|r|true|1|10|1||11|11|11|74850|45150|74850|45150|74850|45150"

// TestRegNativeObjectCreate runs objectCreateSrc by the Go table and by the
// native handlers, marking and not: Node's answer, the same exits, and a
// loop of Object.create's calls of a prototype alone made with none left
// to Go past the caches' filling.
func TestRegNativeObjectCreate(t *testing.T) {
	if got := nativeRunSrc(t, objectCreateSrc, false); got != objectCreateWant {
		t.Fatalf("Go table:\n%s\nwant\n%s", got, objectCreateWant)
	}
	nativeCheck(t, objectCreateSrc, `
		function shared(n) { var proto = { x: 1 }, s = 0, o; for (var j = 0; j < n; j++) { o = Object.create(proto); s += o.x } return s }
		String(shared(1000))`, bytecode.RCallMW)
}
