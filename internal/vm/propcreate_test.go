package vm

import (
	"strings"
	"testing"
)

// propCreateScript adds properties to fresh objects every way the
// interpreter, the tree tier and setIndexed's ordinary-object path
// (setOrdinaryIndex) do: names and indices, on objects built alike past
// their room and their shapes' lookup index, in order and not, deleted
// and added again; with setters on the prototype chain, which must run,
// read-only properties up it, frozen, sealed and non-extensible objects,
// proxies as the object and up its chain, a site's objects used as a
// table of names and of indices, and keys that look like indices and are
// not.
const propCreateScript = `
		var out = [];
		function build(n, m) {
		  var o = {};
		  for (var i = 0; i < n; i++) o["p" + (i % 26 + 10).toString(36) + (i >= 26 ? i : "")] = i;
		  for (var i = 0; i < m; i++) o[i] = -i;
		  return o;
		}
		// Shapes shared: many objects built alike, then read back.
		var objs = [];
		for (var k = 0; k < 50; k++) objs.push(build(12 + k % 3, 10 + k % 4));
		var s = 0;
		for (var k = 0; k < objs.length; k++) { var ks = Object.keys(objs[k]); s += ks.length; }
		out.push(s, Object.keys(objs[7]).join(","), JSON.stringify(objs[5]));
		// Integer keys in insertion order other than ascending: still listed ascending first.
		var o = {}; o.z = 1; o[5] = 5; o.a = 2; o[1] = 1; o[10] = 10; o["01"] = "s"; o[-1] = -1; o[4294967295] = "big"; o[4294967294] = "max"; o[1.5] = "f"; o[-0] = "z";
		out.push(Object.keys(o).join(","), o[0], o["-0"]);
		// Many integer keys on a plain object, out of order, deleted and re-added.
		var t = {};
		for (var i = 0; i < 40; i++) t[(i * 7) % 40] = i;
		delete t[3]; delete t[20]; t[3] = "back"; t.x = "x";
		out.push(Object.keys(t).join(","), t[3], t[20], 20 in t);
		// Accessors on the prototype chain run, for indices and names.
		var log = [];
		var proto = {};
		Object.defineProperty(proto, "2", { set: function (v) { log.push("set2:" + v); }, get: function () { return "g2"; } });
		Object.defineProperty(proto, "q", { set: function (v) { log.push("setq:" + v); } });
		var c = Object.create(proto);
		for (var i = 0; i < 5; i++) c[i] = i * 10;
		c.q = 7; c.r = 8;
		out.push(Object.keys(c).join(","), c[2], log.join(";"));
		// Read-only on the prototype: refused silently in sloppy mode, TypeError in strict.
		var ro = Object.create(Object.freeze({ 0: "ro", k: "rk" }));
		ro[0] = "w"; ro.k = "w"; ro[1] = "ok";
		out.push(Object.keys(ro).join(","), ro[0], ro.k);
		out.push((function () { "use strict"; try { ro[0] = 1; return "no"; } catch (x) { return x.constructor.name; } })());
		// Frozen, sealed, non-extensible objects.
		var f = Object.freeze({ a: 1, 0: 0 });
		f[0] = 9; f.a = 9; f[1] = 9; f.b = 9;
		var g = Object.seal({ a: 1, 0: 0 });
		g[0] = 9; g.a = 9; g[1] = 9; g.b = 9;
		var h = Object.preventExtensions({ a: 1, 0: 0 });
		h[0] = 8; h[1] = 8; h.c = 8;
		out.push(JSON.stringify(f), JSON.stringify(g), JSON.stringify(h));
		out.push((function () { "use strict"; var r = []; for (var x of [f, g, h]) { for (var k of [0, 1, "a", "z"]) { try { x[k] = 5; r.push("ok"); } catch (err) { r.push(err.constructor.name); } } } return r.join(","); })());
		// Proxies: as the object, and as a prototype.
		var traps = [];
		var p = new Proxy({}, { set: function (tg, k, v, rc) { traps.push("set:" + String(k)); tg[k] = v; return true; } });
		for (var i = 0; i < 3; i++) p[i] = i; p.n = 1;
		var viaProto = Object.create(new Proxy({}, { set: function (tg, k, v, rc) { traps.push("pset:" + String(k)); return Reflect.set(tg, k, v, rc); } }));
		for (var i = 0; i < 3; i++) viaProto[i] = i; viaProto.m = 2;
		out.push(traps.join(","), Object.keys(p).join(","), Object.keys(viaProto).join(","));
		// Objects built alike past their room, then one built differently from the same site.
		function mk(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return o; }
		var ms = []; for (var i = 0; i < 30; i++) ms.push(mk(i < 25 ? 20 : 3 + i));
		out.push(ms.map(function (o) { return Object.keys(o).length; }).join(","), Object.keys(ms[29]).slice(-3).join(","));
		// An object used as a table of names (keyed stores) and of indices.
		var tbl = {}; for (var i = 0; i < 100; i++) tbl["n" + i] = i; for (var i = 0; i < 100; i++) tbl[i] = i;
		var tk = Object.keys(tbl);
		out.push(tk.length, tk.slice(0, 3).join(","), tk.slice(98, 103).join(","), tbl.n50 + tbl[50]);
		// Index-like strings that are not indices.
		var nl = {}; nl["007"] = 1; nl["7"] = 2; nl["1e3"] = 3; nl[1e3] = 4; nl["-5"] = 5; nl[" 3"] = 6; nl[3] = 7;
		out.push(Object.keys(nl).join("|"));
		// Reflect.set, which assigns through setProp: setters up the chain for an index and a name run, refusals refuse.
		var rlog = [];
		var rp = {};
		Object.defineProperty(rp, "1", { set: function (v) { rlog.push("r1:" + v); } });
		Object.defineProperty(rp, "nm", { set: function (v) { rlog.push("rnm:" + v); } });
		var rc = Object.create(rp);
		out.push([Reflect.set(rc, 0, "a"), Reflect.set(rc, 1, "b"), Reflect.set(rc, "nm", "c"), Reflect.set(rc, "x", "d"),
		  Reflect.set(f, 0, 1), Reflect.set(f, 5, 1), Reflect.set(g, 5, 1), Reflect.set(g, 0, 7), Reflect.set(h, "a", 3)].join());
		var big = build(12, 0);
		out.push([Reflect.set(big, 3, "three"), Reflect.set(big, "pa", "A"), Reflect.set(big, "pz", "Z")].join(), Object.keys(rc).join(","), rlog.join(";"),
		  JSON.stringify(g), JSON.stringify(h), Object.keys(big).join(","), big.pa, big[3]);
		// Frozen, sealed and non-extensible objects with no elements, given indices.
		var fz = Object.freeze({ a: 1 }), ne = Object.preventExtensions({ b: 2 }), sl = Object.seal({ c: 3 });
		fz[0] = 1; ne[0] = 1; sl[0] = 1; fz.a = 2; sl.c = 4;
		out.push(JSON.stringify([fz, ne, sl]), (function () { "use strict"; var r = []; for (var x of [fz, ne, sl]) { try { x[0] = 2; r.push("ok"); } catch (err) { r.push(err.constructor.name); } } return r.join(); })());
		// An object literal's indices, then the same indices and the next assigned.
		var el = { 0: "e0", 1: "e1", x: 1 };
		el[0] = "n0"; el[2] = "n2"; el[1] = "n1"; el.y = 2;
		out.push(Object.keys(el).join(","), JSON.stringify(el), el[0] + el[1] + el[2]);
		// A setter on Object.prototype for an index, added after objects were built alike.
		var made = [];
		for (var k = 0; k < 20; k++) { var d = {}; for (var i = 0; i < 12; i++) d[i] = i; made.push(d); }
		Object.defineProperty(Object.prototype, "12", { set: function (v) { log.push("op12:" + v); }, configurable: true });
		var e = {}; for (var i = 0; i < 14; i++) e[i] = i;
		out.push(Object.keys(e).join(","), log.join(";"));
		delete Object.prototype[12];
		out.join("\n");`

