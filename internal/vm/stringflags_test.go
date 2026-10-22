package vm

import "testing"

// TestBuiltStringsKnowThemselves pins that the strings String's methods
// write themselves -- trimmed, padded, repeated, joined, converted, made of
// code units -- carry what NewString would find in their text: the length
// in code units, whether every one is ASCII, and the unpaired surrogates at
// either end, which indexing, comparison and the next join rely on but no
// script can see directly. Each is made three times, as the interpreter and
// the tree tier make it.
func TestBuiltStringsKnowThemselves(t *testing.T) {
	exprs := []string{
		`" abc ".trim()`, `"\u00a0abc\u00a0".trim()`, `"\u3000\u00e9\u3000".trimStart()`,
		`" \ud83d\u3000".trimEnd()`, `"\u2028\ude00x ".trim()`, `" \ud83d\ude00\u00e9\u3000".trim()`,
		`"\u00e9".padStart(3, "ab")`, `"\ud83d".padStart(3, "ab")`, `"\ude00".padEnd(3, "ab")`,
		`"x".padEnd(90, "ab")`, `"\u00e9".repeat(70).padEnd(72, "-")`, `"a".padStart(4, "\u00e9")`,
		`"ab".repeat(3)`, `"\ud83d".repeat(2)`, `"\ude00x".repeat(2)`, `"\ude00\ud83d".repeat(3)`,
		`"\u00e9\ud83d\ude00".repeat(40)`, `"Hello".toUpperCase()`, `"ab".repeat(60).toLowerCase()`,
		`String.fromCharCode(65, 66)`, `String.fromCharCode(0xD83D, 0xDE00)`, `String.fromCharCode(0xDE00, 0x41, 0xD83D)`,
		`String.fromCharCode(0xE9)`, `String.fromCharCode(0xE9, 0x100)`, `String.fromCodePoint(0x1F600, 0xD83D)`,
		`String.fromCodePoint(0xDE00, 0x41)`, `String.fromCodePoint(65)`, `String.fromCodePoint(...Array(50).fill(0x1F600))`,
		`"a".concat("b", 1)`, `"\u00e9".concat("x")`, `"x".concat("\ud83d", "")`, `"".concat("", "\ude00", "y")`,
		`"\ud83d".concat("\ude00")`, `"x".repeat(40).concat("y".repeat(40))`, `"\u00e9\ud800".toWellFormed()`,
		`"abc".toWellFormed()`,
	}
	r := New(Config{})
	for _, e := range exprs {
		if _, err := r.Run(compileForTest(t, "var f = () => "+e)); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
		for i := 0; i < 3; i++ {
			got, err := r.Run(compileForTest(t, "f()"))
			if err != nil || !got.IsString() {
				t.Fatalf("%s: %v, %v", e, got, err)
			}
			s := got.String()
			want := NewString(s.Go())
			if s.length != want.length || s.ascii != want.ascii || s.endsHigh != want.endsHigh || s.startsLow != want.startsLow {
				t.Errorf("%s: length %d ascii %v endsHigh %v startsLow %v; its text's are %d %v %v %v", e,
					s.length, s.ascii, s.endsHigh, s.startsLow, want.length, want.ascii, want.endsHigh, want.startsLow)
			}
		}
	}
}
