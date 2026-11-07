package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// jsonRepeated runs each case four times in one runtime, and then all of them
// in another: what JSON keeps -- a place's last layout, a layout's record,
// which is made at its second sighting, its buffers -- is what the runs after
// the first are reading, and a case must not see what another left.
func jsonRepeated(t *testing.T, cases []struct{ src, want string }) {
	t.Helper()
	shared := quickjs.New()
	defer shared.Close()
	for _, tc := range cases {
		rt := quickjs.New()
		for run := range 4 {
			v, err := rt.Eval(tc.src)
			if err != nil {
				t.Errorf("run %d: %s: %v", run, tc.src, err)
				break
			}
			if got := v.String(); got != tc.want {
				t.Errorf("run %d: %s = %q, want %q", run, tc.src, got, tc.want)
				break
			}
		}
		rt.Close()
	}
	for run := range 2 {
		for _, tc := range cases {
			v, err := shared.Eval("(function () {" + "return (0, eval)(" + strconv.Quote(tc.src) + ");})()")
			if err != nil {
				t.Errorf("shared run %d: %s: %v", run, tc.src, err)
			} else if got := v.String(); got != tc.want {
				t.Errorf("shared run %d: %s = %q, want %q", run, tc.src, got, tc.want)
			}
		}
	}
}

// The text of a key is compared with the name a layout expects, which must
// not be taken for more than it is.
func TestJSONParseExpectedKeys(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		// A name ending in a backslash, and text that starts the same but
		// whose backslash escapes the quotation mark after it.
		{`var bs = String.fromCharCode(92), q = String.fromCharCode(34);
		  var one = JSON.parse('{"a' + bs + bs + '":1}');
		  var two = JSON.parse('{"a' + bs + q + 'b":2}');
		  [Object.keys(one)[0].length, Object.keys(two)[0].length, Object.keys(two)[0] === "a" + q + "b"].join()`,
			"2,3,true"},
		// An escaped key is the key it spells.
		{`var a = JSON.parse('{"ab":1}'), b = JSON.parse('{"a' + String.fromCharCode(92) + 'x62":2}'.replace("x62", "u0062"));
		  Object.keys(b)[0] + b.ab`, "ab2"},
		// A transition by a symbol whose description is empty is not "".
		{`var o = new Object(); o[Symbol("")] = 1; var p = JSON.parse('{"":2}');
		  Object.getOwnPropertySymbols(p).length + "," + Object.keys(p).length + "," + p[""]`, "0,1,2"},
		// One by a key of other attributes is not this key.
		{`var o = new Object(); Object.defineProperty(o, "hidden", {value: 1}); var p = JSON.parse('{"hidden":2}');
		  JSON.stringify(Object.getOwnPropertyDescriptor(p, "hidden"))`,
			`{"value":2,"writable":true,"enumerable":true,"configurable":true}`},
		{`var o = new Object(); Object.defineProperty(o, "acc", {get() { return 1; }, enumerable: true});
		  var p = JSON.parse('{"acc":2}'); typeof Object.getOwnPropertyDescriptor(p, "acc").value`, "number"},
		// A setter on the prototype is not called, and __proto__ is a key.
		{`var hit = 0; Object.defineProperty(Object.prototype, "viaSetter", {set(v) { hit++; }, configurable: true});
		  var p = JSON.parse('{"viaSetter":1}'); delete Object.prototype.viaSetter; hit + "," + p.viaSetter`, "0,1"},
		{`var p = JSON.parse('{"__proto__":{"x":1},"y":2}');
		  [Object.getPrototypeOf(p) === Object.prototype, p.x === undefined, Object.keys(p).join("|")].join()`,
			"true,true,__proto__|y"},
		// Keys that are indices come first, whichever way the object was made.
		{`JSON.stringify(Object.keys(JSON.parse('{"b":1,"1":2,"a":3,"0":4}')))`, `["0","1","b","a"]`},
		{`var a = JSON.parse('{"2":"x","1":"y"}'), b = JSON.parse('{"1":"y","2":"x"}');
		  JSON.stringify(a) + JSON.stringify(b)`, `{"1":"y","2":"x"}{"1":"y","2":"x"}`},
		{`JSON.parse('{"4294967294":1,"4294967295":2,"4294967296":3}')[4294967294]`, "1"},
	})
}

// A key given twice is the last, where the first left it.
func TestJSONParseDuplicateKeys(t *testing.T) {
	long := `{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10}`
	jsonRepeated(t, []struct{ src, want string }{
		{`JSON.stringify(JSON.parse('{"a":1,"b":2,"a":3}'))`, `{"a":3,"b":2}`},
		// After objects that make the first keys what is expected.
		{`JSON.stringify(JSON.parse('[{"a":1,"b":2},{"a":1,"b":2,"a":3},{"b":1,"a":2,"b":3},{"a":1,"a":2}]'))`,
			`[{"a":1,"b":2},{"a":3,"b":2},{"b":3,"a":2},{"a":2}]`},
		{`JSON.stringify(JSON.parse('{"x":{"a":1,"a":{"b":1,"b":2}}}'))`, `{"x":{"a":{"b":2}}}`},
		// Past the keys a table scans, and at the end of a long one.
		{`var t = '` + long + `'; JSON.parse(t); var d = t.slice(0, -1) + ',"k3":"again"}';
		  var p = JSON.parse(d); p.k3 + "," + Object.keys(p).length + "," + p.k10`, "again,11,10"},
		{`var o = JSON.parse('{"a":1,"a":2,"b":3,"c":4,"a":5}'); o.d = 6; JSON.stringify(o)`,
			`{"a":5,"b":3,"c":4,"d":6}`},
	})
}

