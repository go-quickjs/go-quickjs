package quickjs_test

import (
	"strings"
	"testing"
)

// TestFixedArraysStayFixed pins that an array whose length is non-writable,
// or which may not be extended, gains no element by assignment: one past a
// read-only length extended the array, and a non-extensible one had its holes
// filled, its raised length written past, and an arguments object gained
// keys (KI-31, KI-32). The answers are node's.
func TestFixedArraysStayFixed(t *testing.T) {
	src := `const out = [];
		const t = (name, f) => { try { out.push(name + ": " + f()) } catch (e) { out.push(name + ": " + e.constructor.name) } };
		const ro = (a) => Object.defineProperty(a, "length", {writable: false});
		t("len-ro sparse", () => { const a = ro([1,2,3]); a[5000] = 4; return a.length + " " + (5000 in a); });
		t("len-ro next", () => { const a = ro([1,2,3]); a[3] = 4; return a.length + " " + (3 in a); });
		t("len-ro strict", () => { "use strict"; const a = ro([1,2,3]); a[3] = 4; return a.length; });
		t("len-ro loop", () => { const a = ro([1,2]); for (let i = 0; i < 5; i++) a[i] = 9; return JSON.stringify(a); });
		t("len-ro Reflect.set", () => { const a = ro([1]); return Reflect.set(a, 1, 5) + " " + Reflect.set(a, 0, 5); });
		t("pe hole", () => { const a = [1,,3]; Object.preventExtensions(a); a[1] = 2; return (1 in a) + " " + a.length; });
		t("pe grow", () => { const a = [1,2]; Object.preventExtensions(a); a[2] = 3; return (2 in a) + " " + a.length; });
		t("pe length", () => { const a = [1,2]; Object.preventExtensions(a); a.length = 5; a[4] = 1; return a.length + " " + (4 in a); });
		t("pe strict hole", () => { "use strict"; const a = [1,,3]; Object.preventExtensions(a); a[1] = 2; return a[1]; });
		t("pe loop", () => { const a = [1,,3]; Object.preventExtensions(a); for (let i = 0; i < 5; i++) a[i] = i; return JSON.stringify(a); });
		t("pe Reflect.set", () => { const a = [1,,3]; Object.preventExtensions(a); return Reflect.set(a, 1, 5) + " " + Reflect.set(a, 0, 5); });
		t("pe arguments", () => { function f(a) { Object.preventExtensions(arguments); arguments[1] = 1; return 1 in arguments; } return f(1); });
		t("sealed", () => { const a = Object.seal([1,,3]); a[1] = 1; a[0] = 7; return JSON.stringify(a); });
		out.join("\n")`
	want := strings.Join([]string{
		"len-ro sparse: 3 false",
		"len-ro next: 3 false",
		"len-ro strict: TypeError",
		"len-ro loop: [9,9]",
		"len-ro Reflect.set: false true",
		"pe hole: false 3",
		"pe grow: false 2",
		"pe length: 5 false",
		"pe strict hole: TypeError",
		"pe loop: [0,null,2]",
		"pe Reflect.set: false true",
		"pe arguments: false",
		"sealed: [7,null,3]",
	}, "\n")
	checkEval(t, src, want)
}

// TestIndexOrderWithAttributes pins that an element given an accessor or
// attributes -- which moves it out of an array's dense storage -- is still
// listed where its index puts it, by Reflect.ownKeys, for-in and
// Object.assign: it came after the rest (KI-35). The answers are node's.
func TestIndexOrderWithAttributes(t *testing.T) {
	checkEval(t, `const out = [];
		const a = [0, 1, 2, 3]; Object.defineProperty(a, 1, {get() { return 9 }, enumerable: true});
		out.push(Reflect.ownKeys(a).join());
		const b = [0, 1, 2]; Object.defineProperty(b, 0, {value: 5, writable: false, enumerable: true, configurable: true}); b.x = 1; b[10] = 1;
		out.push(Reflect.ownKeys(b).join());
		const k = []; for (const i in a) k.push(i); out.push(k.join());
		out.push(JSON.stringify(Object.assign({}, a)));
		const c = [1,2,3]; Object.defineProperty(c, 2, {get() { return 1 }}); c.length = 5; c[3] = 1; out.push(Reflect.ownKeys(c).join());
		out.join(" | ")`,
		`0,1,2,3,length | 0,1,2,10,length,x | 0,1,2,3 | {"0":0,"1":9,"2":2,"3":3} | 0,1,2,3,length`)
}

