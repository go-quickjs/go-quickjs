package quickjs_test

import (
	"fmt"
	"math"
	"runtime"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLinearBuilds pins that building a string a surrogate half at a time,
// binding a function to itself over and over, joining a long list and
// breaking long Thai into words take work in proportion to how much there
// is: joining the halves of a pair flattened the string, each bind copied
// the "bound bound ..." name in full, a list was folded an item at a time,
// and each of Thai's words was searched for from the first (KI-27). The work
// measured is what each allocates, at a size and at twice it: twice as
// much, where a build copying all built so far at each step allocates four
// times as much. Not time, which other tests stretch: the race detector's
// full run took it past a deadline of 20 seconds, which it takes 3 alone.
// go-intl's searches of Thai's breaks, which allocate nothing either way,
// it pins itself (TestSegmentingScales); here the engine's segments, asked
// which segment holds each offset, are not made again for each.
//
// Not parallel: it reads the process's count of what was allocated, which
// a test running beside it would add to.
func TestLinearBuilds(t *testing.T) {
	for name, tc := range map[string]struct {
		n    int
		src  func(n int) string
		want func(n int) string
	}{
		"surrogate halves": {20000, func(n int) string {
			return fmt.Sprintf(`{ let s = "";
			for (let i = 0; i < %d; i++) { s += "\uD83D"; s += "\uDE00"; }
			[s.length, s === "\u{1F600}".repeat(%[1]d), s.codePointAt(%d).toString(16)].join() }`, n, 2*n-2)
		}, func(n int) string { return fmt.Sprintf("%d,true,1f600", 2*n) }},
		"bound names": {5000, func(n int) string {
			return fmt.Sprintf(`{ let f = function g() {};
			for (let i = 0; i < %d; i++) f = f.bind();
			[f.name.length, f.name.startsWith("bound bound "), f.name.endsWith("bound g")].join() }`, n)
		}, func(n int) string { return fmt.Sprintf("%d,true,true", 6*n+1) }},
		"a long list": {20000, func(n int) string {
			return fmt.Sprintf(`new Intl.ListFormat("en").format(Array(%d).fill("x")).length`, n)
		}, func(n int) string { return fmt.Sprint(3*n + 2) }},
		"long Thai": {2000, func(n int) string {
			return fmt.Sprintf(`{ const s = "สวัสดีครับ".repeat(%d);
			const segs = new Intl.Segmenter("th", {granularity: "word"}).segment(s);
			let n = 0;
			for (let i = 0; i < s.length; i += 5) n += segs.containing(i).segment.length > 0;
			[n, [...segs].length >= %[1]d].join() }`, n)
		}, func(n int) string { return fmt.Sprintf("%d,true", 2*n) }},
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			// The bytes allocated evaluating it at size n, the least of two
			// runs: anything else allocating meanwhile only adds.
			alloc := func(n int) uint64 {
				least := uint64(math.MaxUint64)
				for range 2 {
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					v, err := rt.Eval(tc.src(n))
					runtime.ReadMemStats(&after)
					if err != nil || v.String() != tc.want(n) {
						t.Fatalf("size %d: %v, %v; want %s", n, v, err, tc.want(n))
					}
					least = min(least, after.TotalAlloc-before.TotalAlloc)
				}
				return least
			}
			// What a first use makes -- Intl's data, the formatters -- made
			// before.
			alloc(tc.n / 16)
			one, two := alloc(tc.n), alloc(2*tc.n)
			if ratio := float64(two) / float64(one); ratio > 3 {
				t.Errorf("%d bytes allocated at size %d, %d at twice it: %.2f times as much", one, tc.n, two, ratio)
			}
		})
	}
}

// TestUnicodeLastIndexInAPair pins that a /u pattern's lastIndex between the
// halves of a surrogate pair reads the character it falls in, as the
// standard and V8 have it, rather than starting a match at the second half
// (KI-39).
func TestUnicodeLastIndexInAPair(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	checkEval(t, `const e = (f) => { try { return f() } catch (x) { return x.name + ": " + x.message } };
		[e(() => new Uint8Array(4).setFromHex("a\u00e9")), e(() => new Uint8Array(4).setFromHex("ab\u00e9")),
		 "x".padStart(5, "\uD83D").length, "x".padEnd(4, "\uDE00").length, "x".padStart(6, "\u{1F600}").length,
		 "\uDE00\uD83D".repeat(2) === "\uDE00" + "\uD83D\uDE00" + "\uD83D",
		 "x".padEnd(5, "\uDE00\uD83D") === "x\uDE00\uD83D\uDE00\uD83D"].join(" | ")`,
		"SyntaxError: Input string must contain hex characters in even length | "+
			"SyntaxError: Input string must contain hex characters in even length | 5 | 4 | 6 | true | true")
}

// TestLocaleFinalSigma pins that Turkish, Azerbaijani and Lithuanian
// lowercasing keeps the rule every language has for a capital sigma, which
// ends a word as U+03C2: those three lowercased one character at a time and
// always wrote U+03C3 (KI-46). The answers are node's.
func TestLocaleFinalSigma(t *testing.T) {
	t.Parallel()
	checkEval(t, `const cps = (s) => [...s].map(c => c.codePointAt(0).toString(16)).join(" ");
		["tr", "az", "lt", "en"].map(l => cps("ΟΔΟΣ".toLocaleLowerCase(l)) + "/" + cps("IİΣ".toLocaleLowerCase(l))).join(" | ")`,
		"3bf 3b4 3bf 3c2/131 69 3c2 | 3bf 3b4 3bf 3c2/131 69 3c2 | 3bf 3b4 3bf 3c2/69 69 307 3c2 | 3bf 3b4 3bf 3c2/69 69 307 3c2")
}
