package lexer

import (
	"math"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// scanAll tokenizes src with Next only, which treats '/' as division. Tests
// that need regexp or template continuation drive the lexer directly.
func scanAll(t *testing.T, src string) []Token {
	t.Helper()
	l := New(src)
	var out []Token
	for {
		tok, err := l.Next()
		if err != nil {
			t.Fatalf("scanning %q: %v", src, err)
		}
		if tok.Kind == EOF {
			return out
		}
		out = append(out, tok)
	}
}

func TestPunctuatorsAreLongestMatch(t *testing.T) {
	// The scanner must prefer the longest operator, or `a >>>= b` would lex as
	// `>>` `>=`.
	toks := scanAll(t, "a >>>= b ?? c ??= d ** e **= f")
	want := []string{"a", ">>>=", "b", "??", "c", "??=", "d", "**", "e", "**=", "f"}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].Value != w {
			t.Errorf("token %d = %q, want %q", i, toks[i].Value, w)
		}
	}
}

func TestOptionalChainingVersusTernary(t *testing.T) {
	// `a?.5:b` is a conditional expression, not optional chaining, because the
	// character after ?. is a digit.
	toks := scanAll(t, "a?.5:b")
	if toks[1].Value != "?" {
		t.Errorf("expected ternary '?', got %q", toks[1].Value)
	}
	toks = scanAll(t, "a?.b")
	if toks[1].Value != "?." {
		t.Errorf("expected optional chain '?.', got %q", toks[1].Value)
	}
}

func TestNumericLiterals(t *testing.T) {
	tests := []struct {
		src  string
		want float64
	}{
		{"0", 0},
		{"42", 42},
		{"3.14", 3.14},
		{".5", 0.5},
		{"1.", 1},
		{"1e3", 1000},
		{"1E-3", 0.001},
		{"1_000_000", 1000000},
		{"0x1f", 31},
		{"0XFF", 255},
		{"0o17", 15},
		{"0b1011", 11},
		{"0755", 493}, // legacy octal
		{"0888", 888}, // non-octal decimal
		{"0.1e2", 10},
		{"0xdead_beef", 3735928559},
	}
	for _, tt := range tests {
		toks := scanAll(t, tt.src)
		if len(toks) != 1 {
			t.Errorf("%q: got %d tokens, want 1", tt.src, len(toks))
			continue
		}
		if toks[0].Kind != Number {
			t.Errorf("%q: kind = %v, want Number", tt.src, toks[0].Kind)
			continue
		}
		if toks[0].Num != tt.want {
			t.Errorf("%q: value = %v, want %v", tt.src, toks[0].Num, tt.want)
		}
	}
}

func TestBigIntLiteral(t *testing.T) {
	toks := scanAll(t, "123n 0xffn")
	if len(toks) != 2 {
		t.Fatalf("got %d tokens, want 2", len(toks))
	}
	for i, want := range []string{"123", "0xff"} {
		if toks[i].Kind != BigInt {
			t.Errorf("token %d: kind = %v, want BigInt", i, toks[i].Kind)
		}
		if toks[i].Value != want {
			t.Errorf("token %d: value = %q, want %q", i, toks[i].Value, want)
		}
	}
}

func TestNumberFollowedByIdentifierIsAnError(t *testing.T) {
	// `3in[]` must not lex as 3 followed by `in`.
	l := New("3in")
	if _, err := l.Next(); err == nil {
		t.Error("expected an error for a number immediately followed by an identifier")
	}
}

