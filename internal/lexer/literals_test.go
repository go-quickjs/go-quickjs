package lexer

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// A short decimal skips strconv, so it must give strconv's answer, bit for
// bit, however its digits round.
func TestShortDecimalMatchesStrconv(t *testing.T) {
	check := func(s string) {
		t.Helper()
		want, err := strconv.ParseFloat(strings.TrimSuffix(s, "."), 64)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		tok, err := New(s).Next()
		if err != nil {
			t.Fatalf("lexing %q: %v", s, err)
		}
		if tok.Kind != Number || math.Float64bits(tok.Num) != math.Float64bits(want) {
			t.Fatalf("%q = %v (%x), want %v (%x)", s, tok.Num, math.Float64bits(tok.Num), want, math.Float64bits(want))
		}
	}
	for _, s := range []string{
		"0", "0.", ".0", "1", "1.", ".5", "0.1", "0.2", "0.3", "1.1", "2.675", "0.000001",
		"123456789012345", "999999999999999", "0.999999999999999", ".000000000000001",
		"12345678.1234567", "9007199254740992", "9007199254740993", "1234567890123456",
		"0.1234567890123456", "179769313486231570000000000000000", "4.35", "1.005", "8.41",
	} {
		check(s)
	}
	r := rand.New(rand.NewSource(1))
	for range 200000 {
		var b strings.Builder
		n := 1 + r.Intn(17)
		point := r.Intn(n + 1)
		for i := range n {
			if i == point {
				b.WriteByte('.')
			}
			b.WriteByte(byte('0' + r.Intn(10)))
		}
		s := b.String()
		if len(s) > 1 && s[0] == '0' && s[1] != '.' {
			s = "1" + s[1:] // a leading zero is a legacy literal
		}
		check(s)
	}
}

// Ahead is where the next token starts, or empty where a comment, or
// whitespace past ASCII, may come first.
func TestAhead(t *testing.T) {
	for _, tt := range []struct{ src, want string }{
		{"x => 1", "=> 1"},
		{"x\n\t  : y", ": y"},
		{"x\r\n\v\f= 1", "= 1"},
		{"x / y", "/ y"},
		{"x < y", "< y"},
		{"x - y", "- y"},
		{"x", ""},
		{"x   ", ""},
		{"x // c\n=> 1", ""},
		{"x /* c */ => 1", ""},
		{"x <!-- c\n: 1", ""},
		{"x\n--> c\n: 1", ""},
		{"x\u00a0: 1", ""},
		{"x\ufeff: 1", ""},
	} {
		l := New(tt.src)
		if _, err := l.Next(); err != nil {
			t.Fatalf("%q: %v", tt.src, err)
		}
		pos := l.Pos()
		if got := l.Ahead(); got != tt.want {
			t.Errorf("Ahead of %q = %q, want %q", tt.src, got, tt.want)
		}
		if l.Pos() != pos {
			t.Errorf("Ahead of %q moved the lexer", tt.src)
		}
	}
}

// A template part with nothing to cook is its own text; one with something
// to cook has that, wherever in the part it is, and a value built for one
// literal leaves nothing in the next.
func TestTemplateValues(t *testing.T) {
	for _, tt := range []struct{ src, value, raw string }{
		{"``", "", ""},
		{"`abc`", "abc", "abc"},
		{"`a$b`", "a$b", "a$b"},
		{"`a$b\\n$c`", "a$b\n$c", `a$b\n$c`},
		{"`é€😀`", "é€😀", "é€😀"},
		{"`a\nb`", "a\nb", "a\nb"},
		{"`a\r\nb\rc`", "a\nb\nc", "a\nb\nc"},
		{"`a\nb\\tc`", "a\nb\tc", "a\nb\\tc"},
		{"`\xffa`", "\ufffda", "\xffa"},
		{"`a\xff$\n`", "a\ufffd$\n", "a\xff$\n"},
		{"`\\x41\\u{1F600}x`", "A😀x", `\x41\u{1F600}x`},
	} {
		tok, err := New(tt.src).Next()
		if err != nil {
			t.Fatalf("%q: %v", tt.src, err)
		}
		if tok.Value != tt.value || tok.Raw != tt.raw {
			t.Errorf("%q: value %q raw %q, want %q and %q", tt.src, tok.Value, tok.Raw, tt.value, tt.raw)
		}
	}

	l := New("`\\tlong enough to be seen` 'ab\\n' `x` \"\\x41\" `y${1}\\tz`")
	var got []string
	for {
		tok, err := l.Next()
		if err != nil {
			t.Fatal(err)
		}
		if tok.Kind == EOF {
			break
		}
		if tok.Kind == TemplateHead {
			got = append(got, tok.Value)
			if tok, err = l.Next(); err != nil { // the 1
				t.Fatal(err)
			}
			if tok, err = l.Next(); err != nil || tok.Value != "}" {
				t.Fatal(tok, err)
			}
			if tok, err = l.ScanTemplateTail(tok); err != nil {
				t.Fatal(err)
			}
		}
		got = append(got, tok.Value)
	}
	want := []string{"\tlong enough to be seen", "ab\n", "x", "A", "y", "\tz"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("values %q, want %q", got, want)
	}
}

// A string with escapes costs one allocation of its own size, not a
// builder's doublings.
func TestEscapedStringAllocation(t *testing.T) {
	src := "'" + strings.Repeat(`\x41b`, 2000) + "'"
	var tok Token
	got := testing.AllocsPerRun(20, func() {
		l := Lexer{src: src, line: 1}
		l.cooked.b = make([]byte, 0, 2*len(src))
		if err := l.NextInto(&tok); err != nil {
			t.Fatal(err)
		}
	})
	if got > 2 { // the buffer here, and the value
		t.Errorf("%v allocations", got)
	}
	if len(tok.Value) != 4000 {
		t.Errorf("value of %d bytes", len(tok.Value))
	}
}
