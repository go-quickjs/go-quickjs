package regexp

import (
	"sort"
	"sync"
	"unicode"
)

// charSet is a set of code points, stored as sorted non-overlapping ranges.
//
// Ranges rather than a bitmap or a map, because a class such as [\u0000-\uFFFF]
// or \p{L} covers enormous spans that a bitmap could not hold and a map would
// not answer quickly. Matching is a binary search, so a class with many ranges
// costs no more per character than a small one.
type charSet struct {
	ranges []charRange
	// wordComplement marks the set \W denotes, which is the only one whose
	// membership depends on the flags in force where it is used rather than on
	// the ranges alone.
	wordComplement bool
	// negated inverts membership. It is applied at match time rather than by
	// complementing the ranges, because complementing interacts badly with
	// case folding: [^a] under the i flag must exclude both "a" and "A".
	negated bool
	// foldCase is set when the i flag applies, in which case membership is
	// tested against the simple case folds of the character too.
	foldCase bool
}

type charRange struct{ lo, hi rune }

func newCharSet() *charSet { return &charSet{} }

// addRange adds an inclusive range.
func (s *charSet) addRange(lo, hi rune) {
	if lo > hi {
		lo, hi = hi, lo
	}
	s.ranges = append(s.ranges, charRange{lo, hi})
}

// addChar adds a single code point.
func (s *charSet) addChar(r rune) { s.addRange(r, r) }

// addSet merges another set's ranges, which is how a shorthand escape inside a
// bracketed class contributes.
func (s *charSet) addSet(o *charSet) {
	if o.negated {
		// A negated shorthand such as \D inside a class contributes the
		// complement of its ranges, which has to be materialized because the
		// enclosing set has only one negation flag.
		for _, r := range complementRanges(o.ranges) {
			s.ranges = append(s.ranges, r)
		}
		return
	}
	s.ranges = append(s.ranges, o.ranges...)
}

// normalize sorts and coalesces the ranges so that matching can binary search.
func (s *charSet) normalize() {
	if len(s.ranges) < 2 {
		return
	}
	sort.Slice(s.ranges, func(i, j int) bool {
		if s.ranges[i].lo != s.ranges[j].lo {
			return s.ranges[i].lo < s.ranges[j].lo
		}
		return s.ranges[i].hi < s.ranges[j].hi
	})
	out := s.ranges[:1]
	for _, r := range s.ranges[1:] {
		last := &out[len(out)-1]
		// Merge overlapping or adjacent ranges; adjacency counts so that
		// [a-mn-z] becomes one range.
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	s.ranges = out
}

// contains reports whether a code point is in the set.
//
// unicodeFold says which of the two case-insensitive comparisons applies: the
// case folding of unicode mode, or the uppercase rule the older one uses.
func (s *charSet) contains(r rune, unicodeFold bool) bool {
	in := s.rawContains(r)
	if !in && s.foldCase {
		// Case folding is applied to the candidate rather than expanded into
		// the set, so that a large class does not multiply in size.
		for _, f := range caseFolds(r) {
			if !unicodeFold && upperCanonical(f) != upperCanonical(r) {
				// Outside unicode mode these two are different characters
				// however they fold: /[\u017F]/i does not match an "s".
				continue
			}
			if s.rawContains(f) {
				in = true
				break
			}
		}
	}
	if s.negated {
		return !in
	}
	return in
}

// rawContains tests membership without negation or folding.
func (s *charSet) rawContains(r rune) bool {
	lo, hi := 0, len(s.ranges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		switch {
		case r < s.ranges[mid].lo:
			hi = mid - 1
		case r > s.ranges[mid].hi:
			lo = mid + 1
		default:
			return true
		}
	}
	return false
}

// complementRanges returns the ranges not covered by the input, which must be
// sorted and coalesced.
func complementRanges(in []charRange) []charRange {
	tmp := &charSet{ranges: append([]charRange(nil), in...)}
	tmp.normalize()

	var out []charRange
	var next rune
	for _, r := range tmp.ranges {
		if r.lo > next {
			out = append(out, charRange{next, r.lo - 1})
		}
		if r.hi+1 > next {
			next = r.hi + 1
		}
	}
	if next <= unicode.MaxRune {
		out = append(out, charRange{next, unicode.MaxRune})
	}
	return out
}

// caseFolds returns the other code points that case-fold to the same value.
//
// unicode.SimpleFold walks the equivalence class in a cycle, so following it
// until it returns to the start yields every member. This is what makes /k/i
// match the Kelvin sign, which surprises people but is what the specification
// requires.
func caseFolds(r rune) []rune {
	var out []rune
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		out = append(out, f)
		if len(out) > 4 {
			// No equivalence class in Unicode is larger than this; the guard
			// only protects against a malformed table.
			break
		}
	}
	return out
}