func TestStringEscapes(t *testing.T) {
	tests := []struct{ src, want string }{
		{`"abc"`, "abc"},
		{`'a\nb'`, "a\nb"},
		{`"\t\r\b\f\v"`, "\t\r\b\f\v"},
		{`"\x41"`, "A"},
		{"\"\\u0041\"", "A"},
		{`"\u{1F600}"`, "\U0001F600"},
		{"\"\\uD83D\\uDE00\"", "\U0001F600"}, // surrogate pair is combined
		{`"\0"`, "\x00"},
		{`"\101"`, "A"},             // legacy octal escape
		{`"a\` + "\n" + `b"`, "ab"}, // line continuation
		{`"\q"`, "q"},               // unknown escapes drop the backslash
	}
	for _, tt := range tests {
		toks := scanAll(t, tt.src)
		if len(toks) != 1 || toks[0].Kind != String {
			t.Errorf("%s: expected a single string token, got %v", tt.src, toks)
			continue
		}
		if toks[0].Value != tt.want {
			t.Errorf("%s: value = %q, want %q", tt.src, toks[0].Value, tt.want)
		}
	}
}

func TestUnterminatedStringIsAnError(t *testing.T) {
	for _, src := range []string{`"abc`, "'abc\nd'", `"abc`} {
		l := New(src)
		if _, err := l.Next(); err == nil {
			t.Errorf("%q: expected an unterminated string error", src)
		}
	}
}

func TestKeywordsVersusIdentifiers(t *testing.T) {
	toks := scanAll(t, "if let async of x")
	if toks[0].Kind != Keyword {
		t.Errorf("`if` should lex as a keyword, got %v", toks[0].Kind)
	}
	// Contextual keywords lex as identifiers; the parser gives them meaning.
	for i, name := range []string{"let", "async", "of", "x"} {
		if toks[i+1].Kind != Ident {
			t.Errorf("`%s` should lex as an identifier, got %v", name, toks[i+1].Kind)
		}
	}
}

func TestEscapedKeywordIsAnIdentifier(t *testing.T) {
	// \u0069f spells "if" but is an identifier, not the keyword.
	toks := scanAll(t, "\\u0069f")
	if len(toks) != 1 {
		t.Fatalf("got %d tokens, want 1", len(toks))
	}
	if toks[0].Kind != Ident || toks[0].Value != "if" {
		t.Errorf("got %v %q, want identifier \"if\"", toks[0].Kind, toks[0].Value)
	}
}

func TestPrivateIdentifier(t *testing.T) {
	toks := scanAll(t, "this.#count")
	if toks[2].Kind != PrivateIdent || toks[2].Value != "count" {
		t.Errorf("got %v %q, want private identifier \"count\"", toks[2].Kind, toks[2].Value)
	}
}

func TestNewlineBeforeDrivesASI(t *testing.T) {
	toks := scanAll(t, "a\nb c")
	if toks[0].NewlineBefore {
		t.Error("first token should not report a preceding newline")
	}
	if !toks[1].NewlineBefore {
		t.Error("`b` follows a newline and should report it")
	}
	if toks[2].NewlineBefore {
		t.Error("`c` does not follow a newline")
	}
}

func TestCommentsAreSkippedAndCountLines(t *testing.T) {
	toks := scanAll(t, "a // trailing\n/* multi\n   line */ b")
	if len(toks) != 2 {
		t.Fatalf("got %d tokens, want 2", len(toks))
	}
	if toks[1].Line != 3 {
		t.Errorf("`b` is on line %d, want 3", toks[1].Line)
	}
	if !toks[1].NewlineBefore {
		t.Error("a newline inside a block comment counts for ASI")
	}
}

func TestUnterminatedCommentIsAnError(t *testing.T) {
	l := New("/* never closed")
	if _, err := l.Next(); err == nil {
		t.Error("expected an unterminated comment error")
	}
}

func TestRegexpLiteral(t *testing.T) {
	l := New("/ab+c/gi")
	tok, err := l.Next() // lexes as the '/' punctuator
	if err != nil {
		t.Fatal(err)
	}
	re, err := l.ScanRegExp(tok)
	if err != nil {
		t.Fatal(err)
	}
	if re.Kind != Regexp {
		t.Fatalf("kind = %v, want Regexp", re.Kind)
	}
	if re.Value != "ab+c" || re.Flags != "gi" {
		t.Errorf("got /%s/%s, want /ab+c/gi", re.Value, re.Flags)
	}
}

func TestRegexpSlashInCharacterClass(t *testing.T) {
	// The '/' inside [...] is literal and must not end the literal.
	l := New("/[/]/")
	tok, _ := l.Next()
	re, err := l.ScanRegExp(tok)
	if err != nil {
		t.Fatal(err)
	}
	if re.Value != "[/]" {
		t.Errorf("body = %q, want %q", re.Value, "[/]")
	}
}

func TestRegexpEscapedSlash(t *testing.T) {
	l := New(`/a\/b/`)
	tok, _ := l.Next()
	re, err := l.ScanRegExp(tok)
	if err != nil {
		t.Fatal(err)
	}
	if re.Value != `a\/b` {
		t.Errorf("body = %q, want %q", re.Value, `a\/b`)
	}
}

func TestTemplateLiteral(t *testing.T) {
	l := New("`a${x}b${y}c`")
	head, err := l.Next()
	if err != nil {
		t.Fatal(err)
	}
	// A head is distinct from a complete template: it promises a substitution.
	if head.Kind != TemplateHead || head.Value != "a" {
		t.Fatalf("head = %v %q, want TemplateHead \"a\"", head.Kind, head.Value)
	}
	// x
	if tok, _ := l.Next(); tok.Value != "x" {
		t.Errorf("expected `x`, got %q", tok.Value)
	}
	brace, _ := l.Next() // '}'
	mid, err := l.ScanTemplateTail(brace)
	if err != nil {
		t.Fatal(err)
	}
	if mid.Kind != TemplateMid || mid.Value != "b" {
		t.Errorf("mid = %v %q, want TemplateMid \"b\"", mid.Kind, mid.Value)
	}
	if tok, _ := l.Next(); tok.Value != "y" {
		t.Errorf("expected `y`, got %q", tok.Value)
	}
	brace, _ = l.Next()
	tail, err := l.ScanTemplateTail(brace)
	if err != nil {
		t.Fatal(err)
	}
	if tail.Kind != TemplateTail || tail.Value != "c" {
		t.Errorf("tail = %v %q, want TemplateTail \"c\"", tail.Kind, tail.Value)
	}
}

func TestNoSubstitutionTemplateIsNotAHead(t *testing.T) {
	// A template with no substitutions must not be reported as a head, or the
	// parser would go looking for an expression that is not there.
	l := New("`abc`")
	tok, err := l.Next()
	if err != nil {
		t.Fatal(err)
	}
	if tok.Kind != Template {
		t.Errorf("kind = %v, want Template", tok.Kind)
	}
	if tok.Value != "abc" {
		t.Errorf("value = %q, want \"abc\"", tok.Value)
	}
}

func TestTemplateRawNormalizesLineEndings(t *testing.T) {
	l := New("`a\r\nb`")
	tok, err := l.Next()
	if err != nil {
		t.Fatal(err)
	}
	if tok.Raw != "a\nb" {
		t.Errorf("raw = %q, want %q", tok.Raw, "a\nb")
	}
	if tok.Value != "a\nb" {
		t.Errorf("cooked = %q, want %q", tok.Value, "a\nb")
	}
}

func TestUnicodeIdentifiers(t *testing.T) {
	toks := scanAll(t, "变量 café $_ x1")
	want := []string{"变量", "café", "$_", "x1"}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d", len(toks), len(want))
	}
	for i, w := range want {
		if toks[i].Kind != Ident || toks[i].Value != w {
			t.Errorf("token %d = %v %q, want identifier %q", i, toks[i].Kind, toks[i].Value, w)
		}
	}
}

