package quickjs_test

import (
	"strings"
	"testing"
)

// TestProxyInvariantsAskTheTarget pins that a proxy's has and ownKeys traps
// are held to what the target says of itself when asked -- an array's length,
// a String's characters, a function's prototype, frozen elements, a proxy
// target's traps -- not to its property table, which those are not in, so a
// trap could deny them (KI-33). The answers are node's.
func TestProxyInvariantsAskTheTarget(t *testing.T) {
	src := `const out = [];
		const t = (name, f) => { try { out.push(name + ": " + f()) } catch (e) { out.push(name + ": " + e.constructor.name) } };
		t("has array length", () => "length" in new Proxy([], {has() { return false }}));
		t("has string index", () => 0 in new Proxy(new String("ab"), {has() { return false }}));
		t("has fn prototype", () => "prototype" in new Proxy(function f() {}, {has() { return false }}));
		t("has frozen dense", () => 0 in new Proxy(Object.freeze([1]), {has() { return false }}));
		t("has proxy target", () => "x" in new Proxy(new Proxy(Object.freeze({x: 1}), {}), {has() { return false }}));
		t("has nonext", () => "a" in new Proxy(Object.preventExtensions({a: 1}), {has() { return false }}));
		t("keys array length", () => Reflect.ownKeys(new Proxy([], {ownKeys() { return [] }})).length);
		t("keys string", () => Reflect.ownKeys(new Proxy(new String("ab"), {ownKeys() { return ["length"] }})).length);
		t("keys fn prototype", () => Reflect.ownKeys(new Proxy(function f() {}, {ownKeys() { return ["length", "name"] }})).length);
		t("keys frozen dense", () => Reflect.ownKeys(new Proxy(Object.freeze([1]), {ownKeys() { return ["length"] }})).length);
		t("keys proxy target", () => Reflect.ownKeys(new Proxy(new Proxy(Object.freeze({x: 1}), {}), {ownKeys() { return [] }})).length);
		t("keys ok", () => Reflect.ownKeys(new Proxy([1], {ownKeys() { return ["0", "length", "z"] }})).join());
		t("keys nonext invent", () => Reflect.ownKeys(new Proxy(Object.preventExtensions({a: 1}), {ownKeys() { return ["a", "b"] }})).join());
		out.join("\n")`
	want := strings.Join([]string{
		"has array length: TypeError",
		"has string index: TypeError",
		"has fn prototype: TypeError",
		"has frozen dense: TypeError",
		"has proxy target: TypeError",
		"has nonext: TypeError",
		"keys array length: TypeError",
		"keys string: TypeError",
		"keys fn prototype: TypeError",
		"keys frozen dense: TypeError",
		"keys proxy target: TypeError",
		"keys ok: 0,length,z",
		"keys nonext invent: TypeError",
	}, "\n")
	checkEval(t, src, want)
}

// TestGlobalProxyTrapErrors pins that a proxy on the global object's chain
// is asked whether it has a name before it is asked for it, as V8 asks it,
// and that what its trap throws is thrown: an undeclared name was a
// ReferenceError and its typeof undefined whatever the trap threw (KI-34).
func TestGlobalProxyTrapErrors(t *testing.T) {
	checkEval(t, `const log = [];
		Object.setPrototypeOf(globalThis, new Proxy(Object.getPrototypeOf(globalThis), {
			has(t, k) { if (k === "boom") throw "boom"; if (k.startsWith?.("q")) log.push("has " + k); return k === "qfound" || Reflect.has(t, k) },
			get(t, k, r) { if (k.startsWith?.("q")) log.push("get " + k); return k === "qfound" ? 5 : Reflect.get(t, k, r) },
		}));
		const run = (s) => { log.length = 0; let r; try { r = (0, eval)(s) } catch (e) { r = "threw " + (e.name || e) } return r + " (" + log.join(", ") + ")" };
		[run("boom"), run("typeof boom"), run("qfound"), run("qmissing"), run("typeof qfound"), run("typeof qmissing")].join(" | ")`,
		"threw boom () | threw boom () | 5 (has qfound, get qfound) | threw ReferenceError (has qmissing) | "+
			"number (has qfound, get qfound) | undefined (has qmissing)")
}
