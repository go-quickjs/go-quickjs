package quickjs_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

// showJS writes a value as the cases below expect it: a string quoted, its
// code units outside printable ASCII as \u escapes, lone surrogates
// included, and anything else as its type and String's text.
const showJS = `
function show(v) {
	if (typeof v !== "string")
		return typeof v + " " + String(v);
	var o = '"';
	for (var i = 0; i < v.length; i++) {
		var c = v.charCodeAt(i);
		o += c >= 0x20 && c < 0x7f ? v[i] : "\\u" + ("000" + c.toString(16)).slice(-4);
	}
	return o + '"';
}
function run(f) {
	var out = [];
	for (var i = 0; i < 3; i++) {
		try { out.push(show(f())) } catch (e) { out.push(e.constructor.name) }
	}
	return out[0] === out[1] && out[1] === out[2] ? out[0] : out.join(" | ");
}
`

// stringBuiltinCases are expressions over String's methods and what Node
// (v26.10.0) answers for each, as show writes it: the edges of what the
// methods' own code decides -- whitespace at either end, a lone surrogate
// where one is cut or joined, a rope's receiver, the conversions' order.
var stringBuiltinCases = []struct{ src, want string }{
	// trim, trimStart and trimEnd take off WhiteSpace and LineTerminator,
	// all of it and nothing else: not U+0085, U+180E or U+200B.
	{`" \t\n\v\f\r ab \u00a0\u1680\u2000\u2005\u200a\u2028\u2029\u202f\u205f\u3000\ufeff".trim()`, `"ab"`},
	{`"\u00a0\u1680\u2000\u2005\u200a\u2028\u2029\u202f\u205f\u3000\ufeff ab ".trimStart()`, `"ab "`},
	{`" ab \u00a0\u1680\u2000\u2005\u200a\u2028\u2029\u202f\u205f\u3000\ufeff".trimEnd()`, `" ab"`},
	{`"\u0085x\u0085".trim()`, `"\u0085x\u0085"`},
	{`"\u180ex\u180e".trim()`, `"\u180ex\u180e"`},
	{`"\u200bx\u200b".trim()`, `"\u200bx\u200b"`},
	{`"\u2000\u2001".trim()`, `""`},
	{`" \t ".trimEnd()`, `""`},
	{`"".trimStart()`, `""`},
	{`"abc".trim()`, `"abc"`},
	{`" \ud800 ".trim()`, `"\ud800"`},
	{`" \udc00x".trimStart()`, `"\udc00x"`},
	{`"x\ud83d\ude00\u3000".trimEnd()`, `"x\ud83d\ude00"`},
	{`"\u3000\ude00".trim() + "\ud83d"`, `"\ude00\ud83d"`},
	{`"\ud83d" + " \ude00 ".trim()`, `"\ud83d\ude00"`},
	{`("\ud83d" + " \ude00 ".trim()).length`, `number 2`},
	{`" \u00e9t\u00e9 ".trim()`, `"\u00e9t\u00e9"`},
	{`" \u00e9t\u00e9 ".trim().length`, `number 3`},
	{`"\u00a0abc\u00a0".trim() === "abc"`, `boolean true`},
	{`"\u00a0abc\u00a0".trim().charCodeAt(2)`, `number 99`},
	{`(" " + "x".repeat(70) + "\u3000").trim().length`, `number 70`},
	{`String.prototype.trim.call(12)`, `"12"`},
	{`String.prototype.trimEnd.call({ toString() { return " o " } })`, `" o"`},
	// What ToNumber, parseFloat, parseInt and BigInt take off is the same.
	{`Number(" \u00a012.5\ufeff\u2029")`, `number 12.5`},
	{`Number("\u180e1")`, `number NaN`},
	{`Number("\u0085 1")`, `number NaN`},
	{`Number("\u3000\u2028")`, `number 0`},
	{`parseFloat("\u3000\t3.5x")`, `number 3.5`},
	{`parseInt("\u2028\u202f42")`, `number 42`},
	{`String(BigInt(" 12 \u00a0"))`, `"12"`},
	{`String(BigInt("\ufeff"))`, `"0"`},
	// startsWith and endsWith compare code units, a pair's halves alone too.
	{`"\ud83d\ude00x".startsWith("\ud83d")`, `boolean true`},
	{`"\ud83d\ude00x".startsWith("\ude00x", 1)`, `boolean true`},
	{`"\ud83d\ude00x".startsWith("\ud83d\ude00", 1)`, `boolean false`},
	{`"\ud83d\ude00x".endsWith("\ud83d", 1)`, `boolean true`},
	{`"\u00e9\ud83d\ude00".endsWith("\ude00")`, `boolean true`},
	{`"\u00e9\ud83d\ude00".endsWith("\u00e9\ud83d", 2)`, `boolean true`},
	{`"\u00e9\ud83d\ude00".endsWith("\ud83d\ude00", 2)`, `boolean false`},
	{`"\u00e9t\u00e9".startsWith("t\u00e9", 1)`, `boolean true`},
	{`"\u00e9t\u00e9".startsWith("t", 1)`, `boolean true`},
	{`"\u00e9t\u00e9".startsWith("te", 1)`, `boolean false`},
	{`"\u00e9t\u00e9".endsWith("\u00e9t\u00e9")`, `boolean true`},
	{`"abc".startsWith("\u00e9")`, `boolean false`},
	{`"abc".endsWith("")`, `boolean true`},
	{`"abc".startsWith("", 9)`, `boolean true`},
	{`"abc".startsWith("c", 2.9)`, `boolean true`},
	{`"abc".endsWith("ab", -Infinity)`, `boolean false`},
	{`"abc".endsWith("ab", 2)`, `boolean true`},
	{`"abc".startsWith("b")`, `boolean false`},
	{`("\u00e9" + "x".repeat(200) + "ab").startsWith("ab", 201)`, `boolean true`},
	{`("\u00e9" + "x".repeat(200) + "ab").startsWith("ac", 201)`, `boolean false`},
	{`("\u00e9".repeat(100) + "\u00e9a").startsWith("\u00e9a", 100)`, `boolean true`},
	{`("\u00e9".repeat(100) + "\u00e9a").startsWith("\u00e9b", 100)`, `boolean false`},
	{`("\u00e9".repeat(100) + "\ud83d\ude00").endsWith("\ude00")`, `boolean true`},
	{`("\u00e9".repeat(100) + "\ud83d\ude00").endsWith("\u00e9\ud83d", 101)`, `boolean true`},
	{`("\u00e9".repeat(40) + "x".repeat(40)).startsWith("\u00e9x", 39)`, `boolean true`},
	{`("y".repeat(40) + "x".repeat(40)).endsWith("yx", 41)`, `boolean true`},
	{`"abc".startsWith(/a/)`, `TypeError`},
	{`"a,b".startsWith(["a", "b"])`, `boolean true`},
	// padStart and padEnd: the filler repeated and cut, a short result and
	// a long one, a lone surrogate at the end the filler is not at.
	{`"7".padStart(4, "0")`, `"0007"`},
	{`"abc".padStart(10, "123")`, `"1231231abc"`},
	{`"abc".padEnd(10, "123")`, `"abc1231231"`},
	{`"abc".padStart(6)`, `"   abc"`},
	{`"abc".padEnd(6, undefined)`, `"abc   "`},
	{`"abc".padEnd(2, "x")`, `"abc"`},
	{`"abc".padStart(5, "")`, `"abc"`},
	{`"abc".padStart(NaN, "x")`, `"abc"`},
	{`"abc".padStart(4.9, "xy")`, `"xabc"`},
	{`"".padEnd(3, "ab")`, `"aba"`},
	{`"\u00e9".padStart(3, "ab")`, `"ab\u00e9"`},
	{`"\u00e9".padEnd(3, "ab").length`, `number 3`},
	{`"\ud83d".padStart(2, "x") + "\ude00"`, `"x\ud83d\ude00"`},
	{`("\ud83d".padStart(2, "x") + "\ude00").length`, `number 3`},
	{`"\ud83d".padStart(2, "x") + "\ude00" === "x\ud83d\ude00"`, `boolean true`},
	{`"\ud83d" + "\ude00".padEnd(2, "x") === "\ud83d\ude00x"`, `boolean true`},
	{`"\ud83d".padEnd(3, "x") + "\ude00"`, `"\ud83dxx\ude00"`},
	{`"x".repeat(70).padStart(75, "ab").slice(0, 7)`, `"ababaxx"`},
	{`"x".repeat(70).padEnd(75, "ab").slice(-7)`, `"xxababa"`},
	{`"\u00e9".repeat(70).padEnd(72, "-").endsWith("\u00e9--")`, `boolean true`},
	{`"\ud83d".repeat(40).padStart(80, "-") + "\ude00" === "-".repeat(40) + "\ud83d".repeat(39) + "\ud83d\ude00"`, `boolean true`},
	{`"a".padStart(200, "xyz").slice(-5)`, `"xyzxa"`},
	{`"a".padStart(200, "xyz").length`, `number 200`},
	{`"a".padStart(4, "\u00e9")`, `"\u00e9\u00e9\u00e9a"`},
	{`"a".padStart(4, "\ud83d\ude00")`, `"\ud83d\ude00\ud83da"`},
	{`"a".padEnd(4, "\ude00\ud83d")`, `"a\ude00\ud83d\ude00"`},
	{`"a".padStart(2 ** 30)`, `RangeError`},
	{`"a".padStart(2 ** 30, "")`, `"a"`},
	{`"".padStart(Infinity, "")`, `""`},
	{`(function () { var o = []; var r = "a".padStart({ valueOf() { o.push(1); return 3 } }, { toString() { o.push(2); return "x" } }); return r + o })()`, `"xxa1,2"`},
	// repeat: every copy's text, the ends of the result s's.
	{`"ab".repeat(3)`, `"ababab"`},
	{`"ab".repeat(1)`, `"ab"`},
	{`"ab".repeat(0)`, `""`},
	{`"".repeat(5)`, `""`},
	{`"ab".repeat(2.9)`, `"abab"`},
	{`"ab".repeat(-1)`, `RangeError`},
	{`"".repeat(Infinity)`, `RangeError`},
	{`"a".repeat(2 ** 29)`, `RangeError`},
	{`"\u00e9".repeat(3)`, `"\u00e9\u00e9\u00e9"`},
	{`"\u00e9".repeat(3).length`, `number 3`},
	{`"\ud83d".repeat(2)`, `"\ud83d\ud83d"`},
	{`"\ud83d".repeat(2) + "\ude00" === "\ud83d\ud83d\ude00"`, `boolean true`},
	{`"\ud83d" + "\ude00".repeat(2) === "\ud83d\ude00\ude00"`, `boolean true`},
	{`"\ude00\ud83d".repeat(3) === "\ude00\ud83d\ude00\ud83d\ude00\ud83d"`, `boolean true`},
	{`"\ude00\ud83d".repeat(3).codePointAt(1)`, `number 128512`},
	{`"abc".repeat(100).length`, `number 300`},
	{`"abc".repeat(100).slice(-4)`, `"cabc"`},
	{`"\u00e9\ud83d\ude00".repeat(40).length`, `number 120`},
	{`"\u00e9\ud83d\ude00".repeat(40).lastIndexOf("\u00e9")`, `number 117`},
	{`("x".repeat(40) + "y".repeat(40)).repeat(2).indexOf("yx")`, `number 79`},
	// The case conversions of ASCII.
	{`"Hello, World".toUpperCase()`, `"HELLO, WORLD"`},
	{`"Hello, World".toLowerCase()`, `"hello, world"`},
	{`"abc123".toLowerCase()`, `"abc123"`},
	{`"ab".repeat(60).toUpperCase().slice(-3)`, `"BAB"`},
	{`"ab".repeat(60).toUpperCase().length`, `number 120`},
}

// TestStringBuiltinsMatchNode pins String's methods to Node's answers,
// each called three times, which the tree tier takes over by.
func TestStringBuiltinsMatchNode(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	evalString(t, rt, showJS)
	for _, c := range stringBuiltinCases {
		if got := evalString(t, rt, "run(() => "+c.src+")"); got != c.want {
			t.Errorf("%s\n got: %s\nwant: %s", c.src, got, c.want)
		}
	}
}