// TestForOfArrayFastPath pins that the ways an array is iterated without the
// protocol's objects -- for-of, spread -- answer as the protocol does: they
// go to the array's length, where for-of stopped at the end of its dense
// storage, so freezing the array or writing far past its end ended the
// loop; and a replaced %ArrayIteratorPrototype%.next is called, where it
// was ignored (KI-36). The answers are node's.
func TestForOfArrayFastPath(t *testing.T) {
	checkEval(t, `const out = [];
		{ const a = [1, 2, 3]; const r = []; for (const v of a) { r.push(v); if (v === 1) Object.freeze(a); } out.push("freeze " + r.join()); }
		{ const a = [1, 2]; const r = []; for (const v of a) { r.push(v); if (v === 1) a[5000] = 9; if (r.length > 10) break; } out.push("sparse " + r.length + " " + r[r.length - 1]); }
		{ const a = [1, 2, 3]; const r = []; for (const v of a) { r.push(v); if (v === 1) Object.defineProperty(a, 2, {get() { return "g" }}); } out.push("accessor " + r.join()); }
		{ const a = [1, 2, 3]; const r = []; for (const v of a) { r.push(v); a.length = 1; } out.push("shrink " + r.join()); }
		const AIP = Object.getPrototypeOf([][Symbol.iterator]()); const orig = AIP.next;
		let n = 0; AIP.next = function () { n++; return orig.call(this); };
		for (const v of [1, 2]) ; out.push("for-of next " + n);
		n = 0; [...[1, 2]]; out.push("spread next " + n);
		n = 0; Math.max(...[1, 2]); out.push("call spread next " + n);
		AIP.next = orig;
		out.join(" | ")`,
		"freeze 1,2,3 | sparse 11 undefined | accessor 1,2,g | shrink 1 | for-of next 3 | spread next 3 | call spread next 3")
}

// TestArrayFillFastPath pins that fill, which writes a plain dense array's
// elements in place, answers as the property protocol does: holes made
// properties, a range given or negative, an index an inherited setter
// takes, a read-only element, a non-extensible or frozen array, a valueOf
// that shortens or grows the array, a subclass, a primitive and an
// array-like; and an array sparse only for its length, filled whole --
// dense after -- or in part, one with an element in its table, and one
// longer than an atom's indices. The answers are node's.
func TestArrayFillFastPath(t *testing.T) {
	checkEval(t, `const out = [];
		const t = (name, f) => { try { out.push(name + ": " + f()) } catch (e) { out.push(name + ": " + e.constructor.name) } };
		const show = (a) => Array.from({length: a.length}, (_, i) => (i in a) ? String(a[i]) : "_").join(",") + " len " + a.length;
		t("holes", () => show(new Array(5).fill(1)));
		t("keys", () => Object.keys(new Array(4).fill(0)).join(""));
		t("part", () => show([,,,,].fill(7, 1, 3)));
		t("neg", () => show([1,2,3,4,5].fill(0, -3, -1)));
		t("over", () => show([1,2,3].fill(9, 1, 100)));
		t("setter", () => { const log = []; Object.defineProperty(Array.prototype, "1", {set(v) { log.push(v) }, configurable: true}); try { const a = [,,,].fill(9); return show(a) + " log " + log } finally { delete Array.prototype[1] } });
		t("own ro", () => { const a = [1,2,3]; Object.defineProperty(a, "1", {writable: false}); a.fill(5); return show(a) });
		t("pe holes", () => { const a = [1,,3]; Object.preventExtensions(a); a.fill(5); return show(a) });
		t("pe full", () => { const a = [1,2,3]; Object.preventExtensions(a); a.fill(5); return show(a) });
		t("frozen", () => Object.freeze([1,2]).fill(3));
		t("frozen empty range", () => show(Object.freeze([1,2]).fill(3, 2)));
		t("shrunk", () => { const a = [1,2,3,4]; a.fill(0, {valueOf() { a.length = 1; return 0 }}); return show(a) });
		t("grown", () => { const a = [1,2]; a.fill(0, 0, {valueOf() { a.push(8, 9); return 4 }}); return show(a) });
		t("subclass", () => { class A extends Array {} const a = new A(3); a.fill(2); return show(a) + " " + (a instanceof A) });
		t("primitive", () => typeof Array.prototype.fill.call("ab", 1));
		t("object", () => { const o = {length: 2}; Array.prototype.fill.call(o, 4); return o[0] + "" + o[1] });
		t("returns", () => { const a = [0]; return a.fill(1) === a });
		t("long", () => { const a = new Array(3000).fill(1); let s = 0; for (const x of a) s += x; return s + " " + a.length + " " + Object.keys(a).length });
		t("long push", () => { const a = new Array(2000).fill(0); a.push(7); return a.length + " " + a[2000] + " " + (1999 in a) });
		t("long part", () => { const a = new Array(3000); a.fill(1, 0, 10); return a.length + " " + (9 in a) + " " + (10 in a) + " " + (2999 in a) + " " + Object.keys(a).length });
		t("long middle", () => { const a = new Array(3000); a.fill(1, 100, 200); return (99 in a) + " " + (100 in a) + " " + (199 in a) + " " + (200 in a) + " " + Object.keys(a).length });
		t("long with key", () => { const a = new Array(3000); a[2500] = 5; a.fill(2); return a[2500] + " " + Object.keys(a).length + " " + a.length });
		t("long then length", () => { const a = new Array(3000).fill(3); a.length = 10; return a.length + " " + Object.keys(a).length + " " + a[9] });
		t("huge", () => { const a = []; a.length = 2 ** 32 - 1; a.fill(1, 0, 3); return a.length + " " + Object.keys(a).join() });
		t("big", () => { const a = []; a.length = 2 ** 31 + 5; a.fill(4, 2 ** 31, 2 ** 31 + 2); return Object.keys(a).join() });
		t("sort after", () => { const a = new Array(1500).fill(2); a[3] = 1; a.sort(); return a[0] + " " + a[1499] + " " + a.length });
		out.join("\n")`, "holes: 1,1,1,1,1 len 5\nkeys: 0123\npart: _,7,7,_ len 4\nneg: 1,2,0,0,5 len 5\nover: 1,9,9 len 3\nsetter: 9,undefined,9 len 3 log 9\nown ro: TypeError\npe holes: TypeError\npe full: 5,5,5 len 3\nfrozen: TypeError\nfrozen empty range: 1,2 len 2\nshrunk: 0,0,0,0 len 4\ngrown: 0,0,8,9 len 4\nsubclass: 2,2,2 len 3 true\nprimitive: TypeError\nobject: 44\nreturns: true\nlong: 3000 3000 3000\nlong push: 2001 7 true\nlong part: 3000 true false false 10\nlong middle: false true true false 100\nlong with key: 2 3000 3000\nlong then length: 10 10 3\nhuge: 4294967295 0,1,2\nbig: 2147483648,2147483649\nsort after: 1 2 1500")
}

