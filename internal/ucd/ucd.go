// Package ucd answers the questions about Unicode characters the language
// asks -- what may begin or continue an identifier, what is a space, what a
// character's case is and what it folds to -- from tables of its own, at the
// one Unicode version of the regexp's property escapes (Version).
//
// The standard library's unicode package answers them at the Unicode version
// of the Go that builds the program, which made the language a build
// accepts depend on its toolchain: a Go at Unicode 15.0 rejected identifiers
// that a Go at 17.0 accepted, and folded characters apart under /iu that 17.0
// folds together. The tables are generated (tables.go, by
// internal/regexp/internal/unicodegen); the functions here read them the way
// the unicode package reads its own.
//
// The tables' data is the Unicode Character Database's, and that of
// test262's generated property-escape tests: UNICODE_LICENSE holds the
// Unicode License v3 it is used under, and the repository's NOTICE
// test262's license.
package ucd

// range16 and range32 are an inclusive range of code points, in the Basic
// Multilingual Plane and beyond it.
type range16 struct{ lo, hi uint16 }
type range32 struct{ lo, hi uint32 }

// table is a set of code points as sorted, disjoint ranges.
type table struct {
	r16 []range16
	r32 []range32
}

// has reports whether r is in the set, by a binary search of the ranges of
// its plane's half.
func (t *table) has(r rune) bool {
	if uint32(r) <= 0xFFFF {
		c := uint16(r)
		rs := t.r16
		lo, hi := 0, len(rs)
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if rg := rs[m]; c < rg.lo {
				hi = m
			} else if c > rg.hi {
				lo = m + 1
			} else {
				return true
			}
		}
		return false
	}
	c := uint32(r)
	rs := t.r32
	lo, hi := 0, len(rs)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if rg := rs[m]; c < rg.lo {
			hi = m
		} else if c > rg.hi {
			lo = m + 1
		} else {
			return true
		}
	}
	return false
}

// IsIDStart reports whether r has the ID_Start property.
func IsIDStart(r rune) bool { return idStart.has(r) }

// IsIDContinue reports whether r has the ID_Continue property.
func IsIDContinue(r rune) bool { return idContinue.has(r) }

// IsSpaceSeparator reports whether r is in General_Category Space_Separator
// (Zs).
func IsSpaceSeparator(r rune) bool { return spaceSeparator.has(r) }

// IsUpper reports whether r is in General_Category Uppercase_Letter (Lu), as
// unicode.IsUpper does.
func IsUpper(r rune) bool {
	if r < 0x80 {
		return 'A' <= r && r <= 'Z'
	}
	return uppercaseLetter.has(r)
}

// IsCased reports whether r has the Cased property.
func IsCased(r rune) bool { return cased.has(r) }

// IsCaseIgnorable reports whether r has the Case_Ignorable property.
func IsCaseIgnorable(r rune) bool { return caseIgnorable.has(r) }

// caseRange maps the code points from lo to hi by adding up for upper case
// and low for lower case, or, with upperLower set, alternates between an
// upper-case code point at an even offset from lo and the lower case after it.
type caseRange struct {
	lo, hi     uint32
	up, low    int32
	upperLower bool
}

// lookupCase is the range r is in, or nil.
func lookupCase(r rune) *caseRange {
	c := uint32(r)
	lo, hi := 0, len(caseRanges)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if cr := &caseRanges[m]; c < cr.lo {
			hi = m
		} else if c > cr.hi {
			lo = m + 1
		} else {
			return cr
		}
	}
	return nil
}

// convert maps r by cr to upper or lower case.
func (cr *caseRange) convert(r rune, upper bool) rune {
	if cr.upperLower {
		base := rune(cr.lo)
		if upper {
			return base + (r-base)&^1
		}
		return base + ((r-base)&^1 | 1)
	}
	if upper {
		return r + rune(cr.up)
	}
	return r + rune(cr.low)
}

// ToUpper is r's simple upper-case mapping, or r, as unicode.ToUpper answers.
func ToUpper(r rune) rune {
	if r < 0x80 {
		if 'a' <= r && r <= 'z' {
			r -= 'a' - 'A'
		}
		return r
	}
	if cr := lookupCase(r); cr != nil {
		return cr.convert(r, true)
	}
	return r
}

// ToLower is r's simple lower-case mapping, or r, as unicode.ToLower answers.
func ToLower(r rune) rune {
	if r < 0x80 {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		return r
	}
	if cr := lookupCase(r); cr != nil {
		return cr.convert(r, false)
	}
	return r
}

// foldPair is one step of a case-folding orbit.
type foldPair struct{ from, to uint32 }

// SimpleFold is the next code point after r in its orbit under simple case
// folding -- the code points with the same simple folding, in ascending order
// and round again -- or r alone in its, as unicode.SimpleFold answers.
func SimpleFold(r rune) rune {
	if r < 0 || r > 0x10FFFF {
		return r
	}
	if r < 0x80 {
		return rune(asciiFold[r])
	}
	c := uint32(r)
	lo, hi := 0, len(caseOrbit)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if caseOrbit[m].from < c {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(caseOrbit) && caseOrbit[lo].from == c {
		return rune(caseOrbit[lo].to)
	}
	// Otherwise the orbit is r and its lower or upper case.
	if cr := lookupCase(r); cr != nil {
		if l := cr.convert(r, false); l != r {
			return l
		}
		return cr.convert(r, true)
	}
	return r
}

// CaseFold is r's simple case folding, CaseFolding's common and simple
// mapping of it, or r where it has none: the member of r's SimpleFold orbit
// the orbit folds to.
func CaseFold(r rune) rune {
	c := uint32(r)
	lo, hi := 0, len(foldNotLower)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if foldNotLower[m].from < c {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(foldNotLower) && foldNotLower[lo].from == c {
		return rune(foldNotLower[lo].to)
	}
	return ToLower(r)
}

// FoldRanges calls f with ranges of code points outside which every code
// point is alone in its simple case folding orbit (SimpleFold(r) == r): the
// ASCII letters, the case-mapped ranges and the orbits' own entries. A range
// may hold code points that are alone too; ranges may overlap and come in no
// order.
func FoldRanges(f func(lo, hi rune)) {
	f('A', 'Z')
	f('a', 'z')
	for _, p := range caseOrbit {
		f(rune(p.from), rune(p.from))
	}
	for i := range caseRanges {
		f(rune(caseRanges[i].lo), rune(caseRanges[i].hi))
	}
}

// FullUpper is r's language-independent upper-case mapping where it is not
// the simple one: the sharp s's "SS", say. The context-sensitive and the
// language-sensitive mappings are not here.
func FullUpper(r rune) ([]rune, bool) {
	to, ok := fullUpper[r]
	return to, ok
}

// FullLower is FullUpper for lower case. The final sigma's mapping, which
// depends on what surrounds it, is not here.
func FullLower(r rune) ([]rune, bool) {
	to, ok := fullLower[r]
	return to, ok
}
