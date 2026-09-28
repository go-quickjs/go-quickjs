package lexer

import "testing"

// TestStringLiteralValues pins a string literal's value whether it is sliced
// from the source, as one with no escape is, or built, as one with an escape
// is from the escape on: characters past ASCII, a raw line separator, a byte
// that is not UTF-8, and escapes first, last and in between.
func TestStringLiteralValues(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`"abc"`, "abc"},
		{`''`, ""},
		{`"é€😀"`, "é€😀"},
		{"'a b'", "a b"},
		{"\"a\xffb\"", "a\xffb"},
		{`"a'b"`, "a'b"},
		{`'a"b'`, `a"b`},
		{`"\x41bc"`, "Abc"},
		{`"abc"`, "abc"},
		{`"a\nb\tc"`, "a\nb\tc"},
		{`"ééé"`, "ééé"},
		{`"a\"b"`, `a"b`},
		{"\"a\\\nb\"", "ab"},
	} {
		tok, err := New(c.src).Next()
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if tok.Kind != String || tok.Value != c.want || tok.Raw != c.src {
			t.Errorf("%s = %v %q (raw %q), want %q", c.src, tok.Kind, tok.Value, tok.Raw, c.want)
		}
	}
	for _, src := range []string{`"abc`, "\"a\nb\"", `'a`} {
		if _, err := New(src).Next(); err == nil {
			t.Errorf("%s scanned", src)
		}
	}
}