// An object is made in the size it has, which is no limit on it, and shares a
// layout with objects it must not disturb.
func TestJSONParsedObjectsGrow(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var out = [];
		  for (var n = 1; n <= 12; n++) {
		    var pairs = [];
		    for (var i = 0; i < n; i++) pairs.push('"p' + i + '":' + i);
		    var text = "{" + pairs.join() + "}";
		    var a = JSON.parse(text), b = JSON.parse(text);
		    a.extra = 1; delete a.p0;
		    var sum = 0;
		    for (var i = 0; i < n; i++) sum += b["p" + i];
		    out.push(Object.keys(a).length + ":" + Object.keys(b).length + ":" + sum);
		  }
		  out.join()`,
			"1:1:0,2:2:1,3:3:3,4:4:6,5:5:10,6:6:15,7:7:21,8:8:28,9:9:36,10:10:45,11:11:55,12:12:66"},
		{`var a = JSON.parse('[{"a":1,"b":2},{"a":3,"b":4}]'); Object.freeze(a[0]); a[1].a = 5;
		  "use strict"; a[0].a = 9; JSON.stringify(a)`, `[{"a":1,"b":2},{"a":5,"b":4}]`},
		// What reads objects finds them laid out as any others.
		{`function rd(o) { return o.a + o.b; }
		  var s = 0, a = JSON.parse('[{"a":1,"b":2},{"a":3,"b":4}]');
		  for (var i = 0; i < 2000; i++) s += rd(a[i & 1]) + rd({a: 1, b: 1});
		  s`, "14000"},
		{`var s = JSON.parse('["abc"]')[0]; var t = s; s += "x"; s += "y"; t + s + s.length`, "abcabcxy5"},
		{`var big = "{" + Array.from({length: 2000}, (_, i) => '"k' + i + '":' + i).join() + "}";
		  var o = JSON.parse(big), p = JSON.parse(big); p.k1999 + "," + Object.keys(o).length + "," + o.k1500`,
			"1999,2000,1500"},
		{`var o = JSON.parse('{}'); o.a = 1; o.b = 2; o.c = 3; o.d = 4; JSON.stringify(o)`, `{"a":1,"b":2,"c":3,"d":4}`},
	})
}

func TestJSONParseNumbers(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`[Object.is(JSON.parse("-0"), -0), Object.is(JSON.parse("-0.0"), -0), Object.is(JSON.parse("0"), 0),
		   Object.is(JSON.parse("-0e5"), -0), Object.is(JSON.parse("[-0]")[0], -0), Object.is(JSON.parse("-1e-400"), -0)].join()`,
			"true,true,true,true,true,true"},
		{`[JSON.parse("1e400"), JSON.parse("-1e400"), JSON.parse("1e-400"), JSON.parse("5e-324"),
		   JSON.parse("1.7976931348623157e308"), JSON.parse("9007199254740993"), JSON.parse("123456789012345678901234567890")].join()`,
			"Infinity,-Infinity,0,5e-324,1.7976931348623157e+308,9007199254740992,1.2345678901234568e+29"},
		{`[JSON.parse("[1]")[0], JSON.parse("[1,22,333]")[2], JSON.parse('{"a":4444}').a, JSON.parse("55555"), JSON.parse(" 6 "),
		   JSON.parse("[123456789012345]")[0], JSON.parse("[1234567890123456]")[0], JSON.parse("[0.1]")[0], JSON.parse("[12.5]")[0]].join()`,
			"1,333,4444,55555,6,123456789012345,1234567890123456,0.1,12.5"},
		{`var out = [];
		  for (var s of ["01", "-01", "1.", ".5", "-", "+1", "1e", "1e+", "1.e1", "00", "0x10", "1_000", "--1", "1 2", "[01]", "[1.]", '{"a":01}'])
		    try { JSON.parse(s); out.push("parsed " + s); } catch (e) { out.push(e.name); }
		  new Set(out).size + out[0]`, "1SyntaxError"},
	})
}

// Each of these is at every place a word of text can put it.
func TestJSONStringsAtEveryOffset(t *testing.T) {
	const reference = `
	function ref(s) {
		var out = '"';
		for (var i = 0; i < s.length; i++) {
			var c = s.charCodeAt(i);
			if (c === 34) out += '\\"';
			else if (c === 92) out += '\\\\';
			else if (c === 8) out += '\\b';
			else if (c === 12) out += '\\f';
			else if (c === 10) out += '\\n';
			else if (c === 13) out += '\\r';
			else if (c === 9) out += '\\t';
			else if (c < 32) out += '\\' + 'u00' + (c < 16 ? "0" : "") + c.toString(16);
			else if (c >= 0xD800 && c <= 0xDBFF) {
				var d = s.charCodeAt(i + 1);
				if (d >= 0xDC00 && d <= 0xDFFF) { out += s[i] + s[i + 1]; i++; }
				else out += '\\' + 'u' + c.toString(16);
			}
			else if (c >= 0xDC00 && c <= 0xDFFF) out += '\\' + 'u' + c.toString(16);
			else out += s[i];
		}
		return out + '"';
	}
	var bad = [];
	var chars = [[34], [92], [10], [1], [31], [32], [127], [128], [0xE9], [0x7FF], [0x800], [0x4E2D], [0xD7FF], [0xE000],
		[0xFFFD], [0xFFFF], [0xD83D, 0xDE00], [0xD83D], [0xDE00], [0xDE00, 0xD83D]];
	for (var c of chars) {
		var ch = String.fromCharCode.apply(null, c);
		for (var before = 0; before < 18; before++)
			for (var after = 0; after < 18; after += 3) {
				var s = "a".repeat(before) + ch + "b".repeat(after);
				var w = JSON.stringify(s);
				if (w !== ref(s) || w.length !== ref(s).length) bad.push("write " + c + " " + before + " " + after);
				if (JSON.parse(w) !== s) bad.push("parse " + c + " " + before + " " + after);
				var o = {};
				o[s] = s;
				var kw = JSON.stringify(o);
				if (kw !== "{" + ref(s) + ":" + ref(s) + "}" || kw.length !== 3 + 2 * ref(s).length) bad.push("key " + c + " " + before + " " + after);
				var p = JSON.parse(kw);
				if (Object.keys(p)[0] !== s || p[s] !== s) bad.push("parse key " + c + " " + before + " " + after);
				var wa = JSON.stringify([s, s]), slice = wa.slice(1, 1 + ref(s).length);
				if (slice !== ref(s) || wa[wa.length - 1] !== "]" || wa.charCodeAt(1 + ref(s).length) !== 44) bad.push("slice " + c);
			}
	}
	bad.length + (bad.length ? ": " + bad.slice(0, 5).join("; ") : "")
	`
	checkEval(t, reference, "0")
}

// A string with an escape says what it is as it is built: what its bytes
// are decides how it is indexed, and an escape may be what makes it wide.
func TestJSONEscapedStrings(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var u = String.fromCharCode(92) + "u", q = String.fromCharCode(34);
		  function p(t) { return JSON.parse(q + t + q); }
		  var r = [];
		  for (var t of [u + "0041", "x" + u + "0041y", u + "00e9", "x" + u + "00e9yz", u + "4e2d" + "ab", "ab" + u + "4e2d",
		                 u + "d83d" + u + "de00", "x" + u + "d83d" + u + "de00y", u + "d83d", u + "de00" + "x", u + "0000"]) {
		    var s = p(t), c = [];
		    for (var i = 0; i < s.length; i++) c.push(s.charCodeAt(i).toString(16));
		    r.push(s.length + ":" + c.join(" ") + ":" + s.at(-1).length + ":" + s.slice(1).length + ":" + (s + "z").length);
		  }
		  r.join("|")`,
			"1:41:1:0:2|3:78 41 79:1:2:4|1:e9:1:0:2|4:78 e9 79 7a:1:3:5|3:4e2d 61 62:1:2:4|3:61 62 4e2d:1:2:4|2:d83d de00:1:1:3|4:78 d83d de00 79:1:3:5|1:d83d:1:0:2|2:de00 78:1:1:3|1:0:1:0:2"},
		// Wide text, raw, around an escape.
		{`var e = String.fromCharCode(0xE9), n = String.fromCharCode(92) + "n", q = String.fromCharCode(34);
		  var s = JSON.parse(q + e + n + e + "a" + n + "bcdefghijklmnop" + e + q); [s.length, s.charCodeAt(0), s.charCodeAt(1), s.charCodeAt(s.length - 1)].join()`,
			"21,233,10,233"},
		// Text after an escape of every length around a run read by bytes.
		{`var n = String.fromCharCode(92) + "n", q = String.fromCharCode(34), nl = String.fromCharCode(10), ok = true;
		  for (var i = 0; i < 40; i++) { var run = "r".repeat(i), s = JSON.parse(q + n + run + n + run + q);
		    ok = ok && s === nl + run + nl + run; }
		  ok`, "true"},
		// Strings of a batch stay as they are as it fills, and past it.
		{`var nl = String.fromCharCode(10), a = []; for (var i = 0; i < 3000; i++) a.push("line" + nl + i);
		  var b = JSON.parse(JSON.stringify(a)), ok = true;
		  for (var i = 0; i < a.length; i++) ok = ok && a[i] === b[i];
		  var big = "x".repeat(5000) + nl, c = JSON.parse(JSON.stringify([big, "a" + nl, big]));
		  [ok, c[0] === big, c[1].length, c[2].length].join()`, "true,true,2,5001"},
		// A key with an escape is the key.
		{`var n = String.fromCharCode(92) + "n", q = String.fromCharCode(34);
		  var o = JSON.parse("{" + q + "a" + n + "b" + q + ":1," + q + "a" + n + "c" + q + ":2}");
		  Object.keys(o).map(k => k.length + ":" + k.charCodeAt(1)).join()`, "3:10,3:10"},
	})
}

