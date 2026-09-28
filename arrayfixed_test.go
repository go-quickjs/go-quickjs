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
