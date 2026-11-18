package quickjs_test

import "testing"

// TestUnicode17 pins what a script sees of the Unicode version, 17.0 whatever
// Go builds the engine: Go 1.24's own tables are at 15.0, and the lexer, the
// regexp and the case conversions had answered at the toolchain's version.
func TestUnicode17(t *testing.T) {
	t.Parallel()
	evalCases(t, []struct{ src, want string }{
		// Identifiers of 15.1 (CJK Extension I, the katakana middle dot as a
		// part), 16.0 (Garay, Cyrillic TJE) and 17.0 (Beria Erfe).
		{"var \U0002EBF0 = 1, a\u30FB = 2, \U00010D50 = 3, \u1C89 = 4, \U00016EA0 = 5;" +
			" \U0002EBF0 + a\u30FB + \U00010D50 + \u1C89 + \U00016EA0", "15"},
		{`var \u{2EBF0}\u30FB = 6; \u{2EBF0}\u30FB`, "6"},
		// A letter that is Pattern_Syntax is not one.
		{`try { eval("var \u2E2F") } catch (e) { e.name }`, "SyntaxError"},
		// Case pairs of 16.0 and 17.0, both ways.
		{`["\u1C89".toLowerCase() === "\u1C8A", "\u1C8A".toUpperCase() === "\u1C89",
		   "\u{10D50}".toLowerCase() === "\u{10D70}", "\u{10D70}".toUpperCase() === "\u{10D50}",
		   "\uA7CB".toLowerCase() === "\u0264", "\u0264".toUpperCase() === "\uA7CB",
		   "\u{16EA0}".toLowerCase() === "\u{16EBB}",
		   "\u1C89".toLocaleLowerCase("tr") === "\u1C8A"].join()`,
			"true,true,true,true,true,true,true,true"},
		// The final sigma's rule reads Cased and Case_Ignorable at 17.0: TJE
		// is cased since 16.0, so the sigma after it ends a word, and a mark
		// of 17.0 between them is passed over.
		{`["\u1C89\u03A3".toLowerCase() === "\u1C8A\u03C2",
		   "A\u{11B60}\u03A3".toLowerCase() === "a\u{11B60}\u03C2"].join()`, "true,true"},
		// Folding under /iu: Unicode 15.1 folds U+1FD3 to U+0390 and U+FB05 to
		// U+FB06; without u the comparison is of upper cases, none here.
		{`[/\u0390/iu.test("\u1FD3"), /\u1FD3/iu.test("\u0390"), /\uFB05/iu.test("\uFB06"),
		   /\u0390/i.test("\u1FD3"), /\u1C89/i.test("\u1C8A"), /\u{16EA0}/iu.test("\u{16EBB}")].join()`,
			"true,true,true,false,true,true"},
		// A group name is an identifier name: a letter number begins one.
		{`/(?<\u2160>a)/.exec("a").groups["\u2160"]`, "a"},
	})
}