// A string long enough to be read by the fastest way there is, ending at each
// place in a read and among the last bytes of the document.
func TestJSONLongStrings(t *testing.T) {
	checkEval(t, `var bad = [], wide = String.fromCharCode(0x4E2D), q = String.fromCharCode(34), bs = String.fromCharCode(92);
		for (var n = 30; n < 140; n++) {
			for (var tail of ["", "x", "xy", "xyzw"]) {
				var plain = "a".repeat(n) + tail, mixed = wide + plain, end = plain + wide;
				for (var s of [plain, mixed, end, plain + q, plain + bs, plain + String.fromCharCode(10) + "b", q + plain, plain + wide + q]) {
					var w = JSON.stringify(s);
					if (JSON.parse(w) !== s) bad.push("parse " + n);
					if (JSON.parse(w + " ") !== s || JSON.parse("[" + w + "]")[0] !== s) bad.push("in text " + n);
					try { JSON.parse(w.slice(0, -1)); bad.push("no end " + n); } catch (e) { if (e.name !== "SyntaxError") bad.push(e.name); }
					var o = {}; o[s] = 1;
					if (Object.keys(JSON.parse(JSON.stringify(o)))[0] !== s) bad.push("key " + n);
				}
				try { JSON.parse(q + plain + String.fromCharCode(1) + q); bad.push("control " + n); } catch (e) { if (e.name !== "SyntaxError") bad.push(e.name); }
			}
		}
		bad.length + (bad.length ? bad.slice(0, 4).join() : "")`, "0")
}

