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
