package regexp

import (
	"sort"
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
func (s *charSet) contains(r rune) bool {
	in := s.rawContains(r)
	if !in && s.foldCase {
		// Case folding is applied to the candidate rather than expanded into
		// the set, so that a large class does not multiply in size.
		for _, f := range caseFolds(r) {
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

// foldCase returns the canonical form used when comparing single characters
// under the i flag.
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
	classNotDigit = buildSet(true, charRange{'0', '9'})

	classWord = buildSet(false,
		charRange{'0', '9'}, charRange{'A', 'Z'}, charRange{'_', '_'},
		charRange{'a', 'z'})
	classNotWord = buildSet(true,
		charRange{'0', '9'}, charRange{'A', 'Z'}, charRange{'_', '_'},
		charRange{'a', 'z'})

	// The space class is the union of the Unicode space separators, the ASCII
	// whitespace characters, the line terminators and the byte order mark.
	spaceRanges = []charRange{
		{'\t', '\r'}, {' ', ' '}, {0x00A0, 0x00A0}, {0x1680, 0x1680},
		{0x2000, 0x200A}, {0x2028, 0x2029}, {0x202F, 0x202F},
		{0x205F, 0x205F}, {0x3000, 0x3000}, {0xFEFF, 0xFEFF},
	}
	classSpace    = buildSet(false, spaceRanges...)
	classNotSpace = buildSet(true, spaceRanges...)

	// The characters . excludes when dotAll is off.
	lineTerminators = []charRange{
		{'\n', '\n'}, {'\r', '\r'}, {0x2028, 0x2029},
	}
	classNotLineTerminator = buildSet(true, lineTerminators...)
)

func buildSet(negated bool, ranges ...charRange) *charSet {
	s := &charSet{ranges: append([]charRange(nil), ranges...), negated: negated}
	s.normalize()
	return s
}

// isWordChar reports whether a code point counts as a word character for \b.
func isWordChar(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// isLineTerminator reports whether a code point ends a line, which ^ and $
// consult under the m flag.
func isLineTerminator(r rune) bool {
	return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
}

// categoryTable resolves a general category by either of its names.
//
// The long form is what a pattern is likely to spell; Go's tables are keyed by
// the abbreviation.
func categoryTable(name string) []*unicode.RangeTable {
	if short, ok := categoryAliases[name]; ok {
		name = short
	}
	if t, ok := unicode.Categories[name]; ok {
		return []*unicode.RangeTable{t}
	}
	return nil
}

// binaryTable resolves a binary property.
//
// Go carries most of them verbatim. The rest are derived here from the
// categories they are defined in terms of -- close enough to be useful, and
// marked where they are known to be approximate.
func binaryTable(name string) []*unicode.RangeTable {
	if long, ok := binaryAliases[name]; ok {
		name = long
	}
	// The language names a fixed set of binary properties, which is narrower
	// than the set Unicode defines and narrower than the set Go carries. A
	// property outside it is a syntax error rather than a class that happens to
	// work here and nowhere else.
	if !binaryProperties[name] {
		return nil
	}
	if t, ok := unicode.Properties[name]; ok {
		return []*unicode.RangeTable{t}
	}
	switch name {
	case "Assigned":
		return []*unicode.RangeTable{unicode.Cc, unicode.Cf, unicode.Co,
			unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z}
	case "Alphabetic":
		return []*unicode.RangeTable{unicode.L, unicode.Nl, unicode.Other_Alphabetic}
	case "Lowercase":
		return []*unicode.RangeTable{unicode.Ll, unicode.Other_Lowercase}
	case "Uppercase":
		return []*unicode.RangeTable{unicode.Lu, unicode.Other_Uppercase}
	case "Cased":
		return []*unicode.RangeTable{unicode.Ll, unicode.Lu, unicode.Lt,
			unicode.Other_Lowercase, unicode.Other_Uppercase}
	case "Case_Ignorable":
		return []*unicode.RangeTable{unicode.Mn, unicode.Me, unicode.Cf,
			unicode.Lm, unicode.Sk}
	case "Math":
		return []*unicode.RangeTable{unicode.Sm, unicode.Other_Math}
	case "ID_Start", "XID_Start":
		return []*unicode.RangeTable{unicode.L, unicode.Nl, unicode.Other_ID_Start}
	case "ID_Continue", "XID_Continue":
		return []*unicode.RangeTable{unicode.L, unicode.Nl, unicode.Other_ID_Start,
			unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue}
	case "Grapheme_Base":
		return []*unicode.RangeTable{unicode.L, unicode.N, unicode.P, unicode.S,
			unicode.Zs, unicode.Mc, unicode.Me}
	case "Grapheme_Extend":
		return []*unicode.RangeTable{unicode.Mn, unicode.Me, unicode.Other_Grapheme_Extend}
	case "Default_Ignorable_Code_Point":
		return []*unicode.RangeTable{unicode.Other_Default_Ignorable_Code_Point,
			unicode.Cf, unicode.Variation_Selector}
	case "Emoji", "Emoji_Presentation", "Emoji_Modifier", "Emoji_Modifier_Base",
		"Emoji_Component", "Extended_Pictographic":
		// Go carries no emoji data, so these resolve to the symbol categories
		// the characters actually live in. Approximate, and documented as such.
		return []*unicode.RangeTable{unicode.So, unicode.Sk}
	}
	return nil
}

// unicodeClass resolves a \p{...} escape to a set.
//
// Both the bare form, which names a general category or a binary property, and
// the Property=Value form are accepted.
func unicodeClass(name string, negate bool) (*charSet, bool) {
	var tables []*unicode.RangeTable

	if i := indexByte(name, '='); i >= 0 {
		prop, value := name[:i], name[i+1:]
		switch prop {
		case "General_Category", "gc":
			tables = categoryTable(value)
		case "Script", "sc", "Script_Extensions", "scx":
			// Script_Extensions is answered with the plain Script table, which
			// Go is the only data available for. The two differ only for code
			// points a second script borrows, so the answer is a subset rather
			// than a wrong kind of thing.
			if long, ok := scriptAliases[value]; ok {
				value = long
			}
			if t, ok := unicode.Scripts[value]; ok {
				tables = []*unicode.RangeTable{t}
			}
		}
	} else {
		tables = categoryTable(name)
		if tables == nil {
			tables = binaryTable(name)
		}
		// A bare name is a general category or a binary property and nothing
		// else. A script has to be written Script=Latin, so that \p{Greek}
		// cannot quietly change meaning when a property of that name appears.
		switch name {
		case "Any":
			return buildSet(negate, charRange{0, unicode.MaxRune}), true
		case "ASCII":
			return buildSet(negate, charRange{0, 0x7F}), true
		}
	}
	if len(tables) == 0 {
		return nil, false
	}

	s := &charSet{negated: negate}
	for _, t := range tables {
		for _, r := range t.R16 {
			if r.Stride == 1 {
				s.addRange(rune(r.Lo), rune(r.Hi))
				continue
			}
			for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
				s.addChar(c)
			}
		}
		for _, r := range t.R32 {
			if r.Stride == 1 {
				s.addRange(rune(r.Lo), rune(r.Hi))
				continue
			}
			for c := rune(r.Lo); c <= rune(r.Hi); c += rune(r.Stride) {
				s.addChar(c)
			}
		}
	}
	s.normalize()
	return s, true
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
