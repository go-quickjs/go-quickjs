package text

import "testing"

func TestParseInt(t *testing.T) {
	tests := []struct {
		s     string
		width int
		want  uint64
		ok    bool
	}{
		{"0", 32, 0, true},
		{"4294967295", 32, 0xffffffff, true},
		{"4294967296", 32, 0, false},
		{"-2147483648", 32, 0x80000000, true},
		{"-2147483649", 32, 0, false},
		{"+2147483647", 32, 0x7fffffff, true},
		{"0xffff_ffff", 32, 0xffffffff, true},
		{"-0x8000_0000", 32, 0x80000000, true},
		{"18446744073709551615", 64, 0xffffffffffffffff, true},
		{"18446744073709551616", 64, 0, false},
		{"-9223372036854775808", 64, 0x8000000000000000, true},
		{"-9223372036854775809", 64, 0, false},
		{"255", 8, 0xff, true},
		{"256", 8, 0, false},
		{"-128", 8, 0x80, true},
		{"-129", 8, 0, false},
		{"-1", 16, 0xffff, true},
		{"1_000", 32, 1000, true},
		{"1__0", 32, 0, false},
		{"1.0", 32, 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseInt(tt.s, tt.width)
		if ok != tt.ok || ok && got != tt.want {
			t.Errorf("ParseInt(%q, %d) = %#x, %v; want %#x, %v", tt.s, tt.width, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseFloat(t *testing.T) {
	f32 := []struct {
		s    string
		want uint32
		ok   bool
	}{
		{"0", 0, true},
		{"-0", 0x80000000, true},
		{"1", 0x3f800000, true},
		{"0x1p-149", 0x00000001, true},
		{"0x1p-150", 0, true}, // a tie, to even
		{"0x1.000002p-150", 0x00000001, true},
		{"3.4028234663852886e38", 0x7f7fffff, true},
		{"0x1.fffffefffffffffffp127", 0x7f7fffff, true},
		{"0x1.ffffffp127", 0, false}, // rounds to infinity
		{"1e39", 0, false},
		{"inf", 0x7f800000, true},
		{"-inf", 0xff800000, true},
		{"nan", 0x7fc00000, true},
		{"-nan", 0xffc00000, true},
		{"nan:0x1", 0x7f800001, true},
		{"-nan:0x7f_ffff", 0xffffffff, true},
		{"nan:0x0", 0, false},
		{"nan:0x800000", 0, false},
		{"0x1.8", 0x3fc00000, true},
		{"1_000.000_1", 0x447a0002, true},
		{"1.e1", 0x41200000, true},
		{"+0x10", 0x41800000, true},
		// Rounding a decimal straight to float32, not through float64.
		{"1.00000005960464477550", 0x3f800001, true},
		{"1.0000000596046447755", 0x3f800001, true},
	}
	for _, tt := range f32 {
		got, ok := ParseF32(tt.s)
		if ok != tt.ok || ok && got != tt.want {
			t.Errorf("ParseF32(%q) = %#x, %v; want %#x, %v", tt.s, got, ok, tt.want, tt.ok)
		}
	}
	f64 := []struct {
		s    string
		want uint64
		ok   bool
	}{
		{"1", 0x3ff0000000000000, true},
		{"0x1p-1074", 1, true},
		{"0x1.fffffffffffff8p1023", 0, false},
		{"0x1.fffffffffffff7ffffffp1023", 0x7fefffffffffffff, true},
		{"nan:0xf_ffff_ffff_ffff", 0x7fffffffffffffff, true},
		{"nan:0x10_0000_0000_0000", 0, false},
		{"nan", 0x7ff8000000000000, true},
		{"1e-400", 0, true},
		{"1e309", 0, false},
	}
	for _, tt := range f64 {
		got, ok := ParseF64(tt.s)
		if ok != tt.ok || ok && got != tt.want {
			t.Errorf("ParseF64(%q) = %#x, %v; want %#x, %v", tt.s, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLex(t *testing.T) {
	toks, annots, err := Lex([]byte("(module $\"a b\" (; (; nested ;) ;) \"\\41\\u{1F600}\\n\" 0x1_0 -1 1.5e3 nan:0x1 offset=4 ;; to the end\n i32.add (@x (y) \"z\") $x)"))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		kind Kind
		text string
	}{
		{LParen, "("}, {Keyword, "module"}, {ID, "a b"}, {String, "A\U0001F600\n"}, {Nat, "0x1_0"},
		{Int, "-1"}, {Float, "1.5e3"}, {Float, "nan:0x1"}, {Keyword, "offset=4"}, {Keyword, "i32.add"},
		{ID, "x"}, {RParen, ")"}, {EOF, ""},
	}
	if len(toks) != len(want) {
		t.Fatalf("%d tokens, want %d: %+v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].Kind != w.kind || toks[i].Text != w.text {
			t.Errorf("token %d: %v %q, want %v %q", i, toks[i].Kind, toks[i].Text, w.kind, w.text)
		}
	}
	if toks[9].Line != 2 || toks[9].Col != 2 {
		t.Errorf("i32.add at %d:%d, want 2:2", toks[9].Line, toks[9].Col)
	}
	if len(annots) != 1 || annots[0].Name != "x" || annots[0].At != 10 || len(annots[0].Items) != 4 {
		t.Errorf("annotations %+v", annots)
	}
	for _, src := range []string{
		`"unclosed`, "\"a\nb\"", `"\q"`, `"\u{110000}"`, "\"\x01\"", `$`, `$""`, `(@)`, `(@ x)`,
		`(@x`, `0x`, `1_`, `a"b"`, `""""`, "\x7f", "é", `,`, `(@x é)`, "(; \xff ;)", ";; \xff\n",
	} {
		if _, _, err := Lex([]byte(src)); err == nil {
			t.Errorf("Lex(%q) succeeded", src)
		}
	}
}
