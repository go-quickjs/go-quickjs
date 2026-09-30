package regexp

import (
	"math/rand"
	"slices"
	"testing"
	"unicode/utf16"
)

// TestRequiredLiteral pins the literal a pattern is found to require, and the
// least and the most units between the start of a match and it.
func TestRequiredLiteral(t *testing.T) {
	cases := []struct {
		src, flags string
		lit        string // "" when there is none
		lo, hi     int
	}{
		{`(^|[^\\])"\\\/Qngr\((-?[0-9]+)\)\\\/"`, "g", `"\/Qngr(`, 0, 1},
		{`abc`, "", "abc", 0, 0},
		{`a?bcd`, "", "bcd", 0, 1},
		{`(ab|cd)ef`, "", "ef", 2, 2},
		{`a.bc`, "", "bc", 2, 2},
		{`\s?;\s?end`, "", "end", 1, 3},
		{`(?<=xy)cd`, "", "cd", 0, 0},
		{`^(?:ab)+cd`, "", "", 0, 0}, // ab is inside a repetition, cd past an unbounded one
		{`x*abc`, "", "", 0, 0},
		{`ab|cd`, "", "", 0, 0},
		{`(a)\1xy`, "", "", 0, 0},
		{`\d{2}-\d{2}`, "", "", 0, 0},
		{`##oe##`, "i", "##", 0, 0},
		{`K1`, "i", "", 0, 0},
		{`abc`, "u", "", 0, 0}, // only without the unicode flag
		{`.{70}abc`, "", "", 0, 0},
	}
	for _, c := range cases {
		re, err := Compile(c.src, c.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", c.src, c.flags, err)
		}
		got := re.prog.lit
		switch {
		case c.lit == "" && got != nil:
			t.Errorf("/%s/%s: literal %q at %d..%d, want none", c.src, c.flags, string(utf16.Decode(got.units)), got.minOff, got.maxOff)
		case c.lit != "" && got == nil:
			t.Errorf("/%s/%s: no literal, want %q", c.src, c.flags, c.lit)
		case c.lit != "" && (string(utf16.Decode(got.units)) != c.lit || got.minOff != c.lo || got.maxOff != c.hi):
			t.Errorf("/%s/%s: literal %q at %d..%d, want %q at %d..%d", c.src, c.flags,
				string(utf16.Decode(got.units)), got.minOff, got.maxOff, c.lit, c.lo, c.hi)
		}
	}
}

// TestRequiredLiteralAgrees pins that looking for the required literal first
// changes no match: each pattern finds the same match from every start with
// it as without it, over subjects made of the literal's pieces, near misses
// and the characters around it.
func TestRequiredLiteralAgrees(t *testing.T) {
	patterns := []struct{ src, flags string }{
		{`(^|[^\\])"\\\/Qngr\((-?[0-9]+)\)\\\/"`, "g"}, {`abc`, ""}, {`a?bcd`, ""}, {`(ab|cd)ef`, ""},
		{`a.bc`, ""}, {`\s?;\s?end`, ""}, {`(?<=xy)cd`, ""}, {`##oe##`, "i"}, {`(a|b)?cd(e)`, ""},
		{`\bcd\b`, ""}, {`x?y?cdcd`, ""}, {`(?:a|)bc`, "m"}, {`[^c]?cc`, ""}, {`^cd`, "m"}, {`cd$`, ""},
	}
	alphabet := []string{"a", "b", "c", "d", "e", "f", "x", "y", "\\", `"`, "/", "Qngr(", `"\/Qngr(`, "12", ")", `\/"`,
		" ", ";", "end", "#", "##", "oe", "OE", "cd", "\n", "\U0001F600"}
	r := rand.New(rand.NewSource(1))
	var subjects [][]uint16
	for i := 0; i < 400; i++ {
		var s string
		for j := r.Intn(14); j >= 0; j-- {
			s += alphabet[r.Intn(len(alphabet))]
		}
		subjects = append(subjects, utf16.Encode([]rune(s)))
	}
	with := 0
	for _, p := range patterns {
		re, err := Compile(p.src, p.flags)
		if err != nil {
			t.Fatalf("/%s/%s: %v", p.src, p.flags, err)
		}
		if re.prog.lit != nil {
			with++
		}
		plain, _ := Compile(p.src, p.flags)
		plain.prog = &program{}
		*plain.prog = *re.prog
		plain.prog.lit, plain.prog.first = nil, nil
		for _, s := range subjects {
			for start := 0; start <= len(s); start++ {
				got, err1 := re.Match(s, start)
				want, err2 := plain.Match(s, start)
				if err1 != nil || err2 != nil || !slices.Equal(got, want) {
					t.Fatalf("/%s/%s on %q from %d: %v, %v; without the literal %v, %v",
						p.src, p.flags, string(utf16.Decode(s)), start, got, err1, want, err2)
				}
			}
		}
	}
	// The test means nothing if nothing has a literal to look for.
	if with < len(patterns)*2/3 {
		t.Errorf("only %d of %d patterns have a required literal", with, len(patterns))
	}
}
