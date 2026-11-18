package vm

import "testing"

// TestNativeFunctionNameIsIdentifier writes a built-in's name in
// Function.prototype.toString as it is where the grammar takes it for an
// identifier -- ID_Start and ID_Continue at Unicode 17.0, not Go's letters
// at the toolchain's version -- and as a computed string where it does not.
func TestNativeFunctionNameIsIdentifier(t *testing.T) {
	for _, c := range []struct{ name, want string }{
		{"\u2160", "\u2160"},           // ROMAN NUMERAL ONE, Nl
		{"\u1885x", "\u1885x"},         // an Other_ID_Start
		{"\U00016EA0", "\U00016EA0"},   // BERIA ERFE CAPITAL LETTER ARKAB, 17.0
		{"a\U00011B60", "a\U00011B60"}, // SHARADA VOWEL SIGN OE, Mn, 17.0
		{"get \u1C89", "get \u1C89"},   // CYRILLIC CAPITAL LETTER TJE, 16.0
		{"\u2E2F", "[\"\u2E2F\"]"},     // VERTICAL TILDE, Pattern_Syntax
		{"a b", "[\"a b\"]"},
	} {
		if got := nativeFunctionName(c.name); got != c.want {
			t.Errorf("nativeFunctionName(%+q) = %+q, want %+q", c.name, got, c.want)
		}
	}
}

// TestCaseConvertAtUnicode17 converts the case pairs Unicode 16.0 and 17.0
// added, the full mappings, and the final sigma after a character that is
// Cased or Case_Ignorable only since 16.0 or 17.0.
func TestCaseConvertAtUnicode17(t *testing.T) {
	for _, c := range []struct {
		in    string
		upper bool
		want  string
	}{
		{"\u1C89", false, "\u1C8A"},
		{"\u1C8A", true, "\u1C89"},
		{"\U00010D50", false, "\U00010D70"},
		{"\U00010D70", true, "\U00010D50"},
		{"\U00016EA0", false, "\U00016EBB"},
		{"\u00DF\u0149", true, "SS\u02BCN"},
		{"\u0130", false, "i\u0307"},
		{"\u1C89\u03A3", false, "\u1C8A\u03C2"},
		{"A\U00011B60\u03A3", false, "a\U00011B60\u03C2"},
		{"A\U00011B60\u03A3b", false, "a\U00011B60\u03C3b"},
	} {
		if got := caseConvert(c.in, c.upper); got != c.want {
			t.Errorf("caseConvert(%+q, %v) = %+q, want %+q", c.in, c.upper, got, c.want)
		}
	}
}
