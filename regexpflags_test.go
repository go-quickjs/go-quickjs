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

func TestReplaceWithTheBuiltInExec(t *testing.T) {
	// replace with an untouched RegExp keeps its matches as indices rather
	// than as exec's arrays; everything a script can see of the matching is
	// what it was.
	checkEval(t, `(function () {
		var out = [];
		out.push("a1b22c".replace(/(\d)(x)?/g, function (m, d, x, pos, s) {
			return "[" + m + d + x + pos + s.length + RegExp.$1 + "]";
		}));
		out.push("abc".replace(/x*/g, "-"), "\u{1F600}".replace(/(?:)/gu, "."), "aaa".replace(/a/, "$&$&"));
		var ro = /a/g; Object.defineProperty(ro, "lastIndex", { writable: false });
		try { "aa".replace(ro, "b"); } catch (e) { out.push(e.constructor.name); }
		var sticky = /a/y; sticky.lastIndex = 1; out.push("aab".replace(sticky, "X"), sticky.lastIndex);
		var own = /a/g; own.exec = function () { return null; }; out.push("aa".replace(own, "b"));
		var named = /(?<n>a)/g; out.push("aa".replace(named, "$<n>!"), "aa".replace(named, function () { return typeof arguments[arguments.length - 1]; }));
		var saved = RegExp.prototype.exec;
		RegExp.prototype.exec = function (s) { out.push("exec"); return saved.call(this, s); };
		out.push("ab".replace(/b/, "c"));
		RegExp.prototype.exec = saved;
		var g = /b/g; "abb".replace(g, "c"); out.push(g.lastIndex);
		return JSON.stringify(out);
	})()`, `["a[11undefined162]b[22undefined362][22undefined462]c","-a-b-c-",".😀.","aaaa","TypeError","aXb",2,"aa","a!a!","objectobject","exec","ac",0]`)
}