// propCreateWant is Node v26.10.0's answer to propCreateScript.
const propCreateWant = `1222
0,1,2,3,4,5,6,7,8,9,10,11,12,pa,pb,pc,pd,pe,pf,pg,ph,pi,pj,pk,pl,pm
{"0":0,"1":-1,"2":-2,"3":-3,"4":-4,"5":-5,"6":-6,"7":-7,"8":-8,"9":-9,"10":-10,"pa":0,"pb":1,"pc":2,"pd":3,"pe":4,"pf":5,"pg":6,"ph":7,"pi":8,"pj":9,"pk":10,"pl":11,"pm":12,"pn":13}
0,1,5,10,4294967294,z,a,01,-1,4294967295,1.5
z

0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,x
back

false
0,1,3,4,r
g2
set2:20;setq:7
1
ro
rk
TypeError
{"0":0,"a":1}
{"0":9,"a":9}
{"0":8,"a":1}
TypeError,TypeError,TypeError,TypeError,ok,TypeError,ok,TypeError,ok,TypeError,ok,TypeError
set:0,set:1,set:2,set:n,pset:0,pset:1,pset:2,pset:m
0,1,2,n
0,1,2,m
20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,20,28,29,30,31,32
k29,k30,k31
200
0,1,2
98,99,n0,n1,n2
100
3|7|1000|007|1e3|-5| 3
true,true,true,true,false,false,false,true,true
true,true,true
0,x
r1:b;rnm:c
{"0":7,"a":5}
{"0":5,"a":3}
3,pa,pb,pc,pd,pe,pf,pg,ph,pi,pj,pk,pl,pz
A
three
[{"a":1},{"b":2},{"c":4}]
TypeError,TypeError,TypeError
0,1,2,x,y
{"0":"n0","1":"n1","2":"n2","x":1,"y":2}
n0n1n2
0,1,2,3,4,5,6,7,8,9,10,11,13
set2:20;setq:7;op12:12`

