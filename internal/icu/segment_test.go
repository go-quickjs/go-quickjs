package icu

import (
	"strings"
	"testing"
)

// The pieces a text is broken into, written out with a bar between them. Every
// case here is what a full ICU answers, so that a change to the tables or to
// the rules is caught as a difference from it.
func TestBreaks(t *testing.T) {
	for _, tc := range []struct {
		g    Granularity
		in   string
		want string
	}{
		// A character is what a reader would call one: an accent written apart
		// belongs to its letter, a flag is two code points, and a family is
		// seven.
		{Graphemes, "héllo", "h|é|l|l|o"},
		{Graphemes, "éclair", "é|c|l|a|i|r"},
		{Graphemes, "👨‍👩‍👧‍👦!", "👨‍👩‍👧‍👦|!"},
		{Graphemes, "🇺🇸🇬🇧", "🇺🇸|🇬🇧"},
		{Graphemes, "🇺🇸🇬", "🇺🇸|🇬"},
		{Graphemes, "🧑🏽‍🚀", "🧑🏽‍🚀"},
		{Graphemes, "a\r\nb", "a|\r\n|b"},
		{Graphemes, "가각", "가|각"},
		{Graphemes, "ที่", "ที่"},

		// A word keeps the punctuation that stands in the middle of it, and
		// loses the punctuation that stands between words.
		{Words, "can't stop", "can't| |stop"},
		{Words, "3.14 and 1,000", "3.14| |and| |1,000"},
		{Words, "a_b, c", "a_b|,| |c"},
		{Words, "AT&T", "AT|&|T"},
		{Words, "éclair", "éclair"},
		{Words, "word‍join", "word‍join"},
		{Words, "日本語のテキストです", "日本語|の|テキスト|です"},
		{Words, "한국어 텍스트", "한국어| |텍스트"},
		{Words, "שלום.", "שלום|."},

		// A sentence ends at a full stop, unless the full stop is a decimal
		// point, an initial, or an abbreviation.
		{Sentences, "One. Two! Three?", "One. |Two! |Three?"},
		{Sentences, "It is 3.14 exactly. Yes.", "It is 3.14 exactly. |Yes."},
		{Sentences, "The U.S.A. is large.", "The U.S.A. is large."},
		{Sentences, "i.e. this one. Next.", "i.e. this one. |Next."},
		{Sentences, "He said \"hi.\" Then left.", "He said \"hi.\" |Then left."},
		{Sentences, "one\r\ntwo", "one\r\n|two"},
	} {
		at := Breaks(tc.in, tc.g)
		var pieces []string
		for i := 1; i < len(at); i++ {
			pieces = append(pieces, tc.in[at[i-1]:at[i]])
		}
		if got := strings.Join(pieces, "|"); got != tc.want {
			t.Errorf("Breaks(%q, %d)\n got  %q\n want %q", tc.in, tc.g, got, tc.want)
		}
	}
}

// Whether a piece of text is a word, which is the one thing about a word that
// is not a question about where it begins.
func TestWordLike(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"hello", true}, {"can't", true}, {"3.14", true}, {"a_b", true},
		{"日本語", true}, {"한국어", true}, {"עברית", true}, {"ก", true},
		{" ", false}, {",", false}, {"...", false}, {"👨‍👩", false},
		{"\r\n", false},
	} {
		if got := WordLike(tc.in); got != tc.want {
			t.Errorf("WordLike(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// An empty text has nothing to break, and a text of one character breaks only
// at its ends.
func TestBreaksEdges(t *testing.T) {
	for _, g := range []Granularity{Graphemes, Words, Sentences} {
		if at := Breaks("", g); at != nil {
			t.Errorf("Breaks(%q, %d) = %v, want nil", "", g, at)
		}
		if at := Breaks("a", g); len(at) != 2 || at[0] != 0 || at[1] != 1 {
			t.Errorf("Breaks(%q, %d) = %v, want [0 1]", "a", g, at)
		}
	}
}
