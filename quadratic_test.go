package quickjs_test

import (
	"context"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLinearBuilds pins that building a string a surrogate half at a time,
// binding a function to itself over and over, joining a long list and
// breaking long Thai into words take time in proportion to how much there is:
// joining the halves of a pair flattened the string, each bind copied the
// "bound bound ..." name in full, a list was folded an item at a time, and
// each of Thai's words was searched for from the first (KI-27).
func TestLinearBuilds(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"surrogate halves": {`
			let s = "";
			for (let i = 0; i < 200000; i++) { s += "\uD83D"; s += "\uDE00"; }
			[s.length, s === "\u{1F600}".repeat(200000), s.codePointAt(399998).toString(16)].join()`,
			"400000,true,1f600"},
		"bound names": {`
			let f = function g() {};
			for (let i = 0; i < 100000; i++) f = f.bind();
			[f.name.length, f.name.startsWith("bound bound "), f.name.endsWith("bound g")].join()`,
			"600001,true,true"},
		"a long list": {`
			new Intl.ListFormat("en").format(Array(200000).fill("x")).length`,
			"600002"},
		"long Thai": {`
			const s = "สวัสดีครับ".repeat(30000);
			const segs = new Intl.Segmenter("th", {granularity: "word"}).segment(s);
			let n = 0;
			for (let i = 0; i < s.length; i += 5) n += segs.containing(i).segment.length > 0;
			[n, [...segs].length > 30000].join()`,
			"60000,true"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v, err := rt.EvalContext(ctx, tc.src)
			if err != nil || v.String() != tc.want {
				t.Errorf("= %v, %v; want %s", v, err, tc.want)
			}
		})
	}
}

// TestUnicodeLastIndexInAPair pins that a /u pattern's lastIndex between the
// halves of a surrogate pair reads the character it falls in, as the
// standard and V8 have it, rather than starting a match at the second half
// (KI-39).
func TestUnicodeLastIndexInAPair(t *testing.T) {
	checkEval(t, `const out = [];
		{ const r = /\udc00/uy; r.lastIndex = 1; out.push(JSON.stringify(r.exec("\ud800\udc00"))); }
		{ const r = /\udc00/ug; r.lastIndex = 1; out.push(JSON.stringify(r.exec("\ud800\udc00")) + " " + r.lastIndex); }
		{ const r = /./uy; r.lastIndex = 1; const m = r.exec("\ud800\udc00"); out.push(m.index + " " + m[0].length + " " + r.lastIndex); }
		{ const r = /./vg; r.lastIndex = 1; const m = r.exec("\ud800\udc00x"); out.push(m.index + " " + m[0].length + " " + r.lastIndex); }
		{ const r = /\udc00/y; r.lastIndex = 1; out.push(JSON.stringify(r.exec("\ud800\udc00"))); }
		out.join(" | ")`, `null | null 0 | 0 2 2 | 0 2 2 | ["\udc00"]`)
}

// TestCodeUnitCounts pins lengths counted as a string's are, in UTF-16 code
// units: setFromHex counted UTF-8 bytes, so "a\u00e9" was odd and "ab\u00e9"
// even; padStart and padEnd counted the filler in runes, so a lone surrogate
// made the result short; and repeat, like padding, joins the halves of a pair
// that meet into the one spelling a character has (KI-44). The answers are
// node's.
func TestCodeUnitCounts(t *testing.T) {
	checkEval(t, `const e = (f) => { try { return f() } catch (x) { return x.name + ": " + x.message } };
		[e(() => new Uint8Array(4).setFromHex("a\u00e9")), e(() => new Uint8Array(4).setFromHex("ab\u00e9")),
		 "x".padStart(5, "\uD83D").length, "x".padEnd(4, "\uDE00").length, "x".padStart(6, "\u{1F600}").length,
		 "\uDE00\uD83D".repeat(2) === "\uDE00" + "\uD83D\uDE00" + "\uD83D",
		 "x".padEnd(5, "\uDE00\uD83D") === "x\uDE00\uD83D\uDE00\uD83D"].join(" | ")`,
		"SyntaxError: Input string must contain hex characters in even length | "+
			"SyntaxError: Input string must contain hex characters in even length | 5 | 4 | 6 | true | true")
}