// Properties added to fresh objects land where Node puts them, in the
// order it lists them, and assignments the chain or the object refuses or
// intercepts are refused or intercepted, run as a script whose functions
// warm into the tree tier, and run again.
func TestPropertyCreationAgreesWithNode(t *testing.T) {
	for round := range 2 {
		r := New(Config{})
		v, err := r.Run(compileForTest(t, propCreateScript))
		if err != nil {
			t.Fatal(err)
		}
		got := v.String().Go()
		if got != propCreateWant {
			g, w := strings.Split(got, "\n"), strings.Split(propCreateWant, "\n")
			for i := range max(len(g), len(w)) {
				var gl, wl string
				if i < len(g) {
					gl = g[i]
				}
				if i < len(w) {
					wl = w[i]
				}
				if gl != wl {
					t.Errorf("round %d, line %d: got %q, want %q", round, i+1, gl, wl)
				}
			}
		}
		r.Close()
	}
}

// A literal's objects outgrow the room they were made with once, not at
// each doubling: its site's root notes the room they came to need
// (outgrowProps), and the next objects, the tree tier's, are made with it,
// from the third on.
// A keyed store's objects -- a table of names, a dictionary past the soft
// limit -- teach their site nothing, nor do objects with no site teach
// their class's root.
func TestObjectRoomLearnedFromGrowth(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	_, err := r.Run(compileForTest(t, `
		function record() {
			var o = {};
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			for (var i = 0; i < 10; i++) o[i] = i;
			return o;
		}
		function table(n) {
			var o = {};
			for (var i = 0; i < n; i++) o["k" + i] = i;
			return o;
		}
		function plain() {
			var o = Object.create(Object.prototype);
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			o.k = 11; o.l = 12; o.m = 13; o.n = 14; o.o = 15; o.p = 16; o.q = 17; o.r = 18; o.s = 19; o.t = 20;
			return o;
		}
		function first() {
			var o = {};
			o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.j = 10;
			for (var i = 0; i < 10; i++) o[i] = i;
			return o;
		}
		var firsts = [first(), first(), first()];
		var rec, tab, small, pl;
		for (var n = 0; n < 200; n++) { rec = record(); tab = table(60); pl = plain(); }
		small = table(2);`))
	if err != nil {
		t.Fatal(err)
	}
	rec := globalObject(r, "rec")
	if n, c := len(rec.vals), cap(rec.vals); n != 20 || c != 20 {
		t.Errorf("a record's table: %d properties in room for %d, want 20 in 20", n, c)
	}
	if root := rec.shape.root(); root.slack != 20 {
		t.Errorf("the record literal's root notes room for %d, want 20", root.slack)
	}
	// The second object grows once, to what the first came to have along
	// the transitions it follows; the third is made with it.
	if third := globalObject(r, "firsts").elemsOf()[2].Object(); len(third.vals) != 20 || cap(third.vals) != 20 {
		t.Errorf("a literal's third object: %d properties in room for %d, want 20 in 20", len(third.vals), cap(third.vals))
	}
	if tab := globalObject(r, "tab"); !tab.shape.unique || len(tab.vals) != 60 {
		t.Errorf("a table of 60 names is not a dictionary: unique %v, %d properties", tab.shape.unique, len(tab.vals))
	}
	small := globalObject(r, "small")
	if root := small.shape.root(); root.slack > maxSlack+1 {
		t.Errorf("the table literal's root notes room for %d: its keyed stores taught it", root.slack)
	}
	pl := globalObject(r, "pl")
	if root := pl.shape.root(); root != r.shapes.roots[ClassObject] || root.slack > maxSlack+1 {
		t.Errorf("objects of no site taught their class's root room for %d", root.slack)
	}
}

