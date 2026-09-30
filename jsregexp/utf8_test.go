package jsregexp

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// utf8Subjects hold characters of every UTF-8 length, case variants whose
// folds differ in length, a lone surrogate in WTF-8 and a byte that begins no
// character.
var utf8Subjects = []string{
	"",
	"x",
	"héllo wörld, déjà vu",
	"a😀b😁c😂",
	"ſS KELVIN K k ſſss",
	"aé😀ée\u0301 ﬀ ß SS",
	"needle 😀 éneedle ééééneedle 😀😀😀needle",
	"ǅ ǆ Ǆ ω Ω Ω",
	"x\xed\xa0\x80y",
	"x\xffy\xe2\x82z",
	"line one\nline twö\r\nthree😀\u2028four",
	"ab😀ab😀😀ab",
}

// utf8BMPSubjects are utf8Subjects inside the Basic Multilingual Plane, where
// a pattern without the u flag reads a character as JavaScript does.
func utf8BMPSubjects() []string {
	var out []string
	for _, s := range utf8Subjects {
		bmp := true
		for _, r := range s {
			bmp = bmp && r <= 0xFFFF
		}
		if bmp {
			out = append(out, s)
		}
	}
	return out
}

// utf8Results is everything the tests compare: every match with its groups,
// a replacement and a split.
func utf8Results(t *testing.T, r *Regexp, s string) string {
	t.Helper()
	all, err := r.FindAllStringSubmatchIndex(s, -1)
	if err != nil {
		t.Fatal(err)
	}
	repl, err := r.ReplaceAllString(s, "[$&]")
	if err != nil {
		t.Fatal(err)
	}
	split, err := r.Split(s, -1)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%v %q %q", all, repl, split)
}

// TestUTF8AgreesWithUnicode pins that a pattern with the u or v flag matches a
// subject's UTF-8 exactly as it matches its code units.
func TestUTF8AgreesWithUnicode(t *testing.T) {
	patterns := []string{
		``, `.`, `x*`, `\w+`, `\W`, `\S+`, `[^a]`, `\p{L}+`, `\P{L}`, `\p{Lu}`,
		`[😀-😂]`, `[^😀]+`, `(a|é|😀)+`, `^.$`, `.$`, `\b\w+\b`, `\B.`,
		`(?<=é)\w`, `(?<=\p{Emoji_Presentation})\w`, `(?<!a)b`, `(?<=😀.)`,
		`(.)\1`, `(\w)(\w)\2\1`, `(?<=(.)\1)\w`, `(?<=\1(.))\w`, `(?<n>.)\k<n>`,
		`k`, `ſ+`, `s+`, `ω`, `ß`, `ǆ`, `[a-zé]+`, `[^\s\w]`, `.{2,3}`,
		`.+?b`, `(?:😀|a)\w`, `(?=.)`, `\d*`, `needle`, `.{0,3}needle`,
		`[\u{1F600}-\u{1F602}]{2}`, `\u{1F600}`, `(?i:k)s`, `\x{FFFD}`,
	}
	for _, flags := range []string{"u", "iu", "mu", "su", "v", "iv", "uy", "iuy"} {
		for _, p := range patterns {
			std, err := Compile(p, flags)
			if err != nil {
				continue
			}
			u8 := MustCompileUTF8(p, flags)
			for _, s := range utf8Subjects {
				if want, got := utf8Results(t, std, s), utf8Results(t, u8, s); got != want {
					t.Errorf("/%s/%s on %q:\n got %s\nwant %s", p, flags, s, got, want)
				}
			}
		}
	}
}

// TestUTF8AgreesWithoutUnicode pins that a pattern without the u flag matches
// the UTF-8 of a subject inside the BMP as it matches its code units.
func TestUTF8AgreesWithoutUnicode(t *testing.T) {
	patterns := []string{
		``, `.`, `x*`, `\w+`, `\W`, `[^a]`, `[à-ÿ]+`, `é+`, `É`, `(.)\1`,
		`(\w)\1`, `\bé`, `é\b`, `(?<=é)\w`, `(?<=(.)\1)\w`, `(?<=\1(.))\w`,
		`a|é`, `\u00e9`, `k`, `ſ`, `s`, `ω`, `^.$`, `.$`, `.{2,3}`, `.+?d`,
		`needle`, `.{0,3}needle`, `é.needle`, `\w*ne`, `(é|a)(?:dl)?e`,
		`\uFFFD`, `[\uD800-\uDFFF]`, `ǆ`, `(?i:é)r`,
		// Runs of what only ASCII is in, which are matched a byte at a
		// time, and of what more is in, which are not.
		`[a-z]+`, `\d+`, `a*é`, `[^é]+`, `\s+`, `[0-9a-f]*\w`, `k+`, `s+\b`,
		`[^\s]+`, `\W+`, `e+\u0301`,
	}
	subjects := utf8BMPSubjects()
	for _, flags := range []string{"", "i", "m", "s", "y", "iy"} {
		for _, p := range patterns {
			std := MustCompile(p, flags)
			u8 := MustCompileUTF8(p, flags)
			for _, s := range subjects {
				if want, got := utf8Results(t, std, s), utf8Results(t, u8, s); got != want {
					t.Errorf("/%s/%s on %q:\n got %s\nwant %s", p, flags, s, got, want)
				}
			}
		}
	}
}