// canonical returns the form two characters must share to count as the same
// under the i flag.
//
// Unicode mode compares case foldings. Without it the comparison is the older
// one, built on the simple uppercase mapping, which deliberately keeps a
// character outside ASCII apart from one inside it: the long s and the Kelvin
// sign uppercase to "S" and "K" but do not match them, which is what stops a
// pattern written in ASCII from matching text it was never meant to.
func canonical(r rune, unicodeFold bool) rune {
	if unicodeFold {
		return foldCase(r)
	}
	return upperCanonical(r)
}

// upperCanonical is Canonicalize outside unicode mode.
func upperCanonical(r rune) rune {
	u := unicode.ToUpper(r)
	// A mapping to more than one character -- the sharp s to "SS" -- is no
	// mapping at all here, and Go's simple mapping already leaves those alone.
	if r >= 128 && u < 128 {
		return r
	}
	return u
}

// foldCase returns the canonical form used when comparing single characters
// under the i flag in unicode mode.
func foldCase(r rune) rune {
	// Lowercasing is not enough on its own -- ß and ﬀ fold in ways ToLower does
	// not capture -- but SimpleFold's smallest member is a stable
	// representative of the class.
	min := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < min {
			min = f
		}
	}
	return min
}

// ---------------------------------------------------------------------------
// Predefined classes
// ---------------------------------------------------------------------------

