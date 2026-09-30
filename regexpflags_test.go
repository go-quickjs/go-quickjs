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

func TestGlobalReplaceLeavesWhatItsExecsWould(t *testing.T) {
	// The matches of a global replace are found in one go; lastIndex and the
	// legacy statics end as the last of its calls to exec left them.
	checkEval(t, `(function () {
		var g = /(\d)/g; g.lastIndex = 4;
		var out = ["a1b2c3".replace(g, "x"), RegExp.lastMatch, RegExp.$1, RegExp.leftContext, g.lastIndex];
		out.push("aab".replace(/a/gy, "X"), "baa".replace(/a/gy, "X"), "x".replace(/y/g, "z"), RegExp.lastMatch);
		return JSON.stringify(out);
	})()`, `["axbxcx","3","3","a1b2c",0,"XXb","baa","x","a"]`)
}

func TestSplitWithTheBuiltInExec(t *testing.T) {
	// split with an untouched RegExp searches for each separator rather than
	// trying one at every position; the pieces, the captures, the limits and
	// the legacy statics are what they were.
	checkEval(t, `(function () {
		var out = [];
		out.push("a,b,,c".split(/,/), "a1b22c".split(/(\d)(x)?/), "abc".split(/(?:)/), "abc".split(/x*/),
			"a, b ,c".split(/\s*,\s*/, 2), "a1b2".split(/(\d)/, 2), "".split(/x/), "".split(/(?:)/),
			"\u{1F600}x\u{1F600}".split(/(?:)/u), "aXbxc".split(/x/i), "abc".split(/b/y));
		"q1w2e".split(/(\d)/);
		out.push(RegExp.lastMatch, RegExp.$1);
		class R extends RegExp {}
		out.push("a-b".split(new R("-")));
		return JSON.stringify(out);
	})()`, `[["a","b","","c"],["a","1",null,"b","2",null,"","2",null,"c"],["a","b","c"],["a","b","c"],["a","b"],["a","1"],[""],[],["😀","x","😀"],["a","b","c"],["a","c"],"2","2",["a","b"]]`)
}