// TestUTF8Astral pins what a pattern without the u flag does with a character
// past the BMP, where reading by code point differs from JavaScript: the
// character is one, never half of one.
func TestUTF8Astral(t *testing.T) {
	tests := []struct {
		pattern, flags, s string
		want              [][]int
	}{
		{`.`, "", "a😀", [][]int{{0, 1}, {1, 5}}},
		{`^.$`, "", "😀", [][]int{{0, 4}}},
		{`[^a]`, "", "a😀", [][]int{{1, 5}}},
		{`😀`, "", "a😀b", [][]int{{1, 5}}},
		// A quantifier takes the second half alone, which is never there.
		{`\uD83D\uDE00+`, "", "😀😀", nil},
		{`(?:\uD83D\uDE00)+`, "", "😀😀", [][]int{{0, 8}}},
		{`\uD83D`, "", "😀", nil},
		{`[\uD800-\uDFFF]`, "", "😀", nil},
		{`(?:)`, "", "😀a", [][]int{{0, 0}, {4, 4}, {5, 5}}},
		{`.{0,3}needle`, "", "😀😀😀needle", [][]int{{0, 18}}},
		{`.{0,1}needle`, "", "😀😀😀needle", [][]int{{8, 18}}},
		{`(.)\1`, "", "😀😀", [][]int{{0, 8}}},
		{`(?<=😀)x`, "", "😀x", [][]int{{4, 5}}},
		{`(?<=.)x`, "", "😀x", [][]int{{4, 5}}},
		{`\b`, "", "😀a", [][]int{{4, 4}, {5, 5}}},
	}
	for _, tt := range tests {
		got, err := MustCompileUTF8(tt.pattern, tt.flags).FindAllStringIndex(tt.s, -1)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("/%s/%s on %q = %v, want %v", tt.pattern, tt.flags, tt.s, got, tt.want)
		}
	}
}

// TestUTF8Backreference pins that a case-insensitive backreference matches a
// case variant of another length in UTF-8, in either direction.
func TestUTF8Backreference(t *testing.T) {
	tests := []struct {
		pattern, flags, s string
		want              []int
	}{
		{`(ſ)\1`, "iu", "ſS", []int{0, 3, 0, 2}},
		{`(s)\1`, "iu", "sſ", []int{0, 3, 0, 1}},
		{`(k+)\1`, "iu", "kkKK", []int{0, 8, 0, 2}},
		{`(?<=\1(s))x`, "iu", "ſsx", []int{3, 4, 2, 3}},
		// Leftwards the reference comes first, before its group has
		// captured, and the group then matches the S.
		{`(?<=(ſ)\1)x`, "iu", "ſSx", []int{3, 4, 2, 3}},
		{`(é)\1`, "i", "éÉ", []int{0, 4, 0, 2}},
		{`(ω)\1`, "iu", "ωΩ", []int{0, 5, 0, 2}},
		// A byte that begins no character is U+FFFD, as another such is.
		{`(.)\1`, "", "\xff\xfe", []int{0, 2, 0, 1}},
		{`(.)\1`, "", "\xff�", []int{0, 4, 0, 1}},
		{`(a😀)\1`, "", "a😀a😀", []int{0, 10, 0, 5}},
		{`(ſ)\1`, "i", "ſS", nil},
	}
	for _, tt := range tests {
		got, err := MustCompileUTF8(tt.pattern, tt.flags).FindStringSubmatchIndex(tt.s)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("/%s/%s on %q = %v, want %v", tt.pattern, tt.flags, tt.s, got, tt.want)
		}
	}
}

// TestUTF8LongSubject pins the literal skip on a subject long enough that the
// search is moved forward to the literal, across characters of every length.
func TestUTF8LongSubject(t *testing.T) {
	s := strings.Repeat("aé€😀", 200) + "needle" + strings.Repeat("😀x", 50) + "needle"
	for _, p := range []string{`.{0,3}needle`, `.needle`, `(?:é|😀).{0,2}needle`, `needle`, `\w{0,5}needle`} {
		std := MustCompile(p, "u")
		u8 := MustCompileUTF8(p, "")
		if want, got := utf8Results(t, std, s), utf8Results(t, u8, s); got != want {
			t.Errorf("/%s/ on the long subject:\n got %s\nwant %s", p, got, want)
		}
	}
}