// TestArrayHolesInDenseStorage pins that the holes new Array(n) makes, and
// those a length at most doubled adds, which dense storage now holds, are
// holes as before: absent to in, keys, forEach, JSON, join, sort; shadowed
// by nothing but an inherited index; and those past the bounds -- a length
// of 70000, of 2**32-1, a length more than doubled -- as well. The answers
// are node's.
func TestArrayHolesInDenseStorage(t *testing.T) {
	checkEval(t, `const out = [];
		const t = (name, f) => { try { out.push(name + ": " + f()) } catch (e) { out.push(name + ": " + e.constructor.name) } };
		t("ctor", () => { const a = new Array(5000); return a.length + " " + (0 in a) + " " + Object.keys(a).length + " " + a.indexOf(undefined) + " " + a.includes(undefined) });
		t("ctor write", () => { const a = new Array(5000); a[4999] = 1; a[7] = 2; return Object.keys(a).join() + " " + a.length });
		t("ctor call", () => { const a = Array(3000); return a.length + " " + (2999 in a) });
		t("ctor forEach", () => { const a = new Array(2000); a[5] = 1; let n = 0; a.forEach(() => n++); return n });
		t("ctor json", () => JSON.stringify(new Array(1100)).length);
		t("ctor join", () => new Array(1500).join("-").length);
		t("ctor big", () => { const a = new Array(70000); a[69999] = 3; return a.length + " " + Object.keys(a).join() });
		t("ctor huge", () => { const a = new Array(2 ** 32 - 1); return a.length + " " + (0 in a) });
		t("ctor proto index", () => { Array.prototype[3] = "p"; try { const a = new Array(2000); return a[3] + " " + (3 in a) + " " + a.hasOwnProperty(3) } finally { delete Array.prototype[3] } });
		t("double", () => { const a = []; for (let i = 0; i < 2000; i++) a.push(i); a.length = 4000; return (1999 in a) + " " + (2000 in a) + " " + (3999 in a) + " " + Object.keys(a).length + " " + a.length });
		t("double write", () => { const a = []; for (let i = 0; i < 2000; i++) a.push(i); a.length = 4000; a[3999] = "z"; a[2500] = "y"; return Object.keys(a).length + " " + a[2500] + a[3999] + " " + a.lastIndexOf("z") });
		t("triple", () => { const a = []; for (let i = 0; i < 2000; i++) a.push(i); a.length = 7000; return (5000 in a) + " " + Object.keys(a).length + " " + a.length });
		t("shrink after", () => { const a = Array.from({length: 2000}, (_, i) => i); a.length = 4000; a.length = 10; return a.length + " " + Object.keys(a).length });
		t("vector", () => { let s = Array.from({length: 1500}, (_, i) => i); for (let k = 0; k < 3; k++) { s = s.slice(); s.length = s.length * 2; } return s.length + " " + Object.keys(s).length + " " + (s[1499]) + " " + (1500 in s) });
		t("sort holes", () => { const a = new Array(1200); a[3] = 2; a[1] = 1; a.sort(); return a[0] + " " + a[1] + " " + (2 in a) + " " + a.length });
		t("frozen", () => { const a = new Array(1100); Object.freeze(a); try { "use strict"; a[0] = 1 } catch (e) { return e.constructor.name } return (0 in a) });
		out.join("\n")`, "ctor: 5000 false 0 -1 true\nctor write: 7,4999 5000\nctor call: 3000 false\nctor forEach: 1\nctor json: 5501\nctor join: 1499\nctor big: 70000 69999\nctor huge: 4294967295 false\nctor proto index: p true false\ndouble: true false false 2000 4000\ndouble write: 2002 yz 3999\ntriple: false 2000 7000\nshrink after: 10 10\nvector: 12000 1500 1499 false\nsort holes: 1 2 false 1200\nfrozen: false")
}
