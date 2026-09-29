package quickjs_test

import "testing"

func TestRegExpFlagsAreReadAsTheScriptLeftThem(t *testing.T) {
	// An untouched RegExp's flags are taken from its pattern; any change a
	// script makes to how they are read is still what the methods see.
	checkEval(t, `(function () {
		var out = [];
		out.push("aaa".replace(/a/g, "b"), "aaa".replace(/a/, "b"));
		var d = Object.getOwnPropertyDescriptor(RegExp.prototype, "global");
		Object.defineProperty(RegExp.prototype, "global", { get() { return false; }, configurable: true });
		out.push("aaa".replace(/a/g, "b"));
		Object.defineProperty(RegExp.prototype, "global", d);
		out.push("aaa".replace(/a/g, "b"));
		var re = /a/g; Object.defineProperty(re, "flags", { value: "" });
		out.push("aaa".replace(re, "c"));
		class R extends RegExp { get flags() { return ""; } }
		out.push("aaa".replace(new R("a", "g"), "d"));
		var log = [];
		var fd = Object.getOwnPropertyDescriptor(RegExp.prototype, "flags");
		Object.defineProperty(RegExp.prototype, "flags", { get() { log.push("flags"); return fd.get.call(this); } });
		out.push("aaa".replace(/a/g, "e"), log.length);
		Object.defineProperty(RegExp.prototype, "flags", fd);
		delete RegExp.prototype.sticky;
		out.push("aaa".replace(/a/g, "f"), "a,b".split(/,/).length);
		return JSON.stringify(out);
	})()`, `["bbb","baa","baa","bbb","caa","daa","eee",1,"fff",2]`)
}