func TestSeekRestoresPosition(t *testing.T) {
	// The parser relies on Seek to re-scan after lookahead.
	l := New("a b c")
	first, _ := l.Next()
	second, _ := l.Next()
	l.Seek(second.Pos, second.Line, second.LineStart)
	again, _ := l.Next()
	if again.Value != second.Value {
		t.Errorf("after seek got %q, want %q", again.Value, second.Value)
	}
	_ = first
}

func TestBOMIsSkipped(t *testing.T) {
	toks := scanAll(t, "\uFEFFa")
	if len(toks) != 1 || toks[0].Value != "a" {
		t.Errorf("got %v, want a single identifier `a`", toks)
	}
}

func TestLineAndColumnTracking(t *testing.T) {
	src := "a;\n  bc;\n"
	l := New(src)
	var toks []Token
	for {
		tok, err := l.Next()
		if err != nil {
			t.Fatal(err)
		}
		if tok.Kind == EOF {
			break
		}
		toks = append(toks, tok)
	}
	if col := l.Column(toks[0].Pos, toks[0].LineStart); toks[0].Line != 1 || col != 1 {
		t.Errorf("`a` at %d:%d, want 1:1", toks[0].Line, col)
	}
	// `bc` is the third token (a ; bc ;) and starts at column 3 of line 2.
	if col := l.Column(toks[2].Pos, toks[2].LineStart); toks[2].Line != 2 || col != 3 {
		t.Errorf("`bc` at %d:%d, want 2:3", toks[2].Line, col)
	}
}

func TestColumnCountsRunesNotBytes(t *testing.T) {
	// A column is a rune offset, so multi-byte characters count once.
	src := "变量 x"
	l := New(src)
	l.Next() // 变量
	tok, err := l.Next()
	if err != nil {
		t.Fatal(err)
	}
	if col := l.Column(tok.Pos, tok.LineStart); col != 4 {
		t.Errorf("`x` at column %d, want 4", col)
	}
}

