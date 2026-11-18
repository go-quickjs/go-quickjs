package vm

import (
	"hash/maphash"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/ucd"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// The case conversions' data is package ucd's, at its Unicode version
// rather than the toolchain's: the simple mappings, SpecialCasing's
// unconditional full mappings for the characters whose upper- or lower-case
// form is more than one character or is not what the simple mapping gives,
// and the Cased and Case_Ignorable properties the final sigma's rule reads.
// The language-sensitive mappings are not among them: what toUpperCase and
// toLowerCase apply is the language-independent mapping, and the one
// context-sensitive rule they do apply -- a final sigma lowercasing to
// U+03C2 -- is a condition rather than a table entry, so it lives in the code
// that walks the string.

// asciiCase is caseConvert for an ASCII string, in which only the 26 letters
// have a case, each the other's with bit 5 flipped. A string with none to
// change is returned as it is.
func asciiCase(s *String, upper bool) *String {
	g := s.Go()
	lo, hi := byte('A'), byte('Z')
	if upper {
		lo, hi = 'a', 'z'
	}
	i := 0
	for i < len(g) && (g[i] < lo || g[i] > hi) {
		i++
	}
	if i == len(g) {
		return s
	}
	// Written into the String's own allocation where it is short.
	out, b := newFlatBytes(len(g))
	copy(b, g[:i])
	for ; i < len(g); i++ {
		c := g[i]
		if c >= lo && c <= hi {
			c ^= 0x20
		}
		b[i] = c
	}
	out.setBytes(b)
	out.length, out.ascii = s.length, true
	return out
}

// caseMemoMax is the longest text the case conversions remember.
const caseMemoMax = 32

// caseMemoWays is how many results a set of a caseMemo holds.
const caseMemoWays = 4

// caseMemo is a runtime's memo of the case conversions of short ASCII
// strings, by their text (asciiCaseMemo): a render or a replacement that
// converts the same short strings over and over -- a word's first letter,
// a tag's or a style property's name -- had allocated each conversion
// anew. It holds results alone, 64 sets of caseMemoWays, a text's set
// chosen by a seeded hash of it and the case, so that no input picks the
// sets of its strings; a result is the conversion of a text where each of
// its bytes is that byte converted (isCaseOf).
type caseMemo struct {
	sets [64][caseMemoWays]*String
	// next is the way the next result put in takes, in turn.
	next uint8
}

// caseSeed seeds caseMemo's hash.
var caseSeed = maphash.MakeSeed()

// asciiCaseMemo is asciiCase of s, an ASCII string: a single character's
// result the runtime's string of it, and that of a text of up to
// caseMemoMax bytes remembered, in a memo made at the first conversion,
// so that the same text converted again is the same string, which a
// string's immutability allows.
func (r *Runtime) asciiCaseMemo(s *String, upper bool) *String {
	g := s.Go()
	if len(g) > caseMemoMax || len(g) == 0 {
		return asciiCase(s, upper)
	}
	if len(g) == 1 {
		c := g[0]
		if upper && c >= 'a' && c <= 'z' || !upper && c >= 'A' && c <= 'Z' {
			return r.asciiCharString(c ^ 0x20)
		}
		return s
	}
	m := r.caseMemo
	if m == nil {
		m = new(caseMemo)
		r.caseMemo = m
	}
	h := maphash.String(caseSeed, g) >> 58
	if upper {
		// A text's two conversions are in two sets.
		h ^= 1
	}
	set := &m.sets[h]
	for _, out := range set {
		if out != nil && len(out.s) == len(g) && isCaseOf(out.s, g, upper) {
			return out
		}
	}
	out := asciiCase(s, upper)
	if out != s {
		set[m.next%caseMemoWays] = out
		m.next++
	}
	return out
}

// isCaseOf reports whether out, an ASCII text as long as a, is a converted
// to upper case or to lower case, byte by byte.
func isCaseOf(out, a string, upper bool) bool {
	lo, hi := byte('A'), byte('Z')
	if upper {
		lo, hi = 'a', 'z'
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= lo && c <= hi {
			c ^= 0x20
		}
		if out[i] != c {
			return false
		}
	}
	return true
}

// caseConvert maps a string to upper or lower case, character by character.
//
// Most characters have a single-character mapping, the simple one. The rest
// have a full mapping, except the final sigma, whose mapping depends on what
// surrounds it rather than on what it is.
func caseConvert(s string, upper bool) string {
	var b strings.Builder
	b.Grow(len(s))
	// The text is decoded rather than ranged over, because ranging decodes an
	// encoded surrogate as U+FFFD and would replace it: an unpaired surrogate
	// is not a character, has no case, and comes through as it went in.
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		if wtf8.IsSurrogate(r) {
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		switch {
		case !upper && r == 'Σ':
			b.WriteRune(finalSigma(s, i))
		default:
			var to []rune
			var full bool
			if upper {
				to, full = ucd.FullUpper(r)
			} else {
				to, full = ucd.FullLower(r)
			}
			switch {
			case full:
				for _, c := range to {
					b.WriteRune(c)
				}
			case upper:
				b.WriteRune(ucd.ToUpper(r))
			default:
				b.WriteRune(ucd.ToLower(r))
			}
		}
		i += size
	}
	return b.String()
}

// finalSigma is what a capital sigma lowercases to at the given position.
//
// A sigma that ends a word becomes U+03C2, and one anywhere else becomes
// U+03C3. Ending a word means something cased comes before it and nothing cased
// comes after, with the characters that do not count either way -- accents,
// formatting, apostrophes -- skipped on both sides.
func finalSigma(s string, at int) rune {
	before := false
	for _, r := range reverseRunes(s[:at]) {
		if isCaseIgnorable(r) {
			continue
		}
		before = isCased(r)
		break
	}
	if !before {
		return 'σ'
	}
	for _, r := range s[at+len("Σ"):] {
		if isCaseIgnorable(r) {
			continue
		}
		if isCased(r) {
			return 'σ'
		}
		break
	}
	return 'ς'
}

// reverseRunes reads a string backwards, which the sigma rule needs.
func reverseRunes(s string) []rune {
	rs := []rune(s)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return rs
}

// isCased reports whether a character is one the sigma rule counts as a letter
// on either side of it: Cased.
func isCased(r rune) bool {
	return ucd.IsCased(r)
}

// isCaseIgnorable reports whether a character is one the sigma rule looks
// past: Case_Ignorable, the marks, the format characters, the modifier
// letters and symbols, and the word-breaking punctuation that may sit inside a
// word, the MidLetter, MidNumLet and Single_Quote classes. A full stop is one
// of them, which is what makes the sigma in "A.\u03A3" a final one.
func isCaseIgnorable(r rune) bool {
	return ucd.IsCaseIgnorable(r)
}