// A lookup's recent entry for a key the shapes' shared index has past an
// object's table answers that the object has no such key, and the object
// that has it finds it: objects built along one chain of twenty keys, each
// stopped at its own length, then one of them extended, one rolled back
// by a delete of its last key, one given a key's tombstone and the key
// again.
func TestFindOwnPastTheTable(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function make(n) { var o = {}; for (var i = 0; i < n; i++) o["k" + i] = i; return o; }
		var all = make(20), objs = [], out = [];
		for (var n = 0; n <= 20; n++) objs.push(make(n));
		for (var n = 0; n <= 20; n++) {
			var o = objs[n], s = 0;
			for (var round = 0; round < 3; round++)
				for (var i = 0; i < 20; i++) if (("k" + i) in o) s += 1 << i;
			out.push(s);
		}
		var x = objs[10]; x.k10 = "ten"; x.k11 = "eleven";
		var y = objs[15]; delete y.k14;
		var z = objs[18]; delete z.k3; var had = "k3" in z; z.k3 = "three";
		out.push(x.k10, x.k11, "k12" in x, Object.keys(x).length, "k14" in y, "k13" in y, Object.keys(y).length,
			had, z.k3, Object.keys(z).slice(-2).join(), "k18" in z, Object.keys(all).length);
		out.join(" ")`))
	if err != nil {
		t.Fatal(err)
	}
	// Node v26.10.0's answer.
	want := "0 3 9 21 45 93 189 381 765 1533 3069 6141 12285 24573 49149 98301 196605 393213 786429 1572861 3145725 " +
		"ten eleven false 12 false true 14 false three k17,k3 false 20"
	if got := v.String().Go(); got != want {
		t.Errorf("got  %s, want %s", got, want)
	}
}