// What stringify writes it knows the length of, without reading it, and
// strings go on to index it.
func TestJSONStringifyLengths(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var e = String.fromCharCode(0xE9), h = String.fromCharCode(0xD83D), l = String.fromCharCode(0xDE00);
		  var t = JSON.stringify({a: e, b: [e + e, h + l, h, l + h]});
		  var ok = true;
		  for (var i = 0; i < t.length; i++) ok = ok && t[i] !== undefined;
		  [t.length, t.charCodeAt(t.length - 1), ok, t.isWellFormed()].join()`, "49,125,true,true"},
		// Only surrogates, escaped, leave nothing above ASCII.
		{`var t = JSON.stringify(String.fromCharCode(0xD83D)); [t, t.length, t === String.fromCharCode(34) + String.fromCharCode(92) + "ud83d" + String.fromCharCode(34)].join()`,
			`"\ud83d",8,true`},
		{`var k = String.fromCharCode(0x4E2D); var o = {}; o[k] = k + k; var t = JSON.stringify(o); [t.length, t.indexOf(":")].join()`, "10,4"},
		{`var i = String.fromCharCode(0x2192); var t = JSON.stringify([1], null, i); [t.length, t.charCodeAt(2)].join()`, "6,8594"},
		{`var r = JSON.rawJSON('"' + String.fromCharCode(0xE9) + '"'); var t = JSON.stringify([r]); [t.length, t.charCodeAt(2)].join()`, "5,233"},
		{`JSON.stringify("abc").length + "," + JSON.stringify(["x"]).length`, "5,5"},
		// A value dropped takes its key, wide or not, with it.
		{`var k = String.fromCharCode(0x4E2D); var o = {}; o[k] = undefined; o.a = 1; var t = JSON.stringify(o); [t, t.length].join()`, `{"a":1},7`},
	})
}

// A value is read when the walk reaches its key, from an object it may by
// then have changed: the keys are the ones it started with.
func TestJSONLayoutOfAChangingObject(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		// A getter deletes a key still to come, which is then undefined.
		{`function mk() { return {get a() { delete this.b; return 1; }, b: 2, c: 3}; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `{"a":1,"c":3}{"a":1,"c":3}`},
		// A key added is not visited.
		{`function mk() { return {get a() { this.z = 9; return 1; }, b: 2}; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `{"a":1,"b":2}{"a":1,"b":2}`},
		// A value changed is seen.
		{`function mk() { return {get a() { this.b = 7; return 1; }, b: 2}; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `{"a":1,"b":7}{"a":1,"b":7}`},
		// A toJSON of a value changes the object that holds it.
		{`function mk() { var o = {a: {toJSON() { o.b = "changed"; delete o.c; return 1; }}, b: 2, c: 3}; return o; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `{"a":1,"b":"changed"}{"a":1,"b":"changed"}`},
		// One that gives the object another layout, with a getter where a
		// value was.
		{`function mk() { var o = {a: {toJSON() { Object.defineProperty(o, "b", {get() { return "g"; }, enumerable: true}); return 1; }}, b: 2};
		  return o; } JSON.stringify(mk()) + JSON.stringify(mk())`, `{"a":1,"b":"g"}{"a":1,"b":"g"}`},
		// A replacer does the same.
		{`function mk() { return {a: 1, b: 2, c: 3}; }
		  var f = function (k, v) { if (k === "a") { delete this.b; this.c = 4; } return v; };
		  JSON.stringify(mk(), f) + JSON.stringify(mk(), f)`, `{"a":1,"c":4}{"a":1,"c":4}`},
		// An element a toJSON removes is null; one it adds is not visited.
		{`function mk() { var a = [{toJSON() { a.length = 2; a.push(9, 9, 9); return 1; }}, 2, 3]; return a; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `[1,2,9][1,2,9]`},
		{`function mk() { var a = [{toJSON() { a.length = 1; return 1; }}, 2, 3]; return a; }
		  JSON.stringify(mk()) + JSON.stringify(mk())`, `[1,null,null][1,null,null]`},
		// What is not written: private members, symbols, what is not enumerable.
		{`var C = class { #p = 1; q = 2; [Symbol.iterator] = 3; get r() { return 4; } };
		  var o = new C(); Object.defineProperty(o, "h", {value: 5}); JSON.stringify(o) + JSON.stringify(new C())`,
			`{"q":2}{"q":2}`},
		{`function mk() { var o = {a: 1, get b() { return 2; }, c: 3, d: undefined, e() {}, f: Symbol()}; return o; }
		  JSON.stringify(mk()) + JSON.stringify(mk(), null, 1)`, `{"a":1,"b":2,"c":3}{
 "a": 1,
 "b": 2,
 "c": 3
}`},
		{`function mk() { return {a: undefined}; } JSON.stringify(mk()) + JSON.stringify(mk(), null, 2) + JSON.stringify([mk()], null, 1)`,
			"{}{}[\n {}\n]"},
		// An allowed list of keys is the list, not the layout.
		{`var o = {a: 1, b: 2}; JSON.stringify(o) + JSON.stringify(o, ["b", "z", "a"])`, `{"a":1,"b":2}{"b":2,"a":1}`},
		// Objects of a layout with elements or internal slots have more than it.
		{`var o = {a: 1}, p = {a: 1}; p[0] = "x"; var q = new Number(3); q.a = 1;
		  JSON.stringify([o, p, o, p, q])`, `[{"a":1},{"0":"x","a":1},{"a":1},{"0":"x","a":1},3]`},
		{`var m = new Map(), r = /x/; m.a = 1; JSON.stringify([m, r, m])`, `[{"a":1},{},{"a":1}]`},
		// Object.create's objects keep their prototype apart from their layout.
		{`var p = {x: 1}, o = Object.create(p); o.y = 2; var q = Object.create(p); q.y = 3; JSON.stringify([o, q, o])`,
			`[{"y":2},{"y":3},{"y":2}]`},
		{`var n = Object.create(null); n.a = 1; var m = Object.create(null); m.a = 2; JSON.stringify([n, m, n])`,
			`[{"a":1},{"a":2},{"a":1}]`},
	})
}