// The shorthand escapes. Each is built once and shared, since they are by far
// the most common classes in real patterns.
var (
	classDigit    = buildSet(false, charRange{'0', '9'})
	classNotDigit = complementSet(charRange{'0', '9'})

	wordRanges = []charRange{
		{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'},
	}
	classWord    = buildSet(false, wordRanges...)
	classNotWord = markWordComplement(complementSet(wordRanges...))

	// Under the i and u flags together, the word characters take in the two
	// that fold into ASCII, so what is left out of them is a smaller set:
	// \W does not match the long s there, though \w does.
	foldedWordRanges = append(append([]charRange(nil), wordRanges...),
		charRange{0x017F, 0x017F}, charRange{0x212A, 0x212A})
	classNotFoldWord = complementSet(foldedWordRanges...)

	// The space class is the union of the Unicode space separators, the ASCII
	// whitespace characters, the line terminators and the byte order mark.
	spaceRanges = []charRange{
		{'\t', '\r'}, {' ', ' '}, {0x00A0, 0x00A0}, {0x1680, 0x1680},
		{0x2000, 0x200A}, {0x2028, 0x2029}, {0x202F, 0x202F},
		{0x205F, 0x205F}, {0x3000, 0x3000}, {0xFEFF, 0xFEFF},
	}
	classSpace    = buildSet(false, spaceRanges...)
	classNotSpace = complementSet(spaceRanges...)

	// The characters . excludes when dotAll is off.
	lineTerminators = []charRange{
		{'\n', '\n'}, {'\r', '\r'}, {0x2028, 0x2029},
	}
	classNotLineTerminator = complementSet(lineTerminators...)
)

func buildSet(negated bool, ranges ...charRange) *charSet {
	s := &charSet{ranges: append([]charRange(nil), ranges...), negated: negated}
	s.normalize()
	return s
}

// complementSet is the set of everything the given ranges leave out.
//
// The complement is worked out here rather than left to the negated flag,
// because the two are not the same thing under the i flag. \D is the set of
// characters that are not digits, and a character matches it when some member
// of that set has the same canonical form -- which is how \W comes to match the
// long s in unicode mode, where \w matches it too. [^...] is the other kind:
// there the match is inverted rather than the set, so [^a] refuses "A".
func complementSet(ranges ...charRange) *charSet {
	return &charSet{ranges: complementRanges(ranges)}
}

// markWordComplement tags the set \W denotes, whose membership the flags in
// force where it is used can widen.
func markWordComplement(s *charSet) *charSet {
	s.wordComplement = true
	return s
}

// isWordChar reports whether a code point counts as a word character for \b.
//
// Under the i and u flags together the word characters include the two
// characters outside ASCII that case-fold into it: the long s and the Kelvin
// sign are word characters there, since \w matches them.
func isWordChar(r rune, foldUnicode bool) bool {
	if r == '_' || (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
		return true
	}
	return foldUnicode && (r == 0x017F || r == 0x212A)
}

// isLineTerminator reports whether a code point ends a line, which ^ and $
// consult under the m flag.
func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// unicodeClass resolves a \p{...} escape to a set.
//
// The body is looked up verbatim, because the generated table is keyed by every
// spelling the language allows -- \p{Script=Latin}, \p{sc=Latn} and
// \p{Lowercase_Letter} are all keys of their own. A body that is not a key is
// not a property escape, which is what makes \p{Other_Alphabetic} and
// \p{ascii} syntax errors rather than empty classes.
func unicodeClass(name string, negate bool) (*charSet, bool) {
	rs, ok := unicodePropertyRanges(name)
	if !ok {
		return nil, false
	}
	if negate {
		// \P{...} is the set of everything the property leaves out, not the
		// property matched in reverse.
		return complementSet(rs...), true
	}
	return &charSet{ranges: rs}, true
}

// decodedProperties caches the ranges a property decodes to. A runtime is
// single-goroutine, but two of them in different goroutines share this.
var decodedProperties sync.Map // string -> []charRange

// unicodePropertyRanges decodes one property's ranges, or reports that the name
// is not a property at all.
//
// The returned slice is shared and must not be appended to; its capacity is cut
// to its length so that a caller which does gets a copy instead.
func unicodePropertyRanges(name string) ([]charRange, bool) {
	if rs, ok := decodedProperties.Load(name); ok {
		return rs.([]charRange), true
	}
	off, ok := unicodeProperties[name]
	if !ok {
		return nil, false
	}
	p := int(off)
	n, p := decodeVarint(p)
	rs := make([]charRange, n)
	lo := rune(-1)
	for i := range rs {
		var gap, span uint32
		gap, p = decodeVarint(p)
		span, p = decodeVarint(p)
		lo += rune(gap) + 1
		rs[i] = charRange{lo, lo + rune(span)}
		lo += rune(span)
	}
	rs = rs[:len(rs):len(rs)]
	decodedProperties.Store(name, rs)
	return rs, true
}

// decodeVarint reads one base-32 varint, returning it and the position after.
func decodeVarint(p int) (uint32, int) {
	var v uint32
	for shift := 0; ; shift += 5 {
		d := digitValues[unicodePropertyData[p]]
		p++
		v |= uint32(d&31) << shift
		if d&32 == 0 {
			return v, p
		}
	}
}

// digitValues inverts the alphabet the table is written in.
var digitValues = func() (t [256]byte) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz$_"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = byte(i)
	}
	return t
}()

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