func TestNaNLiteralIsNotProducedByLexer(t *testing.T) {
	// NaN is an identifier, not a literal; only the runtime knows its value.
	toks := scanAll(t, "NaN")
	if toks[0].Kind != Ident {
		t.Errorf("NaN should lex as an identifier, got %v", toks[0].Kind)
	}
	if math.IsNaN(toks[0].Num) {
		t.Error("identifier tokens should not carry a numeric value")
	}
}

func BenchmarkScanExpression(b *testing.B) {
	src := `function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }`
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		l := New(src)
		for {
			tok, err := l.Next()
			if err != nil || tok.Kind == EOF {
				break
			}
		}
	}
}

func BenchmarkScanStringHeavy(b *testing.B) {
	src := `var s = "hello world" + 'another string' + "with \n escapes A";`
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		l := New(src)
		for {
			tok, err := l.Next()
			if err != nil || tok.Kind == EOF {
				break
			}
		}
	}
}

// TestLoneSurrogateEscapes checks that a \u escape denoting an unpaired
// surrogate survives lexing.
//
// This is easy to get wrong in Go: strings.Builder.WriteRune substitutes
// U+FFFD for a surrogate, so a naive lexer silently destroys "\uD83D".
func TestLoneSurrogateEscapes(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		units []uint16
	}{
		{"lone high", `"\uD83D"`, []uint16{0xD83D}},
		{"lone low", `"\uDE00"`, []uint16{0xDE00}},
		{"high then ascii", `"\uD83Dx"`, []uint16{0xD83D, 'x'}},
		{"ascii then low", `"x\uDE00"`, []uint16{'x', 0xDE00}},
		{"reversed pair", `"\uDE00\uD83D"`, []uint16{0xDE00, 0xD83D}},
		{"two highs", `"\uD83D\uD83D"`, []uint16{0xD83D, 0xD83D}},
		{"boundary low", `"\uDFFF"`, []uint16{0xDFFF}},
		{"boundary high", `"\uD800"`, []uint16{0xD800}},
		// A well-formed pair must be combined into one code point instead.
		{"valid pair", "\"\\uD83D\\uDE00\"", []uint16{0xD83D, 0xDE00}},
	}
	for _, tt := range tests {
		toks := scanAll(t, tt.src)
		if len(toks) != 1 || toks[0].Kind != String {
			t.Errorf("%s: expected one string token, got %v", tt.name, toks)
			continue
		}
		got := wtf8.ToUTF16(toks[0].Value)
		if len(got) != len(tt.units) {
			t.Errorf("%s: got %d code units (%#x), want %d (%#x)",
				tt.name, len(got), got, len(tt.units), tt.units)
			continue
		}
		for i := range tt.units {
			if got[i] != tt.units[i] {
				t.Errorf("%s: unit %d = %#x, want %#x", tt.name, i, got[i], tt.units[i])
			}
		}
	}
}

func TestValidSurrogatePairBecomesOneCodePoint(t *testing.T) {
	// Two escapes forming a valid pair must combine, producing ordinary UTF-8
	// rather than two encoded halves.
	toks := scanAll(t, "\"\\uD83D\\uDE00\"")
	if toks[0].Value != "\U0001F600" {
		t.Errorf("value = % x, want the emoji encoded as UTF-8", toks[0].Value)
	}
	if len(toks[0].Value) != 4 {
		t.Errorf("value is %d bytes, want 4", len(toks[0].Value))
	}
}

func TestBracedSurrogateEscape(t *testing.T) {
	// The \u{...} form can also name a surrogate.
	toks := scanAll(t, `"\u{D83D}"`)
	got := wtf8.ToUTF16(toks[0].Value)
	if len(got) != 1 || got[0] != 0xD83D {
		t.Errorf("got %#x, want a single 0xD83D", got)
	}
}

func TestLoneSurrogateInTemplate(t *testing.T) {
	l := New("`\\uD83D`")
	tok, err := l.Next()
	if err != nil {
		t.Fatal(err)
	}
	got := wtf8.ToUTF16(tok.Value)
	if len(got) != 1 || got[0] != 0xD83D {
		t.Errorf("template cooked value = %#x, want a single 0xD83D", got)
	}
}