// What was found on an object's chain holds until the chain changes.
func TestJSONToJSONFoundAnew(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var o = {a: 1}, a = JSON.stringify(o) + JSON.stringify(o);
		  Object.prototype.toJSON = function () { return "up"; };
		  var b = JSON.stringify(o); delete Object.prototype.toJSON; a + b + JSON.stringify(o)`,
			`{"a":1}{"a":1}"up"{"a":1}`},
		{`var l = [1], a = JSON.stringify(l) + JSON.stringify(l);
		  Array.prototype.toJSON = function () { return "up"; };
		  var b = JSON.stringify(l); delete Array.prototype.toJSON; a + b + JSON.stringify(l)`, `[1][1]"up"[1]`},
		// A write to Object.prototype made in the middle of a walk.
		{`var o = {a: {b: 1}, get c() { Object.prototype.toJSON = function () { return "late"; }; return 2; }, d: {e: 1}};
		  var t = JSON.stringify(o); delete Object.prototype.toJSON; t`, `{"a":{"b":1},"c":2,"d":"late"}`},
		{`var A = class {}, B = class extends A {}, b = new B(); b.x = 1; var t = JSON.stringify(b) + JSON.stringify(b);
		  A.prototype.toJSON = function () { return "a"; }; var u = JSON.stringify(b);
		  B.prototype.toJSON = function () { return "b"; }; var v = JSON.stringify(b);
		  delete B.prototype.toJSON; delete A.prototype.toJSON; t + u + v + JSON.stringify(b)`, `{"x":1}{"x":1}"a""b"{"x":1}`},
		// A prototype changed, with the object's layout the same.
		{`function P() { this.x = 1; } var a = new P(), t = JSON.stringify(a) + JSON.stringify(a);
		  Object.setPrototypeOf(P.prototype, {toJSON() { return "set"; }});
		  t + JSON.stringify(a)`, `{"x":1}{"x":1}"set"`},
		{`var o = {x: 1}, q = {x: 2}; var t = JSON.stringify(o) + JSON.stringify(o);
		  Object.setPrototypeOf(q, {toJSON() { return "own"; }}); t + JSON.stringify([q, o])`, `{"x":1}{"x":1}["own",{"x":1}]`},
		// On the object, as a value, an accessor, or something else.
		{`var g = 0, mk = () => ({get toJSON() { g++; return () => "got"; }, a: 1});
		  JSON.stringify(mk()) + JSON.stringify(mk()) + JSON.stringify(mk()) + g`, `"got""got""got"3`},
		{`var mk = () => ({toJSON: 1, a: 1}); JSON.stringify(mk()) + JSON.stringify(mk())`, `{"toJSON":1,"a":1}{"toJSON":1,"a":1}`},
		// A prototype that is a proxy is asked, every time.
		{`var log = 0, p = new Proxy({}, {get(t, k) { if (k === "toJSON") log++; }});
		  var o = Object.create(p); o.a = 1; var q = Object.create(p); q.a = 2;
		  JSON.stringify([o, q, o]) + log`, `[{"a":1},{"a":2},{"a":1}]3`},
		// A toJSON's key is a string.
		{`var ks = [], v = {toJSON(k) { ks.push(typeof k + k); return 1; }};
		  JSON.stringify([v, v]); JSON.stringify({x: v}); JSON.stringify(v); ks.join()`, "string0,string1,stringx,string"},
		// A BigInt asks its prototype.
		{`BigInt.prototype.toJSON = function () { return "big"; }; var t = JSON.stringify([1n]); delete BigInt.prototype.toJSON; t`, `["big"]`},
	})
}

// A namespace answers for its exports, none of which is in a table, so that
// one on an object's chain is asked as a proxy is.
func TestJSONToJSONOnANamespace(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		return `export function toJSON() { return "exported"; }`, specifier, nil
	})
	ns, err := rt.EvalModule("main.js", `
		import * as m from "./m.js";
		var a = Object.create(m), b = Object.create(m);
		for (var o of [a, b]) Object.defineProperty(o, "x", {value: 1, enumerable: true});
		export const out = JSON.stringify([a, b, a, b]);
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), `["exported","exported","exported","exported"]`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// toJSON and the replacer get their arguments from a buffer a walk uses
// again, which nothing may be left holding.
func TestJSONCallbacksKeepTheirArguments(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var kept = [];
		  JSON.stringify({a: 1, b: 2}, function (k, v) { kept.push(arguments); return v; });
		  kept.map(a => a[0] + ":" + typeof a[1]).join()`, ":object,a:number,b:number"},
		{`var kept = [];
		  JSON.stringify({a: 1, b: 2}, function (...r) { kept.push(r); return r[1]; });
		  kept.map(a => a[0]).join()`, ",a,b"},
		// A generator's arguments are read after the walk has moved on.
		{`var gens = [], v = {toJSON: function* (k) { yield k; }};
		  JSON.stringify([v, v, v], function (k, x) { if (x && x.next) { gens.push(x); return 0; } return x; });
		  gens.map(g => g.next().value).join()`, "0,1,2"},
		{`var seen = [], p = [], v = {toJSON: async function (k) { await 0; seen.push(k); }};
		  JSON.stringify({a: v, b: v}); 1`, "1"},
		{`var f = function (a, b) { return a + "/" + b; }.bind(null, "bound"), v = {toJSON: f}; JSON.stringify([v, v])`,
			`["bound/0","bound/1"]`},
		// A walk from inside a walk.
		{`var v = {toJSON(k) { return JSON.stringify({inner: k}); }}; JSON.stringify({a: v, b: v})`,
			`{"a":"{\"inner\":\"a\"}","b":"{\"inner\":\"b\"}"}`},
		{`var o = {x: 1}; o.y = {toJSON() { return JSON.stringify(o, ["x"]); }}; JSON.stringify(o)`, `{"x":1,"y":"{\"x\":1}"}`},
		{`JSON.stringify({a: 1}, (k, v) => k === "a" ? JSON.parse(JSON.stringify([v, v])) : v)`, `{"a":[1,1]}`},
		{`JSON.parse('{"a":1,"b":2}', (k, v) => typeof v === "number" ? JSON.parse('{"n":' + v + '}') : v).b.n`, "2"},
	})
}

// A cycle is an object being written, however far down.
func TestJSONCycles(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var a = {}; a.self = a; try { JSON.stringify(a); } catch (e) { e.name; }`, "TypeError"},
		{`var o = {}, x = o; for (var i = 0; i < 200; i++) x = x.n = {};
		  x.n = o; try { JSON.stringify(o); "no"; } catch (e) { e.name; }`, "TypeError"},
		{`var o = [], x = o; for (var i = 0; i < 200; i++) { var n = []; x.push(n); x = n; }
		  x.push(o); try { JSON.stringify(o); "no"; } catch (e) { e.name; }`, "TypeError"},
		// The same object twice, side by side, is no cycle.
		{`var s = {a: 1}, t = [s, s, {s: s, t: [s]}]; JSON.stringify(t)`, `[{"a":1},{"a":1},{"s":{"a":1},"t":[{"a":1}]}]`},
		// Nor at any depth.
		{`var s = [1]; var top = []; var x = top; for (var i = 0; i < 100; i++) { var n = [s]; x.push(n); x = n; }
		  JSON.stringify(top).length`, "601"},
		// Nor is one object written by a walk inside another's.
		{`var a = {toJSON() { return 1; }}, o = {v: 1, get g() { return JSON.stringify(o, ["v"]); }}; JSON.stringify(o)`,
			`{"v":1,"g":"{\"v\":1}"}`},
	})
}

// A failed walk leaves what the next one needs.
func TestJSONAfterAnError(t *testing.T) {
	checkEval(t, `var ok = 0, s = {a: 1}, deep = [], x = deep;
		for (var i = 0; i < 300; i++) { var n = []; x.push(n); x = n; }
		x.push({toJSON() { throw new Error("no"); }});
		for (var i = 0; i < 3000; i++) {
			try { JSON.stringify(deep); } catch (e) { ok++; }
		}
		ok + JSON.stringify(s) + JSON.stringify(deep[0][0].length)`, `3000{"a":1}1`)
	checkEval(t, `var c = {a: [1, 2, {}]}; c.a[2].c = c; var n = 0;
		for (var i = 0; i < 100; i++) { try { JSON.stringify(c); } catch (e) { n++; } }
		delete c.a[2].c; n + JSON.stringify(c, null, 1).length`, "129")
	checkEval(t, `var bad = 0; for (var i = 0; i < 100; i++) { try { JSON.parse('[1,2,{"a":[3,'); } catch (e) { bad++; } }
		bad + JSON.stringify(JSON.parse('[1,{"a":[]}]'))`, `100[1,{"a":[]}]`)
}

// A buffer is used again, and the text that was made in it stays as it was.
func TestJSONBuffersAreNotShared(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var a = "x".repeat(200000), b = "y".repeat(200000);
		  var s = JSON.stringify(a), t = JSON.stringify(b), u = JSON.stringify("short");
		  [s.length, s[1], s[199999], t[1], u].join()`, `200002,x,x,y,"short"`},
		{`var s = JSON.stringify("a".repeat(70000)), t = JSON.stringify("b".repeat(70000)); s[5] + t[5] + s.length`, "ab70002"},
		{`var parts = []; for (var i = 0; i < 40000; i++) parts.push("item" + i);
		  var s = JSON.stringify(parts), t = JSON.stringify(parts.slice().reverse());
		  [s.length, s.slice(0, 8), t.slice(0, 12), JSON.parse(s)[39999]].join()`, `468891,["item0",["item39999",item39999`},
		// Long strings among others, each past what the buffer has.
		{`var a = []; for (var i = 1; i <= 12; i++) a.push("z".repeat(1 << i), i); var s = JSON.stringify(a);
		  s.length + "," + (JSON.parse(s)[22].length)`, "8254,4096"},
		// An indent as long as any, at depth, then none, then another.
		{`var o = {a: [{b: [1]}]};
		  var x = JSON.stringify(o, null, 10), y = JSON.stringify(o), z = JSON.stringify(o, null, "--");
		  [x.length, y, z.length, z.split("\n")[3]].join("|")`, `187|{"a":[{"b":[1]}]}|59|------"b": [`},
	})
}

// A number of few places is written as the digits it was given.
func TestJSONStringifyNumbers(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`JSON.stringify([0, -0, 1, -1, 9, 10, 99, 100, 101, 999, 1000, 12345, 1234567, 99999999, 100000000, 4294967296])`,
			"[0,0,1,-1,9,10,99,100,101,999,1000,12345,1234567,99999999,100000000,4294967296]"},
		{`JSON.stringify([9007199254740991, -9007199254740991, 9007199254740992, 1e21, 1e20, 123456789012345680000])`,
			"[9007199254740991,-9007199254740991,9007199254740992,1e+21,100000000000000000000,123456789012345680000]"},
		{`JSON.stringify([0.5, -0.5, 1.25, 20.5, 0.1, 0.2, 0.1 + 0.2, 1.005, 100.01, 0.000001, 0.0000001, 123.456, 999999999.5, 1000000000.5])`,
			"[0.5,-0.5,1.25,20.5,0.1,0.2,0.30000000000000004,1.005,100.01,0.000001,1e-7,123.456,999999999.5,1000000000.5]"},
		{`JSON.stringify([NaN, Infinity, -Infinity, 5e-324, 1.7976931348623157e308])`, "[null,null,null,5e-324,1.7976931348623157e+308]"},
		{`var a = []; for (var i = 1; i < 2000; i++) a.push(i / 8, i / 100, i * 1.1); var t = JSON.stringify(a), b = JSON.parse(t), ok = true;
		  for (var i = 0; i < a.length; i++) ok = ok && a[i] === b[i] && String(a[i]) === String(b[i]);
		  ok + "," + (t === "[" + a.join(",") + "]")`, "true,true"},
	})
}

// What a reviver is given, and what it leaves.
func TestJSONReviverKeys(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		{`var seen = []; JSON.parse('{"a":[10,{"b":20}],"c":30}', function (k, v, c) {
		    seen.push(typeof k + ":" + k + ":" + (c.source === undefined ? "-" : c.source)); return v; });
		  seen.join()`, "string:0:10,string:b:20,string:1:-,string:a:-,string:c:30,string::-"},
		// A key removed and one added while the walk goes.
		{`JSON.stringify(JSON.parse('{"a":1,"b":2,"c":3}', function (k, v) { if (k === "a") { delete this.b; this.d = 4; } return v; }))`,
			`{"a":1,"c":3,"d":4}`},
		{`JSON.stringify(JSON.parse('{"a":1,"b":2}', function (k, v) { return k === "a" ? undefined : k === "b" ? v * 2 : v; }))`, `{"b":4}`},
		// A frozen holder keeps its value; a property made read-only too.
		{`JSON.stringify(JSON.parse('{"a":1,"b":2}', function (k, v) { if (k === "a") Object.freeze(this); return typeof v === "number" ? v + 1 : v; }))`,
			`{"a":1,"b":2}`},
		{`JSON.stringify(JSON.parse('{"a":1,"b":2}', function (k, v) { if (k === "a") Object.defineProperty(this, "b", {writable: false, configurable: false, value: 2});
		   return typeof v === "number" ? v + 1 : v; }))`, `{"a":2,"b":2}`},
		// An accessor put where a value was is replaced, as a definition does.
		{`var hit = 0; var o = JSON.parse('{"a":1,"b":2}', function (k, v) { if (k === "a") Object.defineProperty(this, "b", {get() { return 5; }, set(x) { hit++; }, configurable: true, enumerable: true});
		   return v; }); hit + JSON.stringify(Object.getOwnPropertyDescriptor(o, "b"))`, `0{"value":5,"writable":true,"enumerable":true,"configurable":true}`},
		// A reviver makes an array of what was one element, and so on.
		{`JSON.stringify(JSON.parse('[1,[2,[3]]]', function (k, v) { return typeof v === "number" ? [v] : v; }))`, "[[1],[[2],[[3]]]]"},
		{`var seen = []; JSON.parse('[1,2,3]', function (k, v) { if (k === "0") this.length = 1; seen.push(k); return v; }); seen.join()`, "0,1,2,"},
		{`var seen = []; JSON.parse('{"1":"a","0":"b","x":"c"}', function (k, v) { seen.push(k); return v; }); seen.join()`, "0,1,x,"},
	})
}

// What JSON keeps is a realm's, or a layout's, and a layout says its
// prototype: objects of two realms are not taken for each other's, and a
// toJSON one realm's prototypes gain is found on its objects alone. Each is
// written often enough to have a record, on either side of the change.
func TestJSONAcrossRealms(t *testing.T) {
	realmCases(t, []struct{ src, want string }{
		// A function runs in its realm, and parses into it.
		{`var a = JSON.parse('{"a":1,"b":{"c":2}}'), b = other.JSON.parse('{"a":1,"b":{"c":2}}'), c = JSON.parse('{"a":1,"b":{"c":2}}');
		  [Object.getPrototypeOf(a) === Object.prototype, Object.getPrototypeOf(b) === other.Object.prototype,
		   Object.getPrototypeOf(b.b) === other.Object.prototype, Object.getPrototypeOf(c.b) === Object.prototype].join()`, "true,true,true,true"},
		{`var mk = other.eval("(function () { return {p: 1, q: [2]}; })"), own = function () { return {p: 1, q: [2]}; };
		  var r = [];
		  for (var i = 0; i < 4; i++) r.push(JSON.stringify(mk()), other.JSON.stringify(own()));
		  other.Object.prototype.toJSON = function () { return "other"; };
		  r.push(JSON.stringify(mk()), JSON.stringify(own()), JSON.stringify([mk()]), other.JSON.stringify(own()));
		  delete other.Object.prototype.toJSON;
		  Object.prototype.toJSON = function () { return "own"; };
		  r.push(JSON.stringify(mk()), JSON.stringify(own()), other.JSON.stringify(mk()));
		  delete Object.prototype.toJSON;
		  r.push(JSON.stringify(mk()), JSON.stringify(own()));
		  r.slice(8).join(" ")`,
			`"other" {"p":1,"q":[2]} ["other"] {"p":1,"q":[2]} {"p":1,"q":[2]} "own" {"p":1,"q":[2]} {"p":1,"q":[2]} {"p":1,"q":[2]}`},
		// An array of the other realm, whose toJSON is on its Array.prototype.
		{`var mk = other.eval("(function () { return [1]; })");
		  for (var i = 0; i < 4; i++) JSON.stringify(mk());
		  other.Array.prototype.toJSON = function () { return "other"; };
		  JSON.stringify(mk()) + JSON.stringify([1])`, `"other"[1]`},
		// A document each realm's hints know, and what is then added to it.
		{`var doc = '[{"a":1,"b":2},{"a":3,"b":4}]';
		  JSON.parse(doc); other.JSON.parse(doc);
		  var x = JSON.parse(doc)[1], y = other.JSON.parse(doc)[1];
		  x.c = 1; y.d = 2;
		  [JSON.stringify(x), JSON.stringify(y), Object.getPrototypeOf(y) === other.Object.prototype].join(" ")`,
			`{"a":3,"b":4,"c":1} {"a":3,"b":4,"d":2} true`},
	})
}

// An object of many keys, or of long ones, keeps no record of its layout:
// what it writes is the same, and a record of each layout of a chain would
// hold the square of what the object does.
func TestJSONLongLayouts(t *testing.T) {
	jsonRepeated(t, []struct{ src, want string }{
		// Past the most keys, over a chain of layouts each written twice.
		{`var text = "", n = 0;
		  for (var i = 0; i < 150; i++) {
		    text += (i ? "," : "") + '"k' + i + '":' + i;
		    var o = JSON.parse("{" + text + "}");
		    n += JSON.stringify(o).length + JSON.stringify(o).length;
		  }
		  [n, JSON.stringify(o) === "{" + text + "}", JSON.stringify(Object.keys(o).slice(148))].join()`, `203430,true,["k148","k149"]`},
		// A key longer than the most text, on the second sighting and later,
		// with a toJSON found anew.
		{`var long = "k".repeat(5000), mk = function () { var o = {}; o[long] = 1; o.b = 2; return o; };
		  var s = JSON.stringify(mk()); JSON.stringify(mk()); JSON.stringify(mk());
		  Object.prototype.toJSON = function () { return "found"; };
		  var f = JSON.stringify(mk()); delete Object.prototype.toJSON;
		  [s.length, f, JSON.stringify(mk()) === s].join()`, `5012,"found",true`},
	})
}

// A long-lived string of a document holds no more than the document's size
// again in what was made together with it, however few of them there are.
func TestJSONBatchesAreBounded(t *testing.T) {
	const records = 100000
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(fmt.Sprintf(`var parts = [];
		for (var i = 0; i < %d; i++) parts.push('{"id":"i' + i + '","a":"a","b":"b","c":"c","d":"d","e":"e","f":"f","g":"g","h":"h","i":"i"}');
		var doc = "[" + parts.join(",") + "]"; parts = null;`, records)); err != nil {
		t.Fatal(err)
	}
	size, err := rt.Eval("doc.length")
	if err != nil {
		t.Fatal(err)
	}
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	before := heap()
	if _, err := rt.Eval(`var d = JSON.parse(doc); doc = null; var kept = d.map(function (r) { return r.id; }); d = null;`); err != nil {
		t.Fatal(err)
	}
	held := heap() - before
	// What is kept is the document, held by the slices, and an array of
	// strings and their headers; of what the batches hold, no more than the
	// document again. Before, it was all of a million headers, 48 MB.
	limit := int64(size.Int()) + records*(16+48) + 1<<20
	if held > limit {
		t.Errorf("a tenth of the strings holds %d bytes of a document of %d, more than %d", held, size.Int(), limit)
	}
}

// JSON.stringify stops for an interrupt, in an array of primitives as in
// objects with a record of their layout.
func TestJSONStringifyInterrupted(t *testing.T) {
	for _, src := range []string{
		`var a = new Array(5000000).fill(1); for (;;) JSON.stringify(a);`,
		`var o = {a: 1, b: 2}, a = new Array(2000000).fill(o); for (;;) JSON.stringify(a);`,
	} {
		rt := quickjs.New()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		_, err := rt.EvalContext(ctx, src)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v", src, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: stopped after %v", src, d)
		}
		rt.Close()
	}
}
